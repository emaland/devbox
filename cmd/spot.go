package cmd

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/route53"

	"github.com/instantcocoa/devbox/internal/awsutil"
	"github.com/instantcocoa/devbox/internal/config"
)

// spotRetryStart starts instances, retrying when the persistent spot request
// lags behind instance state after a stop (IncorrectSpotRequestState). This is
// shared by start/restart/spawn/resize/up so the behaviour is identical.
func spotRetryStart(ctx context.Context, client *ec2.Client, ids []string) (*ec2.StartInstancesOutput, error) {
	var result *ec2.StartInstancesOutput
	var err error
	for attempts := 0; attempts < 6; attempts++ {
		result, err = client.StartInstances(ctx, &ec2.StartInstancesInput{
			InstanceIds: ids,
		})
		if err == nil {
			return result, nil
		}
		if strings.Contains(err.Error(), "IncorrectSpotRequestState") && attempts < 5 {
			fmt.Println("Spot request not ready yet, waiting...")
			time.Sleep(10 * time.Second)
			continue
		}
		return nil, err
	}
	return result, err
}

// startInstances starts the given instances (with spot-lag retry) and prints
// the state transitions.
func startInstances(ctx context.Context, client *ec2.Client, ids []string) error {
	result, err := spotRetryStart(ctx, client, ids)
	if err != nil {
		return fmt.Errorf("starting instances: %w", err)
	}
	for _, change := range result.StartingInstances {
		fmt.Printf("%s: %s -> %s\n",
			*change.InstanceId,
			change.PreviousState.Name,
			change.CurrentState.Name,
		)
	}
	return nil
}

// autoDetectBox finds the single managed devbox instance (spot or on-demand)
// in any active state. Returns ("", "", nil) when none exist so callers can
// decide whether that's an error (resize) or a cue to create one (up).
func autoDetectBox(ctx context.Context, client *ec2.Client) (string, types.InstanceStateName, error) {
	desc, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{Name: aws.String("tag:devbox-managed"), Values: []string{"true"}},
			{Name: aws.String("instance-state-name"), Values: []string{"running", "stopped", "stopping", "pending"}},
		},
	})
	if err != nil {
		return "", "", fmt.Errorf("finding devbox instance: %w", err)
	}
	var ids []string
	var states []types.InstanceStateName
	for _, res := range desc.Reservations {
		for _, inst := range res.Instances {
			ids = append(ids, *inst.InstanceId)
			states = append(states, inst.State.Name)
		}
	}
	if len(ids) == 0 {
		return "", "", nil
	}
	if len(ids) > 1 {
		return "", "", fmt.Errorf("multiple devbox instances found (%s) — specify one explicitly", strings.Join(ids, ", "))
	}
	return ids[0], states[0], nil
}

// waitRunning blocks until the instance reaches the running state.
func waitRunning(ctx context.Context, client *ec2.Client, id string) error {
	w := ec2.NewInstanceRunningWaiter(client)
	return w.Wait(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}}, 5*time.Minute)
}

// waitStopped blocks until the instance reaches the stopped state.
func waitStopped(ctx context.Context, client *ec2.Client, id string) error {
	w := ec2.NewInstanceStoppedWaiter(client)
	return w.Wait(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}}, 5*time.Minute)
}

// instanceState returns the current state name of an instance.
func instanceState(ctx context.Context, client *ec2.Client, id string) (types.InstanceStateName, error) {
	desc, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{id},
	})
	if err != nil {
		return "", err
	}
	if len(desc.Reservations) == 0 || len(desc.Reservations[0].Instances) == 0 {
		return "", fmt.Errorf("instance %s not found", id)
	}
	return desc.Reservations[0].Instances[0].State.Name, nil
}

// defaultNixFile is the path (relative to the repo root) of the NixOS config
// used to render user_data and the pushed configuration.
func defaultNixFile() string {
	return "terraform/configuration.nix"
}

// EmbeddedNixConfig is the built-in copy of terraform/configuration.nix,
// embedded into the binary by main.go so devbox works when run from any
// working directory (not just a checkout of this repo). Set once at startup.
var EmbeddedNixConfig []byte

// readNixConfigSource returns the raw (unrendered) contents of nixFile. A
// copy on disk always takes precedence (so editing configuration.nix in a
// checkout of this repo and re-running takes effect without a rebuild); when
// the default path isn't present on disk — the common case for an installed
// binary run outside the repo — it falls back to the embedded copy.
func readNixConfigSource(nixFile string) ([]byte, error) {
	data, err := os.ReadFile(nixFile)
	if err == nil {
		return data, nil
	}
	if os.IsNotExist(err) && nixFile == defaultNixFile() && len(EmbeddedNixConfig) > 0 {
		return EmbeddedNixConfig, nil
	}
	return nil, fmt.Errorf("reading %s: %w", nixFile, err)
}

