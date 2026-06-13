package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
)

type DevboxConfig struct {
	DNSName          string `json:"dns_name"`
	DNSZone          string `json:"dns_zone"`
	DNSZoneID        string `json:"dns_zone_id"`
	SSHKeyName       string `json:"ssh_key_name"`
	SSHKeyPath       string `json:"ssh_key_path"`
	SSHUser          string `json:"ssh_user"`
	SecurityGroup    string `json:"security_group"`
	IAMProfile       string `json:"iam_profile"`
	DefaultAZ        string `json:"default_az"`
	DefaultType      string `json:"default_type"`
	DefaultMaxPrice  string `json:"default_max_price"`
	SpawnName        string `json:"spawn_name"`
	NixOSAMIOwner    string `json:"nixos_ami_owner"`
	NixOSAMIPattern  string `json:"nixos_ami_pattern"`
	TailscaleAuthKey string `json:"tailscale_auth_key"`
	DataVolumeName   string `json:"data_volume_name"`
	DataVolumeSize   int    `json:"data_volume_size"`
}

// secretKeys are config keys whose values should be masked when displayed.
var secretKeys = map[string]bool{
	"tailscale_auth_key": true,
}

// DefaultConfig returns the built-in defaults used when a field is absent from
// the config file.
func DefaultConfig() DevboxConfig {
	return DevboxConfig{
		DNSName:         "dev.frob.io",
		DNSZone:         "frob.io.",
		DNSZoneID:       "Z09070421BNE0435R08B2",
		SSHKeyName:      "dev-boxes",
		SSHKeyPath:      "~/.ssh/dev-boxes.pem",
		SSHUser:         "emaland",
		SecurityGroup:   "dev-instance",
		IAMProfile:      "dev-workstation-profile",
		DefaultAZ:       "us-east-2a",
		DefaultType:     "m6i.4xlarge",
		DefaultMaxPrice: "2.00",
		SpawnName:       "dev-workstation-tmp",
		NixOSAMIOwner:   "427812963091",
		NixOSAMIPattern: "nixos/*",
		DataVolumeName:  "dev-data-volume",
		DataVolumeSize:  512,
	}
}

// ConfigPath returns the path to the devbox config file.
func ConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "devbox", "default.json"), nil
}

func LoadConfig() (DevboxConfig, error) {
	cfg := DefaultConfig()

	path, err := ConfigPath()
	if err != nil {
		return cfg, nil
	}

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

// Entry is a single config key/value pair plus where the value came from.
type Entry struct {
	Key    string
	Value  string
	Source string // "file" or "default"
	Secret bool
}

// Entries returns the resolved config as ordered key/value pairs, annotated
// with whether each value was set in the file or is a built-in default.
func (c DevboxConfig) Entries() ([]Entry, error) {
	fileKeys, err := FileKeys()
	if err != nil {
		return nil, err
	}
	v := reflect.ValueOf(c)
	t := v.Type()
	entries := make([]Entry, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		key := jsonTag(t.Field(i))
		source := "default"
		if fileKeys[key] {
			source = "file"
		}
		entries = append(entries, Entry{
			Key:    key,
			Value:  fmt.Sprintf("%v", v.Field(i).Interface()),
			Source: source,
			Secret: secretKeys[key],
		})
	}
	return entries, nil
}

// FileKeys returns the set of json keys explicitly present in the config file.
func FileKeys() (map[string]bool, error) {
	keys := map[string]bool{}
	path, err := ConfigPath()
	if err != nil {
		return keys, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return keys, nil
		}
		return keys, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return keys, fmt.Errorf("parsing config %s: %w", path, err)
	}
	for k := range raw {
		keys[k] = true
	}
	return keys, nil
}

// Keys returns the valid config keys in struct order.
func Keys() []string {
	t := reflect.TypeFor[DevboxConfig]()
	keys := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		keys = append(keys, jsonTag(t.Field(i)))
	}
	return keys
}

// Set writes a single key/value into the config file, preserving any other
// keys already present. Numeric fields are stored as JSON numbers.
func Set(key, value string) error {
	kind, ok := fieldKind(key)
	if !ok {
		return fmt.Errorf("unknown config key %q (valid keys: %s)", key, strings.Join(Keys(), ", "))
	}

	path, err := ConfigPath()
	if err != nil {
		return err
	}

	raw := map[string]json.RawMessage{}
	if data, err := os.ReadFile(path); err == nil {
		// Refuse to rewrite a malformed file — otherwise we'd silently drop
		// every other key the user had set (and the recoverable original).
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("refusing to overwrite malformed config %s (fix or remove it first): %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("reading config %s: %w", path, err)
	}

	var encoded []byte
	switch kind {
	case reflect.Int, reflect.Int32, reflect.Int64:
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("config key %q expects an integer: %w", key, err)
		}
		if n <= 0 {
			return fmt.Errorf("config key %q must be a positive integer, got %d", key, n)
		}
		encoded, _ = json.Marshal(n)
	default:
		encoded, _ = json.Marshal(value)
	}
	raw[key] = encoded

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

func fieldKind(jsonKey string) (reflect.Kind, bool) {
	t := reflect.TypeFor[DevboxConfig]()
	for i := 0; i < t.NumField(); i++ {
		if jsonTag(t.Field(i)) == jsonKey {
			return t.Field(i).Type.Kind(), true
		}
	}
	return reflect.Invalid, false
}

func jsonTag(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" {
		return f.Name
	}
	return strings.Split(tag, ",")[0]
}
