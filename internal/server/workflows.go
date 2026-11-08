package server

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"mime"
	"net/http"
	"regexp"
	"strings"

	"github.com/dire-kiwi/kiwicd/internal/wf"
)

// Uploaded workflows are stored in kiwicd rather than committed, for
// workflows that shouldn't live in the repository. They run on every commit
// of the repository, like files in its .github/workflows/; their file names
// carry this prefix so they never collide with those.
const uploadedPrefix = "kiwicd:"

var uploadedNameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*\.ya?ml$`)

type uploadedWorkflow struct {
	File      string `json:"file"`
	Content   string `json:"content"`
	UpdatedAt int64  `json:"updated_at"`
}

func (s *Server) uploadedWorkflows(repoID int64) ([]uploadedWorkflow, error) {
	rows, err := s.db.Query(`SELECT file, content, updated_at FROM uploaded_workflows WHERE repo_id = ? ORDER BY file`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uploadedWorkflow
	for rows.Next() {
		var u uploadedWorkflow
		if err := rows.Scan(&u.File, &u.Content, &u.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// workflowsAt returns the workflows that apply to a commit: the repository's
// own files at sha (none if sha is empty or the repository's repo_workflows
// setting is off) and the uploaded ones.
func (s *Server) workflowsAt(ctx context.Context, repo *Repo, sha string) (map[string][]byte, error) {
	files := map[string][]byte{}
	if sha != "" && repo.RepoWorkflows {
		var err error
		if files, err = workflowFiles(ctx, s.repoPath(repo.Name), sha); err != nil {
			return nil, err
		}
	}
	return s.addUploadedWorkflows(repo, files)
}

// defaultBranchWorkflows is workflowsAt for the commit sha at the tip of
// repo's default branch. Every push looks for repository_push listeners
// there in every repository, so the committed files are kept for each
// repository's latest tip: a commit's files never change.
func (s *Server) defaultBranchWorkflows(ctx context.Context, repo *Repo, sha string) (map[string][]byte, error) {
	if !repo.RepoWorkflows {
		return s.addUploadedWorkflows(repo, map[string][]byte{})
	}
	s.tipMu.Lock()
	tip, ok := s.tipWorkflows[repo.ID]
	s.tipMu.Unlock()
	if !ok || tip.sha != sha {
		files, err := workflowFiles(ctx, s.repoPath(repo.Name), sha)
		if err != nil {
			return nil, err
		}
		tip = cachedWorkflows{sha: sha, files: files}
		s.tipMu.Lock()
		s.tipWorkflows[repo.ID] = tip
		s.tipMu.Unlock()
	}
	return s.addUploadedWorkflows(repo, maps.Clone(tip.files))
}

type cachedWorkflows struct {
	sha   string
	files map[string][]byte
}

func (s *Server) addUploadedWorkflows(repo *Repo, files map[string][]byte) (map[string][]byte, error) {
	uploaded, err := s.uploadedWorkflows(repo.ID)
	if err != nil {
		return nil, err
	}
	for _, u := range uploaded {
		files[uploadedPrefix+u.File] = []byte(u.Content)
	}
	return files, nil
}

func (s *Server) handleGetUploadedWorkflow(w http.ResponseWriter, r *http.Request) {
	repo, err := s.getRepo(r.PathValue("owner") + "/" + r.PathValue("name"))
	if err != nil {
		httpError(w, 404, "no such repository")
		return
	}
	u := uploadedWorkflow{File: r.PathValue("file")}
	err = s.db.QueryRow(`SELECT content, updated_at FROM uploaded_workflows WHERE repo_id = ? AND file = ?`, repo.ID, u.File).Scan(&u.Content, &u.UpdatedAt)
	if err != nil {
		httpError(w, 404, "no uploaded workflow %s", u.File)
		return
	}
	writeJSON(w, u)
}

// handlePutUploadedWorkflow creates or replaces an uploaded workflow. The body
// is the YAML itself (curl -T deploy.yml), or {"content": "..."} as JSON.
func (s *Server) handlePutUploadedWorkflow(w http.ResponseWriter, r *http.Request) {
	repo, err := s.getRepo(r.PathValue("owner") + "/" + r.PathValue("name"))
	if err != nil {
		httpError(w, 404, "no such repository")
		return
	}
	file := r.PathValue("file")
	if !uploadedNameRE.MatchString(file) {
		httpError(w, 400, "the file name must end in .yml or .yaml and contain only letters, digits, '.', '-' and '_'")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		httpError(w, 400, "bad request: %v", err)
		return
	}
	content := string(body)
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct == "application/json" {
		var req struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			httpError(w, 400, "bad request: %v", err)
			return
		}
		content = req.Content
	}
	if strings.TrimSpace(content) == "" {
		httpError(w, 400, "the workflow is empty")
		return
	}
	parsed, err := wf.Parse([]byte(content))
	if err != nil {
		httpError(w, 400, "invalid workflow: %v", err)
		return
	}
	for _, sch := range wf.Schedules(parsed) {
		if _, err := sch.Parse(); err != nil {
			httpError(w, 400, "invalid workflow: schedule: %v", err)
			return
		}
	}
	u := uploadedWorkflow{File: file, Content: content, UpdatedAt: nowMS()}
	_, err = s.db.Exec(`INSERT INTO uploaded_workflows (repo_id, file, content, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (repo_id, file) DO UPDATE SET content = excluded.content, updated_at = excluded.updated_at`, repo.ID, u.File, u.Content, u.UpdatedAt)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	s.audit(r, "workflow.upload", repo.Name, u.File, "")
	s.schedulesChanged(repo.ID)
	writeJSON(w, u)
}

func (s *Server) handleDeleteUploadedWorkflow(w http.ResponseWriter, r *http.Request) {
	repo, err := s.getRepo(r.PathValue("owner") + "/" + r.PathValue("name"))
	if err != nil {
		httpError(w, 404, "no such repository")
		return
	}
	res, err := s.db.Exec(`DELETE FROM uploaded_workflows WHERE repo_id = ? AND file = ?`, repo.ID, r.PathValue("file"))
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpError(w, 404, "no uploaded workflow %s", r.PathValue("file"))
		return
	}
	s.audit(r, "workflow.delete", repo.Name, r.PathValue("file"), "")
	s.schedulesChanged(repo.ID)
	writeJSON(w, map[string]bool{"ok": true})
}
