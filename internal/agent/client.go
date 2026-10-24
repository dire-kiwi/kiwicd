package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
)

// client talks to the kiwicd server's runner API.
type client struct {
	base  string
	token string
	http  *http.Client
}

func newClient(base, token string) *client {
	return &client{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{Timeout: 60 * time.Second}}
}

type apiError struct {
	Status int
	Msg    string
}

func (e *apiError) Error() string { return fmt.Sprintf("server returned %d: %s", e.Status, e.Msg) }

func (c *client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set(protocol.RunnerTokenHeader, c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(b))
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return &apiError{Status: resp.StatusCode, Msg: msg}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *client) register(ctx context.Context, req protocol.RegisterRequest) (*protocol.RegisterResponse, error) {
	var out protocol.RegisterResponse
	return &out, c.do(ctx, "POST", "/api/runner/register", req, &out)
}

func (c *client) poll(ctx context.Context, req protocol.PollRequest) (*protocol.PollResponse, error) {
	var out protocol.PollResponse
	return &out, c.do(ctx, "POST", "/api/runner/poll", req, &out)
}

func (c *client) update(ctx context.Context, jobID int64, u protocol.JobUpdate) (*protocol.JobUpdateResponse, error) {
	var out protocol.JobUpdateResponse
	return &out, c.do(ctx, "POST", fmt.Sprintf("/api/runner/jobs/%d/update", jobID), u, &out)
}

func (c *client) complete(ctx context.Context, jobID int64, done protocol.JobComplete) error {
	return c.do(ctx, "POST", fmt.Sprintf("/api/runner/jobs/%d/complete", jobID), done, nil)
}

func (c *client) download(ctx context.Context, path string, w io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set(protocol.RunnerTokenHeader, c.token)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &apiError{Status: resp.StatusCode, Msg: string(b)}
	}
	_, err = io.Copy(w, resp.Body)
	return err
}
