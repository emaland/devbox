package cmd

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spf13/cobra"
)

func newBidsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "bids",
		Short: "Show current spot request bids (max price)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return showBids(cmd.Context(), ec2Client)
		},
	}
}

func showBids(ctx context.Context, client *ec2.Client) error {
	result, err := client.DescribeSpotInstanceRequests(ctx, &ec2.DescribeSpotInstanceRequestsInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("state"),
				Values: []string{"open", "active"},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("describing spot requests: %w", err)
	}

	if len(result.SpotInstanceRequests) == 0 {
		fmt.Println("No active spot instance requests.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SPOT REQUEST\tINSTANCE ID\tTYPE\tAZ\tMAX PRICE\tSTATE\tSTATUS")

	for _, req := range result.SpotInstanceRequests {
		instanceID := "-"
		if req.InstanceId != nil {
			instanceID = *req.InstanceId
		}
		maxPrice := "-"
		if req.SpotPrice != nil {
			maxPrice = "$" + *req.SpotPrice
		}
		az := "-"
		if req.LaunchedAvailabilityZone != nil {
			az = *req.LaunchedAvailabilityZone
		}
		status := "-"
		if req.Status != nil && req.Status.Code != nil {
			status = *req.Status.Code
		}
		itype := "-"
		if req.LaunchSpecification != nil {
			itype = string(req.LaunchSpecification.InstanceType)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			*req.SpotInstanceRequestId,
			instanceID,
			itype,
			az,
			maxPrice,
			string(req.State),
			status,
		)
	}
	w.Flush()
	return nil
}
