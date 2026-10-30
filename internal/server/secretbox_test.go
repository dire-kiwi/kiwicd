package server

import (
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretBox(t *testing.T) {
	key := make([]byte, 32)
	key[0] = 1
	box, err := newSecretBox(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"", "hunter2", "multi\nline\x00binary", strings.Repeat("x", 10000)} {
		sealed := box.seal(purposeSecret, v)
		if !strings.HasPrefix(sealed, "v1:") || (v != "" && strings.Contains(sealed, v)) {
			t.Errorf("sealed %q as %q", v, sealed)
		}
		if got, err := box.open(purposeSecret, sealed); err != nil || got != v {
			t.Errorf("round trip %q: %q, %v", v, got, err)
		}
	}
	a, b := box.seal(purposeSecret, "same"), box.seal(purposeSecret, "same")
	if a == b {
		t.Error("sealing is deterministic")
	}
	if _, err := box.open(purposeSSHKey, a); err == nil {
		t.Error("opened a secret as an SSH key")
	}
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(a, "v1:"))
	raw[len(raw)-1] ^= 1
	if _, err := box.open(purposeSecret, "v1:"+base64.StdEncoding.EncodeToString(raw)); err == nil {
		t.Error("opened a tampered value")
	}
	if _, err := box.open(purposeSecret, "plain"); err != errNotSealed {
		t.Errorf("plain text: %v", err)
	}
	if _, err := box.open(purposeSecret, "v1:%%%"); err == nil {
		t.Error("opened garbage")
	}

	key[0] = 2
	other, _ := newSecretBox(key)
	if _, err := other.open(purposeSecret, a); err == nil || !strings.Contains(err.Error(), "wrong secret key") {
		t.Errorf("wrong key: %v", err)
	}

	for in, ok := range map[string]bool{
		base64.StdEncoding.EncodeToString(key) + "\n": true,
		hex.EncodeToString(key):                       true,
		"short":                                       false,
		base64.StdEncoding.EncodeToString(key[:16]):   false,
	} {
		if _, err := parseSecretKey(in); (err == nil) != ok {
			t.Errorf("parseSecretKey(%q): %v", in, err)
		}
	}
}

