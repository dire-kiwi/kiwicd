// Package secretsservice opens secrets sealed to its public key for kiwicd
// agents. It runs apart from the kiwicd server, which only has the public
// key, so that the server, its database and its backups hold no secret a
// thief could read.
//
// An agent whose job has sealed secrets sends the job's token with its own
// token for the service (protocol.UnsealRequest). The service asks the
// kiwicd server which running job the token belongs to and for its sealed
// secrets, checks that the agent's client may open secrets of the job's
// repository and environment, opens them and returns them.
package secretsservice

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
	"github.com/dire-kiwi/kiwicd/internal/sealed"
)

// Client is an agent, or a group of agents, that may use the service.
type Client struct {
	Name string `json:"name"`
	// TokenSHA256 is the SHA-256 of the client's token, in hex.
	TokenSHA256 string `json:"token_sha256"`
	// Allow lists the jobs whose secrets the client may open, as
	// OWNER/REPO patterns for jobs without an environment and
	// OWNER/REPO@ENVIRONMENT for jobs with one. * matches any run of
	// characters other than / (so */* is every repository, and */*@* every
	// environment), as in path.Match.
	Allow []string `json:"allow"`
}

// Clients is the clients file.
type Clients struct {
	Clients []Client `json:"clients"`
}

// HashToken returns what goes in a client's token_sha256.
func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// CheckClients reports mistakes in a clients file.
func CheckClients(clients []Client) error {
	names := map[string]bool{}
	for i, c := range clients {
		if c.Name == "" {
			return fmt.Errorf("client %d has no name", i+1)
		}
		if names[c.Name] {
			return fmt.Errorf("client %s is listed twice", c.Name)
		}
		names[c.Name] = true
		if b, err := hex.DecodeString(c.TokenSHA256); err != nil || len(b) != sha256.Size {
			return fmt.Errorf("client %s: token_sha256 must be a SHA-256 in hex (kiwicd-secrets token prints one)", c.Name)
		}
		if len(c.Allow) == 0 {
			return fmt.Errorf("client %s may not open anything: list OWNER/REPO or OWNER/REPO@ENVIRONMENT patterns in allow", c.Name)
		}
		for _, p := range c.Allow {
			repo, env, hasEnv := strings.Cut(p, "@")
			if _, err := path.Match(repo, ""); err != nil || !strings.Contains(repo, "/") {
				return fmt.Errorf("client %s: allow %q: the repository must be an OWNER/REPO pattern", c.Name, p)
			}
			if _, err := path.Match(env, ""); hasEnv && (err != nil || env == "") {
				return fmt.Errorf("client %s: allow %q: bad environment pattern", c.Name, p)
			}
		}
	}
	return nil
}

// allows reports whether the client may open secrets of jobs of repo in env
// ("" for none).
func (c *Client) allows(repo, env string) bool {
	for _, p := range c.Allow {
		repoPat, envPat, hasEnv := strings.Cut(p, "@")
		if hasEnv != (env != "") {
			continue
		}
		if ok, _ := path.Match(strings.ToLower(repoPat), strings.ToLower(repo)); !ok {
			continue
		}
		if ok, _ := path.Match(strings.ToLower(envPat), strings.ToLower(env)); hasEnv && !ok {
			continue
		}
		return true
	}
	return false
}

type Config struct {
	// Server is the kiwicd server's URL.
	Server string
	// Keys open sealed values; the first one's public key is the one the
	// server should seal to. Older ones stay while their secrets are
	// sealed again.
	Keys    []*sealed.PrivateKey
	Clients []Client
	Log     *slog.Logger
	// HTTP fetches from the server; nil means a client with a timeout.
	HTTP *http.Client
}

type Service struct {
	cfg Config
}

func New(cfg Config) (*Service, error) {
	if cfg.Server == "" {
		return nil, errors.New("the kiwicd server's URL is required")
	}
	if len(cfg.Keys) == 0 {
		return nil, errors.New("at least one private key is required")
	}
	if err := CheckClients(cfg.Clients); err != nil {
		return nil, err
	}
	cfg.Server = strings.TrimRight(cfg.Server, "/")
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("the kiwicd server redirected the secrets service; set its URL to where it redirects")
		}}
	}
	return &Service{cfg: cfg}, nil
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/unseal", s.handleUnseal)
	mux.HandleFunc("GET /v1/public-key", func(w http.ResponseWriter, r *http.Request) {
		pub := s.cfg.Keys[0].Public()
		writeJSON(w, 200, map[string]string{"public_key": pub.String(), "key_id": pub.ID()})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]bool{"ok": true}) })
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, format string, args ...any) {
	writeJSON(w, code, map[string]string{"error": fmt.Sprintf(format, args...)})
}

