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

func newStopCmd() *cobra.Command {
	var after string

	cmd := &cobra.Command{
		Use:   "stop [instance-id...]",
		Short: "Stop running spot instances",
		Long: `Stop running spot instances immediately, or schedule an auto-stop timer.

  devbox stop <id> [id...]          Stop instances immediately
  devbox stop --after 4h [id]       SSH in and set auto-stop timer to 4h
  devbox stop --after off [id]      SSH in and disable auto-stop timer`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if after != "" {
				return scheduleStop(cmd.Context(), dcfg, ec2Client, after, args)
			}
			if len(args) == 0 {
				return fmt.Errorf("requires at least 1 arg(s), only received 0 (use --after for scheduled stop)")
			}
			return stopInstances(cmd.Context(), ec2Client, args)
		},
	}

	cmd.Flags().StringVar(&after, "after", "", "schedule auto-stop after duration (e.g. 4h, 30m) or 'off' to disable")

	return cmd
}

func stopInstances(ctx context.Context, client *ec2.Client, ids []string) error {
	input := &ec2.StopInstancesInput{
		InstanceIds: ids,
	}
	result, err := client.StopInstances(ctx, input)
	if err != nil {
		return fmt.Errorf("stopping instances: %w", err)
	}
	for _, change := range result.StoppingInstances {
		fmt.Printf("%s: %s -> %s\n",
			*change.InstanceId,
			change.PreviousState.Name,
			change.CurrentState.Name,
		)
	}
	return nil
}

func scheduleStop(ctx context.Context, dcfg config.DevboxConfig, client *ec2.Client, duration string, args []string) error {
	var instanceID string
	if len(args) >= 1 {
		instanceID = args[0]
	} else {
		id, err := autoDetectRunningInstance(ctx, client)
		if err != nil {
			return err
		}
		instanceID = id
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

	var remoteCmd string
	if duration == "off" {
		remoteCmd = `sudo mkdir -p /etc/devbox
echo "off" | sudo tee /etc/devbox/autostop-after > /dev/null
sudo systemctl stop devbox-autostop-sched.timer 2>/dev/null || true`
		fmt.Printf("Disabling auto-stop on %s (%s)...\n", instanceID, ip)
	} else {
		remoteCmd = fmt.Sprintf(`sudo mkdir -p /etc/devbox
echo %q | sudo tee /etc/devbox/autostop-after > /dev/null
sudo systemctl stop devbox-autostop-sched.timer 2>/dev/null || true
sudo systemctl restart devbox-schedule-autostop.service`, duration)
		fmt.Printf("Setting auto-stop to %s on %s (%s)...\n", duration, instanceID, ip)
	}

	sshCmd := exec.CommandContext(ctx, "ssh",
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		dcfg.SSHUser+"@"+ip,
		remoteCmd,
	)
	sshCmd.Stdout = os.Stdout
	sshCmd.Stderr = os.Stderr
	if err := sshCmd.Run(); err != nil {
		return fmt.Errorf("ssh command failed: %w", err)
	}

	if duration == "off" {
		fmt.Printf("Auto-stop disabled on %s.\n", instanceID)
	} else {
		fmt.Printf("Auto-stop set to %s on %s.\n", duration, instanceID)
	}
	return nil
}
