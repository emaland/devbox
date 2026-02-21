package cmd

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/spf13/cobra"

	"github.com/emaland/devbox/internal/awsutil"
	"github.com/emaland/devbox/internal/config"
)

func newResizeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resize <instance-id> <new-type>",
		Short: "Stop instance, change type, restart, update DNS",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			r53client := route53.NewFromConfig(awsCfg)
			return resizeInstance(cmd.Context(), dcfg, ec2Client, r53client, args[0], args[1])
		},
	}
}

func resizeInstance(ctx context.Context, dcfg config.DevboxConfig, client *ec2.Client, r53client *route53.Client, instanceID, newType string) error {
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
	currentType := string(inst.InstanceType)
	state := inst.State.Name

	fmt.Printf("Instance %s: type=%s state=%s\n", instanceID, currentType, state)

	if currentType == newType {
		fmt.Println("Already the requested type, nothing to do.")
		return nil
	}

	// Spot instances don't support ModifyInstanceAttribute for type changes.
	// We need to terminate and recreate with the new type.
	if inst.SpotInstanceRequestId != nil {
		return resizeSpotInstance(ctx, dcfg, client, r53client, inst, newType)
	}

	// On-demand path: stop → modify → start
	if state == types.InstanceStateNameRunning || state == types.InstanceStateNamePending {
		fmt.Printf("Stopping instance %s...\n", instanceID)
		_, err := client.StopInstances(ctx, &ec2.StopInstancesInput{
			InstanceIds: []string{instanceID},
		})
		if err != nil {
			return fmt.Errorf("stopping instance: %w", err)
		}
		waiter := ec2.NewInstanceStoppedWaiter(client)
		if err := waiter.Wait(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: []string{instanceID},
		}, 5*time.Minute); err != nil {
			return fmt.Errorf("waiting for instance to stop: %w", err)
		}
		fmt.Println("Instance stopped.")
	} else if state != types.InstanceStateNameStopped {
		return fmt.Errorf("instance is in state %s, cannot resize", state)
	}

	fmt.Printf("Changing instance type from %s to %s...\n", currentType, newType)
	_, err = client.ModifyInstanceAttribute(ctx, &ec2.ModifyInstanceAttributeInput{
		InstanceId: aws.String(instanceID),
		InstanceType: &types.AttributeValue{
			Value: aws.String(newType),
		},
	})
	if err != nil {
		return fmt.Errorf("modifying instance type: %w", err)
	}

	fmt.Printf("Starting instance %s...\n", instanceID)
	_, err = client.StartInstances(ctx, &ec2.StartInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return fmt.Errorf("starting instance: %w", err)
	}
	waiter := ec2.NewInstanceRunningWaiter(client)
	if err := waiter.Wait(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	}, 5*time.Minute); err != nil {
		return fmt.Errorf("waiting for instance to start: %w", err)
	}
	fmt.Println("Instance running.")

	if err := updateDNS(ctx, dcfg, client, r53client, instanceID, dcfg.DNSName); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: DNS update failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "The NixOS boot service should update DNS automatically.")
	}

	return nil
}

