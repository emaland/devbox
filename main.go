package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	r53types "github.com/aws/aws-sdk-go-v2/service/route53/types"
)

type devboxConfig struct {
	DNSName          string `json:"dns_name"`
	DNSZone          string `json:"dns_zone"`
	SSHKeyName       string `json:"ssh_key_name"`
	SSHKeyPath       string `json:"ssh_key_path"`
	SSHUser          string `json:"ssh_user"`
	SecurityGroup    string `json:"security_group"`
	IAMProfile       string `json:"iam_profile"`
	DefaultAZ        string `json:"default_az"`
	DefaultType      string `json:"default_type"`
	DefaultMaxPrice  string `json:"default_max_price"`
	SpawnName        string `json:"spawn_name"`
	NixOSAMIOwner   string `json:"nixos_ami_owner"`
	NixOSAMIPattern string `json:"nixos_ami_pattern"`
}

func loadConfig() (devboxConfig, error) {
	cfg := devboxConfig{
		DNSName:          "dev.frob.io",
		DNSZone:          "frob.io.",
		SSHKeyName:       "dev-boxes",
		SSHKeyPath:       "~/.ssh/dev-boxes.pem",
		SSHUser:          "emaland",
		SecurityGroup:    "dev-instance",
		IAMProfile:       "dev-workstation-profile",
		DefaultAZ:        "us-east-2a",
		DefaultType:      "m6i.4xlarge",
		DefaultMaxPrice:  "2.00",
		SpawnName:        "dev-workstation-tmp",
		NixOSAMIOwner:   "427812963091",
		NixOSAMIPattern: "nixos/24.11*",
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return cfg, nil
	}

	path := filepath.Join(home, ".config", "devbox", "default.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("reading config %s: %w", path, err)
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing config %s: %w", path, err)
	}
	return cfg, nil
}

