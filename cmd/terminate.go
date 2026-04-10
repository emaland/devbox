package cmd

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spf13/cobra"
)

func newTerminateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "terminate [instance-id]",
		Short: "Terminate a spot instance and cancel its spot request",
		Long: `Terminate a spot instance, cancel its persistent spot request, and
report which EBS volumes were preserved (DeleteOnTermination=false).`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			instanceID := ""
			if len(args) == 1 {
				instanceID = args[0]
			} else {
				// Try stopped first (common case: can't start), then running
				id, err := autoDetectInstance(cmd.Context(), ec2Client, "stopped")
				if err != nil {
					id, err = autoDetectRunningInstance(cmd.Context(), ec2Client)
					if err != nil {
						return fmt.Errorf("no instance specified and auto-detect failed: %w", err)
					}
				}
				instanceID = id
			}
			return terminateInstance(cmd.Context(), ec2Client, instanceID)
		},
	}
}

func terminateInstance(ctx context.Context, client *ec2.Client, instanceID string) error {
	// Describe the instance to get spot request and volume info
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

	// Cancel the spot request first so AWS doesn't relaunch
	if inst.SpotInstanceRequestId != nil {
		spotID := *inst.SpotInstanceRequestId
		fmt.Printf("Canceling spot request %s...\n", spotID)
		_, err := client.CancelSpotInstanceRequests(ctx, &ec2.CancelSpotInstanceRequestsInput{
			SpotInstanceRequestIds: []string{spotID},
		})
		if err != nil {
			fmt.Printf("Warning: failed to cancel spot request: %v\n", err)
		}
	}

	// Identify volumes that will be preserved
	rootDevice := ""
	if inst.RootDeviceName != nil {
		rootDevice = *inst.RootDeviceName
	}
	var preserved []types.InstanceBlockDeviceMapping
	for _, bdm := range inst.BlockDeviceMappings {
		if bdm.Ebs != nil && bdm.Ebs.DeleteOnTermination != nil && !*bdm.Ebs.DeleteOnTermination {
			preserved = append(preserved, bdm)
		}
	}

	// Terminate
	fmt.Printf("Terminating instance %s...\n", instanceID)
	result, err := client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return fmt.Errorf("terminating instance: %w", err)
	}
	for _, change := range result.TerminatingInstances {
		fmt.Printf("%s: %s -> %s\n",
			*change.InstanceId,
			change.PreviousState.Name,
			change.CurrentState.Name,
		)
	}

	// Report preserved volumes
	if len(preserved) > 0 {
		fmt.Println("\nPreserved volumes (DeleteOnTermination=false):")
		for _, bdm := range preserved {
			device := ""
			if bdm.DeviceName != nil {
				device = *bdm.DeviceName
			}
			fmt.Printf("  %s (%s)\n", *bdm.Ebs.VolumeId, device)
		}
	}

	// Warn about root volume deletion
	for _, bdm := range inst.BlockDeviceMappings {
		if bdm.DeviceName != nil && *bdm.DeviceName == rootDevice {
			if bdm.Ebs != nil && bdm.Ebs.DeleteOnTermination != nil && *bdm.Ebs.DeleteOnTermination {
				fmt.Printf("\nRoot volume %s (%s) will be deleted.\n", *bdm.Ebs.VolumeId, rootDevice)
			}
		}
	}

	return nil
}
