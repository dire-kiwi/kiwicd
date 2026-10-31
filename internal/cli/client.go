// Package cli is the kiwicd command: a client for the server's API, for
// people at a terminal and, as an MCP server, for agents.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client calls a kiwicd server's API with an API token (or the admin
// password, which the server takes the same way).
type Client struct {
	URL   string
	Token string
	HTTP  *http.Client
}

// APIError is an error response from the server.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Status == http.StatusUnauthorized {
		return e.Message + " (run kiwicd login)"
	}
	return e.Message
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: time.Minute}
}

func (c *Client) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	if c.URL == "" {
		return nil, errors.New("no server configured: run kiwicd login --url URL, or set KIWICD_URL")
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(b))
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		if msg == "" {
			msg = resp.Status
		}
		return nil, &APIError{Status: resp.StatusCode, Message: msg}
	}
	return resp, nil
}

// Do sends body (if not nil) as JSON and decodes the response into out (if
// not nil).
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if raw, ok := out.(*json.RawMessage); ok {
		b, err := io.ReadAll(resp.Body)
		*raw = b
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.Do(ctx, http.MethodGet, path, nil, out)
}

// repoPath is /api/repos/OWNER/NAME with each part escaped.
func repoPath(name string) (string, error) {
	owner, repo, ok := strings.Cut(name, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", fmt.Errorf("%q isn't a repository: use OWNER/NAME", name)
	}
	return "/api/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo), nil
}

// ---------------------------------------------------------------- API types

type Me struct {
	User      string `json:"user"`
	Version   string `json:"version"`
	PublicURL string `json:"public_url"`
	Access    Access `json:"access"`
}

// Access is what the caller may do: see the server's access.go.
type Access struct {
	Role        string   `json:"role"` // admin, maintainer or viewer
	Admin       bool     `json:"admin"`
	MaintainAll bool     `json:"maintain_all"`
	Maintain    []string `json:"maintain"` // OWNER/NAME or OWNER/*
	Repos       []string `json:"repos"`    // the only repositories it sees; nil for all
	Token       *Token   `json:"token,omitempty"`
}

// CanWrite reports whether the caller may change anything at all.
func (a Access) CanWrite() bool { return a.Role == "admin" || a.Role == "maintainer" }

func (a Access) String() string {
	s := "a viewer"
	switch {
	case a.Admin:
		s = "an admin"
	case a.Role == "maintainer" && a.MaintainAll:
		s = "a maintainer of every repository"
	case a.Role == "maintainer":
		s = "a maintainer of " + strings.Join(a.Maintain, ", ")
	}
	if a.Repos != nil {
		s += ", seeing only " + strings.Join(a.Repos, ", ")
	}
	return s
}

type Repo struct {
	ID            int64             `json:"id"`
	Name          string            `json:"name"`
	Kind          string            `json:"kind"`
	URL           string            `json:"url,omitempty"`
	DefaultBranch string            `json:"default_branch"`
	Refs          map[string]string `json:"refs"`
	LastSyncAt    int64             `json:"last_sync_at"`
	LastSyncError string            `json:"last_sync_error"`
	CloneURL      string            `json:"clone_url,omitempty"`
	ProtectedRefs []string          `json:"protected_refs"`
	Readers       []string          `json:"readers"`
}

type Workflow struct {
	File     string   `json:"file"`
	Name     string   `json:"name"`
	Events   []string `json:"events"`
	Dispatch bool     `json:"dispatch"`
	Inputs   []struct {
		Name        string   `json:"name"`
		Description string   `json:"description,omitempty"`
		Required    bool     `json:"required,omitempty"`
		Default     string   `json:"default,omitempty"`
		Type        string   `json:"type,omitempty"`
		Options     []string `json:"options,omitempty"`
	} `json:"inputs"`
	Error    string `json:"error,omitempty"`
	Uploaded bool   `json:"uploaded,omitempty"`
}

