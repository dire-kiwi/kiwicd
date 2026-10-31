package server

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS repos (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,          -- owner/name
	kind TEXT NOT NULL,                 -- hosted | mirror
	url TEXT NOT NULL DEFAULT '',       -- remote URL for mirrors
	default_branch TEXT NOT NULL DEFAULT 'main',
	webhook_secret TEXT NOT NULL,
	refs TEXT NOT NULL DEFAULT '{}',    -- last seen ref -> sha
	last_sync_at INTEGER NOT NULL DEFAULT 0,
	last_sync_error TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
-- Columns added after the first release are in migrations.
CREATE TABLE IF NOT EXISTS runs (
	id INTEGER PRIMARY KEY,
	repo_id INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
	number INTEGER NOT NULL,
	attempt INTEGER NOT NULL DEFAULT 1,
	workflow_file TEXT NOT NULL,
	workflow_name TEXT NOT NULL,
	workflow_yaml TEXT NOT NULL,
	title TEXT NOT NULL,
	event TEXT NOT NULL,
	event_json TEXT NOT NULL,
	ref TEXT NOT NULL,
	sha TEXT NOT NULL,
	actor TEXT NOT NULL,
	status TEXT NOT NULL,
	conclusion TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '',
	cancel_requested INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	started_at INTEGER NOT NULL DEFAULT 0,
	finished_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS runs_repo ON runs(repo_id, number);
CREATE TABLE IF NOT EXISTS jobs (
	id INTEGER PRIMARY KEY,
	run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	job_key TEXT NOT NULL,
	name TEXT NOT NULL,
	matrix TEXT NOT NULL DEFAULT 'null',
	runs_on TEXT NOT NULL DEFAULT '[]',
	status TEXT NOT NULL,
	conclusion TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '',
	runner_id INTEGER NOT NULL DEFAULT 0,
	runner_name TEXT NOT NULL DEFAULT '',
	outputs TEXT NOT NULL DEFAULT '{}',
	steps TEXT NOT NULL DEFAULT '[]',
	cancel_requested INTEGER NOT NULL DEFAULT 0,
	log_seq INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	started_at INTEGER NOT NULL DEFAULT 0,
	finished_at INTEGER NOT NULL DEFAULT 0,
	heartbeat_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS jobs_run ON jobs(run_id);
CREATE INDEX IF NOT EXISTS jobs_status ON jobs(status);
CREATE TABLE IF NOT EXISTS log_lines (
	job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
	seq INTEGER NOT NULL,
	step INTEGER NOT NULL,
	ts INTEGER NOT NULL,
	text TEXT NOT NULL,
	PRIMARY KEY (job_id, seq)
);
CREATE TABLE IF NOT EXISTS runners (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL,
	labels TEXT NOT NULL,
	token_hash TEXT NOT NULL UNIQUE,
	os TEXT NOT NULL DEFAULT '',
	arch TEXT NOT NULL DEFAULT '',
	version TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	last_seen_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS registration_tokens (
	token_hash TEXT PRIMARY KEY,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS secrets (
	repo_id INTEGER NOT NULL,           -- 0 = all repositories
	name TEXT NOT NULL,
	value TEXT NOT NULL,                -- encrypted, see secretbox.go
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (repo_id, name)
);
CREATE TABLE IF NOT EXISTS variables (
	repo_id INTEGER NOT NULL,
	name TEXT NOT NULL,
	value TEXT NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (repo_id, name)
);
-- Workflows stored in kiwicd instead of the repository. They apply to every
-- commit, alongside the repository's own .github/workflows/ files.
CREATE TABLE IF NOT EXISTS uploaded_workflows (
	repo_id INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
	file TEXT NOT NULL,                 -- deploy.yml
	content TEXT NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (repo_id, file)
);
CREATE TABLE IF NOT EXISTS meta (
	name TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
-- Tokens for the API (see apitokens.go).
CREATE TABLE IF NOT EXISTS api_tokens (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL,
	token_hash TEXT NOT NULL UNIQUE,
	github_login TEXT NOT NULL DEFAULT '', -- who made it; '' for the admin
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL DEFAULT 0,  -- 0: never
	last_used_at INTEGER NOT NULL DEFAULT 0
);
-- The reusable workflows a run's jobs call, read when it was created (see
-- called.go).
CREATE TABLE IF NOT EXISTS run_workflows (
	run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	path TEXT NOT NULL,                 -- the calling jobs' keys from the run's workflow down: build/deploy
	uses TEXT NOT NULL,                 -- as the calling job has it
	source TEXT NOT NULL,               -- kiwicd | github
	repo TEXT NOT NULL,                 -- owner/name there
	file TEXT NOT NULL,                 -- .github/workflows/x.yml
	sha TEXT NOT NULL,
	yaml TEXT NOT NULL,
	PRIMARY KEY (run_id, path)
);
-- The GitHub App (see github.go): at most one row.
CREATE TABLE IF NOT EXISTS github_app (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	app_id INTEGER NOT NULL,
	slug TEXT NOT NULL DEFAULT '',
	name TEXT NOT NULL DEFAULT '',
	html_url TEXT NOT NULL DEFAULT '',
	owner TEXT NOT NULL DEFAULT '',
	client_id TEXT NOT NULL,
	client_secret TEXT NOT NULL,        -- encrypted, as are the next two
	private_key TEXT NOT NULL,
	webhook_secret TEXT NOT NULL,
	allowed_users TEXT NOT NULL DEFAULT '[]', -- GitHub logins that may sign in
	allowed_orgs TEXT NOT NULL DEFAULT '[]',  -- organizations whose members may
	created_at INTEGER NOT NULL
);
-- Check runs on GitHub for runs' jobs (see checks.go): job_id 0 is a run
-- that failed before it had jobs.
CREATE TABLE IF NOT EXISTS github_checks (
	run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	job_id INTEGER NOT NULL,
	repo TEXT NOT NULL,                 -- owner/name on GitHub
	check_run_id INTEGER NOT NULL,
	check_suite_id INTEGER NOT NULL DEFAULT 0,
	state TEXT NOT NULL,                -- what was last reported: status/conclusion
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (run_id, job_id)
);
CREATE INDEX IF NOT EXISTS github_checks_run ON github_checks(check_run_id);
CREATE INDEX IF NOT EXISTS github_checks_suite ON github_checks(check_suite_id);
`

func openDB(path string) (*sql.DB, error) {
	// journal_size_limit truncates the WAL back to 64 MB after a checkpoint,
	// so one large transaction does not leave a large file behind.
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_pragma=journal_size_limit(67108864)")
	if err != nil {
		return nil, err
	}
	// One connection: SQLite serialises writes anyway, and this avoids SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	if err := incrementalAutoVacuum(db); err != nil {
		return nil, fmt.Errorf("auto_vacuum: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

// migrations add columns to existing tables. Each runs once, when its column
// is missing.
var migrations = []struct{ table, column, sql string }{
	{"repos", "ssh_key", `ALTER TABLE repos ADD COLUMN ssh_key TEXT NOT NULL DEFAULT ''`},
	{"repos", "ssh_public_key", `ALTER TABLE repos ADD COLUMN ssh_public_key TEXT NOT NULL DEFAULT ''`},
	{"repos", "ssh_known_hosts", `ALTER TABLE repos ADD COLUMN ssh_known_hosts TEXT NOT NULL DEFAULT ''`},
	// Last successful sync; a mirror's first one only records a baseline.
	{"repos", "synced_at", `ALTER TABLE repos ADD COLUMN synced_at INTEGER NOT NULL DEFAULT 0;
		UPDATE repos SET synced_at = last_sync_at WHERE last_sync_error = ''`},
	// Log size so far, and whether it hit the per-job limit.
	{"jobs", "log_bytes", `ALTER TABLE jobs ADD COLUMN log_bytes INTEGER NOT NULL DEFAULT 0`},
	{"jobs", "log_truncated", `ALTER TABLE jobs ADD COLUMN log_truncated INTEGER NOT NULL DEFAULT 0`},
	// A finished job's archive index (see logarchive.go); '' while its log
	// lines are in log_lines.
	{"jobs", "log_archive", `ALTER TABLE jobs ADD COLUMN log_archive TEXT NOT NULL DEFAULT ''`},
	// Hash of the token a job gets as secrets.KIWICD_TOKEN (see jobtoken.go).
	{"jobs", "token_hash", `ALTER TABLE jobs ADD COLUMN token_hash TEXT NOT NULL DEFAULT '';
		CREATE INDEX IF NOT EXISTS jobs_token ON jobs(token_hash) WHERE token_hash != ''`},
	// Repositories whose jobs' KIWICD_TOKEN may fetch this one (see jobtoken.go).
	{"repos", "readers", `ALTER TABLE repos ADD COLUMN readers TEXT NOT NULL DEFAULT '[]'`},
	// Ref patterns besides the default branch whose runs get secrets, and
	// whether a run's ref was one when it was created (see protectedrefs.go).
	{"repos", "protected_refs", `ALTER TABLE repos ADD COLUMN protected_refs TEXT NOT NULL DEFAULT '[]'`},
	{"runs", "protected", `ALTER TABLE runs ADD COLUMN protected INTEGER NOT NULL DEFAULT 1`},
	// The workflow's concurrency group, evaluated when the run was created
	// (see concurrency.go).
	{"runs", "concurrency_group", `ALTER TABLE runs ADD COLUMN concurrency_group TEXT NOT NULL DEFAULT '';
		CREATE INDEX IF NOT EXISTS runs_concurrency ON runs(repo_id, concurrency_group) WHERE concurrency_group != ''`},
	// The GitHub user a session is for; '' for the admin (see githublogin.go).
	{"sessions", "github_login", `ALTER TABLE sessions ADD COLUMN github_login TEXT NOT NULL DEFAULT ''`},
	// Jobs of reusable workflows (see called.go): the calling job's row, and
	// for a calling job what it calls and the inputs it gives.
	{"jobs", "parent_id", `ALTER TABLE jobs ADD COLUMN parent_id INTEGER NOT NULL DEFAULT 0`},
	{"jobs", "uses", `ALTER TABLE jobs ADD COLUMN uses TEXT NOT NULL DEFAULT ''`},
	{"jobs", "inputs", `ALTER TABLE jobs ADD COLUMN inputs TEXT NOT NULL DEFAULT 'null'`},
	// Whether runs of workflows that aren't the GitHub repository's own
	// (repository_push and repository_pull_request listeners, uploaded
	// workflows) are reported to it as check runs (see checks.go).
	{"repos", "report_checks", `ALTER TABLE repos ADD COLUMN report_checks INTEGER NOT NULL DEFAULT 0`},
}

// incrementalAutoVacuum switches the database to auto_vacuum=INCREMENTAL, so
// space freed by pruning can be returned to the filesystem. Once the file has
// been written (by creating tables, or even by setting WAL mode) the mode only
// changes when VACUUM rewrites it, which happens once.
func incrementalAutoVacuum(db *sql.DB) error {
	mode := func() (int, error) {
		var m int
		err := db.QueryRow(`PRAGMA auto_vacuum`).Scan(&m)
		return m, err
	}
	if m, err := mode(); err != nil || m == 2 {
		return err
	}
	if _, err := db.Exec(`PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
		return err
	}
	if m, err := mode(); err != nil || m == 2 {
		return err
	}
	var tables int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema`).Scan(&tables); err != nil {
		return err
	}
	if tables > 0 {
		slog.Info("rewriting the database for incremental vacuum; this happens once and may take a while")
	}
	start := time.Now()
	if _, err := db.Exec(`VACUUM`); err != nil {
		return err
	}
	// VACUUM wrote the whole database to the WAL; empty it rather than keep
	// a file as large as the database.
	var busy, walPages, checkpointed int
	if err := db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &walPages, &checkpointed); err != nil {
		return fmt.Errorf("checkpoint after VACUUM: %w", err)
	}
	if busy != 0 {
		slog.Warn("could not truncate the WAL after VACUUM; it stays large until a later checkpoint", "pages", walPages)
	}
	if tables == 0 {
		return nil
	}
	slog.Info("database rewritten", "dur", time.Since(start).Round(time.Millisecond))
	return nil
}

func migrate(db *sql.DB) error {
	for _, m := range migrations {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, m.table, m.column).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		if _, err := db.Exec(m.sql); err != nil {
			return fmt.Errorf("%s.%s: %w", m.table, m.column, err)
		}
	}
	return nil
}

func nowMS() int64 { return time.Now().UnixMilli() }

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
