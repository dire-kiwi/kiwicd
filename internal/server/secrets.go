package server

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
	"github.com/dire-kiwi/kiwicd/internal/sealed"
)

// Secrets and variables are global (repo_id 0), a repository's, or one of its
// environments' (see environments.go). A job gets the global ones, its
// repository's and its environment's, each overriding the ones before.
//
// Secrets are stored encrypted in one of two ways:
//
//   - with the server's own key (secretbox.go). The server opens them and
//     sends them to the agent with the job.
//   - sealed to the public key of a secrets service (package sealed), when
//     the server is started with one (--secrets-public-key). The server
//     can't open these: it gives the agent their names, and the agent gets
//     them from its secrets service, which asks the server for them with the
//     job's token (handleSecretsServiceJob) and opens them.
//
// With a public key, new and updated secrets are sealed to it; the UI seals
// them in the browser, so the server never sees them. Secrets stored before
// the key was set are resealed to it with POST /api/secrets/reseal.

const purposeEnvironmentSecret = "environment_secrets.value"

var settingNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// settingScope is where a request's secrets or variables are: global, a
// repository's or an environment's.
type settingScope struct {
	table, keyCol string
	key           int64
	repo, env     string
}

func (sc settingScope) secrets() bool { return strings.HasSuffix(sc.table, "secrets") }

func (sc settingScope) purpose() string {
	if sc.table == "environment_secrets" {
		return purposeEnvironmentSecret
	}
	return purposeSecret
}

func (sc settingScope) sealedScope(name string) sealed.Scope {
	return sealed.Scope{Repository: sc.repo, Environment: sc.env, Name: name}
}

// settingScope reads ?repo=OWNER/NAME and ?environment=NAME. kind is
// "secrets" or "variables".
func (s *Server) settingScope(r *http.Request, kind string) (settingScope, int, error) {
	repoName, envName := r.URL.Query().Get("repo"), r.URL.Query().Get("environment")
	if repoName == "" {
		if envName != "" {
			return settingScope{}, 400, errors.New("an environment needs a repository (?repo=OWNER/NAME)")
		}
		return settingScope{table: kind, keyCol: "repo_id"}, 0, nil
	}
	repo, err := s.getRepo(repoName)
	if err != nil {
		return settingScope{}, 404, fmt.Errorf("no such repository %s", repoName)
	}
	if envName == "" {
		return settingScope{table: kind, keyCol: "repo_id", key: repo.ID, repo: repo.Name}, 0, nil
	}
	env, err := s.getEnvironment(repo.ID, envName)
	if err != nil {
		return settingScope{}, 404, fmt.Errorf("%s has no environment %s", repo.Name, envName)
	}
	return settingScope{table: "environment_" + kind, keyCol: "environment_id", key: env.ID, repo: repo.Name, env: env.Name}, 0, nil
}

func (s *Server) handleListSettings(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sc, code, err := s.settingScope(r, kind)
		if err != nil {
			httpError(w, code, "%v", err)
			return
		}
		rows, err := s.db.Query(`SELECT name, value, updated_at FROM `+sc.table+` WHERE `+sc.keyCol+` = ? ORDER BY name`, sc.key)
		if err != nil {
			httpError(w, 500, "%v", err)
			return
		}
		defer rows.Close()
		type item struct {
			Name      string `json:"name"`
			Value     string `json:"value,omitempty"`
			UpdatedAt int64  `json:"updated_at"`
			// The public key a secret is sealed to; empty if the server
			// can read it.
			KeyID string `json:"key_id,omitempty"`
		}
		out := []item{}
		for rows.Next() {
			var it item
			_ = rows.Scan(&it.Name, &it.Value, &it.UpdatedAt)
			if sc.secrets() {
				it.KeyID = sealed.KeyID(it.Value)
				it.Value = "" // secrets are write-only
			}
			out = append(out, it)
		}
		writeJSON(w, out)
	}
}

