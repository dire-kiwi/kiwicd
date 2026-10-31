package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Config is what kiwicd login saves: the server and an API token for it.
// KIWICD_URL and KIWICD_API_TOKEN override it. (Not KIWICD_TOKEN: jobs get
// that as a secret, and it only fetches repositories.)
type Config struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// configPath is $KIWICD_CONFIG, or kiwicd/cli.json in the user's config
// directory.
func configPath() (string, error) {
	if p := os.Getenv("KIWICD_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "kiwicd", "cli.json"), nil
}

// savedConfig reads the saved config, if any.
func savedConfig() (Config, error) {
	var c Config
	p, err := configPath()
	if err != nil {
		return c, err
	}
	b, err := os.ReadFile(p)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return c, err
	default:
		if err := json.Unmarshal(b, &c); err != nil {
			return c, errors.New(p + ": " + err.Error())
		}
	}
	return c, nil
}

// loadConfig reads the saved config, if any, and applies the environment.
func loadConfig() (Config, error) {
	c, err := savedConfig()
	if err != nil {
		return c, err
	}
	if v := strings.TrimRight(os.Getenv("KIWICD_URL"), "/"); v != "" {
		if v != strings.TrimRight(c.URL, "/") {
			// The saved token is for another server.
			c.Token = ""
		}
		c.URL = v
	}
	if v := os.Getenv("KIWICD_API_TOKEN"); v != "" {
		c.Token = v
	}
	return c, nil
}

func saveConfig(c Config) (string, error) {
	p, err := configPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return "", err
	}
	return p, os.Rename(tmp, p)
}

func removeConfig() error {
	p, err := configPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
