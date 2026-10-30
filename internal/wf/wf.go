// Package wf holds workflow helpers shared by the server and the agent, built
// on act's model, exprparser and workflowpattern packages.
package wf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/nektos/act/pkg/exprparser"
	"github.com/nektos/act/pkg/model"
	"github.com/nektos/act/pkg/workflowpattern"
	"gopkg.in/yaml.v3"
)

// Parse reads a workflow file and validates the job graph.
func Parse(src []byte) (*model.Workflow, error) {
	w, err := model.ReadWorkflow(bytes.NewReader(src), false)
	if err != nil {
		return nil, err
	}
	if len(w.Jobs) == 0 {
		return nil, fmt.Errorf("workflow has no jobs")
	}
	for id, job := range w.Jobs {
		if job == nil {
			return nil, fmt.Errorf("job %q is empty", id)
		}
		for _, need := range job.Needs() {
			if w.GetJob(need) == nil {
				return nil, fmt.Errorf("job %q needs unknown job %q", id, need)
			}
		}
	}
	if _, err := Order(w); err != nil {
		return nil, err
	}
	return w, nil
}

// Order returns job IDs in dependency order (stable by name within a level).
func Order(w *model.Workflow) ([]string, error) {
	done := map[string]bool{}
	var out []string
	for len(out) < len(w.Jobs) {
		var level []string
		for id, job := range w.Jobs {
			if done[id] {
				continue
			}
			ready := true
			for _, n := range job.Needs() {
				if !done[n] {
					ready = false
				}
			}
			if ready {
				level = append(level, id)
			}
		}
		if len(level) == 0 {
			return nil, fmt.Errorf("job dependencies contain a cycle")
		}
		sort.Strings(level)
		for _, id := range level {
			done[id] = true
		}
		out = append(out, level...)
	}
	return out, nil
}

// PushFilter is the `on.push` configuration.
type PushFilter struct {
	Branches, BranchesIgnore, Tags, TagsIgnore, Paths, PathsIgnore []string
	set                                                            map[string]bool
}

func stringList(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, i := range t {
			out = append(out, fmt.Sprint(i))
		}
		return out
	}
	return nil
}

func eventConfig(w *model.Workflow, event string) (map[string]any, bool) {
	for _, e := range w.On() {
		if e == event {
			cfg, _ := w.OnEvent(event).(map[string]any)
			return cfg, true
		}
	}
	return nil, false
}

// MatchesPush reports whether a push of ref (refs/heads/x or refs/tags/x)
// touching changedFiles (nil when unknown) triggers the workflow.
func MatchesPush(w *model.Workflow, ref string, changedFiles []string) bool {
	cfg, ok := eventConfig(w, "push")
	return ok && matchesPushFilters(cfg, ref, changedFiles)
}

// RepositoryPush is kiwicd's event for pushes to other repositories:
//
//	on:
//	  repository_push:
//	    repositories: [owner/app]
//	    branches: [main]
//
// It takes push's branches, tags and paths filters (and their -ignore forms).
const RepositoryPush = "repository_push"

// RepositoryPushSources returns the repositories whose pushes the workflow
// listens to with repository_push.
func RepositoryPushSources(w *model.Workflow) []string {
	cfg, _ := eventConfig(w, RepositoryPush)
	return stringList(cfg["repositories"])
}

// MatchesRepositoryPush reports whether a push of ref to repo, touching
// changedFiles (nil when unknown), triggers the workflow's repository_push.
func MatchesRepositoryPush(w *model.Workflow, repo, ref string, changedFiles []string) bool {
	cfg, ok := eventConfig(w, RepositoryPush)
	if !ok {
		return false
	}
	for _, r := range stringList(cfg["repositories"]) {
		if strings.EqualFold(r, repo) {
			return matchesPushFilters(cfg, ref, changedFiles)
		}
	}
	return false
}

