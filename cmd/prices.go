package cmd

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spf13/cobra"
)

func newPricesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "prices",
		Short: "Show current spot market prices for our instance types",
		RunE: func(cmd *cobra.Command, args []string) error {
			return showPrices(cmd.Context(), ec2Client)
		},
	}
}

func showPrices(ctx context.Context, client *ec2.Client) error {
	// First gather all instance types + AZs from our active spot requests
	reqs, err := client.DescribeSpotInstanceRequests(ctx, &ec2.DescribeSpotInstanceRequestsInput{
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

	if len(reqs.SpotInstanceRequests) == 0 {
		fmt.Println("No active spot requests to check prices for.")
		return nil
	}

	// Collect unique instance types
	typeSet := map[types.InstanceType]bool{}
	for _, req := range reqs.SpotInstanceRequests {
		if req.LaunchSpecification != nil {
			typeSet[req.LaunchSpecification.InstanceType] = true
		}
	}
	var instanceTypes []types.InstanceType
	for t := range typeSet {
		instanceTypes = append(instanceTypes, t)
	}

	// Get the latest spot price for each
	startTime := time.Now().Add(-1 * time.Hour)

	priceResult, err := client.DescribeSpotPriceHistory(ctx, &ec2.DescribeSpotPriceHistoryInput{
		InstanceTypes:       instanceTypes,
		StartTime:           &startTime,
		ProductDescriptions: []string{"Linux/UNIX"},
	})
	if err != nil {
		return fmt.Errorf("describing spot price history: %w", err)
	}

	// Deduplicate: keep only the latest price per (type, AZ)
	type key struct {
		itype string
		az    string
	}
	latest := map[key]types.SpotPrice{}
	for _, sp := range priceResult.SpotPriceHistory {
		k := key{string(sp.InstanceType), *sp.AvailabilityZone}
		existing, ok := latest[k]
		if !ok || sp.Timestamp.After(*existing.Timestamp) {
			latest[k] = sp
		}
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "INSTANCE TYPE\tAZ\tCURRENT PRICE")
	for _, sp := range latest {
		fmt.Fprintf(w, "%s\t%s\t$%s/hr\n",
			string(sp.InstanceType),
			*sp.AvailabilityZone,
			*sp.SpotPrice,
		)
	}
	w.Flush()
	return nil
}
