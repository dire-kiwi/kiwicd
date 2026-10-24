package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
)

func TestHubDropsSlowSubscriber(t *testing.T) {
	h := newHub()
	slow, unsubSlow := h.subscribe("t")
	fast, unsubFast := h.subscribe("t")
	defer unsubFast()
	for i := 0; i < subBuffer+1; i++ {
		h.publish("t", "x", i)
		<-fast
	}
	for range slow {
	}
	unsubSlow() // after the hub closed it: must not panic
	h.publish("t", "x", "still delivered")
	if ev := <-fast; ev.Data != "still delivered" {
		t.Fatalf("fast subscriber got %v", ev.Data)
	}
}

// stallWriter is an SSE ResponseWriter the test can stall, making its
// handler a slow subscriber.
type stallWriter struct {
	header http.Header
	stall  sync.Mutex // held by the test to block writes
	mu     sync.Mutex
	buf    bytes.Buffer
}

func (w *stallWriter) Header() http.Header { return w.header }
func (w *stallWriter) WriteHeader(int)     {}
func (w *stallWriter) Flush()              {}
func (w *stallWriter) Write(b []byte) (int, error) {
	w.stall.Lock()
	w.stall.Unlock()
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(b)
}

type sseEvent struct{ typ, data string }

func (w *stallWriter) events() []sseEvent {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []sseEvent
	var cur sseEvent
	sc := bufio.NewScanner(bytes.NewReader(w.buf.Bytes()))
	sc.Buffer(nil, 64<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.typ = line[7:]
		case strings.HasPrefix(line, "data: "):
			cur.data = line[6:]
		case line == "" && cur.typ != "":
			out = append(out, cur)
			cur = sseEvent{}
		}
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestJobEventsRecoverFromDrops(t *testing.T) {
	srv, err := New(Config{DataDir: filepath.Join(t.TempDir(), "data"), AdminPassword: "pw", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	now := nowMS()
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := srv.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`INSERT INTO repos (id, name, kind, webhook_secret, created_at) VALUES (1, 't/r', 'hosted', 's', ?)`, now)
	mustExec(`INSERT INTO runs (id, repo_id, number, workflow_file, workflow_name, workflow_yaml, title, event, event_json, ref, sha, actor, status, created_at)
		VALUES (1, 1, 1, 'ci.yml', 'CI', '', 'test', 'push', '{}', 'refs/heads/main', 'abc', 'me', ?, ?)`, protocol.StatusInProgress, now)
	mustExec(`INSERT INTO jobs (id, run_id, job_key, name, status, created_at) VALUES (1, 1, 'build', 'build', ?, ?)`, protocol.StatusInProgress, now)

	// Some backlog from before the viewer connects.
	if err := srv.appendLogs(1, []protocol.LogLine{{Text: "early 1"}, {Text: "early 2"}}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequestWithContext(ctx, "GET", "/api/jobs/1/events?after=1", nil)
	req.SetPathValue("id", "1")
	w := &stallWriter{header: http.Header{}}
	done := make(chan struct{})
	go func() {
		srv.handleJobEvents(w, req)
		close(done)
	}()
	hasJob := func() bool {
		for _, ev := range w.events() {
			if ev.typ == "job" {
				return true
			}
		}
		return false
	}
	waitFor(t, "the initial job event", hasJob)

	// Stall the viewer and flood it with more batches than it can buffer.
	w.stall.Lock()
	const batches, perBatch = subBuffer * 2, 3
	for b := 0; b < batches; b++ {
		lines := make([]protocol.LogLine, perBatch)
		for i := range lines {
			lines[i] = protocol.LogLine{Step: 1, TS: nowMS(), Text: fmt.Sprintf("line %d.%d", b, i)}
		}
		if err := srv.appendLogs(1, lines); err != nil {
			t.Fatal(err)
		}
	}
	w.stall.Unlock()

	// One more batch after recovery, delivered live.
	if err := srv.appendLogs(1, []protocol.LogLine{{Text: "last"}}); err != nil {
		t.Fatal(err)
	}
	total := int64(2 + batches*perBatch + 1)
	var got []int64
	collect := func() bool {
		got = got[:0]
		for _, ev := range w.events() {
			if ev.typ != "logs" {
				continue
			}
			var lines []logLine
			if err := json.Unmarshal([]byte(ev.data), &lines); err != nil {
				t.Fatalf("bad logs event %q: %v", ev.data, err)
			}
			for _, l := range lines {
				got = append(got, l.Seq)
			}
		}
		return len(got) > 0 && got[len(got)-1] == total
	}
	waitFor(t, "every line", collect)
	cancel()
	<-done

	// Exactly seqs 2..total, in order, each once.
	if int64(len(got)) != total-1 {
		t.Fatalf("got %d lines, want %d", len(got), total-1)
	}
	for i, seq := range got {
		if seq != int64(i)+2 {
			t.Fatalf("line %d has seq %d, want %d", i, seq, i+2)
		}
	}
	jobs := 0
	for _, ev := range w.events() {
		if ev.typ == "job" {
			jobs++
		}
	}
	if jobs < 2 {
		t.Errorf("job state was not resent after the drop (%d job events)", jobs)
	}
}

func TestEventsEndAfterDrop(t *testing.T) {
	srv, err := New(Config{DataDir: filepath.Join(t.TempDir(), "data"), AdminPassword: "pw", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/events", nil)
	w := &stallWriter{header: http.Header{}}
	done := make(chan struct{})
	go func() {
		srv.handleEvents(w, req)
		close(done)
	}()
	waitFor(t, "hello", func() bool { return len(w.events()) > 0 })
	w.stall.Lock()
	for i := 0; i < subBuffer*2; i++ {
		srv.hub.publish("runs", "runners", nil)
	}
	w.stall.Unlock()
	select {
	case <-done:
		// Ended, so the browser reconnects and the client refetches.
	case <-time.After(10 * time.Second):
		t.Fatal("stream kept going after dropping events")
	}
}
