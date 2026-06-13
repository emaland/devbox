package cmd

import (
	"context"
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

type volumeAttachment struct {
	VolumeID string
	Device   string
}

// Resize progress is recorded on the new instance via these tags so an
// interrupted resize can be resumed with `devbox resize --resume`.
const (
	resizeStateTag  = "devbox-resize-state"
	resizeSourceTag = "devbox-resize-source"
	resizeVolsTag   = "devbox-resize-volumes"
	resizeSpotTag   = "devbox-resize-spotreq"
	resizeTypeTag   = "devbox-resize-type"
)

// resizeStates lists the spot-resize steps in execution order. Each value is
// the state recorded *after* the corresponding step completes.
var resizeStates = []string{
	"launched",       // new instance launched (capacity not yet confirmed)
	"stopped",        // new instance confirmed running, then stopped for swap
	"spot-canceled",  // old spot request canceled
	"detached",       // data volumes detached from old instance
	"old-terminated", // old instance terminated, volumes available
	"attached",       // data volumes attached to new instance
	"started",        // new instance started
	"done",           // DNS updated, resize tags cleared
}

type resizePlan struct {
	newID     string
	oldID     string
	newType   string
	az        string
	spotReqID string
	volumes   []volumeAttachment
}

func newResizeCmd() *cobra.Command {
	var resume bool

	cmd := &cobra.Command{
		Use:   "resize [instance-id] <new-type>",
		Short: "Move the devbox to a different instance type (carries the data volume)",
		Long: `Change the devbox's instance type. For spot instances this launches a
replacement, swaps the data volume over, and terminates the old one — as a
resumable state machine. If it's interrupted, re-run with --resume.

  devbox resize m6i.2xlarge              Resize the auto-detected devbox
  devbox resize i-0abc... m6i.2xlarge    Resize a specific instance
  devbox resize --resume                 Continue an interrupted resize`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			r53client := route53.NewFromConfig(awsCfg)

			if resume {
				return resumeResize(ctx, dcfg, ec2Client, r53client)
			}

			var instanceID, newType string
			switch len(args) {
			case 1:
				newType = args[0]
				id, _, err := autoDetectBox(ctx, ec2Client)
				if err != nil {
					return err
				}
				if id == "" {
					return fmt.Errorf("no devbox instance found to resize")
				}
				instanceID = id
			case 2:
				instanceID, newType = args[0], args[1]
			default:
				return fmt.Errorf("usage: devbox resize [instance-id] <new-type>  (or --resume)")
			}
			return resizeInstance(ctx, dcfg, ec2Client, r53client, instanceID, newType)
		},
	}

	cmd.Flags().BoolVar(&resume, "resume", false, "Resume an interrupted resize")
	return cmd
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

	// Spot instances don't support ModifyInstanceAttribute for type changes,
	// so we launch a replacement and swap the data volume over.
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
		if err := waitStopped(ctx, client, instanceID); err != nil {
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
	if _, err := spotRetryStart(ctx, client, []string{instanceID}); err != nil {
		return fmt.Errorf("starting instance: %w", err)
	}
	if err := waitRunning(ctx, client, instanceID); err != nil {
		return fmt.Errorf("waiting for instance to start: %w", err)
	}
	fmt.Println("Instance running.")

	if err := updateDNS(ctx, dcfg, client, r53client, instanceID, dcfg.DNSName); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: DNS update failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "The NixOS boot service should update DNS automatically.")
	}

	return nil
}

