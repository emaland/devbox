package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/spf13/cobra"

	devboxconfig "github.com/emaland/devbox/internal/config"
)

var (
	dcfg      devboxconfig.DevboxConfig
	awsCfg    aws.Config
	ec2Client *ec2.Client

	VolumePollInterval   = 5 * time.Second
	SnapshotPollInterval = 15 * time.Second
	BaseEndpointOverride string
)

const awsCredentialGuidance = `AWS credentials not found. Configure them using one of:

  aws sso login                        If you use AWS IAM Identity Center (SSO)
  aws configure                        Interactive setup for ~/.aws/credentials
  export AWS_ACCESS_KEY_ID=...         Set credentials via environment variables
  export AWS_SECRET_ACCESS_KEY=...
  export AWS_PROFILE=my-profile        Use a named profile from ~/.aws/config

Docs: https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-files.html`

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "devbox",
		Short: "Manage AWS spot instances",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			var err error
			dcfg, err = devboxconfig.LoadConfig()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			awsCfg, err = config.LoadDefaultConfig(ctx)
			if err != nil {
				return err
			}

			// Verify credentials are valid before any command runs.
			stsClient := sts.NewFromConfig(awsCfg)
			if _, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}); err != nil {
				fmt.Fprintln(os.Stderr, awsCredentialGuidance)
				return err
			}

			ec2Client = ec2.NewFromConfig(awsCfg)
			return nil
		},
		SilenceUsage: true,
	}
	root.AddCommand(
		newListCmd(),
		newStopCmd(),
		newStartCmd(),
		newRebootCmd(),
		newRestartCmd(),
		newTerminateCmd(),
		newDNSCmd(),
		newBidsCmd(),
		newPricesCmd(),
		newRebidCmd(),
		newSSHCmd(),
		newSetupDNSCmd(),
		newSearchCmd(),
		newResizeCmd(),
		newRecoverCmd(),
		newSpawnCmd(),
		newVolumeCmd(),
		newInfraCmd(),
	)
	return root
}

func Execute() {
	if err := NewRootCmd().ExecuteContext(context.Background()); err != nil {
		os.Exit(1)
	}
}
