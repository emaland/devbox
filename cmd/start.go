package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/spf13/cobra"
)

func newStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start <instance-id> [instance-id...]",
		Short: "Start stopped spot instances",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return startInstances(cmd.Context(), ec2Client, args)
		},
	}
}

func startInstances(ctx context.Context, client *ec2.Client, ids []string) error {
	// Persistent spot requests can lag behind instance state after a stop.
	// Retry if the spot request isn't ready yet.
	var result *ec2.StartInstancesOutput
	var err error
	for attempts := 0; attempts < 6; attempts++ {
		result, err = client.StartInstances(ctx, &ec2.StartInstancesInput{
			InstanceIds: ids,
		})
		if err == nil {
			break
		}
		if strings.Contains(err.Error(), "IncorrectSpotRequestState") && attempts < 5 {
			fmt.Println("Spot request not ready yet, waiting...")
			time.Sleep(10 * time.Second)
			continue
		}
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