// resizeSpotInstance replaces a spot instance with a new one of a different type.
// Spot instances don't support ModifyInstanceAttribute for type changes, so we
// terminate the old instance and launch a new one, preserving non-root EBS volumes.
func resizeSpotInstance(ctx context.Context, dcfg config.DevboxConfig, client *ec2.Client, r53client *route53.Client, inst types.Instance, newType string) error {
	instanceID := *inst.InstanceId
	state := inst.State.Name
	az := *inst.Placement.AvailabilityZone

	fmt.Println("Spot instance detected — will replace instance with new type.")

	// 1. Stop if running
	if state == types.InstanceStateNameRunning || state == types.InstanceStateNamePending {
		fmt.Printf("Stopping instance %s...\n", instanceID)
		_, err := client.StopInstances(ctx, &ec2.StopInstancesInput{
			InstanceIds: []string{instanceID},
		})
		if err != nil {
			return fmt.Errorf("stopping instance: %w", err)
		}
		waiter := ec2.NewInstanceStoppedWaiter(client)
		if err := waiter.Wait(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: []string{instanceID},
		}, 5*time.Minute); err != nil {
			return fmt.Errorf("waiting for instance to stop: %w", err)
		}
		fmt.Println("Instance stopped.")
	} else if state != types.InstanceStateNameStopped {
		return fmt.Errorf("instance is in state %s, cannot resize", state)
	}

	// 2. Gather instance config for recreation
	imageID := ""
	if inst.ImageId != nil {
		imageID = *inst.ImageId
	}
	keyName := ""
	if inst.KeyName != nil {
		keyName = *inst.KeyName
	}
	subnetID := ""
	if inst.SubnetId != nil {
		subnetID = *inst.SubnetId
	}
	var sgIDs []string
	for _, sg := range inst.SecurityGroups {
		if sg.GroupId != nil {
			sgIDs = append(sgIDs, *sg.GroupId)
		}
	}
	var iamProfile *types.IamInstanceProfileSpecification
	if inst.IamInstanceProfile != nil && inst.IamInstanceProfile.Arn != nil {
		iamProfile = &types.IamInstanceProfileSpecification{
			Arn: inst.IamInstanceProfile.Arn,
		}
	}

	// Get user_data and patch it to ensure the amazon-image.nix import is present.
	// Stale user_data may be missing it, which prevents nixos-rebuild on first boot.
	userData, err := awsutil.FetchUserData(ctx, client, instanceID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not fetch user_data: %v\n", err)
		userData = ""
	}
	if userData != "" {
		userData = patchNixOSUserData(userData, awsutil.NameTag(inst.Tags))
	}

	// Get spot max price from the spot request
	maxPrice := dcfg.DefaultMaxPrice
	if inst.SpotInstanceRequestId != nil {
		spotDesc, err := client.DescribeSpotInstanceRequests(ctx, &ec2.DescribeSpotInstanceRequestsInput{
			SpotInstanceRequestIds: []string{*inst.SpotInstanceRequestId},
		})
		if err == nil && len(spotDesc.SpotInstanceRequests) > 0 {
			if spotDesc.SpotInstanceRequests[0].SpotPrice != nil {
				maxPrice = *spotDesc.SpotInstanceRequests[0].SpotPrice
			}
		}
	}

	// Collect tags (excluding aws: prefix)
	var instanceTags []types.Tag
	for _, t := range inst.Tags {
		if t.Key != nil && !strings.HasPrefix(*t.Key, "aws:") {
			instanceTags = append(instanceTags, t)
		}
	}

	// 3. Identify non-root EBS volumes to reattach later
	type volumeAttachment struct {
		VolumeID string
		Device   string
	}
	rootDevice := ""
	if inst.RootDeviceName != nil {
		rootDevice = *inst.RootDeviceName
	}
	var extraVolumes []volumeAttachment
	for _, bdm := range inst.BlockDeviceMappings {
		if bdm.DeviceName == nil || bdm.Ebs == nil || bdm.Ebs.VolumeId == nil {
			continue
		}
		if *bdm.DeviceName == rootDevice {
			continue
		}
		extraVolumes = append(extraVolumes, volumeAttachment{
			VolumeID: *bdm.Ebs.VolumeId,
			Device:   *bdm.DeviceName,
		})
	}

	// 4. Launch new spot instance with new type.
	//    We launch BEFORE touching the old instance so that if this fails
	//    (e.g. InsufficientInstanceCapacity), the old instance, its spot
	//    request, and its volumes are all still intact.
	fmt.Printf("Launching new %s spot instance in %s...\n", newType, az)

	runInput := &ec2.RunInstancesInput{
		ImageId:          aws.String(imageID),
		InstanceType:     types.InstanceType(newType),
		MinCount:         aws.Int32(1),
		MaxCount:         aws.Int32(1),
		SecurityGroupIds: sgIDs,
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
	}
	if keyName != "" {
		runInput.KeyName = aws.String(keyName)
	}
	if subnetID != "" {
		runInput.SubnetId = aws.String(subnetID)
	}
	if iamProfile != nil {
		runInput.IamInstanceProfile = iamProfile
	}
	if userData != "" {
		runInput.UserData = aws.String(userData)
	}
	if len(instanceTags) > 0 {
		runInput.TagSpecifications = []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeInstance,
				Tags:         instanceTags,
			},
		}
	}

	result, err := client.RunInstances(ctx, runInput)
	if err != nil {
		return fmt.Errorf("launching new instance (old instance %s is still intact): %w", instanceID, err)
	}

	newID := *result.Instances[0].InstanceId
	fmt.Printf("New instance %s launched, waiting for running state...\n", newID)

	runWaiter := ec2.NewInstanceRunningWaiter(client)
	if err := runWaiter.Wait(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{newID},
	}, 5*time.Minute); err != nil {
		return fmt.Errorf("waiting for new instance to start (old instance %s still intact): %w", instanceID, err)
	}
	fmt.Println("New instance running — spot capacity confirmed.")

	// 5. Stop the new instance so we can attach volumes before it boots for real.
	//    NixOS expects the data volume present at boot (mounts, home dirs, SSH keys).
	fmt.Printf("Stopping new instance %s for volume swap...\n", newID)
	_, err = client.StopInstances(ctx, &ec2.StopInstancesInput{
		InstanceIds: []string{newID},
	})
	if err != nil {
		return fmt.Errorf("stopping new instance: %w", err)
	}
	stopWaiter := ec2.NewInstanceStoppedWaiter(client)
	if err := stopWaiter.Wait(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{newID},
	}, 5*time.Minute); err != nil {
		return fmt.Errorf("waiting for new instance to stop: %w", err)
	}

	// 5b. Update the instance's user_data so amazon-init applies the
	//     patched config (with imports, hostname, etc.) on every future boot.
	//     userData is base64-encoded; BlobAttributeValue.Value wants raw bytes
	//     (the SDK handles base64 encoding), so we decode first.
	if userData != "" {
		rawUserData, decErr := base64.StdEncoding.DecodeString(userData)
		if decErr == nil {
			_, err := client.ModifyInstanceAttribute(ctx, &ec2.ModifyInstanceAttributeInput{
				InstanceId: aws.String(newID),
				UserData: &types.BlobAttributeValue{
					Value: rawUserData,
				},
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not update user_data on new instance: %v\n", err)
			}
		}
	}

	// 6. Cancel the old spot request now that replacement is confirmed.
	if inst.SpotInstanceRequestId != nil {
		fmt.Printf("Canceling old spot request %s...\n", *inst.SpotInstanceRequestId)
		_, err := client.CancelSpotInstanceRequests(ctx, &ec2.CancelSpotInstanceRequestsInput{
			SpotInstanceRequestIds: []string{*inst.SpotInstanceRequestId},
		})
		if err != nil {
			return fmt.Errorf("canceling spot request: %w", err)
		}
	}

	// 7. Detach volumes from old instance
	for _, vol := range extraVolumes {
		fmt.Printf("Detaching volume %s (%s) from old instance...\n", vol.VolumeID, vol.Device)
		_, err := client.DetachVolume(ctx, &ec2.DetachVolumeInput{
			VolumeId:   aws.String(vol.VolumeID),
			InstanceId: aws.String(instanceID),
		})
		if err != nil {
			return fmt.Errorf("detaching volume %s: %w", vol.VolumeID, err)
		}
	}
	for _, vol := range extraVolumes {
		if err := awsutil.PollVolumeState(ctx, client, vol.VolumeID, "available", VolumePollInterval, 2*time.Minute); err != nil {
			return fmt.Errorf("waiting for volume %s to detach: %w", vol.VolumeID, err)
		}
	}

	// 8. Terminate old instance
	fmt.Printf("Terminating old instance %s...\n", instanceID)
	_, err = client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return fmt.Errorf("terminating old instance: %w", err)
	}
	termWaiter := ec2.NewInstanceTerminatedWaiter(client)
	if err := termWaiter.Wait(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	}, 5*time.Minute); err != nil {
		return fmt.Errorf("waiting for old instance to terminate: %w", err)
	}

	// 9. Attach volumes to new (stopped) instance, then start it.
	//    This way NixOS boots with the data volume present from the start.
	for _, vol := range extraVolumes {
		fmt.Printf("Attaching volume %s as %s to new instance...\n", vol.VolumeID, vol.Device)
		_, err := client.AttachVolume(ctx, &ec2.AttachVolumeInput{
			VolumeId:   aws.String(vol.VolumeID),
			InstanceId: aws.String(newID),
			Device:     aws.String(vol.Device),
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to attach volume %s: %v\n", vol.VolumeID, err)
			continue
		}
	}
	for _, vol := range extraVolumes {
		if err := awsutil.PollVolumeState(ctx, client, vol.VolumeID, "in-use", VolumePollInterval, 2*time.Minute); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: timeout waiting for volume %s to attach: %v\n", vol.VolumeID, err)
		}
	}

	// 10. Start new instance with volumes attached
	fmt.Printf("Starting instance %s...\n", newID)
	_, err = client.StartInstances(ctx, &ec2.StartInstancesInput{
		InstanceIds: []string{newID},
	})
	if err != nil {
		return fmt.Errorf("starting new instance: %w", err)
	}
	startWaiter := ec2.NewInstanceRunningWaiter(client)
	if err := startWaiter.Wait(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{newID},
	}, 5*time.Minute); err != nil {
		return fmt.Errorf("waiting for new instance to start: %w", err)
	}
	fmt.Println("Instance running.")

	// 11. Update DNS
	if err := updateDNS(ctx, dcfg, client, r53client, newID, dcfg.DNSName); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: DNS update failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "The NixOS boot service should update DNS automatically.")
	}

	fmt.Printf("\nDone. Old instance %s terminated, new instance %s (%s) is running.\n", instanceID, newID, newType)
	return nil
}

