package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/spf13/cobra"

	"github.com/emaland/devbox/internal/config"
)

// nixupStartMarker is echoed at the top of the remote apply script so the
// completion poller can show only the current run's journal, not the history
// journalctl -u keeps for prior runs of the (reused) transient unit name.
const nixupStartMarker = "@@DEVBOX-NIXUP-START@@"

func newNixUpdateCmd() *cobra.Command {
	var nixFile string

	cmd := &cobra.Command{
		Use:   "nix-update [instance-id]",
		Short: "Push configuration.nix to an instance and run nixos-rebuild switch",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			instanceID := ""
			if len(args) == 1 {
				instanceID = args[0]
			} else {
				id, err := autoDetectRunningInstance(cmd.Context(), ec2Client)
				if err != nil {
					return err
				}
				instanceID = id
			}
			return nixUpdate(cmd.Context(), dcfg, ec2Client, instanceID, nixFile)
		},
	}

	cmd.Flags().StringVar(&nixFile, "file", "terraform/configuration.nix", "path to configuration.nix")

	return cmd
}

func nixUpdate(ctx context.Context, dcfg config.DevboxConfig, client *ec2.Client, instanceID, nixFile string) error {
	// Verify the config is available (disk or embedded) before we talk to AWS
	if _, err := readNixConfigSource(nixFile); err != nil {
		return err
	}

	ip, err := instancePublicIP(ctx, client, instanceID)
	if err != nil {
		return err
	}

	// Try configured user first, fall back to root (fresh instances only
	// have root SSH until the config is applied and devbox-fetch-ssh-key runs).
	// No volume id here — `nix-update` targets a box whose volume is already
	// formatted, so the format service no-ops on the unrendered marker.
	err = pushNixConfigToHost(ctx, dcfg, ip, instanceID, nixFile, dcfg.SSHUser, "")
	if err != nil && dcfg.SSHUser != "root" {
		fmt.Println("Retrying as root (fresh instance)...")
		err = pushNixConfigToHost(ctx, dcfg, ip, instanceID, nixFile, "root", "")
	}
	return err
}

// pushNixConfig looks up a running instance's IP and pushes configuration.nix
// via root SSH (for fresh instances where emaland keys aren't set up yet).
func pushNixConfig(ctx context.Context, dcfg config.DevboxConfig, client *ec2.Client, instanceID, volumeID string) error {
	nixFile := defaultNixFile()
	if _, err := readNixConfigSource(nixFile); err != nil {
		return err
	}

	ip, err := instancePublicIP(ctx, client, instanceID)
	if err != nil {
		return err
	}

	// Wait for SSH to become available on the freshly-booted stub. A cold
	// instance can take a couple of minutes to bring up sshd, so poll for up
	// to 5 minutes; returning early here (instead of charging ahead into scp)
	// gives the caller a clear "SSH never came up" error rather than a cryptic
	// scp failure.
	fmt.Println("Waiting for SSH to become available...")
	keyPath := dcfg.ResolveSSHKeyPath()
	sshReady := false
	for i := 0; i < 30; i++ {
		testCmd := exec.CommandContext(ctx, "ssh",
			"-i", keyPath,
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "ConnectTimeout=5",
			"root@"+ip, "true",
		)
		if testCmd.Run() == nil {
			sshReady = true
			break
		}
		if i%3 == 2 {
			fmt.Printf("  still waiting for SSH on %s (%ds elapsed)...\n", ip, (i+1)*10)
		}
		time.Sleep(10 * time.Second)
	}
	if !sshReady {
		return fmt.Errorf("SSH on %s (%s) did not become available within 5 minutes", instanceID, ip)
	}

	return pushNixConfigToHost(ctx, dcfg, ip, instanceID, nixFile, "root", volumeID)
}

func instancePublicIP(ctx context.Context, client *ec2.Client, instanceID string) (string, error) {
	desc, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return "", fmt.Errorf("describing instance: %w", err)
	}
	if len(desc.Reservations) == 0 || len(desc.Reservations[0].Instances) == 0 {
		return "", fmt.Errorf("instance %s not found", instanceID)
	}
	inst := desc.Reservations[0].Instances[0]
	if inst.PublicIpAddress == nil {
		return "", fmt.Errorf("instance %s has no public IP (is it running?)", instanceID)
	}
	return *inst.PublicIpAddress, nil
}

