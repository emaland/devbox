package cmd

import (
	"context"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
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
	)
	return root
}

func Execute() {
	if err := NewRootCmd().ExecuteContext(context.Background()); err != nil {
		os.Exit(1)
	}
}
