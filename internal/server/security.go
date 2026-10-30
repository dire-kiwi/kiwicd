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
// nothing else: no inline scripts, no framing. Forms can also go to GitHub,
// which creates the GitHub App from a form the UI posts (see github.go).
func contentSecurityPolicy(githubURL string) string {
	return "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self' " + githubURL +
		"; frame-ancestors 'none'"
}

// secureHeaders sets browser hardening headers on every response and refuses
// cross-origin state-changing requests from browsers. A session cookie is
// SameSite=Lax, which other sites on the same registrable domain (apps on
// sibling subdomains) still get sent along with their requests; the origin check
// stops them creating mirrors, dispatching runs or changing secrets. Agents,
// git and webhooks send neither Origin nor Sec-Fetch-Site and are unaffected.
func (s *Server) secureHeaders(next http.Handler) http.Handler {
	cop := http.NewCrossOriginProtection()
	hsts := strings.HasPrefix(s.cfg.PublicURL, "https://")
	csp := contentSecurityPolicy(s.cfg.GitHubURL)
	return cop.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", csp)
		if hsts {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	}))
}

// ---------------------------------------------------------------- password guessing

// passwordLimiter caps wrong admin passwords per client: a few wrong, then
// one a minute, so the password can't be guessed by brute force over the
// API or git. When a client is over its limit its password isn't even
// checked. There is no limit across clients, which would let anyone lock
// the admin out by guessing wrong from a few addresses.
type passwordLimiter struct {
	mu      sync.Mutex
	now     func() time.Time
	clients map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

const (
	clientBurst  = 10
	clientRefill = time.Minute
)

func newPasswordLimiter() *passwordLimiter {
	return &passwordLimiter{now: time.Now, clients: map[string]*bucket{}}
}

// fill tops up b for the time since it was last used and reports whether it
// has a token left.
func (b *bucket) fill(now time.Time) bool {
	if b.at.IsZero() {
		b.tokens = clientBurst
	} else {
		b.tokens = min(clientBurst, b.tokens+float64(now.Sub(b.at))/float64(clientRefill))
	}
	b.at = now
	return b.tokens >= 1
}

// allow reports whether client may try a password now.
func (l *passwordLimiter) allow(client string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.clients[client]
	if c == nil {
		return true
	}
	return c.fill(l.now())
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
	c.fill(now)
	c.tokens = max(0, c.tokens-1)
	// Forget clients that have been quiet long enough to be back to full.
	if len(l.clients) > 10_000 {
		for k, b := range l.clients {
			if now.Sub(b.at) > clientBurst*clientRefill {
				delete(l.clients, k)
			}
		}
	}
}

// clientIP names r's client for rate limiting: the peer address or, with
// ClientIPHeader set (the server is behind a proxy that sets it), that
// header's last entry, which the proxy added. Headers aren't trusted
// otherwise, since any client can send them. IPv6 clients are named by their
// /64, which one host can have all of.
func (s *Server) clientIP(r *http.Request) string {
	addr := ""
	if h := s.cfg.ClientIPHeader; h != "" {
		if vals := r.Header.Values(h); len(vals) > 0 {
			last := vals[len(vals)-1]
			addr = strings.TrimSpace(last[strings.LastIndex(last, ",")+1:])
		}
	} else if r.Header.Get("X-Forwarded-For") != "" {
		s.proxyWarning.Do(func() {
			s.log.Warn("requests come through a proxy, so wrong passwords are limited per proxy address, not per client; set KIWICD_CLIENT_IP_HEADER to the header the proxy puts the client's address in")
		})
	}
	if addr == "" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		addr = host
	}
	ip := net.ParseIP(addr)
	if ip == nil {
		return addr
	}
	if ip.To4() == nil {
		return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
	return ip.String()
}

var errTooManyPasswords = errors.New("too many wrong passwords; try again in a minute")

// tryPassword checks an admin password from r's client, subject to the limit.
func (s *Server) tryPassword(r *http.Request, p string) (bool, error) {
	client := s.clientIP(r)
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
// revokes the sessions and API tokens made with it. Sessions and tokens of
// GitHub logins stay. The meta row holds the password's hash
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
		res, err := s.db.Exec(`DELETE FROM sessions WHERE github_login = ''`)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			s.log.Info("the admin password changed: logged out all sessions", "sessions", n)
		}
		if err := s.deleteTokens(false, "the admin password changed"); err != nil {
			return err
		}
	}
	_, err = s.db.Exec(`INSERT INTO meta (name, value) VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET value = excluded.value`,
		passwordCheckName, s.box.seal(purposePasswordCheck, want))
	return err
}
