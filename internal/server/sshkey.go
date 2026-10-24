package server

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Mirrors fetch over SSH with their own known_hosts file and, optionally,
// their own private key (a deploy key). Without configured known_hosts the
// host key is trusted on first connection and pinned from then on.

// parseSSHKey checks an unencrypted private key and returns it normalised
// (OpenSSH needs the trailing newline) with its authorized_keys line.
func parseSSHKey(key, comment string) (normalized, public string, err error) {
	key = strings.TrimSpace(strings.ReplaceAll(key, "\r\n", "\n")) + "\n"
	signer, err := ssh.ParsePrivateKey([]byte(key))
	if err != nil {
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return "", "", errors.New("the SSH key is protected by a passphrase; use a key without one")
		}
		return "", "", fmt.Errorf("invalid SSH private key: %v", err)
	}
	return key, authorizedKey(signer.PublicKey(), comment), nil
}

// generateSSHKey makes an ed25519 key pair.
func generateSSHKey(comment string) (private, public string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return "", "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(block)), authorizedKey(sshPub, comment), nil
}

func authorizedKey(k ssh.PublicKey, comment string) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))) + " " + comment
}

// checkKnownHosts validates known_hosts content (such as ssh-keyscan output).
func checkKnownHosts(content string) error {
	rest := []byte(content)
	n := 0
	for len(bytes.TrimSpace(rest)) > 0 {
		_, _, _, _, next, err := ssh.ParseKnownHosts(rest)
		if errors.Is(err, io.EOF) { // only comments left
			break
		}
		if err != nil {
			return fmt.Errorf("invalid known_hosts: %v", err)
		}
		rest = next
		n++
	}
	if n == 0 {
		return errors.New("known_hosts has no host keys")
	}
	return nil
}

// hostKeys lists known_hosts entries as "host type SHA256:fingerprint".
func hostKeys(content []byte) []string {
	out := []string{}
	for len(bytes.TrimSpace(content)) > 0 {
		_, hosts, key, _, next, err := ssh.ParseKnownHosts(content)
		if err != nil {
			break
		}
		out = append(out, strings.Join(hosts, ",")+" "+key.Type()+" "+ssh.FingerprintSHA256(key))
		content = next
	}
	return out
}

func (s *Server) sshDir(repoID int64) string {
	return filepath.Join(s.cfg.DataDir, "ssh", strconv.FormatInt(repoID, 10))
}

func (s *Server) knownHostsPath(repoID int64) string {
	return filepath.Join(s.sshDir(repoID), "known_hosts")
}

// gitSSHEnv writes the mirror's known_hosts and decrypted key and returns the
// GIT_SSH_COMMAND that uses them. Call cleanup when git is done: the key is
// only on disk while it's in use.
func (s *Server) gitSSHEnv(repo *Repo) (env []string, cleanup func(), err error) {
	cleanup = func() {}
	dir := s.sshDir(repo.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, cleanup, err
	}
	knownHosts := s.knownHostsPath(repo.ID)
	checking := "accept-new"
	if repo.SSHKnownHosts != "" {
		checking = "yes"
		if err := os.WriteFile(knownHosts, []byte(repo.SSHKnownHosts), 0o600); err != nil {
			return nil, cleanup, err
		}
	}
	cmd := []string{"ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=" + checking,
		"-o", "UserKnownHostsFile=" + shellQuote(knownHosts), "-o", "HashKnownHosts=no"}
	if repo.SSHKey != "" {
		key, err := s.box.open(purposeSSHKey, repo.SSHKey)
		if err != nil {
			return nil, cleanup, fmt.Errorf("SSH key: %w", err)
		}
		f, err := os.CreateTemp(dir, "id-*") // mode 0600
		if err != nil {
			return nil, cleanup, err
		}
		cleanup = func() { _ = os.Remove(f.Name()) }
		_, err = f.WriteString(key)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			cleanup()
			return nil, func() {}, err
		}
		cmd = append(cmd, "-i", shellQuote(f.Name()), "-o", "IdentitiesOnly=yes")
	}
	return []string{"GIT_SSH_COMMAND=" + strings.Join(cmd, " ")}, cleanup, nil
}

// removeSSHKeyFiles deletes deploy keys left on disk: older releases kept
// them there, and a crash can leave a temporary one behind.
func removeSSHKeyFiles(dataDir string) {
	files, _ := filepath.Glob(filepath.Join(dataDir, "ssh", "*", "id*"))
	for _, f := range files {
		_ = os.Remove(f)
	}
}

// shellQuote quotes s for sh, which git runs GIT_SSH_COMMAND with.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
