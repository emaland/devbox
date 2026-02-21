package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/spf13/cobra"

	"github.com/emaland/devbox/internal/config"
)

func newSSHCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ssh <instance-id>",
		Short: "SSH into an instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return sshToInstance(cmd.Context(), dcfg, ec2Client, args[0])
		},
	}
}

func sshToInstance(ctx context.Context, dcfg config.DevboxConfig, client *ec2.Client, instanceID string) error {
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
		return fmt.Errorf("instance %s has no public IP", instanceID)
	}
	ip := *inst.PublicIpAddress

	keyPath := dcfg.ResolveSSHKeyPath()

	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh not found in PATH: %w", err)
	}

	fmt.Printf("Connecting to %s (%s)...\n", instanceID, ip)
	return syscall.Exec(sshBin, []string{
		"ssh",
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		dcfg.SSHUser + "@" + ip,
	}, os.Environ())
}
