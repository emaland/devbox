package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/spf13/cobra"
)

func newRestartCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "restart [instance-id...]",
		Short: "Stop then start instances (new host)",
		Long: `Stop then start instances (new host).

  devbox restart [id...]          Stop then start instances
  devbox restart --force [id]     Force stop a wedged/unresponsive instance, then start`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				id, err := autoDetectRunningInstance(cmd.Context(), ec2Client)
				if err != nil {
					return err
				}
				args = []string{id}
			}
			return restartInstancesForce(cmd.Context(), ec2Client, args, force)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "force stop without a clean OS shutdown (use for wedged/unresponsive instances)")

	return cmd
}

func restartInstancesForce(ctx context.Context, client *ec2.Client, ids []string, force bool) error {
	fmt.Printf("Stopping %d instance(s)...\n", len(ids))
	stopInput := &ec2.StopInstancesInput{
		InstanceIds: ids,
	}
	if force {
		stopInput.Force = &force
	}
	_, err := client.StopInstances(ctx, stopInput)
	if err != nil {
		return fmt.Errorf("stopping instances: %w", err)
	}
	waiter := ec2.NewInstanceStoppedWaiter(client)
	if err := waiter.Wait(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: ids,
	}, 5*time.Minute); err != nil {
		return fmt.Errorf("waiting for instances to stop: %w", err)
	}
	fmt.Println("Stopped. Starting...")
	// Persistent spot requests lag behind instance state — retry if not ready.
	result, err := spotRetryStart(ctx, client, ids)
	if err != nil {
		return fmt.Errorf("starting instances: %w", err)
	}
	runWaiter := ec2.NewInstanceRunningWaiter(client)
	if err := runWaiter.Wait(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: ids,
	}, 5*time.Minute); err != nil {
		return fmt.Errorf("waiting for instances to start: %w", err)
	}
	for _, change := range result.StartingInstances {
		fmt.Printf("%s: running\n", *change.InstanceId)
	}
	return nil
}
