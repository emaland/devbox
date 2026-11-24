package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/spf13/cobra"
)

func newRebootCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "reboot [instance-id...]",
		Short: "Reboot instances (in-place, same host)",
		Long: `Reboot instances in place. Refused while an agent is working; the
gateway is stopped cleanly first; each interrupted turn then waits for Try
Again (or a new message) after the boot.`,
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
			guard := stopGuard{force: force, out: os.Stdout}
			return guard.run(ctx, args,
				func(ctx context.Context) error {
					_, err := ec2Client.RebootInstances(ctx, &ec2.RebootInstancesInput{InstanceIds: args, DryRun: aws.Bool(true)})
					return dryRunAllowed(err)
				},
				func(ctx context.Context) error { return rebootInstances(ctx, ec2Client, args) })
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "reboot even if an agent is working, or its status cannot be read (the gateway is still stopped cleanly first)")
	return cmd
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
