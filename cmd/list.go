package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spf13/cobra"

	"github.com/emaland/devbox/internal/awsutil"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "status",
		Aliases: []string{"list", "ls"},
		Short:   "Show the devbox: state, IP, attached volumes, and spot price",
		RunE: func(cmd *cobra.Command, args []string) error {
			return listInstances(cmd.Context(), ec2Client)
		},
	}
}

func listInstances(ctx context.Context, client *ec2.Client) error {
	input := &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("tag:devbox-managed"),
				Values: []string{"true"},
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

	var instances []types.Instance
	for _, reservation := range result.Reservations {
		instances = append(instances, reservation.Instances...)
	}
	if len(instances) == 0 {
		fmt.Println("No devbox instances. Run 'devbox up' to launch one.")
		return nil
	}

	volNames := resolveAttachedVolumeNames(ctx, client, instances)

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "INSTANCE ID\tNAME\tTYPE\tSTATE\tAZ\tPUBLIC IP\tSPOT $/hr\tVOLUMES")

	for _, inst := range instances {
		name := awsutil.NameTag(inst.Tags)
		publicIP := "-"
		if inst.PublicIpAddress != nil {
			publicIP = *inst.PublicIpAddress
		}
		az := ""
		if inst.Placement != nil && inst.Placement.AvailabilityZone != nil {
			az = *inst.Placement.AvailabilityZone
		}
		price := "-"
		if p, err := latestSpotPrice(ctx, client, string(inst.InstanceType), az); err == nil && p != "" {
			price = p
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			*inst.InstanceId,
			name,
			string(inst.InstanceType),
			strings.ToUpper(string(inst.State.Name)),
			az,
			publicIP,
			price,
			describeAttachedVolumes(inst, volNames),
		)
	}
	w.Flush()
	return nil
}

// describeAttachedVolumes formats the non-root EBS volumes attached to an
// instance as "name(device)" entries (falling back to the volume ID).
func describeAttachedVolumes(inst types.Instance, names map[string]string) string {
	rootDevice := ""
	if inst.RootDeviceName != nil {
		rootDevice = *inst.RootDeviceName
	}
	var parts []string
	for _, bdm := range inst.BlockDeviceMappings {
		if bdm.DeviceName == nil || bdm.Ebs == nil || bdm.Ebs.VolumeId == nil {
			continue
		}
		if *bdm.DeviceName == rootDevice {
			continue
		}
		volID := *bdm.Ebs.VolumeId
		label := volID
		if n, ok := names[volID]; ok && n != "" && n != "-" {
			label = n
		}
		parts = append(parts, fmt.Sprintf("%s(%s)", label, *bdm.DeviceName))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}

// resolveAttachedVolumeNames looks up the Name tag for every non-root volume
// attached to the given instances in a single DescribeVolumes call.
func resolveAttachedVolumeNames(ctx context.Context, client *ec2.Client, instances []types.Instance) map[string]string {
	names := map[string]string{}
	var ids []string
	for _, inst := range instances {
		rootDevice := ""
		if inst.RootDeviceName != nil {
			rootDevice = *inst.RootDeviceName
		}
		for _, bdm := range inst.BlockDeviceMappings {
			if bdm.DeviceName == nil || bdm.Ebs == nil || bdm.Ebs.VolumeId == nil {
				continue
			}
			if *bdm.DeviceName == rootDevice {
				continue
			}
			ids = append(ids, *bdm.Ebs.VolumeId)
		}
	}
	if len(ids) == 0 {
		return names
	}
	desc, err := client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: ids})
	if err != nil {
		return names // best-effort; fall back to IDs
	}
	for _, v := range desc.Volumes {
		if v.VolumeId != nil {
			names[*v.VolumeId] = awsutil.NameTag(v.Tags)
		}
	}
	return names
}

// latestSpotPrice returns the most recent spot price for an instance type in
// an AZ. Best-effort: returns "" on any error or when no data is available.
func latestSpotPrice(ctx context.Context, client *ec2.Client, instanceType, az string) (string, error) {
	if instanceType == "" || az == "" {
		return "", nil
	}
	out, err := client.DescribeSpotPriceHistory(ctx, &ec2.DescribeSpotPriceHistoryInput{
		InstanceTypes:       []types.InstanceType{types.InstanceType(instanceType)},
		AvailabilityZone:    aws.String(az),
		ProductDescriptions: []string{"Linux/UNIX"},
	})
	if err != nil {
		return "", err
	}
	if len(out.SpotPriceHistory) == 0 {
		return "", nil
	}
	// Most recent timestamp wins.
	sort.Slice(out.SpotPriceHistory, func(i, j int) bool {
		ti, tj := out.SpotPriceHistory[i].Timestamp, out.SpotPriceHistory[j].Timestamp
		if ti == nil || tj == nil {
			return false
		}
		return ti.After(*tj)
	})
	price := ""
	if out.SpotPriceHistory[0].SpotPrice != nil {
		price = *out.SpotPriceHistory[0].SpotPrice
	}
	return price, nil
}
