package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/spf13/cobra"

	"github.com/emaland/devbox/internal/config"
)

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
	// Verify the file exists before we talk to AWS
	if _, err := os.Stat(nixFile); err != nil {
		return fmt.Errorf("reading %s: %w", nixFile, err)
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
	if _, err := os.Stat(nixFile); err != nil {
		return fmt.Errorf("reading %s: %w", nixFile, err)
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
	persist := `sudo mkdir -p /home/emaland/.config/devbox; ` +
		`sudo cp /tmp/configuration.nix /home/emaland/.config/devbox/configuration.nix; ` +
		`sudo chown -R emaland:users /home/emaland/.config/devbox 2>/dev/null || true`
	remoteCmd := `sudo systemctl is-system-running --wait >/dev/null 2>&1 || true; ` +
		`sudo cp /tmp/configuration.nix /etc/nixos/configuration.nix; ` +
		persist + `; ` +
		`sudo nixos-rebuild switch; ` +
		persist
	sshArgs := append([]string{}, sshOpts...)
	sshArgs = append(sshArgs, sshTarget, remoteCmd)
	sshCmd := exec.CommandContext(ctx, "ssh", sshArgs...)
	sshCmd.Stdout = os.Stdout
	sshCmd.Stderr = os.Stderr
	if err := sshCmd.Run(); err != nil {
		fmt.Printf("\nWarning: nixos-rebuild reported errors (likely service failures, not build errors).\n")
	}

	fmt.Printf("NixOS configuration updated on %s.\n", instanceID)
	return nil
}
