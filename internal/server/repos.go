package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dire-kiwi/kiwicd/internal/wf"
	"github.com/nektos/act/pkg/model"
)

type Repo struct {
	ID            int64             `json:"id"`
	Name          string            `json:"name"`
	Kind          string            `json:"kind"`
	URL           string            `json:"url,omitempty"`
	DefaultBranch string            `json:"default_branch"`
	WebhookSecret string            `json:"webhook_secret,omitempty"`
	Refs          map[string]string `json:"refs"`
	LastSyncAt    int64             `json:"last_sync_at"`
	LastSyncError string            `json:"last_sync_error"`
	CreatedAt     int64             `json:"created_at"`
	SyncedAt      int64             `json:"synced_at"`
	SSHKey        string            `json:"-"` // encrypted; see gitSSHEnv
	SSHPublicKey  string            `json:"ssh_public_key,omitempty"`
	SSHKnownHosts string            `json:"ssh_known_hosts,omitempty"`
	SSHHostKeys   []string          `json:"ssh_host_keys,omitempty"`
	Readers       []string          `json:"readers"`        // see jobtoken.go
	ProtectedRefs []string          `json:"protected_refs"` // see protectedrefs.go
	// Branches whose runs get secrets, for the UI.
	ProtectedBranches []string `json:"protected_branches,omitempty"`
	CloneURL          string   `json:"clone_url,omitempty"`
	WebhookURL        string   `json:"webhook_url,omitempty"`
}

const repoCols = `id, name, kind, url, default_branch, webhook_secret, refs, last_sync_at, last_sync_error, created_at,
	synced_at, ssh_key, ssh_public_key, ssh_known_hosts, readers, protected_refs`

func scanRepo(row interface{ Scan(...any) error }) (*Repo, error) {
	var r Repo
	var refs, readers, protectedRefs string
	if err := row.Scan(&r.ID, &r.Name, &r.Kind, &r.URL, &r.DefaultBranch, &r.WebhookSecret, &refs, &r.LastSyncAt, &r.LastSyncError, &r.CreatedAt,
		&r.SyncedAt, &r.SSHKey, &r.SSHPublicKey, &r.SSHKnownHosts, &readers, &protectedRefs); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(refs), &r.Refs)
	if r.Refs == nil {
		r.Refs = map[string]string{}
	}
	_ = json.Unmarshal([]byte(readers), &r.Readers)
	if r.Readers == nil {
		r.Readers = []string{}
	}
	_ = json.Unmarshal([]byte(protectedRefs), &r.ProtectedRefs)
	if r.ProtectedRefs == nil {
		r.ProtectedRefs = []string{}
	}
	return &r, nil
}

func (s *Server) getRepo(name string) (*Repo, error) {
	return scanRepo(s.db.QueryRow(`SELECT `+repoCols+` FROM repos WHERE name = ?`, name))
}

func (s *Server) getRepoByID(id int64) (*Repo, error) {
	return scanRepo(s.db.QueryRow(`SELECT `+repoCols+` FROM repos WHERE id = ?`, id))
}

var repoNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func (s *Server) decorateRepo(r *http.Request, repo *Repo) {
	base := s.publicURL(r)
	repo.CloneURL = base + "/git/" + repo.Name + ".git"
	repo.WebhookURL = base + "/api/hooks/" + repo.Name
	// Credentials in mirror URLs are never sent to the browser.
	repo.URL = redactURL(repo.URL)
	repo.ProtectedBranches = protectedBranches(repo)
	if repo.Kind == "mirror" {
		// The host keys in use: configured, or trusted on first connection.
		if b, err := os.ReadFile(s.knownHostsPath(repo.ID)); err == nil {
			repo.SSHHostKeys = hostKeys(b)
		}
	}
}

// sshKeyComment names a repository's deploy key.
func sshKeyComment(repo string) string { return "kiwicd " + repo }

