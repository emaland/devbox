package cmd

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	r53types "github.com/aws/aws-sdk-go-v2/service/route53/types"
	"github.com/spf13/cobra"

	"github.com/emaland/devbox/internal/awsutil"
	"github.com/emaland/devbox/internal/config"
)

func newDNSCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "dns <instance-id> [dns-name]",
		Short: "Point a DNS name at an instance's public IP",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			r53client := route53.NewFromConfig(awsCfg)
			dnsName := dcfg.DNSName
			if len(args) > 1 {
				dnsName = args[1]
			}
			return updateDNS(cmd.Context(), dcfg, ec2Client, r53client, args[0], dnsName)
		},
	}
}

func updateDNS(ctx context.Context, dcfg config.DevboxConfig, ec2client *ec2.Client, r53client *route53.Client, instanceID string, dnsName string) error {
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

	zoneID, err := awsutil.FindHostedZone(ctx, r53client, dcfg.DNSZone)
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
