package cmd

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/spf13/cobra"
)

func newTerminateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "terminate <instance-id> [instance-id...]",
		Short: "Terminate spot instances",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return terminateInstances(cmd.Context(), ec2Client, args)
		},
	}
}

func terminateInstances(ctx context.Context, client *ec2.Client, ids []string) error {
	input := &ec2.TerminateInstancesInput{
		InstanceIds: ids,
	}
	result, err := client.TerminateInstances(ctx, input)
	if err != nil {
		return fmt.Errorf("terminating instances: %w", err)
	}
	for _, change := range result.TerminatingInstances {
		fmt.Printf("%s: %s -> %s\n",
			*change.InstanceId,
			change.PreviousState.Name,
			change.CurrentState.Name,
		)
	}
	return nil
}
