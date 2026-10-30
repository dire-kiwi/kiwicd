// Command kiwicd-server runs the kiwicd server: repositories, workflow runs,
// the agent API and the web UI.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dire-kiwi/kiwicd/internal/server"
	"github.com/dire-kiwi/kiwicd/web"
)

var version = "dev"

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", key, err)
		os.Exit(2)
	}
	return n
}

func main() {
	listen := flag.String("listen", env("KIWICD_LISTEN", ":8080"), "address to listen on")
	data := flag.String("data", env("KIWICD_DATA", "./data"), "data directory (database, repositories)")
	publicURL := flag.String("public-url", env("KIWICD_PUBLIC_URL", ""), "URL agents and browsers use to reach the server")
	retention := flag.Int("retention-days", int(envInt("KIWICD_RETENTION_DAYS", 30)), "days to keep completed runs and their logs (0 keeps them forever)")
	poll := flag.Duration("poll-interval", 60*time.Second, "how often mirrored repositories are fetched")
	maxLogLines := flag.Int64("max-log-lines", envInt("KIWICD_MAX_LOG_LINES", 1_000_000), "most log lines stored per job (-1: no limit)")
	maxLogBytes := flag.Int64("max-log-bytes", envInt("KIWICD_MAX_LOG_BYTES", 100<<20), "most log bytes stored per job (-1: no limit)")
	clientIPHeader := flag.String("client-ip-header", env("KIWICD_CLIENT_IP_HEADER", ""), "header a reverse proxy puts the client's address in (X-Forwarded-For, CF-Connecting-IP); only set it if every request comes through that proxy")
	var reserved []string
	reserved = append(reserved, strings.Fields(os.Getenv("KIWICD_RESERVED_LABELS"))...)
	flag.Func("reserve-label", "keep a runner label for some repositories: LABEL=OWNER/REPO[@REF][,...] (repeatable; KIWICD_RESERVED_LABELS holds them space-separated)", func(v string) error {
		reserved = append(reserved, v)
		return nil
	})
	var protected []string
	protected = append(protected, strings.Fields(os.Getenv("KIWICD_PROTECTED_LABELS"))...)
	flag.Func("protected-label", "runners with this label take only jobs from protected refs (repeatable; KIWICD_PROTECTED_LABELS holds them space-separated)", func(v string) error {
		protected = append(protected, v)
		return nil
	})
	githubURL := flag.String("github-url", env("KIWICD_GITHUB_URL", "https://github.com"), "GitHub's web URL, for the GitHub App and signing in with GitHub (GitHub Enterprise: https://HOST)")
	githubAPIURL := flag.String("github-api-url", env("KIWICD_GITHUB_API_URL", "https://api.github.com"), "GitHub's API URL (GitHub Enterprise: https://HOST/api/v3)")
	secretsPublicKey := flag.String("secrets-public-key", env("KIWICD_SECRETS_PUBLIC_KEY", ""), "secrets service's public key (kiwicd-secrets keygen): seal secrets to it, so that only the service can read them")
	debug := flag.Bool("debug", os.Getenv("KIWICD_DEBUG") != "", "debug logging")
	showVersion := flag.Bool("version", false, "print the version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	srv, err := server.New(server.Config{
		Listen:            *listen,
		DataDir:           *data,
		AdminPassword:     os.Getenv("KIWICD_ADMIN_PASSWORD"),
		PublicURL:         *publicURL,
		PollInterval:      *poll,
		RetentionDays:     *retention,
		RegistrationToken: os.Getenv("KIWICD_REGISTRATION_TOKEN"),
		ClientIPHeader:    *clientIPHeader,
		MaxLogLines:       *maxLogLines,
		MaxLogBytes:       *maxLogBytes,
		ReservedLabels:    reserved,
		ProtectedLabels:   protected,
		SecretKey:         os.Getenv("KIWICD_SECRET_KEY"),
		SecretsPublicKey:  *secretsPublicKey,
		GitHubURL:         *githubURL,
		GitHubAPIURL:      *githubAPIURL,
		Version:           version,
		Web:               web.FS(),
	})
	if err != nil {
		slog.Error("startup", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx); err != nil {
		slog.Error("server", "err", err)
		os.Exit(1)
	}
}
