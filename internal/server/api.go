package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
	"github.com/dire-kiwi/kiwicd/internal/wf"
	"github.com/nektos/act/pkg/model"
)

var osRemoveAll = os.RemoveAll

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	q := `SELECT ` + runCols + ` FROM runs r JOIN repos p ON p.id = r.repo_id`
	var args []any
	if repo := r.URL.Query().Get("repo"); repo != "" {
		q += ` WHERE p.name = ?`
		args = append(args, repo)
	}
	limit := 50
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 500 {
		limit = l
	}
	q += fmt.Sprintf(` ORDER BY r.id DESC LIMIT %d`, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	defer rows.Close()
	runs := []*Run{}
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			httpError(w, 500, "%v", err)
			return
		}
		run.WorkflowYAML = ""
		run.EventJSON = nil
		runs = append(runs, run)
	}
	writeJSON(w, runs)
}

type plannedJob struct {
	Key   string   `json:"key"`
	Name  string   `json:"name"`
	Needs []string `json:"needs"`
	// For a job that calls a reusable workflow: what it calls, and that
	// workflow's jobs.
	Uses string       `json:"uses,omitempty"`
	Jobs []plannedJob `json:"jobs,omitempty"`
}

// planOf lists a workflow's jobs in order; path is the calledWorkflow path
// of the workflow ("" for the run's own).
func planOf(w *model.Workflow, path string, called map[string]*calledWorkflow) []plannedJob {
	plan := []plannedJob{}
	order, _ := wf.Order(w)
	for _, key := range order {
		j := w.GetJob(key)
		name := j.Name
		if name == "" {
			name = key
		}
		needs := j.Needs()
		if needs == nil {
			needs = []string{}
		}
		p := plannedJob{Key: key, Name: name, Needs: needs, Uses: j.Uses}
		if c := called[strings.TrimPrefix(path+"/"+key, "/")]; c != nil && j.Uses != "" {
			p.Jobs = planOf(c.parsed, c.Path, called)
		}
		plan = append(plan, p)
	}
	return plan
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.getRun(pathID(r))
	if err != nil {
		httpError(w, 404, "no such run")
		return
	}
	jobs, err := s.runJobs(run.ID)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	if jobs == nil {
		jobs = []*Job{}
	}
	plan := []plannedJob{}
	if parsed, err := wf.Parse([]byte(run.WorkflowYAML)); err == nil {
		called, _ := s.calledWorkflows(run.ID)
		plan = planOf(parsed, "", called)
	}
	// Runners with these labels won't take the run's jobs, as its ref isn't
	// protected.
	withheld := []string{}
	if len(s.protectedLabels) > 0 && !s.runGetsSecrets(run) {
		for _, l := range s.protectedLabels {
			withheld = append(withheld, l)
		}
		sort.Strings(withheld)
	}
	writeJSON(w, map[string]any{"run": run, "jobs": jobs, "plan": plan, "protected_labels": withheld})
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	if err := s.cancelRun(r.Context(), pathID(r)); err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleRerun(w http.ResponseWriter, r *http.Request) {
	run, err := s.getRun(pathID(r))
	if err != nil {
		httpError(w, 404, "no such run")
		return
	}
	repo, err := s.getRepoByID(run.RepoID)
	if err != nil {
		httpError(w, 404, "repository is gone")
		return
	}
	var attempt int64
	_ = s.db.QueryRow(`SELECT MAX(attempt) FROM runs WHERE repo_id = ? AND number = ?`, run.RepoID, run.Number).Scan(&attempt)
	// It calls the same reusable workflows, unless reading them failed.
	called, err := s.calledWorkflows(run.ID)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	var list []*calledWorkflow
	for _, path := range sortedKeys(called) {
		list = append(list, called[path])
	}
	id, err := s.createRun(r.Context(), runSpec{repo: repo, file: run.WorkflowFile, yaml: run.WorkflowYAML, name: run.WorkflowName,
		event: run.Event, payload: run.EventJSON, ref: run.Ref, sha: run.Sha, actor: run.Actor, title: run.Title,
		number: run.Number, attempt: attempt + 1, called: list})
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	writeJSON(w, map[string]int64{"run_id": id})
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.getJob(pathID(r))
	if err != nil {
		httpError(w, 404, "no such job")
		return
	}
	writeJSON(w, job)
}

func sseStart(w http.ResponseWriter) (func(typ string, data any), bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher.Flush()
	return func(typ string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, b)
		flusher.Flush()
	}, true
}