// RepositoryPullRequest is kiwicd's event for pull requests on other
// repositories, from GitHub's pull_request webhook to a mirror:
//
//	on:
//	  repository_pull_request:
//	    repositories: [owner/app]
//	    types: [opened, synchronize, reopened, closed]
//	    branches: [main]
//
// types defaults to opened, synchronize and reopened, as for GitHub's
// pull_request; branches and branches-ignore filter the base branch. Pull
// requests from forks only trigger it with forks: true.
const RepositoryPullRequest = "repository_pull_request"

// PullRequest is what repository_pull_request filters look at.
type PullRequest struct {
	Repo   string // the kiwicd repository the pull request is on
	Action string // opened, synchronize, closed, ...
	Base   string // the base branch
	Fork   bool   // the head branch is in another repository
}

var defaultPullRequestTypes = []string{"opened", "synchronize", "reopened"}

// RepositoryPullRequestSources returns the repositories whose pull requests
// the workflow listens to with repository_pull_request.
func RepositoryPullRequestSources(w *model.Workflow) []string {
	cfg, _ := eventConfig(w, RepositoryPullRequest)
	return stringList(cfg["repositories"])
}

// MatchesRepositoryPullRequest reports whether a pull request event triggers
// the workflow's repository_pull_request.
func MatchesRepositoryPullRequest(w *model.Workflow, pr PullRequest) bool {
	cfg, ok := eventConfig(w, RepositoryPullRequest)
	if !ok {
		return false
	}
	listens := false
	for _, r := range stringList(cfg["repositories"]) {
		listens = listens || strings.EqualFold(r, pr.Repo)
	}
	if !listens {
		return false
	}
	if pr.Fork && cfg["forks"] != true {
		return false
	}
	types := defaultPullRequestTypes
	if _, ok := cfg["types"]; ok {
		types = stringList(cfg["types"])
	}
	matched := false
	for _, t := range types {
		matched = matched || t == pr.Action
	}
	if !matched {
		return false
	}
	// Only branch filters apply, to the base branch.
	branches := map[string]any{}
	for _, k := range []string{"branches", "branches-ignore"} {
		if v, ok := cfg[k]; ok {
			branches[k] = v
		}
	}
	return matchesPushFilters(branches, "refs/heads/"+pr.Base, nil)
}

// ListenedRepositories returns the repositories the workflow listens to with
// repository_push or repository_pull_request.
func ListenedRepositories(w *model.Workflow) []string {
	var out []string
	for _, name := range append(RepositoryPushSources(w), RepositoryPullRequestSources(w)...) {
		if !slices.ContainsFunc(out, func(s string) bool { return strings.EqualFold(s, name) }) {
			out = append(out, name)
		}
	}
	return out
}

// Concurrency is a workflow's concurrency setting: at most one run of a
// repository's group runs at a time.
type Concurrency struct {
	Group            string
	CancelInProgress bool
}

// WorkflowConcurrency evaluates the workflow's top-level concurrency (src is
// the workflow file, since act's model doesn't keep it). Expressions can use
// github, inputs and vars. It returns a zero Concurrency if there is none.
func WorkflowConcurrency(src []byte, e *Env) (Concurrency, error) {
	var doc struct {
		Concurrency yaml.Node `yaml:"concurrency"`
	}
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return Concurrency{}, err
	}
	node := doc.Concurrency
	var out Concurrency
	switch node.Kind {
	case 0:
		return out, nil
	case yaml.ScalarNode:
		v, err := e.Interpolate(node.Value)
		if err != nil {
			return out, fmt.Errorf("concurrency: %w", err)
		}
		out.Group = ToString(v)
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, val := node.Content[i].Value, node.Content[i+1]
			v, err := e.Interpolate(val.Value)
			if err != nil {
				return out, fmt.Errorf("concurrency.%s: %w", key, err)
			}
			switch key {
			case "group":
				out.Group = ToString(v)
			case "cancel-in-progress":
				out.CancelInProgress = exprparser.IsTruthy(v) && ToString(v) != "false"
			}
		}
		if out.Group == "" {
			return Concurrency{}, fmt.Errorf("concurrency: the group is empty")
		}
	default:
		return out, fmt.Errorf("concurrency must be a group name or a mapping")
	}
	return out, nil
}

