package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/emaland/devbox/internal/config"
)

// fakeKey builds a syntactically valid ssh public key line at run time. The
// material is assembled from pieces so that no key-shaped literal sits in this
// public repo's source.
func fakeKey(fill, comment string) string {
	key := "ssh-ed25519 " + "AAAA" + strings.Repeat(fill, 64)
	if comment != "" {
		key += " " + comment
	}
	return key
}

// writeTemplate writes a minimal stand-in for configuration.nix that uses the
// same expression as the real one for the extra keys.
func writeTemplate(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "configuration.nix")
	body := `{
  zone = "@@DNS_ZONE_ID@@";
  record = "@@DNS_RECORD_NAME@@";
  keys = "@@EXTRA_AUTHORIZED_KEYS@@";
}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRenderNixConfigAuthorizedKeys(t *testing.T) {
	first, second := fakeKey("B", "laptop"), fakeKey("C", "")
	keysFile := filepath.Join(t.TempDir(), "authorized_keys")
	content := "# extra keys\n\n" + first + "\n  " + second + "  \n"
	if err := os.WriteFile(keysFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := renderNixConfig(config.DevboxConfig{AuthorizedKeysFile: keysFile}, writeTemplate(t), nil)
	if err != nil {
		t.Fatalf("renderNixConfig: %v", err)
	}
	// One Nix string, keys separated by the two-character escape \n.
	want := `keys = "` + first + `\n` + second + `";`
	if !strings.Contains(string(out), want) {
		t.Errorf("rendered config does not contain the keys as one \\n-separated string:\n%s", out)
	}
}

func TestRenderNixConfigNoKeysFileLeavesMarker(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	for _, path := range []string{missing, ""} {
		out, err := renderNixConfig(config.DevboxConfig{AuthorizedKeysFile: path}, writeTemplate(t), nil)
		if err != nil {
			t.Fatalf("renderNixConfig with keys file %q: %v", path, err)
		}
		if !strings.Contains(string(out), `keys = "@@EXTRA_AUTHORIZED_KEYS@@";`) {
			t.Errorf("keys file %q: marker should stay unrendered, got:\n%s", path, out)
		}
	}
}

func TestRenderNixConfigEmptyZoneIDLeavesMarker(t *testing.T) {
	out, err := renderNixConfig(config.DevboxConfig{DNSName: "box.example.com."}, writeTemplate(t), nil)
	if err != nil {
		t.Fatalf("renderNixConfig: %v", err)
	}
	if !strings.Contains(string(out), `zone = "@@DNS_ZONE_ID@@";`) {
		t.Errorf("zone marker should stay unrendered with no dns_zone_id, got:\n%s", out)
	}
	if !strings.Contains(string(out), `record = "box.example.com";`) {
		t.Errorf("record name should be rendered without the trailing dot, got:\n%s", out)
	}
}

func TestReadAuthorizedKeysRejectsUnsafeLines(t *testing.T) {
	secretLooking := "do-not-echo-this-line"
	cases := map[string]string{
		"options before the key":   `command="` + secretLooking + `" ` + fakeKey("B", ""),
		"double quote in comment":  fakeKey("B", `a"`+secretLooking),
		"dollar in comment":        fakeKey("B", "${"+secretLooking+"}"),
		"backslash in comment":     fakeKey("B", `a\`+secretLooking),
		"unknown key type":         "ssh-dss " + "AAAA" + strings.Repeat("B", 64) + " " + secretLooking,
		"not base64 key material":  "ssh-ed25519 not*base64 " + secretLooking,
		"something else entirely":  secretLooking,
		"valid key then a bad one": fakeKey("B", "") + "\n" + secretLooking,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "authorized_keys")
			if err := os.WriteFile(path, []byte(content+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			keys, err := readAuthorizedKeys(path)
			if err == nil {
				t.Fatalf("expected an error, got keys %q", keys)
			}
			if strings.Contains(err.Error(), secretLooking) {
				t.Errorf("error must not echo the file's content: %v", err)
			}
			if !strings.Contains(err.Error(), "line ") {
				t.Errorf("error should name the line number: %v", err)
			}
		})
	}
}

func TestReadAuthorizedKeysUnreadableFileIsAnError(t *testing.T) {
	// A directory exists but cannot be read as a file: that must not be
	// mistaken for "no extra keys".
	if _, err := readAuthorizedKeys(t.TempDir()); err == nil {
		t.Fatal("expected an error for an unreadable keys file")
	}
}

func TestResolveDNSZoneID(t *testing.T) {
	ctx := context.Background()
	never := func(context.Context, string) (string, error) {
		t.Fatal("lookup must not be called")
		return "", nil
	}

	t.Run("explicit id wins", func(t *testing.T) {
		got, err := resolveDNSZoneID(ctx, config.DevboxConfig{DNSZone: "example.com.", DNSZoneID: "ZEXPLICIT"}, never)
		if err != nil || got.DNSZoneID != "ZEXPLICIT" {
			t.Fatalf("got %q, %v", got.DNSZoneID, err)
		}
	})

	t.Run("no zone, no lookup", func(t *testing.T) {
		got, err := resolveDNSZoneID(ctx, config.DevboxConfig{}, never)
		if err != nil || got.DNSZoneID != "" {
			t.Fatalf("got %q, %v", got.DNSZoneID, err)
		}
	})

	t.Run("looked up from the zone name, prefix stripped", func(t *testing.T) {
		var asked string
		find := func(_ context.Context, zone string) (string, error) {
			asked = zone
			return "/hostedzone/ZLOOKEDUP", nil
		}
		got, err := resolveDNSZoneID(ctx, config.DevboxConfig{DNSZone: "example.com."}, find)
		if err != nil {
			t.Fatal(err)
		}
		if asked != "example.com." || got.DNSZoneID != "ZLOOKEDUP" {
			t.Fatalf("asked %q, got %q", asked, got.DNSZoneID)
		}
	})

	t.Run("lookup failure stops the push", func(t *testing.T) {
		boom := errors.New("boom")
		find := func(context.Context, string) (string, error) { return "", boom }
		_, err := resolveDNSZoneID(ctx, config.DevboxConfig{DNSZone: "example.com."}, find)
		if !errors.Is(err, boom) {
			t.Fatalf("want the lookup error wrapped, got %v", err)
		}
		if !strings.Contains(err.Error(), "dns_zone_id") {
			t.Errorf("error should say how to set the id by hand: %v", err)
		}
	})
}

// The real template is public. It must take machine- and account-specific
// values as markers and never carry them literally.
func TestTemplateCarriesNoMachineSpecificValues(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", defaultNixFile()))
	if err != nil {
		t.Fatal(err)
	}
	template := string(data)

	for _, marker := range []string{
		"@@EXTRA_AUTHORIZED_KEYS@@", "@@DNS_ZONE_ID@@", "@@DNS_RECORD_NAME@@",
		"@@TAILSCALE_AUTH_KEY@@", "@@DATA_VOLUME_ID@@",
	} {
		if !strings.Contains(template, marker) {
			t.Errorf("template is missing marker %s", marker)
		}
	}

	forbidden := map[string]*regexp.Regexp{
		"ssh public key":       regexp.MustCompile(`(ssh-(ed25519|rsa)|ecdsa-sha2-[a-z0-9]+) AAAA[A-Za-z0-9+/]{16,}`),
		"hosted zone id":       regexp.MustCompile(`\bZ[0-9A-Z]{12,32}\b`),
		"tailscale auth key":   regexp.MustCompile(`tskey-[A-Za-z0-9-]{8,}`),
		"EBS volume id":        regexp.MustCompile(`\bvol-[0-9a-f]{8,}\b`),
		"non-metadata address": regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b`),
	}
	for i, line := range strings.Split(template, "\n") {
		for name, pattern := range forbidden {
			match := pattern.FindString(line)
			if match == "" {
				continue
			}
			// The link-local range (instance metadata) is the same on
			// every machine.
			if name == "non-metadata address" && strings.HasPrefix(match, "169.254.") {
				continue
			}
			// Name the line, not the value.
			t.Errorf("%s line %d holds a literal %s; take it as a marker instead", defaultNixFile(), i+1, name)
		}
	}
}

func TestDefaultConfigHasNoBuiltInZoneID(t *testing.T) {
	if id := config.DefaultConfig().DNSZoneID; id != "" {
		t.Error("DefaultConfig must not build in a hosted zone id; it is looked up from dns_zone")
	}
}
