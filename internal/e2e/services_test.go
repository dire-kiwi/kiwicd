package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dire-kiwi/kiwicd/internal/agent"
	"github.com/dire-kiwi/kiwicd/internal/server"
)

// serviceImage answers each connection on port 8080 with "hello $GREETING".
const serviceImage = "kiwicd-e2e-service:test"

const serviceDockerfile = `FROM debian:trixie-slim
CMD ["perl", "-MIO::Socket::INET", "-e", "my $s = IO::Socket::INET->new(LocalAddr => '0.0.0.0:8080', Listen => 5, ReuseAddr => 1) or die $!; while (my $c = $s->accept) { print $c \"hello $ENV{GREETING}\\n\"; close $c }"]
`

const servicesWorkflow = `
name: Services
on: push
jobs:
  host:
    runs-on: self-hosted
    services:
      web:
        image: kiwicd-e2e-service:test
        env:
          GREETING: host-${{ github.run_number }}
        ports: ["8080"]
        options: >-
          --health-cmd "perl -MIO::Socket::INET -e 'IO::Socket::INET->new(q(127.0.0.1:8080)) or exit 1'"
          --health-interval 1s
    steps:
      - name: The service is on localhost
        shell: bash
        run: |
          test -n "${{ job.services.web.id }}"
          case "${{ job.services.web.network }}" in kiwicd-r*) ;; *) exit 1 ;; esac
          exec 3<>"/dev/tcp/127.0.0.1/${{ job.services.web.ports[8080] }}"
          read -r line <&3
          test "$line" = "hello host-1" && echo "host job reached its service"
  container:
    runs-on: self-hosted
    container:
      image: debian:trixie-slim
      env:
        IN_CONTAINER: "yes"
    services:
      web:
        image: kiwicd-e2e-service:test
        env:
          GREETING: container
        options: >-
          --health-cmd "perl -MIO::Socket::INET -e 'IO::Socket::INET->new(q(127.0.0.1:8080)) or exit 1'"
          --health-interval 1s
    steps:
      - uses: actions/checkout@v4
      - name: The job runs in its container, which reaches the service by name
        shell: bash
        run: |
          test "$IN_CONTAINER" = yes && test "$(cat hello.txt)" = hi
          grep -q trixie /etc/os-release
          test ! -e /var/run/docker.sock
          test -n "${{ job.container.id }}"
          exec 3<>/dev/tcp/web/8080
          read -r line <&3
          test "$line" = "hello container" && echo "container job reached its service"
`

// TestServices runs jobs with services and a container on an agent that
// runs jobs on its machine: a host job reaches its service on a localhost
// port, a container job by the service's name, and nothing is left after.
func TestServices(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if out, err := exec.Command("docker", "image", "inspect", "debian:trixie-slim").CombinedOutput(); err != nil {
		t.Skipf("needs Docker with debian:trixie-slim: %v %s", err, out)
	}
	build := exec.Command("docker", "build", "-q", "-t", serviceImage, "-")
	build.Stdin = strings.NewReader(serviceDockerfile)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", serviceImage).Run() })

	e := startE2E(t, "e2e-services")
	r, logs := e.pushAndWait(t, map[string]string{".github/workflows/ci.yml": servicesWorkflow, "hello.txt": "hi\n"})
	want := map[string]string{"host": "success", "container": "success"}
	for k, v := range want {
		if r[k] != v {
			t.Errorf("job %s: got %q want %q\n%s", k, r[k], v, logs[k])
		}
	}
	for job, s := range map[string]string{"host": "host job reached its service", "container": "container job reached its service"} {
		if !strings.Contains(logs[job], s) {
			t.Errorf("%s job's log lacks %q:\n%s", job, s, logs[job])
		}
	}
	assertNoDockerLeftovers(t, e.runnerID)
}

const noDockerWorkflow = `
name: No Docker
on: push
jobs:
  services:
    runs-on: self-hosted
    services:
      db:
        image: postgres:17
    steps:
      - run: echo "services ran"
  plain:
    runs-on: self-hosted
    steps:
      - run: echo "plain ran"
`

