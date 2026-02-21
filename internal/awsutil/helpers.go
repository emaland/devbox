package awsutil

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/route53"
)

func NameTag(tags []types.Tag) string {
	for _, t := range tags {
		if *t.Key == "Name" {
			return *t.Value
		}
	}
	return "-"
}

func FindHostedZone(ctx context.Context, client *route53.Client, domain string) (string, error) {
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

func FetchUserData(ctx context.Context, client *ec2.Client, instanceID string) (string, error) {
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

func PollVolumeState(ctx context.Context, client *ec2.Client, volumeID, desiredState string, interval, timeout time.Duration) error {
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
