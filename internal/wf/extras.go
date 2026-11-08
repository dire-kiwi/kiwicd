package wf

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
	"github.com/nektos/act/pkg/exprparser"
	"github.com/nektos/act/pkg/model"
	"gopkg.in/yaml.v3"
)

// Extras are the parts of a workflow file that act's model doesn't keep.
type Extras struct {
	RunName     yaml.Node            `yaml:"run-name"`
	Concurrency yaml.Node            `yaml:"concurrency"`
	Permissions yaml.Node            `yaml:"permissions"`
	Jobs        map[string]JobExtras `yaml:"jobs"`
}

// JobExtras are a job's settings that act's model doesn't keep.
type JobExtras struct {
	Concurrency     yaml.Node `yaml:"concurrency"`
	ContinueOnError yaml.Node `yaml:"continue-on-error"`
	Permissions     yaml.Node `yaml:"permissions"`
}

// ReadExtras reads a workflow file's Extras.
func ReadExtras(src []byte) (*Extras, error) {
	var x Extras
	if err := yaml.Unmarshal(src, &x); err != nil {
		return nil, err
	}
	return &x, nil
}

// validateExtras checks what can be checked before a run: permissions, and
// the shape of the settings that take expressions.
func validateExtras(src []byte) error {
	x, err := ReadExtras(src)
	if err != nil {
		return err
	}
	if _, err := ParsePermissions(x.Permissions); err != nil {
		return err
	}
	if x.RunName.Kind != 0 && x.RunName.Kind != yaml.ScalarNode {
		return fmt.Errorf("run-name must be a string")
	}
	for id, j := range x.Jobs {
		if _, err := ParsePermissions(j.Permissions); err != nil {
			return fmt.Errorf("job %q: %w", id, err)
		}
		switch j.Concurrency.Kind {
		case 0, yaml.ScalarNode:
		case yaml.MappingNode:
			if !hasKey(j.Concurrency, "group") {
				return fmt.Errorf("job %q: concurrency needs a group", id)
			}
		default:
			return fmt.Errorf("job %q: concurrency must be a group name or a mapping", id)
		}
		if k := j.ContinueOnError.Kind; k != 0 && (k != yaml.ScalarNode || !boolOrExpression(j.ContinueOnError.Value)) {
			return fmt.Errorf("job %q: continue-on-error must be a boolean or an expression", id)
		}
	}
	return nil
}

func boolOrExpression(v string) bool {
	return strings.Contains(v, "${{") || strings.EqualFold(v, "true") || strings.EqualFold(v, "false")
}

func hasKey(node yaml.Node, key string) bool {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return true
		}
	}
	return false
}

// RunName evaluates the workflow's run-name (github, inputs and vars). It
// returns "" if there is none.
func (e *Env) RunName(x *Extras) (string, error) {
	if x.RunName.Kind == 0 {
		return "", nil
	}
	v, err := e.Interpolate(x.RunName.Value)
	if err != nil {
		return "", fmt.Errorf("run-name: %w", err)
	}
	return strings.TrimSpace(ToString(v)), nil
}

// EvalConcurrency evaluates a concurrency setting, a group name or a mapping
// with group and cancel-in-progress. It returns a zero Concurrency for an
// empty node.
func (e *Env) EvalConcurrency(node yaml.Node) (Concurrency, error) {
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
				out.CancelInProgress = truthy(v)
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

// truthy reads a boolean setting that may come from an expression; the
// string "false", in any case, is false.
func truthy(v any) bool {
	return exprparser.IsTruthy(v) && !strings.EqualFold(ToString(v), "false")
}

// JobConcurrency evaluates the job's concurrency (github, inputs, vars,
// needs and matrix).
func (e *Env) JobConcurrency(x *Extras) (Concurrency, error) {
	return e.EvalConcurrency(x.Jobs[e.JobID].Concurrency)
}

// ContinueOnError evaluates the job's continue-on-error, which may use the
// matrix (continue-on-error: ${{ matrix.experimental }}).
func (e *Env) ContinueOnError(x *Extras) (bool, error) {
	node := x.Jobs[e.JobID].ContinueOnError
	if node.Kind == 0 {
		return false, nil
	}
	v, err := e.Interpolate(node.Value)
	if err != nil {
		return false, fmt.Errorf("continue-on-error: %w", err)
	}
	return truthy(v), nil
}

// MaxParallel evaluates the job's strategy.max-parallel; 0 means no limit.
func (e *Env) MaxParallel() (int, error) {
	job := e.Workflow.GetJob(e.JobID)
	if job.Strategy == nil || strings.TrimSpace(job.Strategy.MaxParallelString) == "" {
		return 0, nil
	}
	v, err := e.Interpolate(job.Strategy.MaxParallelString)
	if err != nil {
		return 0, fmt.Errorf("strategy.max-parallel: %w", err)
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(ToString(v)), 64)
	if err != nil || n < 1 || n != float64(int(n)) {
		return 0, fmt.Errorf("strategy.max-parallel must be a whole number of at least 1, not %q", ToString(v))
	}
	return int(n), nil
}

// ParsePermissions reads a permissions: setting; nil means there is none.
func ParsePermissions(node yaml.Node) (*protocol.Permissions, error) {
	switch node.Kind {
	case 0:
		return nil, nil
	case yaml.ScalarNode:
		switch node.Value {
		case "read-all":
			return &protocol.Permissions{All: "read"}, nil
		case "write-all":
			return &protocol.Permissions{All: "write"}, nil
		}
		return nil, fmt.Errorf("permissions must be read-all, write-all or a mapping of scopes, not %q", node.Value)
	case yaml.MappingNode:
		p := &protocol.Permissions{Scopes: map[string]string{}}
		for i := 0; i+1 < len(node.Content); i += 2 {
			scope, level := node.Content[i].Value, node.Content[i+1]
			switch level.Value {
			case "read", "write", "none":
			default:
				return nil, fmt.Errorf("permissions: %s must be read, write or none, not %q", scope, level.Value)
			}
			if level.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("permissions: %s must be read, write or none", scope)
			}
			p.Scopes[scope] = level.Value
		}
		return p, nil
	}
	return nil, fmt.Errorf("permissions must be read-all, write-all or a mapping of scopes")
}

// WorkflowPermissions returns the workflow's permissions, or nil.
func (x *Extras) WorkflowPermissions() *protocol.Permissions {
	p, _ := ParsePermissions(x.Permissions)
	return p
}

// JobPermissions returns the permissions a job gets: its own, else the
// workflow's, or nil.
func (x *Extras) JobPermissions(jobID string) *protocol.Permissions {
	if p, _ := ParsePermissions(x.Jobs[jobID].Permissions); p != nil {
		return p
	}
	return x.WorkflowPermissions()
}

// Triggers reads only the `on:` of a workflow file that may be invalid, so
// that an invalid workflow can be reported for the events it listens to. It
// returns nil if the file's triggers can't be read.
func Triggers(src []byte) *model.Workflow {
	var doc struct {
		On yaml.Node `yaml:"on"`
	}
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil
	}
	// act's model.Workflow.On exits the process on a shape it can't decode.
	switch doc.On.Kind {
	case yaml.ScalarNode:
		if doc.On.Tag != "!!str" {
			return nil
		}
	case yaml.SequenceNode:
		for _, c := range doc.On.Content {
			if c.Kind != yaml.ScalarNode || c.Tag != "!!str" {
				return nil
			}
		}
	case yaml.MappingNode:
		var m map[string]any
		if doc.On.Decode(&m) != nil {
			return nil
		}
	default:
		return nil
	}
	return &model.Workflow{RawOn: doc.On}
}
