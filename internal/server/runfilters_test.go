package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestRunFilters(t *testing.T) {
	srv, ts, _ := newRerunServer(t)
	ctx := context.Background()
	repo, err := srv.getRepo("t/app")
	if err != nil {
		t.Fatal(err)
	}
	const workflow = "on: push\njobs:\n  a:\n    if: false\n    runs-on: x\n    steps:\n      - run: echo\n"
	for _, r := range []struct{ file, name, event, ref, actor string }{
		{".github/workflows/ci.yml", "CI", "push", "refs/heads/main", "ann"},
		{".github/workflows/ci.yml", "CI", "push", "refs/heads/feature", "bob"},
		{".github/workflows/deploy.yml", "Deploy", "workflow_dispatch", "refs/heads/main", "ann"},
		{"kiwicd:nightly.yml", "Nightly", "push", "refs/tags/v1", "bob"},
		{".github/workflows/ci.yml", "Tests", "push", "refs/heads/main", "bob"},
	} {
		if _, err := srv.createRun(ctx, runSpec{repo: repo, file: r.file, yaml: workflow, name: r.name, event: r.event,
			payload: map[string]any{}, ref: r.ref, sha: strings.Repeat("a", 40), actor: r.actor}); err != nil {
			t.Fatal(err)
		}
	}
	// Run 3 fails.
	if _, err := srv.db.Exec(`UPDATE runs SET conclusion = 'failure' WHERE id = 3`); err != nil {
		t.Fatal(err)
	}
	list := func(query string) string {
		t.Helper()
		_, body := get(t, ts, "/api/runs?"+query)
		var runs []Run
		if err := json.Unmarshal(body, &runs); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, r := range runs {
			ids = append(ids, fmt.Sprint(r.ID))
		}
		return strings.Join(ids, ",")
	}
	for query, want := range map[string]string{
		"":                                  "5,4,3,2,1",
		"repo=t/app&workflow=ci.yml":        "5,2,1",
		"workflow=.github/workflows/ci.yml": "5,2,1",
		"workflow=kiwicd:nightly.yml":       "4",
		"branch=main":                       "5,3,1",
		"branch=refs/tags/v1":               "4",
		"event=workflow_dispatch":           "3",
		"actor=bob&branch=main":             "5",
		"status=completed":                  "5,4,3,2,1",
		"status=failure":                    "3",
		"status=success":                    "5,4,2,1",
		"status=queued":                     "",
		"repo=other/app":                    "",
	} {
		if got := list(query); got != want {
			t.Errorf("?%s: runs %s, want %s", query, got, want)
		}
	}

	_, body := get(t, ts, "/api/runs/filters?repo=t/app")
	var f struct {
		Workflows []struct{ File, Name string }
		Refs      []string
		Events    []string
		Actors    []string
	}
	if err := json.Unmarshal(body, &f); err != nil {
		t.Fatal(err)
	}
	var wfs []string
	for _, w := range f.Workflows {
		wfs = append(wfs, w.Name+"="+w.File)
	}
	sort.Strings(wfs)
	if got := strings.Join(wfs, " "); got != "Deploy=.github/workflows/deploy.yml Nightly=kiwicd:nightly.yml Tests=.github/workflows/ci.yml" {
		t.Errorf("workflows: %s", got)
	}
	if got := fmt.Sprint(f.Refs, f.Events, f.Actors); got != "[refs/heads/feature refs/heads/main refs/tags/v1] [push workflow_dispatch] [ann bob]" {
		t.Errorf("refs, events, actors: %s", got)
	}
}
