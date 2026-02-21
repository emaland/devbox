package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type DevboxConfig struct {
	DNSName          string `json:"dns_name"`
	DNSZone          string `json:"dns_zone"`
	SSHKeyName       string `json:"ssh_key_name"`
	SSHKeyPath       string `json:"ssh_key_path"`
	SSHUser          string `json:"ssh_user"`
	SecurityGroup    string `json:"security_group"`
	IAMProfile       string `json:"iam_profile"`
	DefaultAZ        string `json:"default_az"`
	DefaultType      string `json:"default_type"`
	DefaultMaxPrice  string `json:"default_max_price"`
	SpawnName        string `json:"spawn_name"`
	NixOSAMIOwner   string `json:"nixos_ami_owner"`
	NixOSAMIPattern string `json:"nixos_ami_pattern"`
}

func LoadConfig() (DevboxConfig, error) {
	cfg := DevboxConfig{
		DNSName:          "dev.frob.io",
		DNSZone:          "frob.io.",
		SSHKeyName:       "dev-boxes",
		SSHKeyPath:       "~/.ssh/dev-boxes.pem",
		SSHUser:          "emaland",
		SecurityGroup:    "dev-instance",
		IAMProfile:       "dev-workstation-profile",
		DefaultAZ:        "us-east-2a",
		DefaultType:      "m6i.4xlarge",
		DefaultMaxPrice:  "2.00",
		SpawnName:        "dev-workstation-tmp",
		NixOSAMIOwner:   "427812963091",
		NixOSAMIPattern: "nixos/24.11*",
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return cfg, nil
	}

	path := filepath.Join(home, ".config", "devbox", "default.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("reading config %s: %w", path, err)
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing config %s: %w", path, err)
	}
	return cfg, nil
}

func (c DevboxConfig) ResolveSSHKeyPath() string {
	if strings.HasPrefix(c.SSHKeyPath, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, c.SSHKeyPath[2:])
		}
	}
	return c.SSHKeyPath
}
