package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------- headers

// contentSecurityPolicy allows the UI's own scripts, styles and API, and
// nothing else: no inline scripts, no framing.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// secureHeaders sets browser hardening headers on every response and refuses
// cross-origin state-changing requests from browsers. A session cookie is
// SameSite=Lax, which other sites on the same registrable domain (apps on
// sibling subdomains) still get sent along with their requests; the origin check
// stops them creating mirrors, dispatching runs or changing secrets. Agents,
// git and webhooks send neither Origin nor Sec-Fetch-Site and are unaffected.
func (s *Server) secureHeaders(next http.Handler) http.Handler {
	cop := http.NewCrossOriginProtection()
	hsts := strings.HasPrefix(s.cfg.PublicURL, "https://")
	return cop.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		if hsts {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	}))
}

// ---------------------------------------------------------------- password guessing

// passwordLimiter caps wrong admin passwords: each client may get a few
// wrong, then one a minute, and all clients together one every two seconds,
// so the password can't be guessed by brute force over the API or git. When
// a client is over its limit its password isn't even checked.
type passwordLimiter struct {
	mu      sync.Mutex
	now     func() time.Time
	clients map[string]*bucket
	global  bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

const (
	clientBurst  = 10
	clientRefill = time.Minute
	globalBurst  = 100
	globalRefill = 2 * time.Second
)

func newPasswordLimiter() *passwordLimiter {
	return &passwordLimiter{now: time.Now, clients: map[string]*bucket{}}
}

// fill tops up b for the time since it was last used and reports whether it
// has a token left.
func (b *bucket) fill(now time.Time, burst float64, refill time.Duration) bool {
	if b.at.IsZero() {
		b.tokens = burst
	} else {
		b.tokens = min(burst, b.tokens+float64(now.Sub(b.at))/float64(refill))
	}
	b.at = now
	return b.tokens >= 1
}

// allow reports whether client may try a password now.
func (l *passwordLimiter) allow(client string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	c := l.clients[client]
	if c == nil {
		c = &bucket{}
	}
	ok := c.fill(now, clientBurst, clientRefill) && l.global.fill(now, globalBurst, globalRefill)
	if c.tokens < clientBurst {
		l.clients[client] = c
	}
	return ok
}

// failed records a wrong password from client.
func (l *passwordLimiter) failed(client string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	c := l.clients[client]
	if c == nil {
		c = &bucket{}
		l.clients[client] = c
	}
	c.fill(now, clientBurst, clientRefill)
	c.tokens = max(0, c.tokens-1)
	l.global.fill(now, globalBurst, globalRefill)
	l.global.tokens = max(0, l.global.tokens-1)
	// Forget clients that have been quiet long enough to be back to full.
	if len(l.clients) > 10_000 {
		for k, b := range l.clients {
			if now.Sub(b.at) > clientBurst*clientRefill {
				delete(l.clients, k)
			}
		}
	}
}

// clientIP names the client for rate limiting. Behind a reverse proxy the
// peer address is always the proxy's, so the client's address is taken from
// CF-Connecting-IP (Cloudflare) or X-Forwarded-For. A client that reaches the
// server directly can forge those, but then the global limit still applies.
func clientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); ip != "" {
		return ip
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		return strings.TrimSpace(first)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

var errTooManyPasswords = errors.New("too many wrong passwords; try again in a minute")

// tryPassword checks an admin password from r's client, subject to the limit.
func (s *Server) tryPassword(r *http.Request, p string) (bool, error) {
	client := clientIP(r)
	if !s.limiter.allow(client) {
		return false, errTooManyPasswords
	}
	if s.checkPassword(p) {
		return true, nil
	}
	s.limiter.failed(client)
	s.log.Warn("wrong admin password", "client", client, "path", r.URL.Path)
	return false, nil
}

// ---------------------------------------------------------------- sessions

const passwordCheckName = "admin_password_check"

const purposePasswordCheck = "meta.admin_password_check"

// expireSessionsOnPasswordChange logs everyone out when the admin password
// has changed since the last start, so rotating a leaked password also
// revokes the sessions made with it. The meta row holds the password's hash
// sealed with the secret key, which can't be checked without that key.
func (s *Server) expireSessionsOnPasswordChange() error {
	sum := sha256.Sum256([]byte(s.cfg.AdminPassword))
	want := hex.EncodeToString(sum[:])
	var stored string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE name = ?`, passwordCheckName).Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	default:
		if got, err := s.box.open(purposePasswordCheck, stored); err == nil && got == want {
			return nil
		}
		res, err := s.db.Exec(`DELETE FROM sessions`)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			s.log.Info("the admin password changed: logged out all sessions", "sessions", n)
		}
	}
	_, err = s.db.Exec(`INSERT INTO meta (name, value) VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET value = excluded.value`,
		passwordCheckName, s.box.seal(purposePasswordCheck, want))
	return err
}
