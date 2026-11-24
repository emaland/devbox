package cmd

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/route53"

	"github.com/instantcocoa/devbox/internal/config"
)

// Tests for `devbox spawn --no-dns`: a clone that must never take over the
// primary's DNS name, neither from the user_data it boots first, nor from the
// config pushed after, nor from any later command run against it.

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

// spawnDNS is what spawnInstance launches with: the config to push and the
// tags that keep later commands from writing DNS for the clone.
func TestSpawnDNS(t *testing.T) {
	primary := config.DevboxConfig{DNSName: "box.example.com.", DNSZone: "example.com", DNSZoneID: "ZTESTZONE"}

	dcfg, tags := spawnDNS(primary, true)
	if dcfg.DNSName != "" || dcfg.DNSZone != "" || dcfg.DNSZoneID != "" {
		t.Errorf("--no-dns kept DNS values: %+v", dcfg)
	}
	if !dnsOff(tags) {
		t.Errorf("--no-dns does not tag the clone: %v", tags)
	}

	dcfg, tags = spawnDNS(primary, false)
	if dcfg != primary || len(tags) != 0 {
		t.Errorf("without --no-dns: %+v, tags %v", dcfg, tags)
	}
}

func TestDNSOffTag(t *testing.T) {
	tag := func(k, v string) types.Tag { return types.Tag{Key: aws.String(k), Value: aws.String(v)} }
	for _, c := range []struct {
		tags []types.Tag
		want bool
	}{
		{[]types.Tag{tag("devbox-managed", "true"), tag(dnsTag, "off")}, true},
		{[]types.Tag{tag(dnsTag, "on")}, false},
		{[]types.Tag{tag("devbox-managed", "true")}, false},
		{nil, false},
	} {
		if got := dnsOff(c.tags); got != c.want {
			t.Errorf("%v: %v, want %v", c.tags, got, c.want)
		}
	}
}