// handleJobEvents streams a job's logs and state: the backlog after ?after=
// first (in pages), then live updates. Lines are sent in seq order without
// gaps; if the live feed skips any (the subscriber fell behind and was
// dropped, or batches were published out of order) the missing ones are
// refilled from the stored logs.
func (s *Server) handleJobEvents(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	job, err := s.getJob(id)
	if err != nil {
		httpError(w, 404, "no such job")
		return
	}
	topic := fmt.Sprintf("job:%d", id)
	ch, unsub := s.hub.subscribe(topic)
	defer func() { unsub() }()
	send, ok := sseStart(w)
	if !ok {
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	// catchUp sends every stored line after the last one sent, a page at a
	// time.
	catchUp := func() bool {
		for {
			backlog, more, err := s.logPage(id, after, 0)
			if err != nil {
				return false
			}
			if len(backlog) > 0 {
				send("logs", backlog)
				after = backlog[len(backlog)-1].Seq
			}
			if !more {
				return true
			}
			if r.Context().Err() != nil {
				return false
			}
		}
	}
	if !catchUp() {
		return
	}
	send("job", job)
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			w.(http.Flusher).Flush()
		case ev, ok := <-ch:
			if !ok {
				// Dropped for falling behind: resubscribe, then refill
				// whatever was missed and resend the job's state.
				ch, unsub = s.hub.subscribe(topic)
				if !catchUp() {
					return
				}
				if job, err := s.getJob(id); err == nil {
					send("job", job)
				}
				continue
			}
			if ev.Type == "logs" {
				lines, _ := ev.Data.([]logLine)
				var fresh []logLine
				for _, l := range lines {
					if l.Seq <= after {
						continue
					}
					if l.Seq != after+1 {
						// A gap: the stored logs have everything committed
						// so far, including this batch.
						if len(fresh) > 0 {
							send("logs", fresh)
							fresh = nil
						}
						if !catchUp() {
							return
						}
						if l.Seq <= after {
							continue
						}
					}
					fresh = append(fresh, l)
					after = l.Seq
				}
				if len(fresh) > 0 {
					send("logs", fresh)
				}
				continue
			}
			send(ev.Type, ev.Data)
		}
	}
}

// handleEvents streams run and runner changes for list views. ?run=<id>
// narrows it to one run (including its jobs). The events are hints to
// refetch, so if the subscriber falls behind the stream simply ends: the
// browser reconnects and the client refetches on reopen.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	topic := "runs"
	if id := r.URL.Query().Get("run"); id != "" {
		topic = "run:" + id
	}
	ch, unsub := s.hub.subscribe(topic)
	defer unsub()
	send, ok := sseStart(w)
	if !ok {
		return
	}
	send("hello", map[string]string{"topic": topic})
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			w.(http.Flusher).Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			send(ev.Type, ev.Data)
		}
	}
}

