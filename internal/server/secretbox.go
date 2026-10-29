package server

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Secret values and SSH deploy keys are stored encrypted with AES-256-GCM
// under the server's secret key: KIWICD_SECRET_KEY, or <data>/secret.key,
// which is generated on first start. Stored values look like
// "v1:" + base64(nonce || ciphertext); the version says which format and key
// sealed a value, so plaintext from older releases can be told apart and the
// key can be rotated later.

const sealedPrefix = "v1:"

// What a sealed value is for. Bound into the ciphertext, so a value can't be
// moved to another column and decrypted there.
const (
	purposeSecret   = "secrets.value"
	purposeSSHKey   = "repos.ssh_key"
	purposeKeyCheck = "meta.secret_key_check"

	purposeGitHubClientSecret  = "github_app.client_secret"
	purposeGitHubPrivateKey    = "github_app.private_key"
	purposeGitHubWebhookSecret = "github_app.webhook_secret"
)

// keyCheckName is the meta row holding a sealed known value, which tells a
// wrong key from a right one before anything is decrypted or encrypted.
const keyCheckName = "secret_key_check"

const keyCheckValue = "kiwicd"

type secretBox struct{ aead cipher.AEAD }

func newSecretBox(key []byte) (*secretBox, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("secret key must be 32 bytes, not %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &secretBox{aead}, nil
}

func (b *secretBox) seal(purpose, plaintext string) string {
	nonce := make([]byte, b.aead.NonceSize(), b.aead.NonceSize()+len(plaintext)+b.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	out := b.aead.Seal(nonce, nonce, []byte(plaintext), []byte(purpose))
	return sealedPrefix + base64.StdEncoding.EncodeToString(out)
}

var errNotSealed = errors.New("value is not encrypted")

func (b *secretBox) open(purpose, stored string) (string, error) {
	enc, ok := strings.CutPrefix(stored, sealedPrefix)
	if !ok {
		return "", errNotSealed
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(raw) < b.aead.NonceSize() {
		return "", errors.New("malformed encrypted value")
	}
	n := b.aead.NonceSize()
	out, err := b.aead.Open(nil, raw[:n], raw[n:], []byte(purpose))
	if err != nil {
		return "", errors.New("cannot decrypt value: wrong secret key or corrupted data")
	}
	return string(out), nil
}

// parseSecretKey accepts 32 bytes encoded as base64 (openssl rand -base64 32)
// or hex (openssl rand -hex 32).
func parseSecretKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if k, err := base64.StdEncoding.DecodeString(s); err == nil && len(k) == 32 {
		return k, nil
	}
	if k, err := hex.DecodeString(s); err == nil && len(k) == 32 {
		return k, nil
	}
	return nil, errors.New("must be 32 bytes as base64 or hex (make one with: openssl rand -base64 32)")
}

func secretKeyPath(dataDir string) string { return filepath.Join(dataDir, "secret.key") }

// setupSecretBox loads the secret key, checks it against the database and
// encrypts any values still stored in plain text.
func setupSecretBox(db *sql.DB, cfg Config, log *slog.Logger) (*secretBox, error) {
	var check string
	err := db.QueryRow(`SELECT value FROM meta WHERE name = ?`, keyCheckName).Scan(&check)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	path := secretKeyPath(cfg.DataDir)
	var key []byte
	var source string
	if cfg.SecretKey != "" {
		source = "KIWICD_SECRET_KEY"
		if key, err = parseSecretKey(cfg.SecretKey); err != nil {
			return nil, fmt.Errorf("KIWICD_SECRET_KEY %w", err)
		}
	} else {
		source = path
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			if key, err = parseSecretKey(string(b)); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
		case !errors.Is(err, os.ErrNotExist):
			return nil, err
		case check != "":
			// Don't make a new key: it couldn't decrypt what's stored.
			return nil, fmt.Errorf("the database's secrets are encrypted, but %s is missing and KIWICD_SECRET_KEY is not set: restore secret.key from your backup or set KIWICD_SECRET_KEY", path)
		default:
			key = make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				return nil, err
			}
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return nil, err
			}
			_, err = f.WriteString(base64.StdEncoding.EncodeToString(key) + "\n")
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return nil, err
			}
			log.Info("generated a secret key for encrypting secrets; back it up separately from the database", "path", path)
		}
	}
	box, err := newSecretBox(key)
	if err != nil {
		return nil, err
	}
	if check != "" {
		if v, err := box.open(purposeKeyCheck, check); err != nil || v != keyCheckValue {
			hint := ""
			if source == "KIWICD_SECRET_KEY" {
				if _, err := os.Stat(path); err == nil {
					hint = fmt.Sprintf(" (%s exists; was it encrypted with that key?)", path)
				}
			}
			return nil, fmt.Errorf("wrong secret key: %s is not the key this database's secrets were encrypted with%s", source, hint)
		}
	}
	n, err := encryptPlaintext(db, box, check == "")
	if err != nil {
		return nil, fmt.Errorf("encrypt stored secrets: %w", err)
	}
	if n > 0 {
		log.Info("encrypted stored secrets and SSH keys; vacuuming the database to drop plaintext copies", "count", n)
		// Old values (and ones older releases deleted) can linger in free
		// pages and the WAL until the file is rewritten.
		if _, err := db.Exec(`VACUUM`); err != nil {
			log.Warn("vacuum after encrypting secrets", "err", err)
		}
		if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			log.Warn("checkpoint after encrypting secrets", "err", err)
		}
	}
	return box, nil
}

// encryptPlaintext seals values written in plain text by older releases.
func encryptPlaintext(db *sql.DB, box *secretBox, writeCheck bool) (n int, err error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if writeCheck {
		if _, err := tx.Exec(`INSERT INTO meta (name, value) VALUES (?, ?)`, keyCheckName, box.seal(purposeKeyCheck, keyCheckValue)); err != nil {
			return 0, err
		}
	}
	for _, c := range []struct{ table, key, column, where, purpose string }{
		{"secrets", "rowid", "value", "1", purposeSecret},
		{"repos", "id", "ssh_key", "ssh_key != ''", purposeSSHKey},
	} {
		rows, err := tx.Query(`SELECT ` + c.key + `, ` + c.column + ` FROM ` + c.table + ` WHERE ` + c.where)
		if err != nil {
			return 0, err
		}
		plain := map[int64]string{}
		for rows.Next() {
			var id int64
			var v string
			if err := rows.Scan(&id, &v); err != nil {
				rows.Close()
				return 0, err
			}
			// The key is known to be right, so anything that doesn't open is
			// plain text (even if it happens to start with "v1:").
			if _, err := box.open(c.purpose, v); err != nil {
				plain[id] = v
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, err
		}
		for id, v := range plain {
			if _, err := tx.Exec(`UPDATE `+c.table+` SET `+c.column+` = ? WHERE `+c.key+` = ?`, box.seal(c.purpose, v), id); err != nil {
				return 0, err
			}
		}
		n += len(plain)
	}
	return n, tx.Commit()
}
