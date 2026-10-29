package wf

import (
	"testing"

	"github.com/nektos/act/pkg/model"
)

const src = `
name: CI
on:
  push:
    branches: [main, 'release/**']
    paths-ignore: ['docs/**']
  workflow_dispatch:
    inputs:
      target: {description: where, default: prod}
jobs:
  build:
    runs-on: [self-hosted, linux]
    outputs:
      os: ${{ steps.x.outputs.os }}
    steps: [{run: echo}]
  test:
    needs: build
    name: Test
    runs-on: ${{ matrix.os }}
    strategy:
      matrix:
        os: [linux, "${{ needs.build.outputs.os }}"]
        n: [1, 2]
        include:
          - os: linux
            extra: x
    steps: [{run: echo}]
  deploy:
    needs: test
    if: github.ref == 'refs/heads/main'
    runs-on: self-hosted
    steps: [{run: echo}]
`

func TestWorkflow(t *testing.T) {
	w, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	order, _ := Order(w)
	if order[0] != "build" || order[2] != "deploy" {
		t.Fatalf("order %v", order)
	}
	if !MatchesPush(w, "refs/heads/main", nil) || !MatchesPush(w, "refs/heads/release/1", nil) || MatchesPush(w, "refs/heads/dev", nil) {
		t.Fatal("branch filter")
	}
	if MatchesPush(w, "refs/heads/main", []string{"docs/a.md"}) || !MatchesPush(w, "refs/heads/main", []string{"docs/a.md", "x.go"}) {
		t.Fatal("paths-ignore")
	}
	if len(DispatchInputs(w)) != 1 {
		t.Fatal("inputs")
	}
	env := &Env{Workflow: w, JobID: "test", Github: &model.GithubContext{Ref: "refs/heads/main"},
		Results: map[string]JobResult{"build": {Result: "success", Outputs: map[string]string{"os": "mac"}}}}
	ok, err := env.JobIf()
	if err != nil || !ok {
		t.Fatal("if", err)
	}
	combos, err := env.Matrixes()
	if err != nil {
		t.Fatal(err)
	}
	var mac map[string]any
	for _, c := range combos {
		if c["os"] == "mac" && c["n"] == 1 {
			mac = c
		}
	}
	if len(combos) != 4 || mac == nil || mac["extra"] != nil {
		t.Fatalf("combos %v", combos)
	}
	env.Matrix = mac
	labels, _ := env.RunsOn()
	if len(labels) != 1 || labels[0] != "mac" {
		t.Fatalf("runs-on %v", labels)
	}
	if env.Name() != "Test (1, mac)" {
		t.Fatalf("name %q", env.Name())
	}
	denv := &Env{Workflow: w, JobID: "deploy", Github: &model.GithubContext{Ref: "refs/heads/main"},
		Results: map[string]JobResult{"build": {Result: "success"}, "test": {Result: "failure"}}}
	if ok, _ := denv.JobIf(); ok {
		t.Fatal("deploy should not run after failure")
	}
}

