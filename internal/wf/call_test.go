package wf

import (
	"reflect"
	"strings"
	"testing"

	"github.com/nektos/act/pkg/model"
)

func TestParseUses(t *testing.T) {
	for in, want := range map[string]Uses{
		"./.github/workflows/build.yml":                 {Local: true, Path: ".github/workflows/build.yml"},
		"owner/repo/.github/workflows/x.yaml@v1":        {Repo: "owner/repo", Path: ".github/workflows/x.yaml", Ref: "v1"},
		"owner/repo/.github/workflows/x.yml@release/v2": {Repo: "owner/repo", Path: ".github/workflows/x.yml", Ref: "release/v2"},
	} {
		got, err := ParseUses(in)
		if err != nil || got != want {
			t.Errorf("ParseUses(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"./build.yml", "./.github/workflows/a/b.yml", "./.github/workflows/x.yml@main", "owner/repo/.github/workflows/x.yml",
		"owner/.github/workflows/x.yml@v1", "owner/repo/ci/x.yml@v1", "owner/repo/.github/workflows/x.yml@", "owner/repo/.github/workflows/x.yml@-x",
		"owner/repo/.github/workflows/x.yml@a..b", "actions/checkout@v4", "../repo/.github/workflows/x.yml@v1", "o/./.github/workflows/x.yml@v1",
	} {
		if _, err := ParseUses(bad); err == nil {
			t.Errorf("ParseUses(%q) succeeded", bad)
		}
	}
}

const caller = `on: push
jobs:
  call:
    strategy:
      matrix:
        n: [2]
    uses: ./.github/workflows/lib.yml
    with:
      name: build ${{ matrix.n }}
      count: ${{ matrix.n }}
      flag: "true"
    secrets:
      key: ${{ secrets.A }}-x
  inherit:
    uses: ./.github/workflows/lib.yml
    with:
      name: x
    secrets: inherit
`

const callee = `on:
  workflow_call:
    inputs:
      name:
        required: true
      count:
        type: number
      flag:
        type: boolean
      who:
        type: string
        default: ${{ github.actor }}
    secrets:
      key:
    outputs:
      out:
        value: ${{ jobs.a.outputs.v }}/${{ inputs.name }}
jobs:
  a:
    runs-on: x
    steps:
      - run: true
`

func TestCall(t *testing.T) {
	w, err := Parse([]byte(caller))
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse([]byte(callee))
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckCall(w.GetJob("call"), c); err != nil {
		t.Errorf("CheckCall: %v", err)
	}
	env := &Env{Workflow: w, JobID: "call", Github: &model.GithubContext{Actor: "someone"}, Matrix: map[string]any{"n": 2.0},
		Secrets: map[string]string{"A": "a", "B": "b"}}
	inputs, err := env.CallInputs(c)
	if want := map[string]any{"name": "build 2", "count": 2.0, "flag": true, "who": "someone"}; err != nil || !reflect.DeepEqual(inputs, want) {
		t.Errorf("CallInputs = %v, %v; want %v", inputs, err, want)
	}
	secrets, err := env.CallSecrets()
	if err != nil || !reflect.DeepEqual(secrets, map[string]string{"key": "a-x"}) {
		t.Errorf("CallSecrets = %v, %v", secrets, err)
	}
	env.JobID = "inherit"
	if secrets, err = env.CallSecrets(); err != nil || !reflect.DeepEqual(secrets, map[string]string{"A": "a", "B": "b"}) {
		t.Errorf("inherited CallSecrets = %v, %v", secrets, err)
	}
	out := &Env{Workflow: c, Inputs: inputs, Jobs: map[string]JobResult{"a": {Result: "success", Outputs: map[string]string{"v": "1"}}}}
	if outputs, err := out.CallOutputs(); err != nil || outputs["out"] != "1/build 2" {
		t.Errorf("CallOutputs = %v, %v", outputs, err)
	}

	for typ, cases := range map[string]map[any]any{
		"boolean": {nil: false, true: true, "false": false, "yes": nil, 1.0: nil},
		"number":  {nil: 0.0, 3: 3.0, "4.5": 4.5, "x": nil, true: nil},
		"string":  {nil: "", 3.0: "3", true: "true", "s": "s"},
	} {
		for in, want := range cases {
			got, err := inputValue(typ, in)
			if (want == nil) != (err != nil) || (err == nil && got != want) {
				t.Errorf("inputValue(%s, %v) = %v, %v; want %v", typ, in, got, err, want)
			}
		}
	}

	for name, yml := range map[string]string{
		`no input "nope"`:          strings.Replace(caller, "      flag: \"true\"", "      nope: 1", 1),
		`input "name" is required`: strings.Replace(caller, "      name: build ${{ matrix.n }}\n", "", 1),
		`no secret "other"`:        strings.Replace(caller, "      key: ${{ secrets.A }}-x", "      other: x", 1),
	} {
		w, err := Parse([]byte(yml))
		if err != nil {
			t.Fatal(err)
		}
		if err := CheckCall(w.GetJob("call"), c); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("CheckCall = %v, want an error with %q", err, name)
		}
	}
	plain, _ := Parse([]byte("on: push\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: true\n"))
	if err := CheckCall(w.GetJob("call"), plain); err == nil {
		t.Error("CheckCall accepted a workflow without workflow_call")
	}
}