// renderNixConfig reads configuration.nix and substitutes the @@MARKER@@
// placeholders with values from the devbox config (plus any per-instance
// `extra` substitutions, e.g. the data volume id). Markers whose value is
// empty are left untouched — the NixOS config guards for an unrendered marker
// and no-ops rather than acting on a bogus value. The result is the concrete
// NixOS config pushed to instances.
//
// Values that identify one person's machines or account (the hosted zone id,
// extra ssh public keys) come from the operator's config and files here; they
// are never written into the template in this repo.
func renderNixConfig(dcfg config.DevboxConfig, nixFile string, extra map[string]string) ([]byte, error) {
	data, err := readNixConfigSource(nixFile)
	if err != nil {
		return nil, err
	}
	keys, err := readAuthorizedKeys(dcfg.ResolveAuthorizedKeysFile())
	if err != nil {
		return nil, err
	}
	repl := map[string]string{
		"@@DNS_ZONE_ID@@":        dcfg.DNSZoneID,
		"@@DNS_RECORD_NAME@@":    strings.TrimSuffix(dcfg.DNSName, "."),
		"@@TAILSCALE_AUTH_KEY@@": dcfg.TailscaleAuthKey,
		"@@DATA_VOLUME_ID@@":     "",
		// Rendered inside a double-quoted Nix string that the config splits
		// on newlines, so the separator is the two-character escape `\n`.
		"@@EXTRA_AUTHORIZED_KEYS@@": strings.Join(keys, `\n`),
	}
	for k, v := range extra {
		repl[k] = v
	}
	// One pass over the template: a substituted value is never scanned again,
	// so marker text inside one value cannot pull in another value.
	var pairs []string
	for marker, val := range repl {
		if val == "" {
			continue
		}
		pairs = append(pairs, marker, val)
	}
	return []byte(strings.NewReplacer(pairs...).Replace(string(data))), nil
}

// renderForPush is what a push sends to the box: the hosted zone id is
// resolved first (it has no built-in default), then the template is rendered.
func renderForPush(ctx context.Context, dcfg config.DevboxConfig, nixFile, volumeID string, find zoneFinder) ([]byte, error) {
	dcfg, err := resolveDNSZoneID(ctx, dcfg, find)
	if err != nil {
		return nil, err
	}
	return renderNixConfig(dcfg, nixFile, dataVolumeMarker(volumeID))
}

// setForPush renders the main file and packs it with its tree and the
// operator's local.nix into the archive a push uploads. It also returns the
// install script from that same tree.
func setForPush(ctx context.Context, dcfg config.DevboxConfig, nixFile, volumeID string, find zoneFinder) (packed, installScript []byte, err error) {
	source, err := readNixSource(nixFile)
	if err != nil {
		return nil, nil, err
	}
	installScript, ok := source.tree[installSetScriptPath]
	if !ok {
		return nil, nil, fmt.Errorf("the config tree beside %s has no %s; it cannot be installed on the box", nixFile, installSetScriptPath)
	}
	rendered, err := renderForPush(ctx, dcfg, nixFile, volumeID, find)
	if err != nil {
		return nil, nil, err
	}
	localModule, err := readLocalModule(dcfg.ResolveNixLocalModule())
	if err != nil {
		return nil, nil, err
	}
	packed, err = buildSetTar(rendered, source.tree, localModule)
	return packed, installScript, err
}

// preflightNixConfig checks, before any instance is created or changed,
// everything a later config push needs from this machine: the template, the
// extra keys file and the hosted zone id. It returns dcfg with the zone id
// filled in, and says how many extra ssh keys the push will install, because
// a push replaces the box's extra keys with exactly that set.
func preflightNixConfig(ctx context.Context, dcfg config.DevboxConfig, nixFile string, find zoneFinder) (config.DevboxConfig, error) {
	if _, err := readNixSource(nixFile); err != nil {
		return dcfg, err
	}
	localModule := dcfg.ResolveNixLocalModule()
	if _, err := readLocalModule(localModule); err != nil {
		return dcfg, err
	}
	if localModule == "" {
		fmt.Println("No nix_local_module configured: the box keeps the local.nix it has, if any.")
	} else {
		fmt.Printf("Operator module to install as local.nix: %s\n", localModule)
	}
	keysFile := dcfg.ResolveAuthorizedKeysFile()
	keys, err := readAuthorizedKeys(keysFile)
	if err != nil {
		return dcfg, err
	}
	if len(keys) == 0 {
		fmt.Printf("Warning: no extra ssh keys to install (%s). After the config is applied only the EC2 key pair can log in; any extra key the box has now is removed.\n", describeKeysFile(keysFile))
	} else {
		fmt.Printf("Extra ssh keys to install: %d (from %s)\n", len(keys), keysFile)
	}
	return resolveDNSZoneID(ctx, dcfg, find)
}

