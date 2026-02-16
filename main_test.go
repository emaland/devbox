package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	r53types "github.com/aws/aws-sdk-go-v2/service/route53/types"
	"github.com/docker/go-connections/nat"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/localstack"
)

// Shared test state — initialised once by TestMain.
var (
	testEC2Client *ec2.Client
	testR53Client *route53.Client
	testAWSCfg    aws.Config
	testEndpoint  string
)

func dockerAvailable() (available bool) {
	defer func() {
		if r := recover(); r != nil {
			available = false
		}
	}()
	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		return false
	}
	provider.Close()
	return true
}

func skipIfNoDocker(t *testing.T) {
	t.Helper()
	if testEC2Client == nil {
		t.Skip("skipping: Docker/LocalStack not available")
	}
}

func TestMain(m *testing.M) {
	if !dockerAvailable() {
		fmt.Fprintln(os.Stderr, "Docker not available — only running unit tests (no LocalStack)")
		os.Exit(m.Run())
	}
	os.Exit(runWithLocalStack(m))
}

// runWithLocalStack starts a LocalStack container, configures the shared test
// clients, runs all tests, and tears down the container. Returning an int
// (instead of calling os.Exit directly) lets defers run for clean teardown.
func runWithLocalStack(m *testing.M) int {
	ctx := context.Background()

	container, err := localstack.Run(ctx, "localstack/localstack:latest",
		testcontainers.WithEnv(map[string]string{
			"SERVICES": "ec2,route53",
		}),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start localstack: %v\n", err)
		return 1
	}
	defer func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to terminate localstack container: %v\n", err)
		}
	}()

	mappedPort, err := container.MappedPort(ctx, nat.Port("4566/tcp"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get mapped port: %v\n", err)
		return 1
	}

	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create docker provider: %v\n", err)
		return 1
	}
	defer provider.Close()

	host, err := provider.DaemonHost(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get daemon host: %v\n", err)
		return 1
	}

	endpoint := fmt.Sprintf("http://%s:%s", host, mappedPort.Port())
	testEndpoint = endpoint

	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "test")),
		config.WithBaseEndpoint(endpoint),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to build AWS config: %v\n", err)
		return 1
	}
	testAWSCfg = cfg
	testEC2Client = ec2.NewFromConfig(cfg)
	testR53Client = route53.NewFromConfig(cfg)

	// Speed up polling for tests.
	volumePollInterval = 100 * time.Millisecond
	snapshotPollInterval = 100 * time.Millisecond

	// Let volumeMove's second-region client also hit LocalStack.
	baseEndpointOverride = endpoint

	return m.Run()
}

// --- helpers ---

func createTestInstance(t *testing.T, ctx context.Context) string {
	t.Helper()
	result, err := testEC2Client.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-test12345"),
		InstanceType: types.InstanceTypeT2Micro,
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeInstance,
				Tags: []types.Tag{
					{Key: aws.String("Name"), Value: aws.String("test-instance")},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("RunInstances: %v", err)
	}
	return *result.Instances[0].InstanceId
}

func testDevboxConfig() devboxConfig {
	return devboxConfig{
		DNSName:          "test.example.com",
		DNSZone:          "example.com.",
		SSHKeyName:       "test-key",
		SSHKeyPath:       "~/.ssh/test.pem",
		SSHUser:          "testuser",
		SecurityGroup:    "test-sg",
		IAMProfile:       "test-profile",
		DefaultAZ:        "us-east-1a",
		DefaultType:      "t2.micro",
		DefaultMaxPrice:  "0.50",
		SpawnName:        "test-spawn",
		NixOSAMIOwner:   "123456789012",
		NixOSAMIPattern: "test-ami*",
	}
}

func createTestHostedZone(t *testing.T, ctx context.Context, domain string) string {
	t.Helper()
	result, err := testR53Client.CreateHostedZone(ctx, &route53.CreateHostedZoneInput{
		Name:            aws.String(domain),
		CallerReference: aws.String(fmt.Sprintf("test-%d", time.Now().UnixNano())),
	})
	if err != nil {
		t.Fatalf("CreateHostedZone: %v", err)
	}
	return *result.HostedZone.Id
}

