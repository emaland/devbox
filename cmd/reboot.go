package cmd

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/spf13/cobra"
)

func newRebootCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reboot <instance-id> [instance-id...]",
		Short: "Reboot instances (in-place, same host)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return rebootInstances(cmd.Context(), ec2Client, args)
		},
	}
}

func rebootInstances(ctx context.Context, client *ec2.Client, ids []string) error {
	_, err := client.RebootInstances(ctx, &ec2.RebootInstancesInput{
		InstanceIds: ids,
	})
	if err != nil {
		return fmt.Errorf("rebooting instances: %w", err)
	}
	for _, id := range ids {
		fmt.Printf("%s: rebooting\n", id)
	}
	return nil
}
