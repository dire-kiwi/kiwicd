// Command kiwicd-secrets is the secrets service: it holds the private key
// that opens secrets the kiwicd server sealed to its public key, and opens
// them for agents whose jobs get them. Run it apart from the server, close
// to the runners:
//
//	kiwicd-secrets keygen --out secrets.key        # prints the public key for the server
//	kiwicd-secrets token deploy-runners            # prints a token for agents and its clients.json entry
//	kiwicd-secrets serve --server https://ci.example.com --key secrets.key --clients clients.json
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dire-kiwi/kiwicd/internal/sealed"
	"github.com/dire-kiwi/kiwicd/internal/secretsservice"
)

var version = "dev"

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func usage() {
	fmt.Fprintf(os.Stderr, `kiwicd-secrets %s

Usage:
  kiwicd-secrets keygen --out FILE
  kiwicd-secrets public-key --key FILE
  kiwicd-secrets token NAME [--allow PATTERN,...]
  kiwicd-secrets seal --public-key KEY [--repo OWNER/NAME [--environment NAME]] NAME < value
  kiwicd-secrets serve --server URL --key FILE[,OLD-FILE...] --clients FILE [--listen ADDR] [--tls-cert FILE --tls-key FILE]
  kiwicd-secrets version
`, version)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	var err error
	args := os.Args[2:]
	switch os.Args[1] {
	case "keygen":
		err = keygen(args)
	case "public-key":
		err = publicKey(args)
	case "token":
		err = token(args)
	case "seal":
		err = seal(args)
	case "serve":
		err = serve(args)
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

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fs.String("out", "", "file to write the private key to (must not exist)")
	_ = fs.Parse(args)
	if *out == "" {
		return errors.New("--out is required")
	}
	k, err := sealed.GenerateKey()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(k.String() + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Wrote the private key to %s. Back it up: secrets sealed to it can't be read without it.\n", *out)
	fmt.Fprintf(os.Stderr, "Start the kiwicd server with this public key (key ID %s):\n\n", k.Public().ID())
	fmt.Printf("KIWICD_SECRETS_PUBLIC_KEY=%s\n", k.Public())
	return nil
}

func loadKeys(files string) ([]*sealed.PrivateKey, error) {
	var keys []*sealed.PrivateKey
	for _, f := range strings.Split(files, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		k, err := sealed.ParsePrivateKey(string(b))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil, errors.New("--key is required")
	}
	return keys, nil
}

func publicKey(args []string) error {
	fs := flag.NewFlagSet("public-key", flag.ExitOnError)
	key := fs.String("key", env("KIWICD_SECRETS_KEY", ""), "private key file")
	_ = fs.Parse(args)
	keys, err := loadKeys(*key)
	if err != nil {
		return err
	}
	fmt.Println(keys[0].Public())
	return nil
}

func token(args []string) error {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	allow := fs.String("allow", "*/*", "comma-separated OWNER/REPO[@ENVIRONMENT] patterns the client may open secrets of")
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: kiwicd-secrets token NAME [--allow PATTERN,...]")
	}
	name := args[0]
	_ = fs.Parse(args[1:])
	t := make([]byte, 32)
	if _, err := rand.Read(t); err != nil {
		return err
	}
	tok := fmt.Sprintf("%x", t)
	c := secretsservice.Client{Name: name, TokenSHA256: secretsservice.HashToken(tok)}
	for _, p := range strings.Split(*allow, ",") {
		if p = strings.TrimSpace(p); p != "" {
			c.Allow = append(c.Allow, p)
		}
	}
	if err := secretsservice.CheckClients([]secretsservice.Client{c}); err != nil {
		return err
	}
	entry, _ := json.MarshalIndent(c, "    ", "  ")
	fmt.Fprintf(os.Stderr, "Give the agents this token (KIWICD_SECRETS_TOKEN, or a file for --secrets-token-file):\n\n")
	fmt.Println(tok)
	fmt.Fprintf(os.Stderr, "\nand add the client to the service's clients file:\n\n  {\"clients\": [\n    %s\n  ]}\n", entry)
	return nil
}

func seal(args []string) error {
	fs := flag.NewFlagSet("seal", flag.ExitOnError)
	pub := fs.String("public-key", env("KIWICD_SECRETS_PUBLIC_KEY", ""), "the service's public key")
	repo := fs.String("repo", "", "the secret's repository (none for a global secret)")
	environment := fs.String("environment", "", "the secret's environment")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: kiwicd-secrets seal --public-key KEY [--repo OWNER/NAME [--environment NAME]] NAME < value")
	}
	if *environment != "" && *repo == "" {
		return errors.New("--environment needs --repo")
	}
	k, err := sealed.ParsePublicKey(*pub)
	if err != nil {
		return err
	}
	value, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	name := fs.Arg(0)
	v, err := k.Seal(sealed.Scope{Repository: *repo, Environment: *environment, Name: name}, string(value))
	if err != nil {
		return err
	}
	// Ready for PUT /api/secrets?repo=...&environment=...
	return json.NewEncoder(os.Stdout).Encode(map[string]string{"name": name, "encrypted_value": v})
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", env("KIWICD_SECRETS_LISTEN", ":8091"), "address to listen on")
	server := fs.String("server", env("KIWICD_URL", ""), "the kiwicd server's URL, which the service asks about jobs")
	key := fs.String("key", env("KIWICD_SECRETS_KEY", ""), "private key file; add older keys after a comma while secrets sealed to them remain")
	clientsFile := fs.String("clients", env("KIWICD_SECRETS_CLIENTS", ""), "JSON file listing the clients (agents) and what they may open")
	tlsCert := fs.String("tls-cert", env("KIWICD_SECRETS_TLS_CERT", ""), "TLS certificate file, to serve HTTPS")
	tlsKey := fs.String("tls-key", env("KIWICD_SECRETS_TLS_KEY", ""), "TLS key file")
	_ = fs.Parse(args)
	keys, err := loadKeys(*key)
	if err != nil {
		return err
	}
	if *clientsFile == "" {
		return errors.New("--clients is required")
	}
	b, err := os.ReadFile(*clientsFile)
	if err != nil {
		return err
	}
	var clients secretsservice.Clients
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&clients); err != nil {
		return fmt.Errorf("%s: %w", *clientsFile, err)
	}
	if !strings.HasPrefix(*server, "https://") {
		slog.Warn("the kiwicd server's URL is not https: job tokens and sealed secrets cross the network in the clear", "server", *server)
	}
	svc, err := secretsservice.New(secretsservice.Config{Server: *server, Keys: keys, Clients: clients.Clients})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hs := &http.Server{Addr: *listen, Handler: svc.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hs.Shutdown(shut)
	}()
	slog.Info("kiwicd-secrets listening", "addr", *listen, "server", *server, "key", keys[0].Public().ID(), "clients", len(clients.Clients), "tls", *tlsCert != "")
	if *tlsCert != "" {
		err = hs.ListenAndServeTLS(*tlsCert, *tlsKey)
	} else {
		err = hs.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
