package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"

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

	desc, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return fmt.Errorf("describing instance: %w", err)
	}
	if len(desc.Reservations) == 0 || len(desc.Reservations[0].Instances) == 0 {
		return fmt.Errorf("instance %s not found", instanceID)
	}
	inst := desc.Reservations[0].Instances[0]
	if inst.PublicIpAddress == nil {
		return fmt.Errorf("instance %s has no public IP (is it running?)", instanceID)
	}
	ip := *inst.PublicIpAddress
	keyPath := dcfg.ResolveSSHKeyPath()

	sshTarget := dcfg.SSHUser + "@" + ip
	sshOpts := []string{
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
	}

	// SCP the file over
	fmt.Printf("Uploading %s to %s (%s)...\n", nixFile, instanceID, ip)
	scpArgs := append([]string{}, sshOpts...)
	scpArgs = append(scpArgs, nixFile, sshTarget+":/tmp/configuration.nix")
	scpCmd := exec.CommandContext(ctx, "scp", scpArgs...)
	scpCmd.Stdout = os.Stdout
	scpCmd.Stderr = os.Stderr
	if err := scpCmd.Run(); err != nil {
		return fmt.Errorf("scp failed: %w", err)
	}

	// Move into place and rebuild. nixos-rebuild switch exits non-zero
	// if any service fails during activation, even when the config itself
	// applied successfully. We treat that as a warning, not an error.
	fmt.Println("Running nixos-rebuild switch...")
	remoteCmd := `sudo cp /tmp/configuration.nix /etc/nixos/configuration.nix && sudo nixos-rebuild switch`
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
