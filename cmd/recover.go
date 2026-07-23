package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
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
		maxVCPUFlag int
		minMemFlag  float64
		maxPrice    float64
		autoYes     bool
		sortBy      string
	)

	cmd := &cobra.Command{
		Use:   "recover [instance-id]",
		Short: "Find alternative instance types with spot capacity in the same AZ",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			instanceID := ""
			if len(args) == 1 {
				instanceID = args[0]
			} else {
				id, err := autoDetectInstance(cmd.Context(), ec2Client, "stopped")
				if err != nil {
					return err
				}
				instanceID = id
			}
			r53client := route53.NewFromConfig(awsCfg)
			return recoverInstance(cmd.Context(), dcfg, ec2Client, r53client, instanceID, minVCPUFlag, maxVCPUFlag, minMemFlag, maxPrice, autoYes, sortBy)
		},
	}

	cmd.Flags().IntVar(&minVCPUFlag, "min-vcpu", 0, "Minimum vCPUs (default: 50% of current)")
	cmd.Flags().IntVar(&maxVCPUFlag, "max-vcpu", 0, "Maximum vCPUs (default: 4x current)")
	cmd.Flags().Float64Var(&minMemFlag, "min-mem", 0, "Minimum memory in GiB (default: 50% of current)")
	cmd.Flags().Float64Var(&maxPrice, "max-price", 0, "Max spot price $/hr (0 = use config default)")
	cmd.Flags().BoolVar(&autoYes, "yes", false, "Auto-pick best candidate and resize")
	cmd.Flags().StringVar(&sortBy, "sort", "efficiency", "Sort by: efficiency, price, vcpu, mem")

	return cmd
}

func recoverInstance(ctx context.Context, dcfg config.DevboxConfig, client *ec2.Client, r53client *route53.Client, instanceID string, minVCPUFlag, maxVCPUFlag int, minMemFlag, maxPriceFlag float64, autoYes bool, sortBy string) error {
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

	maxVCPU := int(vcpus) * 4
	if maxVCPUFlag > 0 {
		maxVCPU = maxVCPUFlag
	}

	fmt.Printf("\nSearching for alternatives (%d-%d vCPU, >=%.0f GiB, %s) in %s...\n",
		minVCPU, maxVCPU, minMem, arch, az)

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

	// 6. Filter: max price, max vCPU, and exclude accelerator families
	//    (inf, trn, dl, p, g, f, vt) unless the current instance has a GPU.
	{
		var filtered []awsutil.SpotSearchResult
		for _, r := range results {
			if defaultMaxPrice > 0 && r.Price > defaultMaxPrice {
				continue
			}
			if int(r.VCPUs) > maxVCPU {
				continue
			}
			if !hasGPU && isAcceleratorFamily(r.InstanceType) {
				continue
			}
			filtered = append(filtered, r)
		}
		results = filtered
	}

	if len(results) == 0 {
		fmt.Println("No spot capacity found matching filters.")
		return nil
	}

	// 7. Sort
	switch sortBy {
	case "price":
		sort.Slice(results, func(i, j int) bool { return results[i].Price < results[j].Price })
	case "vcpu":
		sort.Slice(results, func(i, j int) bool { return results[i].VCPUs > results[j].VCPUs })
	case "mem":
		sort.Slice(results, func(i, j int) bool { return results[i].MemoryMiB > results[j].MemoryMiB })
	default: // efficiency
		sort.Slice(results, func(i, j int) bool { return results[i].EfficiencyScore < results[j].EfficiencyScore })
	}

	// 8. Display
	display := results
	if len(display) > 15 {
		display = display[:15]
	}
	fmt.Printf("Found %d candidates (showing top %d, sorted by %s):\n\n", len(results), len(display), sortBy)
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "#\tTYPE\tVCPU\tMEMORY\t$/DAY\t$/vCPU\t$/GiB\tNETWORK\tGPU")
	for i, r := range display {
		netPerf := r.NetworkPerformance
		if netPerf == "" {
			netPerf = "-"
		}
		gpuStr := "-"
		if r.GPU {
			gpuStr = "yes"
		}
		fmt.Fprintf(w, "%d\t%s\t%d\t%.0f GiB\t$%.2f\t$%.4f\t$%.4f\t%s\t%s\n",
			i+1, r.InstanceType, r.VCPUs, float64(r.MemoryMiB)/1024.0, r.Price*24,
			r.PricePerVCPU, r.PricePerGiB, netPerf, gpuStr)
	}
	w.Flush()

	if autoYes {
		fmt.Println("\nAuto-resizing — trying candidates in order until one has spot capacity...")
		return tryResizeCandidates(ctx, dcfg, client, r53client, instanceID, display)
	}

	// 9. Interactive selection
	fmt.Printf("\nSelect a candidate to resize to (1-%d), or press Enter to skip: ", len(display))
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	choice, err := strconv.Atoi(line)
	if err != nil || choice < 1 || choice > len(display) {
		return fmt.Errorf("invalid selection: %s", line)
	}
	return tryResizeCandidates(ctx, dcfg, client, r53client, instanceID, display[choice-1:])
}

// tryResizeCandidates attempts to resize into each candidate in order,
// automatically skipping ones that fail with InsufficientInstanceCapacity —
// AWS has no API to check Spot capacity ahead of time, so a real launch
// attempt is the only ground-truth signal — and stopping at the first that
// succeeds. Safe to chain: a failed launch always leaves the old instance,
// its spot request, and volumes intact (see resizeSpotInstance).
func tryResizeCandidates(ctx context.Context, dcfg config.DevboxConfig, client *ec2.Client, r53client *route53.Client, instanceID string, candidates []awsutil.SpotSearchResult) error {
	for i, c := range candidates {
		fmt.Printf("Trying %s ($%.2f/day)...\n", c.InstanceType, c.Price*24)
		err := resizeInstance(ctx, dcfg, client, r53client, instanceID, c.InstanceType)
		if err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "InsufficientInstanceCapacity") {
			return err
		}
		fmt.Printf("  no spot capacity for %s right now.\n", c.InstanceType)
		if i < len(candidates)-1 {
			fmt.Println("  trying the next candidate...")
		}
	}
	return fmt.Errorf("no spot capacity available for any of the %d candidates tried", len(candidates))
}

// isAcceleratorFamily returns true for instance families designed for
// ML inference/training or FPGAs (inf, trn, dl, p, g, f, vt).
func isAcceleratorFamily(instanceType string) bool {
	prefixes := []string{"inf", "trn", "dl", "p2", "p3", "p4", "p5", "g3", "g4", "g5", "g6", "f1", "vt"}
	for _, p := range prefixes {
		if strings.HasPrefix(instanceType, p) {
			return true
		}
	}
	return false
}
