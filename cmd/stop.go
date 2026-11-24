package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/spf13/cobra"

	"github.com/instantcocoa/devbox/internal/config"
)

func newDownCmd() *cobra.Command {
	var after string
	var force, hard, whenIdle bool

	cmd := &cobra.Command{
		Use:     "down [instance-id...]",
		Aliases: []string{"stop"},
		Short:   "Stop the devbox (preserves the data volume)",
		Long: `Stop running instances, or schedule an auto-stop timer. The data volume
is preserved; bring the box back with 'devbox up'.

Before stopping, the agents on the box are checked: if one is working, the
stop is refused and what is running is shown. The gateway is then stopped
cleanly; at the next start each interrupted turn waits for Try Again.

  devbox down [id...]               Stop, unless an agent is working
  devbox down --force [id...]       Stop even if an agent is working (still cleanly)
  devbox down --when-idle [id...]   Stop once no agent is working (checked on the box)
  devbox down --hard [id]           EC2 force stop of a wedged instance (no clean shutdown)
  devbox down --after 4h [id]       SSH in and set the idle window to 4h
  devbox down --after off [id]      SSH in and disable auto-stop`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if after != "" {
				if !autostopDurationPattern.MatchString(after) {
					return fmt.Errorf("--after %q: use a number with s, m, min, h or d (for example 90m or 4h), or off; the box ignores anything else", after)
				}
				return scheduleStop(ctx, dcfg, ec2Client, after, args)
			}
			if len(args) == 0 {
				id, err := autoDetectRunningInstance(ctx, ec2Client)
				if err != nil {
					return err
				}
				args = []string{id}
			}
			if whenIdle {
				return stopWhenIdle(ctx, args)
			}
			guard := stopGuard{force: force, hard: hard, out: os.Stdout}
			return guard.run(ctx, args,
				func(ctx context.Context) error {
					_, err := ec2Client.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: args, DryRun: aws.Bool(true)})
					return dryRunAllowed(err)
				},
				func(ctx context.Context) error { return stopInstancesForce(ctx, ec2Client, args, hard) })
		},
	}

	cmd.Flags().StringVar(&after, "after", "", "schedule auto-stop after duration (e.g. 4h, 30m) or 'off' to disable")
	cmd.Flags().BoolVar(&force, "force", false, "stop even if an agent is working, or its status cannot be read (the gateway is still stopped cleanly first)")
	cmd.Flags().BoolVar(&hard, "hard", false, "EC2 force stop without a clean OS shutdown, for a wedged instance; skips the agent check")
	cmd.Flags().BoolVar(&whenIdle, "when-idle", false, "stop once no agent is working; the box itself checks every 10 minutes")
	cmd.MarkFlagsMutuallyExclusive("when-idle", "force", "hard", "after")

	return cmd
}

func stopWhenIdle(ctx context.Context, ids []string) error {
	for _, id := range ids {
		out, status, err := runRemote(ctx, id, "devbox-agents stop-when-idle")
		if err != nil {
			return err
		}
		fmt.Printf("%s: %s", id, out)
		if err := remoteStatusError(id, status); err != nil {
			return err
		}
	}
	return nil
}

func stopInstances(ctx context.Context, client *ec2.Client, ids []string) error {
	return stopInstancesForce(ctx, client, ids, false)
}

func stopInstancesForce(ctx context.Context, client *ec2.Client, ids []string, force bool) error {
	input := &ec2.StopInstancesInput{
		InstanceIds: ids,
	}
	if force {
		input.Force = &force
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

// autostopDurationPattern is the grammar autostop.sh accepts for its window
// file; it falls back to its default for anything else.
var autostopDurationPattern = regexp.MustCompile(`^(off|[0-9]+(s|m|min|h|d)?)$`)

// autostopAfterCommand writes the idle window file on the box. The value is
// quoted for the remote shell.
func autostopAfterCommand(duration string) string {
	return "sudo mkdir -p /etc/devbox && printf '%s\\n' " + shellQuote(duration) + " | sudo tee /etc/devbox/autostop-after >/dev/null"
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

	// The auto-stop poll reads the file every time; nothing to restart.
	remoteCmd := autostopAfterCommand(duration)
	if duration == "off" {
		fmt.Printf("Disabling auto-stop on %s (%s)...\n", instanceID, ip)
	} else {
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
