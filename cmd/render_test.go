package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/emaland/devbox/internal/config"
)

// No test in this package may reach Route 53 for a zone id by accident: the
// command-level lookup is replaced for the whole test binary. Tests that care
// about the lookup pass their own zoneFinder.
//
// Commands also check that the template is readable before they start. Tests
// run from this package's directory, where the default relative path does not
// exist, so they get a stand-in for the copy main.go embeds.
func init() {
	findHostedZoneID = func(context.Context, string) (string, error) {
		return "/hostedzone/ZTESTBINARY", nil
	}
	EmbeddedNixConfig = []byte("{ }\n")
}

// realTemplate is the template this repo ships, relative to this package.
func realTemplate() string { return filepath.Join("..", defaultNixFile()) }

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

// writeTemplate writes a minimal stand-in for configuration.nix: one line per
// marker, each inside a double-quoted string as in the real template. The
// real template's expressions are covered by the tests further down that
// evaluate them.
func writeTemplate(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "configuration.nix")
	body := `{
  zone = "@@DNS_ZONE_ID@@";
  record = "@@DNS_RECORD_NAME@@";
  keys = "@@EXTRA_AUTHORIZED_KEYS@@";
  tailscale = "@@TAILSCALE_AUTH_KEY@@";
}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// A main file is pushed together with the tree beside it; give the
	// stand-in the real install script and a layout version.
	tree := filepath.Join(filepath.Dir(path), "devbox")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(filepath.Join("..", "terraform", "devbox", "install_set.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"install_set.sh": script, "layout-version": []byte("1\n")} {
		if err := os.WriteFile(filepath.Join(tree, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
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
	onlyComments := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(onlyComments, []byte("# nothing yet\n\n   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{missing, "", onlyComments} {
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
		// Nix reads a carriage return inside a string as a line break, so
		// text after one would become a second, unchecked entry.
		"carriage return hides a second entry": fakeKey("B", "laptop") + "\rcert-authority " + fakeKey("C", secretLooking),
		"carriage-return-only line endings":    "# header\r" + fakeKey("B", "") + "\r" + secretLooking + "\r",
		"other control character in comment":   fakeKey("B", "a\x0b"+secretLooking),
		"non-ASCII space in comment":           fakeKey("B", "a\u00a0"+secretLooking),
		"tab instead of space":                 strings.Replace(fakeKey("B", secretLooking), " ", "\t", 1),
		"marker text in comment":               fakeKey("B", "@@TAILSCALE_AUTH_KEY@@"+secretLooking),
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
	data, err := os.ReadFile(realTemplate())
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

func TestReadAuthorizedKeysAcceptsValidKeys(t *testing.T) {
	material := "AAAA" + strings.Repeat("B", 60)
	opensshSuffix := "@" + "openssh.com"
	want := []string{
		"ssh-ed25519 " + material,
		"ssh-rsa " + material + "==",
		"ecdsa-sha2-nistp256 " + material + "=",
		"ecdsa-sha2-nistp384 " + material,
		"ecdsa-sha2-nistp521 " + material,
		// The security-key type names end in an OpenSSH domain suffix. It is
		// kept in a variable so the names do not read as email addresses to
		// the repository's public-content checks.
		"sk-ssh-ed25519" + opensshSuffix + " " + material,
		"sk-ecdsa-sha2-nistp256" + opensshSuffix + " " + material,
		"ssh-ed25519 " + material + " user@host work laptop (2nd), it's fine",
	}
	for name, ending := range map[string]string{"LF": "\n", "CRLF": "\r\n"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "authorized_keys")
			if err := os.WriteFile(path, []byte(strings.Join(want, ending)+ending), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := readAuthorizedKeys(path)
			if err != nil {
				t.Fatalf("readAuthorizedKeys: %v", err)
			}
			if strings.Join(got, "|") != strings.Join(want, "|") {
				t.Errorf("got %d keys %q, want %d", len(got), got, len(want))
			}
		})
	}
}

// A value is substituted once and never rescanned, so marker text that
// arrives inside one value cannot pull another value (here a secret) in.
func TestRenderNixConfigDoesNotRescanSubstitutedValues(t *testing.T) {
	dcfg := config.DevboxConfig{
		DNSName:          "@@TAILSCALE_AUTH_KEY@@.example.com",
		DNSZoneID:        "@@TAILSCALE_AUTH_KEY@@",
		TailscaleAuthKey: "canary-secret",
	}
	// Map iteration order is random; repeat so either order would show.
	for i := 0; i < 50; i++ {
		out, err := renderNixConfig(dcfg, writeTemplate(t), nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(string(out), "canary-secret"); got != 1 {
			t.Fatalf("secret rendered %d times, want exactly once (its own marker):\n%s", got, out)
		}
	}
}

func TestRenderNixConfigExpandsHomeInDefaultKeysFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "devbox")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	key := fakeKey("B", "tablet")
	if err := os.WriteFile(filepath.Join(dir, "authorized_keys"), []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dcfg := config.DevboxConfig{AuthorizedKeysFile: config.DefaultConfig().AuthorizedKeysFile}
	out, err := renderNixConfig(dcfg, writeTemplate(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `keys = "`+key+`";`) {
		t.Errorf("key from the default keys file under HOME was not rendered:\n%s", out)
	}
}

func TestRenderForPushLooksUpZoneID(t *testing.T) {
	ctx := context.Background()
	dcfg := config.DevboxConfig{DNSZone: "example.com.", DNSName: "box.example.com"}

	found := func(context.Context, string) (string, error) { return "/hostedzone/ZLOOKEDUP", nil }
	out, err := renderForPush(ctx, dcfg, writeTemplate(t), "", found)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `zone = "ZLOOKEDUP";`) {
		t.Errorf("pushed config does not carry the looked-up zone id:\n%s", out)
	}

	boom := errors.New("boom")
	failing := func(context.Context, string) (string, error) { return "", boom }
	out, err = renderForPush(ctx, dcfg, writeTemplate(t), "", failing)
	if !errors.Is(err, boom) || out != nil {
		t.Errorf("a failed lookup must stop the push with no output; got %d bytes, err %v", len(out), err)
	}
}

func TestPreflightNixConfig(t *testing.T) {
	ctx := context.Background()
	found := func(context.Context, string) (string, error) { return "/hostedzone/ZLOOKEDUP", nil }

	t.Run("returns the config with the zone id resolved", func(t *testing.T) {
		got, err := preflightNixConfig(ctx, config.DevboxConfig{DNSZone: "example.com."}, writeTemplate(t), found)
		if err != nil || got.DNSZoneID != "ZLOOKEDUP" {
			t.Fatalf("got %q, %v", got.DNSZoneID, err)
		}
	})

	t.Run("a bad keys file stops before anything else happens", func(t *testing.T) {
		keysFile := filepath.Join(t.TempDir(), "authorized_keys")
		if err := os.WriteFile(keysFile, []byte("not a key\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		dcfg := config.DevboxConfig{DNSZone: "example.com.", AuthorizedKeysFile: keysFile}
		if _, err := preflightNixConfig(ctx, dcfg, writeTemplate(t), found); err == nil {
			t.Fatal("expected an error for an invalid keys file")
		}
	})

	t.Run("a failed zone lookup is an error", func(t *testing.T) {
		boom := errors.New("boom")
		failing := func(context.Context, string) (string, error) { return "", boom }
		if _, err := preflightNixConfig(ctx, config.DevboxConfig{DNSZone: "example.com."}, writeTemplate(t), failing); !errors.Is(err, boom) {
			t.Fatalf("want the lookup error, got %v", err)
		}
	})

	t.Run("a missing template is an error", func(t *testing.T) {
		if _, err := preflightNixConfig(ctx, config.DevboxConfig{}, filepath.Join(t.TempDir(), "nope.nix"), found); err == nil {
			t.Fatal("expected an error for a missing template")
		}
	})
}

// The Go renderer and the template's Nix expression share a contract: one
// double-quoted string, keys separated by the two-character escape \n, and an
// unrendered marker filtered out. This evaluates the real expression.
func TestTemplateAuthorizedKeysExpressionEvaluates(t *testing.T) {
	if _, err := exec.LookPath("nix-instantiate"); err != nil {
		t.Skip("nix-instantiate not on PATH")
	}
	if err := exec.Command("nix-instantiate", "--find-file", "nixpkgs").Run(); err != nil {
		t.Skip("<nixpkgs> is not resolvable here")
	}
	expression := regexp.MustCompile(`(?s)openssh\.authorizedKeys\.keys =(.*?);`)

	one, two := fakeKey("B", "it's a laptop"), fakeKey("C", "")
	cases := map[string][]string{"no keys file": nil, "one key": {one}, "two keys": {one, two}}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			dcfg := config.DevboxConfig{}
			if want != nil {
				dcfg.AuthorizedKeysFile = filepath.Join(t.TempDir(), "authorized_keys")
				if err := os.WriteFile(dcfg.AuthorizedKeysFile, []byte(strings.Join(want, "\n")+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			rendered, err := renderNixConfig(dcfg, realTemplate(), nil)
			if err != nil {
				t.Fatal(err)
			}
			match := expression.FindSubmatch(rendered)
			if match == nil {
				t.Fatal("authorized keys expression not found in the template")
			}
			nix := "let lib = import <nixpkgs/lib>; in " + string(match[1])
			out, err := exec.Command("nix-instantiate", "--eval", "--strict", "--json", "-E", nix).Output()
			if err != nil {
				t.Fatalf("evaluating the expression: %v", err)
			}
			var got []string
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("unexpected evaluation result %s: %v", out, err)
			}
			if strings.Join(got, "|") != strings.Join(want, "|") || len(got) != len(want) {
				t.Errorf("evaluated to %d keys %q, want %d", len(got), got, len(want))
			}
		})
	}
}

// With no built-in zone id, an unrendered marker on the box is a normal
// state. The boot script must then exit before it calls anything.
func TestUpdateRoute53ScriptGuard(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	data, err := os.ReadFile(realTemplate())
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`(?s)writeShellScript "update-route53" ''\n(.*?)\n\s*''\);`).FindSubmatch(data)
	if match == nil {
		t.Fatal("update-route53 script not found in the template")
	}

	run := func(t *testing.T, zone, record string) (output string, awsArgs string, calls []string) {
		t.Helper()
		stubs := t.TempDir()
		stub := func(name, body string) {
			script := "#!" + bash + "\necho " + name + " >> " + filepath.Join(stubs, "calls") + "\n" + body + "\n"
			if err := os.WriteFile(filepath.Join(stubs, name), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		stub("curl", "echo 192.0.2.1")
		stub("aws", `echo "$@" > `+filepath.Join(stubs, "aws-args"))

		// Undo the Nix quoting: ''${ is a literal ${, and ${pkgs.x}/bin/ is a
		// store path, replaced here by the stub directory.
		script := strings.ReplaceAll(string(match[1]), "''${", "${")
		script = regexp.MustCompile(`\$\{pkgs\.[A-Za-z0-9_-]+\}/bin/`).ReplaceAllString(script, stubs+"/")
		script = strings.NewReplacer("@@DNS_ZONE_ID@@", zone, "@@DNS_RECORD_NAME@@", record).Replace(script)
		if strings.Contains(script, "${pkgs.") {
			t.Fatal("unhandled Nix interpolation left in the script")
		}

		out, err := exec.Command(bash, "-c", script).CombinedOutput()
		if err != nil {
			t.Fatalf("script failed: %v\n%s", err, out)
		}
		if called, err := os.ReadFile(filepath.Join(stubs, "calls")); err == nil {
			calls = strings.Fields(string(called))
		}
		args, _ := os.ReadFile(filepath.Join(stubs, "aws-args"))
		return string(out), string(args), calls
	}

	skipped := map[string][2]string{
		"both markers unrendered":    {"@@DNS_ZONE_ID@@", "@@DNS_RECORD_NAME@@"},
		"zone marker unrendered":     {"@@DNS_ZONE_ID@@", "box.example.com"},
		"record marker unrendered":   {"ZTEST", "@@DNS_RECORD_NAME@@"},
		"zone rendered to nothing":   {"", "box.example.com"},
		"record rendered to nothing": {"ZTEST", ""},
	}
	for name, values := range skipped {
		t.Run(name, func(t *testing.T) {
			output, _, calls := run(t, values[0], values[1])
			if !strings.Contains(output, "skipping Route 53 update") {
				t.Errorf("expected the skip message, got: %s", output)
			}
			if len(calls) != 0 {
				t.Errorf("nothing may be called when a value is missing; called: %v", calls)
			}
		})
	}

	t.Run("rendered values reach Route 53", func(t *testing.T) {
		_, awsArgs, _ := run(t, "ZTEST", "box.example.com")
		for _, want := range []string{"--hosted-zone-id ZTEST", "box.example.com", "192.0.2.1"} {
			if !strings.Contains(awsArgs, want) {
				t.Errorf("aws was not called with %q; args: %s", want, awsArgs)
			}
		}
	})
}

// The template must not start an agent at boot with permission prompts off.
func TestTemplateHasNoAgentAutostart(t *testing.T) {
	data, err := os.ReadFile(realTemplate())
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"devbox-claude", "skip-permissions"} {
		if strings.Contains(string(data), unwanted) {
			t.Errorf("template contains %q", unwanted)
		}
	}
}