func TestChooseUserData(t *testing.T) {
	type calls struct{ fetched, detected []string }
	source := func(c *calls) userDataSource {
		return userDataSource{
			fetch: func(_ context.Context, id string) (string, error) {
				c.fetched = append(c.fetched, id)
				if id == "broken" {
					return "", errors.New("fetch failed")
				}
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
	if err != nil || got != stubUserData() || len(c.fetched) != 0 || len(c.detected) != 0 {
		t.Fatalf("--no-dns: got %q, %v, calls %+v; want the stub and no lookups", decodeOrRaw(got), err, c)
	}

	c = calls{}
	if got, err := chooseUserData(context.Background(), source(&c), "", false); err != nil || got != "user-data-of-primary" || len(c.detected) != 1 {
		t.Fatalf("default: got %q, %v, calls %+v", got, err, c)
	}
	c = calls{}
	if got, err := chooseUserData(context.Background(), source(&c), "other", false); err != nil || got != "user-data-of-other" || len(c.detected) != 0 {
		t.Fatalf("--from: got %q, %v, calls %+v", got, err, c)
	}
	// A fetch that fails is an error, not a quiet fall back to the stub.
	c = calls{}
	if _, err := chooseUserData(context.Background(), source(&c), "broken", false); err == nil {
		t.Fatal("--from with a failing fetch: no error")
	}

	// No source to clone from: the stub, as before.
	none := userDataSource{
		fetch:  func(context.Context, string) (string, error) { t.Fatal("fetched with no source"); return "", nil },
		detect: func(context.Context) (string, error) { return "", errors.New("none") },
	}
	if got, err := chooseUserData(context.Background(), none, "", false); err != nil || got != stubUserData() {
		t.Fatalf("no source: got %q, %v", decodeOrRaw(got), err)
	}
}

func TestSpawnNoDNSAndFromAreExclusive(t *testing.T) {
	spawn := newSpawnCmd()
	if err := spawn.ParseFlags([]string{"--no-dns", "--from", "i-x"}); err != nil {
		t.Fatal(err)
	}
	if err := spawn.ValidateFlagGroups(); err == nil {
		t.Fatal("--no-dns --from accepted")
	}
	spawn = newSpawnCmd()
	if err := spawn.ParseFlags([]string{"--no-dns"}); err != nil {
		t.Fatal(err)
	}
	if err := spawn.ValidateFlagGroups(); err != nil {
		t.Fatalf("--no-dns alone refused: %v", err)
	}
}

// LocalStack: a clone tagged at spawn gets no DNS from later commands.
func TestLaterCommandsWriteNoDNSForANoDNSClone(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	domain := uniqueName("nodns") + ".test."
	zoneID := createTestHostedZone(t, ctx, domain)
	id := createTestInstance(t, ctx)
	_, tags := spawnDNS(config.DevboxConfig{}, true)
	if _, err := testEC2Client.CreateTags(ctx, &ec2.CreateTagsInput{Resources: []string{id}, Tags: tags}); err != nil {
		t.Fatal(err)
	}
	cfg := testDevboxConfig()
	cfg.DNSZone = domain
	cfg.DNSName = "dev." + strings.TrimSuffix(domain, ".")

	// `devbox dns`, `up` and `resize` go through updateDNS.
	if err := updateDNS(ctx, cfg, testEC2Client, testR53Client, id, cfg.DNSName); err != nil {
		t.Fatalf("updateDNS: %v", err)
	}
	records, err := testR53Client.ListResourceRecordSets(ctx, &route53.ListResourceRecordSetsInput{HostedZoneId: aws.String(zoneID)})
	if err != nil {
		t.Fatal(err)
	}
	for _, rr := range records.ResourceRecordSets {
		if strings.HasPrefix(*rr.Name, "dev.") {
			t.Fatalf("a record was written for the --no-dns clone: %s", *rr.Name)
		}
	}

	// `nix-update <clone>` and any later push render without DNS.
	pushed, err := dnsConfigFor(ctx, testEC2Client, id, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if pushed.DNSName != "" || pushed.DNSZone != "" || pushed.DNSZoneID != "" {
		t.Errorf("a push to the --no-dns clone would carry DNS: %+v", pushed)
	}

	// `setup-dns <clone>` refuses.
	if err := setupDNSOnBoot(ctx, cfg, testEC2Client, id); err == nil || !strings.Contains(err.Error(), "--no-dns") {
		t.Errorf("setup-dns on the --no-dns clone: %v", err)
	}

	// An untagged instance is unchanged.
	other := createTestInstance(t, ctx)
	if pushed, err := dnsConfigFor(ctx, testEC2Client, other, cfg); err != nil || pushed.DNSName != cfg.DNSName {
		t.Errorf("an ordinary instance lost its DNS: %+v, %v", pushed, err)
	}
}

func decodeOrRaw(s string) string {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return string(b)
	}
	return s
}

// R-CODE on PR 19: the tag reaches the launch, and a resize carries it to
// the replacement, so the clone stays a no-DNS box.
func TestNoDNSTagIsLaunchedAndCarried(t *testing.T) {
	_, dnsTags := spawnDNS(config.DevboxConfig{}, true)
	launched := launchTags("clone", dnsTags)
	if !dnsOff(launched) {
		t.Fatalf("launched without the tag: %v", launched)
	}
	if dnsOff(launchTags("primary", nil)) {
		t.Fatal("an ordinary spawn is tagged")
	}
	withPlan := append(launched,
		types.Tag{Key: aws.String("aws:ec2spot:fleet-request-id"), Value: aws.String("x")},
		types.Tag{Key: aws.String(resizeStateTag), Value: aws.String("launched")})
	carried := carriedTags(withPlan)
	if !dnsOff(carried) {
		t.Fatalf("resize drops the tag: %v", carried)
	}
	for _, tag := range carried {
		if k := aws.ToString(tag.Key); strings.HasPrefix(k, "aws:") || k == resizeStateTag {
			t.Errorf("resize carries %s", k)
		}
	}
}
