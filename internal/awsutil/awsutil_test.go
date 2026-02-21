package awsutil

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func TestNameTag(t *testing.T) {
	tags := []types.Tag{
		{Key: aws.String("Env"), Value: aws.String("prod")},
		{Key: aws.String("Name"), Value: aws.String("my-box")},
	}
	if got := NameTag(tags); got != "my-box" {
		t.Errorf("NameTag = %q, want %q", got, "my-box")
	}
	if got := NameTag(nil); got != "-" {
		t.Errorf("NameTag(nil) = %q, want %q", got, "-")
	}
}
