// Package server is the kiwicd server: repositories, workflow runs, the job
// scheduler, the agent API and the web UI's API.
package server

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Listen        string
	DataDir       string
	AdminPassword string
	// PublicURL is the URL agents and browsers use to reach the server.
	PublicURL    string
	PollInterval time.Duration
	// RetentionDays is how long completed runs and their logs are kept;
	// 0 keeps them forever.
	RetentionDays int
	// RegistrationToken, if set, is a long-lived registration token accepted
	// alongside the short-lived ones the UI creates (for bundled agents).
	RegistrationToken string
	// SecretKey encrypts secrets and SSH keys in the database (base64 or hex,
	// 32 bytes). If empty, <data>/secret.key is used, and made if missing.
	SecretKey string
	// ClientIPHeader names the header a reverse proxy puts the client's
	// address in (X-Forwarded-For, CF-Connecting-IP), for limiting wrong
	// passwords per client. Only set it when every request comes through
	// that proxy: clients can send the header themselves.
	ClientIPHeader string
	// MaxLogLines and MaxLogBytes cap one job's log; later output is
	// dropped. 0 means the default, negative means no limit.
	MaxLogLines int64
	MaxLogBytes int64
	// ReservedLabels keep runner labels for some repositories and refs:
	// LABEL=OWNER/REPO[@REF][,...], see parseReservedLabels.
	ReservedLabels []string
	// ProtectedLabels are runner labels whose runners take only jobs from
	// protected refs, see protectedLabels.
	ProtectedLabels []string
	// GitHubURL and GitHubAPIURL are where GitHub is, for the GitHub App
	// and logins (see github.go): https://github.com and
	// https://api.github.com by default.
	GitHubURL    string
	GitHubAPIURL string
	Version      string
	Web          fs.FS
}

type Server struct {
	cfg Config
	db  *sql.DB
	box *secretBox
	log *slog.Logger
	hub *hub

	limiter      *passwordLimiter
	proxyWarning sync.Once
	reserved     reservedLabels
	// protectedLabels: runners with one take only protected refs' jobs.
	protectedLabels protectedLabels

	// mu serialises scheduling decisions (run advancement, job assignment).
	mu sync.Mutex
	// jobsReady is closed and replaced whenever jobs are queued.
	readyMu   sync.Mutex
	jobsReady chan struct{}

	syncMu    sync.Mutex
	syncLocks map[int64]*sync.Mutex

	// archiveMu serialises moving finished jobs' logs to archive files.
	archiveMu sync.Mutex

	// Workflow files at the tip of each repository's default branch, by
	// repository ID (see defaultBranchWorkflows).
	tipMu        sync.Mutex
	tipWorkflows map[int64]cachedWorkflows

	// Pull request events waiting to be handled (see pullrequest.go).
	prOnce  sync.Once
	prQueue chan queuedPullRequest

	// The GitHub App's caches and pending setups (see github.go).
	gh githubState
}

func New(cfg Config) (*Server, error) {
	if cfg.AdminPassword == "" {
		return nil, errors.New("an admin password is required (KIWICD_ADMIN_PASSWORD)")
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = time.Minute
	}
	if cfg.MaxLogLines == 0 {
		cfg.MaxLogLines = defaultMaxLogLines
	}
	if cfg.MaxLogBytes == 0 {
		cfg.MaxLogBytes = defaultMaxLogBytes
	}
	if cfg.GitHubURL == "" {
		cfg.GitHubURL = "https://github.com"
	}
	if cfg.GitHubAPIURL == "" {
		cfg.GitHubAPIURL = "https://api.github.com"
	}
	cfg.GitHubURL = strings.TrimRight(cfg.GitHubURL, "/")
	cfg.GitHubAPIURL = strings.TrimRight(cfg.GitHubAPIURL, "/")
	reserved, err := parseReservedLabels(cfg.ReservedLabels)
	if err != nil {
		return nil, err
	}
	protected, err := parseProtectedLabels(cfg.ProtectedLabels)
	if err != nil {
		return nil, err
	}
	// Git commands run in other directories, so relative paths into the data
	// directory would resolve from the wrong place.
	dataDir, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	cfg.DataDir = dataDir
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "repos"), 0o755); err != nil {
		return nil, err
	}
	if _, err := exec.LookPath("git"); err != nil {
		return nil, errors.New("git is required on the server")
	}
	db, err := openDB(filepath.Join(cfg.DataDir, "kiwicd.db"))
	if err != nil {
		return nil, err
	}
	box, err := setupSecretBox(db, cfg, slog.Default())
	if err != nil {
		db.Close()
		return nil, err
	}
	removeSSHKeyFiles(cfg.DataDir)
	s := &Server{
		cfg:             cfg,
		db:              db,
		box:             box,
		log:             slog.Default(),
		hub:             newHub(),
		limiter:         newPasswordLimiter(),
		reserved:        reserved,
		protectedLabels: protected,
		jobsReady:       make(chan struct{}),
		syncLocks:       map[int64]*sync.Mutex{},

		tipWorkflows: map[int64]cachedWorkflows{},
		gh:           newGitHubState(),
	}
	if err := s.expireSessionsOnPasswordChange(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Server) notifyJobsReady() {
	s.readyMu.Lock()
	close(s.jobsReady)
	s.jobsReady = make(chan struct{})
	s.readyMu.Unlock()
}

