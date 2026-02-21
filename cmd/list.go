package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spf13/cobra"

	"github.com/emaland/devbox/internal/awsutil"
)

func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List spot instances and their state",
		RunE: func(cmd *cobra.Command, args []string) error {
			return listInstances(cmd.Context(), ec2Client)
		},
	}
}

func listInstances(ctx context.Context, client *ec2.Client) error {
	input := &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("instance-lifecycle"),
				Values: []string{"spot"},
			},
			{
				Name:   aws.String("instance-state-name"),
				Values: []string{"running", "stopped", "stopping", "pending"},
			},
		},
	}

	result, err := client.DescribeInstances(ctx, input)
	if err != nil {
		return fmt.Errorf("describing instances: %w", err)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "INSTANCE ID\tNAME\tTYPE\tSTATE\tAZ\tPUBLIC IP\tSPOT REQUEST")

	for _, reservation := range result.Reservations {
		for _, inst := range reservation.Instances {
			name := awsutil.NameTag(inst.Tags)
			publicIP := "-"
			if inst.PublicIpAddress != nil {
				publicIP = *inst.PublicIpAddress
			}
			spotReqID := "-"
			if inst.SpotInstanceRequestId != nil {
				spotReqID = *inst.SpotInstanceRequestId
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				*inst.InstanceId,
				name,
				string(inst.InstanceType),
				strings.ToUpper(string(inst.State.Name)),
				*inst.Placement.AvailabilityZone,
				publicIP,
				spotReqID,
			)
		}
	}
	w.Flush()
	return nil
}
