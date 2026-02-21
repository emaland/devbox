package awsutil

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func FetchInstanceTypes(ctx context.Context, client *ec2.Client, arch string, minVCPU int, minMem float64, requireGPU bool) ([]InstanceTypeInfo, error) {
	var results []InstanceTypeInfo
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

			netPerf := ""
			if it.NetworkInfo != nil && it.NetworkInfo.NetworkPerformance != nil {
				netPerf = *it.NetworkInfo.NetworkPerformance
			}
			results = append(results, InstanceTypeInfo{
				Name:               string(it.InstanceType),
				VCPUs:              vcpus,
				MemoryMiB:          memMiB,
				HasGPU:             hasGPU,
				NetworkPerformance: netPerf,
			})
		}
	}
	return results, nil
}

func DescribeSpecificTypes(ctx context.Context, client *ec2.Client, typeNames []types.InstanceType) ([]InstanceTypeInfo, error) {
	result, err := client.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{
		InstanceTypes: typeNames,
	})
	if err != nil {
		return nil, fmt.Errorf("describing instance types: %w", err)
	}
	var infos []InstanceTypeInfo
	for _, it := range result.InstanceTypes {
		hasGPU := it.GpuInfo != nil && len(it.GpuInfo.Gpus) > 0
		netPerf := ""
		if it.NetworkInfo != nil && it.NetworkInfo.NetworkPerformance != nil {
			netPerf = *it.NetworkInfo.NetworkPerformance
		}
		infos = append(infos, InstanceTypeInfo{
			Name:               string(it.InstanceType),
			VCPUs:              *it.VCpuInfo.DefaultVCpus,
			MemoryMiB:          *it.MemoryInfo.SizeInMiB,
			HasGPU:             hasGPU,
			NetworkPerformance: netPerf,
		})
	}
	return infos, nil
}

func FetchSpotPrices(ctx context.Context, client *ec2.Client, instanceTypes []InstanceTypeInfo, azFilter string) ([]SpotSearchResult, error) {
	// Build lookup map
	infoMap := map[string]InstanceTypeInfo{}
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

	var results []SpotSearchResult
	for k, sp := range latest {
		info := infoMap[k.itype]
		price, _ := strconv.ParseFloat(*sp.SpotPrice, 64)
		results = append(results, SpotSearchResult{
			InstanceType:       k.itype,
			VCPUs:              info.VCPUs,
			MemoryMiB:          info.MemoryMiB,
			AZ:                 k.az,
			Price:              price,
			GPU:                info.HasGPU,
			NetworkPerformance: info.NetworkPerformance,
		})
	}
	return results, nil
}