func (c devboxConfig) resolveSSHKeyPath() string {
	if strings.HasPrefix(c.SSHKeyPath, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, c.SSHKeyPath[2:])
		}
	}
	return c.SSHKeyPath
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	dcfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading devbox config: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading AWS config: %v\n", err)
		os.Exit(1)
	}
	client := ec2.NewFromConfig(cfg)

	switch os.Args[1] {
	case "list", "ls":
		if err := listInstances(ctx, client); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "stop":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: devbox stop <instance-id> [instance-id...]")
			os.Exit(1)
		}
		if err := stopInstances(ctx, client, os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "start":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: devbox start <instance-id> [instance-id...]")
			os.Exit(1)
		}
		if err := startInstances(ctx, client, os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "terminate":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: devbox terminate <instance-id> [instance-id...]")
			os.Exit(1)
		}
		if err := terminateInstances(ctx, client, os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "dns":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: devbox dns <instance-id> [dns-name]")
			os.Exit(1)
		}
		dnsName := dcfg.DNSName
		if len(os.Args) >= 4 {
			dnsName = os.Args[3]
		}
		r53client := route53.NewFromConfig(cfg)
		if err := updateDNS(ctx, dcfg, client, r53client, os.Args[2], dnsName); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "bids":
		if err := showBids(ctx, client); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "prices":
		if err := showPrices(ctx, client); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "rebid":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "Usage: devbox rebid <spot-request-id> <new-price>")
			fmt.Fprintln(os.Stderr, "  e.g. devbox rebid sir-abc123 0.05")
			os.Exit(1)
		}
		if err := rebid(ctx, client, os.Args[2], os.Args[3]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "ssh":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: devbox ssh <instance-id>")
			os.Exit(1)
		}
		if err := sshToInstance(ctx, dcfg, client, os.Args[2]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "setup-dns":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: devbox setup-dns <instance-id>")
			os.Exit(1)
		}
		if err := setupDNSOnBoot(ctx, dcfg, client, os.Args[2]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "resize":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "Usage: devbox resize <instance-id> <new-type>")
			os.Exit(1)
		}
		r53client := route53.NewFromConfig(cfg)
		if err := resizeInstance(ctx, dcfg, client, r53client, os.Args[2], os.Args[3]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "search":
		if err := searchSpotPrices(ctx, client, os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "spawn":
		if err := spawnInstance(ctx, dcfg, client, os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "volume":
		if err := volumeCommand(ctx, dcfg, client, cfg, os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `Usage: devbox <command> [args]

Commands:
  list, ls                          List spot instances and their state
  start    <instance-id> [...]      Start stopped spot instances
  stop     <instance-id> [...]      Stop running spot instances
  terminate <instance-id> [...]     Terminate spot instances
  dns      <instance-id> [dns-name]  Point a DNS name at the instance's public IP
  bids                              Show current spot request bids (max price)
  prices                            Show current spot market prices for our instance types
  rebid    <spot-req-id> <price>    Cancel and re-create a spot request with a new max price
  ssh      <instance-id>            SSH into an instance
  setup-dns <instance-id>           Install a boot script that updates dev.frob.io on startup
  search   [flags]                  Browse spot prices by hardware specs
  resize   <instance-id> <type>     Stop instance, change type, restart, update DNS
  spawn    [flags]                  Spin up a new spot instance cloned from the primary
  volume   <subcommand>             Manage EBS volumes (ls, create, attach, detach, snapshot, snapshots, destroy, move)`)
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
			name := nameTag(inst.Tags)
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

func stopInstances(ctx context.Context, client *ec2.Client, ids []string) error {
	input := &ec2.StopInstancesInput{
		InstanceIds: ids,
	}
	result, err := client.StopInstances(ctx, input)
	if err != nil {
		return fmt.Errorf("stopping instances: %w", err)
	}
	for _, change := range result.StoppingInstances {
		fmt.Printf("%s: %s -> %s\n",
			*change.InstanceId,
			change.PreviousState.Name,
			change.CurrentState.Name,
		)
	}
	return nil
}

func startInstances(ctx context.Context, client *ec2.Client, ids []string) error {
	input := &ec2.StartInstancesInput{
		InstanceIds: ids,
	}
	result, err := client.StartInstances(ctx, input)
	if err != nil {
		return fmt.Errorf("starting instances: %w", err)
	}
	for _, change := range result.StartingInstances {
		fmt.Printf("%s: %s -> %s\n",
			*change.InstanceId,
			change.PreviousState.Name,
			change.CurrentState.Name,
		)
	}
	return nil
}

func terminateInstances(ctx context.Context, client *ec2.Client, ids []string) error {
	input := &ec2.TerminateInstancesInput{
		InstanceIds: ids,
	}
	result, err := client.TerminateInstances(ctx, input)
	if err != nil {
		return fmt.Errorf("terminating instances: %w", err)
	}
	for _, change := range result.TerminatingInstances {
		fmt.Printf("%s: %s -> %s\n",
			*change.InstanceId,
			change.PreviousState.Name,
			change.CurrentState.Name,
		)
	}
	return nil
}

func updateDNS(ctx context.Context, dcfg devboxConfig, ec2client *ec2.Client, r53client *route53.Client, instanceID string, dnsName string) error {
	// Look up the instance's public IP
	desc, err := ec2client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return fmt.Errorf("describing instance: %w", err)
	}
	if len(desc.Reservations) == 0 || len(desc.Reservations[0].Instances) == 0 {
		return fmt.Errorf("instance %s not found", instanceID)
	}
	inst := desc.Reservations[0].Instances[0]
	if inst.PublicIpAddress == nil {
		return fmt.Errorf("instance %s has no public IP", instanceID)
	}
	ip := *inst.PublicIpAddress

	zoneID, err := findHostedZone(ctx, r53client, dcfg.DNSZone)
	if err != nil {
		return err
	}

	// Upsert the A record
	_, err = r53client.ChangeResourceRecordSets(ctx, &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(zoneID),
		ChangeBatch: &r53types.ChangeBatch{
			Comment: aws.String(fmt.Sprintf("devbox: point %s at %s (%s)", dnsName, instanceID, ip)),
			Changes: []r53types.Change{
				{
					Action: r53types.ChangeActionUpsert,
					ResourceRecordSet: &r53types.ResourceRecordSet{
						Name: aws.String(dnsName),
						Type: r53types.RRTypeA,
						TTL:  aws.Int64(60),
						ResourceRecords: []r53types.ResourceRecord{
							{Value: aws.String(ip)},
						},
					},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("updating DNS record: %w", err)
	}

	fmt.Printf("%s -> %s (%s)\n", dnsName, ip, instanceID)
	return nil
}

func findHostedZone(ctx context.Context, client *route53.Client, domain string) (string, error) {
	result, err := client.ListHostedZonesByName(ctx, &route53.ListHostedZonesByNameInput{
		DNSName:  aws.String(domain),
		MaxItems: aws.Int32(1),
	})
	if err != nil {
		return "", fmt.Errorf("listing hosted zones: %w", err)
	}
	for _, zone := range result.HostedZones {
		if *zone.Name == domain {
			return *zone.Id, nil
		}
	}
	return "", fmt.Errorf("hosted zone for %s not found", domain)
}

func showBids(ctx context.Context, client *ec2.Client) error {
	result, err := client.DescribeSpotInstanceRequests(ctx, &ec2.DescribeSpotInstanceRequestsInput{
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

	if len(result.SpotInstanceRequests) == 0 {
		fmt.Println("No active spot instance requests.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SPOT REQUEST\tINSTANCE ID\tTYPE\tAZ\tMAX PRICE\tSTATE\tSTATUS")

	for _, req := range result.SpotInstanceRequests {
		instanceID := "-"
		if req.InstanceId != nil {
			instanceID = *req.InstanceId
		}
		maxPrice := "-"
		if req.SpotPrice != nil {
			maxPrice = "$" + *req.SpotPrice
		}
		az := "-"
		if req.LaunchedAvailabilityZone != nil {
			az = *req.LaunchedAvailabilityZone
		}
		status := "-"
		if req.Status != nil && req.Status.Code != nil {
			status = *req.Status.Code
		}
		itype := "-"
		if req.LaunchSpecification != nil {
			itype = string(req.LaunchSpecification.InstanceType)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			*req.SpotInstanceRequestId,
			instanceID,
			itype,
			az,
			maxPrice,
			string(req.State),
			status,
		)
	}
	w.Flush()
	return nil
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
	var typeStrings []string
	for _, t := range instanceTypes {
		typeStrings = append(typeStrings, string(t))
	}

	priceResult, err := client.DescribeSpotPriceHistory(ctx, &ec2.DescribeSpotPriceHistoryInput{
		InstanceTypes: instanceTypes,
		StartTime:     &startTime,
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

func rebid(ctx context.Context, client *ec2.Client, spotRequestID string, newPrice string) error {
	// Validate the price parses as a float
	price, err := strconv.ParseFloat(newPrice, 64)
	if err != nil || price <= 0 {
		return fmt.Errorf("invalid price %q: must be a positive number", newPrice)
	}

	// Fetch the existing spot request to clone its parameters
	desc, err := client.DescribeSpotInstanceRequests(ctx, &ec2.DescribeSpotInstanceRequestsInput{
		SpotInstanceRequestIds: []string{spotRequestID},
	})
	if err != nil {
		return fmt.Errorf("describing spot request: %w", err)
	}
	if len(desc.SpotInstanceRequests) == 0 {
		return fmt.Errorf("spot request %s not found", spotRequestID)
	}
	old := desc.SpotInstanceRequests[0]

	oldPrice := "(unset/on-demand)"
	if old.SpotPrice != nil {
		oldPrice = "$" + *old.SpotPrice
	}

	// Cancel the old request
	_, err = client.CancelSpotInstanceRequests(ctx, &ec2.CancelSpotInstanceRequestsInput{
		SpotInstanceRequestIds: []string{spotRequestID},
	})
	if err != nil {
		return fmt.Errorf("canceling old spot request: %w", err)
	}
	fmt.Printf("Canceled old request %s (was %s)\n", spotRequestID, oldPrice)

	// Create a new request with the same launch spec but new price
	priceStr := newPrice
	newReq, err := client.RequestSpotInstances(ctx, &ec2.RequestSpotInstancesInput{
		SpotPrice:               &priceStr,
		InstanceCount:           aws.Int32(1),
		Type:                    old.Type,
		LaunchSpecification:     toLaunchSpec(old.LaunchSpecification),
		AvailabilityZoneGroup:   old.AvailabilityZoneGroup,
		BlockDurationMinutes:    old.BlockDurationMinutes,
		ValidUntil:              old.ValidUntil,
	})
	if err != nil {
		return fmt.Errorf("creating new spot request: %w", err)
	}

	for _, req := range newReq.SpotInstanceRequests {
		fmt.Printf("New request %s with max price $%s\n", *req.SpotInstanceRequestId, newPrice)
	}

	return nil
}

func toLaunchSpec(from *types.LaunchSpecification) *types.RequestSpotLaunchSpecification {
	if from == nil {
		return nil
	}
	spec := &types.RequestSpotLaunchSpecification{
		ImageId:      from.ImageId,
		InstanceType: from.InstanceType,
		KeyName:      from.KeyName,
		SubnetId:     from.SubnetId,
	}
	if from.Placement != nil {
		spec.Placement = &types.SpotPlacement{
			AvailabilityZone: from.Placement.AvailabilityZone,
		}
	}
	if len(from.SecurityGroups) > 0 {
		var sgIDs []string
		for _, sg := range from.SecurityGroups {
			if sg.GroupId != nil {
				sgIDs = append(sgIDs, *sg.GroupId)
			}
		}
		spec.SecurityGroupIds = sgIDs
	}
	if from.BlockDeviceMappings != nil {
		spec.BlockDeviceMappings = from.BlockDeviceMappings
	}
	if from.IamInstanceProfile != nil {
		spec.IamInstanceProfile = &types.IamInstanceProfileSpecification{
			Arn:  from.IamInstanceProfile.Arn,
			Name: from.IamInstanceProfile.Name,
		}
	}
	if from.Monitoring != nil && from.Monitoring.Enabled != nil {
		spec.Monitoring = &types.RunInstancesMonitoringEnabled{
			Enabled: from.Monitoring.Enabled,
		}
	}
	if from.EbsOptimized != nil {
		spec.EbsOptimized = from.EbsOptimized
	}
	return spec
}

func setupDNSOnBoot(ctx context.Context, dcfg devboxConfig, ec2client *ec2.Client, instanceID string) error {
	desc, err := ec2client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return fmt.Errorf("describing instance: %w", err)
	}
	if len(desc.Reservations) == 0 || len(desc.Reservations[0].Instances) == 0 {
		return fmt.Errorf("instance %s not found", instanceID)
	}
	inst := desc.Reservations[0].Instances[0]
	if inst.PublicIpAddress == nil {
		return fmt.Errorf("instance %s has no public IP (is it running?)", instanceID)
	}
	ip := *inst.PublicIpAddress

	// Find the hosted zone ID so we can bake it into the script
	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("loading AWS config: %w", err)
	}
	r53client := route53.NewFromConfig(awsCfg)
	zoneID, err := findHostedZone(ctx, r53client, dcfg.DNSZone)
	if err != nil {
		return err
	}

	keyPath := dcfg.resolveSSHKeyPath()

	// The script that runs on boot to update Route 53
	bootScript := fmt.Sprintf(`#!/bin/bash
set -e

# Wait for network and metadata
sleep 5

TOKEN=$(curl -s -X PUT "http://169.254.169.254/latest/api/token" \
  -H "X-aws-ec2-metadata-token-ttl-seconds: 60")

PUBLIC_IP=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" \
  http://169.254.169.254/latest/meta-data/public-ipv4)

if [ -z "$PUBLIC_IP" ]; then
  echo "No public IP found, skipping DNS update"
  exit 0
fi

aws route53 change-resource-record-sets \
  --hosted-zone-id %q \
  --change-batch '{
    "Comment": "devbox boot DNS update",
    "Changes": [{
      "Action": "UPSERT",
      "ResourceRecordSet": {
        "Name": "%s",
        "Type": "A",
        "TTL": 60,
        "ResourceRecords": [{"Value": "'$PUBLIC_IP'"}]
      }
    }]
  }'

echo "Updated %s -> $PUBLIC_IP"
`, zoneID, dcfg.DNSName, dcfg.DNSName)

	serviceUnit := fmt.Sprintf(`[Unit]
Description=Update %s DNS on boot
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/opt/update-dns.sh

[Install]
WantedBy=multi-user.target
`, dcfg.DNSName)

	// Commands to install the script and service on the remote box
	installCmd := fmt.Sprintf(
		`cat > /tmp/update-dns.sh << 'SCRIPT'
%s
SCRIPT
sudo mv /tmp/update-dns.sh /opt/update-dns.sh
sudo chmod +x /opt/update-dns.sh

cat > /tmp/update-dns.service << 'UNIT'
%s
UNIT
sudo mv /tmp/update-dns.service /etc/systemd/system/update-dns.service
sudo systemctl daemon-reload
sudo systemctl enable update-dns.service
echo "DNS boot script installed and enabled"`,
		bootScript, serviceUnit)

	fmt.Printf("Installing DNS boot script on %s (%s)...\n", instanceID, ip)

	cmd := exec.CommandContext(ctx, "ssh",
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=no",
		dcfg.SSHUser+"@"+ip,
		installCmd,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ssh command failed: %w", err)
	}

	fmt.Printf("Done. %s will update %s on every boot.\n", instanceID, dcfg.DNSName)
	return nil
}

func sshToInstance(ctx context.Context, dcfg devboxConfig, client *ec2.Client, instanceID string) error {
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
	if inst.PublicIpAddress == nil {
		return fmt.Errorf("instance %s has no public IP", instanceID)
	}
	ip := *inst.PublicIpAddress

	keyPath := dcfg.resolveSSHKeyPath()

	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh not found in PATH: %w", err)
	}

	fmt.Printf("Connecting to %s (%s)...\n", instanceID, ip)
	return syscall.Exec(sshBin, []string{"ssh", "-i", keyPath, dcfg.SSHUser + "@" + ip}, os.Environ())
}

func nameTag(tags []types.Tag) string {
	for _, t := range tags {
		if *t.Key == "Name" {
			return *t.Value
		}
	}
	return "-"
}

// --- resize command ---

func resizeInstance(ctx context.Context, dcfg devboxConfig, client *ec2.Client, r53client *route53.Client, instanceID, newType string) error {
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

	// Stop if running
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

	// Modify instance type
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

	// Start instance
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

	// Update DNS (non-fatal)
	if err := updateDNS(ctx, dcfg, client, r53client, instanceID, dcfg.DNSName); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: DNS update failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "The NixOS boot service should update DNS automatically.")
	}

	// Warn about persistent spot request
	if inst.SpotInstanceRequestId != nil {
		fmt.Printf("\nNote: The persistent spot request %s still references type %s.\n", *inst.SpotInstanceRequestId, currentType)
		fmt.Println("Run 'devbox rebid' if you want to update the spot request too.")
	}

	return nil
}

// --- search command ---

type spotSearchResult struct {
	InstanceType string
	VCPUs        int32
	MemoryMiB    int64
	AZ           string
	Price        float64
	GPU          bool
}

func searchSpotPrices(ctx context.Context, client *ec2.Client, args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	minVCPU := fs.Int("min-vcpu", 8, "Minimum vCPUs")
	minMem := fs.Float64("min-mem", 16, "Minimum memory (GiB)")
	maxPrice := fs.Float64("max-price", 0, "Max spot price $/hr (0 = no limit)")
	arch := fs.String("arch", "x86_64", "Architecture (x86_64 or arm64)")
	gpu := fs.Bool("gpu", false, "Require GPU")
	az := fs.String("az", "", "Filter by availability zone")
	sortBy := fs.String("sort", "price", "Sort by: price, vcpu, mem")
	limit := fs.Int("limit", 20, "Max rows to display")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// If specific instance types were passed as positional args, look those up directly
	var instanceTypes []instanceTypeInfo
	var err error
	if fs.NArg() > 0 {
		fmt.Println("Looking up instance types...")
		var typeNames []types.InstanceType
		for _, arg := range fs.Args() {
			typeNames = append(typeNames, types.InstanceType(arg))
		}
		instanceTypes, err = describeSpecificTypes(ctx, client, typeNames)
		if err != nil {
			return err
		}
	} else {
		// Broad search by hardware specs
		fmt.Println("Fetching instance types...")
		instanceTypes, err = fetchInstanceTypes(ctx, client, *arch, *minVCPU, *minMem, *gpu)
		if err != nil {
			return err
		}
	}
	if len(instanceTypes) == 0 {
		fmt.Println("No instance types match the given filters.")
		return nil
	}

	// 2. Fetch spot prices for those types
	fmt.Printf("Fetching spot prices for %d instance types...\n", len(instanceTypes))
	results, err := fetchSpotPrices(ctx, client, instanceTypes, *az)
	if err != nil {
		return err
	}

	// 3. Apply max price filter
	if *maxPrice > 0 {
		var filtered []spotSearchResult
		for _, r := range results {
			if r.Price <= *maxPrice {
				filtered = append(filtered, r)
			}
		}
		results = filtered
	}

	if len(results) == 0 {
		fmt.Println("No spot prices found matching filters.")
		return nil
	}

	// 4. Sort
	switch *sortBy {
	case "vcpu":
		sort.Slice(results, func(i, j int) bool { return results[i].VCPUs < results[j].VCPUs })
	case "mem":
		sort.Slice(results, func(i, j int) bool { return results[i].MemoryMiB < results[j].MemoryMiB })
	default:
		sort.Slice(results, func(i, j int) bool { return results[i].Price < results[j].Price })
	}

	// 5. Truncate
	if *limit > 0 && len(results) > *limit {
		results = results[:*limit]
	}

	// 6. Display
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "INSTANCE TYPE\tVCPU\tMEMORY\tAZ\tPRICE\tGPU")
	for _, r := range results {
		gpuStr := "-"
		if r.GPU {
			gpuStr = "yes"
		}
		fmt.Fprintf(w, "%s\t%d\t%.0f GiB\t%s\t$%.4f\t%s\n",
			r.InstanceType, r.VCPUs, float64(r.MemoryMiB)/1024.0, r.AZ, r.Price, gpuStr)
	}
	w.Flush()
	return nil
}

type instanceTypeInfo struct {
	Name      string
	VCPUs     int32
	MemoryMiB int64
	HasGPU    bool
}

func fetchInstanceTypes(ctx context.Context, client *ec2.Client, arch string, minVCPU int, minMem float64, requireGPU bool) ([]instanceTypeInfo, error) {
	var results []instanceTypeInfo
	minMemMiB := int64(minMem * 1024)

	input := &ec2.DescribeInstanceTypesInput{
		Filters: []types.Filter{
			{Name: aws.String("supported-usage-class"), Values: []string{"spot"}},
			{Name: aws.String("current-generation"), Values: []string{"true"}},
			{Name: aws.String("processor-info.supported-architecture"), Values: []string{arch}},
		},
	}

	paginator := ec2.NewDescribeInstanceTypesPaginator(client, input)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("describing instance types: %w", err)
		}
		for _, it := range page.InstanceTypes {
			vcpus := *it.VCpuInfo.DefaultVCpus
			memMiB := *it.MemoryInfo.SizeInMiB
			hasGPU := it.GpuInfo != nil && len(it.GpuInfo.Gpus) > 0

			if int(vcpus) < minVCPU {
				continue
			}
			if memMiB < minMemMiB {
				continue
			}
			if requireGPU && !hasGPU {
				continue
			}

			results = append(results, instanceTypeInfo{
				Name:      string(it.InstanceType),
				VCPUs:     vcpus,
				MemoryMiB: memMiB,
				HasGPU:    hasGPU,
			})
		}
	}
	return results, nil
}

func describeSpecificTypes(ctx context.Context, client *ec2.Client, typeNames []types.InstanceType) ([]instanceTypeInfo, error) {
	result, err := client.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{
		InstanceTypes: typeNames,
	})
	if err != nil {
		return nil, fmt.Errorf("describing instance types: %w", err)
	}
	var infos []instanceTypeInfo
	for _, it := range result.InstanceTypes {
		hasGPU := it.GpuInfo != nil && len(it.GpuInfo.Gpus) > 0
		infos = append(infos, instanceTypeInfo{
			Name:      string(it.InstanceType),
			VCPUs:     *it.VCpuInfo.DefaultVCpus,
			MemoryMiB: *it.MemoryInfo.SizeInMiB,
			HasGPU:    hasGPU,
		})
	}
	return infos, nil
}

func fetchSpotPrices(ctx context.Context, client *ec2.Client, instanceTypes []instanceTypeInfo, azFilter string) ([]spotSearchResult, error) {
	// Build lookup map
	infoMap := map[string]instanceTypeInfo{}
	var typeNames []types.InstanceType
	for _, it := range instanceTypes {
		infoMap[it.Name] = it
		typeNames = append(typeNames, types.InstanceType(it.Name))
	}

	// Paginate spot price history in batches (API allows ~100 instance types per call)
	type priceKey struct {
		itype string
		az    string
	}
	latest := map[priceKey]types.SpotPrice{}
	startTime := time.Now().Add(-1 * time.Hour)

	batchSize := 100
	for i := 0; i < len(typeNames); i += batchSize {
		end := i + batchSize
		if end > len(typeNames) {
			end = len(typeNames)
		}
		batch := typeNames[i:end]

		input := &ec2.DescribeSpotPriceHistoryInput{
			InstanceTypes:       batch,
			StartTime:           &startTime,
			ProductDescriptions: []string{"Linux/UNIX"},
		}

		paginator := ec2.NewDescribeSpotPriceHistoryPaginator(client, input)
		for paginator.HasMorePages() {
			page, err := paginator.NextPage(ctx)
			if err != nil {
				return nil, fmt.Errorf("describing spot price history: %w", err)
			}
			for _, sp := range page.SpotPriceHistory {
				if azFilter != "" && *sp.AvailabilityZone != azFilter {
					continue
				}
				k := priceKey{string(sp.InstanceType), *sp.AvailabilityZone}
				existing, ok := latest[k]
				if !ok || sp.Timestamp.After(*existing.Timestamp) {
					latest[k] = sp
				}
			}
		}
	}

	var results []spotSearchResult
	for k, sp := range latest {
		info := infoMap[k.itype]
		price, _ := strconv.ParseFloat(*sp.SpotPrice, 64)
		results = append(results, spotSearchResult{
			InstanceType: k.itype,
			VCPUs:        info.VCPUs,
			MemoryMiB:    info.MemoryMiB,
			AZ:           k.az,
			Price:        price,
			GPU:          info.HasGPU,
		})
	}
	return results, nil
}

// --- spawn command ---

func spawnInstance(ctx context.Context, dcfg devboxConfig, client *ec2.Client, args []string) error {
	fs := flag.NewFlagSet("spawn", flag.ExitOnError)
	instanceType := fs.String("type", dcfg.DefaultType, "Instance type")
	az := fs.String("az", dcfg.DefaultAZ, "Availability zone")
	name := fs.String("name", dcfg.SpawnName, "Name tag for the instance")
	maxPrice := fs.String("max-price", dcfg.DefaultMaxPrice, "Spot max price $/hr")
	from := fs.String("from", "", "Instance ID to clone user_data from")
	if err := fs.Parse(args); err != nil {
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

	subnetID, err := lookupSubnet(ctx, client, *az)
	if err != nil {
		return err
	}
	fmt.Printf("  Subnet: %s\n", subnetID)

	// Get user_data from source instance
	sourceID := *from
	if sourceID == "" {
		sourceID, err = autoDetectSourceInstance(ctx, client)
		if err != nil {
			return err
		}
	}
	fmt.Printf("  Cloning user_data from: %s\n", sourceID)

	userData, err := fetchUserData(ctx, client, sourceID)
	if err != nil {
		return err
	}

	// Launch the instance
	fmt.Printf("Launching %s spot instance in %s...\n", *instanceType, *az)

	runInput := &ec2.RunInstancesInput{
		ImageId:      aws.String(amiID),
		InstanceType: types.InstanceType(*instanceType),
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
				MaxPrice:                     maxPrice,
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
					{Key: aws.String("Name"), Value: aws.String(*name)},
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
	}, 5*time.Minute); err != nil {
		return fmt.Errorf("waiting for instance to start: %w", err)
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

	fmt.Printf("\nInstance ready:\n")
	fmt.Printf("  ID:        %s\n", newID)
	fmt.Printf("  Type:      %s\n", *instanceType)
	fmt.Printf("  AZ:        %s\n", *az)
	fmt.Printf("  Public IP: %s\n", publicIP)
	if publicIP != "-" {
		fmt.Printf("  SSH:       ssh -i %s %s@%s\n", dcfg.SSHKeyPath, dcfg.SSHUser, publicIP)
	}
	return nil
}

func lookupAMI(ctx context.Context, dcfg devboxConfig, client *ec2.Client) (string, error) {
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
		return "", fmt.Errorf("no NixOS 24.11 AMI found")
	}
	// Pick the latest by sorting on name (NixOS AMI names include dates)
	sort.Slice(result.Images, func(i, j int) bool {
		return *result.Images[i].Name > *result.Images[j].Name
	})
	return *result.Images[0].ImageId, nil
}

func lookupSecurityGroup(ctx context.Context, dcfg devboxConfig, client *ec2.Client) (string, error) {
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

func fetchUserData(ctx context.Context, client *ec2.Client, instanceID string) (string, error) {
	result, err := client.DescribeInstanceAttribute(ctx, &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String(instanceID),
		Attribute:  types.InstanceAttributeNameUserData,
	})
	if err != nil {
		return "", fmt.Errorf("fetching user_data from %s: %w", instanceID, err)
	}
	if result.UserData == nil || result.UserData.Value == nil {
		return "", fmt.Errorf("instance %s has no user_data", instanceID)
	}
	// The API returns base64-encoded data. RunInstances also expects base64,
	// but let's verify it decodes properly.
	decoded, err := base64.StdEncoding.DecodeString(*result.UserData.Value)
	if err != nil {
		return "", fmt.Errorf("user_data is not valid base64: %w", err)
	}
	// Re-encode since RunInstances expects base64
	return base64.StdEncoding.EncodeToString(decoded), nil
}

// --- volume commands ---

func volumeCommand(ctx context.Context, dcfg devboxConfig, client *ec2.Client, awsCfg aws.Config, args []string) error {
	if len(args) == 0 {
		printVolumeUsage()
		return nil
	}
	switch args[0] {
	case "ls", "list":
		return volumeLS(ctx, client)
	case "create":
		return volumeCreate(ctx, dcfg, client, args[1:])
	case "attach":
		return volumeAttach(ctx, client, args[1:])
	case "detach":
		return volumeDetach(ctx, client, args[1:])
	case "snapshot":
		return volumeSnapshot(ctx, client, args[1:])
	case "snapshots":
		return volumeSnapshots(ctx, client)
	case "destroy":
		return volumeDestroy(ctx, client, args[1:])
	case "move":
		return volumeMove(ctx, client, awsCfg, args[1:])
	default:
		printVolumeUsage()
		return fmt.Errorf("unknown volume subcommand: %s", args[0])
	}
}

func printVolumeUsage() {
	fmt.Fprintln(os.Stderr, `Usage: devbox volume <subcommand> [args]

Subcommands:
  ls                                 List EBS volumes
  create   [flags]                   Create a new EBS volume
  attach   <volume> <instance-id>    Attach a volume to an instance
  detach   <volume>                  Detach a volume
  snapshot <volume>                  Create a snapshot of a volume
  snapshots                          List snapshots
  destroy  <volume>                  Delete a volume (must be detached)
  move     <volume> <target-region>  Move a volume to another region

Volumes can be specified by ID (vol-xxx) or by Name tag.`)
}

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

func volumeLS(ctx context.Context, client *ec2.Client) error {
	result, err := client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{})
	if err != nil {
		return fmt.Errorf("describing volumes: %w", err)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "VOLUME ID\tNAME\tSIZE\tTYPE\tIOPS\tSTATE\tAZ\tATTACHED TO")
	for _, v := range result.Volumes {
		name := nameTag(v.Tags)
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

func volumeCreate(ctx context.Context, dcfg devboxConfig, client *ec2.Client, args []string) error {
	fs := flag.NewFlagSet("volume create", flag.ExitOnError)
	size := fs.Int("size", 512, "Volume size in GiB")
	volType := fs.String("type", "gp3", "Volume type")
	iops := fs.Int("iops", 3000, "IOPS")
	throughput := fs.Int("throughput", 250, "Throughput MB/s")
	az := fs.String("az", dcfg.DefaultAZ, "Availability zone")
	name := fs.String("name", "dev-data-volume", "Name tag")
	if err := fs.Parse(args); err != nil {
		return err
	}

	input := &ec2.CreateVolumeInput{
		AvailabilityZone: az,
		Size:             aws.Int32(int32(*size)),
		VolumeType:       types.VolumeType(*volType),
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeVolume,
				Tags: []types.Tag{
					{Key: aws.String("Name"), Value: aws.String(*name)},
				},
			},
		},
	}
	if *volType == "gp3" || *volType == "io1" || *volType == "io2" {
		input.Iops = aws.Int32(int32(*iops))
	}
	if *volType == "gp3" {
		input.Throughput = aws.Int32(int32(*throughput))
	}

	result, err := client.CreateVolume(ctx, input)
	if err != nil {
		return fmt.Errorf("creating volume: %w", err)
	}
	volID := *result.VolumeId
	fmt.Printf("Created volume %s, waiting for available state...\n", volID)

	if err := pollVolumeState(ctx, client, volID, "available", 5*time.Second, 2*time.Minute); err != nil {
		return err
	}
	fmt.Printf("Volume %s is available.\n", volID)
	return nil
}

func volumeAttach(ctx context.Context, client *ec2.Client, args []string) error {
	fs := flag.NewFlagSet("volume attach", flag.ExitOnError)
	device := fs.String("device", "/dev/xvdf", "Device name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: devbox volume attach [--device DEV] <volume> <instance-id>")
	}

	volID, err := resolveVolume(ctx, client, fs.Arg(0))
	if err != nil {
		return err
	}

	_, err = client.AttachVolume(ctx, &ec2.AttachVolumeInput{
		VolumeId:   aws.String(volID),
		InstanceId: aws.String(fs.Arg(1)),
		Device:     device,
	})
	if err != nil {
		return fmt.Errorf("attaching volume: %w", err)
	}
	fmt.Printf("Attaching %s to %s as %s, waiting...\n", volID, fs.Arg(1), *device)

	if err := pollVolumeState(ctx, client, volID, "in-use", 5*time.Second, 2*time.Minute); err != nil {
		return err
	}
	fmt.Println("Volume attached.")
	return nil
}

