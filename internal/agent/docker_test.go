package agent

import "testing"

func TestJobNames(t *testing.T) {
	if got := jobPrefix(3, 1234); got != "kiwicd-r3-j1234-" {
		t.Errorf("jobPrefix = %q", got)
	}
	// A runner's prefix covers its jobs' and no other runner's.
	if p := runnerPrefix(3); jobPrefix(3, 1)[:len(p)] != p || jobPrefix(31, 1)[:len(p)] == p {
		t.Errorf("runnerPrefix(3) = %q", p)
	}
	// Job 1234's objects aren't job 12's.
	if hasPrefixedName([]string{"/kiwicd-r3-j1234-CI-build-abc"}, jobPrefix(3, 12)) {
		t.Error("job 12's prefix matched job 1234's container")
	}
	if !hasPrefixedName([]string{"/other", "/kiwicd-r3-j12-CI-build-abc"}, jobPrefix(3, 12)) {
		t.Error("job 12's prefix didn't match its container")
	}
	if name, err := bridgeName("kiwicd-", 1234); err != nil || name != "kiwicd-ya" {
		t.Errorf("bridgeName = %q, %v", name, err)
	}
	// 8 base-36 digits (jobs below 36^8) fit after a 7-character prefix.
	if name, err := bridgeName("kiwicd-", 2821109907455); err != nil || len(name) != 15 {
		t.Errorf("bridgeName = %q, %v", name, err)
	}
	if _, err := bridgeName("kiwicd-", 2821109907456); err == nil {
		t.Error("bridgeName accepted a name longer than 15 characters")
	}
}

func TestBridgePrefixLength(t *testing.T) {
	creds := &Credentials{URL: "http://x", Token: "t"}
	if _, err := New(Config{Creds: creds, WorkDir: t.TempDir(), JobBridgePrefix: "kiwicd-x"}); err == nil {
		t.Error("New accepted an 8-character bridge prefix")
	}
	if _, err := New(Config{Creds: creds, WorkDir: t.TempDir(), JobBridgePrefix: "kiwicd-"}); err != nil {
		t.Error(err)
	}
}
