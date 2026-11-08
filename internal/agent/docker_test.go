package agent

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestJobNames(t *testing.T) {
	if got := jobPrefix("1a2b3c4d", 1234); got != "kiwicd-1a2b3c4d-j1234-" {
		t.Errorf("jobPrefix = %q", got)
	}
	// An agent's prefix covers its jobs' and no other agent's.
	if p := agentPrefix("1a2b3c4d"); !strings.HasPrefix(jobPrefix("1a2b3c4d", 1), p) || strings.HasPrefix(jobPrefix("1a2b3c4e", 1), p) {
		t.Errorf("agentPrefix = %q", p)
	}
	// Agents named their jobs by runner ID before; neither name covers the other.
	if p := oldAgentPrefix(3); p != "kiwicd-r3-j" || strings.HasPrefix(jobPrefix("1a2b3c4d", 1), p) {
		t.Errorf("oldAgentPrefix = %q", p)
	}
	// Job 1234's objects aren't job 12's.
	if hasPrefixedName([]string{"/kiwicd-1a2b3c4d-j1234-CI-build-abc"}, jobPrefix("1a2b3c4d", 12)) {
		t.Error("job 12's prefix matched job 1234's container")
	}
	if !hasPrefixedName([]string{"/other", "/kiwicd-1a2b3c4d-j12-CI-build-abc"}, jobPrefix("1a2b3c4d", 12)) {
		t.Error("job 12's prefix didn't match its container")
	}
}

func TestBridgeName(t *testing.T) {
	a, b := bridgeName("kiwicd-", "kiwicd-1a2b3c4d-j1-net"), bridgeName("kiwicd-", "kiwicd-1a2b3c4e-j1-net")
	if !strings.HasPrefix(a, "kiwicd-") || len(a) != 15 || len(b) != 15 {
		t.Errorf("bridgeName = %q, %q", a, b)
	}
	// The same job ID of two agents gets two interfaces.
	if a == b {
		t.Errorf("two agents' job 1 share the bridge %q", a)
	}
	if again := bridgeName("kiwicd-", "kiwicd-1a2b3c4d-j1-net"); again != a {
		t.Errorf("bridgeName changed: %q, %q", a, again)
	}
	if got := bridgeName("k-", "kiwicd-1a2b3c4d-j1-net"); len(got) != 10 {
		t.Errorf("bridgeName = %q", got)
	}
}

func TestBridgePrefixLength(t *testing.T) {
	creds := &Credentials{URL: "http://x", Token: "t", AgentID: "1a2b3c4d"}
	if _, err := New(Config{Creds: creds, WorkDir: t.TempDir(), JobBridgePrefix: "kiwicd-x"}); err == nil {
		t.Error("New accepted an 8-character bridge prefix")
	}
	if _, err := New(Config{Creds: creds, WorkDir: t.TempDir(), JobBridgePrefix: "kiwicd-"}); err != nil {
		t.Error(err)
	}
}

func TestAgentID(t *testing.T) {
	if a, b := newAgentID(), newAgentID(); !agentIDPattern.MatchString(a) || a == b {
		t.Errorf("newAgentID = %q, %q", a, b)
	}
	if _, err := New(Config{Creds: &Credentials{URL: "http://x", Token: "t"}, WorkDir: t.TempDir()}); err == nil {
		t.Error("New accepted a registration without an agent ID")
	}

	// A registration from before agent IDs gets one, from its token, and keeps it.
	path := filepath.Join(t.TempDir(), ".runner")
	old := &Credentials{URL: "http://x", RunnerID: 1, Token: "t"}
	if err := old.Save(path); err != nil {
		t.Fatal(err)
	}
	c, _ := LoadCredentials(path)
	if added, err := c.AddAgentID(path); !added || err != nil || !agentIDPattern.MatchString(c.AgentID) {
		t.Fatalf("AddAgentID = %v, %v; ID %q", added, err, c.AgentID)
	}
	saved, _ := LoadCredentials(path)
	if saved.AgentID != c.AgentID {
		t.Errorf("saved agent ID %q, want %q", saved.AgentID, c.AgentID)
	}
	if added, err := saved.AddAgentID(path); added || err != nil || saved.AgentID != c.AgentID {
		t.Errorf("second AddAgentID = %v, %v; ID %q", added, err, saved.AgentID)
	}
	// If it can't be saved, the next start derives the same ID.
	unsaved := &Credentials{URL: "http://x", RunnerID: 1, Token: "t"}
	if added, err := unsaved.AddAgentID(filepath.Join(path, "not-a-dir", ".runner")); !added || err == nil || unsaved.AgentID != c.AgentID {
		t.Errorf("unsaved AddAgentID = %v, %v; ID %q, want %q", added, err, unsaved.AgentID, c.AgentID)
	}
	// Two registrations' IDs differ.
	other := &Credentials{Token: "u"}
	_, _ = other.AddAgentID(filepath.Join(t.TempDir(), ".runner"))
	if other.AgentID == c.AgentID {
		t.Errorf("tokens t and u got the same agent ID %q", c.AgentID)
	}
}