func pushNixConfigToHost(ctx context.Context, dcfg config.DevboxConfig, ip, instanceID, nixFile, sshUser, volumeID string) error {
	keyPath := dcfg.ResolveSSHKeyPath()

	sshTarget := sshUser + "@" + ip
	sshOpts := []string{
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
	}

	// Render the config (substitute @@MARKER@@ placeholders) to a temp file so
	// the instance never receives literal markers. SCP the rendered file.
	rendered, err := renderNixConfig(dcfg, nixFile, dataVolumeMarker(volumeID))
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "devbox-configuration-*.nix")
	if err != nil {
		return fmt.Errorf("creating temp config: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(rendered); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp config: %w", err)
	}
	tmp.Close()

	// SCP the rendered file over
	fmt.Printf("Uploading %s to %s (%s)...\n", nixFile, instanceID, ip)
	scpArgs := append([]string{}, sshOpts...)
	scpArgs = append(scpArgs, tmp.Name(), sshTarget+":/tmp/configuration.nix")
	scpCmd := exec.CommandContext(ctx, "scp", scpArgs...)
	scpCmd.Stdout = os.Stdout
	scpCmd.Stderr = os.Stderr
	if err := scpCmd.Run(); err != nil {
		return fmt.Errorf("scp failed: %w", err)
	}

	// Move into place, save a copy to the persistent /home volume so it
	// survives instance replacements, and rebuild.  nixos-rebuild switch
	// exits non-zero if any service fails during activation, even when
	// the config itself applied successfully — we treat that as a warning.
	// Wait for the box to finish its initial boot before switching. SSH comes
	// up early, and running `nixos-rebuild switch` while first-boot activation
	// is still in flight collides on the switch-to-configuration transient unit
	// (so /home wouldn't mount until a reboot). is-system-running --wait blocks
	// until boot completes (running or degraded).
	// Persist the saved /home copy BEFORE the switch (when /home is mounted) so
	// the boot-time restore service sees a matching saved config and won't
	// revert /etc/nixos to the older copy. nixos-rebuild switch restarts sshd
	// and can cut this session short, so the before-switch copy is what makes
	// the update stick on an existing box; the after-switch copy covers a fresh
	// box whose /home only mounts during the switch.
	fmt.Println("Waiting for boot to settle, then running nixos-rebuild switch...")
	// Persist the saved /home copy BEFORE the switch so the boot-time restore
	// service sees a matching saved config and skips (otherwise it reverts
	// /etc/nixos to the older copy when the switch re-triggers it). cp is the
	// critical step; chown is best-effort (emaland may not exist yet on a fresh
	// box, where this copy lands on the not-yet-mounted /home and is harmless).
	// This runs inside a systemd-run unit as root with a minimal PATH, so no
	// sudo (unavailable there, and redundant) and set PATH explicitly so
	// nixos-rebuild/cp/etc. resolve.
	persist := `mkdir -p /home/emaland/.config/devbox; ` +
		`cp /tmp/configuration.nix /home/emaland/.config/devbox/configuration.nix; ` +
		`chown -R emaland:users /home/emaland/.config/devbox 2>/dev/null || true`
	// Reuse a local Nix binary cache kept on the persistent /home volume so a
	// fresh box (post resize/recover) substitutes prebuilt paths from local gp3
	// disk instead of re-downloading the whole closure from cache.nixos.org.
	//
	// The store lives on the *root* volume, which is new on every swap; only
	// /home is recycled — so we stage the closure there. On a fresh box /home is
	// not mounted yet, so we mount the data volume at /home first (its canonical
	// mountpoint, which the switch's home.mount then finds already satisfied) —
	// that both exposes the cache for substitution and avoids a second mount of
	// the same device. If a cache is present we point the switch at it; the paths
	// we stage are unsigned, so require-sigs=false (our own volume). After the
	// switch we (re)populate the cache from the running system for the next fresh
	// box — regardless of the switch's exit code, since a flaky activation-time
	// service (e.g. update-route53) still yields a valid /run/current-system.
	// The whole thing is best-effort: any failure just falls back to the network.
	cacheDir := "/home/.nix-cache"
	// cachePre: on a fresh box /home is not mounted yet, so mount the data volume
	// at /home (its canonical mountpoint, which the switch's home.mount then finds
	// already satisfied) — that exposes the cache and avoids a second mount of the
	// same device. If a cache is present, point the switch at it; the staged paths
	// are unsigned, so require-sigs=false (our own volume).
	cachePre := `if ! grep -qs " /home " /proc/mounts; then mount /dev/disk/by-label/home-data /home 2>/dev/null || true; fi; ` +
		`OPT=""; if [ -d ` + cacheDir + ` ]; then OPT="--option extra-substituters file://` + cacheDir + ` --option extra-trusted-substituters file://` + cacheDir + ` --option require-sigs false"; echo "devbox: substituting from local nix cache ` + cacheDir + `"; fi`
	// cachePost: repopulate the cache for the next fresh box. This runs AFTER the
	// rc marker is written, so the client returns as soon as the switch is done —
	// the first-time nix copy (~GBs) then finishes in the background inside the
	// unit. Populated regardless of the switch's exit code (a flaky activation-time
	// service still yields a valid /run/current-system). Best-effort throughout.
	cachePost := `if grep -qs " /home " /proc/mounts; then echo "devbox: updating local nix cache in background"; mkdir -p ` + cacheDir + `; nix copy --to "file://` + cacheDir + `" /run/current-system 2>/dev/null || true; chown -R emaland:users ` + cacheDir + ` 2>/dev/null || true; fi`

	// The apply script captures nixos-rebuild's own exit code (the trailing
	// persist would otherwise mask it) and writes it to a marker file that
	// signals completion to the poller below. It sources /etc/set-environment
	// for NIX_PATH etc. (the transient unit's env is otherwise minimal, so
	// nixos-rebuild can't find <nixpkgs/nixos>), and emits a start marker so
	// the poller can show only the current run's log, not prior units' history.
	// The rc marker is written before cachePost so the client isn't blocked on the
	// slow first-time cache copy.
	applyScript := `echo ` + nixupStartMarker + `; ` +
		`. /etc/set-environment 2>/dev/null || true; ` +
		`export PATH=/run/wrappers/bin:/run/current-system/sw/bin:$PATH; ` +
		`systemctl is-system-running --wait >/dev/null 2>&1 || true; ` +
		`cp /tmp/configuration.nix /etc/nixos/configuration.nix; ` +
		persist + `; ` +
		cachePre + `; ` +
		`nixos-rebuild switch $OPT; rc=$?; ` +
		persist + `; ` +
		`echo "$rc" | tee /tmp/devbox-nixup.rc >/dev/null; ` +
		cachePost

	// Run the switch as a transient systemd unit and poll for completion,
	// instead of holding one long SSH channel open across the whole rebuild.
	// nixos-rebuild switch restarts sshd mid-activation; a synchronous channel
	// has no keepalive and stalls on the dropped connection for many minutes.
	// Detaching it into `systemd-run` lets the switch finish independently while
	// we watch it over short, keepalive'd connections that reconnect freely.
	// applyScript contains no single quotes, so single-quoting it is safe.
	const unit = "devbox-nixup"
	launch := `sudo systemctl stop ` + unit + ` 2>/dev/null; ` +
		`sudo systemctl reset-failed ` + unit + ` 2>/dev/null; ` +
		`sudo rm -f /tmp/devbox-nixup.rc; ` +
		`sudo systemd-run --unit=` + unit + ` --collect /bin/sh -c '` + applyScript + `'`
	launchArgs := append([]string{}, sshOpts...)
	launchArgs = append(launchArgs, sshTarget, launch)
	if out, err := exec.CommandContext(ctx, "ssh", launchArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("starting remote nixos-rebuild: %w\n%s", err, out)
	}

	fmt.Println("Running nixos-rebuild switch (this can take several minutes on a fresh box)...")
	pollOpts := append([]string{}, sshOpts...)
	pollOpts = append(pollOpts,
		"-o", "ServerAliveInterval=10", "-o", "ServerAliveCountMax=3",
		"-o", "ConnectTimeout=10")
	query := `journalctl -u ` + unit + ` -o cat --no-pager 2>/dev/null; ` +
		`echo "@@RC@@"; cat /tmp/devbox-nixup.rc 2>/dev/null`
	printed := 0
	deadline := time.Now().Add(30 * time.Minute)
	for {
		args := append([]string{}, pollOpts...)
		args = append(args, sshTarget, query)
		out, _ := exec.CommandContext(ctx, "ssh", args...).CombinedOutput()
		logPart, rcPart, found := strings.Cut(string(out), "@@RC@@")
		if found {
			// journalctl -u shows every past run of this unit name; skip to the
			// last start marker so we only stream the current invocation (and
			// print nothing until that marker appears).
			lines := strings.Split(strings.TrimRight(logPart, "\n"), "\n")
			lastStart := -1
			for i, l := range lines {
				if strings.Contains(l, nixupStartMarker) {
					lastStart = i
				}
			}
			if lastStart >= 0 {
				if printed < lastStart+1 {
					printed = lastStart + 1
				}
				for ; printed < len(lines); printed++ {
					if lines[printed] != "" {
						fmt.Println("  " + lines[printed])
					}
				}
			}
			if rc := strings.TrimSpace(rcPart); rc != "" {
				if rc != "0" {
					fmt.Printf("\nWarning: nixos-rebuild exited %s (usually service-activation warnings, not a build failure).\n", rc)
				}
				break
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after 30m waiting for nixos-rebuild on %s; it may still be running (check: journalctl -u %s)", instanceID, unit)
		}
		time.Sleep(10 * time.Second)
	}

	fmt.Printf("NixOS configuration updated on %s.\n", instanceID)
	return nil
}
