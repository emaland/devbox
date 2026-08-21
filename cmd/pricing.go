package cmd

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"
)

// regionLocationNames maps EC2 region codes to the "location" attribute name
// used by the AWS Price List API (GetProducts). The Pricing API only exists
// in us-east-1 / ap-south-1 and has no notion of region codes, so this
// mapping is required. Covers the commercial regions devbox is likely to run
// or move volumes into; extend as needed.
var regionLocationNames = map[string]string{
	"us-east-1":      "US East (N. Virginia)",
	"us-east-2":      "US East (Ohio)",
	"us-west-1":      "US West (N. California)",
	"us-west-2":      "US West (Oregon)",
	"af-south-1":     "Africa (Cape Town)",
	"ap-east-1":      "Asia Pacific (Hong Kong)",
	"ap-south-1":     "Asia Pacific (Mumbai)",
	"ap-south-2":     "Asia Pacific (Hyderabad)",
	"ap-northeast-1": "Asia Pacific (Tokyo)",
	"ap-northeast-2": "Asia Pacific (Seoul)",
	"ap-northeast-3": "Asia Pacific (Osaka)",
	"ap-southeast-1": "Asia Pacific (Singapore)",
	"ap-southeast-2": "Asia Pacific (Sydney)",
	"ap-southeast-3": "Asia Pacific (Jakarta)",
	"ap-southeast-4": "Asia Pacific (Melbourne)",
	"ca-central-1":   "Canada (Central)",
	"eu-central-1":   "EU (Frankfurt)",
	"eu-central-2":   "EU (Zurich)",
	"eu-west-1":      "EU (Ireland)",
	"eu-west-2":      "EU (London)",
	"eu-west-3":      "EU (Paris)",
	"eu-north-1":     "EU (Stockholm)",
	"eu-south-1":     "EU (Milan)",
	"eu-south-2":     "EU (Spain)",
	"me-south-1":     "Middle East (Bahrain)",
	"me-central-1":   "Middle East (UAE)",
	"sa-east-1":      "South America (Sao Paulo)",
}

// onDemandPrice returns the Linux/UNIX on-demand $/hr price for an instance
// type in the region implied by az (e.g. "us-east-1a" -> "us-east-1"),
// caching results per instance type for the duration of the process. It is
// best-effort: returns "" (no error surfaced to the caller as fatal) when
// the price can't be determined.
func onDemandPrice(ctx context.Context, cache onDemandPriceCache, instanceType, az string) (string, error) {
	if instanceType == "" || az == "" {
		return "", nil
	}
	if p, ok := cache[instanceType]; ok {
		return p, nil
	}

	region := az
	if len(az) > 0 {
		region = az[:len(az)-1]
	}
	location, ok := regionLocationNames[region]
	if !ok {
		return "", nil
	}

	// The Pricing API is only available in us-east-1 and ap-south-1,
	// regardless of which region's prices are being queried.
	pricingCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1"))
	if err != nil {
		return "", err
	}
	client := pricing.NewFromConfig(pricingCfg)

	out, err := client.GetProducts(ctx, &pricing.GetProductsInput{
		ServiceCode: aws.String("AmazonEC2"),
		Filters: []pricingtypes.Filter{
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("instanceType"), Value: aws.String(instanceType)},
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("location"), Value: aws.String(location)},
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("operatingSystem"), Value: aws.String("Linux")},
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("tenancy"), Value: aws.String("Shared")},
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("preInstalledSw"), Value: aws.String("NA")},
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("capacitystatus"), Value: aws.String("Used")},
		},
		MaxResults: aws.Int32(1),
	})
	if err != nil {
		cache[instanceType] = ""
		return "", err
	}
	price := extractOnDemandUSD(out.PriceList)
	cache[instanceType] = price
	return price, nil
}

// extractOnDemandUSD digs the USD price-per-hour out of a Price List API
// product JSON blob.
func extractOnDemandUSD(priceList []string) string {
	if len(priceList) == 0 {
		return ""
	}
	var product struct {
		Terms struct {
			OnDemand map[string]struct {
				PriceDimensions map[string]struct {
					PricePerUnit map[string]string `json:"pricePerUnit"`
				} `json:"priceDimensions"`
			} `json:"OnDemand"`
		} `json:"terms"`
	}
	if err := json.Unmarshal([]byte(priceList[0]), &product); err != nil {
		return ""
	}
	for _, term := range product.Terms.OnDemand {
		for _, dim := range term.PriceDimensions {
			if usd, ok := dim.PricePerUnit["USD"]; ok && usd != "" && usd != "0.0000000000" {
				return strings.TrimRight(strings.TrimRight(usd, "0"), ".")
			}
		}
	}
	return ""
}

