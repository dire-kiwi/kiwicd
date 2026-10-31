package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// kiwicd mcp serves the API to agents with the Model Context Protocol over
// stdio: one JSON-RPC message per line on stdin and stdout. It needs only
// the tools part of the protocol, so it's implemented here rather than with
// an SDK.

var mcpVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

const mcpInstructions = `kiwicd is a self-hosted CI server that runs GitHub Actions workflows. Repositories are named OWNER/NAME. A run is one workflow run (IDs are global, not per repository); it has jobs, each with steps and a log. Secrets are write-only: they can be listed by name and set, never read back. Secrets and variables without a repository are global (every repository gets them); a repository's override global ones of the same name.`

type mcpTool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`

	readOnly bool
	call     func(ctx context.Context, c *Client, args map[string]any) (any, error)
}

// schema builds an object schema from properties, with required ones named.
func schema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func prop(typ, desc string) map[string]any { return map[string]any{"type": typ, "description": desc} }

var (
	repoProp        = prop("string", "Repository, OWNER/NAME")
	optRepoProp     = prop("string", "Repository, OWNER/NAME; omit for the global ones")
	runIDProp       = prop("integer", "Run ID")
	settingNameProp = prop("string", "Name: letters, digits and underscores, not starting with a digit")
)

// argument getters: the schema says what's required, but clients don't
// always check.
func argString(args map[string]any, name string, required bool) (string, error) {
	v, ok := args[name]
	if !ok || v == nil {
		if required {
			return "", fmt.Errorf("%s is required", name)
		}
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", name)
	}
	if required && s == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return s, nil
}

func argInt(args map[string]any, name string, required bool, def int64) (int64, error) {
	v, ok := args[name]
	if !ok || v == nil {
		if required {
			return 0, fmt.Errorf("%s is required", name)
		}
		return def, nil
	}
	switch n := v.(type) {
	case float64:
		if n != float64(int64(n)) {
			return 0, fmt.Errorf("%s must be a whole number", name)
		}
		return int64(n), nil
	case string:
		var i int64
		if _, err := fmt.Sscan(n, &i); err == nil {
			return i, nil
		}
	}
	return 0, fmt.Errorf("%s must be a number", name)
}

func argBool(args map[string]any, name string) bool {
	b, _ := args[name].(bool)
	return b
}

// runSummary is a run with its jobs and steps, without the noise.
func runSummary(c *Client, d *RunDetail) map[string]any {
	r := d.Run
	jobs := []map[string]any{}
	for _, j := range d.Jobs {
		job := map[string]any{"id": j.ID, "key": j.Key, "name": j.Name, "state": state(j.Status, j.Conclusion), "runs_on": j.RunsOn}
		if j.RunnerName != "" {
			job["runner"] = j.RunnerName
		}
		if j.Error != "" {
			job["error"] = j.Error
		}
		if j.StartedAt != 0 {
			job["duration"] = elapsed(j.StartedAt, j.FinishedAt)
		}
		if len(j.Outputs) > 0 {
			job["outputs"] = j.Outputs
		}
		var steps []map[string]any
		for i, s := range j.Steps {
			if s.Name == "" {
				continue
			}
			steps = append(steps, map[string]any{"index": i, "name": s.Name, "state": state(s.Status, s.Conclusion)})
		}
		if steps != nil {
			job["steps"] = steps
		}
		jobs = append(jobs, job)
	}
	out := map[string]any{
		"id": r.ID, "repo": r.Repo, "workflow": r.WorkflowName, "workflow_file": r.WorkflowFile, "number": r.Number,
		"attempt": r.Attempt, "title": r.Title, "event": r.Event, "ref": r.Ref, "sha": r.Sha, "actor": r.Actor,
		"state": state(r.Status, r.Conclusion), "gets_secrets": r.Protected, "created": time.UnixMilli(r.CreatedAt).UTC().Format(time.RFC3339),
		"url": fmt.Sprintf("%s/runs/%d", strings.TrimRight(c.URL, "/"), r.ID), "jobs": jobs,
	}
	if r.StartedAt != 0 {
		out["duration"] = elapsed(r.StartedAt, r.FinishedAt)
	}
	if r.Error != "" {
		out["error"] = r.Error
	}
	if r.Cancel && r.Status != "completed" {
		out["cancelling"] = true
	}
	return out
}

const maxLogTail = 2000

func mcpTools() []*mcpTool {
	return []*mcpTool{
		{
			Name: "list_repos", Title: "List repositories", readOnly: true,
			Description: "List the repositories kiwicd hosts or mirrors, with their default branch and last sync.",
			InputSchema: schema(map[string]any{}),
			call: func(ctx context.Context, c *Client, args map[string]any) (any, error) {
				repos, err := c.Repos(ctx)
				if err != nil {
					return nil, err
				}
				out := []map[string]any{}
				for _, r := range repos {
					m := map[string]any{"name": r.Name, "kind": r.Kind, "default_branch": r.DefaultBranch}
					if r.LastSyncError != "" {
						m["sync_error"] = r.LastSyncError
					}
					out = append(out, m)
				}
				return out, nil
			},
		},
		{
			Name: "list_workflows", Title: "List workflows", readOnly: true,
			Description: "List a repository's workflows at a ref: file, name, events, and for workflow_dispatch ones their inputs. Files starting kiwicd: are stored in kiwicd rather than the repository.",
			InputSchema: schema(map[string]any{"repo": repoProp, "ref": prop("string", "Branch, tag or full ref; default the default branch")}, "repo"),
			call: func(ctx context.Context, c *Client, args map[string]any) (any, error) {
				repo, err := argString(args, "repo", true)
				if err != nil {
					return nil, err
				}
				ref, err := argString(args, "ref", false)
				if err != nil {
					return nil, err
				}
				return c.Workflows(ctx, repo, ref)
			},
		},
		{
			Name: "list_runs", Title: "List runs", readOnly: true,
			Description: "List recent runs, newest first, optionally for one repository.",
			InputSchema: schema(map[string]any{"repo": prop("string", "Only this repository's runs, OWNER/NAME"),
				"limit": prop("integer", "How many (default 20, at most 500)")}),
			call: func(ctx context.Context, c *Client, args map[string]any) (any, error) {
				repo, err := argString(args, "repo", false)
				if err != nil {
					return nil, err
				}
				limit, err := argInt(args, "limit", false, 20)
				if err != nil {
					return nil, err
				}
				runs, err := c.Runs(ctx, repo, int(limit))
				if err != nil {
					return nil, err
				}
				out := []map[string]any{}
				for _, r := range runs {
					m := map[string]any{"id": r.ID, "repo": r.Repo, "workflow": runName(r), "title": r.Title, "event": r.Event,
						"ref": r.Ref, "sha": shortSHA(r.Sha), "state": state(r.Status, r.Conclusion),
						"created": time.UnixMilli(r.CreatedAt).UTC().Format(time.RFC3339)}
					if r.StartedAt != 0 {
						m["duration"] = elapsed(r.StartedAt, r.FinishedAt)
					}
					out = append(out, m)
				}
				return out, nil
			},
		},
		{
			Name: "get_run", Title: "Get a run", readOnly: true,
			Description: "Get a run with its jobs (IDs, states, runners, errors, outputs) and their steps.",
			InputSchema: schema(map[string]any{"run_id": runIDProp}, "run_id"),
			call: func(ctx context.Context, c *Client, args map[string]any) (any, error) {
				id, err := argInt(args, "run_id", true, 0)
				if err != nil {
					return nil, err
				}
				d, err := c.Run(ctx, id)
				if err != nil {
					return nil, err
				}
				return runSummary(c, d), nil
			},
		},
		{
			Name: "get_job_logs", Title: "Get a job's log", readOnly: true,
			Description: "Get the end of a job's log as text, lines grouped under their step's name. Job IDs are in get_run. Use step to read one step's lines.",
			InputSchema: schema(map[string]any{
				"job_id": prop("integer", "Job ID"),
				"tail":   prop("integer", fmt.Sprintf("How many lines from the end (default 200, at most %d)", maxLogTail)),
				"step":   prop("integer", "Only this step's lines (its index in get_run)"),
			}, "job_id"),
			call: func(ctx context.Context, c *Client, args map[string]any) (any, error) {
				id, err := argInt(args, "job_id", true, 0)
				if err != nil {
					return nil, err
				}
				tail, err := argInt(args, "tail", false, 200)
				if err != nil {
					return nil, err
				}
				tail = max(1, min(tail, maxLogTail))
				step, err := argInt(args, "step", false, -1)
				if err != nil {
					return nil, err
				}
				return jobLogText(ctx, c, id, int(tail), int(step))
			},
		},
		{
			Name: "wait_for_run", Title: "Wait for a run",
			Description: "Wait until a run finishes (or the timeout passes) and return it as get_run does.", readOnly: true,
			InputSchema: schema(map[string]any{"run_id": runIDProp,
				"timeout_seconds": prop("integer", "How long to wait (default 120, at most 600)")}, "run_id"),
			call: func(ctx context.Context, c *Client, args map[string]any) (any, error) {
				id, err := argInt(args, "run_id", true, 0)
				if err != nil {
					return nil, err
				}
				secs, err := argInt(args, "timeout_seconds", false, 120)
				if err != nil {
					return nil, err
				}
				d, err := c.WaitRun(ctx, id, time.Duration(max(1, min(secs, 600)))*time.Second, nil)
				if errors.Is(err, errTimeout) {
					out := runSummary(c, d)
					out["note"] = "still running when the timeout passed"
					return out, nil
				}
				if err != nil {
					return nil, err
				}
				return runSummary(c, d), nil
			},
		},
		{
			Name: "list_runners", Title: "List runners", readOnly: true,
			Description: "List the agents that run jobs: labels, online, and the job each is running.",
			InputSchema: schema(map[string]any{}),
			call: func(ctx context.Context, c *Client, args map[string]any) (any, error) {
				return c.Runners(ctx)
			},
		},
		{
			Name: "list_secrets", Title: "List secrets", readOnly: true,
			Description: "List secrets' names (values can't be read), global or a repository's.",
			InputSchema: schema(map[string]any{"repo": optRepoProp}),
			call:        listSettingsTool("secrets"),
		},
		{
			Name: "list_variables", Title: "List variables", readOnly: true,
			Description: "List variables with their values, global or a repository's.",
			InputSchema: schema(map[string]any{"repo": optRepoProp}),
			call:        listSettingsTool("variables"),
		},
		{
			Name: "start_run", Title: "Start a run",
			Description: "Start a workflow_dispatch run of a workflow at a ref, and return its ID. The workflow must have a workflow_dispatch trigger; list_workflows shows which do and their inputs.",
			InputSchema: schema(map[string]any{
				"repo":     repoProp,
				"workflow": prop("string", "Workflow file, e.g. ci.yml, .github/workflows/ci.yml or kiwicd:deploy.yml"),
				"ref":      prop("string", "Branch, tag or full ref; default the default branch"),
				"inputs":   map[string]any{"type": "object", "description": "workflow_dispatch inputs by name", "additionalProperties": map[string]any{"type": "string"}},
			}, "repo", "workflow"),
			Annotations: map[string]any{"destructiveHint": false, "openWorldHint": true},
			call: func(ctx context.Context, c *Client, args map[string]any) (any, error) {
				repo, err := argString(args, "repo", true)
				if err != nil {
					return nil, err
				}
				workflow, err := argString(args, "workflow", true)
				if err != nil {
					return nil, err
				}
				ref, err := argString(args, "ref", false)
				if err != nil {
					return nil, err
				}
				inputs := map[string]string{}
				if raw, ok := args["inputs"].(map[string]any); ok {
					for k, v := range raw {
						switch v := v.(type) {
						case string:
							inputs[k] = v
						case nil:
						default:
							b, _ := json.Marshal(v)
							inputs[k] = string(b)
						}
					}
				}
				id, err := c.Dispatch(ctx, repo, workflow, ref, inputs)
				if err != nil {
					return nil, err
				}
				return map[string]any{"run_id": id, "url": fmt.Sprintf("%s/runs/%d", strings.TrimRight(c.URL, "/"), id)}, nil
			},
		},
		{
			Name: "dispatch_event", Title: "Send a repository_dispatch event",
			Description: "Send a repository_dispatch event to a repository, which starts the workflows on its default branch that listen for repository_dispatch with the event type (on.repository_dispatch.types, or any type without one). Returns the runs it started, which may be none.",
			InputSchema: schema(map[string]any{
				"repo":           repoProp,
				"event_type":     prop("string", "The event type, github.event.action in the runs"),
				"client_payload": map[string]any{"type": "object", "description": "Any JSON object, github.event.client_payload in the runs"},
			}, "repo", "event_type"),
			Annotations: map[string]any{"destructiveHint": false, "openWorldHint": true},
			call: func(ctx context.Context, c *Client, args map[string]any) (any, error) {
				repo, err := argString(args, "repo", true)
				if err != nil {
					return nil, err
				}
				eventType, err := argString(args, "event_type", true)
				if err != nil {
					return nil, err
				}
				payload, _ := args["client_payload"].(map[string]any)
				ids, err := c.RepositoryDispatch(ctx, repo, eventType, payload)
				if err != nil {
					return nil, err
				}
				runs := []map[string]any{}
				for _, id := range ids {
					runs = append(runs, map[string]any{"run_id": id, "url": fmt.Sprintf("%s/runs/%d", strings.TrimRight(c.URL, "/"), id)})
				}
				return map[string]any{"runs": runs}, nil
			},
		},
		{
			Name: "rerun", Title: "Re-run a run",
			Description: "Start a new attempt of a run, with the same workflow, commit and event. Returns the new run's ID.",
			InputSchema: schema(map[string]any{"run_id": runIDProp}, "run_id"),
			Annotations: map[string]any{"destructiveHint": false, "openWorldHint": true},
			call: func(ctx context.Context, c *Client, args map[string]any) (any, error) {
				id, err := argInt(args, "run_id", true, 0)
				if err != nil {
					return nil, err
				}
				newID, err := c.Rerun(ctx, id)
				if err != nil {
					return nil, err
				}
				return map[string]any{"run_id": newID, "url": fmt.Sprintf("%s/runs/%d", strings.TrimRight(c.URL, "/"), newID)}, nil
			},
		},
		{
			Name: "cancel_run", Title: "Cancel a run",
			Description: "Cancel a run that hasn't finished.",
			InputSchema: schema(map[string]any{"run_id": runIDProp}, "run_id"),
			Annotations: map[string]any{"destructiveHint": true, "idempotentHint": true},
			call: func(ctx context.Context, c *Client, args map[string]any) (any, error) {
				id, err := argInt(args, "run_id", true, 0)
				if err != nil {
					return nil, err
				}
				if err := c.Cancel(ctx, id); err != nil {
					return nil, err
				}
				return map[string]any{"ok": true, "note": "jobs stop shortly; wait_for_run shows when"}, nil
			},
		},
		{
			Name: "set_secret", Title: "Set a secret",
			Description: "Create or replace a secret, global or a repository's. Only runs from protected refs (the default branch and the repository's protected_refs) get secrets.",
			InputSchema: schema(map[string]any{"repo": optRepoProp, "name": settingNameProp, "value": prop("string", "The secret value")}, "name", "value"),
			Annotations: map[string]any{"destructiveHint": true, "idempotentHint": true},
			call:        setSettingTool("secrets"),
		},
		{
			Name: "delete_secret", Title: "Delete a secret",
			Description: "Delete a secret, global or a repository's.",
			InputSchema: schema(map[string]any{"repo": optRepoProp, "name": settingNameProp}, "name"),
			Annotations: map[string]any{"destructiveHint": true, "idempotentHint": true},
			call:        deleteSettingTool("secrets"),
		},
		{
			Name: "set_variable", Title: "Set a variable",
			Description: "Create or replace a variable (vars.NAME in workflows), global or a repository's.",
			InputSchema: schema(map[string]any{"repo": optRepoProp, "name": settingNameProp, "value": prop("string", "The value")}, "name", "value"),
			Annotations: map[string]any{"destructiveHint": true, "idempotentHint": true},
			call:        setSettingTool("variables"),
		},
		{
			Name: "delete_variable", Title: "Delete a variable",
			Description: "Delete a variable, global or a repository's.",
			InputSchema: schema(map[string]any{"repo": optRepoProp, "name": settingNameProp}, "name"),
			Annotations: map[string]any{"destructiveHint": true, "idempotentHint": true},
			call:        deleteSettingTool("variables"),
		},
	}
}

func listSettingsTool(kind string) func(context.Context, *Client, map[string]any) (any, error) {
	return func(ctx context.Context, c *Client, args map[string]any) (any, error) {
		repo, err := argString(args, "repo", false)
		if err != nil {
			return nil, err
		}
		list, err := c.Settings(ctx, kind, repo)
		if err != nil {
			return nil, err
		}
		out := []map[string]any{}
		for _, s := range list {
			m := map[string]any{"name": s.Name, "updated": time.UnixMilli(s.UpdatedAt).UTC().Format(time.RFC3339)}
			if kind == "variables" {
				m["value"] = s.Value
			}
			out = append(out, m)
		}
		return out, nil
	}
}

func setSettingTool(kind string) func(context.Context, *Client, map[string]any) (any, error) {
	return func(ctx context.Context, c *Client, args map[string]any) (any, error) {
		repo, err := argString(args, "repo", false)
		if err != nil {
			return nil, err
		}
		name, err := argString(args, "name", true)
		if err != nil {
			return nil, err
		}
		value, err := argString(args, "value", false)
		if err != nil {
			return nil, err
		}
		if _, ok := args["value"]; !ok {
			return nil, errors.New("value is required")
		}
		if err := c.SetSetting(ctx, kind, repo, name, value); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "scope": scopeName(repo), "name": name}, nil
	}
}

func deleteSettingTool(kind string) func(context.Context, *Client, map[string]any) (any, error) {
	return func(ctx context.Context, c *Client, args map[string]any) (any, error) {
		repo, err := argString(args, "repo", false)
		if err != nil {
			return nil, err
		}
		name, err := argString(args, "name", true)
		if err != nil {
			return nil, err
		}
		if err := c.DeleteSetting(ctx, kind, repo, name); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "scope": scopeName(repo), "name": name}, nil
	}
}

// jobLogText is the last tail lines of a job's log (of one step, if step >=
// 0), without terminal escapes, under headers naming their steps.
func jobLogText(ctx context.Context, c *Client, jobID int64, tail, step int) (string, error) {
	j, err := c.Job(ctx, jobID)
	if err != nil {
		return "", err
	}
	// Reading a step's lines means reading the whole log; otherwise start
	// close to the end.
	after := int64(0)
	if step < 0 {
		after = max(0, j.LogSeq-int64(tail))
	}
	var lines []LogLine
	if _, err := c.Logs(ctx, jobID, after, func(l LogLine) {
		if step >= 0 && l.Step != step {
			return
		}
		lines = append(lines, l)
		if len(lines) > tail {
			lines = lines[1:]
		}
	}); err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Job %d %s: %s", j.ID, j.Name, state(j.Status, j.Conclusion))
	if j.Error != "" {
		fmt.Fprintf(&b, " (%s)", j.Error)
	}
	b.WriteByte('\n')
	if len(lines) == 0 {
		b.WriteString("(no log lines)\n")
		return b.String(), nil
	}
	if lines[0].Seq > 1 {
		fmt.Fprintf(&b, "(earlier lines left out; showing the last %d)\n", len(lines))
	}
	cur := -2
	for _, l := range lines {
		if l.Step != cur {
			cur = l.Step
			name := "other output"
			if cur >= 0 && cur < len(j.Steps) && j.Steps[cur].Name != "" {
				name = j.Steps[cur].Name
			}
			fmt.Fprintf(&b, "--- step %d: %s\n", cur, name)
		}
		b.WriteString(stripANSI(l.Text))
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// ---------------------------------------------------------------- protocol

// rpcMessage is a request or notification from the client, or our
// response to one: a response has a result or an error, never both.
type rpcMessage struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
	Result any
	Error  *rpcError
}

func (m *rpcMessage) UnmarshalJSON(b []byte) error {
	var v struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	m.ID, m.Method, m.Params = v.ID, v.Method, v.Params
	return nil
}

func (m rpcMessage) MarshalJSON() ([]byte, error) {
	out := map[string]any{"jsonrpc": "2.0", "id": m.ID}
	if m.Error != nil {
		out["error"] = m.Error
	} else {
		out["result"] = m.Result
	}
	return json.Marshal(out)
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpServer struct {
	client  *Client
	version string
	tools   []*mcpTool
	byName  map[string]*mcpTool

	outMu sync.Mutex
	out   io.Writer
	log   io.Writer
}

func newMCPServer(c *Client, version string, readOnly bool, out, log io.Writer) *mcpServer {
	s := &mcpServer{client: c, version: version, byName: map[string]*mcpTool{}, out: out, log: log}
	for _, t := range mcpTools() {
		if readOnly && !t.readOnly {
			continue
		}
		if t.readOnly {
			if t.Annotations == nil {
				t.Annotations = map[string]any{}
			}
			t.Annotations["readOnlyHint"] = true
		}
		s.tools = append(s.tools, t)
		s.byName[t.Name] = t
	}
	return s
}

func (s *mcpServer) send(m rpcMessage) {
	b, err := json.Marshal(m)
	if err != nil {
		b, _ = json.Marshal(rpcMessage{ID: m.ID, Error: &rpcError{Code: -32603, Message: err.Error()}})
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	_, _ = s.out.Write(append(b, '\n'))
}

// serve handles messages from in until it ends or ctx is done. Tool calls
// run concurrently, so a wait_for_run doesn't hold up the others.
func (s *mcpServer) serve(ctx context.Context, in io.Reader) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	defer wg.Wait()
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 32<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var m rpcMessage
		if err := json.Unmarshal(line, &m); err != nil {
			s.send(rpcMessage{ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error: " + err.Error()}})
			continue
		}
		if m.Method == "" {
			continue // a response to something we never send
		}
		if len(m.ID) == 0 || string(m.ID) == "null" {
			continue // notifications: initialized, cancelled
		}
		if m.Method == "tools/call" {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.handle(ctx, m)
			}()
			continue
		}
		s.handle(ctx, m)
	}
	return sc.Err()
}

func (s *mcpServer) handle(ctx context.Context, m rpcMessage) {
	reply := rpcMessage{ID: m.ID}
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(m.Params, &p)
		version := mcpVersions[0]
		for _, v := range mcpVersions {
			if v == p.ProtocolVersion {
				version = v
			}
		}
		reply.Result = map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "kiwicd", "title": "kiwicd", "version": s.version},
			"instructions":    mcpInstructions,
		}
	case "ping":
		reply.Result = map[string]any{}
	case "tools/list":
		reply.Result = map[string]any{"tools": s.tools}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			reply.Error = &rpcError{Code: -32602, Message: "invalid params: " + err.Error()}
			break
		}
		t := s.byName[p.Name]
		if t == nil {
			reply.Error = &rpcError{Code: -32602, Message: "unknown tool " + p.Name}
			break
		}
		if p.Arguments == nil {
			p.Arguments = map[string]any{}
		}
		reply.Result = s.callTool(ctx, t, p.Arguments)
	default:
		reply.Error = &rpcError{Code: -32601, Message: "method not found: " + m.Method}
	}
	s.send(reply)
}

// callTool runs a tool; its errors are results the model sees, not
// protocol errors.
func (s *mcpServer) callTool(ctx context.Context, t *mcpTool, args map[string]any) map[string]any {
	v, err := t.call(ctx, s.client, args)
	if err != nil {
		fmt.Fprintf(s.log, "kiwicd mcp: %s: %v\n", t.Name, err)
		return map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": err.Error()}}}
	}
	text, ok := v.(string)
	if !ok {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": err.Error()}}}
		}
		text = string(b)
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
}

func mcpCommand(ctx context.Context, a *App, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	readOnly := fs.Bool("read-only", false, "only the tools that don't change anything")
	if _, err := flags(fs, args, 0, 0); err != nil {
		return err
	}
	c, err := a.api()
	if err != nil {
		return err
	}
	return newMCPServer(c, a.Version, *readOnly, a.Stdout, a.Stderr).serve(ctx, a.Stdin)
}