func (s *Server) handleListRunners(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT id, name, labels, os, arch, version, created_at, last_seen_at FROM runners ORDER BY name`)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	runners := []*Runner{}
	for rows.Next() {
		var rn Runner
		var labels string
		if err := rows.Scan(&rn.ID, &rn.Name, &labels, &rn.OS, &rn.Arch, &rn.Version, &rn.CreatedAt, &rn.LastSeenAt); err != nil {
			rows.Close()
			httpError(w, 500, "%v", err)
			return
		}
		_ = json.Unmarshal([]byte(labels), &rn.Labels)
		rn.Online = nowMS()-rn.LastSeenAt < 60_000
		rn.Reserved = s.reserved.describe(rn.Labels)
		rn.ProtectedLabels = s.protectedLabels.of(rn.Labels)
		runners = append(runners, &rn)
	}
	rows.Close()
	for _, rn := range runners {
		_ = s.db.QueryRow(`SELECT id, run_id, name FROM jobs WHERE runner_id = ? AND status = ? LIMIT 1`, rn.ID, protocol.StatusInProgress).Scan(&rn.JobID, &rn.RunID, &rn.JobName)
		rn.Busy = rn.JobID != 0
	}
	writeJSON(w, runners)
}

func (s *Server) handleDeleteRunner(w http.ResponseWriter, r *http.Request) {
	if _, err := s.db.Exec(`DELETE FROM runners WHERE id = ?`, pathID(r)); err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleRegistrationToken(w http.ResponseWriter, r *http.Request) {
	token := randomToken(20)
	exp := time.Now().Add(time.Hour)
	if _, err := s.db.Exec(`INSERT INTO registration_tokens (token_hash, created_at, expires_at) VALUES (?, ?, ?)`, hashToken(token), nowMS(), exp.UnixMilli()); err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	writeJSON(w, map[string]any{"token": token, "expires_at": exp.UnixMilli(), "url": s.publicURL(r)})
}

// ---------------------------------------------------------------- secrets / variables

var settingNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (s *Server) settingRepoID(r *http.Request) (int64, error) {
	name := r.URL.Query().Get("repo")
	if name == "" {
		return 0, nil
	}
	repo, err := s.getRepo(name)
	if err != nil {
		return 0, fmt.Errorf("no such repository %s", name)
	}
	return repo.ID, nil
}

func (s *Server) handleListSettings(table string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoID, err := s.settingRepoID(r)
		if err != nil {
			httpError(w, 404, "%v", err)
			return
		}
		rows, err := s.db.Query(`SELECT name, value, updated_at FROM `+table+` WHERE repo_id = ? ORDER BY name`, repoID)
		if err != nil {
			httpError(w, 500, "%v", err)
			return
		}
		defer rows.Close()
		type item struct {
			Name      string `json:"name"`
			Value     string `json:"value,omitempty"`
			UpdatedAt int64  `json:"updated_at"`
		}
		out := []item{}
		for rows.Next() {
			var it item
			_ = rows.Scan(&it.Name, &it.Value, &it.UpdatedAt)
			if table == "secrets" {
				it.Value = "" // secrets are write-only
			}
			out = append(out, it)
		}
		writeJSON(w, out)
	}
}

func (s *Server) handlePutSetting(table string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoID, err := s.settingRepoID(r)
		if err != nil {
			httpError(w, 404, "%v", err)
			return
		}
		var req struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		if err := readJSON(r, &req); err != nil {
			httpError(w, 400, "bad request: %v", err)
			return
		}
		if !settingNameRE.MatchString(req.Name) {
			httpError(w, 400, "names may contain only letters, digits and underscores, and must not start with a digit")
			return
		}
		value := req.Value
		if table == "secrets" {
			value = s.box.seal(purposeSecret, value)
		}
		_, err = s.db.Exec(`INSERT INTO `+table+` (repo_id, name, value, updated_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (repo_id, name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, repoID, req.Name, value, nowMS())
		if err != nil {
			httpError(w, 500, "%v", err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	}
}

func (s *Server) handleDeleteSetting(table string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoID, err := s.settingRepoID(r)
		if err != nil {
			httpError(w, 404, "%v", err)
			return
		}
		if _, err := s.db.Exec(`DELETE FROM `+table+` WHERE repo_id = ? AND name = ?`, repoID, r.URL.Query().Get("name")); err != nil {
			httpError(w, 500, "%v", err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	}
}
