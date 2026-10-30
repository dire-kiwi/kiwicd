package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// Environments are a repository's deployment targets, as on GitHub. A job
// names one with jobs.<id>.environment (a name, or a mapping with name and
// url) and then gets the environment's secrets and variables on top of the
// repository's and the global ones, which they override.
//
// An environment's refs limit which runs may use it: a job of a run from
// another ref fails, as GitHub's deployment branch policies do. With no refs
// any run may use it. Either way its secrets go only to runs that get
// secrets at all, from protected refs (see protectedrefs.go), so refs only
// narrow who gets them: a production environment for the default branch
// only, say, while release branches are protected for other secrets.
//
// A job naming an environment that doesn't exist runs without environment
// secrets or variables, like a job naming none.

type Environment struct {
	ID        int64    `json:"id"`
	RepoID    int64    `json:"-"`
	Name      string   `json:"name"`
	Refs      []string `json:"refs"`
	CreatedAt int64    `json:"created_at"`
	// How many secrets and variables it has, for the UI.
	Secrets   int `json:"secrets"`
	Variables int `json:"variables"`
}

const environmentCols = `id, repo_id, name, refs, created_at,
	(SELECT count(*) FROM environment_secrets WHERE environment_id = environments.id),
	(SELECT count(*) FROM environment_variables WHERE environment_id = environments.id)`

func scanEnvironment(row interface{ Scan(...any) error }) (*Environment, error) {
	var e Environment
	var refs string
	if err := row.Scan(&e.ID, &e.RepoID, &e.Name, &refs, &e.CreatedAt, &e.Secrets, &e.Variables); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(refs), &e.Refs)
	if e.Refs == nil {
		e.Refs = []string{}
	}
	return &e, nil
}

// getEnvironment finds a repository's environment by name, ignoring case.
func (s *Server) getEnvironment(repoID int64, name string) (*Environment, error) {
	return scanEnvironment(s.db.QueryRow(`SELECT `+environmentCols+` FROM environments WHERE repo_id = ? AND name = ?`, repoID, name))
}

func (s *Server) listEnvironments(repoID int64) ([]*Environment, error) {
	rows, err := s.db.Query(`SELECT `+environmentCols+` FROM environments WHERE repo_id = ? ORDER BY name`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Environment{}
	for rows.Next() {
		e, err := scanEnvironment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

var environmentNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]([A-Za-z0-9 _.-]*[A-Za-z0-9_.-])?$`)

func checkEnvironmentName(name string) error {
	if len(name) > 100 || !environmentNameRE.MatchString(name) {
		return errors.New("environment names may contain letters, digits, spaces, '.', '-' and '_', up to 100 characters, and must not start or end with a space")
	}
	return nil
}

// environmentDenied explains why a run may not use env, or returns "".
func environmentDenied(env *Environment, run *Run) string {
	if env == nil || len(env.Refs) == 0 || refMatches(env.Refs, run.Ref) {
		return ""
	}
	return fmt.Sprintf("environment %s: %s may not use this environment; it is limited to %s. Its refs are set in the repository's Environments tab.",
		env.Name, run.Ref, strings.Join(env.Refs, ", "))
}

// jobEnvironment returns the environment a job uses, nil if it names none or
// one that doesn't exist, or an error if the run may not use it.
func (s *Server) jobEnvironment(run *Run, j *Job) (*Environment, error) {
	if j.Environment == "" {
		return nil, nil
	}
	env, err := s.getEnvironment(run.RepoID, j.Environment)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if msg := environmentDenied(env, run); msg != "" {
		return nil, errors.New(msg)
	}
	return env, nil
}

// ---------------------------------------------------------------- API

func (s *Server) pathRepo(w http.ResponseWriter, r *http.Request) (*Repo, bool) {
	repo, err := s.getRepo(r.PathValue("owner") + "/" + r.PathValue("name"))
	if err != nil {
		httpError(w, 404, "no such repository")
		return nil, false
	}
	return repo, true
}

func (s *Server) pathEnvironment(w http.ResponseWriter, r *http.Request) (*Repo, *Environment, bool) {
	repo, ok := s.pathRepo(w, r)
	if !ok {
		return nil, nil, false
	}
	env, err := s.getEnvironment(repo.ID, r.PathValue("env"))
	if errors.Is(err, sql.ErrNoRows) {
		httpError(w, 404, "no such environment")
		return nil, nil, false
	}
	if err != nil {
		httpError(w, 500, "%v", err)
		return nil, nil, false
	}
	return repo, env, true
}

func (s *Server) handleListEnvironments(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.pathRepo(w, r)
	if !ok {
		return
	}
	envs, err := s.listEnvironments(repo.ID)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	writeJSON(w, envs)
}

func (s *Server) handleCreateEnvironment(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.pathRepo(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string   `json:"name"`
		Refs []string `json:"refs"`
	}
	if err := readJSON(r, &req); err != nil {
		httpError(w, 400, "bad request: %v", err)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if err := checkEnvironmentName(req.Name); err != nil {
		httpError(w, 400, "%v", err)
		return
	}
	refs, err := cleanRefPatterns(req.Refs)
	if err != nil {
		httpError(w, 400, "%v", err)
		return
	}
	if _, err := s.getEnvironment(repo.ID, req.Name); err == nil {
		httpError(w, 409, "%s already has an environment called %s", repo.Name, req.Name)
		return
	}
	if _, err := s.db.Exec(`INSERT INTO environments (repo_id, name, refs, created_at) VALUES (?, ?, ?, ?)`,
		repo.ID, req.Name, mustJSON(refs), nowMS()); err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	s.log.Info("environment created", "repo", repo.Name, "environment", req.Name, "refs", refs)
	env, err := s.getEnvironment(repo.ID, req.Name)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	writeJSON(w, env)
}

func (s *Server) handleUpdateEnvironment(w http.ResponseWriter, r *http.Request) {
	repo, env, ok := s.pathEnvironment(w, r)
	if !ok {
		return
	}
	var req struct {
		Refs *[]string `json:"refs"`
	}
	if err := readJSON(r, &req); err != nil {
		httpError(w, 400, "bad request: %v", err)
		return
	}
	if req.Refs != nil {
		refs, err := cleanRefPatterns(*req.Refs)
		if err != nil {
			httpError(w, 400, "%v", err)
			return
		}
		if _, err := s.db.Exec(`UPDATE environments SET refs = ? WHERE id = ?`, mustJSON(refs), env.ID); err != nil {
			httpError(w, 500, "%v", err)
			return
		}
		s.log.Info("environment updated", "repo", repo.Name, "environment", env.Name, "refs", refs)
	}
	env, err := s.getEnvironment(repo.ID, env.Name)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	writeJSON(w, env)
}

func (s *Server) handleDeleteEnvironment(w http.ResponseWriter, r *http.Request) {
	repo, env, ok := s.pathEnvironment(w, r)
	if !ok {
		return
	}
	// Its secrets and variables go with it (ON DELETE CASCADE).
	if _, err := s.db.Exec(`DELETE FROM environments WHERE id = ?`, env.ID); err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	s.log.Info("environment deleted", "repo", repo.Name, "environment", env.Name)
	writeJSON(w, map[string]bool{"ok": true})
}
