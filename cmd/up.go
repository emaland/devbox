package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/spf13/cobra"

	"github.com/emaland/devbox/internal/config"
)

func newUpCmd() *cobra.Command {
	var (
		instanceType string
		az           string
		name         string
		maxPrice     string
	)

	cmd := &cobra.Command{
		Use:     "up [instance-id...]",
		Aliases: []string{"start"},
		Short:   "Bring up the devbox (idempotent: create, attach volume, or just start)",
		Long: `Bring up the devbox. Idempotent — run it whenever you want the box ready:

  - No box yet:        ensures the data volume exists, then launches and attaches it.
  - Box stopped:       starts it and refreshes DNS.
  - Box already up:    prints its status.
  - Explicit IDs:      starts exactly those instances (like the old 'start').`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			r53client := route53.NewFromConfig(awsCfg)
			return upDevbox(cmd.Context(), dcfg, ec2Client, r53client, instanceType, az, name, maxPrice, args)
		},
	}

	cmd.Flags().StringVar(&instanceType, "type", "", "Instance type (default from config)")
	cmd.Flags().StringVar(&az, "az", "", "Availability zone (default from config)")
	cmd.Flags().StringVar(&name, "name", "", "Name tag for a newly launched instance (default from config)")
	cmd.Flags().StringVar(&maxPrice, "max-price", "", "Spot max price $/hr (default from config)")

	return cmd
}

func upDevbox(ctx context.Context, dcfg config.DevboxConfig, client *ec2.Client, r53client *route53.Client, instanceType, az, name, maxPrice string, args []string) error {
	// Explicit instance IDs: just start them (preserves `devbox start i-...`).
	if len(args) > 0 {
		return startInstances(ctx, client, args)
	}

	if az == "" {
		az = dcfg.DefaultAZ
	}

	id, state, err := autoDetectBox(ctx, client)
	if err != nil {
		return err
	}

	if id != "" {
		switch state {
		case types.InstanceStateNameRunning:
			fmt.Printf("devbox %s is already running.\n\n", id)
			return listInstances(ctx, client)

		case types.InstanceStateNameStopped:
			fmt.Printf("Starting devbox %s...\n", id)
			if err := startInstances(ctx, client, []string{id}); err != nil {
				return err
			}
			return finishUp(ctx, dcfg, client, r53client, id)

		case types.InstanceStateNameStopping:
			fmt.Printf("devbox %s is stopping; waiting for it to stop before starting...\n", id)
			if err := waitStopped(ctx, client, id); err != nil {
				return fmt.Errorf("waiting for instance to stop: %w", err)
			}
			if err := startInstances(ctx, client, []string{id}); err != nil {
				return err
			}
			return finishUp(ctx, dcfg, client, r53client, id)

		case types.InstanceStateNamePending:
			fmt.Printf("devbox %s is starting; waiting...\n", id)
			return finishUp(ctx, dcfg, client, r53client, id)

		default:
			fmt.Printf("devbox %s is in state %s.\n\n", id, state)
			return listInstances(ctx, client)
		}
	}

	// No box exists — ensure the data volume, then launch and attach it.
	if name == "" {
		name = dcfg.SpawnName
	}
	volID, err := ensureDataVolume(ctx, dcfg, client, az)
	if err != nil {
		return err
	}
	return spawnInstance(ctx, dcfg, client, instanceType, az, name, maxPrice, "", volID)
}

// finishUp waits for the instance to be running, refreshes DNS, and prints
// the resulting status.
func finishUp(ctx context.Context, dcfg config.DevboxConfig, client *ec2.Client, r53client *route53.Client, id string) error {
	if err := waitRunning(ctx, client, id); err != nil {
		return fmt.Errorf("waiting for instance to start: %w", err)
	}
	if err := updateDNS(ctx, dcfg, client, r53client, id, dcfg.DNSName); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: DNS update failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "The NixOS boot service should update DNS automatically.")
	}
	fmt.Println()
	return listInstances(ctx, client)
}

// ensureDataVolume finds the configured data volume by Name tag in the target
// AZ, creating it if absent. Errors if the volume exists in a different AZ
// (EBS volumes can't attach across AZs).
func ensureDataVolume(ctx context.Context, dcfg config.DevboxConfig, client *ec2.Client, az string) (string, error) {
	name := dcfg.DataVolumeName
	if name == "" {
		name = "dev-data-volume"
	}

	desc, err := client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		Filters: []types.Filter{
			{Name: aws.String("tag:Name"), Values: []string{name}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("looking up data volume %q: %w", name, err)
	}

	// Prefer a volume already in the target AZ if several share the name.
	var inAZ, other *types.Volume
	for i := range desc.Volumes {
		v := &desc.Volumes[i]
		if v.AvailabilityZone != nil && *v.AvailabilityZone == az {
			inAZ = v
			break
		}
		other = v
	}
	if inAZ != nil {
		fmt.Printf("Using existing data volume %s (%s) in %s.\n", *inAZ.VolumeId, name, az)
		return *inAZ.VolumeId, nil
	}
	if other != nil {
		otherAZ := "?"
		if other.AvailabilityZone != nil {
			otherAZ = *other.AvailabilityZone
		}
		targetRegion := az
		if len(az) > 1 {
			targetRegion = az[:len(az)-1] // strip the AZ letter to get the region
		}
		return "", fmt.Errorf("data volume %q (%s) is in %s but you're launching in %s — EBS volumes can't attach across AZs.\n"+
			"Either launch there with --az %s, or move the volume with: devbox volume move %s %s",
			name, *other.VolumeId, otherAZ, az, otherAZ, *other.VolumeId, targetRegion)
	}

	size := dcfg.DataVolumeSize
	if size == 0 {
		size = 512
	}
	fmt.Printf("No data volume named %q found — creating a %d GiB gp3 volume in %s...\n", name, size, az)
	volID, err := volumeCreate(ctx, dcfg, client, size, "gp3", 3000, 250, az, name)
	if err != nil {
		return "", err
	}
	return volID, nil
}