// resizeSpotInstance replaces a spot instance with a new one of a different
// type, preserving non-root EBS volumes. It launches the replacement, tags it
// with the resize plan, and then drives the swap as a resumable state machine.
func resizeSpotInstance(ctx context.Context, dcfg config.DevboxConfig, client *ec2.Client, r53client *route53.Client, inst types.Instance, newType string) error {
	instanceID := *inst.InstanceId
	state := inst.State.Name
	az := *inst.Placement.AvailabilityZone

	fmt.Println("Spot instance detected — will replace it with a new type.")

	// 1. Stop the old instance so its data volume can be cleanly detached.
	if state == types.InstanceStateNameRunning || state == types.InstanceStateNamePending {
		fmt.Printf("Stopping instance %s...\n", instanceID)
		_, err := client.StopInstances(ctx, &ec2.StopInstancesInput{
			InstanceIds: []string{instanceID},
		})
		if err != nil {
			return fmt.Errorf("stopping instance: %w", err)
		}
		if err := waitStopped(ctx, client, instanceID); err != nil {
			return fmt.Errorf("waiting for instance to stop: %w", err)
		}
		fmt.Println("Instance stopped.")
	} else if state != types.InstanceStateNameStopped {
		return fmt.Errorf("instance is in state %s, cannot resize", state)
	}

	// 2. Gather config for the replacement. Verify the AMI still exists;
	//    fall back to the latest NixOS AMI if it was deregistered.
	imageID := ""
	if inst.ImageId != nil {
		imageID = *inst.ImageId
	}
	if imageID != "" {
		amiDesc, err := client.DescribeImages(ctx, &ec2.DescribeImagesInput{
			ImageIds: []string{imageID},
		})
		if err != nil || len(amiDesc.Images) == 0 {
			fmt.Printf("Original AMI %s no longer exists, looking up current NixOS AMI...\n", imageID)
			newAMI, lookupErr := lookupAMI(ctx, dcfg, client)
			if lookupErr != nil {
				return fmt.Errorf("original AMI gone and failed to find replacement: %w", lookupErr)
			}
			imageID = newAMI
			fmt.Printf("Using AMI: %s\n", imageID)
		}
	}

	var sgIDs []string
	for _, sg := range inst.SecurityGroups {
		if sg.GroupId != nil {
			sgIDs = append(sgIDs, *sg.GroupId)
		}
	}

	// Spot max price from the old request.
	maxPrice := dcfg.DefaultMaxPrice
	spotReqID := ""
	if inst.SpotInstanceRequestId != nil {
		spotReqID = *inst.SpotInstanceRequestId
		spotDesc, err := client.DescribeSpotInstanceRequests(ctx, &ec2.DescribeSpotInstanceRequestsInput{
			SpotInstanceRequestIds: []string{spotReqID},
		})
		if err == nil && len(spotDesc.SpotInstanceRequests) > 0 && spotDesc.SpotInstanceRequests[0].SpotPrice != nil {
			maxPrice = *spotDesc.SpotInstanceRequests[0].SpotPrice
		}
	}

	// Non-root EBS volumes to carry over.
	rootDevice := ""
	if inst.RootDeviceName != nil {
		rootDevice = *inst.RootDeviceName
	}
	var volumes []volumeAttachment
	for _, bdm := range inst.BlockDeviceMappings {
		if bdm.DeviceName == nil || bdm.Ebs == nil || bdm.Ebs.VolumeId == nil {
			continue
		}
		if *bdm.DeviceName == rootDevice {
			continue
		}
		volumes = append(volumes, volumeAttachment{VolumeID: *bdm.Ebs.VolumeId, Device: *bdm.DeviceName})
	}

	// Carry over the old root volume's size/type — the user may have grown it
	// (large Nix store / Docker images live on root, not /home). Fall back to
	// 75 GiB gp3 if it can't be determined.
	rootSize := int32(75)
	rootType := types.VolumeTypeGp3
	rootDeviceName := "/dev/xvda"
	if rootDevice != "" {
		rootDeviceName = rootDevice
	}
	for _, bdm := range inst.BlockDeviceMappings {
		if bdm.DeviceName == nil || *bdm.DeviceName != rootDevice || bdm.Ebs == nil || bdm.Ebs.VolumeId == nil {
			continue
		}
		if vd, err := client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{*bdm.Ebs.VolumeId}}); err == nil && len(vd.Volumes) > 0 {
			if vd.Volumes[0].Size != nil && *vd.Volumes[0].Size > 0 {
				rootSize = *vd.Volumes[0].Size
			}
			if vd.Volumes[0].VolumeType != "" {
				rootType = vd.Volumes[0].VolumeType
			}
		}
		break
	}

	// Boot the replacement with a minimal stub — the full configuration.nix is
	// too large for EC2 user_data. The real config (with the data-volume id for
	// the format service) is pushed via SSH after the box is up.
	userData := stubUserData()

	// Tags for the new instance: carry over non-aws tags, then add the resize
	// plan so the swap can be resumed if interrupted.
	tags := []types.Tag{}
	for _, t := range inst.Tags {
		if t.Key != nil && !strings.HasPrefix(*t.Key, "aws:") {
			tags = append(tags, t)
		}
	}
	tags = append(tags,
		types.Tag{Key: aws.String(resizeStateTag), Value: aws.String("launched")},
		types.Tag{Key: aws.String(resizeSourceTag), Value: aws.String(instanceID)},
		types.Tag{Key: aws.String(resizeTypeTag), Value: aws.String(newType)},
		types.Tag{Key: aws.String(resizeSpotTag), Value: aws.String(spotReqID)},
		types.Tag{Key: aws.String(resizeVolsTag), Value: aws.String(formatVolumes(volumes))},
	)

	// 3. Launch the replacement BEFORE touching the old instance, so a capacity
	//    failure leaves the old instance, its spot request, and volumes intact.
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
				DeviceName: aws.String(rootDeviceName),
				Ebs: &types.EbsBlockDevice{
					VolumeSize: aws.Int32(rootSize),
					VolumeType: rootType,
				},
			},
		},
		TagSpecifications: []types.TagSpecification{
			{ResourceType: types.ResourceTypeInstance, Tags: tags},
		},
	}
	if inst.KeyName != nil {
		runInput.KeyName = inst.KeyName
	}
	if inst.SubnetId != nil {
		runInput.SubnetId = inst.SubnetId
	}
	if inst.IamInstanceProfile != nil && inst.IamInstanceProfile.Arn != nil {
		runInput.IamInstanceProfile = &types.IamInstanceProfileSpecification{Arn: inst.IamInstanceProfile.Arn}
	}
	if userData != "" {
		runInput.UserData = aws.String(userData)
	}

	result, err := client.RunInstances(ctx, runInput)
	if err != nil {
		return fmt.Errorf("launching new instance (old instance %s is still intact): %w", instanceID, err)
	}
	newID := *result.Instances[0].InstanceId
	fmt.Printf("New instance %s launched.\n", newID)

	plan := resizePlan{
		newID:     newID,
		oldID:     instanceID,
		newType:   newType,
		az:        az,
		spotReqID: spotReqID,
		volumes:   volumes,
	}
	return driveResize(ctx, dcfg, client, r53client, plan, "launched")
}

