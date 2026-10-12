// Package config loads and saves the list of DNS sources (provider accounts)
// to manage, as TOML.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Providers are the supported provider kinds.
var Providers = []string{"route53", "cloudflare", "namecheap", "godaddy", "ovh", "hetzner", "digitalocean", "googlecloud", "azure"}

// ProviderLabel is how each provider is shown in the UI.
var ProviderLabel = map[string]string{
	"route53":      "AWS Route53",
	"cloudflare":   "Cloudflare",
	"namecheap":    "Namecheap",
	"godaddy":      "GoDaddy",
	"ovh":          "OVH",
	"hetzner":      "Hetzner",
	"digitalocean": "DigitalOcean",
	"googlecloud":  "Google Cloud DNS",
	"azure":        "Azure DNS",
}

// Source is one provider account. Credentials not set here fall back to the
// provider's usual environment variables / profiles.
type Source struct {
	Name     string `toml:"name"`
	Provider string `toml:"provider"` // one of Providers

	// route53
	Profile         string `toml:"profile,omitempty"`
	Region          string `toml:"region,omitempty"`
	AccessKeyID     string `toml:"access_key_id,omitempty"`
	SecretAccessKey string `toml:"secret_access_key,omitempty"`
	SessionToken    string `toml:"session_token,omitempty"`

	// cloudflare, hetzner, digitalocean
	APIToken string `toml:"api_token,omitempty"`

	// namecheap (and godaddy's api_key)
	APIUser     string `toml:"api_user,omitempty"`
	APIKey      string `toml:"api_key,omitempty"`
	ClientIP    string `toml:"client_ip,omitempty"`
	APIEndpoint string `toml:"api_endpoint,omitempty"`

	// godaddy
	APISecret string `toml:"api_secret,omitempty"`

	// googlecloud
	Project         string `toml:"project,omitempty"`
	CredentialsFile string `toml:"credentials_file,omitempty"` // empty: application default credentials

	// azure
	SubscriptionID string `toml:"subscription_id,omitempty"`
	TenantID       string `toml:"tenant_id,omitempty"` // with client_id and client_secret, a service principal
	ClientID       string `toml:"client_id,omitempty"`
	ClientSecret   string `toml:"client_secret,omitempty"`

	// ovh
	Endpoint          string `toml:"endpoint,omitempty"` // ovh-eu (default), ovh-ca, ovh-us...
	ApplicationKey    string `toml:"application_key,omitempty"`
	ApplicationSecret string `toml:"application_secret,omitempty"`
	ConsumerKey       string `toml:"consumer_key,omitempty"`
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
	case "cloudflare", "hetzner", "digitalocean":
		if s.APIToken == "" {
			return errors.New("the API token is required")
		}
	case "namecheap":
		if s.APIUser == "" || s.APIKey == "" {
			return errors.New("the API user and API key are required")
		}
	case "godaddy":
		if s.APIKey == "" || s.APISecret == "" {
			return errors.New("the API key and API secret are required")
		}
	case "googlecloud":
		if s.Project == "" {
			return errors.New("the GCP project ID is required")
		}
	case "azure":
		if s.SubscriptionID == "" {
			return errors.New("the subscription ID is required")
		}
		if n := btoi(s.TenantID != "") + btoi(s.ClientID != "") + btoi(s.ClientSecret != ""); n != 0 && n != 3 {
			return errors.New("set the tenant ID, client ID and client secret together, or none of them")
		}
	case "ovh":
		if s.ApplicationKey == "" || s.ApplicationSecret == "" || s.ConsumerKey == "" {
			return errors.New("the application key, application secret and consumer key are required")
		}
	default:
		return fmt.Errorf("unknown provider %q (%s)", s.Provider, strings.Join(Providers, ", "))
	}
	return nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
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
