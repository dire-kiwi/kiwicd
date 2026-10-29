package server

import (
	"net/http"
	"strings"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
	"github.com/dire-kiwi/kiwicd/internal/wf"
)

// Every job gets a token, secrets.KIWICD_TOKEN, that can fetch repositories
// over /git/ while the job runs:
//
//	git -c http.extraHeader="Authorization: Bearer $KIWICD_TOKEN" clone "$GITHUB_SERVER_URL/git/OWNER/NAME.git"
//
// It reads the job's own repository and, for runs started by repository_push,
// repository_pull_request or by hand, the repositories the workflow listens
// to with either event that list the job's repository among their readers.
// Push runs get only their own repository: their workflow can come from any
// branch, and a listener added on a branch of a public repository mustn't
// open a private one to it. repository_push and repository_pull_request runs
// take their workflow from the listener's default branch, and dispatches are
// started by the admin. They only get other repositories when their ref is
// protected, like secrets (see protectedrefs.go).
//
// Readers are set on the repository being read, so that a listener can't
// open a repository to itself: any repository's default branch can name
// another under repository_push, including a public mirror's, where outside
// contributions land.
const jobTokenSecret = "KIWICD_TOKEN"

func (s *Server) issueJobToken(jobID int64) (string, error) {
	token := randomToken(32)
	_, err := s.db.Exec(`UPDATE jobs SET token_hash = ? WHERE id = ?`, hashToken(token), jobID)
	return token, err
}

// jobCanRead reports whether the request carries the token of a running job
// that may read the repository called name.
func (s *Server) jobCanRead(r *http.Request, name string) bool {
	var token string
	if _, p, ok := r.BasicAuth(); ok {
		token = p
	} else if t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		token = t
	}
	if token == "" || name == "" {
		return false
	}
	var runID int64
	err := s.db.QueryRow(`SELECT run_id FROM jobs WHERE token_hash = ? AND status = ?`, hashToken(token), protocol.StatusInProgress).Scan(&runID)
	if err != nil {
		return false
	}
	run, err := s.getRun(runID)
	if err != nil {
		return false
	}
	for _, repo := range s.jobReadableRepos(run) {
		if strings.EqualFold(repo, name) {
			return true
		}
	}
	return false
}

func (s *Server) jobReadableRepos(run *Run) []string {
	repos := []string{run.Repo}
	if run.Event != wf.RepositoryPush && run.Event != wf.RepositoryPullRequest && run.Event != "workflow_dispatch" || !s.runGetsSecrets(run) {
		return repos
	}
	w, err := wf.Parse([]byte(run.WorkflowYAML))
	if err != nil {
		return repos
	}
	for _, name := range wf.ListenedRepositories(w) {
		if src, err := s.getRepo(name); err == nil && containsFold(src.Readers, run.Repo) {
			repos = append(repos, src.Name)
		}
	}
	return repos
}