func (s *Server) handleListRepos(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT ` + repoCols + ` FROM repos ORDER BY name`)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	defer rows.Close()
	repos := []*Repo{}
	for rows.Next() {
		repo, err := scanRepo(rows)
		if err != nil {
			httpError(w, 500, "%v", err)
			return
		}
		s.decorateRepo(r, repo)
		repo.WebhookSecret = ""
		repos = append(repos, repo)
	}
	writeJSON(w, repos)
}

func (s *Server) handleCreateRepo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name           string `json:"name"`
		Kind           string `json:"kind"`
		URL            string `json:"url"`
		DefaultBranch  string `json:"default_branch"`
		SSHKey         string `json:"ssh_key"`
		GenerateSSHKey bool   `json:"generate_ssh_key"`
		SSHKnownHosts  string `json:"ssh_known_hosts"`
	}
	if err := readJSON(r, &req); err != nil {
		httpError(w, 400, "bad request: %v", err)
		return
	}
	req.Name = strings.TrimSuffix(strings.TrimSpace(req.Name), ".git")
	if !repoNameRE.MatchString(req.Name) || strings.Contains(req.Name, "..") {
		httpError(w, 400, "name must look like owner/repo")
		return
	}
	if req.Kind == "" {
		req.Kind = "hosted"
		if req.URL != "" {
			req.Kind = "mirror"
		}
	}
	if req.Kind != "hosted" && req.Kind != "mirror" {
		httpError(w, 400, "kind must be hosted or mirror")
		return
	}
	if req.Kind == "mirror" && req.URL == "" {
		httpError(w, 400, "mirror repositories need a url")
		return
	}
	if strings.HasPrefix(req.URL, "-") {
		httpError(w, 400, "invalid url")
		return
	}
	if req.DefaultBranch == "" {
		req.DefaultBranch = "main"
	}
	var ssh sshSettings
	if req.Kind == "mirror" {
		var err error
		if ssh, err = newSSHSettings(req.Name, req.SSHKey, req.GenerateSSHKey, req.SSHKnownHosts); err != nil {
			httpError(w, 400, "%v", err)
			return
		}
	}
	if _, err := s.getRepo(req.Name); err == nil {
		httpError(w, 409, "repository %s already exists", req.Name)
		return
	}
	ctx := r.Context()
	if req.Kind == "hosted" {
		if err := s.initHostedRepo(ctx, req.Name); err != nil {
			httpError(w, 400, "%v", err)
			return
		}
	}
	res, err := s.db.Exec(`INSERT INTO repos (name, kind, url, default_branch, webhook_secret, created_at, ssh_key, ssh_public_key, ssh_known_hosts)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.Name, req.Kind, req.URL, req.DefaultBranch, randomToken(20), nowMS(), s.sealSSHKey(ssh.key), ssh.public, ssh.knownHosts)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	id, _ := res.LastInsertId()
	// The first sync clones a mirror and records existing refs as a baseline,
	// so they don't trigger runs.
	if err := s.syncRepo(ctx, id, true); err != nil {
		// A generated deploy key can't be on the remote yet: keep the mirror
		// so its public key can be added, and sync again afterwards.
		if !req.GenerateSSHKey {
			s.deleteRepo(id, req.Name)
			httpError(w, 400, "%v", err)
			return
		}
		s.log.Warn("initial sync", "repo", req.Name, "err", err)
	}
	repo, _ := s.getRepoByID(id)
	s.decorateRepo(r, repo)
	writeJSON(w, repo)
}

type sshSettings struct{ key, public, knownHosts string }

// sealSSHKey encrypts a private key for the database; "" stays "" (no key).
func (s *Server) sealSSHKey(key string) string {
	if key == "" {
		return ""
	}
	return s.box.seal(purposeSSHKey, key)
}

func newSSHSettings(repo, key string, generate bool, knownHosts string) (sshSettings, error) {
	var out sshSettings
	var err error
	switch {
	case generate && key != "":
		return out, errors.New("give an SSH key or generate one, not both")
	case generate:
		out.key, out.public, err = generateSSHKey(sshKeyComment(repo))
	case key != "":
		out.key, out.public, err = parseSSHKey(key, sshKeyComment(repo))
	}
	if err != nil {
		return out, err
	}
	if strings.TrimSpace(knownHosts) != "" {
		if err := checkKnownHosts(knownHosts); err != nil {
			return out, err
		}
		out.knownHosts = strings.TrimSpace(knownHosts) + "\n"
	}
	return out, nil
}

