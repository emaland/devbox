package cmd

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/spf13/cobra"
)

func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <instance-id> [instance-id...]",
		Short: "Stop running spot instances",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return stopInstances(cmd.Context(), ec2Client, args)
		},
	}
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
