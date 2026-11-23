package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, ".config", "devbox")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := `{"dns_name":"custom.example.com","default_type":"c5.xlarge"}`
	if err := os.WriteFile(filepath.Join(cfgDir, "default.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	// Override HOME so LoadConfig reads our temp file.
	t.Setenv("HOME", dir)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DNSName != "custom.example.com" {
		t.Errorf("DNSName = %q, want %q", cfg.DNSName, "custom.example.com")
	}
	if cfg.DefaultType != "c5.xlarge" {
		t.Errorf("DefaultType = %q, want %q", cfg.DefaultType, "c5.xlarge")
	}
	// Non-overridden fields keep defaults.
	if cfg.SSHUser != "emaland" {
		t.Errorf("SSHUser = %q, want default %q", cfg.SSHUser, "emaland")
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DNSName != "dev.frob.io" {
		t.Errorf("default DNSName = %q, want %q", cfg.DNSName, "dev.frob.io")
	}
}

func TestResolveSSHKeyPath(t *testing.T) {
	cfg := DevboxConfig{SSHKeyPath: "~/.ssh/test.pem"}
	got := cfg.ResolveSSHKeyPath()
	if strings.HasPrefix(got, "~") {
		t.Errorf("ResolveSSHKeyPath still starts with ~: %s", got)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".ssh", "test.pem")
	if got != want {
		t.Errorf("ResolveSSHKeyPath = %q, want %q", got, want)
	}
}

func TestLoadConfigBadJSON(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, ".config", "devbox")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "default.json"), []byte("{bad json"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for bad JSON")
	}
}

// verify our test devbox config produces valid JSON round-trip
func TestDevboxConfigJSON(t *testing.T) {
	cfg := DevboxConfig{
		DNSName:          "test.example.com",
		DNSZone:          "example.com.",
		SSHKeyName:       "test-key",
		SSHKeyPath:       "~/.ssh/test.pem",
		SSHUser:          "testuser",
		SecurityGroup:    "test-sg",
		IAMProfile:       "test-profile",
		DefaultAZ:        "us-east-1a",
		DefaultType:      "t2.micro",
		DefaultMaxPrice:  "0.50",
		SpawnName:        "test-spawn",
		NixOSAMIOwner:   "123456789012",
		NixOSAMIPattern: "test-ami*",
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var parsed DevboxConfig
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.DNSName != cfg.DNSName {
		t.Errorf("DNSName = %q, want %q", parsed.DNSName, cfg.DNSName)
	}
}

func TestResolveAuthorizedKeysFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cases := map[string]string{
		"~/x/keys":      filepath.Join(home, "x", "keys"),
		"/abs/keys":     "/abs/keys",
		"relative/keys": "relative/keys",
		"":              "",
	}
	for in, want := range cases {
		if got := (DevboxConfig{AuthorizedKeysFile: in}).ResolveAuthorizedKeysFile(); got != want {
			t.Errorf("ResolveAuthorizedKeysFile(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAuthorizedKeysFileConfigKeyAndDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if want := "~/.config/devbox/authorized_keys"; cfg.AuthorizedKeysFile != want {
		t.Errorf("default AuthorizedKeysFile = %q, want %q", cfg.AuthorizedKeysFile, want)
	}

	dir := filepath.Join(home, ".config", "devbox")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "default.json"), []byte(`{"authorized_keys_file": "/elsewhere/keys"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthorizedKeysFile != "/elsewhere/keys" {
		t.Errorf("authorized_keys_file from the config file = %q", cfg.AuthorizedKeysFile)
	}
}