func matchesPushFilters(cfg map[string]any, ref string, changedFiles []string) bool {
	has := func(k string) bool { _, ok := cfg[k]; return ok }
	compile := func(k string) []*workflowpattern.WorkflowPattern {
		p, err := workflowpattern.CompilePatterns(stringList(cfg[k])...)
		if err != nil {
			return nil
		}
		return p
	}
	tw := &workflowpattern.EmptyTraceWriter{}
	if branch, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		switch {
		case has("branches"):
			if workflowpattern.Skip(compile("branches"), []string{branch}, tw) {
				return false
			}
		case has("branches-ignore"):
			if workflowpattern.Filter(compile("branches-ignore"), []string{branch}, tw) {
				return false
			}
		case has("tags") || has("tags-ignore"):
			return false
		}
	} else if tag, ok := strings.CutPrefix(ref, "refs/tags/"); ok {
		switch {
		case has("tags"):
			if workflowpattern.Skip(compile("tags"), []string{tag}, tw) {
				return false
			}
		case has("tags-ignore"):
			if workflowpattern.Filter(compile("tags-ignore"), []string{tag}, tw) {
				return false
			}
		case has("branches") || has("branches-ignore"):
			return false
		}
	} else {
		return false
	}
	if changedFiles != nil {
		if has("paths") && workflowpattern.Skip(compile("paths"), changedFiles, tw) {
			return false
		}
		if has("paths-ignore") && workflowpattern.Filter(compile("paths-ignore"), changedFiles, tw) {
			return false
		}
	}
	return true
}

// HasEvent reports whether the workflow listens for event.
func HasEvent(w *model.Workflow, event string) bool {
	_, ok := eventConfig(w, event)
	return ok
}

// DispatchInput describes a workflow_dispatch input for the UI.
type DispatchInput struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Required    bool     `json:"required"`
	Default     string   `json:"default"`
	Type        string   `json:"type"`
	Options     []string `json:"options,omitempty"`
}

