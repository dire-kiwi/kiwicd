package server

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
)

type Runner struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	Labels     []string `json:"labels"`
	OS         string   `json:"os"`
	Arch       string   `json:"arch"`
	Version    string   `json:"version"`
	CreatedAt  int64    `json:"created_at"`
	LastSeenAt int64    `json:"last_seen_at"`
	Online     bool     `json:"online"`
	Busy       bool     `json:"busy"`
	JobID      int64    `json:"job_id,omitempty"`
	RunID      int64    `json:"run_id,omitempty"`
	JobName    string   `json:"job_name,omitempty"`
	// Reserved lists who may use the runner's reserved labels.
	Reserved map[string][]string `json:"reserved,omitempty"`
	// ProtectedLabels are the runner's labels that keep it for jobs from
	// protected refs.
	ProtectedLabels []string `json:"protected_labels,omitempty"`
}

func (s *Server) runnerFromRequest(r *http.Request) (*Runner, error) {
	token := r.Header.Get(protocol.RunnerTokenHeader)
	if token == "" {
		return nil, errors.New("missing runner token")
	}
	var rn Runner
	var labels string
	err := s.db.QueryRow(`SELECT id, name, labels, os, arch, version FROM runners WHERE token_hash = ?`, hashToken(token)).
		Scan(&rn.ID, &rn.Name, &labels, &rn.OS, &rn.Arch, &rn.Version)
	if err != nil {
		return nil, errors.New("unknown runner token; register the agent again")
	}
	_ = json.Unmarshal([]byte(labels), &rn.Labels)
	_, _ = s.db.Exec(`UPDATE runners SET last_seen_at = ? WHERE id = ?`, nowMS(), rn.ID)
	return &rn, nil
}

// normalizeLabels adds GitHub's implicit self-hosted runner labels.
func normalizeLabels(labels []string, osName, arch string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(l string) {
		l = strings.TrimSpace(l)
		if l == "" || seen[strings.ToLower(l)] {
			return
		}
		seen[strings.ToLower(l)] = true
		out = append(out, l)
	}
	add("self-hosted")
	switch osName {
	case "linux":
		add("Linux")
	case "darwin":
		add("macOS")
	case "windows":
		add("Windows")
	}
	switch arch {
	case "amd64":
		add("X64")
	case "arm64":
		add("ARM64")
	}
	for _, l := range labels {
		add(l)
	}
	return out
}

