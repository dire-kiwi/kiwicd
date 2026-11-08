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

	// Runs' workflow_run activity waiting to be handled (see triggers.go).
	wrOnce  sync.Once
	wrQueue chan workflowRunActivity

	// The GitHub App's caches and pending setups (see github.go).
	gh githubState
	// Runs waiting to be reported to GitHub as check runs (see checks.go).
	checks checkReporter

	// Scheduled workflows' entries (see schedule.go).
	sched scheduleState
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
		sched:        newScheduleState(),
	}
	if err := s.expireSessionsOnPasswordChange(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrateAllowList(); err != nil {
		db.Close()
		return nil, fmt.Errorf("roles: %w", err)
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
	go s.scheduleLoop(ctx)
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

// route is an HTTP route and who may use it.
type route struct {
	pattern string
	// need is the role the route needs (see access.go); roleNone for routes
	// anyone may use, which check what they need themselves.
	need role
	// repo finds the repository the request is about, for routes about one.
	repo    *repoOf
	handler http.HandlerFunc
}

func (s *Server) routes() []route {
	const (
		public     = roleNone
		viewer     = roleViewer
		maintainer = roleMaintainer
		admin      = roleAdmin
	)
	return []route{
		{"GET /healthz", public, nil, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok\n")) }},
		{"POST /api/login", public, nil, s.handleLogin},
		{"POST /api/logout", public, nil, s.handleLogout},
		{"GET /api/me", viewer, nil, s.handleMe},
		{"GET /api/auth/providers", public, nil, s.handleAuthProviders},
		{"GET /api/auth/github", public, nil, s.handleGitHubLogin},
		{"GET /api/auth/github/callback", public, nil, s.handleGitHubLoginCallback},

		{"GET /api/github/app", admin, nil, s.handleGetGitHubApp},
		{"PUT /api/github/app", admin, nil, s.handlePutGitHubApp},
		{"DELETE /api/github/app", admin, nil, s.handleDeleteGitHubApp},
		{"POST /api/github/app/manifest", admin, nil, s.handleGitHubAppManifest},
		{"GET /api/github/app/callback", admin, nil, s.handleGitHubAppCallback},
		{"GET /api/github/repos", admin, nil, s.handleListGitHubRepos},
		{"POST /api/github/webhook", public, nil, s.handleGitHubWebhook},

		{"GET /api/roles", admin, nil, s.handleListRoles},
		{"PUT /api/roles", admin, nil, s.handlePutRole},
		{"DELETE /api/roles", admin, nil, s.handleDeleteRole},
		{"GET /api/users", admin, nil, s.handleListUsers},
		{"GET /api/audit", admin, nil, s.handleAuditLog},

		{"GET /api/repos", viewer, nil, s.handleListRepos},
		{"POST /api/repos", admin, nil, s.handleCreateRepo},
		{"GET /api/repos/{owner}/{name}", viewer, pathRepo, s.handleGetRepo},
		{"PATCH /api/repos/{owner}/{name}", maintainer, pathRepo, s.handleUpdateRepo},
		{"DELETE /api/repos/{owner}/{name}", admin, pathRepo, s.handleDeleteRepo},
		{"POST /api/repos/{owner}/{name}/sync", maintainer, pathRepo, s.handleSyncRepo},
		{"GET /api/repos/{owner}/{name}/approved-contributors", maintainer, pathRepo, s.handleListApprovedContributors},
		{"DELETE /api/repos/{owner}/{name}/approved-contributors/{user}", maintainer, pathRepo, s.handleDeleteApprovedContributor},
		{"GET /api/repos/{owner}/{name}/workflows", viewer, pathRepo, s.handleListWorkflows},
		// Badges check who may see them themselves (see badge.go).
		{"GET /api/repos/{owner}/{name}/badge/{file...}", public, nil, s.handleBadge},
		{"POST /api/repos/{owner}/{name}/dispatch", maintainer, pathRepo, s.handleDispatch},
		{"POST /api/repos/{owner}/{name}/dispatches", maintainer, pathRepo, s.handleRepositoryDispatch},
		{"GET /api/repos/{owner}/{name}/uploaded-workflows/{file}", viewer, pathRepo, s.handleGetUploadedWorkflow},
		{"PUT /api/repos/{owner}/{name}/uploaded-workflows/{file}", maintainer, pathRepo, s.handlePutUploadedWorkflow},
		{"DELETE /api/repos/{owner}/{name}/uploaded-workflows/{file}", maintainer, pathRepo, s.handleDeleteUploadedWorkflow},

		{"GET /api/runs", viewer, nil, s.handleListRuns},
		{"GET /api/runs/filters", viewer, nil, s.handleRunFilters},
		{"GET /api/runs/{id}", viewer, runRepo, s.handleGetRun},
		{"POST /api/runs/{id}/cancel", maintainer, runRepo, s.handleCancelRun},
		{"POST /api/runs/{id}/rerun", maintainer, runRepo, s.handleRerun},
		{"POST /api/runs/{id}/approve", maintainer, runRepo, s.handleApproveRun},
		{"GET /api/runs/{id}/logs", viewer, runRepo, s.handleRunLogs},
		{"GET /api/runs/{id}/reports", viewer, runRepo, s.handleRunReports},
		{"GET /api/jobs/{id}", viewer, jobRepo, s.handleGetJob},
		{"GET /api/jobs/{id}/logs", viewer, jobRepo, s.handleJobLogs},
		{"GET /api/jobs/{id}/events", viewer, jobRepo, s.handleJobEvents},
		{"GET /api/events", viewer, eventsRepo, s.handleEvents},

		{"GET /api/runners", viewer, nil, s.handleListRunners},
		{"DELETE /api/runners/{id}", admin, nil, s.handleDeleteRunner},
		{"POST /api/runners/registration-token", admin, nil, s.handleRegistrationToken},

		// Everyone has their own tokens; the handlers check whose.
		{"GET /api/tokens", viewer, nil, s.handleListTokens},
		{"POST /api/tokens", viewer, nil, s.handleCreateToken},
		{"DELETE /api/tokens/{id}", viewer, nil, s.handleDeleteToken},

		// Without ?repo=, the global ones, which need an admin.
		{"GET /api/secrets", maintainer, queryRepo, s.handleListSettings("secrets")},
		{"PUT /api/secrets", maintainer, queryRepo, s.handlePutSetting("secrets")},
		{"DELETE /api/secrets", maintainer, queryRepo, s.handleDeleteSetting("secrets")},
		{"GET /api/variables", maintainer, queryRepo, s.handleListSettings("variables")},
		{"PUT /api/variables", maintainer, queryRepo, s.handlePutSetting("variables")},
		{"DELETE /api/variables", maintainer, queryRepo, s.handleDeleteSetting("variables")},

		// Webhooks check their secret, agents their runner token, git its
		// own way (see handleGit).
		{"POST /api/hooks/{owner}/{name}", public, nil, s.handleWebhook},
		{"POST /api/runner/register", public, nil, s.handleRunnerRegister},
		{"POST /api/runner/poll", public, nil, s.handleRunnerPoll},
		{"POST /api/runner/jobs/{id}/update", public, nil, s.handleRunnerUpdate},
		{"POST /api/runner/jobs/{id}/complete", public, nil, s.handleRunnerComplete},
		{"GET /api/runner/jobs/{id}/source", public, nil, s.handleRunnerSource},
		{"/git/", public, nil, s.handleGit},
		{"/", public, nil, s.handleStatic},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		mux.Handle(rt.pattern, s.authorize(rt))
	}
	return logRequests(s.log, s.secureHeaders(mux))
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"user": requestUser(r), "version": s.cfg.Version, "public_url": s.publicURL(r),
		"access": requestPrincipal(r).access()})
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

// authenticate returns who r is from (see access.go): the admin for a
// session made with the admin password or the password itself, a GitHub
// user with a role for a session made by signing in with GitHub (see
// githublogin.go), whoever made the API token it carries, limited by the
// token's scope (see apitokens.go), or nil if r isn't authenticated. The
// error is errTooManyPasswords if r's client has had too many wrong
// passwords to try another now.
func (s *Server) authenticate(r *http.Request) (*principal, error) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		var exp int64
		var login string
		err := s.db.QueryRow(`SELECT expires_at, github_login FROM sessions WHERE token_hash = ?`, hashToken(c.Value)).Scan(&exp, &login)
		if err == nil && exp > nowMS() {
			if login == "" {
				return adminPrincipal(), nil
			}
			if p, err := s.githubPrincipal(login); err == nil && p.role != roleNone {
				return p, nil
			}
		}
	}
	if p, ok := requestPassword(r); ok && p != "" {
		if strings.HasPrefix(p, apiTokenPrefix) {
			return s.tokenPrincipal(p), nil
		}
		if ok, err := s.tryPassword(r, p); !ok {
			return nil, err
		}
		return adminPrincipal(), nil
	}
	return nil, nil
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
	var p *principal
	if push || !s.jobCanRead(r, name) {
		var err error
		if p, err = s.authenticate(r); err != nil {
			http.Error(w, err.Error(), http.StatusTooManyRequests)
			return
		}
		if p == nil {
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
	if err != nil || p != nil && !p.canView(repo.Name) {
		http.NotFound(w, r)
		return
	}
	user := "job"
	if p != nil {
		user = p.user
	}
	if push && repo.Kind != "hosted" {
		http.Error(w, "this repository is a mirror; push to its origin instead", http.StatusForbidden)
		return
	}
	if push && !p.canMaintain(repo.Name) {
		http.Error(w, forbidden(p, roleMaintainer, repo.Name), http.StatusForbidden)
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
			"REMOTE_USER=" + user,
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
