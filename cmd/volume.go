package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spf13/cobra"

	"github.com/emaland/devbox/internal/awsutil"
	"github.com/emaland/devbox/internal/config"
)

func newVolumeCmd() *cobra.Command {
	vol := &cobra.Command{
		Use:   "volume",
		Short: "Manage EBS volumes (ls, create, attach, detach, snapshot, snapshots, destroy, move)",
	}

	vol.AddCommand(
		newVolumeLSCmd(),
		newVolumeCreateCmd(),
		newVolumeAttachCmd(),
		newVolumeDetachCmd(),
		newVolumeSnapshotCmd(),
		newVolumeSnapshotsCmd(),
		newVolumeDestroyCmd(),
		newVolumeMoveCmd(),
	)

	return vol
}

// --- ls ---

func newVolumeLSCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List EBS volumes",
		RunE: func(cmd *cobra.Command, args []string) error {
			return volumeLS(cmd.Context(), ec2Client)
		},
	}
}

func volumeLS(ctx context.Context, client *ec2.Client) error {
	result, err := client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{})
	if err != nil {
		return fmt.Errorf("describing volumes: %w", err)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "VOLUME ID\tNAME\tSIZE\tTYPE\tIOPS\tSTATE\tAZ\tATTACHED TO")
	for _, v := range result.Volumes {
		name := awsutil.NameTag(v.Tags)
		attached := "-"
		if len(v.Attachments) > 0 {
			attached = *v.Attachments[0].InstanceId
		}
		iops := "-"
		if v.Iops != nil {
			iops = fmt.Sprintf("%d", *v.Iops)
		}
		fmt.Fprintf(w, "%s\t%s\t%d GiB\t%s\t%s\t%s\t%s\t%s\n",
			*v.VolumeId,
			name,
			*v.Size,
			string(v.VolumeType),
			iops,
			string(v.State),
			*v.AvailabilityZone,
			attached,
		)
	}
	w.Flush()
	return nil
}

// --- create ---

func newVolumeCreateCmd() *cobra.Command {
	var (
		size       int
		volType    string
		iops       int
		throughput int
		az         string
		name       string
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new EBS volume",
		RunE: func(cmd *cobra.Command, args []string) error {
			if az == "" {
				az = dcfg.DefaultAZ
			}
			return volumeCreate(cmd.Context(), dcfg, ec2Client, size, volType, iops, throughput, az, name)
		},
	}

	cmd.Flags().IntVar(&size, "size", 512, "Volume size in GiB")
	cmd.Flags().StringVar(&volType, "type", "gp3", "Volume type")
	cmd.Flags().IntVar(&iops, "iops", 3000, "IOPS")
	cmd.Flags().IntVar(&throughput, "throughput", 250, "Throughput MB/s")
	cmd.Flags().StringVar(&az, "az", "", "Availability zone (default from config)")
	cmd.Flags().StringVar(&name, "name", "dev-data-volume", "Name tag")

	return cmd
}

func volumeCreate(ctx context.Context, dcfg config.DevboxConfig, client *ec2.Client, size int, volType string, iops, throughput int, az, name string) error {
	input := &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(az),
		Size:             aws.Int32(int32(size)),
		VolumeType:       types.VolumeType(volType),
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeVolume,
				Tags: []types.Tag{
					{Key: aws.String("Name"), Value: aws.String(name)},
				},
			},
		},
	}
	if volType == "gp3" || volType == "io1" || volType == "io2" {
		input.Iops = aws.Int32(int32(iops))
	}
	if volType == "gp3" {
		input.Throughput = aws.Int32(int32(throughput))
	}

	result, err := client.CreateVolume(ctx, input)
	if err != nil {
		return fmt.Errorf("creating volume: %w", err)
	}
	volID := *result.VolumeId
	fmt.Printf("Created volume %s, waiting for available state...\n", volID)

	if err := awsutil.PollVolumeState(ctx, client, volID, "available", VolumePollInterval, 2*time.Minute); err != nil {
		return err
	}
	fmt.Printf("Volume %s is available.\n", volID)
	return nil
}

// --- attach ---

