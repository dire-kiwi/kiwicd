package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const zeroSHA = "0000000000000000000000000000000000000000"

func (s *Server) repoPath(name string) string {
	return filepath.Join(s.cfg.DataDir, "repos", name+".git")
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	return gitEnv(ctx, dir, nil, args...)
}

func gitEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false")
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, redactURL(strings.TrimSpace(stderr.String())))
	}
	return stdout.String(), nil
}

// redactURL hides credentials embedded in remote URLs in error messages.
func redactURL(s string) string {
	for {
		i := strings.Index(s, "://")
		if i < 0 {
			return s
		}
		rest := s[i+3:]
		at := strings.Index(rest, "@")
		slash := strings.IndexAny(rest, "/ ")
		if at < 0 || (slash >= 0 && slash < at) {
			return s
		}
		// An SSH user name (git@) is not a secret; HTTP user names often
		// are tokens.
		if strings.HasSuffix(s[:i], "ssh") && !strings.Contains(rest[:at], ":") {
			return s[:i+3] + rest[:at] + redactURL(rest[at:])
		}
		return s[:i+3] + "***" + redactURL(rest[at:])
	}
}

func (s *Server) initHostedRepo(ctx context.Context, name string) error {
	p := s.repoPath(name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if _, err := git(ctx, "", "init", "--bare", "--initial-branch=main", p); err != nil {
		return err
	}
	_, err := git(ctx, p, "config", "http.receivepack", "true")
	return err
}

func (s *Server) cloneMirror(ctx context.Context, repo *Repo) error {
	p := s.repoPath(repo.Name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	env, cleanup, err := s.gitSSHEnv(repo)
	if err != nil {
		return err
	}
	defer cleanup()
	_, err = gitEnv(ctx, "", env, "clone", "--mirror", "--", repo.URL, p)
	return err
}

func (s *Server) fetchMirror(ctx context.Context, repo *Repo) error {
	p := s.repoPath(repo.Name)
	if _, err := os.Stat(p); err != nil {
		return s.cloneMirror(ctx, repo)
	}
	if _, err := git(ctx, p, "remote", "set-url", "origin", repo.URL); err != nil {
		return err
	}
	env, cleanup, err := s.gitSSHEnv(repo)
	if err != nil {
		return err
	}
	defer cleanup()
	_, err = gitEnv(ctx, p, env, "fetch", "--prune", "--tags", "origin", "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*")
	return err
}

// listRefs returns branch and tag heads (tags peeled to commits).
func listRefs(ctx context.Context, repo string) (map[string]string, error) {
	out, err := git(ctx, repo, "for-each-ref", "--format=%(refname) %(objectname) %(*objectname)", "refs/heads", "refs/tags")
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		sha := f[1]
		if len(f) == 3 && f[2] != "" {
			sha = f[2]
		}
		refs[f[0]] = sha
	}
	return refs, nil
}

func resolveRef(ctx context.Context, repo, ref string) (string, error) {
	out, err := git(ctx, repo, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func changedFiles(ctx context.Context, repo, before, after string) []string {
	if before == "" || before == zeroSHA {
		return nil
	}
	out, err := git(ctx, repo, "diff", "--name-only", before, after)
	if err != nil {
		return nil
	}
	files := []string{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l != "" {
			files = append(files, l)
		}
	}
	return files
}

// workflowFiles returns .github/workflows/*.yml at sha.
func workflowFiles(ctx context.Context, repo, sha string) (map[string][]byte, error) {
	out, err := git(ctx, repo, "ls-tree", "--name-only", sha, ".github/workflows/")
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	for _, path := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.HasSuffix(path, ".yml") && !strings.HasSuffix(path, ".yaml") {
			continue
		}
		content, err := git(ctx, repo, "show", sha+":"+path)
		if err != nil {
			return nil, err
		}
		files[path] = []byte(content)
	}
	return files, nil
}

type commitInfo struct {
	Subject, Author, Email string
}

func readCommit(ctx context.Context, repo, sha string) commitInfo {
	out, err := git(ctx, repo, "log", "-1", "--format=%s%x00%an%x00%ae", sha)
	if err != nil {
		return commitInfo{}
	}
	f := strings.SplitN(strings.TrimSpace(out), "\x00", 3)
	for len(f) < 3 {
		f = append(f, "")
	}
	return commitInfo{Subject: f[0], Author: f[1], Email: f[2]}
}

func archive(ctx context.Context, repo, sha string, w io.Writer) error {
	cmd := exec.CommandContext(ctx, "git", "archive", "--format=tar.gz", sha)
	cmd.Dir = repo
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git archive: %v: %s", err, stderr.String())
	}
	return nil
}