func (s *Server) handlePutSetting(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sc, code, err := s.settingScope(r, kind)
		if err != nil {
			httpError(w, code, "%v", err)
			return
		}
		var req struct {
			Name  string `json:"name"`
			Value string `json:"value"`
			// A secret already sealed to the public key, for its scope.
			EncryptedValue string `json:"encrypted_value"`
		}
		if err := readJSON(r, &req); err != nil {
			httpError(w, 400, "bad request: %v", err)
			return
		}
		if !settingNameRE.MatchString(req.Name) {
			httpError(w, 400, "names may contain only letters, digits and underscores, and must not start with a digit")
			return
		}
		value := req.Value
		switch {
		case !sc.secrets():
			if req.EncryptedValue != "" {
				httpError(w, 400, "variables are not encrypted; send value")
				return
			}
		case req.EncryptedValue != "":
			if s.sealKey == nil {
				httpError(w, 400, "the server has no secrets public key to check encrypted_value against; send value instead")
				return
			}
			if req.Value != "" {
				httpError(w, 400, "send value or encrypted_value, not both")
				return
			}
			if err := s.sealKey.Check(req.EncryptedValue); err != nil {
				httpError(w, 400, "encrypted_value: %v", err)
				return
			}
			value = req.EncryptedValue
		case s.sealKey != nil:
			if value, err = s.sealKey.Seal(sc.sealedScope(req.Name), req.Value); err != nil {
				httpError(w, 500, "%v", err)
				return
			}
		default:
			value = s.box.seal(sc.purpose(), req.Value)
		}
		_, err = s.db.Exec(`INSERT INTO `+sc.table+` (`+sc.keyCol+`, name, value, updated_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (`+sc.keyCol+`, name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, sc.key, req.Name, value, nowMS())
		if err != nil {
			httpError(w, 500, "%v", err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	}
}

func (s *Server) handleDeleteSetting(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sc, code, err := s.settingScope(r, kind)
		if err != nil {
			httpError(w, code, "%v", err)
			return
		}
		if _, err := s.db.Exec(`DELETE FROM `+sc.table+` WHERE `+sc.keyCol+` = ? AND name = ?`, sc.key, r.URL.Query().Get("name")); err != nil {
			httpError(w, 500, "%v", err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	}
}

// ---------------------------------------------------------------- what jobs get

// storedSetting is a secret or variable as stored, with where it's from.
type storedSetting struct {
	scope   sealed.Scope
	purpose string
	value   string
}

// layeredSettings returns the stored secrets or variables (kind) of a
// repository and, if env isn't nil, its environment, with the global ones,
// by name: environment values override the repository's, which override
// global ones.
func (s *Server) layeredSettings(kind string, repoID int64, env *Environment) (map[string]storedSetting, error) {
	out := map[string]storedSetting{}
	rows, err := s.db.Query(`SELECT s.name, s.value, coalesce(r.name, '') FROM `+kind+` s LEFT JOIN repos r ON r.id = s.repo_id
		WHERE s.repo_id = 0 OR s.repo_id = ? ORDER BY s.repo_id`, repoID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var st storedSetting
		if err := rows.Scan(&st.scope.Name, &st.value, &st.scope.Repository); err != nil {
			rows.Close()
			return nil, err
		}
		st.purpose = purposeSecret
		out[st.scope.Name] = st
	}
	rows.Close()
	if err := rows.Err(); err != nil || env == nil {
		return out, err
	}
	repo, err := s.getRepoByID(repoID)
	if err != nil {
		return nil, err
	}
	rows, err = s.db.Query(`SELECT name, value FROM environment_`+kind+` WHERE environment_id = ?`, env.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		st := storedSetting{scope: sealed.Scope{Repository: repo.Name, Environment: env.Name}, purpose: purposeEnvironmentSecret}
		if err := rows.Scan(&st.scope.Name, &st.value); err != nil {
			return nil, err
		}
		out[st.scope.Name] = st
	}
	return out, rows.Err()
}

// varsFor returns the variables of a repository's jobs, with those of env
// if it isn't nil.
func (s *Server) varsFor(repoID int64, env *Environment) map[string]string {
	out := map[string]string{}
	vars, err := s.layeredSettings("variables", repoID, env)
	if err != nil {
		s.log.Warn("loading variables", "repo", repoID, "err", err)
	}
	for k, v := range vars {
		out[k] = v.value
	}
	return out
}

// jobSecrets returns the secrets a repository's jobs get, with those of env
// if it isn't nil: the ones the server can read, opened, and the ones sealed
// to the secrets service's public key, which only the agent's secrets
// service opens.
func (s *Server) jobSecrets(repoID int64, env *Environment) (map[string]string, []protocol.SealedSecret, error) {
	stored, err := s.layeredSettings("secrets", repoID, env)
	if err != nil {
		return nil, nil, err
	}
	plain := map[string]string{}
	var out []protocol.SealedSecret
	for name, st := range stored {
		if sealed.Is(st.value) {
			out = append(out, protocol.SealedSecret{Name: name, Repository: st.scope.Repository, Environment: st.scope.Environment, Value: st.value})
			continue
		}
		v, err := s.box.open(st.purpose, st.value)
		if err != nil {
			return nil, nil, fmt.Errorf("secret %s: %w", name, err)
		}
		plain[name] = v
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return plain, out, nil
}

// ---------------------------------------------------------------- the secrets service

// handleSecretsServiceJob tells a secrets service which running job a token
// (secrets.KIWICD_TOKEN, as a bearer token) belongs to, and gives it the
// job's sealed secrets to open for the job's agent. Runs that get no secrets
// get none here either.
func (s *Server) handleSecretsServiceJob(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		httpError(w, 401, "send the job's token as a bearer token")
		return
	}
	var jobID int64
	err := s.db.QueryRow(`SELECT id FROM jobs WHERE token_hash = ? AND status = ?`, hashToken(token), protocol.StatusInProgress).Scan(&jobID)
	if errors.Is(err, sql.ErrNoRows) {
		httpError(w, 401, "the token is not a running job's")
		return
	}
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	j, err := s.getJob(jobID)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	run, err := s.getRun(j.RunID)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	out := protocol.SealedJob{JobID: j.ID, RunID: run.ID, Repository: run.Repo, Ref: run.Ref, Workflow: run.WorkflowFile, Event: run.Event,
		Runner: j.RunnerName, Secrets: []protocol.SealedSecret{}}
	if s.runGetsSecrets(run) {
		env, err := s.jobEnvironment(run, j)
		if err != nil {
			httpError(w, 403, "%v", err)
			return
		}
		if env != nil {
			out.Environment = env.Name
		}
		if _, out.Secrets, err = s.jobSecrets(run.RepoID, env); err != nil {
			httpError(w, 500, "%v", err)
			return
		}
		if out.Secrets == nil {
			out.Secrets = []protocol.SealedSecret{}
		}
	}
	s.log.Info("secrets service fetched a job's sealed secrets", "job", j.ID, "repo", run.Repo, "environment", out.Environment, "secrets", len(out.Secrets))
	writeJSON(w, out)
}

// ---------------------------------------------------------------- the public key

// secretsKeyInfo describes the secrets public key and how many stored
// secrets the server can still read.
type secretsKeyInfo struct {
	PublicKey string `json:"public_key,omitempty"`
	KeyID     string `json:"key_id,omitempty"`
	// Secrets sealed with the server's own key, which it can read.
	ServerSealed int `json:"server_sealed"`
	// Secrets sealed to another public key, such as one rotated out.
	OtherKey int `json:"other_key"`
}

func (s *Server) handleSecretsKey(w http.ResponseWriter, r *http.Request) {
	var info secretsKeyInfo
	if s.sealKey != nil {
		info.PublicKey, info.KeyID = s.sealKey.String(), s.sealKey.ID()
	}
	for _, table := range []string{"secrets", "environment_secrets"} {
		rows, err := s.db.Query(`SELECT value FROM ` + table)
		if err != nil {
			httpError(w, 500, "%v", err)
			return
		}
		for rows.Next() {
			var v string
			_ = rows.Scan(&v)
			switch {
			case !sealed.Is(v):
				info.ServerSealed++
			case s.sealKey == nil || sealed.KeyID(v) != s.sealKey.ID():
				info.OtherKey++
			}
		}
		rows.Close()
	}
	writeJSON(w, info)
}

// handleResealSecrets seals every secret the server can read to the public
// key, so that it no longer can.
func (s *Server) handleResealSecrets(w http.ResponseWriter, r *http.Request) {
	if s.sealKey == nil {
		httpError(w, 400, "the server has no secrets public key (--secrets-public-key)")
		return
	}
	n, err := s.resealSecrets()
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	writeJSON(w, map[string]int{"resealed": n})
}

func (s *Server) resealSecrets() (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	type row struct {
		key   int64
		scope sealed.Scope
		value string
	}
	n := 0
	for _, t := range []struct{ table, keyCol, query, purpose string }{
		{"secrets", "repo_id", `SELECT s.repo_id, coalesce(r.name, ''), '', s.name, s.value FROM secrets s LEFT JOIN repos r ON r.id = s.repo_id`, purposeSecret},
		{"environment_secrets", "environment_id", `SELECT s.environment_id, r.name, e.name, s.name, s.value FROM environment_secrets s
			JOIN environments e ON e.id = s.environment_id JOIN repos r ON r.id = e.repo_id`, purposeEnvironmentSecret},
	} {
		rows, err := tx.Query(t.query)
		if err != nil {
			return 0, err
		}
		var todo []row
		for rows.Next() {
			var rw row
			if err := rows.Scan(&rw.key, &rw.scope.Repository, &rw.scope.Environment, &rw.scope.Name, &rw.value); err != nil {
				rows.Close()
				return 0, err
			}
			if !sealed.Is(rw.value) {
				todo = append(todo, rw)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, err
		}
		for _, rw := range todo {
			plain, err := s.box.open(t.purpose, rw.value)
			if err != nil {
				return 0, fmt.Errorf("secret %s: %w", rw.scope, err)
			}
			v, err := s.sealKey.Seal(rw.scope, plain)
			if err != nil {
				return 0, err
			}
			if _, err := tx.Exec(`UPDATE `+t.table+` SET value = ? WHERE `+t.keyCol+` = ? AND name = ?`, v, rw.key, rw.scope.Name); err != nil {
				return 0, err
			}
			n++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if n > 0 {
		s.log.Info("resealed secrets to the secrets public key; vacuuming the database to drop the old copies", "count", n, "key", s.sealKey.ID())
		// The server could still open the old values left in free pages and
		// the WAL until the file is rewritten.
		if _, err := s.db.Exec(`VACUUM`); err != nil {
			s.log.Warn("vacuum after resealing secrets", "err", err)
		}
		if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			s.log.Warn("checkpoint after resealing secrets", "err", err)
		}
	}
	return n, nil
}