func TestRepositoryPush(t *testing.T) {
	w, err := Parse([]byte(`
on:
  repository_push:
    repositories: [Owner/App, owner/lib]
    branches: [main]
    paths-ignore: ['docs/**']
  push:
jobs:
  deploy:
    runs-on: self-hosted
    steps: [{run: echo}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := RepositoryPushSources(w); len(got) != 2 || got[0] != "Owner/App" {
		t.Fatalf("sources %v", got)
	}
	for _, c := range []struct {
		repo, ref string
		changed   []string
		want      bool
	}{
		{"owner/app", "refs/heads/main", nil, true},
		{"owner/lib", "refs/heads/main", []string{"x.go"}, true},
		{"owner/app", "refs/heads/dev", nil, false},
		{"owner/app", "refs/tags/v1", nil, false},
		{"owner/app", "refs/heads/main", []string{"docs/a.md"}, false},
		{"owner/other", "refs/heads/main", nil, false},
	} {
		if got := MatchesRepositoryPush(w, c.repo, c.ref, c.changed); got != c.want {
			t.Errorf("%s %s %v: got %v", c.repo, c.ref, c.changed, got)
		}
	}
	// A plain push trigger doesn't listen to other repositories, and a
	// single repository can be a string.
	if !MatchesPush(w, "refs/heads/dev", nil) {
		t.Error("push")
	}
	w, err = Parse([]byte("on:\n  repository_push:\n    repositories: owner/app\njobs:\n  a:\n    runs-on: x\n    steps: [{run: echo}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !MatchesRepositoryPush(w, "owner/app", "refs/tags/v1", nil) || MatchesPush(w, "refs/heads/main", nil) || len(RepositoryPushSources(w)) != 1 {
		t.Error("string repositories")
	}
}

func TestRepositoryPullRequest(t *testing.T) {
	w, err := Parse([]byte(`
on:
  repository_push:
    repositories: [o/app]
  repository_pull_request:
    repositories: [o/app, O/Lib]
    types: [opened, closed]
    branches: [main]
jobs:
  x: {runs-on: a, steps: [{run: echo}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	pr := func(repo, action, base string, fork bool) bool {
		return MatchesRepositoryPullRequest(w, PullRequest{Repo: repo, Action: action, Base: base, Fork: fork})
	}
	if !pr("o/app", "opened", "main", false) || !pr("o/lib", "closed", "main", false) {
		t.Error("listened pull requests don't match")
	}
	if pr("o/other", "opened", "main", false) || pr("o/app", "synchronize", "main", false) || pr("o/app", "opened", "dev", false) {
		t.Error("repositories, types or branches not checked")
	}
	if pr("o/app", "opened", "main", true) {
		t.Error("a fork's pull request matched without forks: true")
	}
	if got := ListenedRepositories(w); len(got) != 2 || got[0] != "o/app" || got[1] != "O/Lib" {
		t.Errorf("listened repositories: %v", got)
	}

	w, _ = Parse([]byte(`
on:
  repository_pull_request:
    repositories: o/app
    branches-ignore: [old]
    forks: true
jobs:
  x: {runs-on: a, steps: [{run: echo}]}
`))
	for action, want := range map[string]bool{"opened": true, "synchronize": true, "reopened": true, "closed": false, "labeled": false} {
		if got := MatchesRepositoryPullRequest(w, PullRequest{Repo: "o/app", Action: action, Base: "main", Fork: true}); got != want {
			t.Errorf("default types: %s = %v", action, got)
		}
	}
	if MatchesRepositoryPullRequest(w, PullRequest{Repo: "o/app", Action: "opened", Base: "old"}) {
		t.Error("branches-ignore")
	}
}

func TestWorkflowConcurrency(t *testing.T) {
	env := func(src string) *Env {
		w, err := Parse([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		return &Env{Workflow: w, Github: &model.GithubContext{Ref: "refs/heads/main",
			Event: map[string]any{"number": 7.0, "action": "closed"}}, Vars: map[string]string{"ENV": "prod"}}
	}
	for _, tc := range []struct {
		src  string
		want Concurrency
		err  bool
	}{
		{src: "", want: Concurrency{}},
		{src: "concurrency: deploy-${{ vars.ENV }}\n", want: Concurrency{Group: "deploy-prod"}},
		{src: "concurrency:\n  group: pr-${{ github.event.number }}\n  cancel-in-progress: true\n",
			want: Concurrency{Group: "pr-7", CancelInProgress: true}},
		{src: "concurrency:\n  group: x\n  cancel-in-progress: false\n", want: Concurrency{Group: "x"}},
		{src: "concurrency:\n  group: x\n  cancel-in-progress: ${{ github.event.action != 'closed' }}\n", want: Concurrency{Group: "x"}},
		{src: "concurrency:\n  group: x\n  cancel-in-progress: ${{ github.event.action == 'closed' }}\n",
			want: Concurrency{Group: "x", CancelInProgress: true}},
		{src: "concurrency:\n  cancel-in-progress: true\n", err: true},
	} {
		src := "on: push\n" + tc.src + "jobs:\n  x: {runs-on: a, steps: [{run: echo}]}\n"
		got, err := WorkflowConcurrency([]byte(src), env(src))
		if (err != nil) != tc.err || got != tc.want {
			t.Errorf("%q: %+v %v, want %+v", tc.src, got, err, tc.want)
		}
	}
}
