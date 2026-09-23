package pricing

import "fmt"

const (
	// ComputeCentsPerUnitPer720Hours is Canter's current published compute usage rate.
	ComputeCentsPerUnitPer720Hours int64 = 300
	HoursPerResourceMonth                = 720
)

// ComputeUsageUnits follows the billing meter: one unit is the larger of the
// allocated vCPU count or the memory allocation rounded up to whole GiB.
func ComputeUsageUnits(vCPU, memoryMiB int) (int64, error) {
	if vCPU < 1 || vCPU > 256 || memoryMiB < 1 || memoryMiB > 1<<20 {
		return 0, fmt.Errorf("compute allocation is outside the supported estimate range")
	}
	memoryGiB := (memoryMiB + 1023) / 1024
	return int64(max(vCPU, memoryGiB)), nil
}

// EstimateComputeMonthlyCents estimates Canter compute usage for one VM over
// 720 hours. Provider charges, workspace credits, and separately metered
// object storage are not included.
func EstimateComputeMonthlyCents(vCPU, memoryMiB int) (int64, error) {
	units, err := ComputeUsageUnits(vCPU, memoryMiB)
	if err != nil {
		return 0, err
	}
	return units * ComputeCentsPerUnitPer720Hours, nil
}
