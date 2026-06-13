package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spf13/cobra"

	"github.com/emaland/devbox/internal/awsutil"
	"github.com/emaland/devbox/internal/config"
)

func newSpawnCmd() *cobra.Command {
	var (
		instanceType string
		az           string
		name         string
		maxPrice     string
		from         string
		volumeID     string
	)

	cmd := &cobra.Command{
		Use:   "spawn",
		Short: "Spin up a new spot instance cloned from the primary",
		RunE: func(cmd *cobra.Command, args []string) error {
			return spawnInstance(cmd.Context(), dcfg, ec2Client, instanceType, az, name, maxPrice, from, volumeID)
		},
	}

	cmd.Flags().StringVar(&instanceType, "type", "", "Instance type (default from config)")
	cmd.Flags().StringVar(&az, "az", "", "Availability zone (default from config)")
	cmd.Flags().StringVar(&name, "name", "", "Name tag for the instance (default from config)")
	cmd.Flags().StringVar(&maxPrice, "max-price", "", "Spot max price $/hr (default from config)")
	cmd.Flags().StringVar(&from, "from", "", "Instance ID to clone user_data from")
	cmd.Flags().StringVar(&volumeID, "volume", "", "EBS data volume ID to attach as /dev/xvdf")

	return cmd
}

func spawnInstance(ctx context.Context, dcfg config.DevboxConfig, client *ec2.Client, instanceType, az, name, maxPrice, from, volumeID string) error {
	// Apply config defaults for empty flags
	if instanceType == "" {
		instanceType = dcfg.DefaultType
	}
	if az == "" {
		az = dcfg.DefaultAZ
	}
	if name == "" {
		name = dcfg.SpawnName
	}
	if maxPrice == "" {
		maxPrice = dcfg.DefaultMaxPrice
	}

	// Discover infrastructure
	fmt.Println("Looking up infrastructure...")

	amiID, err := lookupAMI(ctx, dcfg, client)
	if err != nil {
		return err
	}
	fmt.Printf("  AMI: %s\n", amiID)

	sgID, err := lookupSecurityGroup(ctx, dcfg, client)
	if err != nil {
		return err
	}
	fmt.Printf("  Security Group: %s\n", sgID)

	subnetID, err := lookupSubnet(ctx, client, az)
	if err != nil {
		return err
	}
	fmt.Printf("  Subnet: %s\n", subnetID)

	// Get user_data: clone from a source instance, or fall back to local configuration.nix
	var userData string
	if from != "" {
		fmt.Printf("  Cloning user_data from: %s\n", from)
		userData, err = awsutil.FetchUserData(ctx, client, from)
		if err != nil {
			return err
		}
	} else {
		sourceID, detectErr := autoDetectSourceInstance(ctx, client)
		if detectErr == nil {
			fmt.Printf("  Cloning user_data from: %s\n", sourceID)
			userData, err = awsutil.FetchUserData(ctx, client, sourceID)
			if err != nil {
				return err
			}
		} else {
			// No source instance — boot a minimal stub (the full configuration.nix
			// exceeds EC2's 16 KB user_data limit). The real config is pushed via
			// SSH after launch (pushNixConfig below).
			userData = stubUserData()
			fmt.Println("  No source instance — booting minimal stub; full config pushed after launch")
		}
	}

	// Launch the instance
	fmt.Printf("Launching %s spot instance in %s...\n", instanceType, az)

	runInput := &ec2.RunInstancesInput{
		ImageId:      aws.String(amiID),
		InstanceType: types.InstanceType(instanceType),
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		KeyName:      aws.String(dcfg.SSHKeyName),
		SubnetId:     aws.String(subnetID),
		SecurityGroupIds: []string{sgID},
		IamInstanceProfile: &types.IamInstanceProfileSpecification{
			Name: aws.String(dcfg.IAMProfile),
		},
		UserData: aws.String(userData),
		InstanceMarketOptions: &types.InstanceMarketOptionsRequest{
			MarketType: types.MarketTypeSpot,
			SpotOptions: &types.SpotMarketOptions{
				SpotInstanceType:             types.SpotInstanceTypePersistent,
				InstanceInterruptionBehavior: types.InstanceInterruptionBehaviorStop,
				MaxPrice:                     aws.String(maxPrice),
			},
		},
		BlockDeviceMappings: []types.BlockDeviceMapping{
			{
				DeviceName: aws.String("/dev/xvda"),
				Ebs: &types.EbsBlockDevice{
					VolumeSize: aws.Int32(75),
					VolumeType: types.VolumeTypeGp3,
				},
			},
		},
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeInstance,
				Tags: []types.Tag{
					{Key: aws.String("Name"), Value: aws.String(name)},
					{Key: aws.String("devbox-managed"), Value: aws.String("true")},
				},
			},
		},
	}

	result, err := client.RunInstances(ctx, runInput)
	if err != nil {
		return fmt.Errorf("launching instance: %w", err)
	}

	newID := *result.Instances[0].InstanceId
	fmt.Printf("Instance %s launched, waiting for running state...\n", newID)

	waiter := ec2.NewInstanceRunningWaiter(client)
	if err := waiter.Wait(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{newID},
	}, 5*60e9); err != nil {
		return fmt.Errorf("waiting for instance to start: %w", err)
	}

	// Attach data volume if requested — stop instance, attach, restart so
	// NixOS boots with /home mounted from the start.
	if volumeID != "" {
		fmt.Printf("Stopping instance to attach volume %s...\n", volumeID)
		_, err := client.StopInstances(ctx, &ec2.StopInstancesInput{
			InstanceIds: []string{newID},
		})
		if err != nil {
			return fmt.Errorf("stopping instance for volume attach: %w", err)
		}
		stopWaiter := ec2.NewInstanceStoppedWaiter(client)
		if err := stopWaiter.Wait(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: []string{newID},
		}, 5*60e9); err != nil {
			return fmt.Errorf("waiting for stop: %w", err)
		}

		fmt.Printf("Attaching volume %s as /dev/xvdf...\n", volumeID)
		_, err = client.AttachVolume(ctx, &ec2.AttachVolumeInput{
			VolumeId:   aws.String(volumeID),
			InstanceId: aws.String(newID),
			Device:     aws.String("/dev/xvdf"),
		})
		if err != nil {
			return fmt.Errorf("attaching volume: %w", err)
		}
		if err := awsutil.PollVolumeState(ctx, client, volumeID, "in-use", VolumePollInterval, 2*60e9); err != nil {
			return fmt.Errorf("waiting for volume attach: %w", err)
		}

		fmt.Println("Starting instance with volume attached...")
		if _, err := spotRetryStart(ctx, client, []string{newID}); err != nil {
			return fmt.Errorf("restarting instance: %w", err)
		}
		if err := waiter.Wait(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: []string{newID},
		}, 5*60e9); err != nil {
			return fmt.Errorf("waiting for instance to start: %w", err)
		}
	}

	// Re-describe to get public IP
	desc, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{newID},
	})
	if err != nil {
		return fmt.Errorf("describing new instance: %w", err)
	}
	newInst := desc.Reservations[0].Instances[0]
	publicIP := "-"
	if newInst.PublicIpAddress != nil {
		publicIP = *newInst.PublicIpAddress
	}

	// Push configuration.nix via root SSH and rebuild
	fmt.Println("Pushing NixOS configuration...")
	if err := pushNixConfig(ctx, dcfg, client, newID, volumeID); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to push NixOS config: %v\n", err)
		fmt.Fprintln(os.Stderr, "Run 'devbox nix-update' manually once the instance is ready.")
	}

	fmt.Printf("\nInstance ready:\n")
	fmt.Printf("  ID:        %s\n", newID)
	fmt.Printf("  Type:      %s\n", instanceType)
	fmt.Printf("  AZ:        %s\n", az)
	fmt.Printf("  Public IP: %s\n", publicIP)
	if publicIP != "-" {
		fmt.Printf("  SSH:       devbox ssh %s\n", newID)
	}
	return nil
}