// An upgrade from a release that stored secrets and SSH keys in plain text.
func TestSecretMigration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	data := filepath.Join(t.TempDir(), "data")
	_ = os.MkdirAll(data, 0o755)
	db, err := openDB(filepath.Join(data, "kiwicd.db"))
	if err != nil {
		t.Fatal(err)
	}
	sshKey, _, _ := generateSSHKey("kiwicd t/m")
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`INSERT INTO repos (id, name, kind, url, webhook_secret, created_at, ssh_key) VALUES (7, 't/m', 'mirror', 'ssh://h/r', 'w', 0, ?)`, sshKey)
	mustExec(`INSERT INTO repos (id, name, kind, webhook_secret, created_at) VALUES (8, 't/h', 'hosted', 'w', 0)`)
	plain := map[string]string{"GLOBAL": "g-value", "LOOKS_SEALED": "v1:not really", "EMPTY": ""}
	for k, v := range plain {
		mustExec(`INSERT INTO secrets (repo_id, name, value, updated_at) VALUES (0, ?, ?, 0)`, k, v)
	}
	mustExec(`INSERT INTO secrets (repo_id, name, value, updated_at) VALUES (7, 'GLOBAL', 'repo-value', 0)`)
	mustExec(`INSERT INTO variables (repo_id, name, value, updated_at) VALUES (0, 'VAR', 'visible', 0)`)
	// Deleted values linger in free pages.
	mustExec(`INSERT INTO secrets (repo_id, name, value, updated_at) VALUES (0, 'OLD', 'deleted-plaintext', 0)`)
	mustExec(`DELETE FROM secrets WHERE name = 'OLD'`)
	db.Close()
	// Older releases left deploy keys on disk.
	legacyKey := filepath.Join(data, "ssh", "7", "id")
	_ = os.MkdirAll(filepath.Dir(legacyKey), 0o700)
	_ = os.WriteFile(legacyKey, []byte(sshKey), 0o600)

	start := func(secretKey string) (*Server, error) {
		t.Helper()
		srv, err := New(Config{DataDir: data, AdminPassword: "pw", SecretKey: secretKey, Version: "test"})
		if err == nil {
			t.Cleanup(func() { srv.db.Close() })
		}
		return srv, err
	}
	check := func(srv *Server) {
		t.Helper()
		rows, err := srv.db.Query(`SELECT value FROM secrets UNION ALL SELECT ssh_key FROM repos WHERE ssh_key != ''`)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for rows.Next() {
			var v string
			_ = rows.Scan(&v)
			if !strings.HasPrefix(v, "v1:") || strings.Contains(v, "value") || strings.Contains(v, "OPENSSH") {
				t.Errorf("stored in plain text: %q", v)
			}
			n++
		}
		rows.Close()
		if n != 5 {
			t.Errorf("%d stored values, want 5", n)
		}
		got, err := srv.plainSecrets(7)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"GLOBAL": "repo-value", "LOOKS_SEALED": "v1:not really", "EMPTY": ""}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("secret %s = %q, want %q", k, got[k], v)
			}
		}
		if got, _ := srv.plainSecrets(8); got["GLOBAL"] != "g-value" {
			t.Errorf("global secret = %q", got["GLOBAL"])
		}
		if v := srv.varsFor(0, nil)["VAR"]; v != "visible" {
			t.Errorf("variable changed: %q", v)
		}
		repo, _ := srv.getRepoByID(7)
		if k, err := srv.box.open(purposeSSHKey, repo.SSHKey); err != nil || k != sshKey {
			t.Errorf("ssh key: %v", err)
		}
		if h, _ := srv.getRepoByID(8); h.SSHKey != "" {
			t.Errorf("hosted repo got an ssh key: %q", h.SSHKey)
		}
	}

	// First start: a key file is made and everything is encrypted.
	srv, err := start("")
	if err != nil {
		t.Fatal(err)
	}
	check(srv)
	files, _ := filepath.Glob(filepath.Join(data, "kiwicd.db*"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for _, v := range []string{"deleted-plaintext", "g-value", "repo-value", "BEGIN OPENSSH"} {
			if strings.Contains(string(b), v) {
				t.Errorf("%s still has %q", filepath.Base(f), v)
			}
		}
	}
	keyFile := filepath.Join(data, "secret.key")
	st, err := os.Stat(keyFile)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("secret.key: %v %v", st, err)
	}
	if _, err := os.Stat(legacyKey); !os.IsNotExist(err) {
		t.Errorf("legacy key file kept: %v", err)
	}
	// The key is only on disk during a fetch.
	env, cleanup, err := srv.gitSSHEnv(mustRepo(t, srv, 7))
	if err != nil || !strings.Contains(env[0], " -i ") {
		t.Fatalf("gitSSHEnv: %v %v", env, err)
	}
	keys, _ := filepath.Glob(filepath.Join(srv.sshDir(7), "id*"))
	if len(keys) != 1 {
		t.Fatalf("key files during fetch: %v", keys)
	}
	if b, _ := os.ReadFile(keys[0]); string(b) != sshKey {
		t.Error("key file isn't the decrypted key")
	}
	cleanup()
	if keys, _ := filepath.Glob(filepath.Join(srv.sshDir(7), "id*")); len(keys) != 0 {
		t.Errorf("key files after fetch: %v", keys)
	}
	srv.db.Close()

	// Restarting doesn't encrypt anything twice.
	srv, err = start("")
	if err != nil {
		t.Fatal(err)
	}
	check(srv)
	srv.db.Close()

	// The same key from KIWICD_SECRET_KEY, as base64 or hex.
	b, _ := os.ReadFile(keyFile)
	key, _ := parseSecretKey(string(b))
	for _, k := range []string{strings.TrimSpace(string(b)), hex.EncodeToString(key)} {
		srv, err = start(k)
		if err != nil {
			t.Fatalf("KIWICD_SECRET_KEY %s: %v", k, err)
		}
		check(srv)
		srv.db.Close()
	}

	// A different key is refused before anything is touched.
	wrong := make([]byte, 32)
	if _, err := start(base64.StdEncoding.EncodeToString(wrong)); err == nil || !strings.Contains(err.Error(), "wrong secret key: KIWICD_SECRET_KEY") {
		t.Errorf("wrong key: %v", err)
	}
	if _, err := start("not a key"); err == nil || !strings.Contains(err.Error(), "KIWICD_SECRET_KEY must be 32 bytes") {
		t.Errorf("bad key: %v", err)
	}
	_ = os.WriteFile(keyFile, []byte(base64.StdEncoding.EncodeToString(wrong)), 0o600)
	if _, err := start(""); err == nil || !strings.Contains(err.Error(), "wrong secret key: "+keyFile) {
		t.Errorf("wrong key file: %v", err)
	}
	// A lost key file isn't silently replaced.
	_ = os.Remove(keyFile)
	if _, err := start(""); err == nil || !strings.Contains(err.Error(), "secret.key is missing") {
		t.Errorf("missing key file: %v", err)
	}
	if _, err := os.Stat(keyFile); !os.IsNotExist(err) {
		t.Error("made a new key for a database with encrypted secrets")
	}
	srv, err = start(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	check(srv)
}

func mustRepo(t *testing.T, srv *Server, id int64) *Repo {
	t.Helper()
	repo, err := srv.getRepoByID(id)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestSecretsAPI(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	srv, err := New(Config{DataDir: filepath.Join(t.TempDir(), "data"), AdminPassword: "pw", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	api := func(method, path, body string) string {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.SetBasicAuth("admin", "pw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
		}
		return string(b)
	}
	api("PUT", "/api/secrets", `{"name":"TOKEN","value":"s3cret"}`)
	api("PUT", "/api/variables", `{"name":"NAME","value":"plain"}`)
	if out := api("GET", "/api/secrets", ""); strings.Contains(out, "s3cret") || strings.Contains(out, "v1:") || !strings.Contains(out, "TOKEN") {
		t.Errorf("secrets list: %s", out)
	}
	var stored string
	_ = srv.db.QueryRow(`SELECT value FROM secrets WHERE name = 'TOKEN'`).Scan(&stored)
	if !strings.HasPrefix(stored, "v1:") || strings.Contains(stored, "s3cret") {
		t.Errorf("stored secret %q", stored)
	}
	_ = srv.db.QueryRow(`SELECT value FROM variables WHERE name = 'NAME'`).Scan(&stored)
	if stored != "plain" {
		t.Errorf("stored variable %q", stored)
	}
	if got, err := srv.plainSecrets(0); err != nil || got["TOKEN"] != "s3cret" {
		t.Errorf("secretsFor: %v %v", got, err)
	}
}

// plainSecrets returns the secrets the server can read of a repository's jobs.
func (s *Server) plainSecrets(repoID int64) (map[string]string, error) {
	plain, _, err := s.jobSecrets(repoID, nil)
	return plain, err
}