func (s *Service) client(r *http.Request) *Client {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return nil
	}
	h := HashToken(token)
	var found *Client
	for i := range s.cfg.Clients {
		if subtle.ConstantTimeCompare([]byte(h), []byte(strings.ToLower(s.cfg.Clients[i].TokenSHA256))) == 1 {
			found = &s.cfg.Clients[i]
		}
	}
	return found
}

func (s *Service) handleUnseal(w http.ResponseWriter, r *http.Request) {
	c := s.client(r)
	if c == nil {
		httpError(w, 401, "unknown client token")
		return
	}
	var req protocol.UnsealRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.JobToken == "" {
		httpError(w, 400, "send {\"job_token\": ...}")
		return
	}
	job, code, err := s.fetchJob(r.Context(), req.JobToken)
	if err != nil {
		s.cfg.Log.Warn("unseal: fetching the job failed", "client", c.Name, "err", err)
		httpError(w, code, "%v", err)
		return
	}
	log := s.cfg.Log.With("client", c.Name, "job", job.JobID, "run", job.RunID, "repo", job.Repository, "environment", job.Environment,
		"ref", job.Ref, "workflow", job.Workflow, "runner", job.Runner)
	if len(job.Secrets) == 0 {
		writeJSON(w, 200, protocol.UnsealResponse{Secrets: map[string]string{}})
		return
	}
	if !c.allows(job.Repository, job.Environment) {
		target := job.Repository
		if job.Environment != "" {
			target += "@" + job.Environment
		}
		log.Warn("unseal refused: the client may not open this job's secrets")
		httpError(w, 403, "client %s may not open secrets of %s", c.Name, target)
		return
	}
	out := map[string]string{}
	var names []string
	for _, sec := range job.Secrets {
		// The server says what each secret was sealed for. A value sealed
		// for one scope doesn't open for another, but check that the scope
		// is the job's too: global, its repository's, or its environment's.
		if sec.Repository != "" && !strings.EqualFold(sec.Repository, job.Repository) ||
			sec.Environment != "" && (sec.Repository == "" || !strings.EqualFold(sec.Environment, job.Environment)) {
			log.Error("unseal refused: the server sent a secret of another repository or environment", "secret", sec.Name,
				"secret_repo", sec.Repository, "secret_environment", sec.Environment)
			httpError(w, 502, "the server sent secret %s of another repository or environment", sec.Name)
			return
		}
		scope := sealed.Scope{Repository: sec.Repository, Environment: sec.Environment, Name: sec.Name}
		v, err := sealed.Open(s.cfg.Keys, scope, sec.Value)
		if err != nil {
			log.Warn("unseal: a secret didn't open", "secret", scope.String(), "err", err)
			httpError(w, 422, "secret %s: %v", scope, err)
			return
		}
		out[sec.Name] = v
		names = append(names, scope.String())
	}
	log.Info("unsealed secrets", "secrets", strings.Join(names, " "))
	writeJSON(w, 200, protocol.UnsealResponse{Secrets: out})
}

// fetchJob asks the kiwicd server for the running job a token belongs to.
func (s *Service) fetchJob(ctx context.Context, token string) (*protocol.SealedJob, int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", s.cfg.Server+"/api/secrets-service/job", nil)
	if err != nil {
		return nil, 500, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.cfg.HTTP.Do(req)
	if err != nil {
		return nil, 502, fmt.Errorf("asking the kiwicd server about the job: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode != 200 {
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(body))
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		code := 502
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			code = 403
		}
		return nil, code, fmt.Errorf("the kiwicd server refused the job token (%d): %s", resp.StatusCode, msg)
	}
	var job protocol.SealedJob
	if err := json.Unmarshal(body, &job); err != nil {
		return nil, 502, fmt.Errorf("the kiwicd server's answer: %v", err)
	}
	return &job, 0, nil
}