// driveResize executes the spot-resize steps starting after fromState. Every
// step is idempotent so a resume from any recorded state is safe.
func driveResize(ctx context.Context, dcfg config.DevboxConfig, client *ec2.Client, r53client *route53.Client, plan resizePlan, fromState string) error {
	// An unrecognized state would make resizeStateIndex return -1, so every
	// resizeAfter() would be true and the whole machine (including terminate)
	// would re-run from scratch. Refuse instead.
	if resizeStateIndex(fromState) < 0 {
		return fmt.Errorf("unknown resize state %q on instance %s; refusing to proceed (inspect/clean the devbox-resize-* tags)", fromState, plan.newID)
	}
	cur := fromState
	advance := func(to string) error {
		if err := setResizeState(ctx, client, plan.newID, to); err != nil {
			return err
		}
		cur = to
		return nil
	}

	// launched → stopped: confirm capacity (running), then stop for the swap.
	if resizeAfter(cur, "stopped") {
		if err := waitRunning(ctx, client, plan.newID); err != nil {
			return fmt.Errorf("waiting for new instance (old %s still intact): %w", plan.oldID, err)
		}
		fmt.Println("New instance running — spot capacity confirmed.")
		st, _ := instanceState(ctx, client, plan.newID)
		if st != types.InstanceStateNameStopped {
			fmt.Printf("Stopping new instance %s for volume swap...\n", plan.newID)
			if _, err := client.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{plan.newID}}); err != nil {
				return fmt.Errorf("stopping new instance: %w", err)
			}
			if err := waitStopped(ctx, client, plan.newID); err != nil {
				return fmt.Errorf("waiting for new instance to stop: %w", err)
			}
		}
		if err := advance("stopped"); err != nil {
			return err
		}
	}

	// → spot-canceled: cancel the old persistent spot request.
	if resizeAfter(cur, "spot-canceled") {
		if plan.spotReqID != "" {
			fmt.Printf("Canceling old spot request %s...\n", plan.spotReqID)
			if _, err := client.CancelSpotInstanceRequests(ctx, &ec2.CancelSpotInstanceRequestsInput{
				SpotInstanceRequestIds: []string{plan.spotReqID},
			}); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not cancel spot request: %v\n", err)
			}
		}
		if err := advance("spot-canceled"); err != nil {
			return err
		}
	}

	// → detached: detach data volumes from the old instance.
	if resizeAfter(cur, "detached") {
		for _, vol := range plan.volumes {
			if err := detachIfNeeded(ctx, client, vol.VolumeID, plan.oldID); err != nil {
				return err
			}
		}
		if err := advance("detached"); err != nil {
			return err
		}
	}

	// → old-terminated: terminate the old instance; ensure volumes are free.
	if resizeAfter(cur, "old-terminated") {
		if exists, _ := instanceExists(ctx, client, plan.oldID); exists {
			fmt.Printf("Terminating old instance %s...\n", plan.oldID)
			if _, err := client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{plan.oldID}}); err != nil {
				return fmt.Errorf("terminating old instance: %w", err)
			}
			termWaiter := ec2.NewInstanceTerminatedWaiter(client)
			if err := termWaiter.Wait(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{plan.oldID}}, 5*time.Minute); err != nil {
				return fmt.Errorf("waiting for old instance to terminate: %w", err)
			}
		}
		for _, vol := range plan.volumes {
			if err := awsutil.PollVolumeState(ctx, client, vol.VolumeID, "available", VolumePollInterval, 2*time.Minute); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: volume %s not yet available: %v\n", vol.VolumeID, err)
			}
		}
		if err := advance("old-terminated"); err != nil {
			return err
		}
	}

	// → attached: attach data volumes to the new (stopped) instance so NixOS
	//   boots with /home present.
	if resizeAfter(cur, "attached") {
		// Fail hard here (don't warn-and-advance): if a volume isn't attached
		// the new box would boot with no /home and the data volume would be
		// orphaned. Returning leaves the resize tags in place so the user can
		// retry with `devbox resize --resume`.
		for _, vol := range plan.volumes {
			if err := attachIfNeeded(ctx, client, vol.VolumeID, plan.newID, vol.Device); err != nil {
				return fmt.Errorf("attaching volume %s to %s (retry with `devbox resize --resume`): %w", vol.VolumeID, plan.newID, err)
			}
		}
		for _, vol := range plan.volumes {
			if err := awsutil.PollVolumeState(ctx, client, vol.VolumeID, "in-use", VolumePollInterval, 2*time.Minute); err != nil {
				return fmt.Errorf("waiting for volume %s to attach to %s (retry with `devbox resize --resume`): %w", vol.VolumeID, plan.newID, err)
			}
		}
		if err := advance("attached"); err != nil {
			return err
		}
	}

	// → started: start the new instance.
	if resizeAfter(cur, "started") {
		st, _ := instanceState(ctx, client, plan.newID)
		if st != types.InstanceStateNameRunning {
			fmt.Printf("Starting instance %s...\n", plan.newID)
			if _, err := spotRetryStart(ctx, client, []string{plan.newID}); err != nil {
				return fmt.Errorf("starting new instance: %w", err)
			}
			if err := waitRunning(ctx, client, plan.newID); err != nil {
				return fmt.Errorf("waiting for new instance to start: %w", err)
			}
		}
		if err := advance("started"); err != nil {
			return err
		}
	}

	// → done: push the full config (the box booted a stub), update DNS, clear tags.
	if resizeAfter(cur, "done") {
		if err := pushNixConfig(ctx, dcfg, client, plan.newID, homeVolumeID(plan.volumes)); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to push NixOS config: %v\n", err)
			fmt.Fprintln(os.Stderr, "Run 'devbox nix-update' manually once the instance is ready.")
		}
		if err := updateDNS(ctx, dcfg, client, r53client, plan.newID, dcfg.DNSName); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: DNS update failed: %v\n", err)
			fmt.Fprintln(os.Stderr, "The NixOS boot service should update DNS automatically.")
		}
		clearResizeTags(ctx, client, plan.newID)
	}

	fmt.Printf("\nDone. Old instance %s terminated, new instance %s (%s) is running.\n", plan.oldID, plan.newID, plan.newType)
	return nil
}