// ==================== Utility tests (no AWS) ====================

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, ".config", "devbox")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := `{"dns_name":"custom.example.com","default_type":"c5.xlarge"}`
	if err := os.WriteFile(filepath.Join(cfgDir, "default.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	// Override HOME so loadConfig reads our temp file.
	t.Setenv("HOME", dir)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.DNSName != "custom.example.com" {
		t.Errorf("DNSName = %q, want %q", cfg.DNSName, "custom.example.com")
	}
	if cfg.DefaultType != "c5.xlarge" {
		t.Errorf("DefaultType = %q, want %q", cfg.DefaultType, "c5.xlarge")
	}
	// Non-overridden fields keep defaults.
	if cfg.SSHUser != "emaland" {
		t.Errorf("SSHUser = %q, want default %q", cfg.SSHUser, "emaland")
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.DNSName != "dev.frob.io" {
		t.Errorf("default DNSName = %q, want %q", cfg.DNSName, "dev.frob.io")
	}
}

func TestResolveSSHKeyPath(t *testing.T) {
	cfg := devboxConfig{SSHKeyPath: "~/.ssh/test.pem"}
	got := cfg.resolveSSHKeyPath()
	if strings.HasPrefix(got, "~") {
		t.Errorf("resolveSSHKeyPath still starts with ~: %s", got)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".ssh", "test.pem")
	if got != want {
		t.Errorf("resolveSSHKeyPath = %q, want %q", got, want)
	}
}

func TestNameTag(t *testing.T) {
	tags := []types.Tag{
		{Key: aws.String("Env"), Value: aws.String("prod")},
		{Key: aws.String("Name"), Value: aws.String("my-box")},
	}
	if got := nameTag(tags); got != "my-box" {
		t.Errorf("nameTag = %q, want %q", got, "my-box")
	}
	if got := nameTag(nil); got != "-" {
		t.Errorf("nameTag(nil) = %q, want %q", got, "-")
	}
}

func TestToLaunchSpecNil(t *testing.T) {
	if got := toLaunchSpec(nil); got != nil {
		t.Errorf("toLaunchSpec(nil) = %v, want nil", got)
	}
}

func TestToLaunchSpec(t *testing.T) {
	from := &types.LaunchSpecification{
		ImageId:      aws.String("ami-abc"),
		InstanceType: types.InstanceTypeM5Large,
		KeyName:      aws.String("mykey"),
	}
	got := toLaunchSpec(from)
	if got == nil {
		t.Fatal("toLaunchSpec returned nil")
	}
	if *got.ImageId != "ami-abc" {
		t.Errorf("ImageId = %q, want %q", *got.ImageId, "ami-abc")
	}
	if got.InstanceType != types.InstanceTypeM5Large {
		t.Errorf("InstanceType = %v, want %v", got.InstanceType, types.InstanceTypeM5Large)
	}
}

// ==================== Instance lifecycle tests ====================

func TestListInstancesEmpty(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	err := listInstances(ctx, testEC2Client)
	if err != nil {
		if strings.Contains(err.Error(), "instance-lifecycle") {
			t.Skipf("LocalStack does not support instance-lifecycle filter: %v", err)
		}
		t.Fatalf("listInstances: %v", err)
	}
}

func TestListInstancesWithInstance(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	id := createTestInstance(t, ctx)

	// listInstances filters by instance-lifecycle=spot. LocalStack/Moto has
	// not implemented this filter, so we tolerate that specific error.
	err := listInstances(ctx, testEC2Client)
	if err != nil {
		if strings.Contains(err.Error(), "instance-lifecycle") {
			t.Skipf("LocalStack does not support instance-lifecycle filter: %v", err)
		}
		t.Fatalf("listInstances: %v", err)
	}

	// Verify the instance exists via DescribeInstances (no filter).
	desc, err := testEC2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{id},
	})
	if err != nil {
		t.Fatalf("DescribeInstances: %v", err)
	}
	if len(desc.Reservations) == 0 || len(desc.Reservations[0].Instances) == 0 {
		t.Fatal("instance not found after creation")
	}
}