func newVolumeAttachCmd() *cobra.Command {
	var device string

	cmd := &cobra.Command{
		Use:   "attach <volume> <instance-id>",
		Short: "Attach a volume to an instance",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return volumeAttach(cmd.Context(), ec2Client, args[0], args[1], device)
		},
	}

	cmd.Flags().StringVar(&device, "device", "/dev/xvdf", "Device name")

	return cmd
}

func volumeAttach(ctx context.Context, client *ec2.Client, volumeRef, instanceID, device string) error {
	volID, err := resolveVolume(ctx, client, volumeRef)
	if err != nil {
		return err
	}

	_, err = client.AttachVolume(ctx, &ec2.AttachVolumeInput{
		VolumeId:   aws.String(volID),
		InstanceId: aws.String(instanceID),
		Device:     aws.String(device),
	})
	if err != nil {
		return fmt.Errorf("attaching volume: %w", err)
	}
	fmt.Printf("Attaching %s to %s as %s, waiting...\n", volID, instanceID, device)

	if err := awsutil.PollVolumeState(ctx, client, volID, "in-use", VolumePollInterval, 2*time.Minute); err != nil {
		return err
	}
	fmt.Println("Volume attached.")
	return nil
}

// --- detach ---

func newVolumeDetachCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "detach <volume>",
		Short: "Detach a volume",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return volumeDetach(cmd.Context(), ec2Client, args[0], force)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "Force detach")

	return cmd
}

func volumeDetach(ctx context.Context, client *ec2.Client, volumeRef string, force bool) error {
	volID, err := resolveVolume(ctx, client, volumeRef)
	if err != nil {
		return err
	}

	_, err = client.DetachVolume(ctx, &ec2.DetachVolumeInput{
		VolumeId: aws.String(volID),
		Force:    aws.Bool(force),
	})
	if err != nil {
		return fmt.Errorf("detaching volume: %w", err)
	}
	fmt.Printf("Detaching %s, waiting...\n", volID)

	if err := awsutil.PollVolumeState(ctx, client, volID, "available", VolumePollInterval, 2*time.Minute); err != nil {
		return err
	}
	fmt.Println("Volume detached.")
	return nil
}

// --- snapshot ---

func newVolumeSnapshotCmd() *cobra.Command {
	var name string

	cmd := &cobra.Command{
		Use:   "snapshot <volume>",
		Short: "Create a snapshot of a volume",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return volumeSnapshot(cmd.Context(), ec2Client, args[0], name)
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Description/tag for the snapshot")

	return cmd
}

func volumeSnapshot(ctx context.Context, client *ec2.Client, volumeRef, name string) error {
	volID, err := resolveVolume(ctx, client, volumeRef)
	if err != nil {
		return err
	}

	input := &ec2.CreateSnapshotInput{
		VolumeId: aws.String(volID),
	}
	if name != "" {
		input.Description = aws.String(name)
		input.TagSpecifications = []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeSnapshot,
				Tags: []types.Tag{
					{Key: aws.String("Name"), Value: aws.String(name)},
				},
			},
		}
	}

	result, err := client.CreateSnapshot(ctx, input)
	if err != nil {
		return fmt.Errorf("creating snapshot: %w", err)
	}
	fmt.Printf("Snapshot %s started for volume %s.\n", *result.SnapshotId, volID)
	fmt.Println("Snapshots can take a while. Check progress with: devbox volume snapshots")
	return nil
}

// --- snapshots ---

func newVolumeSnapshotsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "snapshots",
		Short: "List snapshots",
		RunE: func(cmd *cobra.Command, args []string) error {
			return volumeSnapshots(cmd.Context(), ec2Client)
		},
	}
}

func volumeSnapshots(ctx context.Context, client *ec2.Client) error {
	result, err := client.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{
		OwnerIds: []string{"self"},
	})
	if err != nil {
		return fmt.Errorf("describing snapshots: %w", err)
	}

	if len(result.Snapshots) == 0 {
		fmt.Println("No snapshots found.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SNAPSHOT ID\tVOLUME ID\tSIZE\tSTATE\tPROGRESS\tDESCRIPTION\tCREATED")
	for _, s := range result.Snapshots {
		desc := "-"
		if s.Description != nil && *s.Description != "" {
			desc = *s.Description
		}
		created := "-"
		if s.StartTime != nil {
			created = s.StartTime.Format("2006-01-02 15:04")
		}
		progress := "-"
		if s.Progress != nil {
			progress = *s.Progress
		}
		fmt.Fprintf(w, "%s\t%s\t%d GiB\t%s\t%s\t%s\t%s\n",
			*s.SnapshotId,
			*s.VolumeId,
			*s.VolumeSize,
			string(s.State),
			progress,
			desc,
			created,
		)
	}
	w.Flush()
	return nil
}