// resumeResize finds an interrupted resize (an instance still carrying the
// resize-state tag) and continues it.
func resumeResize(ctx context.Context, dcfg config.DevboxConfig, client *ec2.Client, r53client *route53.Client) error {
	desc, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{Name: aws.String("tag-key"), Values: []string{resizeStateTag}},
			{Name: aws.String("instance-state-name"), Values: []string{"running", "stopped", "stopping", "pending"}},
		},
	})
	if err != nil {
		return fmt.Errorf("looking for a resize in progress: %w", err)
	}
	var found []types.Instance
	for _, r := range desc.Reservations {
		found = append(found, r.Instances...)
	}
	if len(found) == 0 {
		return fmt.Errorf("no resize in progress to resume")
	}
	if len(found) > 1 {
		var ids []string
		for _, i := range found {
			ids = append(ids, *i.InstanceId)
		}
		return fmt.Errorf("multiple resizes in progress (%s) — clean up manually", strings.Join(ids, ", "))
	}

	inst := found[0]
	tags := tagMap(inst.Tags)
	state := tags[resizeStateTag]
	if state == "" || state == "done" {
		return fmt.Errorf("no resize in progress to resume")
	}

	plan := resizePlan{
		newID:     *inst.InstanceId,
		oldID:     tags[resizeSourceTag],
		newType:   tags[resizeTypeTag],
		spotReqID: tags[resizeSpotTag],
		volumes:   parseVolumes(tags[resizeVolsTag]),
	}
	if inst.Placement != nil && inst.Placement.AvailabilityZone != nil {
		plan.az = *inst.Placement.AvailabilityZone
	}
	if plan.newType == "" {
		plan.newType = string(inst.InstanceType)
	}

	// If the resume still has to detach from / terminate the source instance,
	// the source tag must be a real instance id — otherwise detachIfNeeded("")
	// and instanceExists("") hit malformed-id errors and we could leave the old
	// box running or abort mid-swap. (Past old-terminated the source is gone
	// and no longer referenced, so this only guards the destructive states.)
	if resizeAfter(state, "old-terminated") && !strings.HasPrefix(plan.oldID, "i-") {
		return fmt.Errorf("resize state %q still needs the source instance, but tag %s is missing/invalid (%q); refusing to proceed — clean up the devbox-resize-* tags manually",
			state, resizeSourceTag, plan.oldID)
	}

	fmt.Printf("Resuming resize of %s (new instance %s) from state %q...\n", plan.oldID, plan.newID, state)
	return driveResize(ctx, dcfg, client, r53client, plan, state)
}

