package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.toml")
	in := &Config{Sources: []Source{
		{Name: "AWS prod", Provider: "route53", Profile: "prod"},
		{Name: "Namecheap", Provider: "namecheap", APIUser: "me", APIKey: "secret"},
		{Name: "CF", Provider: "cloudflare", APIToken: "t"},
		{Name: "GD", Provider: "godaddy", APIKey: "k", APISecret: "s"},
		{Name: "OVH", Provider: "ovh", Endpoint: "ovh-ca", ApplicationKey: "a", ApplicationSecret: "b", ConsumerKey: "c"},
	}}
	if err := Save(path, in); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %v, want 0600", fi.Mode().Perm())
	}
	out, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Sources) != len(in.Sources) {
		t.Fatalf("round trip: got %+v", out.Sources)
	}
	for i := range in.Sources {
		if out.Sources[i] != in.Sources[i] {
			t.Errorf("round trip: got %+v, want %+v", out.Sources[i], in.Sources[i])
		}
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "[[sources]]") || strings.Contains(string(b), "access_key_id") {
		t.Errorf("unexpected TOML:\n%s", b)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	if c, err := Load(filepath.Join(dir, "missing.toml")); err != nil || len(c.Sources) != 0 {
		t.Fatalf("missing file: %+v, %v", c, err)
	}
	cases := map[string]string{
		"unknown provider": "[[sources]]\nname = \"x\"\nprovider = \"dyndns\"\n",
		"duplicate":        "[[sources]]\nname = \"x\"\nprovider = \"route53\"\n[[sources]]\nname = \"x\"\nprovider = \"route53\"\n",
		"typo":             "[[sources]]\nname = \"x\"\nprovider = \"route53\"\nprofil = \"p\"\n",
		"namecheap keys":   "[[sources]]\nname = \"x\"\nprovider = \"namecheap\"\napi_user = \"me\"\n",
		"cloudflare token": "[[sources]]\nname = \"x\"\nprovider = \"cloudflare\"\n",
		"godaddy secret":   "[[sources]]\nname = \"x\"\nprovider = \"godaddy\"\napi_key = \"k\"\n",
		"ovh consumer key": "[[sources]]\nname = \"x\"\nprovider = \"ovh\"\napplication_key = \"a\"\napplication_secret = \"b\"\n",
	}
	for name, body := range cases {
		path := filepath.Join(dir, name+".toml")
		os.WriteFile(path, []byte(body), 0o600)
		if _, err := Load(path); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