// patchNixOSUserData decodes base64 user_data, ensures the NixOS configuration
// has the amazon-image.nix import, modulesPath arg, and hostname, then re-encodes.
// This fixes stale user_data that would cause nixos-rebuild to fail on first boot.
func patchNixOSUserData(b64data, spawnName string) string {
	decoded, err := base64.StdEncoding.DecodeString(b64data)
	if err != nil {
		return b64data
	}
	content := string(decoded)

	// Only patch if it looks like a NixOS config
	if !strings.Contains(content, "config, pkgs") {
		return b64data
	}

	// Ensure modulesPath is in the function args
	if !strings.Contains(content, "modulesPath") {
		content = strings.Replace(content,
			"{ config, pkgs, lib, ... }:",
			"{ config, pkgs, lib, modulesPath, ... }:", 1)
	}

	// Ensure amazon-image.nix import exists
	if !strings.Contains(content, "amazon-image.nix") {
		content = strings.Replace(content,
			"\n{\n",
			"\n{\n  imports = [ \"${modulesPath}/virtualisation/amazon-image.nix\" ];\n", 1)
	}

	// Ensure networking.hostName is set
	if !strings.Contains(content, "networking.hostName") {
		hostname := "dev-workstation"
		if spawnName != "" {
			hostname = spawnName
		}
		// Insert after the imports line, or after the opening brace
		if idx := strings.Index(content, "imports = ["); idx != -1 {
			// Find end of imports line
			if nl := strings.Index(content[idx:], "\n"); nl != -1 {
				pos := idx + nl + 1
				content = content[:pos] + fmt.Sprintf("  networking.hostName = %q;\n", hostname) + content[pos:]
			}
		}
	}

	return base64.StdEncoding.EncodeToString([]byte(content))
}
