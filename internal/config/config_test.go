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
	if len(out.Sources) != 2 || out.Sources[0] != in.Sources[0] || out.Sources[1] != in.Sources[1] {
		t.Errorf("round trip: got %+v", out.Sources)
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
		"unknown provider": "[[sources]]\nname = \"x\"\nprovider = \"godaddy\"\n",
		"duplicate":        "[[sources]]\nname = \"x\"\nprovider = \"route53\"\n[[sources]]\nname = \"x\"\nprovider = \"route53\"\n",
		"typo":             "[[sources]]\nname = \"x\"\nprovider = \"route53\"\nprofil = \"p\"\n",
		"namecheap keys":   "[[sources]]\nname = \"x\"\nprovider = \"namecheap\"\napi_user = \"me\"\n",
	}
	for name, body := range cases {
		path := filepath.Join(dir, name+".toml")
		os.WriteFile(path, []byte(body), 0o600)
		if _, err := Load(path); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
