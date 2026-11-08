// Command kiwicd-agent runs kiwicd jobs on this machine. Register it once with
// a registration token from the server's Runners page, then run it:
//
//	kiwicd-agent register --url https://ci.example.com --token TOKEN --labels deploy
//	kiwicd-agent run
//
// For containers, `run` registers itself first when KIWICD_URL and
// KIWICD_REGISTRATION_TOKEN are set and no .runner file exists yet.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/dire-kiwi/kiwicd/internal/agent"
)

var version = "dev"

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func splitLabels(s string) []string {
	var out []string
	for _, l := range strings.Split(s, ",") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func usage() {
	fmt.Fprintf(os.Stderr, `kiwicd-agent %s

Usage:
  kiwicd-agent register --url URL --token TOKEN [--name NAME] [--labels a,b:docker://image]
  kiwicd-agent run [--config .runner] [--work-dir _work] [--capacity N]
                   [--job-image IMAGE [--job-options OPTS]] [--docker-socket PATH]
  kiwicd-agent version
`, version)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "register":
		err = register(ctx, os.Args[2:])
	case "run":
		err = run(ctx, os.Args[2:])
	case "version", "--version":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func register(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("register", flag.ExitOnError)
	url := fs.String("url", env("KIWICD_URL", ""), "kiwicd server URL")
	token := fs.String("token", env("KIWICD_REGISTRATION_TOKEN", ""), "registration token from the Runners page")
	name := fs.String("name", env("KIWICD_RUNNER_NAME", ""), "runner name (default: hostname)")
	labels := fs.String("labels", env("KIWICD_LABELS", ""), "extra labels: name, name:host or name:docker://image")
	config := fs.String("config", env("KIWICD_CONFIG", ".runner"), "where to store the registration")
	_ = fs.Parse(args)
	if *url == "" || *token == "" {
		return errors.New("--url and --token are required")
	}
	creds, err := agent.Register(ctx, *url, *token, *name, splitLabels(*labels), version)
	if err != nil {
		return err
	}
	if err := creds.Save(*config); err != nil {
		return err
	}
	fmt.Printf("Registered runner %q (id %d) with %s. Credentials saved to %s.\n", creds.Name, creds.RunnerID, creds.URL, *config)
	return nil
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	config := fs.String("config", env("KIWICD_CONFIG", ".runner"), "registration file written by `register`")
	workDir := fs.String("work-dir", env("KIWICD_WORK_DIR", "_work"), "directory for job workspaces and the action cache")
	capacity := fs.Int("capacity", 1, "jobs to run at once")
	socket := fs.String("docker-socket", env("KIWICD_DOCKER_SOCKET", "-"), "Docker socket to mount into job containers, which gives jobs the machine; - for none")
	jobImage := fs.String("job-image", env("KIWICD_JOB_IMAGE", ""), "run each job in a new container of this image instead of on this machine")
	jobOptions := fs.String("job-options", env("KIWICD_JOB_OPTIONS", ""), "docker create options for job containers, e.g. --memory 4g --cpus 2")
	bridgePrefix := fs.String("job-bridge-prefix", env("KIWICD_JOB_BRIDGE_PREFIX", "kiwicd-"), "start of the network interface names of --job-image jobs' networks")
	actionsURL := fs.String("actions-url", env("KIWICD_ACTIONS_URL", "https://github.com"), "where uses: owner/repo@ref actions are cloned from")
	_ = fs.Parse(args)

	creds, err := agent.LoadCredentials(*config)
	if errors.Is(err, os.ErrNotExist) && os.Getenv("KIWICD_URL") != "" && os.Getenv("KIWICD_REGISTRATION_TOKEN") != "" {
		slog.Info("no registration found; registering", "url", os.Getenv("KIWICD_URL"))
		creds, err = agent.Register(ctx, os.Getenv("KIWICD_URL"), os.Getenv("KIWICD_REGISTRATION_TOKEN"),
			os.Getenv("KIWICD_RUNNER_NAME"), splitLabels(os.Getenv("KIWICD_LABELS")), version)
		if err == nil {
			err = creds.Save(*config)
		}
	}
	if err != nil {
		return fmt.Errorf("loading registration: %w (run `kiwicd-agent register` first)", err)
	}
	oldNames, err := creds.AddAgentID(*config)
	if err != nil {
		slog.Warn("saving the registration's new agent ID failed; it's derived from the token, so the next start finds the same", "config", *config, "err", err)
	}
	// Host jobs inherit the agent's environment: don't hand them a
	// registration token, which could register a runner that takes jobs.
	os.Unsetenv("KIWICD_REGISTRATION_TOKEN")
	a, err := agent.New(agent.Config{Creds: creds, WorkDir: *workDir, Capacity: *capacity, Version: version, ContainerDaemonSocket: *socket, ActionsURL: *actionsURL,
		JobImage: *jobImage, JobOptions: *jobOptions, JobBridgePrefix: *bridgePrefix, RemoveOldNames: oldNames})
	if err != nil {
		return err
	}
	return a.Run(ctx)
}