func volumeDetach(ctx context.Context, client *ec2.Client, args []string) error {
	fs := flag.NewFlagSet("volume detach", flag.ExitOnError)
	force := fs.Bool("force", false, "Force detach")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: devbox volume detach [--force] <volume>")
	}

	volID, err := resolveVolume(ctx, client, fs.Arg(0))
	if err != nil {
		return err
	}

	_, err = client.DetachVolume(ctx, &ec2.DetachVolumeInput{
		VolumeId: aws.String(volID),
		Force:    force,
	})
	if err != nil {
		return fmt.Errorf("detaching volume: %w", err)
	}
	fmt.Printf("Detaching %s, waiting...\n", volID)

	if err := pollVolumeState(ctx, client, volID, "available", 5*time.Second, 2*time.Minute); err != nil {
		return err
	}
	fmt.Println("Volume detached.")
	return nil
}

func volumeSnapshot(ctx context.Context, client *ec2.Client, args []string) error {
	fs := flag.NewFlagSet("volume snapshot", flag.ExitOnError)
	name := fs.String("name", "", "Description/tag for the snapshot")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: devbox volume snapshot [--name DESC] <volume>")
	}

	volID, err := resolveVolume(ctx, client, fs.Arg(0))
	if err != nil {
		return err
	}

	input := &ec2.CreateSnapshotInput{
		VolumeId: aws.String(volID),
	}
	if *name != "" {
		input.Description = name
		input.TagSpecifications = []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeSnapshot,
				Tags: []types.Tag{
					{Key: aws.String("Name"), Value: aws.String(*name)},
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

func volumeDestroy(ctx context.Context, client *ec2.Client, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: devbox volume destroy <volume>")
	}

	volID, err := resolveVolume(ctx, client, args[0])
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

func volumeMove(ctx context.Context, client *ec2.Client, awsCfg aws.Config, args []string) error {
	fs := flag.NewFlagSet("volume move", flag.ExitOnError)
	targetAZ := fs.String("az", "", "Target AZ (default: <region>a)")
	cleanup := fs.Bool("cleanup", false, "Delete intermediate snapshots after move")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: devbox volume move [--az AZ] [--cleanup] <volume> <target-region>")
	}

	volID, err := resolveVolume(ctx, client, fs.Arg(0))
	if err != nil {
		return err
	}
	targetRegion := fs.Arg(1)

	if *targetAZ == "" {
		*targetAZ = targetRegion + "a"
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
	sourceRegion := awsCfg.Region

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
	if err := pollSnapshotState(ctx, client, srcSnapID, "completed", 15*time.Second, 30*time.Minute); err != nil {
		return fmt.Errorf("waiting for source snapshot: %w", err)
	}
	fmt.Println("Source snapshot completed.")

	// Step 2: Create client for target region
	targetCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(targetRegion))
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
	if err := pollSnapshotState(ctx, targetClient, dstSnapID, "completed", 15*time.Second, 30*time.Minute); err != nil {
		return fmt.Errorf("waiting for target snapshot: %w", err)
	}
	fmt.Println("Target snapshot completed.")

	// Step 4: Create volume from copied snapshot
	createInput := &ec2.CreateVolumeInput{
		AvailabilityZone: targetAZ,
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

	fmt.Printf("Creating volume in %s...\n", *targetAZ)
	newVol, err := targetClient.CreateVolume(ctx, createInput)
	if err != nil {
		return fmt.Errorf("creating volume in target region: %w", err)
	}
	newVolID := *newVol.VolumeId

	if err := pollVolumeState(ctx, targetClient, newVolID, "available", 5*time.Second, 2*time.Minute); err != nil {
		return fmt.Errorf("waiting for new volume: %w", err)
	}

	fmt.Printf("\nVolume moved successfully!\n")
	fmt.Printf("  New volume: %s in %s\n", newVolID, *targetAZ)

	// Step 5: Cleanup intermediate snapshots if requested
	if *cleanup {
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

func pollVolumeState(ctx context.Context, client *ec2.Client, volumeID, desiredState string, interval, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for volume %s to reach state %q", volumeID, desiredState)
		}
		result, err := client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
			VolumeIds: []string{volumeID},
		})
		if err != nil {
			return fmt.Errorf("polling volume state: %w", err)
		}
		if len(result.Volumes) > 0 && string(result.Volumes[0].State) == desiredState {
			return nil
		}
		time.Sleep(interval)
	}
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