// handleUpdateRepo changes a repository's settings. Omitted fields are kept;
// an empty ssh_key removes the key and empty ssh_known_hosts goes back to
// trusting the host key on first connection.
func (s *Server) handleUpdateRepo(w http.ResponseWriter, r *http.Request) {
	repo, err := s.getRepo(r.PathValue("owner") + "/" + r.PathValue("name"))
	if err != nil {
		httpError(w, 404, "no such repository")
		return
	}
	var req struct {
		URL            *string   `json:"url"`
		DefaultBranch  *string   `json:"default_branch"`
		SSHKey         *string   `json:"ssh_key"`
		GenerateSSHKey bool      `json:"generate_ssh_key"`
		SSHKnownHosts  *string   `json:"ssh_known_hosts"`
		Readers        *[]string `json:"readers"`
		ProtectedRefs  *[]string `json:"protected_refs"`
	}
	if err := readJSON(r, &req); err != nil {
		httpError(w, 400, "bad request: %v", err)
		return
	}
	if req.Readers != nil {
		if repo.Readers, err = cleanReaders(repo.Name, *req.Readers); err != nil {
			httpError(w, 400, "%v", err)
			return
		}
	}
	if req.ProtectedRefs != nil {
		if repo.ProtectedRefs, err = cleanProtectedRefs(*req.ProtectedRefs); err != nil {
			httpError(w, 400, "%v", err)
			return
		}
	}
	if repo.Kind != "mirror" && (req.URL != nil || req.SSHKey != nil || req.GenerateSSHKey || req.SSHKnownHosts != nil) {
		httpError(w, 400, "only mirrors have a remote URL and SSH settings")
		return
	}
	if req.URL != nil {
		if *req.URL = strings.TrimSpace(*req.URL); *req.URL == "" {
			httpError(w, 400, "mirror repositories need a url")
			return
		}
		if strings.HasPrefix(*req.URL, "-") {
			httpError(w, 400, "invalid url")
			return
		}
		repo.URL = *req.URL
	}
	if req.DefaultBranch != nil {
		if *req.DefaultBranch = strings.TrimSpace(*req.DefaultBranch); *req.DefaultBranch == "" {
			httpError(w, 400, "the default branch can't be empty")
			return
		}
		repo.DefaultBranch = *req.DefaultBranch
	}
	if req.SSHKey != nil || req.GenerateSSHKey {
		key := ""
		if req.SSHKey != nil {
			key = *req.SSHKey
		}
		ssh, err := newSSHSettings(repo.Name, key, req.GenerateSSHKey, "")
		if err != nil {
			httpError(w, 400, "%v", err)
			return
		}
		repo.SSHKey, repo.SSHPublicKey = s.sealSSHKey(ssh.key), ssh.public
	}
	if req.SSHKnownHosts != nil {
		ssh, err := newSSHSettings(repo.Name, "", false, *req.SSHKnownHosts)
		if err != nil {
			httpError(w, 400, "%v", err)
			return
		}
		repo.SSHKnownHosts = ssh.knownHosts
	}
	// Under the sync lock, so a fetch never runs with half-applied settings.
	lock := s.repoLock(repo.ID)
	lock.Lock()
	_, err = s.db.Exec(`UPDATE repos SET url = ?, default_branch = ?, ssh_key = ?, ssh_public_key = ?, ssh_known_hosts = ?, readers = ?, protected_refs = ?
		WHERE id = ?`, repo.URL, repo.DefaultBranch, repo.SSHKey, repo.SSHPublicKey, repo.SSHKnownHosts, mustJSON(repo.Readers),
		mustJSON(repo.ProtectedRefs), repo.ID)
	if err == nil && req.SSHKnownHosts != nil && repo.SSHKnownHosts == "" {
		// Forget pinned host keys: the next connection trusts the host again.
		err = os.Remove(s.knownHostsPath(repo.ID))
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	}
	lock.Unlock()
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	repo, _ = s.getRepoByID(repo.ID)
	s.decorateRepo(r, repo)
	writeJSON(w, repo)
}

