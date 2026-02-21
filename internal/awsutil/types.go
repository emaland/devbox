package awsutil

type InstanceTypeInfo struct {
	Name               string
	VCPUs              int32
	MemoryMiB          int64
	HasGPU             bool
	NetworkPerformance string
}

type SpotSearchResult struct {
	InstanceType       string
	VCPUs              int32
	MemoryMiB          int64
	AZ                 string
	Price              float64
	GPU                bool
	NetworkPerformance string
}