// TestServicesWithoutDocker fails a job with services, saying why, on an
// agent that can't reach Docker; jobs that don't need it still run.
func TestServicesWithoutDocker(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(t.TempDir(), "no-docker.sock"))
	e := startE2E(t, "e2e-no-docker")
	r, logs := e.pushAndWait(t, map[string]string{".github/workflows/ci.yml": noDockerWorkflow})
	if r["services"] != "failure" || r["plain"] != "success" {
		t.Errorf("got %v\n%v", r, logs)
	}
	if !strings.Contains(logs["services"], "This job's services run in Docker, but the agent can't reach the Docker daemon") ||
		strings.Contains(logs["services"], "services ran") {
		t.Errorf("services job:\n%s", logs["services"])
	}
}

type e2eEnv struct {
	url      string
	runnerID int64
	tmp      string
}

// startE2E starts a server and an agent that runs jobs on this machine.
func startE2E(t *testing.T, name string) *e2eEnv {
	t.Helper()
	tmp := t.TempDir()
	srv, err := server.New(server.Config{DataDir: filepath.Join(tmp, "data"), AdminPassword: "pw", RegistrationToken: "reg", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	creds, err := agent.Register(ctx, ts.URL, "reg", name, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{Creds: creds, WorkDir: filepath.Join(tmp, "work"), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = a.Run(ctx) }()
	return &e2eEnv{url: ts.URL, runnerID: creds.RunnerID, tmp: tmp}
}

func (e *e2eEnv) api(t *testing.T, method, path, body string, out any) {
	t.Helper()
	req, _ := http.NewRequest(method, e.url+path, strings.NewReader(body))
	req.SetBasicAuth("admin", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatal(err)
		}
	}
}

// pushAndWait pushes files to a new repository t/repo, waits for its run
// and returns each job's conclusion and log.
func (e *e2eEnv) pushAndWait(t *testing.T, files map[string]string) (map[string]string, map[string]string) {
	t.Helper()
	e.api(t, "POST", "/api/repos", `{"name":"t/repo"}`, nil)
	src := filepath.Join(e.tmp, "src")
	for name, content := range files {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(src, name)), 0o755)
		_ = os.WriteFile(filepath.Join(src, name), []byte(content), 0o644)
	}
	run(t, src, "git", "init", "-q", "-b", "main")
	run(t, src, "git", "add", "-A")
	run(t, src, "git", "commit", "-qm", "init")
	run(t, src, "git", "push", "-q", strings.Replace(e.url, "http://", "http://admin:pw@", 1)+"/git/t/repo.git", "main")

	var runs []struct{ ID int64 }
	deadline := time.Now().Add(30 * time.Second)
	for len(runs) == 0 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		e.api(t, "GET", "/api/runs?repo=t/repo", "", &runs)
	}
	if len(runs) != 1 {
		t.Fatalf("expected one run, got %d", len(runs))
	}
	var r runResp
	for time.Now().Before(deadline.Add(3 * time.Minute)) {
		e.api(t, "GET", fmt.Sprintf("/api/runs/%d", runs[0].ID), "", &r)
		if r.Run.Status == "completed" {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	got, logs := map[string]string{}, map[string]string{}
	for _, j := range r.Jobs {
		got[j.Name] = j.Conclusion
		var lines []struct{ Text string }
		e.api(t, "GET", fmt.Sprintf("/api/jobs/%d/logs", j.ID), "", &lines)
		for _, l := range lines {
			logs[j.Name] += l.Text + "\n"
		}
	}
	return got, logs
}

// assertNoDockerLeftovers checks that a runner's jobs left no containers,
// volumes, networks or images.
func assertNoDockerLeftovers(t *testing.T, runnerID int64) {
	t.Helper()
	prefix := fmt.Sprintf("kiwicd-r%d-j", runnerID)
	for _, args := range [][]string{
		{"ps", "-a", "--format", "{{.Names}}"},
		{"volume", "ls", "--format", "{{.Name}}"},
		{"network", "ls", "--format", "{{.Name}}"},
		{"image", "ls", "--format", "{{.Repository}}"},
	} {
		out, err := exec.Command("docker", args...).Output()
		if err != nil {
			t.Fatalf("docker %v: %v", args, err)
		}
		for _, name := range strings.Fields(string(out)) {
			if strings.HasPrefix(name, prefix) {
				t.Errorf("docker %s: %s is left", args[0], name)
			}
		}
	}
}
