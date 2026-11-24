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

	"github.com/instantcocoa/devbox/internal/awsutil"
	"github.com/instantcocoa/devbox/internal/config"
)

func newSpawnCmd() *cobra.Command {
	var (
		instanceType string
		az           string
		name         string
		maxPrice     string
		from         string
		volumeID     string
		noDNS        bool
	)

	cmd := &cobra.Command{
		Use:   "spawn",
		Short: "Spin up a new spot instance cloned from the primary",
		Long: `Spin up a new spot instance cloned from the primary: it boots the
primary's user_data, then gets the NixOS config pushed, DNS record included.

With --no-dns the clone does not point the primary's DNS name at itself. It
boots the minimal stub instead of the primary's user_data, its pushed config
names no DNS record or zone, so its boot-time Route 53 update is skipped, and
it is tagged devbox-dns=off, so dns, up, resize, nix-update and setup-dns
write no DNS for it later. Use it for a short-lived second box next to the
primary.

Limits: the clone has the primary's instance profile, whose role may change
records in the hosted zone, so a program on the clone could still do so
itself (your local.nix is pushed to it unchanged). With --volume and a copy
of the primary's data volume, the primary's saved config is restored on it
until the first push succeeds. Removing the devbox-dns tag makes it an
ordinary box again. It also joins the tailnet with the primary's host name.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return spawnInstance(cmd.Context(), dcfg, ec2Client, instanceType, az, name, maxPrice, from, volumeID, noDNS)
		},
	}

	cmd.Flags().StringVar(&instanceType, "type", "", "Instance type (default from config)")
	cmd.Flags().StringVar(&az, "az", "", "Availability zone (default from config)")
	cmd.Flags().StringVar(&name, "name", "", "Name tag for the instance (default from config)")
	cmd.Flags().StringVar(&maxPrice, "max-price", "", "Spot max price $/hr (default from config)")
	cmd.Flags().StringVar(&from, "from", "", "Instance ID to clone user_data from")
	cmd.Flags().StringVar(&volumeID, "volume", "", "EBS data volume ID to attach as /dev/xvdf")
	cmd.Flags().BoolVar(&noDNS, "no-dns", false, "Never take over the primary's DNS name: boot the stub user_data and push a config without a DNS record")
	// --from clones a source's user_data, which can carry its DNS record.
	cmd.MarkFlagsMutuallyExclusive("no-dns", "from")

	return cmd
}

// withoutDNS is the config a --no-dns clone is pushed with: no record name
// and no zone, so the box's Route 53 update finds its markers unrendered and
// skips, and no hosted zone is looked up.
func withoutDNS(dcfg config.DevboxConfig) config.DevboxConfig {
	dcfg.DNSName = ""
	dcfg.DNSZone = ""
	dcfg.DNSZoneID = ""
	return dcfg
}

// dnsTag marks an instance spawned with --no-dns, so every later command that
// would write DNS for it (dns, up, resize, nix-update, setup-dns) does not.
// resize carries it to the new instance with the other tags.
const dnsTag = "devbox-dns"

// spawnDNS is what --no-dns changes in a launch: the config pushed to the
// clone, and the tags it is launched with.
func spawnDNS(dcfg config.DevboxConfig, noDNS bool) (config.DevboxConfig, []types.Tag) {
	if !noDNS {
		return dcfg, nil
	}
	return withoutDNS(dcfg), []types.Tag{{Key: aws.String(dnsTag), Value: aws.String("off")}}
}

// launchTags are the tags spawn launches an instance with.
func launchTags(name string, dnsTags []types.Tag) []types.Tag {
	return append([]types.Tag{
		{Key: aws.String("Name"), Value: aws.String(name)},
		{Key: aws.String("devbox-managed"), Value: aws.String("true")},
	}, dnsTags...)
}

// dnsOff reports whether an instance's tags mark it as spawned with --no-dns.
func dnsOff(tags []types.Tag) bool {
	for _, t := range tags {
		if aws.ToString(t.Key) == dnsTag && aws.ToString(t.Value) == "off" {
			return true
		}
	}
	return false
}

// dnsConfigFor is the config to push to an instance: without DNS for one
// spawned with --no-dns. An instance that cannot be described is an error,
// not a guess.
func dnsConfigFor(ctx context.Context, client *ec2.Client, instanceID string, dcfg config.DevboxConfig) (config.DevboxConfig, error) {
	desc, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}})
	if err != nil {
		return dcfg, fmt.Errorf("describing instance %s: %w", instanceID, err)
	}
	if len(desc.Reservations) == 0 || len(desc.Reservations[0].Instances) == 0 {
		return dcfg, fmt.Errorf("instance %s not found", instanceID)
	}
	if dnsOff(desc.Reservations[0].Instances[0].Tags) {
		fmt.Printf("  %s was spawned with --no-dns: pushing a config without a DNS record\n", instanceID)
		return withoutDNS(dcfg), nil
	}
	return dcfg, nil
}

// userDataSource is how spawn reads another instance's user_data.
type userDataSource struct {
	fetch  func(ctx context.Context, instanceID string) (string, error)
	detect func(ctx context.Context) (string, error)
}

// chooseUserData picks what the new instance boots: the stub for --no-dns,
// else the user_data of --from, of the one devbox instance found, or the stub
// when there is none.
func chooseUserData(ctx context.Context, src userDataSource, from string, noDNS bool) (string, error) {
	if noDNS {
		fmt.Println("  --no-dns: booting the minimal stub; full config pushed after launch")
		return stubUserData(), nil
	}
	if from != "" {
		fmt.Printf("  Cloning user_data from: %s\n", from)
		return src.fetch(ctx, from)
	}
	sourceID, err := src.detect(ctx)
	if err != nil {
		// No source instance — boot a minimal stub (the full configuration.nix
		// exceeds EC2's 16 KB user_data limit). The real config is pushed via
		// SSH after launch (pushNixConfig).
		fmt.Println("  No source instance — booting minimal stub; full config pushed after launch")
		return stubUserData(), nil
	}
	fmt.Printf("  Cloning user_data from: %s\n", sourceID)
	return src.fetch(ctx, sourceID)
}

func spawnInstance(ctx context.Context, dcfg config.DevboxConfig, client *ec2.Client, instanceType, az, name, maxPrice, from, volumeID string, noDNS bool) error {
	dcfg, dnsTags := spawnDNS(dcfg, noDNS)
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

	// Fail on a local config problem before launching anything: the config
	// push at the end needs the template, the keys file and the zone id.
	dcfg, err := preflightNixConfig(ctx, dcfg, defaultNixFile(), findHostedZoneID)
	if err != nil {
		return err
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

	userData, err := chooseUserData(ctx, userDataSource{
		fetch:  func(ctx context.Context, id string) (string, error) { return awsutil.FetchUserData(ctx, client, id) },
		detect: func(ctx context.Context) (string, error) { return autoDetectSourceInstance(ctx, client) },
	}, from, noDNS)
	if err != nil {
		return err
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
		UserData:                            aws.String(userData),
		InstanceInitiatedShutdownBehavior:   types.ShutdownBehaviorStop,
		BlockDeviceMappings: []types.BlockDeviceMapping{
			{
				DeviceName: aws.String("/dev/xvda"),
				Ebs: &types.EbsBlockDevice{
					VolumeSize:           aws.Int32(75),
					VolumeType:           types.VolumeTypeGp3,
					DeleteOnTermination:  aws.Bool(false),
				},
			},
		},
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeInstance,
				Tags:         launchTags(name, dnsTags),
			},
		},
	}
	if dcfg.UseSpot {
		runInput.InstanceMarketOptions = &types.InstanceMarketOptionsRequest{
			MarketType: types.MarketTypeSpot,
			SpotOptions: &types.SpotMarketOptions{
				SpotInstanceType:             types.SpotInstanceTypePersistent,
				InstanceInterruptionBehavior: types.InstanceInterruptionBehaviorStop,
				MaxPrice:                     aws.String(maxPrice),
			},
		}
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
			{Name: aws.String("tag:devbox-managed"), Values: []string{"true"}},
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