func (s *Server) jobsReadyChan() <-chan struct{} {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	return s.jobsReady
}

// Run serves HTTP and runs background loops until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{Addr: s.cfg.Listen, Handler: s.Handler(), ReadHeaderTimeout: 30 * time.Second}
	go s.backgroundLoop(ctx)
	go s.pruneLoop(ctx)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	s.log.Info("kiwicd server listening", "addr", s.cfg.Listen, "data", s.cfg.DataDir, "version", s.cfg.Version)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	ui := func(h http.HandlerFunc) http.Handler { return s.requireUser(h) }

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.Handle("GET /api/me", ui(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"user": requestUser(r), "version": s.cfg.Version, "public_url": s.publicURL(r)})
	}))
	mux.HandleFunc("GET /api/auth/providers", s.handleAuthProviders)
	mux.HandleFunc("GET /api/auth/github", s.handleGitHubLogin)
	mux.HandleFunc("GET /api/auth/github/callback", s.handleGitHubLoginCallback)

	mux.Handle("GET /api/github/app", ui(s.handleGetGitHubApp))
	mux.Handle("PUT /api/github/app", ui(s.handlePutGitHubApp))
	mux.Handle("PATCH /api/github/app", ui(s.handleUpdateGitHubApp))
	mux.Handle("DELETE /api/github/app", ui(s.handleDeleteGitHubApp))
	mux.Handle("POST /api/github/app/manifest", ui(s.handleGitHubAppManifest))
	mux.Handle("GET /api/github/app/callback", ui(s.handleGitHubAppCallback))
	mux.Handle("GET /api/github/repos", ui(s.handleListGitHubRepos))
	mux.HandleFunc("POST /api/github/webhook", s.handleGitHubWebhook)

	mux.Handle("GET /api/repos", ui(s.handleListRepos))
	mux.Handle("POST /api/repos", ui(s.handleCreateRepo))
	mux.Handle("GET /api/repos/{owner}/{name}", ui(s.handleGetRepo))
	mux.Handle("PATCH /api/repos/{owner}/{name}", ui(s.handleUpdateRepo))
	mux.Handle("DELETE /api/repos/{owner}/{name}", ui(s.handleDeleteRepo))
	mux.Handle("POST /api/repos/{owner}/{name}/sync", ui(s.handleSyncRepo))
	mux.Handle("GET /api/repos/{owner}/{name}/workflows", ui(s.handleListWorkflows))
	mux.Handle("POST /api/repos/{owner}/{name}/dispatch", ui(s.handleDispatch))
	mux.Handle("GET /api/repos/{owner}/{name}/uploaded-workflows/{file}", ui(s.handleGetUploadedWorkflow))
	mux.Handle("PUT /api/repos/{owner}/{name}/uploaded-workflows/{file}", ui(s.handlePutUploadedWorkflow))
	mux.Handle("DELETE /api/repos/{owner}/{name}/uploaded-workflows/{file}", ui(s.handleDeleteUploadedWorkflow))

	mux.Handle("GET /api/runs", ui(s.handleListRuns))
	mux.Handle("GET /api/runs/{id}", ui(s.handleGetRun))
	mux.Handle("POST /api/runs/{id}/cancel", ui(s.handleCancelRun))
	mux.Handle("POST /api/runs/{id}/rerun", ui(s.handleRerun))
	mux.Handle("GET /api/runs/{id}/reports", ui(s.handleRunReports))
	mux.Handle("GET /api/jobs/{id}", ui(s.handleGetJob))
	mux.Handle("GET /api/jobs/{id}/logs", ui(s.handleJobLogs))
	mux.Handle("GET /api/jobs/{id}/events", ui(s.handleJobEvents))
	mux.Handle("GET /api/events", ui(s.handleEvents))

	mux.Handle("GET /api/runners", ui(s.handleListRunners))
	mux.Handle("DELETE /api/runners/{id}", ui(s.handleDeleteRunner))
	mux.Handle("POST /api/runners/registration-token", ui(s.handleRegistrationToken))

	mux.Handle("GET /api/tokens", ui(s.handleListTokens))
	mux.Handle("POST /api/tokens", ui(s.handleCreateToken))
	mux.Handle("DELETE /api/tokens/{id}", ui(s.handleDeleteToken))

	mux.Handle("GET /api/secrets", ui(s.handleListSettings("secrets")))
	mux.Handle("PUT /api/secrets", ui(s.handlePutSetting("secrets")))
	mux.Handle("DELETE /api/secrets", ui(s.handleDeleteSetting("secrets")))
	mux.Handle("GET /api/variables", ui(s.handleListSettings("variables")))
	mux.Handle("PUT /api/variables", ui(s.handlePutSetting("variables")))
	mux.Handle("DELETE /api/variables", ui(s.handleDeleteSetting("variables")))

	mux.HandleFunc("POST /api/hooks/{owner}/{name}", s.handleWebhook)

	mux.HandleFunc("POST /api/runner/register", s.handleRunnerRegister)
	mux.HandleFunc("POST /api/runner/poll", s.handleRunnerPoll)
	mux.HandleFunc("POST /api/runner/jobs/{id}/update", s.handleRunnerUpdate)
	mux.HandleFunc("POST /api/runner/jobs/{id}/complete", s.handleRunnerComplete)
	mux.HandleFunc("GET /api/runner/jobs/{id}/source", s.handleRunnerSource)

	mux.HandleFunc("/git/", s.handleGit)
	mux.HandleFunc("/", s.handleStatic)
	return logRequests(s.log, s.secureHeaders(mux))
}

