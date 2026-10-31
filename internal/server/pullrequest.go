package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/dire-kiwi/kiwicd/internal/wf"
	"github.com/nektos/act/pkg/model"
)

// Pull requests come from GitHub's pull_request webhook, sent to a mirror's
// webhook URL with its secret. A mirror's git refs alone can't tell a pull
// request from a fork from one of the repository's own branches, and GitHub
// keeps refs/pull/N/head after a pull request closes, so the webhook is the
// only source: kiwicd doesn't poll for pull requests.
//
// For each event the mirror fetches refs/pull/N/head, so that jobs can fetch
// the head commit from kiwicd. Events are handled one at a time in the order
// they arrive, so runs are created in that order too. Each event starts:
//
//   - the mirror's own workflows that listen with pull_request, as on GitHub.
//     They run on a merge commit of the head into the base branch, which
//     kiwicd makes itself (GitHub's refs/pull/N/merge is made in the
//     background and may not be up to date when the event comes) and keeps
//     at refs/pull/N/merge: github.ref is refs/pull/N/merge and github.sha
//     is that commit, whose workflow files are the ones that run. As on
//     GitHub, a pull request whose head doesn't merge cleanly starts none.
//     These runs are from an unprotected ref, so they get no secrets but
//     KIWICD_TOKEN and protected and reserved runner labels keep them off
//     those runners. Pull requests from forks only start them if the
//     repository allows it (fork_pull_requests), since the workflow itself
//     comes from the fork.
//   - other repositories' workflows that listen with repository_pull_request,
//     from their default branches, as triggerRepositoryPush does for pushes.