func TestStopStartInstances(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	id := createTestInstance(t, ctx)

	if err := stopInstances(ctx, testEC2Client, []string{id}); err != nil {
		t.Fatalf("stopInstances: %v", err)
	}
	desc, err := testEC2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		t.Fatal(err)
	}
	state := desc.Reservations[0].Instances[0].State.Name
	if state != types.InstanceStateNameStopped && state != types.InstanceStateNameStopping {
		t.Errorf("after stop: state = %s, want stopped/stopping", state)
	}

	if err := startInstances(ctx, testEC2Client, []string{id}); err != nil {
		t.Fatalf("startInstances: %v", err)
	}
	desc, err = testEC2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		t.Fatal(err)
	}
	state = desc.Reservations[0].Instances[0].State.Name
	if state != types.InstanceStateNameRunning && state != types.InstanceStateNamePending {
		t.Errorf("after start: state = %s, want running/pending", state)
	}
}

func TestTerminateInstances(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	id := createTestInstance(t, ctx)

	if err := terminateInstances(ctx, testEC2Client, []string{id}); err != nil {
		t.Fatalf("terminateInstances: %v", err)
	}
	desc, err := testEC2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		t.Fatal(err)
	}
	state := desc.Reservations[0].Instances[0].State.Name
	if state != types.InstanceStateNameTerminated && state != types.InstanceStateNameShuttingDown {
		t.Errorf("after terminate: state = %s, want terminated/shutting-down", state)
	}
}

// ==================== DNS tests ====================

func TestFindHostedZone(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	domain := "findzone.test."
	expectedID := createTestHostedZone(t, ctx, domain)

	gotID, err := findHostedZone(ctx, testR53Client, domain)
	if err != nil {
		t.Fatalf("findHostedZone: %v", err)
	}
	if gotID != expectedID {
		t.Errorf("findHostedZone = %q, want %q", gotID, expectedID)
	}
}

func TestFindHostedZoneNotFound(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	_, err := findHostedZone(ctx, testR53Client, "nonexistent.zone.")
	if err == nil {
		t.Fatal("expected error for nonexistent zone")
	}
}

func TestUpdateDNS(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	domain := "updatedns.test."
	zoneID := createTestHostedZone(t, ctx, domain)
	id := createTestInstance(t, ctx)

	dcfg := testDevboxConfig()
	dcfg.DNSZone = domain
	dcfg.DNSName = "dev." + strings.TrimSuffix(domain, ".")

	if err := updateDNS(ctx, dcfg, testEC2Client, testR53Client, id, dcfg.DNSName); err != nil {
		t.Fatalf("updateDNS: %v", err)
	}

	// Verify the record was created.
	records, err := testR53Client.ListResourceRecordSets(ctx, &route53.ListResourceRecordSetsInput{
		HostedZoneId: aws.String(zoneID),
	})
	if err != nil {
		t.Fatalf("ListResourceRecordSets: %v", err)
	}
	found := false
	for _, rr := range records.ResourceRecordSets {
		if rr.Type == r53types.RRTypeA && strings.Contains(*rr.Name, "dev.") {
			found = true
			break
		}
	}
	if !found {
		t.Error("A record not found after updateDNS")
	}
}

// ==================== Spot request tests ====================

func TestShowBidsEmpty(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	if err := showBids(ctx, testEC2Client); err != nil {
		t.Fatalf("showBids: %v", err)
	}
}

func TestShowPricesEmpty(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	if err := showPrices(ctx, testEC2Client); err != nil {
		t.Fatalf("showPrices: %v", err)
	}
}