// --- destroy ---

func newVolumeDestroyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "destroy <volume>",
		Short: "Delete a volume (must be detached)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return volumeDestroy(cmd.Context(), ec2Client, args[0])
		},
	}
}

func volumeDestroy(ctx context.Context, client *ec2.Client, volumeRef string) error {
	volID, err := resolveVolume(ctx, client, volumeRef)
	if err != nil {
		return err
	}

	_, err = client.DeleteVolume(ctx, &ec2.DeleteVolumeInput{
		VolumeId: aws.String(volID),
	})
	if err != nil {
		return fmt.Errorf("deleting volume: %w", err)
	}
	fmt.Printf("Volume %s deleted.\n", volID)
	return nil
}

// --- move ---

func newVolumeMoveCmd() *cobra.Command {
	var (
		targetAZ string
		cleanup  bool
	)

	cmd := &cobra.Command{
		Use:   "move <volume> <target-region>",
		Short: "Move a volume to another region",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return volumeMove(cmd.Context(), ec2Client, awsCfg, args[0], args[1], targetAZ, cleanup)
		},
	}

	cmd.Flags().StringVar(&targetAZ, "az", "", "Target AZ (default: <region>a)")
	cmd.Flags().BoolVar(&cleanup, "cleanup", false, "Delete intermediate snapshots after move")

	return cmd
}

func volumeMove(ctx context.Context, client *ec2.Client, cfg aws.Config, volumeRef, targetRegion, targetAZ string, cleanup bool) error {
	volID, err := resolveVolume(ctx, client, volumeRef)
	if err != nil {
		return err
	}

	if targetAZ == "" {
		targetAZ = targetRegion + "a"
	}

	// Describe the source volume to preserve its attributes
	descVol, err := client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		VolumeIds: []string{volID},
	})
	if err != nil {
		return fmt.Errorf("describing source volume: %w", err)
	}
	if len(descVol.Volumes) == 0 {
		return fmt.Errorf("volume %s not found", volID)
	}
	srcVol := descVol.Volumes[0]
	sourceRegion := cfg.Region

	// Step 1: Create snapshot in source region
	fmt.Printf("Creating snapshot of %s in %s...\n", volID, sourceRegion)
	snap, err := client.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{
		VolumeId:    aws.String(volID),
		Description: aws.String(fmt.Sprintf("devbox move: %s -> %s", volID, targetRegion)),
	})
	if err != nil {
		return fmt.Errorf("creating source snapshot: %w", err)
	}
	srcSnapID := *snap.SnapshotId
	fmt.Printf("Source snapshot: %s\n", srcSnapID)

	fmt.Println("Waiting for source snapshot to complete...")
	if err := pollSnapshotState(ctx, client, srcSnapID, "completed", SnapshotPollInterval, 30*time.Minute); err != nil {
		return fmt.Errorf("waiting for source snapshot: %w", err)
	}
	fmt.Println("Source snapshot completed.")

	// Step 2: Create client for target region
	loadOpts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(targetRegion)}
	if BaseEndpointOverride != "" {
		loadOpts = append(loadOpts, awsconfig.WithBaseEndpoint(BaseEndpointOverride))
	}
	targetCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return fmt.Errorf("loading config for region %s: %w", targetRegion, err)
	}
	targetClient := ec2.NewFromConfig(targetCfg)

	// Step 3: Copy snapshot to target region
	fmt.Printf("Copying snapshot to %s...\n", targetRegion)
	copyResult, err := targetClient.CopySnapshot(ctx, &ec2.CopySnapshotInput{
		SourceRegion:     aws.String(sourceRegion),
		SourceSnapshotId: aws.String(srcSnapID),
		Description:      aws.String(fmt.Sprintf("devbox move: %s from %s", volID, sourceRegion)),
	})
	if err != nil {
		return fmt.Errorf("copying snapshot to %s: %w", targetRegion, err)
	}
	dstSnapID := *copyResult.SnapshotId
	fmt.Printf("Target snapshot: %s\n", dstSnapID)

	fmt.Println("Waiting for target snapshot to complete...")
	if err := pollSnapshotState(ctx, targetClient, dstSnapID, "completed", SnapshotPollInterval, 30*time.Minute); err != nil {
		return fmt.Errorf("waiting for target snapshot: %w", err)
	}
	fmt.Println("Target snapshot completed.")

	// Step 4: Create volume from copied snapshot
	createInput := &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(targetAZ),
		SnapshotId:       aws.String(dstSnapID),
		Size:             srcVol.Size,
		VolumeType:       srcVol.VolumeType,
	}
	if srcVol.Iops != nil {
		createInput.Iops = srcVol.Iops
	}
	if srcVol.Throughput != nil {
		createInput.Throughput = srcVol.Throughput
	}
	// Copy tags from source volume
	if len(srcVol.Tags) > 0 {
		createInput.TagSpecifications = []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeVolume,
				Tags:         srcVol.Tags,
			},
		}
	}

	fmt.Printf("Creating volume in %s...\n", targetAZ)
	newVol, err := targetClient.CreateVolume(ctx, createInput)
	if err != nil {
		return fmt.Errorf("creating volume in target region: %w", err)
	}
	newVolID := *newVol.VolumeId

	if err := awsutil.PollVolumeState(ctx, targetClient, newVolID, "available", VolumePollInterval, 2*time.Minute); err != nil {
		return fmt.Errorf("waiting for new volume: %w", err)
	}

	fmt.Printf("\nVolume moved successfully!\n")
	fmt.Printf("  New volume: %s in %s\n", newVolID, targetAZ)

	// Step 5: Cleanup intermediate snapshots if requested
	if cleanup {
		fmt.Println("Cleaning up intermediate snapshots...")
		if _, err := client.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{
			SnapshotId: aws.String(srcSnapID),
		}); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to delete source snapshot %s: %v\n", srcSnapID, err)
		} else {
			fmt.Printf("  Deleted source snapshot %s\n", srcSnapID)
		}
		if _, err := targetClient.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{
			SnapshotId: aws.String(dstSnapID),
		}); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to delete target snapshot %s: %v\n", dstSnapID, err)
		} else {
			fmt.Printf("  Deleted target snapshot %s\n", dstSnapID)
		}
	}

	return nil
}

