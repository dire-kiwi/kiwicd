package server

import (
	"context"
	"encoding/json"
	"fmt"
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
// the head commit from kiwicd, and then starts the workflows of other
// repositories that listen with repository_pull_request, from their default
// branches, as triggerRepositoryPush does for pushes. Events are handled one
// at a time in the order they arrive, so runs are created in that order too.

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
	// Runs start even if the fetch fails: a closed pull request needs no
	// commit, and other jobs report what they can't fetch.
	if err := s.fetchPullRequest(ctx, repo, ev.Number, ev.PullRequest.Head.Sha); err != nil {
		s.log.Warn("fetch pull request", "repo", repo.Name, "number", ev.Number, "err", err)
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
	title := fmt.Sprintf("%s #%d %s: %s", repo.Name, ev.Number, ev.Action, ev.PullRequest.Title)
	s.log.Info("pull request", "repo", repo.Name, "number", ev.Number, "action", ev.Action, "fork", pr.Fork)
	return s.triggerListeners(ctx, repo, wf.RepositoryPullRequest, payload, ev.Sender.Login, title, func(w *model.Workflow) bool {
		return wf.MatchesRepositoryPullRequest(w, pr)
	})
}

// pullRequestFetchRetry is how long to wait before fetching a pull request's
// head again when it isn't the event's commit yet.
var pullRequestFetchRetry = 3 * time.Second

// fetchPullRequest fetches refs/pull/N/head of a mirror's origin, until the
// mirror has the commit sha (GitHub may send the event before it updates the
// ref).
func (s *Server) fetchPullRequest(ctx context.Context, repo *Repo, number int64, sha string) error {
	if repo.Kind != "mirror" {
		return fmt.Errorf("%s is not a mirror", repo.Name)
	}
	lock := s.repoLock(repo.ID)
	lock.Lock()
	defer lock.Unlock()
	p := s.repoPath(repo.Name)
	if _, err := git(ctx, p, "remote", "set-url", "origin", repo.URL); err != nil {
		return err
	}
	env, cleanup, err := s.gitSSHEnv(repo)
	if err != nil {
		return err
	}
	defer cleanup()
	ref := fmt.Sprintf("refs/pull/%d/head", number)
	for attempt := 1; ; attempt++ {
		_, err := gitEnv(ctx, p, env, "fetch", "--no-tags", "origin", "+"+ref+":"+ref)
		if err == nil {
			if _, err := resolveRef(ctx, p, sha); err == nil {
				return nil
			}
			err = fmt.Errorf("%s doesn't have %s", ref, sha)
		}
		if attempt == 3 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pullRequestFetchRetry):
		}
	}
}
