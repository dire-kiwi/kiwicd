package server

import (
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

	// Many clients together hit the global limit.
	for i := 0; l.allow(string(rune('A' + i%50))); i++ {
		l.failed(string(rune('A' + i%50)))
		if i > globalBurst {
			t.Fatal("no global limit")
		}
	}
	now = now.Add(globalRefill)
	if !l.allow("z") {
		t.Fatal("global limit doesn't refill")
	}
}

func TestPasswordGuessingBlocked(t *testing.T) {
	srv, err := New(Config{DataDir: filepath.Join(t.TempDir(), "data"), AdminPassword: "pw", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	get := func(path, password string) int {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.SetBasicAuth("admin", password)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for i := 0; i < clientBurst; i++ {
		if code := get("/api/me", "guess"); code != 401 {
			t.Fatalf("guess %d: %d", i, code)
		}
	}
	// Now even the right password is refused, over the API and git.
	if code := get("/api/me", "pw"); code != http.StatusTooManyRequests {
		t.Errorf("API after guesses: %d, want 429", code)
	}
	if code := get("/git/t/r.git/info/refs?service=git-upload-pack", "pw"); code != http.StatusTooManyRequests {
		t.Errorf("git after guesses: %d, want 429", code)
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