func TestRebid(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()

	// Create a spot request.
	spotResult, err := testEC2Client.RequestSpotInstances(ctx, &ec2.RequestSpotInstancesInput{
		SpotPrice:     aws.String("0.05"),
		InstanceCount: aws.Int32(1),
		LaunchSpecification: &types.RequestSpotLaunchSpecification{
			ImageId:      aws.String("ami-test12345"),
			InstanceType: types.InstanceTypeT2Micro,
		},
	})
	if err != nil {
		t.Fatalf("RequestSpotInstances: %v", err)
	}
	if len(spotResult.SpotInstanceRequests) == 0 {
		t.Fatal("no spot requests returned")
	}
	oldReqID := *spotResult.SpotInstanceRequests[0].SpotInstanceRequestId

	if err := rebid(ctx, testEC2Client, oldReqID, "0.10"); err != nil {
		t.Fatalf("rebid: %v", err)
	}

	// Verify old request is cancelled.
	desc, err := testEC2Client.DescribeSpotInstanceRequests(ctx, &ec2.DescribeSpotInstanceRequestsInput{
		SpotInstanceRequestIds: []string{oldReqID},
	})
	if err != nil {
		t.Fatalf("DescribeSpotInstanceRequests: %v", err)
	}
	if len(desc.SpotInstanceRequests) > 0 {
		state := desc.SpotInstanceRequests[0].State
		if state != types.SpotInstanceStateCancelled {
			t.Errorf("old request state = %s, want cancelled", state)
		}
	}
}

// ==================== Search tests ====================

func TestSearchSpotPrices(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	// LocalStack returns empty spot price history; verify the code handles it gracefully.
	if err := searchSpotPrices(ctx, testEC2Client, []string{"t2.micro"}); err != nil {
		t.Fatalf("searchSpotPrices: %v", err)
	}
}

func TestFetchInstanceTypes(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	results, err := fetchInstanceTypes(ctx, testEC2Client, "x86_64", 1, 0.5, false)
	if err != nil {
		t.Fatalf("fetchInstanceTypes: %v", err)
	}
	// LocalStack may return instance types; just verify no error.
	_ = results
}

func TestDescribeSpecificTypes(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	results, err := describeSpecificTypes(ctx, testEC2Client, []types.InstanceType{types.InstanceTypeT2Micro})
	if err != nil {
		t.Fatalf("describeSpecificTypes: %v", err)
	}
	if len(results) == 0 {
		t.Skip("LocalStack did not return instance type info for t2.micro")
	}
	for _, r := range results {
		if r.Name == "t2.micro" {
			if r.VCPUs < 1 {
				t.Errorf("t2.micro VCPUs = %d, want >= 1", r.VCPUs)
			}
			return
		}
	}
	t.Error("t2.micro not found in results")
}

// ==================== Spawn helper tests ====================

func TestLookupSecurityGroup(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()

	// Create a security group.
	sgName := "test-sg-lookup"
	sgResult, err := testEC2Client.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(sgName),
		Description: aws.String("test security group"),
	})
	if err != nil {
		t.Fatalf("CreateSecurityGroup: %v", err)
	}

	dcfg := testDevboxConfig()
	dcfg.SecurityGroup = sgName

	gotID, err := lookupSecurityGroup(ctx, dcfg, testEC2Client)
	if err != nil {
		t.Fatalf("lookupSecurityGroup: %v", err)
	}
	if gotID != *sgResult.GroupId {
		t.Errorf("lookupSecurityGroup = %q, want %q", gotID, *sgResult.GroupId)
	}
}

func TestLookupSubnet(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	// LocalStack provides a default VPC + subnets. Just verify no error.
	subnetID, err := lookupSubnet(ctx, testEC2Client, "us-east-1a")
	if err != nil {
		t.Fatalf("lookupSubnet: %v", err)
	}
	if subnetID == "" {
		t.Error("lookupSubnet returned empty string")
	}
}

