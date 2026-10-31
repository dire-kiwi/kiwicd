package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nektos/act/pkg/model"
	"github.com/sirupsen/logrus"
)

func tarGz(t *testing.T, entries []tar.Header, bodies map[string]string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		body := bodies[h.Name]
		h.Size = int64(len(body))
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return &buf
}

func TestExtractTarGzStaysInside(t *testing.T) {
	tmp := t.TempDir()
	outside := filepath.Join(tmp, "outside")
	dest := filepath.Join(tmp, "ws")
	for _, d := range []string{outside, dest} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	archive := tarGz(t, []tar.Header{
		{Name: "src/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "src/main.go", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: outside},
		{Name: "link/planted", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string]string{"src/main.go": "package main\n", "../escape": "x", "link/planted": "x"})

	err := extractTarGz(archive, dest)
	if err == nil {
		t.Error("writing through a symlink out of the workspace succeeded")
	}
	if b, err := os.ReadFile(filepath.Join(dest, "src/main.go")); err != nil || string(b) != "package main\n" {
		t.Errorf("src/main.go: %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "escape")); err != nil {
		t.Errorf("../escape wasn't kept inside the workspace: %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("files written outside the workspace: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(tmp, "escape")); err == nil {
		t.Error("../escape written outside the workspace")
	}
}

func TestMaskMultilineSecrets(t *testing.T) {
	key := "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n}\n-----END OPENSSH PRIVATE KEY-----\n"
	r := newReporter(nil, 1, &model.Job{}, map[string]string{"KEY": key, "SHORT": "abc", "LONG": "abcdef"}, nil)
	for in, want := range map[string]string{
		"b3BlbnNzaC1rZXktdjEAAAAA":              "***",
		"x -----END OPENSSH PRIVATE KEY----- y": "x *** y",
		"token=abcdef":                          "token=***",
		"abc":                                   "***",
		"}":                                     "}",
	} {
		if got := r.maskSecrets(in); got != want {
			t.Errorf("mask(%q) = %q, want %q", in, got, want)
		}
	}
	r.logf(0, "error: %s", "abcdef")
	if got := r.pending[len(r.pending)-1].Text; strings.Contains(got, "abc") {
		t.Errorf("agent message not masked: %q", got)
	}
}

func TestGroupCommands(t *testing.T) {
	r := newReporter(nil, 1, &model.Job{}, map[string]string{"KEY": "hunter22"}, nil)
	entry := func(level logrus.Level, msg string, data logrus.Fields) {
		r.handle(&logrus.Entry{Level: level, Message: msg, Data: data, Time: time.Now()})
	}
	entry(logrus.InfoLevel, "  ❓  ::group::Build hunter22\n", logrus.Fields{"command": "group", "arg": "Build hunter22", "raw": "::group::Build hunter22\n"})
	entry(logrus.InfoLevel, "make", logrus.Fields{"raw_output": true})
	entry(logrus.InfoLevel, "  ❓  ::endgroup::\n", logrus.Fields{"command": "endgroup", "arg": "", "raw": "::endgroup::\n"})
	entry(logrus.WarnLevel, "  🚧  ::warning::careful\n", logrus.Fields{"command": "warning", "arg": "careful"})
	var got []string
	for _, l := range r.pending {
		got = append(got, l.Text)
	}
	want := []string{"##[group]Build ***", "make", "##[endgroup]", "##[warning]🚧  ::warning::careful"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("lines = %q, want %q", got, want)
	}
}
