package sealed

import (
	"errors"
	"strings"
	"testing"
)

func TestSealOpen(t *testing.T) {
	k, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ParsePublicKey(k.Public().String())
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Repository: "o/app", Environment: "production", Name: "TOKEN"}
	v, err := pub.Seal(scope, "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !Is(v) || KeyID(v) != pub.ID() || pub.Check(v) != nil {
		t.Fatalf("sealed value %q", v)
	}
	k2, _ := ParsePrivateKey(k.String())
	got, err := Open([]*PrivateKey{k2}, scope, v)
	if err != nil || got != "hunter2" {
		t.Fatalf("Open = %q, %v", got, err)
	}
	for _, other := range []Scope{
		{Repository: "o/other", Environment: "production", Name: "TOKEN"},
		{Repository: "o/app", Name: "TOKEN"},
		{Repository: "o/app", Environment: "production", Name: "OTHER"},
		{Environment: "production", Name: "TOKEN"},
	} {
		if _, err := Open([]*PrivateKey{k}, other, v); err == nil {
			t.Errorf("opened a value sealed for %v as %v", scope, other)
		}
	}
	other, _ := GenerateKey()
	if _, err := Open([]*PrivateKey{other}, scope, v); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("Open with another key: %v", err)
	}
	if err := other.Public().Check(v); err == nil || !strings.Contains(err.Error(), "current key") {
		t.Errorf("Check with another key: %v", err)
	}
}

// A value sealed by web/src/seal.ts (with node), to keep the two in step.
func TestOpenBrowserSealed(t *testing.T) {
	k, err := ParsePrivateKey(browserKey)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open([]*PrivateKey{k}, Scope{Repository: "o/app", Environment: "production", Name: "TOKEN"}, browserSealed)
	if err != nil || got != "hunter2 ✓" {
		t.Fatalf("Open = %q, %v", got, err)
	}
	got, err = Open([]*PrivateKey{k}, Scope{Name: "GLOBAL"}, browserSealedGlobal)
	if err != nil || got != "" {
		t.Fatalf("Open = %q, %v", got, err)
	}
}

// Made with: node -e 'import("./web/src/seal.ts").then(m => m.seal(PUBLIC, SCOPE, VALUE))'.
const (
	browserKey          = "0ofj6Gx5VFfiY/OvtiooJbe7V3YLT4qYihQn9bKDxpc="
	browserSealed       = "pk1:107e753be553fb63:E2zSjl23yR/DQEbansaaTB5wNgEUPlZDFCpk7+5Fd1i3odYFN/kWdyGYlatq8g5QiTfDDab85b/+IjQ="
	browserSealedGlobal = "pk1:107e753be553fb63:BCYwzbElrhWaGRA2aLOnH3zhoYJwkHfmxU8d/THZPDYUYWmr3bH3R2B4vWTY1UN2"
)
