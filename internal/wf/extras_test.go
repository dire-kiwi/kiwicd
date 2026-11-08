package wf

import (
	"reflect"
	"strings"
	"testing"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
	"github.com/nektos/act/pkg/model"
)

const extrasSrc = `
name: Deploy
run-name: Deploy ${{ inputs.target }} by ${{ github.actor }}
on:
  workflow_dispatch:
    inputs:
      target: {default: prod}
permissions: read-all
jobs:
  build:
    runs-on: x
    permissions:
      contents: read
      packages: write
    steps: [{run: echo}]
  test:
    needs: build
    runs-on: x
    continue-on-error: ${{ matrix.experimental }}
    concurrency:
      group: test-${{ needs.build.outputs.env }}-${{ matrix.os }}
      cancel-in-progress: ${{ inputs.target != 'prod' }}
    strategy:
      max-parallel: ${{ vars.PARALLEL }}
      matrix:
        os: [linux, mac]
        experimental: [false]
        include:
          - os: next
            experimental: true
    steps: [{run: echo}]
  deploy:
    needs: test
    runs-on: x
    continue-on-error: true
    concurrency: deploy
    strategy:
      max-parallel: 2
    steps: [{run: echo}]
`

func TestExtras(t *testing.T) {
	w, err := Parse([]byte(extrasSrc))
	if err != nil {
		t.Fatal(err)
	}
	x, err := ReadExtras([]byte(extrasSrc))
	if err != nil {
		t.Fatal(err)
	}
	env := &Env{Workflow: w, Github: &model.GithubContext{Actor: "ann"}, Inputs: map[string]any{"target": "staging"},
		Vars: map[string]string{"PARALLEL": "3"}, Results: map[string]JobResult{"build": {Result: "success", Outputs: map[string]string{"env": "qa"}}}}
	if name, err := env.RunName(x); err != nil || name != "Deploy staging by ann" {
		t.Errorf("run-name: %q %v", name, err)
	}

	if p := x.WorkflowPermissions(); !reflect.DeepEqual(p, &protocol.Permissions{All: "read"}) {
		t.Errorf("workflow permissions: %+v", p)
	}
	build := x.JobPermissions("build")
	if build.Level("contents") != "read" || build.Level("packages") != "write" || build.Level("issues") != "none" {
		t.Errorf("job permissions: %+v", build)
	}
	if p := x.JobPermissions("test"); p.Level("issues") != "read" {
		t.Errorf("inherited permissions: %+v", p)
	}

	env.JobID = "test"
	combos, err := env.Matrixes()
	if err != nil || len(combos) != 3 {
		t.Fatalf("matrix: %v %v", combos, err)
	}
	for _, combo := range combos {
		env.Matrix = combo
		coe, err := env.ContinueOnError(x)
		if err != nil || coe != (combo["os"] == "next") {
			t.Errorf("continue-on-error for %v: %v %v", combo, coe, err)
		}
		c, err := env.JobConcurrency(x)
		if want := (Concurrency{Group: "test-qa-" + combo["os"].(string), CancelInProgress: true}); err != nil || c != want {
			t.Errorf("concurrency for %v: %+v %v", combo, c, err)
		}
	}
	if n, err := env.MaxParallel(); err != nil || n != 3 {
		t.Errorf("max-parallel from vars: %d %v", n, err)
	}
	env.Vars = map[string]string{"PARALLEL": "0"}
	if _, err := env.MaxParallel(); err == nil {
		t.Error("max-parallel 0 accepted")
	}

	env.JobID, env.Matrix = "deploy", nil
	if coe, _ := env.ContinueOnError(x); !coe {
		t.Error("continue-on-error: true")
	}
	if c, _ := env.JobConcurrency(x); c != (Concurrency{Group: "deploy"}) {
		t.Errorf("deploy concurrency: %+v", c)
	}
	if n, _ := env.MaxParallel(); n != 2 {
		t.Errorf("max-parallel: %d", n)
	}
	env.JobID = "build"
	if coe, _ := env.ContinueOnError(x); coe {
		t.Error("continue-on-error without the setting")
	}
	if !truthy(true) || truthy("FALSE") || truthy(false) || !truthy("true") {
		t.Error("truthy")
	}
	if c, _ := env.JobConcurrency(x); c.Group != "" {
		t.Errorf("concurrency without the setting: %+v", c)
	}
	if n, _ := env.MaxParallel(); n != 0 {
		t.Errorf("max-parallel without a strategy: %d", n)
	}
}

func TestParseRejectsInvalidExtras(t *testing.T) {
	for _, tc := range []struct{ extra, job, want string }{
		{extra: "permissions: everything\n", want: "permissions"},
		{extra: "permissions:\n  contents: admin\n", want: "permissions"},
		{job: "    permissions: {issues: maybe}\n", want: "permissions"},
		{job: "    concurrency: {cancel-in-progress: true}\n", want: "group"},
		{job: "    concurrency: [a, b]\n", want: "concurrency"},
		{job: "    continue-on-error: [true]\n", want: "continue-on-error"},
		{job: "    continue-on-error: maybe\n", want: "continue-on-error"},
	} {
		src := "on: push\n" + tc.extra + "jobs:\n  x:\n    runs-on: a\n" + tc.job + "    steps: [{run: echo}]\n"
		if _, err := Parse([]byte(src)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s%s: %v", tc.extra, tc.job, err)
		}
	}
	ok := "on: push\npermissions: {}\nconcurrency: x\njobs:\n  x:\n    runs-on: a\n    permissions: write-all\n" +
		"    concurrency: y\n    continue-on-error: ${{ false }}\n    steps: [{run: echo}]\n"
	if _, err := Parse([]byte(ok)); err != nil {
		t.Error(err)
	}
	x, _ := ReadExtras([]byte(ok))
	if p := x.WorkflowPermissions(); p == nil || p.Level("contents") != "none" {
		t.Errorf("permissions: {} gives no scope: %+v", p)
	}
	if p := x.JobPermissions("x"); p.Level("contents") != "write" {
		t.Errorf("write-all: %+v", p)
	}
	if x, _ := ReadExtras([]byte("on: push\njobs:\n  x: {runs-on: a}\n")); x.JobPermissions("x") != nil {
		t.Error("permissions without a setting")
	}
}

func TestTriggers(t *testing.T) {
	for _, tc := range []struct {
		src  string
		push bool // listens for pushes to main
		none bool // triggers unreadable
	}{
		{src: "on: push\njobs: {x: 1}\n", push: true},
		{src: "on: [workflow_dispatch, push]\njobs: [", none: true}, // not YAML
		{src: "on: [workflow_dispatch, push]\njobs: 3\n", push: true},
		{src: "on:\n  push:\n    branches: [release]\njobs: 3\n"},
		{src: "on: workflow_dispatch\njobs: {}\n"},
		{src: "on: [push, {x: 1}]\n", none: true},
		{src: "jobs: {}\n", none: true},
		{src: "on: 3\n", none: true},
	} {
		w := Triggers([]byte(tc.src))
		if (w == nil) != tc.none || (w != nil && MatchesPush(w, "refs/heads/main", nil) != tc.push) {
			t.Errorf("%q: %v", tc.src, w)
		}
	}
}
