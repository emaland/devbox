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