func lookupAMI(ctx context.Context, dcfg config.DevboxConfig, client *ec2.Client) (string, error) {
	result, err := client.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners: []string{dcfg.NixOSAMIOwner},
		Filters: []types.Filter{
			{Name: aws.String("name"), Values: []string{dcfg.NixOSAMIPattern}},
			{Name: aws.String("architecture"), Values: []string{"x86_64"}},
			{Name: aws.String("state"), Values: []string{"available"}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("looking up AMI: %w", err)
	}
	if len(result.Images) == 0 {
		return "", fmt.Errorf("no NixOS AMI found matching pattern %q from owner %s", dcfg.NixOSAMIPattern, dcfg.NixOSAMIOwner)
	}
	// Pick the latest by sorting on name (NixOS AMI names include dates)
	sort.Slice(result.Images, func(i, j int) bool {
		return *result.Images[i].Name > *result.Images[j].Name
	})
	return *result.Images[0].ImageId, nil
}

func lookupSecurityGroup(ctx context.Context, dcfg config.DevboxConfig, client *ec2.Client) (string, error) {
	result, err := client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		GroupNames: []string{dcfg.SecurityGroup},
	})
	if err != nil {
		return "", fmt.Errorf("looking up security group: %w", err)
	}
	if len(result.SecurityGroups) == 0 {
		return "", fmt.Errorf("security group %q not found", dcfg.SecurityGroup)
	}
	return *result.SecurityGroups[0].GroupId, nil
}

func lookupSubnet(ctx context.Context, client *ec2.Client, az string) (string, error) {
	result, err := client.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []types.Filter{
			{Name: aws.String("availability-zone"), Values: []string{az}},
			{Name: aws.String("default-for-az"), Values: []string{"true"}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("looking up subnet: %w", err)
	}
	if len(result.Subnets) == 0 {
		return "", fmt.Errorf("no default subnet found for AZ %s", az)
	}
	return *result.Subnets[0].SubnetId, nil
}

func autoDetectSourceInstance(ctx context.Context, client *ec2.Client) (string, error) {
	desc, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{Name: aws.String("instance-lifecycle"), Values: []string{"spot"}},
			{Name: aws.String("instance-state-name"), Values: []string{"running", "stopped"}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("auto-detecting source instance: %w", err)
	}
	var ids []string
	for _, res := range desc.Reservations {
		for _, inst := range res.Instances {
			ids = append(ids, *inst.InstanceId)
		}
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("no running/stopped spot instances found to clone user_data from; use --from to specify")
	}
	if len(ids) > 1 {
		return "", fmt.Errorf("multiple spot instances found (%s); use --from to specify which one", strings.Join(ids, ", "))
	}
	return ids[0], nil
}