func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(rw, r)
		if r.URL.Path == "/api/runner/poll" && rw.status == 200 {
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/git/") {
			log.Debug("http", "method", r.Method, "path", r.URL.Path, "status", rw.status, "dur", time.Since(start))
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ---------------------------------------------------------------- helpers

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, format string, args ...any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf(format, args...)})
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 32<<20))
	return dec.Decode(v)
}

func (s *Server) publicURL(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return strings.TrimRight(s.cfg.PublicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// ---------------------------------------------------------------- auth

const sessionCookie = "kiwicd_session"

func (s *Server) checkPassword(p string) bool {
	return subtle.ConstantTimeCompare([]byte(p), []byte(s.cfg.AdminPassword)) == 1
}

// requestPassword returns the password of Basic or Bearer authorization.
func requestPassword(r *http.Request) (string, bool) {
	if _, p, ok := r.BasicAuth(); ok {
		return p, true
	}
	return strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// adminUser is who the admin password and its sessions act as.
const adminUser = "admin"

// authenticate returns who r is: adminUser for a session made with the
// admin password or the password itself, a GitHub login for a session made
// by signing in with GitHub (see githublogin.go), whoever made the API token
// it carries (see apitokens.go), or "" if r isn't
// authenticated. The error is errTooManyPasswords if r's client has had too
// many wrong passwords to try another now.
func (s *Server) authenticate(r *http.Request) (string, error) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		var exp int64
		var login string
		err := s.db.QueryRow(`SELECT expires_at, github_login FROM sessions WHERE token_hash = ?`, hashToken(c.Value)).Scan(&exp, &login)
		if err == nil && exp > nowMS() {
			if login == "" {
				login = adminUser
			}
			return login, nil
		}
	}
	if p, ok := requestPassword(r); ok && p != "" {
		if user := s.tokenUser(p); user != "" {
			return user, nil
		}
		if ok, err := s.tryPassword(r, p); !ok {
			return "", err
		}
		return adminUser, nil
	}
	return "", nil
}

type userKey struct{}

// requestUser is who made a request that requireUser let through.
func requestUser(r *http.Request) string {
	if u, ok := r.Context().Value(userKey{}).(string); ok {
		return u
	}
	return adminUser
}

func (s *Server) requireUser(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := s.authenticate(r)
		if err != nil {
			httpError(w, http.StatusTooManyRequests, "%v", err)
			return
		}
		if user == "" {
			httpError(w, http.StatusUnauthorized, "login required")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey{}, user)))
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, "bad request: %v", err)
		return
	}
	if ok, err := s.tryPassword(r, req.Password); err != nil {
		httpError(w, http.StatusTooManyRequests, "%v", err)
		return
	} else if !ok {
		time.Sleep(500 * time.Millisecond)
		httpError(w, http.StatusUnauthorized, "wrong password")
		return
	}
	if err := s.startSession(w, r, "", 30*24*time.Hour); err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	writeJSON(w, map[string]string{"user": adminUser})
}

