package main

import (
	"context"
	"encoding/base64"
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

func main() {
	if len(os.Args) < 2 {
		printUsage()
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
			fmt.Fprintln(os.Stderr, "Usage: devbox dns <instance-id>")
			os.Exit(1)
		}
		r53client := route53.NewFromConfig(cfg)
		if err := updateDNS(ctx, client, r53client, os.Args[2]); err != nil {
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
		if err := sshToInstance(ctx, client, os.Args[2]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "setup-dns":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: devbox setup-dns <instance-id>")
			os.Exit(1)
		}
		if err := setupDNSOnBoot(ctx, client, os.Args[2]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "resize":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "Usage: devbox resize <instance-id> <new-type>")
			os.Exit(1)
		}
		r53client := route53.NewFromConfig(cfg)
		if err := resizeInstance(ctx, client, r53client, os.Args[2], os.Args[3]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "search":
		if err := searchSpotPrices(ctx, client, os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "spawn":
		if err := spawnInstance(ctx, client, os.Args[2:]); err != nil {
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
  dns      <instance-id>            Point dev.frob.io at the instance's public IP
  bids                              Show current spot request bids (max price)
  prices                            Show current spot market prices for our instance types
  rebid    <spot-req-id> <price>    Cancel and re-create a spot request with a new max price
  ssh      <instance-id>            SSH into an instance
  setup-dns <instance-id>           Install a boot script that updates dev.frob.io on startup
  search   [flags]                  Browse spot prices by hardware specs
  resize   <instance-id> <type>     Stop instance, change type, restart, update DNS
  spawn    [flags]                  Spin up a new spot instance cloned from the primary`)
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

const dnsName = "dev.frob.io"

func updateDNS(ctx context.Context, ec2client *ec2.Client, r53client *route53.Client, instanceID string) error {
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

	// Find the hosted zone for frob.io
	zoneID, err := findHostedZone(ctx, r53client, "frob.io.")
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

func setupDNSOnBoot(ctx context.Context, ec2client *ec2.Client, instanceID string) error {
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
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("loading AWS config: %w", err)
	}
	r53client := route53.NewFromConfig(cfg)
	zoneID, err := findHostedZone(ctx, r53client, "frob.io.")
	if err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("getting home dir: %w", err)
	}
	keyPath := filepath.Join(home, ".ssh", "dev-boxes.pem")

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
`, zoneID, dnsName, dnsName)

	serviceUnit := `[Unit]
Description=Update dev.frob.io DNS on boot
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/opt/update-dns.sh

[Install]
WantedBy=multi-user.target
`

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
		"emaland@"+ip,
		installCmd,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ssh command failed: %w", err)
	}

	fmt.Printf("Done. %s will update %s on every boot.\n", instanceID, dnsName)
	return nil
}

func sshToInstance(ctx context.Context, client *ec2.Client, instanceID string) error {
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

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("getting home dir: %w", err)
	}
	keyPath := filepath.Join(home, ".ssh", "dev-boxes.pem")

	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh not found in PATH: %w", err)
	}

	fmt.Printf("Connecting to %s (%s)...\n", instanceID, ip)
	return syscall.Exec(sshBin, []string{"ssh", "-i", keyPath, "emaland@" + ip}, os.Environ())
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

func resizeInstance(ctx context.Context, client *ec2.Client, r53client *route53.Client, instanceID, newType string) error {
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
	if err := updateDNS(ctx, client, r53client, instanceID); err != nil {
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

	// 1. Fetch instance types matching filters
	fmt.Println("Fetching instance types...")
	instanceTypes, err := fetchInstanceTypes(ctx, client, *arch, *minVCPU, *minMem, *gpu)
	if err != nil {
		return err
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

func spawnInstance(ctx context.Context, client *ec2.Client, args []string) error {
	fs := flag.NewFlagSet("spawn", flag.ExitOnError)
	instanceType := fs.String("type", "m6i.4xlarge", "Instance type")
	az := fs.String("az", "us-east-2a", "Availability zone")
	name := fs.String("name", "dev-workstation-tmp", "Name tag for the instance")
	maxPrice := fs.String("max-price", "2.00", "Spot max price $/hr")
	from := fs.String("from", "", "Instance ID to clone user_data from")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Discover infrastructure
	fmt.Println("Looking up infrastructure...")

	amiID, err := lookupAMI(ctx, client)
	if err != nil {
		return err
	}
	fmt.Printf("  AMI: %s\n", amiID)

	sgID, err := lookupSecurityGroup(ctx, client)
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
		KeyName:      aws.String("dev-boxes"),
		SubnetId:     aws.String(subnetID),
		SecurityGroupIds: []string{sgID},
		IamInstanceProfile: &types.IamInstanceProfileSpecification{
			Name: aws.String("dev-workstation-profile"),
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
		fmt.Printf("  SSH:       ssh -i ~/.ssh/dev-boxes.pem emaland@%s\n", publicIP)
	}
	return nil
}

func lookupAMI(ctx context.Context, client *ec2.Client) (string, error) {
	result, err := client.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners: []string{"427812963091"},
		Filters: []types.Filter{
			{Name: aws.String("name"), Values: []string{"nixos/24.11*"}},
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

func lookupSecurityGroup(ctx context.Context, client *ec2.Client) (string, error) {
	result, err := client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		GroupNames: []string{"dev-instance"},
	})
	if err != nil {
		return "", fmt.Errorf("looking up security group: %w", err)
	}
	if len(result.SecurityGroups) == 0 {
		return "", fmt.Errorf("security group 'dev-instance' not found")
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