func DispatchInputs(w *model.Workflow) []DispatchInput {
	cfg := w.WorkflowDispatchConfig()
	if cfg == nil {
		return nil
	}
	var out []DispatchInput
	for name, in := range cfg.Inputs {
		out = append(out, DispatchInput{Name: name, Description: in.Description, Required: in.Required, Default: in.Default, Type: in.Type, Options: in.Options})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ---------------------------------------------------------------- expressions

// Env is what job-level expressions can see.
type Env struct {
	Workflow *model.Workflow
	JobID    string
	Github   *model.GithubContext
	Vars     map[string]string
	Inputs   map[string]any
	Matrix   map[string]any
	// Results of finished jobs, by job ID.
	Results map[string]JobResult
}

type JobResult struct {
	Result  string
	Outputs map[string]string
}

func (e *Env) interpreter() exprparser.Interpreter {
	// The status functions read results from the workflow's jobs.
	for id, r := range e.Results {
		if j := e.Workflow.GetJob(id); j != nil {
			j.Result = r.Result
			j.Outputs = r.Outputs
		}
	}
	needs := map[string]exprparser.Needs{}
	if job := e.Workflow.GetJob(e.JobID); job != nil {
		for _, n := range job.Needs() {
			r := e.Results[n]
			needs[n] = exprparser.Needs{Result: r.Result, Outputs: r.Outputs}
		}
	}
	vars := e.Vars
	if vars == nil {
		vars = map[string]string{}
	}
	return exprparser.NewInterpeter(&exprparser.EvaluationEnvironment{
		Github:   e.Github,
		Env:      map[string]string{},
		Vars:     vars,
		Needs:    needs,
		Inputs:   e.Inputs,
		Matrix:   e.Matrix,
		Strategy: map[string]any{},
		Job:      &model.JobContext{Status: "success"},
		Runner:   map[string]any{},
		Secrets:  map[string]string{},
	}, exprparser.Config{Run: &model.Run{Workflow: e.Workflow, JobID: e.JobID}, Context: "job"})
}

// JobIf evaluates the job's `if:` (default `success()`).
func (e *Env) JobIf() (bool, error) {
	job := e.Workflow.GetJob(e.JobID)
	cond := strings.TrimSpace(job.If.Value)
	if cond == "" {
		cond = "success()"
	}
	cond = stripWrapper(cond)
	v, err := e.interpreter().Evaluate(cond, exprparser.DefaultStatusCheckSuccess)
	if err != nil {
		return false, err
	}
	return exprparser.IsTruthy(v), nil
}

// Matrixes evaluates strategy.matrix and returns the combinations to run.
func (e *Env) Matrixes() ([]map[string]any, error) {
	job := e.Workflow.GetJob(e.JobID)
	if job.Strategy == nil || job.Strategy.RawMatrix.Kind == 0 {
		return []map[string]any{nil}, nil
	}
	node := job.Strategy.RawMatrix
	if err := e.EvaluateNode(&node); err != nil {
		return nil, fmt.Errorf("strategy.matrix: %w", err)
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("strategy.matrix must be a mapping")
	}
	copyJob := *job
	strategy := *job.Strategy
	strategy.RawMatrix = node
	copyJob.Strategy = &strategy
	combos, err := copyJob.GetMatrixes()
	if err != nil {
		return nil, err
	}
	if len(combos) > 256 {
		return nil, fmt.Errorf("matrix produces %d jobs; the limit is 256", len(combos))
	}
	return combos, nil
}

// FailFast reports strategy.fail-fast (default true).
func FailFast(job *model.Job) bool {
	if job.Strategy == nil {
		return true
	}
	return job.Strategy.GetFailFast()
}

// RunsOn returns the interpolated runs-on labels.
func (e *Env) RunsOn() ([]string, error) {
	job := e.Workflow.GetJob(e.JobID)
	var out []string
	for _, l := range job.RunsOn() {
		v, err := e.Interpolate(l)
		if err != nil {
			return nil, err
		}
		switch t := v.(type) {
		case []any:
			for _, i := range t {
				out = append(out, ToString(i))
			}
		default:
			if s := ToString(t); s != "" {
				out = append(out, s)
			}
		}
	}
	return out, nil
}

// Name returns the job's display name, with matrix values appended as GitHub does.
func (e *Env) Name() string {
	job := e.Workflow.GetJob(e.JobID)
	base := e.JobID
	if job.Name != "" {
		if v, err := e.Interpolate(job.Name); err == nil {
			base = ToString(v)
		}
	}
	if len(e.Matrix) == 0 || strings.Contains(job.Name, "matrix.") {
		return base
	}
	keys := make([]string, 0, len(e.Matrix))
	for k := range e.Matrix {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	vals := make([]string, 0, len(keys))
	for _, k := range keys {
		vals = append(vals, ToString(e.Matrix[k]))
	}
	return fmt.Sprintf("%s (%s)", base, strings.Join(vals, ", "))
}

// Interpolate replaces every ${{ }} in s. A string that is exactly one
// expression keeps the expression's type.
func (e *Env) Interpolate(s string) (any, error) {
	return Interpolate(e.interpreter(), s)
}

// EvaluateNode interpolates every string scalar in a YAML node.
func (e *Env) EvaluateNode(node *yaml.Node) error {
	interp := e.interpreter()
	return evaluateNode(interp, node)
}

func evaluateNode(interp exprparser.Interpreter, node *yaml.Node) error {
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range node.Content {
			if err := evaluateNode(interp, c); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for i := 1; i < len(node.Content); i += 2 {
			if err := evaluateNode(interp, node.Content[i]); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		if node.Tag != "!!str" || !strings.Contains(node.Value, "${{") {
			return nil
		}
		v, err := Interpolate(interp, node.Value)
		if err != nil {
			return err
		}
		var replacement yaml.Node
		if err := replacement.Encode(v); err != nil {
			return err
		}
		*node = replacement
	}
	return nil
}

type part struct {
	text string
	expr bool
}

func splitTemplate(s string) ([]part, error) {
	var parts []part
	rest := s
	for {
		start := strings.Index(rest, "${{")
		if start < 0 {
			break
		}
		if start > 0 {
			parts = append(parts, part{text: rest[:start]})
		}
		body := rest[start+3:]
		inStr := false
		end := -1
		for i := 0; i < len(body); i++ {
			switch {
			case body[i] == '\'':
				inStr = !inStr
			case !inStr && body[i] == '}' && i+1 < len(body) && body[i+1] == '}':
				end = i
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			return nil, fmt.Errorf("unclosed '${{' in %q", s)
		}
		parts = append(parts, part{text: strings.TrimSpace(body[:end]), expr: true})
		rest = body[end+2:]
	}
	if rest != "" {
		parts = append(parts, part{text: rest})
	}
	return parts, nil
}

func stripWrapper(s string) string {
	t := strings.TrimSpace(s)
	if parts, err := splitTemplate(t); err == nil && len(parts) == 1 && parts[0].expr {
		return parts[0].text
	}
	return t
}

// Interpolate evaluates the ${{ }} expressions in s.
func Interpolate(interp exprparser.Interpreter, s string) (any, error) {
	if !strings.Contains(s, "${{") {
		return s, nil
	}
	parts, err := splitTemplate(strings.TrimSpace(s))
	if err != nil {
		return nil, err
	}
	if len(parts) == 1 && parts[0].expr {
		return interp.Evaluate(parts[0].text, exprparser.DefaultStatusCheckNone)
	}
	parts, _ = splitTemplate(s)
	var b strings.Builder
	for _, p := range parts {
		if !p.expr {
			b.WriteString(p.text)
			continue
		}
		v, err := interp.Evaluate(p.text, exprparser.DefaultStatusCheckNone)
		if err != nil {
			return nil, err
		}
		b.WriteString(ToString(v))
	}
	return b.String(), nil
}

// ToString formats an expression value the way GitHub interpolates it.
func ToString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	default:
		b, err := json.MarshalIndent(t, "", "  ")
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	}
}

// JobEnvironment is the deployment environment a job names.
type JobEnvironment struct {
	Name string
	URL  string
}

// Environment evaluates the job's environment (src is the workflow file,
// since act's model doesn't keep it): a name, or a mapping with name and url.
// Expressions can use github, inputs, vars, needs and matrix. A url that
// can't be evaluated, such as one using steps, or isn't http(s) is left out.
func (e *Env) Environment(src []byte) (JobEnvironment, error) {
	var doc struct {
		Jobs map[string]struct {
			Environment yaml.Node `yaml:"environment"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return JobEnvironment{}, err
	}
	node := doc.Jobs[e.JobID].Environment
	var out JobEnvironment
	var nameNode, urlNode *yaml.Node
	switch node.Kind {
	case 0:
		return out, nil
	case yaml.ScalarNode:
		nameNode = &node
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			switch node.Content[i].Value {
			case "name":
				nameNode = node.Content[i+1]
			case "url":
				urlNode = node.Content[i+1]
			}
		}
		if nameNode == nil {
			return out, fmt.Errorf("environment: the name is missing")
		}
	default:
		return out, fmt.Errorf("environment must be a name or a mapping with name and url")
	}
	v, err := e.Interpolate(nameNode.Value)
	if err != nil {
		return out, fmt.Errorf("environment: %w", err)
	}
	out.Name = strings.TrimSpace(ToString(v))
	if out.Name == "" {
		return JobEnvironment{}, nil
	}
	if urlNode != nil {
		if v, err := e.Interpolate(urlNode.Value); err == nil {
			u := strings.TrimSpace(ToString(v))
			if strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
				out.URL = u
			}
		}
	}
	return out, nil
}