// --- helpers ---

func resolveVolume(ctx context.Context, client *ec2.Client, nameOrID string) (string, error) {
	if strings.HasPrefix(nameOrID, "vol-") {
		return nameOrID, nil
	}
	result, err := client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		Filters: []types.Filter{
			{Name: aws.String("tag:Name"), Values: []string{nameOrID}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("looking up volume by name %q: %w", nameOrID, err)
	}
	if len(result.Volumes) == 0 {
		return "", fmt.Errorf("no volume found with name %q", nameOrID)
	}
	if len(result.Volumes) > 1 {
		var ids []string
		for _, v := range result.Volumes {
			ids = append(ids, *v.VolumeId)
		}
		return "", fmt.Errorf("multiple volumes found with name %q: %s — use the volume ID instead", nameOrID, strings.Join(ids, ", "))
	}
	return *result.Volumes[0].VolumeId, nil
}

func pollSnapshotState(ctx context.Context, client *ec2.Client, snapshotID, desiredState string, interval, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for snapshot %s to reach state %q", snapshotID, desiredState)
		}
		result, err := client.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{
			SnapshotIds: []string{snapshotID},
		})
		if err != nil {
			return fmt.Errorf("polling snapshot state: %w", err)
		}
		if len(result.Snapshots) > 0 {
			snap := result.Snapshots[0]
			state := string(snap.State)
			if state == desiredState {
				return nil
			}
			if snap.Progress != nil {
				fmt.Printf("  %s: %s (%s)\n", snapshotID, state, *snap.Progress)
			}
			if state == "error" {
				msg := ""
				if snap.StateMessage != nil {
					msg = ": " + *snap.StateMessage
				}
				return fmt.Errorf("snapshot %s failed%s", snapshotID, msg)
			}
		}
		time.Sleep(interval)
	}
}
