// Package config loads the list of DNS sources (provider accounts) to manage.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Source is one provider account. Credentials not set here fall back to the
// provider's usual environment variables / profiles.
type Source struct {
	Name     string `json:"name"`
	Provider string `json:"provider"` // "route53" or "namecheap"

	// route53
	Profile         string `json:"profile,omitempty"`
	Region          string `json:"region,omitempty"`
	AccessKeyID     string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
	SessionToken    string `json:"session_token,omitempty"`

	// namecheap
	APIUser     string `json:"api_user,omitempty"`
	APIKey      string `json:"api_key,omitempty"`
	ClientIP    string `json:"client_ip,omitempty"`
	APIEndpoint string `json:"api_endpoint,omitempty"`
}

type Config struct {
	Sources []Source `json:"sources"`
}

func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "dnszoner", "config.json")
}

// Load reads the config file. A missing file is not an error: it yields an
// empty config so the cached data can still be browsed.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	seen := map[string]bool{}
	for i, s := range c.Sources {
		if s.Name == "" {
			return nil, fmt.Errorf("%s: source #%d has no name", path, i+1)
		}
		if seen[s.Name] {
			return nil, fmt.Errorf("%s: duplicate source name %q", path, s.Name)
		}
		seen[s.Name] = true
		switch s.Provider {
		case "route53", "namecheap":
		default:
			return nil, fmt.Errorf("%s: source %q has unknown provider %q", path, s.Name, s.Provider)
		}
	}
	return &c, nil
}
