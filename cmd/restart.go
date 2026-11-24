package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/spf13/cobra"
)

func newRestartCmd() *cobra.Command {
	var force, hard bool

	cmd := &cobra.Command{
		Use:   "restart [instance-id...]",
		Short: "Stop then start instances (new host)",
		Long: `Stop then start instances (new host). Refused while an agent is working;
the gateway is stopped cleanly first; each interrupted turn then waits for
Try Again (or a new message) after the start.

  devbox restart [id...]          Stop then start, unless an agent is working
  devbox restart --force [id...]  Even if an agent is working (still cleanly)
  devbox restart --hard [id]      EC2 force stop of a wedged instance, then start`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if len(args) == 0 {
				id, err := autoDetectRunningInstance(ctx, ec2Client)
				if err != nil {
					return err
				}
				args = []string{id}
			}
			guard := stopGuard{force: force, hard: hard, out: os.Stdout}
			return guard.run(ctx, args,
				func(ctx context.Context) error {
					_, err := ec2Client.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: args, DryRun: aws.Bool(true)})
					return dryRunAllowed(err)
				},
				func(ctx context.Context) error { return restartInstancesForce(ctx, ec2Client, args, hard) })
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "restart even if an agent is working, or its status cannot be read (the gateway is still stopped cleanly first)")
	cmd.Flags().BoolVar(&hard, "hard", false, "EC2 force stop without a clean OS shutdown, for a wedged instance; skips the agent check")
	cmd.MarkFlagsMutuallyExclusive("force", "hard")

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
