package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/spf13/cobra"

	"github.com/emaland/devbox/internal/awsutil"
	"github.com/emaland/devbox/internal/config"
)

func newSetupDNSCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup-dns <instance-id>",
		Short: "Install a boot script that updates dev.frob.io on startup",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setupDNSOnBoot(cmd.Context(), dcfg, ec2Client, args[0])
		},
	}
}

func setupDNSOnBoot(ctx context.Context, dcfg config.DevboxConfig, ec2client *ec2.Client, instanceID string) error {
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
	loadedCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("loading AWS config: %w", err)
	}
	r53client := route53.NewFromConfig(loadedCfg)
	zoneID, err := awsutil.FindHostedZone(ctx, r53client, dcfg.DNSZone)
	if err != nil {
		return err
	}

	keyPath := dcfg.ResolveSSHKeyPath()

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

	sshCmd := exec.CommandContext(ctx, "ssh",
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=no",
		dcfg.SSHUser+"@"+ip,
		installCmd,
	)
	sshCmd.Stdout = os.Stdout
	sshCmd.Stderr = os.Stderr
	if err := sshCmd.Run(); err != nil {
		return fmt.Errorf("ssh command failed: %w", err)
	}

	fmt.Printf("Done. %s will update %s on every boot.\n", instanceID, dcfg.DNSName)
	return nil
}
