package server

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
)

func TestPruneRuns(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "kiwicd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Server{db: db, log: slog.Default(), hub: newHub()}
	exec := func(q string, args ...any) int64 {
		t.Helper()
		res, err := db.Exec(q, args...)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := db.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	now := time.Now()
	old := now.Add(-40 * 24 * time.Hour).UnixMilli()
	recent := now.Add(-2 * 24 * time.Hour).UnixMilli()
	repo := exec(`INSERT INTO repos (name, kind, webhook_secret, created_at) VALUES ('t/r', 'hosted', 's', ?)`, old)
	text := strings.Repeat("x", 200)
	addRun := func(status string, created, finished int64, logLines ...int) int64 {
		t.Helper()
		run := exec(`INSERT INTO runs (repo_id, number, workflow_file, workflow_name, workflow_yaml, title, event, event_json, ref, sha, actor,
			status, created_at, finished_at) VALUES (?, 1, 'ci.yml', 'CI', '', 'CI', 'push', '{}', 'refs/heads/main', 'abc', 'me', ?, ?, ?)`,
			repo, status, created, finished)
		for _, n := range logLines {
			job := exec(`INSERT INTO jobs (run_id, job_key, name, status, created_at, finished_at) VALUES (?, 'build', 'build', ?, ?, ?)`,
				run, status, created, finished)
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			for seq := 1; seq <= n; seq++ {
				if _, err := tx.Exec(`INSERT INTO log_lines (job_id, seq, step, ts, text) VALUES (?, ?, 0, ?, ?)`, job, seq, created, text); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
		return run
	}
	// More lines than one delete batch, to check batching finishes the job.
	oldDone := addRun(protocol.StatusCompleted, old, old, 2*logDeleteBatch+7, 10)
	oldDone2 := addRun(protocol.StatusCompleted, old, old+1000, 3)
	recentDone := addRun(protocol.StatusCompleted, recent, recent, 5)
	oldRunning := addRun(protocol.StatusInProgress, old, 0, 5)
	oldQueued := addRun(protocol.StatusQueued, old, 0, 1)

	var pagesBefore int
	if err := db.QueryRow(`PRAGMA page_count`).Scan(&pagesBefore); err != nil {
		t.Fatal(err)
	}

	n, err := s.pruneRuns(context.Background(), now.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("pruned %d runs, want 2", n)
	}
	for _, id := range []int64{oldDone, oldDone2} {
		if c := count(`SELECT count(*) FROM runs WHERE id = ?`, id); c != 0 {
			t.Errorf("run %d was kept", id)
		}
		if c := count(`SELECT count(*) FROM jobs WHERE run_id = ?`, id); c != 0 {
			t.Errorf("run %d: %d jobs were kept", id, c)
		}
	}
	for _, id := range []int64{recentDone, oldRunning, oldQueued} {
		if c := count(`SELECT count(*) FROM runs WHERE id = ?`, id); c != 1 {
			t.Errorf("run %d was deleted", id)
		}
	}
	if c := count(`SELECT count(*) FROM log_lines`); c != 5+5+1 {
		t.Errorf("%d log lines left, want 11", c)
	}
	if c := count(`SELECT count(*) FROM log_lines l JOIN jobs j ON j.id = l.job_id WHERE j.run_id = ?`, recentDone); c != 5 {
		t.Errorf("recent run has %d log lines, want 5", c)
	}

	// Pruning again finds nothing.
	if n, err := s.pruneRuns(context.Background(), now.Add(-30*24*time.Hour)); err != nil || n != 0 {
		t.Errorf("second prune: %d, %v", n, err)
	}

	if c := count(`PRAGMA freelist_count`); c == 0 {
		t.Fatal("no free pages after pruning")
	}
	if err := s.incrementalVacuum(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := count(`PRAGMA freelist_count`); c != 0 {
		t.Errorf("%d free pages after incremental vacuum", c)
	}
	if pages := count(`PRAGMA page_count`); pages >= pagesBefore/2 {
		t.Errorf("database has %d pages after vacuum, %d before", pages, pagesBefore)
	}
}

func TestDeleteRepoDeletesRuns(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "kiwicd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Server{cfg: Config{DataDir: t.TempDir()}, db: db, log: slog.Default(), hub: newHub()}
	res, err := db.Exec(`INSERT INTO repos (name, kind, webhook_secret, created_at) VALUES ('t/r', 'hosted', 's', 0)`)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := res.LastInsertId()
	for i := 0; i < 3; i++ {
		res, err := db.Exec(`INSERT INTO runs (repo_id, number, workflow_file, workflow_name, workflow_yaml, title, event, event_json, ref, sha, actor,
			status, created_at) VALUES (?, ?, 'ci.yml', 'CI', '', 'CI', 'push', '{}', 'refs/heads/main', 'abc', 'me', 'completed', 0)`, repo, i+1)
		if err != nil {
			t.Fatal(err)
		}
		run, _ := res.LastInsertId()
		res, err = db.Exec(`INSERT INTO jobs (run_id, job_key, name, status, created_at) VALUES (?, 'b', 'b', 'completed', 0)`, run)
		if err != nil {
			t.Fatal(err)
		}
		job, _ := res.LastInsertId()
		if _, err := db.Exec(`INSERT INTO log_lines (job_id, seq, step, ts, text) VALUES (?, 1, 0, 0, 'hi')`, job); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.deleteRepo(repo, "t/r"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"repos", "runs", "jobs", "log_lines"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%d rows left in %s", n, table)
		}
	}
}

func TestIncrementalAutoVacuum(t *testing.T) {
	autoVacuum := func(db *sql.DB) int {
		t.Helper()
		var mode int
		if err := db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil {
			t.Fatal(err)
		}
		return mode
	}

	t.Run("new database", func(t *testing.T) {
		db, err := openDB(filepath.Join(t.TempDir(), "kiwicd.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if m := autoVacuum(db); m != 2 {
			t.Errorf("auto_vacuum = %d, want 2 (incremental)", m)
		}
	})

	t.Run("existing database", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "kiwicd.db")
		// A database created before auto_vacuum was set, with some data.
		old, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := old.Exec(schema); err != nil {
			t.Fatal(err)
		}
		if _, err := old.Exec(`INSERT INTO repos (name, kind, webhook_secret, created_at) VALUES ('t/r', 'hosted', 's', 0)`); err != nil {
			t.Fatal(err)
		}
		// A few MB of logs, so that a WAL left behind by VACUUM would show.
		if _, err := old.Exec(`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 20000)
			INSERT INTO log_lines (job_id, seq, step, ts, text) SELECT 1, i, 0, 0, hex(randomblob(125)) FROM c`); err != nil {
			t.Fatal(err)
		}
		if _, err := old.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			t.Fatal(err)
		}
		if m := autoVacuum(old); m != 0 {
			t.Fatalf("setup: auto_vacuum = %d, want 0", m)
		}
		old.Close()

		db, err := openDB(path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if m := autoVacuum(db); m != 2 {
			t.Errorf("auto_vacuum = %d after migration, want 2 (incremental)", m)
		}
		var name string
		if err := db.QueryRow(`SELECT name FROM repos`).Scan(&name); err != nil || name != "t/r" {
			t.Errorf("data lost in migration: %q, %v", name, err)
		}
		// VACUUM writes the whole database to the WAL; it must not stay that big.
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		wal, err := os.Stat(path + "-wal")
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err == nil && wal.Size() > 1<<20 {
			t.Errorf("WAL is %d bytes after migration (database %d bytes)", wal.Size(), st.Size())
		}
	})
}
