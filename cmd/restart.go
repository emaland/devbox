package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/spf13/cobra"
)

func newRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart <instance-id> [instance-id...]",
		Short: "Stop then start instances (new host)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return restartInstances(cmd.Context(), ec2Client, args)
		},
	}
}

func restartInstances(ctx context.Context, client *ec2.Client, ids []string) error {
	fmt.Printf("Stopping %d instance(s)...\n", len(ids))
	_, err := client.StopInstances(ctx, &ec2.StopInstancesInput{
		InstanceIds: ids,
	})
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
	var result *ec2.StartInstancesOutput
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
