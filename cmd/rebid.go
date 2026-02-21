package cmd

import (
	"context"
	"fmt"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spf13/cobra"
)

func newRebidCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rebid <spot-request-id> <new-price>",
		Short: "Cancel and re-create a spot request with a new max price",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return rebid(cmd.Context(), ec2Client, args[0], args[1])
		},
	}
}

func rebid(ctx context.Context, client *ec2.Client, spotRequestID string, newPrice string) error {
	// Validate the price parses as a float
	price, err := strconv.ParseFloat(newPrice, 64)
	if err != nil || price <= 0 {
		return fmt.Errorf("invalid price %q: must be a positive number", newPrice)
	}

	// Fetch the existing spot request to clone its parameters
	desc, err := client.DescribeSpotInstanceRequests(ctx, &ec2.DescribeSpotInstanceRequestsInput{
		SpotInstanceRequestIds: []string{spotRequestID},
	})
	if err != nil {
		return fmt.Errorf("describing spot request: %w", err)
	}
	if len(desc.SpotInstanceRequests) == 0 {
		return fmt.Errorf("spot request %s not found", spotRequestID)
	}
	old := desc.SpotInstanceRequests[0]

	oldPrice := "(unset/on-demand)"
	if old.SpotPrice != nil {
		oldPrice = "$" + *old.SpotPrice
	}

	// Cancel the old request
	_, err = client.CancelSpotInstanceRequests(ctx, &ec2.CancelSpotInstanceRequestsInput{
		SpotInstanceRequestIds: []string{spotRequestID},
	})
	if err != nil {
		return fmt.Errorf("canceling old spot request: %w", err)
	}
	fmt.Printf("Canceled old request %s (was %s)\n", spotRequestID, oldPrice)

	// Create a new request with the same launch spec but new price
	priceStr := newPrice
	newReq, err := client.RequestSpotInstances(ctx, &ec2.RequestSpotInstancesInput{
		SpotPrice:             &priceStr,
		InstanceCount:         aws.Int32(1),
		Type:                  old.Type,
		LaunchSpecification:   toLaunchSpec(old.LaunchSpecification),
		AvailabilityZoneGroup: old.AvailabilityZoneGroup,
		BlockDurationMinutes:  old.BlockDurationMinutes,
		ValidUntil:            old.ValidUntil,
	})
	if err != nil {
		return fmt.Errorf("creating new spot request: %w", err)
	}

	for _, req := range newReq.SpotInstanceRequests {
		fmt.Printf("New request %s with max price $%s\n", *req.SpotInstanceRequestId, newPrice)
	}

	return nil
}

func toLaunchSpec(from *types.LaunchSpecification) *types.RequestSpotLaunchSpecification {
	if from == nil {
		return nil
	}
	spec := &types.RequestSpotLaunchSpecification{
		ImageId:      from.ImageId,
		InstanceType: from.InstanceType,
		KeyName:      from.KeyName,
		SubnetId:     from.SubnetId,
	}
	if from.Placement != nil {
		spec.Placement = &types.SpotPlacement{
			AvailabilityZone: from.Placement.AvailabilityZone,
		}
	}
	if len(from.SecurityGroups) > 0 {
		var sgIDs []string
		for _, sg := range from.SecurityGroups {
			if sg.GroupId != nil {
				sgIDs = append(sgIDs, *sg.GroupId)
			}
		}
		spec.SecurityGroupIds = sgIDs
	}
	if from.BlockDeviceMappings != nil {
		spec.BlockDeviceMappings = from.BlockDeviceMappings
	}
	if from.IamInstanceProfile != nil {
		spec.IamInstanceProfile = &types.IamInstanceProfileSpecification{
			Arn:  from.IamInstanceProfile.Arn,
			Name: from.IamInstanceProfile.Name,
		}
	}
	if from.Monitoring != nil && from.Monitoring.Enabled != nil {
		spec.Monitoring = &types.RunInstancesMonitoringEnabled{
			Enabled: from.Monitoring.Enabled,
		}
	}
	if from.EbsOptimized != nil {
		spec.EbsOptimized = from.EbsOptimized
	}
	return spec
}
