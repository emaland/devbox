package cmd

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/instantcocoa/devbox/internal/config"
)

// Tests for `devbox spawn --no-dns`: a clone that must never take over the
// primary's DNS name, neither from the user_data it boots first nor from the
// config pushed after.

func TestWithoutDNSPushesNoRecord(t *testing.T) {
	dcfg := withoutDNS(config.DevboxConfig{DNSName: "box.example.com.", DNSZone: "example.com", DNSZoneID: "ZTESTZONE"})
	found := false
	resolved, err := resolveDNSZoneID(context.Background(), dcfg, func(context.Context, string) (string, error) {
		found = true
		return "/hostedzone/ZTESTZONE", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Error("a --no-dns clone looked up the hosted zone")
	}
	out, err := renderNixConfig(resolved, writeTemplate(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Left unrendered, which update-route53 skips.
	if !strings.Contains(string(out), `record = "@@DNS_RECORD_NAME@@";`) || !strings.Contains(string(out), `zone = "@@DNS_ZONE_ID@@";`) {
		t.Errorf("the pushed config names a DNS record or zone:\n%s", out)
	}
	if strings.Contains(string(out), "box.example.com") || strings.Contains(string(out), "ZTESTZONE") {
		t.Errorf("the primary's DNS values reached the clone's config:\n%s", out)
	}
}

func TestChooseUserData(t *testing.T) {
	type calls struct{ fetched, detected []string }
	source := func(c *calls) userDataSource {
		return userDataSource{
			fetch: func(_ context.Context, id string) (string, error) {
				c.fetched = append(c.fetched, id)
				return "user-data-of-" + id, nil
			},
			detect: func(context.Context) (string, error) {
				c.detected = append(c.detected, "detect")
				return "primary", nil
			},
		}
	}

	var c calls
	got, err := chooseUserData(context.Background(), source(&c), "", true)
	if err != nil {
		t.Fatal(err)
	}
	if got != stubUserData() || len(c.fetched) != 0 || len(c.detected) != 0 {
		t.Fatalf("--no-dns: got %q, calls %+v; want the stub and no lookups", decodeOrRaw(got), c)
	}

	c = calls{}
	if got, _ := chooseUserData(context.Background(), source(&c), "", false); got != "user-data-of-primary" || len(c.detected) != 1 {
		t.Fatalf("default: got %q, calls %+v", got, c)
	}
	c = calls{}
	if got, _ := chooseUserData(context.Background(), source(&c), "other", false); got != "user-data-of-other" || len(c.detected) != 0 {
		t.Fatalf("--from: got %q, calls %+v", got, c)
	}

	// No source to clone from: the stub, as before.
	none := userDataSource{
		fetch:  func(context.Context, string) (string, error) { t.Fatal("fetched with no source"); return "", nil },
		detect: func(context.Context) (string, error) { return "", errors.New("none") },
	}
	if got, _ := chooseUserData(context.Background(), none, "", false); got != stubUserData() {
		t.Fatalf("no source: got %q", decodeOrRaw(got))
	}
}

func TestSpawnNoDNSAndFromAreExclusive(t *testing.T) {
	spawn := newSpawnCmd()
	spawn.SetArgs([]string{"--no-dns", "--from", "i-x"})
	spawn.SilenceUsage, spawn.SilenceErrors = true, true
	err := spawn.Execute()
	if err == nil || !strings.Contains(err.Error(), "no-dns") {
		t.Fatalf("--no-dns --from accepted: %v", err)
	}
}

func decodeOrRaw(s string) string {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return string(b)
	}
	return s
}
