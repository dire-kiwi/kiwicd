package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestSSHKeys(t *testing.T) {
	priv, pub, err := generateSSHKey("kiwicd t/r")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pub, "ssh-ed25519 ") || !strings.HasSuffix(pub, " kiwicd t/r") {
		t.Errorf("public key %q", pub)
	}
	// Pasted keys are normalised: CRLF and missing trailing newline.
	norm, pub2, err := parseSSHKey(strings.ReplaceAll(strings.TrimSpace(priv), "\n", "\r\n"), "kiwicd t/r")
	if err != nil {
		t.Fatal(err)
	}
	if norm != priv || pub2 != pub {
		t.Errorf("parse(generate) differs:\n%q\n%q", norm, pub2)
	}
	_, k, _ := ed25519.GenerateKey(rand.Reader)
	block, _ := ssh.MarshalPrivateKeyWithPassphrase(k, "", []byte("pw"))
	if _, _, err := parseSSHKey(string(pem.EncodeToMemory(block)), ""); err == nil || !strings.Contains(err.Error(), "passphrase") {
		t.Errorf("passphrase key: %v", err)
	}
	if _, _, err := parseSSHKey("nope", ""); err == nil {
		t.Error("garbage key accepted")
	}

	line := "github.com " + strings.Fields(pub)[0] + " " + strings.Fields(pub)[1]
	if err := checkKnownHosts("# github.com:22 SSH-2.0\n" + line + "\n# trailing comment\n"); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{"# only a comment\n", "github.com ssh-ed25519 notbase64"} {
		if err := checkKnownHosts(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if got := hostKeys([]byte(line + "\n")); len(got) != 1 || !strings.HasPrefix(got[0], "github.com ssh-ed25519 SHA256:") {
		t.Errorf("hostKeys %v", got)
	}
	if got := shellQuote("/a b/it's"); got != `'/a b/it'\''s'` {
		t.Errorf("shellQuote %s", got)
	}
}

// sshServer starts an unprivileged sshd that lets the current user in with
// authorized_keys (initially empty).
type sshServer struct {
	addr, user, authorizedKeys, hostKeyPub string
}

func startSSHD(t *testing.T) *sshServer {
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		t.Skip("sshd not installed")
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh not installed")
	}
	u, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	hostKey := filepath.Join(dir, "host_key")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", hostKey).CombinedOutput(); err != nil {
		t.Skipf("ssh-keygen: %v %s", err, out)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	_, port, _ := net.SplitHostPort(addr)
	s := &sshServer{addr: addr, user: u.Username, authorizedKeys: filepath.Join(dir, "authorized_keys")}
	_ = os.WriteFile(s.authorizedKeys, nil, 0o600)
	b, _ := os.ReadFile(hostKey + ".pub")
	s.hostKeyPub = string(b)
	cfg := filepath.Join(dir, "sshd_config")
	_ = os.WriteFile(cfg, []byte(fmt.Sprintf(`Port %s
ListenAddress 127.0.0.1
HostKey %s
PidFile %s
AuthorizedKeysFile %s
StrictModes no
UsePAM no
PasswordAuthentication no
KbdInteractiveAuthentication no
`, port, hostKey, filepath.Join(dir, "pid"), s.authorizedKeys)), 0o600)
	cmd := exec.Command(sshd, "-D", "-e", "-f", cfg)
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Skipf("sshd: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	for i := 0; i < 50; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			return s
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Skip("sshd did not start")
	return nil
}

func TestMirrorOverSSH(t *testing.T) {
	sshd := startSSHD(t)
	tmp := t.TempDir()

	// The remote: a bare repository with a workflow, pushed from a work tree.
	remote := filepath.Join(tmp, "remote.git")
	work := filepath.Join(tmp, "work")
	gitT := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	gitT(tmp, "init", "-q", "--bare", "-b", "main", remote)
	gitT(tmp, "init", "-q", "-b", "main", work)
	_ = os.MkdirAll(filepath.Join(work, ".github/workflows"), 0o755)
	_ = os.WriteFile(filepath.Join(work, ".github/workflows/ci.yml"), []byte("on: push\njobs:\n  a:\n    runs-on: self-hosted\n    steps:\n      - run: true\n"), 0o644)
	gitT(work, "add", "-A")
	gitT(work, "commit", "-qm", "one")
	gitT(work, "push", "-q", remote, "main")
	commit := func(msg string) {
		_ = os.WriteFile(filepath.Join(work, "f"), []byte(msg), 0o644)
		gitT(work, "add", "-A")
		gitT(work, "commit", "-qm", msg)
		gitT(work, "push", "-q", remote, "main")
	}

	srv, err := New(Config{DataDir: filepath.Join(tmp, "data"), AdminPassword: "pw", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	api := func(method, path, body string, out any) int {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.SetBasicAuth("admin", "pw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if out != nil {
			_ = json.Unmarshal(b, out)
		}
		return resp.StatusCode
	}
	runCount := func() int {
		var runs []struct{ ID int64 }
		api("GET", "/api/runs?repo=t/mirror", "", &runs)
		return len(runs)
	}
	url := fmt.Sprintf("ssh://%s@%s%s", sshd.user, sshd.addr, remote)

	// A pasted key the remote doesn't accept: nothing is created.
	other, _, _ := generateSSHKey("x")
	body, _ := json.Marshal(map[string]any{"name": "t/bad", "url": url, "ssh_key": other})
	if code := api("POST", "/api/repos", string(body), nil); code != 400 {
		t.Errorf("unauthorised pasted key: status %d, want 400", code)
	}
	if code := api("GET", "/api/repos/t/bad", "", nil); code != 404 {
		t.Errorf("failed mirror was kept: %d", code)
	}

	// A generated key is kept even though the first sync fails.
	var repo Repo
	body, _ = json.Marshal(map[string]any{"name": "t/mirror", "url": url, "generate_ssh_key": true})
	if code := api("POST", "/api/repos", string(body), &repo); code != 200 {
		t.Fatalf("create: %d %+v", code, repo)
	}
	if repo.SSHPublicKey == "" || repo.LastSyncError == "" || repo.SyncedAt != 0 {
		t.Fatalf("after create: %+v", repo)
	}
	if strings.Contains(repo.LastSyncError, "BEGIN OPENSSH") {
		t.Fatal("private key leaked")
	}

	// Authorise it: the first successful sync is a baseline (no runs) and
	// pins the host key.
	_ = os.WriteFile(sshd.authorizedKeys, []byte(repo.SSHPublicKey+"\n"), 0o600)
	var res map[string]any
	if code := api("POST", "/api/repos/t/mirror/sync", "", &res); code != 200 {
		t.Fatalf("sync: %d %v", code, res)
	}
	api("GET", "/api/repos/t/mirror", "", &repo)
	if repo.SyncedAt == 0 || len(repo.Refs) != 1 || len(repo.SSHHostKeys) != 1 {
		t.Fatalf("after first sync: %+v", repo)
	}
	if n := runCount(); n != 0 {
		t.Errorf("baseline sync started %d runs", n)
	}
	// The key is encrypted in the database and not left on disk.
	var stored string
	_ = srv.db.QueryRow(`SELECT ssh_key FROM repos WHERE id = ?`, repo.ID).Scan(&stored)
	if !strings.HasPrefix(stored, "v1:") || strings.Contains(stored, "OPENSSH") {
		t.Errorf("stored ssh key %q", stored)
	}
	if keys, _ := filepath.Glob(filepath.Join(srv.sshDir(repo.ID), "id*")); len(keys) != 0 {
		t.Errorf("key files after sync: %v", keys)
	}
	commit("two")
	api("POST", "/api/repos/t/mirror/sync", "", nil)
	if n := runCount(); n != 1 {
		t.Errorf("push after baseline: %d runs, want 1", n)
	}

	// Configured known_hosts are enforced.
	wrongHost := "[127.0.0.1]:1 " + strings.Join(strings.Fields(repo.SSHPublicKey)[:2], " ")
	_, port, _ := net.SplitHostPort(sshd.addr)
	wrongHost = strings.Replace(wrongHost, ":1 ", ":"+port+" ", 1)
	body, _ = json.Marshal(map[string]any{"ssh_known_hosts": wrongHost})
	if code := api("PATCH", "/api/repos/t/mirror", string(body), &repo); code != 200 {
		t.Fatalf("patch: %d", code)
	}
	if code := api("POST", "/api/repos/t/mirror/sync", "", nil); code == 200 {
		t.Error("sync succeeded with the wrong host key")
	}
	right := fmt.Sprintf("[127.0.0.1]:%s %s", port, strings.Join(strings.Fields(sshd.hostKeyPub)[:2], " "))
	body, _ = json.Marshal(map[string]any{"ssh_known_hosts": "# from ssh-keyscan\n" + right})
	api("PATCH", "/api/repos/t/mirror", string(body), &repo)
	if code := api("POST", "/api/repos/t/mirror/sync", "", &res); code != 200 {
		t.Errorf("sync with the right host key: %d %v", code, res)
	}

	// Removing the key makes the remote refuse us.
	repo = Repo{}
	api("PATCH", "/api/repos/t/mirror", `{"ssh_key":""}`, &repo)
	if repo.SSHPublicKey != "" {
		t.Errorf("key not removed: %+v", repo)
	}
	if code := api("POST", "/api/repos/t/mirror/sync", "", nil); code == 200 {
		t.Error("sync succeeded without a key")
	}
	// Hosted repositories have no SSH settings.
	api("POST", "/api/repos", `{"name":"t/hosted"}`, nil)
	if code := api("PATCH", "/api/repos/t/hosted", `{"generate_ssh_key":true}`, nil); code != 400 {
		t.Errorf("hosted ssh key: %d", code)
	}
}

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://user:pw@example.com/r.git":      "https://***@example.com/r.git",
		"https://ghp_token@github.com/o/r.git":   "https://***@github.com/o/r.git",
		"ssh://git@github.com/o/r.git":           "ssh://git@github.com/o/r.git",
		"ssh://git:pw@host/r.git":                "ssh://***@host/r.git",
		"git@github.com:o/r.git":                 "git@github.com:o/r.git",
		"fatal: https://a:b@h/x and ssh://u@h/y": "fatal: https://***@h/x and ssh://u@h/y",
	} {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
}
