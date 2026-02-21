package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"text/tabwriter"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/spf13/cobra"

	"github.com/emaland/devbox/internal/awsutil"
	"github.com/emaland/devbox/internal/config"
)

func newRecoverCmd() *cobra.Command {
	var (
		minVCPUFlag int
		minMemFlag  float64
		maxPrice    float64
		autoYes     bool
	)

	cmd := &cobra.Command{
		Use:   "recover <instance-id>",
		Short: "Find alternative instance types with spot capacity in the same AZ",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r53client := route53.NewFromConfig(awsCfg)
			return recoverInstance(cmd.Context(), dcfg, ec2Client, r53client, args[0], minVCPUFlag, minMemFlag, maxPrice, autoYes)
		},
	}

	cmd.Flags().IntVar(&minVCPUFlag, "min-vcpu", 0, "Minimum vCPUs (default: 50% of current)")
	cmd.Flags().Float64Var(&minMemFlag, "min-mem", 0, "Minimum memory in GiB (default: 50% of current)")
	cmd.Flags().Float64Var(&maxPrice, "max-price", 0, "Max spot price $/hr (0 = use config default)")
	cmd.Flags().BoolVar(&autoYes, "yes", false, "Auto-pick cheapest candidate and resize")

	return cmd
}

func recoverInstance(ctx context.Context, dcfg config.DevboxConfig, client *ec2.Client, r53client *route53.Client, instanceID string, minVCPUFlag int, minMemFlag, maxPriceFlag float64, autoYes bool) error {
	// 1. Describe the instance
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
	az := *inst.Placement.AvailabilityZone
	state := string(inst.State.Name)

	if inst.State.Name == types.InstanceStateNameTerminated {
		return fmt.Errorf("instance %s is terminated", instanceID)
	}

	fmt.Printf("Instance %s: %s (%s) in %s\n", instanceID, currentType, state, az)

	// Show attached volumes
	for _, bdm := range inst.BlockDeviceMappings {
		if bdm.Ebs != nil && bdm.Ebs.VolumeId != nil {
			fmt.Printf("  Volume: %s (%s)\n", *bdm.Ebs.VolumeId, *bdm.DeviceName)
		}
	}

	// 2. Get current instance type specs and architecture
	typeDesc, err := client.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{
		InstanceTypes: []types.InstanceType{types.InstanceType(currentType)},
	})
	if err != nil {
		return fmt.Errorf("describing instance type %s: %w", currentType, err)
	}
	if len(typeDesc.InstanceTypes) == 0 {
		return fmt.Errorf("instance type %s not found", currentType)
	}
	typeInfo := typeDesc.InstanceTypes[0]
	vcpus := *typeInfo.VCpuInfo.DefaultVCpus
	memMiB := *typeInfo.MemoryInfo.SizeInMiB
	hasGPU := typeInfo.GpuInfo != nil && len(typeInfo.GpuInfo.Gpus) > 0

	arch := "x86_64"
	if len(typeInfo.ProcessorInfo.SupportedArchitectures) > 0 {
		arch = string(typeInfo.ProcessorInfo.SupportedArchitectures[0])
	}
	currentNetPerf := ""
	if typeInfo.NetworkInfo != nil && typeInfo.NetworkInfo.NetworkPerformance != nil {
		currentNetPerf = *typeInfo.NetworkInfo.NetworkPerformance
	}

	netStr := ""
	if currentNetPerf != "" {
		netStr = ", " + currentNetPerf
	}
	fmt.Printf("  Current specs: %d vCPU, %.0f GiB, %s%s\n", vcpus, float64(memMiB)/1024.0, arch, netStr)

	// 3. Determine search criteria
	minVCPU := int(vcpus) / 2
	if minVCPUFlag > 0 {
		minVCPU = minVCPUFlag
	}
	minMem := float64(memMiB) / 1024.0 / 2.0
	if minMemFlag > 0 {
		minMem = minMemFlag
	}

	defaultMaxPrice := 0.0
	if maxPriceFlag > 0 {
		defaultMaxPrice = maxPriceFlag
	} else if dcfg.DefaultMaxPrice != "" {
		defaultMaxPrice, _ = strconv.ParseFloat(dcfg.DefaultMaxPrice, 64)
	}

	fmt.Printf("\nSearching for alternatives (>=%d vCPU, >=%.0f GiB, %s) in %s...\n",
		minVCPU, minMem, arch, az)

	// 4. Find candidate instance types
	candidates, err := awsutil.FetchInstanceTypes(ctx, client, arch, minVCPU, minMem, hasGPU)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		fmt.Println("No instance types match the given specs.")
		return nil
	}

	// 5. Fetch spot prices filtered to the instance's AZ
	results, err := awsutil.FetchSpotPrices(ctx, client, candidates, az)
	if err != nil {
		return err
	}

	// 6. Apply max price filter
	if defaultMaxPrice > 0 {
		var filtered []awsutil.SpotSearchResult
		for _, r := range results {
			if r.Price <= defaultMaxPrice {
				filtered = append(filtered, r)
			}
		}
		results = filtered
	}

	if len(results) == 0 {
		fmt.Println("No spot capacity found matching filters.")
		return nil
	}

	// 7. Sort by price ascending
	sort.Slice(results, func(i, j int) bool { return results[i].Price < results[j].Price })

	// 8. Display (top 10 by default)
	display := results
	if len(display) > 10 {
		display = display[:10]
	}
	fmt.Printf("Found %d instance types with spot capacity (showing top %d):\n\n", len(results), len(display))
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TYPE\tVCPU\tMEMORY\tNETWORK\tPRICE\tGPU")
	for _, r := range display {
		netPerf := r.NetworkPerformance
		if netPerf == "" {
			netPerf = "-"
		}
		gpuStr := "-"
		if r.GPU {
			gpuStr = "yes"
		}
		fmt.Fprintf(w, "%s\t%d\t%.0f GiB\t%s\t$%.4f\t%s\n",
			r.InstanceType, r.VCPUs, float64(r.MemoryMiB)/1024.0, netPerf, r.Price, gpuStr)
	}
	w.Flush()

	if !autoYes {
		fmt.Printf("\nTo resize: devbox resize %s %s\n", instanceID, results[0].InstanceType)
		return nil
	}

	// 9. Auto-resize to cheapest
	cheapest := results[0].InstanceType
	fmt.Printf("\nAuto-resizing to %s (cheapest at $%.4f)...\n", cheapest, results[0].Price)
	return resizeInstance(ctx, dcfg, client, r53client, instanceID, cheapest)
}
