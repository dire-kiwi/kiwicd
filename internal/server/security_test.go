package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCrossOriginRequestsRefused(t *testing.T) {
	srv, err := New(Config{DataDir: filepath.Join(t.TempDir(), "data"), AdminPassword: "pw", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	do := func(method, path string, headers map[string]string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(`{"name":"t/r"}`))
		req.SetBasicAuth("admin", "pw")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	// A page on another site (or a sibling subdomain) posting to the API.
	for _, h := range []map[string]string{
		{"Sec-Fetch-Site": "same-site", "Origin": "https://evil.example.com"},
		{"Sec-Fetch-Site": "cross-site"},
		{"Origin": "https://evil.example"},
	} {
		if resp := do("POST", "/api/repos", h); resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST with %v: %d, want 403", h, resp.StatusCode)
		}
	}
	// The UI itself, and clients that aren't browsers.
	if resp := do("POST", "/api/repos", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": ts.URL}); resp.StatusCode != 200 {
		t.Errorf("same-origin POST: %d", resp.StatusCode)
	}
	if resp := do("DELETE", "/api/repos/t/r", nil); resp.StatusCode != 200 {
		t.Errorf("DELETE without browser headers: %d", resp.StatusCode)
	}
	// Reads are allowed from anywhere (they need the session anyway), and
	// every response carries the hardening headers.
	resp := do("GET", "/api/repos", map[string]string{"Sec-Fetch-Site": "cross-site"})
	if resp.StatusCode != 200 {
		t.Errorf("cross-site GET: %d", resp.StatusCode)
	}
	for _, h := range []string{"Content-Security-Policy", "X-Frame-Options", "X-Content-Type-Options"} {
		if resp.Header.Get(h) == "" {
			t.Errorf("missing %s", h)
		}
	}
}

func TestPasswordLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newPasswordLimiter()
	l.now = func() time.Time { return now }
	for i := 0; i < clientBurst; i++ {
		if !l.allow("a") {
			t.Fatalf("attempt %d refused", i)
		}
		l.failed("a")
	}
	if l.allow("a") {
		t.Fatal("allowed after the burst")
	}
	if !l.allow("b") {
		t.Fatal("another client was refused")
	}
	now = now.Add(clientRefill)
	if !l.allow("a") {
		t.Fatal("refused after the refill interval")
	}
	l.failed("a")
	if l.allow("a") {
		t.Fatal("allowed twice in one refill interval")
	}

	// However many clients guess wrong, one that hasn't can still try.
	for i := 0; i < 1000; i++ {
		l.failed(fmt.Sprint("bot", i))
	}
	if !l.allow("admin") {
		t.Fatal("other clients' wrong passwords locked out a new client")
	}
}

func TestPasswordGuessingBlocked(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		// Headers of the attacker's guesses (i numbers them) and the
		// admin's request, and their peer addresses.
		guess     func(i int) map[string]string
		guessPeer string
		admin     map[string]string
		adminPeer string
	}{{
		// Forwarded headers are ignored by default, so rotating them
		// doesn't give a guesser fresh tries.
		name: "direct",
		guess: func(i int) map[string]string {
			return map[string]string{"X-Forwarded-For": fmt.Sprintf("10.0.%d.1", i), "CF-Connecting-IP": fmt.Sprintf("10.1.%d.1", i)}
		},
		guessPeer: "203.0.113.9:4000",
		adminPeer: "198.51.100.7:5000",
	}, {
		// Behind a proxy, the client is the entry the proxy appended.
		name:   "proxy",
		header: "X-Forwarded-For",
		guess: func(i int) map[string]string {
			return map[string]string{"X-Forwarded-For": fmt.Sprintf("10.0.%d.1, 203.0.113.9", i)}
		},
		guessPeer: "192.0.2.1:4000",
		admin:     map[string]string{"X-Forwarded-For": "198.51.100.7"},
		adminPeer: "192.0.2.1:5000",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			srv, err := New(Config{DataDir: filepath.Join(t.TempDir(), "data"), AdminPassword: "pw", Version: "test", ClientIPHeader: tc.header})
			if err != nil {
				t.Fatal(err)
			}
			h := srv.Handler()
			get := func(path, password, peer string, headers map[string]string) int {
				t.Helper()
				req := httptest.NewRequest("GET", path, nil)
				req.RemoteAddr = peer
				req.SetBasicAuth("admin", password)
				for k, v := range headers {
					req.Header.Set(k, v)
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				return rec.Code
			}
			for i := 0; i < clientBurst; i++ {
				if code := get("/api/me", "guess", tc.guessPeer, tc.guess(i)); code != 401 {
					t.Fatalf("guess %d: %d", i, code)
				}
			}
			// The guesser is refused even the right password, over the API
			// and git, whatever it puts in the headers.
			if code := get("/api/me", "pw", tc.guessPeer, tc.guess(clientBurst)); code != http.StatusTooManyRequests {
				t.Errorf("API after guesses: %d, want 429", code)
			}
			if code := get("/git/t/r.git/info/refs?service=git-upload-pack", "pw", tc.guessPeer, tc.guess(clientBurst+1)); code != http.StatusTooManyRequests {
				t.Errorf("git after guesses: %d, want 429", code)
			}
			// Another client still gets in.
			if code := get("/api/me", "pw", tc.adminPeer, tc.admin); code != 200 {
				t.Errorf("another client's right password: %d, want 200", code)
			}
		})
	}
}

func TestClientIPv6Prefix(t *testing.T) {
	srv := &Server{}
	a := httptest.NewRequest("GET", "/", nil)
	a.RemoteAddr = "[2001:db8:1:2::1]:4000"
	b := httptest.NewRequest("GET", "/", nil)
	b.RemoteAddr = "[2001:db8:1:2:ffff::9]:4000"
	if srv.clientIP(a) != srv.clientIP(b) || srv.clientIP(a) != "2001:db8:1:2::/64" {
		t.Errorf("one /64 is two clients: %s, %s", srv.clientIP(a), srv.clientIP(b))
	}
}

func TestPasswordChangeEndsSessions(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	start := func(password string) *httptest.Server {
		t.Helper()
		srv, err := New(Config{DataDir: data, AdminPassword: password, Version: "test"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { srv.db.Close() })
		return httptest.NewServer(srv.Handler())
	}
	me := func(ts *httptest.Server, cookie *http.Cookie) int {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+"/api/me", nil)
		req.AddCookie(cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	ts := start("old")
	resp, err := http.Post(ts.URL+"/api/login", "application/json", strings.NewReader(`{"password":"old"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatalf("login: %d, no session cookie", resp.StatusCode)
	}
	ts.Close()

	// A restart with the same password keeps the session.
	ts = start("old")
	if code := me(ts, cookie); code != 200 {
		t.Fatalf("after restart: %d", code)
	}
	ts.Close()

	ts = start("new")
	defer ts.Close()
	if code := me(ts, cookie); code != 401 {
		t.Fatalf("after password change: %d, want 401", code)
	}
}
