// Package sealed encrypts secret values to a public key, so that the kiwicd
// server stores secrets it can't read. Only the secrets service, which holds
// the private key, opens them, for the agents of jobs that get them.
//
// Values are sealed with HPKE (RFC 9180) in base mode: DHKEM(X25519,
// HKDF-SHA256), HKDF-SHA256 and AES-256-GCM, which browsers can do with
// WebCrypto (see web/src/seal.ts). A sealed value is
//
//	"pk1:" + key ID + ":" + base64(enc || ciphertext)
//
// where the key ID names the public key (so the service can hold several
// while keys are rotated) and the secret's scope, its repository,
// environment and name, is the additional authenticated data: a value can't
// be moved to another repository, environment or name and opened there.
package sealed

import (
	"crypto/ecdh"
	"crypto/hpke"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Prefix starts every sealed value.
const Prefix = "pk1:"

var info = []byte("kiwicd secrets v1")

func suite() (hpke.KDF, hpke.AEAD) { return hpke.HKDFSHA256(), hpke.AES256GCM() }

// Scope is what a secret belongs to. Global secrets have no repository;
// repository and global secrets have no environment.
type Scope struct {
	Repository  string `json:"repository,omitempty"`
	Environment string `json:"environment,omitempty"`
	Name        string `json:"name"`
}

func (s Scope) aad() []byte {
	return []byte("kiwicd secret\x00" + s.Repository + "\x00" + s.Environment + "\x00" + s.Name)
}

func (s Scope) String() string {
	out := s.Name
	if s.Environment != "" {
		out = s.Environment + "/" + out
	}
	if s.Repository != "" {
		out = s.Repository + ":" + out
	}
	return out
}

// PublicKey seals values.
type PublicKey struct {
	raw []byte
	pk  hpke.PublicKey
}

// ParsePublicKey reads a base64 X25519 public key.
func ParsePublicKey(s string) (*PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(raw) != 32 {
		return nil, errors.New("expected a base64 X25519 public key (32 bytes), as printed by kiwicd-secrets keygen")
	}
	pub, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		return nil, err
	}
	pk, err := hpke.NewDHKEMPublicKey(pub)
	if err != nil {
		return nil, err
	}
	return &PublicKey{raw: raw, pk: pk}, nil
}

// String returns the key in base64.
func (k *PublicKey) String() string { return base64.StdEncoding.EncodeToString(k.raw) }

// ID names the key: the first 8 bytes of its SHA-256, in hex.
func (k *PublicKey) ID() string {
	h := sha256.Sum256(k.raw)
	return hex.EncodeToString(h[:8])
}

// Seal encrypts value for scope.
func (k *PublicKey) Seal(scope Scope, value string) (string, error) {
	kdf, aead := suite()
	enc, s, err := hpke.NewSender(k.pk, kdf, aead, info)
	if err != nil {
		return "", err
	}
	ct, err := s.Seal(scope.aad(), []byte(value))
	if err != nil {
		return "", err
	}
	return Prefix + k.ID() + ":" + base64.StdEncoding.EncodeToString(append(enc, ct...)), nil
}

// PrivateKey opens values.
type PrivateKey struct {
	raw []byte
	sk  hpke.PrivateKey
	pub *PublicKey
}

// GenerateKey makes a new key pair.
func GenerateKey() (*PrivateKey, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	return newPrivateKey(raw)
}

// ParsePrivateKey reads a base64 X25519 private key.
func ParsePrivateKey(s string) (*PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(raw) != 32 {
		return nil, errors.New("expected a base64 X25519 private key (32 bytes), as written by kiwicd-secrets keygen")
	}
	return newPrivateKey(raw)
}

func newPrivateKey(raw []byte) (*PrivateKey, error) {
	priv, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return nil, err
	}
	sk, err := hpke.NewDHKEMPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	pubRaw := priv.PublicKey().Bytes()
	pk, err := hpke.NewDHKEMPublicKey(priv.PublicKey())
	if err != nil {
		return nil, err
	}
	return &PrivateKey{raw: raw, sk: sk, pub: &PublicKey{raw: pubRaw, pk: pk}}, nil
}

// String returns the key in base64.
func (k *PrivateKey) String() string { return base64.StdEncoding.EncodeToString(k.raw) }

func (k *PrivateKey) Public() *PublicKey { return k.pub }

// Is reports whether stored is a sealed value.
func Is(stored string) bool { return strings.HasPrefix(stored, Prefix) }

// KeyID returns the ID of the key a value was sealed to.
func KeyID(stored string) string {
	rest, ok := strings.CutPrefix(stored, Prefix)
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, ":")
	return id
}

// Check reports whether stored looks like a value sealed to k, without
// opening it.
func (k *PublicKey) Check(stored string) error {
	rest, ok := strings.CutPrefix(stored, Prefix)
	if !ok {
		return fmt.Errorf("a sealed value starts with %q", Prefix)
	}
	id, b64, _ := strings.Cut(rest, ":")
	if id != k.ID() {
		return fmt.Errorf("the value was sealed to key %s, not the current key %s", id, k.ID())
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return errors.New("the sealed value is not valid base64")
	}
	if len(raw) < 32+16 {
		return errors.New("the sealed value is too short")
	}
	return nil
}

// ErrUnknownKey means a value was sealed to a key that isn't loaded.
var ErrUnknownKey = errors.New("sealed to an unknown key")

// Open decrypts a value sealed for scope with one of keys.
func Open(keys []*PrivateKey, scope Scope, stored string) (string, error) {
	rest, ok := strings.CutPrefix(stored, Prefix)
	if !ok {
		return "", errors.New("not a sealed value")
	}
	id, b64, _ := strings.Cut(rest, ":")
	var key *PrivateKey
	for _, k := range keys {
		if k.pub.ID() == id {
			key = k
		}
	}
	if key == nil {
		return "", fmt.Errorf("%w %s", ErrUnknownKey, id)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) < 32 {
		return "", errors.New("malformed sealed value")
	}
	kdf, aead := suite()
	r, err := hpke.NewRecipient(raw[:32], key.sk, kdf, aead, info)
	if err != nil {
		return "", err
	}
	out, err := r.Open(scope.aad(), raw[32:])
	if err != nil {
		return "", errors.New("cannot open the value: it was sealed for another repository, environment or name, or is corrupted")
	}
	return string(out), nil
}