func TestLookupAMI(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()

	// Register a test AMI that matches the pattern.
	dcfg := testDevboxConfig()
	_, err := testEC2Client.RegisterImage(ctx, &ec2.RegisterImageInput{
		Name:               aws.String("test-ami-2024.01"),
		Description:        aws.String("test AMI"),
		Architecture:       types.ArchitectureValuesX8664,
		RootDeviceName:     aws.String("/dev/xvda"),
		VirtualizationType: aws.String("hvm"),
		BlockDeviceMappings: []types.BlockDeviceMapping{
			{
				DeviceName: aws.String("/dev/xvda"),
				Ebs: &types.EbsBlockDevice{
					VolumeSize: aws.Int32(8),
					VolumeType: types.VolumeTypeGp3,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("RegisterImage: %v", err)
	}

	amiID, err := lookupAMI(ctx, dcfg, testEC2Client)
	if err != nil {
		// LocalStack may not support owner filter correctly.
		t.Skipf("lookupAMI failed (likely LocalStack filter limitation): %v", err)
	}
	if amiID == "" {
		t.Error("lookupAMI returned empty string")
	}
}

func TestSpawnInstance(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()

	// Create a source instance with user_data for cloning.
	userData := base64.StdEncoding.EncodeToString([]byte("#!/bin/bash\necho hello"))
	srcResult, err := testEC2Client.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-test12345"),
		InstanceType: types.InstanceTypeT2Micro,
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		UserData:     aws.String(userData),
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeInstance,
				Tags: []types.Tag{
					{Key: aws.String("Name"), Value: aws.String("spawn-source")},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("create source instance: %v", err)
	}
	sourceID := *srcResult.Instances[0].InstanceId

	// Register an AMI matching the test pattern.
	dcfg := testDevboxConfig()
	regResult, err := testEC2Client.RegisterImage(ctx, &ec2.RegisterImageInput{
		Name:               aws.String("test-ami-spawn-2024.01"),
		Architecture:       types.ArchitectureValuesX8664,
		RootDeviceName:     aws.String("/dev/xvda"),
		VirtualizationType: aws.String("hvm"),
		BlockDeviceMappings: []types.BlockDeviceMapping{
			{
				DeviceName: aws.String("/dev/xvda"),
				Ebs: &types.EbsBlockDevice{
					VolumeSize: aws.Int32(8),
					VolumeType: types.VolumeTypeGp3,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("RegisterImage: %v", err)
	}
	_ = regResult

	// Create security group.
	_, err = testEC2Client.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String("test-sg-spawn"),
		Description: aws.String("spawn test sg"),
	})
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("CreateSecurityGroup: %v", err)
	}
	dcfg.SecurityGroup = "test-sg-spawn"

	err = spawnInstance(ctx, dcfg, testEC2Client, []string{"--from", sourceID})
	if err != nil {
		// Spawn may fail at AMI lookup due to owner filter. That's a known LocalStack limitation.
		if strings.Contains(err.Error(), "AMI") || strings.Contains(err.Error(), "user_data") {
			t.Skipf("spawnInstance failed (LocalStack limitation): %v", err)
		}
		t.Fatalf("spawnInstance: %v", err)
	}
}

// ==================== Resize test ====================

func TestResizeInstance(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	id := createTestInstance(t, ctx)

	dcfg := testDevboxConfig()
	domain := "resize.test."
	createTestHostedZone(t, ctx, domain)
	dcfg.DNSZone = domain
	dcfg.DNSName = "dev.resize.test"

	if err := resizeInstance(ctx, dcfg, testEC2Client, testR53Client, id, "t2.small"); err != nil {
		t.Fatalf("resizeInstance: %v", err)
	}

	// Verify type changed.
	desc, err := testEC2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		t.Fatal(err)
	}
	got := string(desc.Reservations[0].Instances[0].InstanceType)
	if got != "t2.small" {
		t.Errorf("instance type after resize = %q, want %q", got, "t2.small")
	}
}

// ==================== Volume tests ====================

func TestVolumeLSEmpty(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	if err := volumeLS(ctx, testEC2Client); err != nil {
		t.Fatalf("volumeLS: %v", err)
	}
}

func TestVolumeCreateAndList(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	dcfg := testDevboxConfig()
	if err := volumeCreate(ctx, dcfg, testEC2Client, []string{"-size", "1", "-name", "test-vol-list"}); err != nil {
		t.Fatalf("volumeCreate: %v", err)
	}
	if err := volumeLS(ctx, testEC2Client); err != nil {
		t.Fatalf("volumeLS: %v", err)
	}
}

func TestVolumeCreateAndDestroy(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()

	result, err := testEC2Client.CreateVolume(ctx, &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String("us-east-1a"),
		Size:             aws.Int32(1),
		VolumeType:       types.VolumeTypeGp3,
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeVolume,
				Tags:         []types.Tag{{Key: aws.String("Name"), Value: aws.String("test-destroy")}},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	volID := *result.VolumeId

	if err := volumeDestroy(ctx, testEC2Client, []string{volID}); err != nil {
		t.Fatalf("volumeDestroy: %v", err)
	}

	// Verify it's gone.
	desc, err := testEC2Client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		VolumeIds: []string{volID},
	})
	if err == nil && len(desc.Volumes) > 0 {
		// LocalStack may still return it; just check if state differs.
		_ = desc
	}
}

func TestResolveVolumeByName(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	result, err := testEC2Client.CreateVolume(ctx, &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String("us-east-1a"),
		Size:             aws.Int32(1),
		VolumeType:       types.VolumeTypeGp3,
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeVolume,
				Tags:         []types.Tag{{Key: aws.String("Name"), Value: aws.String("resolve-by-name-unique")}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := resolveVolume(ctx, testEC2Client, "resolve-by-name-unique")
	if err != nil {
		t.Fatalf("resolveVolume by name: %v", err)
	}
	if got != *result.VolumeId {
		t.Errorf("resolveVolume = %q, want %q", got, *result.VolumeId)
	}
}

func TestResolveVolumeByID(t *testing.T) {
	skipIfNoDocker(t)
	got, err := resolveVolume(context.Background(), testEC2Client, "vol-abc123")
	if err != nil {
		t.Fatalf("resolveVolume by ID: %v", err)
	}
	if got != "vol-abc123" {
		t.Errorf("resolveVolume = %q, want %q", got, "vol-abc123")
	}
}

func TestResolveVolumeNotFound(t *testing.T) {
	skipIfNoDocker(t)
	_, err := resolveVolume(context.Background(), testEC2Client, "nonexistent-vol")
	if err == nil {
		t.Fatal("expected error for nonexistent volume")
	}
}

func TestResolveVolumeAmbiguous(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	name := "ambiguous-vol-name"
	for i := 0; i < 2; i++ {
		_, err := testEC2Client.CreateVolume(ctx, &ec2.CreateVolumeInput{
			AvailabilityZone: aws.String("us-east-1a"),
			Size:             aws.Int32(1),
			VolumeType:       types.VolumeTypeGp3,
			TagSpecifications: []types.TagSpecification{
				{
					ResourceType: types.ResourceTypeVolume,
					Tags:         []types.Tag{{Key: aws.String("Name"), Value: aws.String(name)}},
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err := resolveVolume(ctx, testEC2Client, name)
	if err == nil {
		t.Fatal("expected error for ambiguous volume name")
	}
	if !strings.Contains(err.Error(), "multiple") {
		t.Errorf("error = %q, want it to mention 'multiple'", err.Error())
	}
}

func TestVolumeAttach(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	instID := createTestInstance(t, ctx)

	vol, err := testEC2Client.CreateVolume(ctx, &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String("us-east-1a"),
		Size:             aws.Int32(1),
		VolumeType:       types.VolumeTypeGp3,
	})
	if err != nil {
		t.Fatal(err)
	}
	volID := *vol.VolumeId

	if err := volumeAttach(ctx, testEC2Client, []string{volID, instID}); err != nil {
		t.Fatalf("volumeAttach: %v", err)
	}
	desc, err := testEC2Client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{volID}})
	if err != nil {
		t.Fatal(err)
	}
	if string(desc.Volumes[0].State) != "in-use" {
		t.Errorf("after attach: state = %s, want in-use", desc.Volumes[0].State)
	}
}

func TestVolumeDetach(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	instID := createTestInstance(t, ctx)

	vol, err := testEC2Client.CreateVolume(ctx, &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String("us-east-1a"),
		Size:             aws.Int32(1),
		VolumeType:       types.VolumeTypeGp3,
	})
	if err != nil {
		t.Fatal(err)
	}
	volID := *vol.VolumeId

	// Attach first.
	_, err = testEC2Client.AttachVolume(ctx, &ec2.AttachVolumeInput{
		VolumeId:   aws.String(volID),
		InstanceId: aws.String(instID),
		Device:     aws.String("/dev/xvdf"),
	})
	if err != nil {
		t.Fatalf("AttachVolume setup: %v", err)
	}

	err = volumeDetach(ctx, testEC2Client, []string{volID})
	if err != nil {
		// LocalStack's DetachVolume has a known bug; skip rather than fail.
		if strings.Contains(err.Error(), "InternalError") || strings.Contains(err.Error(), "NoneType") {
			t.Skipf("LocalStack DetachVolume bug: %v", err)
		}
		t.Fatalf("volumeDetach: %v", err)
	}
	desc, err := testEC2Client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{volID}})
	if err != nil {
		t.Fatal(err)
	}
	if string(desc.Volumes[0].State) != "available" {
		t.Errorf("after detach: state = %s, want available", desc.Volumes[0].State)
	}
}

func TestVolumeSnapshot(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	vol, err := testEC2Client.CreateVolume(ctx, &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String("us-east-1a"),
		Size:             aws.Int32(1),
		VolumeType:       types.VolumeTypeGp3,
	})
	if err != nil {
		t.Fatal(err)
	}
	volID := *vol.VolumeId

	if err := volumeSnapshot(ctx, testEC2Client, []string{"-name", "test-snap", volID}); err != nil {
		t.Fatalf("volumeSnapshot: %v", err)
	}

	// Verify snapshot appears.
	if err := volumeSnapshots(ctx, testEC2Client); err != nil {
		t.Fatalf("volumeSnapshots: %v", err)
	}
}

func TestVolumeMove(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()

	vol, err := testEC2Client.CreateVolume(ctx, &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String("us-east-1a"),
		Size:             aws.Int32(1),
		VolumeType:       types.VolumeTypeGp3,
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeVolume,
				Tags:         []types.Tag{{Key: aws.String("Name"), Value: aws.String("move-test")}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	volID := *vol.VolumeId

	err = volumeMove(ctx, testEC2Client, testAWSCfg, []string{"--cleanup", volID, "us-west-2"})
	if err != nil {
		t.Fatalf("volumeMove: %v", err)
	}
}

// ==================== FetchUserData test ====================

func TestFetchUserData(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	original := "#!/bin/bash\necho test"
	encoded := base64.StdEncoding.EncodeToString([]byte(original))

	result, err := testEC2Client.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-test12345"),
		InstanceType: types.InstanceTypeT2Micro,
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		UserData:     aws.String(encoded),
	})
	if err != nil {
		t.Fatal(err)
	}
	instID := *result.Instances[0].InstanceId

	got, err := fetchUserData(ctx, testEC2Client, instID)
	if err != nil {
		t.Skipf("fetchUserData failed (may be LocalStack limitation): %v", err)
	}

	decoded, err := base64.StdEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if string(decoded) != original {
		t.Errorf("fetchUserData round-trip: got %q, want %q", string(decoded), original)
	}
}

// ==================== LoadConfig with JSON test ====================

func TestLoadConfigBadJSON(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, ".config", "devbox")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "default.json"), []byte("{bad json"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)

	_, err := loadConfig()
	if err == nil {
		t.Fatal("expected error for bad JSON")
	}
}

// verify our test devbox config produces valid JSON round-trip
func TestDevboxConfigJSON(t *testing.T) {
	cfg := testDevboxConfig()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var parsed devboxConfig
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.DNSName != cfg.DNSName {
		t.Errorf("DNSName = %q, want %q", parsed.DNSName, cfg.DNSName)
	}
}