type Run struct {
	ID           int64  `json:"id"`
	Repo         string `json:"repo"`
	Number       int64  `json:"number"`
	Attempt      int64  `json:"attempt"`
	WorkflowFile string `json:"workflow_file"`
	WorkflowName string `json:"workflow_name"`
	Title        string `json:"title"`
	Event        string `json:"event"`
	Ref          string `json:"ref"`
	Sha          string `json:"sha"`
	Actor        string `json:"actor"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	Error        string `json:"error"`
	Cancel       bool   `json:"cancel_requested"`
	Protected    bool   `json:"protected"`
	CreatedAt    int64  `json:"created_at"`
	StartedAt    int64  `json:"started_at"`
	FinishedAt   int64  `json:"finished_at"`
}

type Step struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
	StartedAt  int64  `json:"started_at,omitempty"`
	FinishedAt int64  `json:"finished_at,omitempty"`
}

type Job struct {
	ID         int64             `json:"id"`
	RunID      int64             `json:"run_id"`
	Key        string            `json:"key"`
	Name       string            `json:"name"`
	Uses       string            `json:"uses,omitempty"` // a job calling a reusable workflow
	RunsOn     []string          `json:"runs_on"`
	Status     string            `json:"status"`
	Conclusion string            `json:"conclusion"`
	Error      string            `json:"error"`
	RunnerName string            `json:"runner_name"`
	Outputs    map[string]string `json:"outputs"`
	Steps      []Step            `json:"steps"`
	LogSeq     int64             `json:"log_seq"`
	CreatedAt  int64             `json:"created_at"`
	StartedAt  int64             `json:"started_at"`
	FinishedAt int64             `json:"finished_at"`
}

type RunDetail struct {
	Run  Run   `json:"run"`
	Jobs []Job `json:"jobs"`
	Plan []struct {
		Key   string   `json:"key"`
		Name  string   `json:"name"`
		Needs []string `json:"needs"`
	} `json:"plan"`
	ProtectedLabels []string `json:"protected_labels"`
}

type LogLine struct {
	Seq  int64  `json:"seq"`
	Step int    `json:"step"`
	TS   int64  `json:"ts"`
	Text string `json:"text"`
}

type Setting struct {
	Name      string `json:"name"`
	Value     string `json:"value,omitempty"`
	UpdatedAt int64  `json:"updated_at"`
}

type Runner struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	Labels     []string `json:"labels"`
	OS         string   `json:"os"`
	Arch       string   `json:"arch"`
	Version    string   `json:"version"`
	LastSeenAt int64    `json:"last_seen_at"`
	Online     bool     `json:"online"`
	Busy       bool     `json:"busy"`
	JobID      int64    `json:"job_id,omitempty"`
	RunID      int64    `json:"run_id,omitempty"`
	JobName    string   `json:"job_name,omitempty"`
}

type Token struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	User       string   `json:"user"`
	CreatedAt  int64    `json:"created_at"`
	ExpiresAt  int64    `json:"expires_at"`
	LastUsedAt int64    `json:"last_used_at"`
	Role       string   `json:"role"`  // "" for its maker's
	Repos      []string `json:"repos"` // none for all
}

// Scope says what a token may do.
func (t Token) Scope() string {
	s := "full"
	if t.Role != "" {
		s = t.Role
	}
	if len(t.Repos) > 0 {
		s += " of " + strings.Join(t.Repos, ",")
	}
	return s
}

// NewToken is what CreateToken makes: ExpiresInDays 0 means never (or the
// server's limit, for GitHub users); Role ("viewer", "maintainer" or
// "admin"; "" for its maker's) and Repos limit what it may do.
type NewToken struct {
	Name          string   `json:"name"`
	ExpiresInDays int      `json:"expires_in_days"`
	Role          string   `json:"role,omitempty"`
	Repos         []string `json:"repos,omitempty"`
}

// ---------------------------------------------------------------- calls

func (c *Client) Runs(ctx context.Context, repo string, limit int) ([]Run, error) {
	q := url.Values{}
	if repo != "" {
		q.Set("repo", repo)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var runs []Run
	err := c.Get(ctx, "/api/runs?"+q.Encode(), &runs)
	return runs, err
}

func (c *Client) Run(ctx context.Context, id int64) (*RunDetail, error) {
	var d RunDetail
	if err := c.Get(ctx, fmt.Sprintf("/api/runs/%d", id), &d); err != nil {
		return nil, err
	}
	return &d, nil
}

func (c *Client) Job(ctx context.Context, id int64) (*Job, error) {
	var j Job
	if err := c.Get(ctx, fmt.Sprintf("/api/jobs/%d", id), &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// Logs calls fn for each of a job's log lines after seq after, a page at a
// time, and returns the last seq seen.
func (c *Client) Logs(ctx context.Context, jobID, after int64, fn func(LogLine)) (int64, error) {
	for {
		resp, err := c.request(ctx, http.MethodGet, fmt.Sprintf("/api/jobs/%d/logs?after=%d", jobID, after), nil)
		if err != nil {
			return after, err
		}
		var lines []LogLine
		err = json.NewDecoder(resp.Body).Decode(&lines)
		more := resp.Header.Get("Link") != ""
		resp.Body.Close()
		if err != nil {
			return after, err
		}
		for _, l := range lines {
			fn(l)
			after = l.Seq
		}
		if !more || len(lines) == 0 {
			return after, nil
		}
	}
}

// Dispatch starts a workflow_dispatch run and returns its ID.
func (c *Client) Dispatch(ctx context.Context, repo, workflow, ref string, inputs map[string]string) (int64, error) {
	p, err := repoPath(repo)
	if err != nil {
		return 0, err
	}
	var out struct {
		RunID int64 `json:"run_id"`
	}
	err = c.Do(ctx, http.MethodPost, p+"/dispatch", map[string]any{"workflow": workflow, "ref": ref, "inputs": inputs}, &out)
	return out.RunID, err
}

// RepositoryDispatch sends a repository_dispatch event of eventType and
// returns the runs it started.
func (c *Client) RepositoryDispatch(ctx context.Context, repo, eventType string, payload map[string]any) ([]int64, error) {
	p, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	var out struct {
		RunIDs []int64 `json:"run_ids"`
	}
	err = c.Do(ctx, http.MethodPost, p+"/dispatches", map[string]any{"event_type": eventType, "client_payload": payload}, &out)
	return out.RunIDs, err
}

func (c *Client) Rerun(ctx context.Context, id int64) (int64, error) {
	var out struct {
		RunID int64 `json:"run_id"`
	}
	err := c.Do(ctx, http.MethodPost, fmt.Sprintf("/api/runs/%d/rerun", id), struct{}{}, &out)
	return out.RunID, err
}

func (c *Client) Cancel(ctx context.Context, id int64) error {
	return c.Do(ctx, http.MethodPost, fmt.Sprintf("/api/runs/%d/cancel", id), struct{}{}, nil)
}

func (c *Client) Repos(ctx context.Context) ([]Repo, error) {
	var repos []Repo
	err := c.Get(ctx, "/api/repos", &repos)
	return repos, err
}

func (c *Client) Repo(ctx context.Context, name string) (*Repo, error) {
	p, err := repoPath(name)
	if err != nil {
		return nil, err
	}
	var r Repo
	if err := c.Get(ctx, p, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *Client) SyncRepo(ctx context.Context, name string) error {
	p, err := repoPath(name)
	if err != nil {
		return err
	}
	return c.Do(ctx, http.MethodPost, p+"/sync", struct{}{}, nil)
}

func (c *Client) Workflows(ctx context.Context, repo, ref string) ([]Workflow, error) {
	p, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	p += "/workflows"
	if ref != "" {
		p += "?ref=" + url.QueryEscape(ref)
	}
	var out []Workflow
	err = c.Get(ctx, p, &out)
	return out, err
}

// settingsPath is /api/secrets or /api/variables for a repository's (or,
// with repo "", the global ones), and for one named setting if name isn't "".
func settingsPath(kind, repo, name string) string {
	q := url.Values{}
	if repo != "" {
		q.Set("repo", repo)
	}
	if name != "" {
		q.Set("name", name)
	}
	p := "/api/" + kind
	if len(q) > 0 {
		p += "?" + q.Encode()
	}
	return p
}

// Settings lists secrets (kind "secrets", without values) or variables.
func (c *Client) Settings(ctx context.Context, kind, repo string) ([]Setting, error) {
	if repo != "" {
		if _, err := repoPath(repo); err != nil {
			return nil, err
		}
	}
	var out []Setting
	err := c.Get(ctx, settingsPath(kind, repo, ""), &out)
	return out, err
}

func (c *Client) SetSetting(ctx context.Context, kind, repo, name, value string) error {
	if repo != "" {
		if _, err := repoPath(repo); err != nil {
			return err
		}
	}
	return c.Do(ctx, http.MethodPut, settingsPath(kind, repo, ""), map[string]string{"name": name, "value": value}, nil)
}

// DeleteSetting deletes a secret or variable, and fails if there's no such
// one (which the server doesn't say).
func (c *Client) DeleteSetting(ctx context.Context, kind, repo, name string) error {
	list, err := c.Settings(ctx, kind, repo)
	if err != nil {
		return err
	}
	found := false
	for _, s := range list {
		found = found || s.Name == name
	}
	if !found {
		where := "global"
		if repo != "" {
			where = repo + "'s"
		}
		return fmt.Errorf("there's no %s %s named %s", where, strings.TrimSuffix(kind, "s"), name)
	}
	return c.Do(ctx, http.MethodDelete, settingsPath(kind, repo, name), nil, nil)
}

func (c *Client) Runners(ctx context.Context) ([]Runner, error) {
	var out []Runner
	err := c.Get(ctx, "/api/runners", &out)
	return out, err
}

func (c *Client) Tokens(ctx context.Context) ([]Token, error) {
	var out []Token
	err := c.Get(ctx, "/api/tokens", &out)
	return out, err
}

// CreateToken makes an API token.
func (c *Client) CreateToken(ctx context.Context, t NewToken) (string, *Token, error) {
	var out struct {
		Token string `json:"token"`
		Info  Token  `json:"info"`
	}
	err := c.Do(ctx, http.MethodPost, "/api/tokens", t, &out)
	return out.Token, &out.Info, err
}

// Me says who the client is and what it may do.
func (c *Client) Me(ctx context.Context) (*Me, error) {
	var me Me
	err := c.Get(ctx, "/api/me", &me)
	return &me, err
}

// RevokeToken revokes a token by ID, or the client's own with id "current".
func (c *Client) RevokeToken(ctx context.Context, id string) error {
	return c.Do(ctx, http.MethodDelete, "/api/tokens/"+url.PathEscape(id), nil, nil)
}

// WaitRun polls a run until it completes, the timeout passes (0: no
// timeout; errTimeout with the run as it was) or ctx is done, calling
// progress (if not nil) on every poll.
func (c *Client) WaitRun(ctx context.Context, id int64, timeout time.Duration, progress func(*RunDetail)) (*RunDetail, error) {
	var deadline <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadline = t.C
	}
	for {
		d, err := c.Run(ctx, id)
		if err != nil {
			return nil, err
		}
		if progress != nil {
			progress(d)
		}
		if d.Run.Status == "completed" {
			return d, nil
		}
		select {
		case <-ctx.Done():
			return d, ctx.Err()
		case <-deadline:
			return d, errTimeout
		case <-time.After(pollInterval):
		}
	}
}

// pollInterval is how often runs and logs being watched are fetched.
var pollInterval = 2 * time.Second

var errTimeout = errors.New("timed out waiting for the run to finish")