func (s *Server) handleRunnerRegister(w http.ResponseWriter, r *http.Request) {
	var req protocol.RegisterRequest
	if err := readJSON(r, &req); err != nil {
		httpError(w, 400, "bad request: %v", err)
		return
	}
	if req.Protocol != protocol.Version {
		httpError(w, 400, "agent protocol %d is not supported (server speaks %d); upgrade the agent", req.Protocol, protocol.Version)
		return
	}
	var exp int64
	err := s.db.QueryRow(`SELECT expires_at FROM registration_tokens WHERE token_hash = ?`, hashToken(req.Token)).Scan(&exp)
	static := s.cfg.RegistrationToken != "" && subtle.ConstantTimeCompare([]byte(req.Token), []byte(s.cfg.RegistrationToken)) == 1
	if !static && (err != nil || exp < nowMS()) {
		httpError(w, 401, "invalid or expired registration token")
		return
	}
	if req.Name == "" {
		httpError(w, 400, "runner name is required")
		return
	}
	labels := normalizeLabels(req.Labels, req.OS, req.Arch)
	token := randomToken(32)
	// Re-registering a name replaces the old runner, as `config.sh --replace` does.
	_, _ = s.db.Exec(`DELETE FROM runners WHERE name = ?`, req.Name)
	res, err := s.db.Exec(`INSERT INTO runners (name, labels, token_hash, os, arch, version, created_at, last_seen_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		req.Name, mustJSON(labels), hashToken(token), req.OS, req.Arch, req.Version, nowMS(), nowMS())
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	id, _ := res.LastInsertId()
	s.log.Info("runner registered", "name", req.Name, "labels", labels)
	s.hub.publish("runs", "runners", nil)
	writeJSON(w, protocol.RegisterResponse{RunnerID: id, RunnerToken: token, Name: req.Name, Labels: labels})
}

func labelsMatch(want, have []string) bool {
	set := map[string]bool{}
	for _, h := range have {
		set[strings.ToLower(h)] = true
	}
	for _, l := range want {
		if !set[strings.ToLower(l)] {
			return false
		}
	}
	return true
}

func (s *Server) handleRunnerPoll(w http.ResponseWriter, r *http.Request) {
	rn, err := s.runnerFromRequest(r)
	if err != nil {
		httpError(w, 401, "%v", err)
		return
	}
	var req protocol.PollRequest
	_ = readJSON(r, &req)
	if req.Protocol != protocol.Version {
		httpError(w, 400, "agent protocol %d is not supported (server speaks %d)", req.Protocol, protocol.Version)
		return
	}
	if req.Version != "" && req.Version != rn.Version {
		_, _ = s.db.Exec(`UPDATE runners SET version = ? WHERE id = ?`, req.Version, rn.ID)
	}
	deadline := time.NewTimer(25 * time.Second)
	defer deadline.Stop()
	for {
		ready := s.jobsReadyChan()
		job, err := s.assignJob(r.Context(), rn, s.publicURL(r), req.Unseal)
		if err != nil {
			httpError(w, 500, "%v", err)
			return
		}
		if job != nil {
			writeJSON(w, protocol.PollResponse{Job: job})
			return
		}
		select {
		case <-ready:
		case <-deadline.C:
			writeJSON(w, protocol.PollResponse{})
			return
		case <-r.Context().Done():
			return
		}
	}
}

// assignJob gives the oldest queued job whose runs-on labels the runner has
// and whose run may use the runner's reserved and protected labels. unseal
// says the agent has a secrets service for sealed secrets.
func (s *Server) assignJob(ctx context.Context, rn *Runner, baseURL string, unseal bool) (*protocol.JobRequest, error) {
	s.mu.Lock()
	rows, err := s.db.Query(`SELECT `+jobCols+` FROM jobs WHERE status = ? ORDER BY id`, protocol.StatusQueued)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	var candidates []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			rows.Close()
			s.mu.Unlock()
			return nil, err
		}
		if labelsMatch(j.RunsOn, rn.Labels) {
			candidates = append(candidates, j)
		}
	}
	rows.Close()
	var picked *Job
	// Jobs whose run may no longer use the labels they ask for, say because
	// their ref stopped being protected after they were queued, would wait
	// forever: fail them.
	denied := map[int64]string{}
	limited := s.reserved.reservesAny(rn.Labels) || len(s.protectedLabels.of(rn.Labels)) > 0
	for _, j := range candidates {
		if limited {
			run, err := s.getRun(j.RunID)
			if err != nil {
				continue
			}
			if msg := s.labelDenied(run, j.RunsOn); msg != "" {
				denied[j.ID] = msg
				continue
			}
			if !s.runnerTakes(rn, run) {
				continue
			}
		}
		picked = j
		break
	}
	if picked == nil {
		s.mu.Unlock()
		s.failDenied(ctx, denied)
		return nil, nil
	}
	now := nowMS()
	_, err = s.db.Exec(`UPDATE jobs SET status = ?, runner_id = ?, runner_name = ?, started_at = ?, heartbeat_at = ? WHERE id = ?`,
		protocol.StatusInProgress, rn.ID, rn.Name, now, now, picked.ID)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	req, err := s.buildJobRequest(picked, rn, baseURL, unseal)
	s.mu.Unlock()
	s.failDenied(ctx, denied)
	if err != nil {
		_ = s.completeJob(ctx, picked.ID, protocol.ResultFailure, err.Error(), nil, nil)
		return nil, nil
	}
	s.log.Info("job assigned", "job", picked.ID, "name", picked.Name, "runner", rn.Name)
	if j, err := s.getJob(picked.ID); err == nil {
		s.publishJob(j)
	}
	_ = s.advance(ctx, picked.RunID)
	return req, nil
}

func (s *Server) failDenied(ctx context.Context, denied map[int64]string) {
	for id, msg := range denied {
		_ = s.completeJob(ctx, id, protocol.ResultFailure, msg, nil, nil)
	}
}

func (s *Server) buildJobRequest(j *Job, rn *Runner, baseURL string, unseal bool) (*protocol.JobRequest, error) {
	run, err := s.getRun(j.RunID)
	if err != nil {
		return nil, err
	}
	jobs, err := s.runJobs(run.ID)
	if err != nil {
		return nil, err
	}
	byKey := map[string][]*Job{}
	for _, other := range jobs {
		if other.Key != j.Key {
			byKey[other.Key] = append(byKey[other.Key], other)
		}
	}
	results := map[string]protocol.JobResult{}
	for key, rows := range byKey {
		if r, done := aggregate(rows); done {
			results[key] = protocol.JobResult{Result: r.Result, Outputs: r.Outputs}
		}
	}
	owner, _, _ := strings.Cut(run.Repo, "/")
	server := strings.TrimRight(baseURL, "/")
	if s.cfg.PublicURL != "" {
		server = strings.TrimRight(s.cfg.PublicURL, "/")
	}
	ghc := s.githubContext(run)
	env := map[string]string{
		"GITHUB_RUN_ID":           fmt.Sprint(run.ID),
		"GITHUB_RUN_NUMBER":       fmt.Sprint(run.Number),
		"GITHUB_RUN_ATTEMPT":      fmt.Sprint(run.Attempt),
		"GITHUB_REPOSITORY":       run.Repo,
		"GITHUB_REPOSITORY_OWNER": owner,
		"GITHUB_REF":              run.Ref,
		"GITHUB_REF_NAME":         ghc.RefName,
		"GITHUB_REF_TYPE":         ghc.RefType,
		"GITHUB_SHA":              run.Sha,
		"SHA_REF":                 run.Sha,
		"GITHUB_SERVER_URL":       server,
		"GITHUB_RETENTION_DAYS":   fmt.Sprint(s.cfg.RetentionDays),
		"KIWICD":                  "true",
		"KIWICD_SERVER_URL":       server,
		"KIWICD_RUN_URL":          fmt.Sprintf("%s/runs/%d", server, run.ID),
	}
	deploy, err := s.jobEnvironment(run, j)
	if err != nil {
		return nil, err
	}
	secrets := map[string]string{}
	var sealedNames, notices []string
	if j.Environment != "" && deploy == nil {
		notices = append(notices, fmt.Sprintf("%s has no environment called %s, so this job gets no environment secrets or variables. "+
			"Environments are set up in the repository's Environments tab.", run.Repo, j.Environment))
	}
	if s.runGetsSecrets(run) {
		var sealedSecrets []protocol.SealedSecret
		if secrets, sealedSecrets, err = s.jobSecrets(run.RepoID, deploy); err != nil {
			return nil, err
		}
		for _, sec := range sealedSecrets {
			sealedNames = append(sealedNames, sec.Name)
		}
		if len(sealedNames) > 0 && !unseal {
			return nil, fmt.Errorf("this job's secrets %s are sealed for a secrets service, but runner %s has none "+
				"(kiwicd-agent run --secrets-url), or its agent is too old to use one", strings.Join(sealedNames, ", "), rn.Name)
		}
	} else {
		notices = append(notices, fmt.Sprintf("%s is not a protected ref of %s, so this job gets no secrets except KIWICD_TOKEN. "+
			"Protected refs are set in the repository's Settings.", run.Ref, run.Repo))
	}
	token, err := s.issueJobToken(j.ID)
	if err != nil {
		return nil, err
	}
	secrets[jobTokenSecret] = token
	return &protocol.JobRequest{
		JobID:          j.ID,
		RunID:          run.ID,
		RunNumber:      run.Number,
		RunAttempt:     run.Attempt,
		JobKey:         j.Key,
		DisplayName:    j.Name,
		WorkflowFile:   run.WorkflowFile,
		WorkflowYAML:   run.WorkflowYAML,
		Matrix:         j.Matrix,
		JobResults:     results,
		Repository:     run.Repo,
		Sha:            run.Sha,
		Ref:            run.Ref,
		EventName:      run.Event,
		Event:          run.EventJSON,
		Actor:          run.Actor,
		Environment:    j.Environment,
		Secrets:        secrets,
		SealedSecrets:  sealedNames,
		Vars:           s.varsFor(run.RepoID, deploy),
		Notices:        notices,
		Env:            env,
		TimeoutMinutes: 360,
		ServerURL:      server,
		SourcePath:     fmt.Sprintf("/api/runner/jobs/%d/source", j.ID),
	}, nil
}

func (s *Server) runnerJob(w http.ResponseWriter, r *http.Request) (*Runner, *Job, bool) {
	rn, err := s.runnerFromRequest(r)
	if err != nil {
		httpError(w, 401, "%v", err)
		return nil, nil, false
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	j, err := s.getJob(id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && j.RunnerID != rn.ID) {
		httpError(w, 404, "job %d is not assigned to this runner", id)
		return nil, nil, false
	}
	if err != nil {
		httpError(w, 500, "%v", err)
		return nil, nil, false
	}
	return rn, j, true
}

func (s *Server) handleRunnerUpdate(w http.ResponseWriter, r *http.Request) {
	_, j, ok := s.runnerJob(w, r)
	if !ok {
		return
	}
	var req protocol.JobUpdate
	if err := readJSON(r, &req); err != nil {
		httpError(w, 400, "bad request: %v", err)
		return
	}
	if j.Status == protocol.StatusCompleted {
		// Cancelled (or reaped) server-side: tell the agent to stop.
		writeJSON(w, protocol.JobUpdateResponse{Cancelled: true})
		return
	}
	if err := s.appendLogs(j.ID, req.Logs); err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	if req.Steps != nil {
		_, _ = s.db.Exec(`UPDATE jobs SET steps = ?, heartbeat_at = ? WHERE id = ?`, mustJSON(req.Steps), nowMS(), j.ID)
		j.Steps = req.Steps
		s.publishJob(j)
	} else {
		_, _ = s.db.Exec(`UPDATE jobs SET heartbeat_at = ? WHERE id = ?`, nowMS(), j.ID)
	}
	writeJSON(w, protocol.JobUpdateResponse{Cancelled: j.Cancel})
}

func (s *Server) handleRunnerComplete(w http.ResponseWriter, r *http.Request) {
	_, j, ok := s.runnerJob(w, r)
	if !ok {
		return
	}
	var req protocol.JobComplete
	if err := readJSON(r, &req); err != nil {
		httpError(w, 400, "bad request: %v", err)
		return
	}
	if err := s.appendLogs(j.ID, req.Logs); err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	switch req.Result {
	case protocol.ResultSuccess, protocol.ResultFailure, protocol.ResultCancelled, protocol.ResultSkipped:
	default:
		req.Result = protocol.ResultFailure
	}
	if err := s.completeJob(r.Context(), j.ID, req.Result, "", req.Outputs, req.Steps); err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleRunnerSource(w http.ResponseWriter, r *http.Request) {
	_, j, ok := s.runnerJob(w, r)
	if !ok {
		return
	}
	run, err := s.getRun(j.RunID)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	if err := archive(r.Context(), s.repoPath(run.Repo), run.Sha, w); err != nil {
		s.log.Error("source archive", "job", j.ID, "err", err)
	}
}