func describeKeysFile(path string) string {
	if path == "" {
		return "authorized_keys_file is not set"
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path + " does not exist"
	}
	return path + " holds no keys"
}

// authorizedKeyLine matches one ssh public key: a known key type, base64 key
// material, and an optional comment, separated by single spaces. The comment
// is limited to printable ASCII without the characters that are special inside
// a double-quoted Nix string (", \, $), because the key is rendered into one.
// Control characters are excluded on purpose: Nix reads a carriage return in a
// string as a line break, which would turn the rest of the line into a second,
// unchecked entry.
var authorizedKeyLine = regexp.MustCompile(
	`^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com)` +
		` [A-Za-z0-9+/]+={0,3}( [ !#%-\[\]-~]*)?$`)

// readAuthorizedKeys returns the ssh public keys in path, one per line,
// skipping blank lines and # comments. A missing file (or an empty path) means
// no extra keys. A line that is not a plain public key is an error: the keys
// end up in sshd's authorized keys on the box, so nothing is passed through
// unchecked. Errors name the line number, never its content.
func readAuthorizedKeys(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading authorized keys file %s: %w", path, err)
	}
	var keys []string
	for i, line := range strings.Split(string(data), "\n") {
		// Accept CRLF line endings, then refuse any other control character
		// anywhere, comment lines included: a file with carriage-return-only
		// line endings would otherwise read as one long comment and silently
		// yield no keys.
		line = strings.Trim(strings.TrimSuffix(line, "\r"), " \t")
		hasControl := strings.IndexFunc(line, unicode.IsControl) >= 0
		if !hasControl && (line == "" || strings.HasPrefix(line, "#")) {
			continue
		}
		if hasControl || !authorizedKeyLine.MatchString(line) || strings.Contains(line, "@@") {
			return nil, fmt.Errorf("%s line %d is not an accepted ssh public key (expected \"<type> <base64> [comment]\" separated by single spaces; no options; printable ASCII only; no quote, backslash, dollar sign or @@ in the comment)", path, i+1)
		}
		keys = append(keys, line)
	}
	return keys, nil
}

// zoneFinder looks up a hosted zone id from its name.
type zoneFinder func(ctx context.Context, zone string) (string, error)

// findHostedZoneID is the zoneFinder used by commands: Route 53 with the
// command's AWS credentials. Tests replace it.
var findHostedZoneID zoneFinder = func(ctx context.Context, zone string) (string, error) {
	return awsutil.FindHostedZone(ctx, route53.NewFromConfig(awsCfg), zone)
}

// resolveDNSZoneID returns dcfg with DNSZoneID filled in. An explicit
// dns_zone_id wins. Otherwise the id is looked up from dns_zone with find, so
// no account-specific id has to be built into this program. With no dns_zone
// either, the id stays empty and the boot-time DNS updater on the box skips.
func resolveDNSZoneID(ctx context.Context, dcfg config.DevboxConfig, find zoneFinder) (config.DevboxConfig, error) {
	if dcfg.DNSZoneID != "" || dcfg.DNSZone == "" {
		return dcfg, nil
	}
	zoneID, err := find(ctx, dcfg.DNSZone)
	if err != nil {
		return dcfg, fmt.Errorf("looking up the hosted zone id for %s: %w\n\nSet it explicitly with: devbox config set dns_zone_id <id>", dcfg.DNSZone, err)
	}
	// FindHostedZone returns e.g. "/hostedzone/ZXXXXX"; the box wants the bare id.
	dcfg.DNSZoneID = strings.TrimPrefix(zoneID, "/hostedzone/")
	return dcfg, nil
}

// nixosStub is a minimal, bootable NixOS config used as EC2 user_data. The full
// configuration.nix is far larger than the 16 KB user_data limit, so a new box
// boots this (SSH only) and the real config — including the @@DATA_VOLUME_ID@@
// for the auto-format service — is delivered via the SSH push afterward
// (pushNixConfig), which has no size limit.
const nixosStub = `{ config, pkgs, lib, modulesPath, ... }:
{
  imports = [ "${modulesPath}/virtualisation/amazon-image.nix" ];
  services.openssh.enable = true;
  services.openssh.settings.PermitRootLogin = "prohibit-password";
}
`

// stubUserData returns the base64-encoded minimal stub for EC2 user_data.
func stubUserData() string {
	return base64.StdEncoding.EncodeToString([]byte(nixosStub))
}

// dataVolumeMarker returns the @@DATA_VOLUME_ID@@ substitution map (or nil when
// no data volume is designated) for rendering the pushed config.
func dataVolumeMarker(volumeID string) map[string]string {
	if volumeID == "" {
		return nil
	}
	return map[string]string{"@@DATA_VOLUME_ID@@": volumeID}
}