// startSession logs r's browser in: as the admin if githubLogin is "", or as
// that GitHub user.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, githubLogin string, d time.Duration) error {
	token := randomToken(32)
	exp := time.Now().Add(d)
	if _, err := s.db.Exec(`INSERT INTO sessions (token_hash, created_at, expires_at, github_login) VALUES (?, ?, ?, ?)`,
		hashToken(token), nowMS(), exp.UnixMilli(), githubLogin); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", Expires: exp, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: strings.HasPrefix(s.publicURL(r), "https://"),
	})
	return nil
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_, _ = s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, hashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------- git

// handleGit serves hosted and mirrored repositories over git's smart HTTP
// protocol. Pushing is allowed to hosted repositories and triggers workflows.
func (s *Server) handleGit(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/git/")
	i := strings.Index(rest, ".git/")
	name := ""
	if i >= 0 {
		name = rest[:i]
	}
	push := strings.HasSuffix(r.URL.Path, "/git-receive-pack") || r.URL.Query().Get("service") == "git-receive-pack"
	// Jobs can fetch with their KIWICD_TOKEN, but not push. A job token
	// isn't tried as the admin password, so it doesn't count as a wrong one.
	if push || !s.jobCanRead(r, name) {
		user, err := s.authenticate(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusTooManyRequests)
			return
		}
		if user == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="kiwicd"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
	}
	if i < 0 {
		http.NotFound(w, r)
		return
	}
	repo, err := s.getRepo(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if push && repo.Kind != "hosted" {
		http.Error(w, "this repository is a mirror; push to its origin instead", http.StatusForbidden)
		return
	}
	gitPath, _ := exec.LookPath("git")
	h := &cgi.Handler{
		Path: gitPath,
		Args: []string{"http-backend"},
		Root: "/git",
		Env: []string{
			"GIT_PROJECT_ROOT=" + filepath.Join(s.cfg.DataDir, "repos"),
			"GIT_HTTP_EXPORT_ALL=1",
			"REMOTE_USER=" + adminUser,
		},
	}
	h.ServeHTTP(w, r)
	if push && r.Method == http.MethodPost {
		go func() {
			if err := s.syncRepo(context.Background(), repo.ID, false); err != nil {
				s.log.Error("sync after push", "repo", repo.Name, "err", err)
			}
		}()
	}
}

// ---------------------------------------------------------------- static UI

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Web == nil {
		http.Error(w, "web UI not built", http.StatusNotFound)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p != "" {
		if f, err := s.cfg.Web.Open(p); err == nil {
			st, _ := f.Stat()
			f.Close()
			if st != nil && !st.IsDir() {
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				http.ServeFileFS(w, r, s.cfg.Web, p)
				return
			}
		}
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}
	// SPA fallback.
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFileFS(w, r, s.cfg.Web, "index.html")
}

// ---------------------------------------------------------------- background

func (s *Server) backgroundLoop(ctx context.Context) {
	watchdog := time.NewTicker(15 * time.Second)
	poll := time.NewTicker(s.cfg.PollInterval)
	defer watchdog.Stop()
	defer poll.Stop()
	go s.archivePendingLogs()
	s.pollMirrors(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-watchdog.C:
			s.reapLostJobs(ctx)
			_, _ = s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, nowMS())
			_, _ = s.db.Exec(`DELETE FROM api_tokens WHERE expires_at != 0 AND expires_at < ?`, nowMS())
			_, _ = s.db.Exec(`DELETE FROM registration_tokens WHERE expires_at < ?`, nowMS())
		case <-poll.C:
			s.pollMirrors(ctx)
		}
	}
}

func (s *Server) pollMirrors(ctx context.Context) {
	rows, err := s.db.Query(`SELECT id FROM repos WHERE kind = 'mirror'`)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if err := s.syncRepo(ctx, id, false); err != nil {
			s.log.Warn("mirror sync failed", "repo", id, "err", err)
		}
	}
}