// --- resize helpers ---

func resizeStateIndex(s string) int {
	for i, st := range resizeStates {
		if st == s {
			return i
		}
	}
	return -1
}

// resizeAfter reports whether target comes after cur in the state order, i.e.
// whether the step that produces target still needs to run.
func resizeAfter(cur, target string) bool {
	return resizeStateIndex(target) > resizeStateIndex(cur)
}

func setResizeState(ctx context.Context, client *ec2.Client, id, state string) error {
	_, err := client.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{id},
		Tags:      []types.Tag{{Key: aws.String(resizeStateTag), Value: aws.String(state)}},
	})
	if err != nil {
		return fmt.Errorf("updating resize state tag: %w", err)
	}
	return nil
}

func clearResizeTags(ctx context.Context, client *ec2.Client, id string) {
	_, _ = client.DeleteTags(ctx, &ec2.DeleteTagsInput{
		Resources: []string{id},
		Tags: []types.Tag{
			{Key: aws.String(resizeStateTag)},
			{Key: aws.String(resizeSourceTag)},
			{Key: aws.String(resizeVolsTag)},
			{Key: aws.String(resizeSpotTag)},
			{Key: aws.String(resizeTypeTag)},
		},
	})
}

func formatVolumes(vols []volumeAttachment) string {
	parts := make([]string, 0, len(vols))
	for _, v := range vols {
		parts = append(parts, v.VolumeID+":"+v.Device)
	}
	return strings.Join(parts, ",")
}

