package cmd

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/emaland/devbox/internal/config"
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
// empty are left untouched — the NixOS scripts guard for an unrendered marker
// (case "@@*@@") and no-op rather than acting on a bogus value. The result is
// the concrete NixOS config baked into user_data and pushed to instances.
func renderNixConfig(dcfg config.DevboxConfig, nixFile string, extra map[string]string) ([]byte, error) {
	data, err := readNixConfigSource(nixFile)
	if err != nil {
		return nil, err
	}
	s := string(data)
	repl := map[string]string{
		"@@DNS_ZONE_ID@@":        dcfg.DNSZoneID,
		"@@DNS_RECORD_NAME@@":    strings.TrimSuffix(dcfg.DNSName, "."),
		"@@TAILSCALE_AUTH_KEY@@": dcfg.TailscaleAuthKey,
		"@@DATA_VOLUME_ID@@":     "",
	}
	for k, v := range extra {
		repl[k] = v
	}
	for marker, val := range repl {
		if val == "" {
			continue
		}
		s = strings.ReplaceAll(s, marker, val)
	}
	return []byte(s), nil
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
