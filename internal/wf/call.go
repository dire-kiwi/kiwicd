package wf

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/nektos/act/pkg/model"
	"gopkg.in/yaml.v3"
)

// Reusable workflows: a job with `uses:` runs the jobs of another workflow,
// one that has `on: workflow_call`. The server reads the called workflows
// when a run starts and runs their jobs as part of the run.

// MaxCallDepth is how many levels of workflows can be connected: the run's
// own workflow and up to three levels of reusable workflows below it, as on
// GitHub.
const MaxCallDepth = 4

// MaxCalls limits the reusable workflows one run calls, counting each call.
const MaxCalls = 50

// Uses is a parsed `jobs.<id>.uses`.
type Uses struct {
	// Local is ./.github/workflows/x.yml: the same repository and commit as
	// the workflow calling it.
	Local bool
	Repo  string // owner/name, for a remote workflow
	Path  string // .github/workflows/x.yml
	Ref   string // branch, tag or commit, for a remote workflow
}

var (
	workflowPathRE = regexp.MustCompile(`^\.github/workflows/[^/]+\.ya?ml$`)
	repoNamePart   = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

// ParseUses parses ./.github/workflows/x.yml or
// owner/repo/.github/workflows/x.yml@ref.
func ParseUses(s string) (Uses, error) {
	if path, ok := strings.CutPrefix(s, "./"); ok {
		if !workflowPathRE.MatchString(path) {
			return Uses{}, fmt.Errorf("uses: %q: a local workflow is ./.github/workflows/<file>.yml", s)
		}
		return Uses{Local: true, Path: path}, nil
	}
	at := strings.LastIndex(s, "@")
	if at < 0 {
		return Uses{}, fmt.Errorf("uses: %q: a workflow in another repository is owner/repo/.github/workflows/<file>.yml@ref", s)
	}
	parts := strings.SplitN(s[:at], "/", 3)
	ref := s[at+1:]
	if len(parts) < 3 || !repoNamePart.MatchString(parts[0]) || !repoNamePart.MatchString(parts[1]) || !workflowPathRE.MatchString(parts[2]) ||
		ref == "" || strings.HasPrefix(ref, "-") || strings.ContainsAny(ref, " \t\n:^~?*[\\") || strings.Contains(ref, "..") {
		return Uses{}, fmt.Errorf("uses: %q: a workflow in another repository is owner/repo/.github/workflows/<file>.yml@ref", s)
	}
	return Uses{Repo: parts[0] + "/" + parts[1], Path: parts[2], Ref: ref}, nil
}

// callConfig is a reusable workflow's `on.workflow_call`. act's model leaves
// out its secrets.
type callConfig struct {
	Inputs  map[string]model.WorkflowCallInput  `yaml:"inputs"`
	Outputs map[string]model.WorkflowCallOutput `yaml:"outputs"`
	Secrets map[string]struct {
		Required bool `yaml:"required"`
	} `yaml:"secrets"`
}

func workflowCall(w *model.Workflow) callConfig {
	var cfg callConfig
	if w.RawOn.Kind != yaml.MappingNode {
		return cfg
	}
	for i := 0; i+1 < len(w.RawOn.Content); i += 2 {
		if w.RawOn.Content[i].Value == "workflow_call" {
			_ = w.RawOn.Content[i+1].Decode(&cfg)
		}
	}
	return cfg
}

// CheckCall checks a calling job against the workflow it calls, as far as
// can be done before the run starts: the callee is reusable, every with:
// input and secret it's given exists, and the required ones are given.
func CheckCall(caller *model.Job, callee *model.Workflow) error {
	if !HasEvent(callee, "workflow_call") {
		return fmt.Errorf("it isn't a reusable workflow: it has no `on: workflow_call`")
	}
	cfg := workflowCall(callee)
	for name := range caller.With {
		if _, ok := cfg.Inputs[name]; !ok {
			return fmt.Errorf("with: the called workflow has no input %q", name)
		}
	}
	for _, name := range sortedKeys(cfg.Inputs) {
		in := cfg.Inputs[name]
		if _, given := caller.With[name]; in.Required && !given && in.Default.Kind == 0 {
			return fmt.Errorf("with: the called workflow's input %q is required", name)
		}
		switch in.Type {
		case "", "string", "boolean", "number":
		default:
			return fmt.Errorf("the called workflow's input %q has type %q; it can be string, boolean or number", name, in.Type)
		}
	}
	if caller.InheritSecrets() {
		return nil
	}
	if caller.RawSecrets.Kind != 0 && caller.RawSecrets.Kind != yaml.MappingNode {
		return fmt.Errorf("secrets: expected inherit or a mapping")
	}
	given := caller.Secrets()
	for name := range given {
		if _, ok := cfg.Secrets[name]; !ok {
			return fmt.Errorf("secrets: the called workflow has no secret %q", name)
		}
	}
	for _, name := range sortedKeys(cfg.Secrets) {
		if _, ok := given[name]; cfg.Secrets[name].Required && !ok {
			return fmt.Errorf("secrets: the called workflow's secret %q is required", name)
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// CallInputs evaluates the calling job's with: (e is the caller's job
// environment, with its matrix) and returns the called workflow's inputs
// context: given values converted to the inputs' types, defaults for the
// rest.
func (e *Env) CallInputs(callee *model.Workflow) (map[string]any, error) {
	caller := e.Workflow.GetJob(e.JobID)
	cfg := workflowCall(callee)
	out := map[string]any{}
	interp := e.interpreter()
	// Defaults can use the github context.
	inner := &Env{Workflow: callee, Github: e.Github, Vars: e.Vars}
	for _, name := range sortedKeys(cfg.Inputs) {
		in := cfg.Inputs[name]
		var v any
		if raw, ok := caller.With[name]; ok {
			var node yaml.Node
			if err := node.Encode(raw); err != nil {
				return nil, fmt.Errorf("with.%s: %w", name, err)
			}
			if err := evaluateNode(interp, &node); err != nil {
				return nil, fmt.Errorf("with.%s: %w", name, err)
			}
			if err := node.Decode(&v); err != nil {
				return nil, fmt.Errorf("with.%s: %w", name, err)
			}
		} else if in.Default.Kind != 0 {
			node := in.Default
			if err := inner.EvaluateNode(&node); err != nil {
				return nil, fmt.Errorf("inputs.%s.default: %w", name, err)
			}
			if err := node.Decode(&v); err != nil {
				return nil, fmt.Errorf("inputs.%s.default: %w", name, err)
			}
		}
		typed, err := inputValue(in.Type, v)
		if err != nil {
			return nil, fmt.Errorf("with.%s: %w", name, err)
		}
		out[name] = typed
	}
	return out, nil
}

// inputValue converts an input's value to its type; nil becomes the type's
// zero value, as on GitHub.
func inputValue(typ string, v any) (any, error) {
	switch typ {
	case "boolean":
		switch t := v.(type) {
		case nil:
			return false, nil
		case bool:
			return t, nil
		case string:
			if t == "true" || t == "false" {
				return t == "true", nil
			}
		}
		return nil, fmt.Errorf("expected a boolean, got %q", ToString(v))
	case "number":
		switch t := v.(type) {
		case nil:
			return 0.0, nil
		case int:
			return float64(t), nil
		case int64:
			return float64(t), nil
		case float64:
			return t, nil
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
				return f, nil
			}
		}
		return nil, fmt.Errorf("expected a number, got %q", ToString(v))
	default:
		switch v.(type) {
		case map[string]any, []any:
			return nil, fmt.Errorf("expected a string, got %s", ToString(v))
		}
		return ToString(v), nil
	}
}

// CallSecrets returns the secrets a calling job passes to the workflow it
// calls: e.Secrets with `secrets: inherit`, or its secrets: mapping
// evaluated in e, the caller's job environment.
func (e *Env) CallSecrets() (map[string]string, error) {
	caller := e.Workflow.GetJob(e.JobID)
	if caller.InheritSecrets() {
		out := make(map[string]string, len(e.Secrets))
		for k, v := range e.Secrets {
			out[k] = v
		}
		return out, nil
	}
	out := map[string]string{}
	interp := e.interpreter()
	for name, expr := range caller.Secrets() {
		v, err := Interpolate(interp, expr)
		if err != nil {
			return nil, fmt.Errorf("secrets.%s: %w", name, err)
		}
		out[name] = ToString(v)
	}
	return out, nil
}

// CallOutputs evaluates the called workflow's on.workflow_call.outputs with
// the results of its jobs (e.Jobs), for the calling job's outputs.
func (e *Env) CallOutputs() (map[string]string, error) {
	cfg := workflowCall(e.Workflow)
	out := map[string]string{}
	interp := e.interpreter()
	for _, name := range sortedKeys(cfg.Outputs) {
		v, err := Interpolate(interp, cfg.Outputs[name].Value)
		if err != nil {
			return nil, fmt.Errorf("outputs.%s: %w", name, err)
		}
		out[name] = ToString(v)
	}
	return out, nil
}

// jobsContext is the jobs context of a reusable workflow's outputs.
func jobsContext(results map[string]JobResult) *map[string]*model.WorkflowCallResult {
	if results == nil {
		return nil
	}
	m := make(map[string]*model.WorkflowCallResult, len(results))
	for id, r := range results {
		outputs := r.Outputs
		if outputs == nil {
			outputs = map[string]string{}
		}
		m[id] = &model.WorkflowCallResult{Outputs: outputs}
	}
	return &m
}
