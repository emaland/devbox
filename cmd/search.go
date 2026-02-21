package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spf13/cobra"

	"github.com/emaland/devbox/internal/awsutil"
)

func newSearchCmd() *cobra.Command {
	var (
		minVCPU  int
		minMem   float64
		maxPrice float64
		arch     string
		gpu      bool
		az       string
		sortBy   string
		limit    int
	)

	cmd := &cobra.Command{
		Use:   "search [instance-type...]",
		Short: "Browse spot prices by hardware specs",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSearch(cmd.Context(), ec2Client, args, minVCPU, minMem, maxPrice, arch, gpu, az, sortBy, limit)
		},
	}

	cmd.Flags().IntVar(&minVCPU, "min-vcpu", 8, "Minimum vCPUs")
	cmd.Flags().Float64Var(&minMem, "min-mem", 16, "Minimum memory (GiB)")
	cmd.Flags().Float64Var(&maxPrice, "max-price", 0, "Max spot price $/hr (0 = no limit)")
	cmd.Flags().StringVar(&arch, "arch", "x86_64", "Architecture (x86_64 or arm64)")
	cmd.Flags().BoolVar(&gpu, "gpu", false, "Require GPU")
	cmd.Flags().StringVar(&az, "az", "", "Filter by availability zone")
	cmd.Flags().StringVar(&sortBy, "sort", "price", "Sort by: price, vcpu, mem")
	cmd.Flags().IntVar(&limit, "limit", 20, "Max rows to display")

	return cmd
}

func runSearch(ctx context.Context, client *ec2.Client, args []string, minVCPU int, minMem, maxPrice float64, arch string, gpu bool, az, sortBy string, limit int) error {
	// If specific instance types were passed as positional args, look those up directly
	var instanceTypes []awsutil.InstanceTypeInfo
	var err error
	if len(args) > 0 {
		fmt.Println("Looking up instance types...")
		var typeNames []types.InstanceType
		for _, arg := range args {
			typeNames = append(typeNames, types.InstanceType(arg))
		}
		instanceTypes, err = awsutil.DescribeSpecificTypes(ctx, client, typeNames)
		if err != nil {
			return err
		}
	} else {
		// Broad search by hardware specs
		fmt.Println("Fetching instance types...")
		instanceTypes, err = awsutil.FetchInstanceTypes(ctx, client, arch, minVCPU, minMem, gpu)
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
	results, err := awsutil.FetchSpotPrices(ctx, client, instanceTypes, az)
	if err != nil {
		return err
	}

	// 3. Apply max price filter
	if maxPrice > 0 {
		var filtered []awsutil.SpotSearchResult
		for _, r := range results {
			if r.Price <= maxPrice {
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
	switch sortBy {
	case "vcpu":
		sort.Slice(results, func(i, j int) bool { return results[i].VCPUs < results[j].VCPUs })
	case "mem":
		sort.Slice(results, func(i, j int) bool { return results[i].MemoryMiB < results[j].MemoryMiB })
	default:
		sort.Slice(results, func(i, j int) bool { return results[i].Price < results[j].Price })
	}

	// 5. Truncate
	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}

	// 6. Display
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "INSTANCE TYPE\tVCPU\tMEMORY\tNETWORK\tAZ\tPRICE\tGPU")
	for _, r := range results {
		gpuStr := "-"
		if r.GPU {
			gpuStr = "yes"
		}
		netPerf := r.NetworkPerformance
		if netPerf == "" {
			netPerf = "-"
		}
		fmt.Fprintf(w, "%s\t%d\t%.0f GiB\t%s\t%s\t$%.4f\t%s\n",
			r.InstanceType, r.VCPUs, float64(r.MemoryMiB)/1024.0, netPerf, r.AZ, r.Price, gpuStr)
	}
	w.Flush()
	return nil
}