// pullRequestEvent is the part of GitHub's pull_request event kiwicd reads.
type pullRequestEvent struct {
	Action      string `json:"action"`
	Number      int64  `json:"number"`
	PullRequest struct {
		Title string `json:"title"`
		Head  struct {
			Ref  string    `json:"ref"`
			Sha  string    `json:"sha"`
			Repo *ghRepoID `json:"repo"` // null once a fork is deleted
		} `json:"head"`
		Base struct {
			Ref  string    `json:"ref"`
			Repo *ghRepoID `json:"repo"`
		} `json:"base"`
	} `json:"pull_request"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
}

type ghRepoID struct {
	FullName string `json:"full_name"`
}

// fork reports whether the pull request's head branch is in another
// repository than its base branch, or in one that's gone.
func (e *pullRequestEvent) fork() bool {
	head, base := e.PullRequest.Head.Repo, e.PullRequest.Base.Repo
	return head == nil || base == nil || !strings.EqualFold(head.FullName, base.FullName)
}

type queuedPullRequest struct {
	repoID int64
	body   []byte
}

func parsePullRequestEvent(body []byte) (*pullRequestEvent, map[string]any, error) {
	var ev pullRequestEvent
	var raw map[string]any
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, nil, err
	}
	if ev.Action == "" || ev.Number <= 0 || ev.PullRequest.Head.Sha == "" || ev.PullRequest.Base.Ref == "" {
		return nil, nil, fmt.Errorf("not a pull_request event")
	}
	return &ev, raw, nil
}

// queuePullRequest hands an event to the worker, which handles events one at
// a time. It reports false if too many are waiting.
func (s *Server) queuePullRequest(q queuedPullRequest) bool {
	s.prOnce.Do(func() {
		s.prQueue = make(chan queuedPullRequest, 256)
		go func() {
			for q := range s.prQueue {
				if err := s.onPullRequest(context.Background(), q.repoID, q.body); err != nil {
					s.log.Error("pull request trigger", "repo", q.repoID, "err", err)
				}
			}
		}()
	})
	select {
	case s.prQueue <- q:
		return true
	default:
		return false
	}
}

// onPullRequest fetches the pull request's head into the mirror and starts
// the workflows listening to it.
func (s *Server) onPullRequest(ctx context.Context, repoID int64, body []byte) error {
	ev, raw, err := parsePullRequestEvent(body)
	if err != nil {
		return err
	}
	repo, err := s.getRepoByID(repoID)
	if err != nil {
		return err
	}
	// Listeners start even if the fetch fails: a closed pull request needs no
	// commit, and other jobs report what they can't fetch. The repository's
	// own workflows need the merge commit.
	merge, ferr := s.fetchPullRequest(ctx, repo, ev)
	if ferr != nil {
		s.log.Warn("fetch pull request", "repo", repo.Name, "number", ev.Number, "err", ferr)
	}
	payload := map[string]any{}
	for _, k := range []string{"action", "number", "pull_request", "sender", "before", "after"} {
		if v, ok := raw[k]; ok {
			payload[k] = v
		}
	}
	// Like push events: kiwicd's repository, which the job can clone.
	payload["repository"] = repoEventInfo(repo, s.cfg.PublicURL)
	pr := wf.PullRequest{Repo: repo.Name, Action: ev.Action, Base: ev.PullRequest.Base.Ref, Fork: ev.fork()}
	s.log.Info("pull request", "repo", repo.Name, "number", ev.Number, "action", ev.Action, "fork", pr.Fork, "merge", merge.sha)
	switch {
	case ferr != nil: // no merge commit to run
	case merge.conflict:
		s.log.Info("pull request doesn't merge cleanly; its pull_request workflows don't run", "repo", repo.Name, "number", ev.Number)
	case pr.Fork && !repo.ForkPullRequests:
		s.log.Info("pull request from a fork; the repository doesn't run pull_request workflows for them", "repo", repo.Name, "number", ev.Number)
	default:
		pr.ChangedFiles = merge.changed
		title := fmt.Sprintf("#%d %s: %s", ev.Number, ev.Action, ev.PullRequest.Title)
		err = s.runPullRequestWorkflows(ctx, repo, ev, pr, merge.sha, payload, title)
	}
	title := fmt.Sprintf("%s #%d %s: %s", repo.Name, ev.Number, ev.Action, ev.PullRequest.Title)
	lerr := s.triggerListeners(ctx, repo, wf.RepositoryPullRequest, payload, ev.Sender.Login, title, func(w *model.Workflow) bool {
		return wf.MatchesRepositoryPullRequest(w, pr)
	})
	if err == nil {
		err = lerr
	}
	return err
}

// runPullRequestWorkflows starts the repository's own workflows at the
// pull request's merge commit that listen with pull_request.
func (s *Server) runPullRequestWorkflows(ctx context.Context, repo *Repo, ev *pullRequestEvent, pr wf.PullRequest, sha string,
	payload map[string]any, title string) error {
	files, err := s.workflowsAt(ctx, repo, sha)
	if err != nil {
		return err
	}
	ref := pullRequestMergeRef(ev.Number)
	actor := ev.Sender.Login
	for _, file := range sortedKeys(files) {
		src := files[file]
		w, perr := wf.Parse(src)
		if perr != nil {
			// Only report broken workflows that look like they listen for
			// pull requests.
			if bytes.Contains(src, []byte("pull_request")) {
				_, _ = s.createRun(ctx, runSpec{repo: repo, file: file, yaml: string(src), name: file, event: "pull_request",
					payload: payload, ref: ref, sha: sha, actor: actor, title: title, parseErr: perr})
			}
			continue
		}
		if !wf.MatchesPullRequest(w, pr) {
			continue
		}
		name := w.Name
		if name == "" {
			name = file
		}
		if _, err := s.createRun(ctx, runSpec{repo: repo, file: file, yaml: string(src), name: name, event: "pull_request",
			payload: payload, ref: ref, sha: sha, actor: actor, title: title}); err != nil {
			return err
		}
	}
	return nil
}

func pullRequestMergeRef(number int64) string { return fmt.Sprintf("refs/pull/%d/merge", number) }

// pullRequestFetchRetry is how long to wait before fetching a pull request's
// head again when it isn't the event's commit yet.
var pullRequestFetchRetry = 3 * time.Second

// pullRequestMerge is the result of merging a pull request's head into its
// base branch.
type pullRequestMerge struct {
	sha      string   // the merge commit, at refs/pull/N/merge
	changed  []string // the files the pull request changes
	conflict bool     // the head doesn't merge cleanly; no commit
}

// fetchPullRequest fetches refs/pull/N/head of a mirror's origin, until the
// mirror has the event's head commit (GitHub may send the event before it
// updates the ref), and the base branch's tip, and merges the head into it.
func (s *Server) fetchPullRequest(ctx context.Context, repo *Repo, ev *pullRequestEvent) (pullRequestMerge, error) {
	if repo.Kind != "mirror" {
		return pullRequestMerge{}, fmt.Errorf("%s is not a mirror", repo.Name)
	}
	lock := s.repoLock(repo.ID)
	lock.Lock()
	defer lock.Unlock()
	p := s.repoPath(repo.Name)
	if _, err := git(ctx, p, "remote", "set-url", "origin", repo.URL); err != nil {
		return pullRequestMerge{}, err
	}
	env, cleanup, err := s.remoteEnv(ctx, repo)
	if err != nil {
		return pullRequestMerge{}, err
	}
	defer cleanup()
	head := ev.PullRequest.Head.Sha
	ref := fmt.Sprintf("refs/pull/%d/head", ev.Number)
	for attempt := 1; ; attempt++ {
		_, err := gitEnv(ctx, p, env, "fetch", "--no-tags", "origin", "+"+ref+":"+ref)
		if err == nil {
			if _, err = resolveRef(ctx, p, head); err == nil {
				break
			}
			err = fmt.Errorf("%s doesn't have %s", ref, head)
		}
		if attempt == 3 {
			return pullRequestMerge{}, err
		}
		select {
		case <-ctx.Done():
			return pullRequestMerge{}, ctx.Err()
		case <-time.After(pullRequestFetchRetry):
		}
	}
	// The base branch as it is now, which may be newer than the mirror's,
	// but only in FETCH_HEAD: the mirror's branches change when it syncs, so
	// that the sync sees the push.
	if strings.ContainsAny(ev.PullRequest.Base.Ref, ": ") {
		return pullRequestMerge{}, fmt.Errorf("bad base branch %q", ev.PullRequest.Base.Ref)
	}
	if _, err := gitEnv(ctx, p, env, "fetch", "--no-tags", "origin", "refs/heads/"+ev.PullRequest.Base.Ref); err != nil {
		return pullRequestMerge{}, err
	}
	base, err := resolveRef(ctx, p, "FETCH_HEAD")
	if err != nil {
		return pullRequestMerge{}, err
	}
	return mergePullRequest(ctx, p, ev.Number, base, head)
}

// mergePullRequest merges head into base without a work tree and points
// refs/pull/N/merge at the merge commit. The commit is made the same way
// each time, so the same head and base give the same commit.
func mergePullRequest(ctx context.Context, p string, number int64, base, head string) (pullRequestMerge, error) {
	// git merge-tree exits with 1 and still prints a tree if there are
	// conflicts.
	mctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(mctx, "git", "merge-tree", "--write-tree", "--no-messages", base, head)
	cmd.Dir = p
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return pullRequestMerge{conflict: true}, nil
	}
	if err != nil {
		return pullRequestMerge{}, fmt.Errorf("git merge-tree (git 2.38 or later is needed): %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	tree, _, _ := strings.Cut(stdout.String(), "\n")
	date, err := git(ctx, p, "log", "-1", "--format=%cI", head)
	if err != nil {
		return pullRequestMerge{}, err
	}
	date = strings.TrimSpace(date)
	env := []string{"GIT_AUTHOR_NAME=kiwicd", "GIT_AUTHOR_EMAIL=kiwicd@localhost", "GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_NAME=kiwicd", "GIT_COMMITTER_EMAIL=kiwicd@localhost", "GIT_COMMITTER_DATE=" + date}
	out, err := gitEnv(ctx, p, env, "commit-tree", strings.TrimSpace(tree), "-p", base, "-p", head,
		"-m", fmt.Sprintf("Merge %s into %s", head, base))
	if err != nil {
		return pullRequestMerge{}, err
	}
	merge := pullRequestMerge{sha: strings.TrimSpace(out)}
	if _, err := git(ctx, p, "update-ref", pullRequestMergeRef(number), merge.sha); err != nil {
		return pullRequestMerge{}, err
	}
	// The pull request's changes, as GitHub lists them: from where the head
	// branched off the base.
	if mb, err := git(ctx, p, "merge-base", base, head); err == nil {
		merge.changed = changedFiles(ctx, p, strings.TrimSpace(mb), head)
	}
	return merge, nil
}