// cleanReaders checks a repository's readers: owner/name each, without
// duplicates or the repository itself, which can always read itself.
func cleanReaders(self string, names []string) ([]string, error) {
	out := []string{}
	for _, name := range names {
		name = strings.TrimSuffix(strings.TrimSpace(name), ".git")
		if name == "" || strings.EqualFold(name, self) || containsFold(out, name) {
			continue
		}
		if !repoNameRE.MatchString(name) || strings.Contains(name, "..") {
			return nil, fmt.Errorf("reader %q must look like owner/repo", name)
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

func (s *Server) handleGetRepo(w http.ResponseWriter, r *http.Request) {
	repo, err := s.getRepo(r.PathValue("owner") + "/" + r.PathValue("name"))
	if err != nil {
		httpError(w, 404, "no such repository")
		return
	}
	s.decorateRepo(r, repo)
	writeJSON(w, repo)
}

func (s *Server) handleDeleteRepo(w http.ResponseWriter, r *http.Request) {
	repo, err := s.getRepo(r.PathValue("owner") + "/" + r.PathValue("name"))
	if err != nil {
		httpError(w, 404, "no such repository")
		return
	}
	if err := s.deleteRepo(repo.ID, repo.Name); err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) deleteRepo(id int64, name string) error {
	// Delete runs one by one first, so that their logs go in small batches
	// rather than in one long cascade.
	rows, err := s.db.Query(`SELECT id FROM runs WHERE repo_id = ?`, id)
	if err != nil {
		return err
	}
	var runIDs []int64
	for rows.Next() {
		var runID int64
		_ = rows.Scan(&runID)
		runIDs = append(runIDs, runID)
	}
	rows.Close()
	for _, runID := range runIDs {
		if err := s.deleteRun(context.Background(), runID); err != nil {
			return err
		}
	}
	s.mu.Lock()
	_, err = s.db.Exec(`DELETE FROM repos WHERE id = ?`, id)
	_, _ = s.db.Exec(`DELETE FROM secrets WHERE repo_id = ?`, id)
	_, _ = s.db.Exec(`DELETE FROM variables WHERE repo_id = ?`, id)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.tipMu.Lock()
	delete(s.tipWorkflows, id)
	s.tipMu.Unlock()
	for _, dir := range []string{s.repoPath(name), s.sshDir(id)} {
		if err := removeAll(dir); err != nil {
			s.log.Warn("remove repository files", "dir", dir, "err", err)
		}
	}
	return nil
}

func (s *Server) handleSyncRepo(w http.ResponseWriter, r *http.Request) {
	repo, err := s.getRepo(r.PathValue("owner") + "/" + r.PathValue("name"))
	if err != nil {
		httpError(w, 404, "no such repository")
		return
	}
	if err := s.syncRepo(r.Context(), repo.ID, false); err != nil {
		httpError(w, 502, "%v", err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// handleWebhook accepts GitHub-style webhooks. A pull_request event starts
// repository_pull_request workflows (see pullrequest.go); any other event
// triggers a sync. The secret is checked with X-Hub-Signature-256 or a
// ?secret= parameter.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	repo, err := s.getRepo(r.PathValue("owner") + "/" + r.PathValue("name"))
	if err != nil {
		httpError(w, 404, "no such repository")
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	ok := false
	if sig := r.Header.Get("X-Hub-Signature-256"); sig != "" {
		ok = validSignature(repo.WebhookSecret, body, sig)
	} else if q := r.URL.Query().Get("secret"); q != "" {
		ok = hmac.Equal([]byte(q), []byte(repo.WebhookSecret))
	}
	if !ok {
		httpError(w, 401, "bad webhook signature")
		return
	}
	if r.Header.Get("X-GitHub-Event") == "pull_request" {
		body = webhookJSON(r, body)
		if repo.Kind != "mirror" {
			httpError(w, 400, "pull request events need a mirror of the repository they come from")
			return
		}
		if _, _, err := parsePullRequestEvent(body); err != nil {
			httpError(w, 400, "bad pull_request event: %v", err)
			return
		}
		if !s.queuePullRequest(queuedPullRequest{repoID: repo.ID, body: body}) {
			httpError(w, 503, "too many pull request events waiting; redeliver this one later")
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	go func() {
		if err := s.syncRepo(context.Background(), repo.ID, false); err != nil {
			s.log.Warn("webhook sync", "repo", repo.Name, "err", err)
		}
	}()
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) repoLock(id int64) *sync.Mutex {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	if s.syncLocks[id] == nil {
		s.syncLocks[id] = &sync.Mutex{}
	}
	return s.syncLocks[id]
}

// syncRepo fetches a mirror (hosted repos are already local), compares refs
// with the last seen state and triggers push workflows for changed refs.
func (s *Server) syncRepo(ctx context.Context, id int64, baseline bool) error {
	lock := s.repoLock(id)
	lock.Lock()
	defer lock.Unlock()
	repo, err := s.getRepoByID(id)
	if err != nil {
		return err
	}
	if repo.Kind == "mirror" {
		if repo.SyncedAt == 0 {
			baseline = true // first successful fetch of this mirror
		}
		if err := s.fetchMirror(ctx, repo); err != nil {
			_, _ = s.db.Exec(`UPDATE repos SET last_sync_at = ?, last_sync_error = ? WHERE id = ?`, nowMS(), err.Error(), id)
			return err
		}
	}
	path := s.repoPath(repo.Name)
	refs, err := listRefs(ctx, path)
	if err != nil {
		return err
	}
	if !baseline {
		var changed []string
		for ref, sha := range refs {
			if repo.Refs[ref] != sha {
				changed = append(changed, ref)
			}
		}
		sort.Strings(changed)
		for _, ref := range changed {
			if err := s.onPush(ctx, repo, ref, repo.Refs[ref], refs[ref]); err != nil {
				s.log.Error("push trigger", "repo", repo.Name, "ref", ref, "err", err)
			}
		}
	}
	_, err = s.db.Exec(`UPDATE repos SET refs = ?, last_sync_at = ?, synced_at = ?, last_sync_error = '' WHERE id = ?`, mustJSON(refs), nowMS(), nowMS(), id)
	return err
}

func repoEventInfo(repo *Repo, base string) map[string]any {
	owner, name, _ := strings.Cut(repo.Name, "/")
	return map[string]any{
		"full_name":      repo.Name,
		"name":           name,
		"owner":          map[string]any{"login": owner, "name": owner},
		"default_branch": repo.DefaultBranch,
		"clone_url":      base + "/git/" + repo.Name + ".git",
		"html_url":       base + "/repos/" + repo.Name,
	}
}

func (s *Server) onPush(ctx context.Context, repo *Repo, ref, before, after string) error {
	path := s.repoPath(repo.Name)
	if before == "" {
		before = zeroSHA
	}
	changed := changedFiles(ctx, path, before, after)
	commit := readCommit(ctx, path, after)
	event := map[string]any{
		"ref":        ref,
		"before":     before,
		"after":      after,
		"created":    before == zeroSHA,
		"deleted":    false,
		"forced":     false,
		"repository": repoEventInfo(repo, s.cfg.PublicURL),
		"pusher":     map[string]any{"name": commit.Author, "email": commit.Email},
		"sender":     map[string]any{"login": commit.Author},
		"head_commit": map[string]any{
			"id": after, "message": commit.Subject,
			"author":    map[string]any{"name": commit.Author, "email": commit.Email},
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		},
	}
	err := s.runPushWorkflows(ctx, repo, ref, after, changed, event, commit)
	if lerr := s.triggerRepositoryPush(ctx, repo, ref, changed, event, commit); err == nil {
		err = lerr
	}
	return err
}

func (s *Server) runPushWorkflows(ctx context.Context, repo *Repo, ref, after string, changed []string, event map[string]any, commit commitInfo) error {
	files, err := s.workflowsAt(ctx, repo, after)
	if err != nil {
		return err
	}
	for _, file := range sortedKeys(files) {
		src := files[file]
		w, perr := wf.Parse(src)
		if perr != nil {
			// Only report broken workflows that look like they listen for pushes.
			if strings.Contains(string(src), "push") {
				_, _ = s.createRun(ctx, runSpec{repo: repo, file: file, yaml: string(src), name: file, event: "push",
					payload: event, ref: ref, sha: after, actor: commit.Author, title: commit.Subject, parseErr: perr})
			}
			continue
		}
		if !wf.MatchesPush(w, ref, changed) {
			continue
		}
		name := w.Name
		if name == "" {
			name = file
		}
		if _, err := s.createRun(ctx, runSpec{repo: repo, file: file, yaml: string(src), name: name, event: "push",
			payload: event, ref: ref, sha: after, actor: commit.Author, title: commit.Subject}); err != nil {
			return err
		}
	}
	return nil
}

// triggerRepositoryPush starts other repositories' workflows that listen for
// pushes to src with repository_push. Like GitHub's repository_dispatch, they
// come from the listener's default branch and run there, with its source,
// secrets and variables; the event payload is src's push event. That way a
// public repository can be deployed by a private one that holds the
// deployment's details and secrets.
func (s *Server) triggerRepositoryPush(ctx context.Context, src *Repo, ref string, changed []string, event map[string]any, commit commitInfo) error {
	return s.triggerListeners(ctx, src, wf.RepositoryPush, event, commit.Author, src.Name+": "+commit.Subject, func(w *model.Workflow) bool {
		return wf.MatchesRepositoryPush(w, src.Name, ref, changed)
	})
}

// triggerListeners starts the workflows of other repositories' default
// branches that listen to src with event (repository_push or
// repository_pull_request) and that matches accepts.
func (s *Server) triggerListeners(ctx context.Context, src *Repo, event string, payload map[string]any, actor, title string,
	matches func(*model.Workflow) bool) error {
	rows, err := s.db.Query(`SELECT `+repoCols+` FROM repos WHERE id != ? ORDER BY name`, src.ID)
	if err != nil {
		return err
	}
	var listeners []*Repo
	for rows.Next() {
		repo, err := scanRepo(rows)
		if err != nil {
			rows.Close()
			return err
		}
		listeners = append(listeners, repo)
	}
	rows.Close()
	mentionsSrc := mentionsRepo(src.Name)
	var firstErr error
	for _, repo := range listeners {
		dstRef := "refs/heads/" + repo.DefaultBranch
		sha, err := resolveRef(ctx, s.repoPath(repo.Name), dstRef)
		if err != nil {
			continue // no commits on the default branch yet
		}
		files, err := s.defaultBranchWorkflows(ctx, repo, sha)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, file := range sortedKeys(files) {
			yaml := files[file]
			if !bytes.Contains(yaml, []byte(event)) {
				continue // only listeners need parsing
			}
			w, perr := wf.Parse(yaml)
			if perr != nil {
				// Report broken workflows that look like they listen to src.
				if mentionsSrc.Match(yaml) {
					_, _ = s.createRun(ctx, runSpec{repo: repo, file: file, yaml: string(yaml), name: file, event: event,
						payload: payload, ref: dstRef, sha: sha, actor: actor, title: title, parseErr: perr})
				}
				continue
			}
			if !matches(w) {
				continue
			}
			name := w.Name
			if name == "" {
				name = file
			}
			if _, err := s.createRun(ctx, runSpec{repo: repo, file: file, yaml: string(yaml), name: name, event: event,
				payload: payload, ref: dstRef, sha: sha, actor: actor, title: title}); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// mentionsRepo matches text that names the repository: name as a whole,
// not part of a longer name such as owner/name-deploy or other/owner/name.
func mentionsRepo(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_./-])` + regexp.QuoteMeta(name) + `($|[^A-Za-z0-9_./-])`)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

type workflowInfo struct {
	File     string             `json:"file"`
	Name     string             `json:"name"`
	Events   []string           `json:"events"`
	Dispatch bool               `json:"dispatch"`
	Inputs   []wf.DispatchInput `json:"inputs"`
	Error    string             `json:"error,omitempty"`
	Uploaded bool               `json:"uploaded,omitempty"`
}

func (s *Server) handleListWorkflows(w http.ResponseWriter, r *http.Request) {
	repo, err := s.getRepo(r.PathValue("owner") + "/" + r.PathValue("name"))
	if err != nil {
		httpError(w, 404, "no such repository")
		return
	}
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		ref = "refs/heads/" + repo.DefaultBranch
	}
	out := []workflowInfo{}
	// Uploaded workflows are listed even before the repository has commits.
	sha, _ := resolveRef(r.Context(), s.repoPath(repo.Name), ref)
	files, err := s.workflowsAt(r.Context(), repo, sha)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	for file, src := range files {
		info := workflowInfo{File: file, Name: file, Uploaded: strings.HasPrefix(file, uploadedPrefix)}
		parsed, err := wf.Parse(src)
		if err != nil {
			info.Error = err.Error()
		} else {
			if parsed.Name != "" {
				info.Name = parsed.Name
			}
			info.Events = parsed.On()
			info.Dispatch = wf.HasEvent(parsed, "workflow_dispatch")
			info.Inputs = wf.DispatchInputs(parsed)
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	writeJSON(w, out)
}

func (s *Server) handleDispatch(w http.ResponseWriter, r *http.Request) {
	repo, err := s.getRepo(r.PathValue("owner") + "/" + r.PathValue("name"))
	if err != nil {
		httpError(w, 404, "no such repository")
		return
	}
	var req struct {
		Workflow string            `json:"workflow"`
		Ref      string            `json:"ref"`
		Inputs   map[string]string `json:"inputs"`
	}
	if err := readJSON(r, &req); err != nil {
		httpError(w, 400, "bad request: %v", err)
		return
	}
	if req.Ref == "" {
		req.Ref = repo.DefaultBranch
	}
	if !strings.HasPrefix(req.Ref, "refs/") {
		req.Ref = "refs/heads/" + req.Ref
	}
	ctx := r.Context()
	path := s.repoPath(repo.Name)
	sha, err := resolveRef(ctx, path, req.Ref)
	if err != nil {
		httpError(w, 400, "unknown ref %s", req.Ref)
		return
	}
	files, err := s.workflowsAt(ctx, repo, sha)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	// A bare file name means the repository's file, then an uploaded one.
	var src []byte
	for _, file := range []string{req.Workflow, ".github/workflows/" + req.Workflow, uploadedPrefix + req.Workflow} {
		if src = files[file]; src != nil {
			req.Workflow = file
			break
		}
	}
	if src == nil {
		httpError(w, 404, "workflow %s not found at %s", req.Workflow, req.Ref)
		return
	}
	parsed, err := wf.Parse(src)
	if err != nil {
		httpError(w, 400, "invalid workflow: %v", err)
		return
	}
	if !wf.HasEvent(parsed, "workflow_dispatch") {
		httpError(w, 400, "workflow does not have a workflow_dispatch trigger")
		return
	}
	inputs := map[string]any{}
	for _, in := range wf.DispatchInputs(parsed) {
		v, set := req.Inputs[in.Name]
		if !set || v == "" {
			v = in.Default
		}
		if in.Required && v == "" {
			httpError(w, 400, "input %s is required", in.Name)
			return
		}
		switch in.Type {
		case "boolean":
			inputs[in.Name] = v == "true"
		case "number":
			var f float64
			if _, err := fmt.Sscan(v, &f); err == nil {
				inputs[in.Name] = f
			} else {
				inputs[in.Name] = v
			}
		default:
			inputs[in.Name] = v
		}
	}
	name := parsed.Name
	if name == "" {
		name = req.Workflow
	}
	event := map[string]any{
		"ref":        req.Ref,
		"inputs":     inputs,
		"workflow":   req.Workflow,
		"repository": repoEventInfo(repo, s.publicURL(r)),
		"sender":     map[string]any{"login": requestUser(r)},
	}
	commit := readCommit(ctx, path, sha)
	run, err := s.createRun(ctx, runSpec{repo: repo, file: req.Workflow, yaml: string(src), name: name, event: "workflow_dispatch",
		payload: event, ref: req.Ref, sha: sha, actor: requestUser(r), title: "Manually run: " + commit.Subject})
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	writeJSON(w, map[string]int64{"run_id": run})
}

func removeAll(path string) error {
	if path == "" || path == "/" {
		return errors.New("refusing to remove " + path)
	}
	return osRemoveAll(path)
}

var errNotFound = sql.ErrNoRows
