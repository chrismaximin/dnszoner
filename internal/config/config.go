// Package config loads and saves the list of DNS sources (provider accounts)
// to manage, as TOML.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Providers are the supported provider kinds.
var Providers = []string{"route53", "namecheap"}

// ProviderLabel is how each provider is shown in the UI.
var ProviderLabel = map[string]string{"route53": "AWS Route53", "namecheap": "Namecheap"}

// Source is one provider account. Credentials not set here fall back to the
// provider's usual environment variables / profiles.
type Source struct {
	Name     string `toml:"name"`
	Provider string `toml:"provider"` // "route53" or "namecheap"

	// route53
	Profile         string `toml:"profile,omitempty"`
	Region          string `toml:"region,omitempty"`
	AccessKeyID     string `toml:"access_key_id,omitempty"`
	SecretAccessKey string `toml:"secret_access_key,omitempty"`
	SessionToken    string `toml:"session_token,omitempty"`

	// namecheap
	APIUser     string `toml:"api_user,omitempty"`
	APIKey      string `toml:"api_key,omitempty"`
	ClientIP    string `toml:"client_ip,omitempty"`
	APIEndpoint string `toml:"api_endpoint,omitempty"`
}

type Config struct {
	Sources []Source `toml:"sources"`
}

func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "dnszoner", "config.toml")
}

// Load reads the config file. A missing file is not an error: it yields an
// empty config so the cached data can still be browsed.
func Load(path string) (*Config, error) {
	var c Config
	md, err := toml.DecodeFile(path, &c)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if undec := md.Undecoded(); len(undec) > 0 {
		return nil, fmt.Errorf("%s: unknown setting %q", path, undec[0].String())
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// Validate checks every source and that names are unique.
func (c *Config) Validate() error {
	seen := map[string]bool{}
	for i, s := range c.Sources {
		if err := s.Validate(); err != nil {
			if s.Name == "" {
				return fmt.Errorf("source #%d: %w", i+1, err)
			}
			return fmt.Errorf("source %q: %w", s.Name, err)
		}
		if seen[s.Name] {
			return fmt.Errorf("duplicate source name %q", s.Name)
		}
		seen[s.Name] = true
	}
	return nil
}

// Validate checks one source on its own.
func (s Source) Validate() error {
	if s.Name == "" {
		return errors.New("name is required")
	}
	switch s.Provider {
	case "route53":
		if (s.AccessKeyID == "") != (s.SecretAccessKey == "") {
			return errors.New("set both the access key ID and the secret access key, or neither")
		}
	case "namecheap":
		if s.APIUser == "" || s.APIKey == "" {
			return errors.New("the API user and API key are required")
		}
	default:
		return fmt.Errorf("unknown provider %q (route53 or namecheap)", s.Provider)
	}
	return nil
}

const header = `# dnszoner accounts. Edit here or from the app (ctrl+o).
# This file holds credentials: keep it private.

`

// Save writes the config atomically, readable only by the user.
func Save(path string, c *Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.WriteString(header)
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(c); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