// homeVolumeID returns the EBS volume id the box's auto-format service is
// allowed to initialize as /home — the first carried data volume, or "" when
// none. (The devbox setup uses a single data volume; if that ever changes,
// this picks the first.)
func homeVolumeID(vols []volumeAttachment) string {
	if len(vols) == 0 {
		return ""
	}
	return vols[0].VolumeID
}

func parseVolumes(s string) []volumeAttachment {
	var vols []volumeAttachment
	if s == "" {
		return vols
	}
	for _, p := range strings.Split(s, ",") {
		kv := strings.SplitN(p, ":", 2)
		if len(kv) != 2 {
			continue
		}
		vols = append(vols, volumeAttachment{VolumeID: kv[0], Device: kv[1]})
	}
	return vols
}

func tagMap(tags []types.Tag) map[string]string {
	m := map[string]string{}
	for _, t := range tags {
		if t.Key != nil && t.Value != nil {
			m[*t.Key] = *t.Value
		}
	}
	return m
}

// volumeAttachedTo returns a volume's state and the instance it's attached to
// (empty if detached).
func volumeAttachedTo(ctx context.Context, client *ec2.Client, volID string) (state, instanceID string, err error) {
	desc, err := client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{volID}})
	if err != nil {
		return "", "", err
	}
	if len(desc.Volumes) == 0 {
		return "", "", fmt.Errorf("volume %s not found", volID)
	}
	v := desc.Volumes[0]
	if len(v.Attachments) > 0 && v.Attachments[0].InstanceId != nil {
		instanceID = *v.Attachments[0].InstanceId
	}
	return string(v.State), instanceID, nil
}

func detachIfNeeded(ctx context.Context, client *ec2.Client, volID, oldID string) error {
	state, inst, err := volumeAttachedTo(ctx, client, volID)
	if err != nil {
		return err
	}
	if state == "available" || inst == "" || inst != oldID {
		// Already detached, or already moved elsewhere — nothing to do.
		return nil
	}
	fmt.Printf("Detaching volume %s from old instance %s...\n", volID, oldID)
	if _, err := client.DetachVolume(ctx, &ec2.DetachVolumeInput{
		VolumeId:   aws.String(volID),
		InstanceId: aws.String(oldID),
	}); err != nil {
		return fmt.Errorf("detaching volume %s: %w", volID, err)
	}
	return nil
}

func attachIfNeeded(ctx context.Context, client *ec2.Client, volID, newID, device string) error {
	state, inst, err := volumeAttachedTo(ctx, client, volID)
	if err != nil {
		return err
	}
	if inst == newID {
		return nil // already attached to the new instance
	}
	if state != "available" {
		return fmt.Errorf("volume %s is %s (attached to %s), cannot attach to %s", volID, state, inst, newID)
	}
	fmt.Printf("Attaching volume %s as %s to new instance %s...\n", volID, device, newID)
	if _, err := client.AttachVolume(ctx, &ec2.AttachVolumeInput{
		VolumeId:   aws.String(volID),
		InstanceId: aws.String(newID),
		Device:     aws.String(device),
	}); err != nil {
		return err
	}
	return nil
}

func instanceExists(ctx context.Context, client *ec2.Client, id string) (bool, error) {
	desc, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		return false, err
	}
	if len(desc.Reservations) == 0 || len(desc.Reservations[0].Instances) == 0 {
		return false, nil
	}
	inst := desc.Reservations[0].Instances[0]
	if inst.State != nil && inst.State.Name == types.InstanceStateNameTerminated {
		return false, nil
	}
	return true, nil
}
