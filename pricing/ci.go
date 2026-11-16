package pricing

import (
	"fmt"
	"time"
)

const (
	CIRateVersion              = "ci-free-hour-2026-11-v1"
	CIFreeSeconds        int64 = 3600
	CIChargeUnitsPerCent       = 600_000
)

type CISize struct {
	ID                        string `json:"id"`
	Label                     string `json:"label"`
	VCPUs                     int    `json:"vcpus"`
	MemoryMiB                 int    `json:"memoryMiB"`
	MicrosPerMinute           int64  `json:"microsPerMinute"`
	BlacksmithMicrosPerMinute int64  `json:"blacksmithMicrosPerMinute"`
}

type CICatalog struct {
	Version            string   `json:"version"`
	Currency           string   `json:"currency"`
	FreeSecondsPerRun  int64    `json:"freeSecondsPerRun"`
	MonthlyMinuteLimit *int64   `json:"monthlyMinuteLimit"`
	MaxVCPUs           int      `json:"maxVcpus"`
	ReferenceURL       string   `json:"referenceUrl"`
	Sizes              []CISize `json:"sizes"`
}

type CIUsage struct {
	ElapsedSeconds  int64 `json:"elapsedSeconds"`
	FreeSeconds     int64 `json:"freeSeconds"`
	BillableSeconds int64 `json:"billableSeconds"`
	ChargeUnits     int64 `json:"chargeUnits"`
	CostMicros      int64 `json:"costMicros"`
	Remainder       int64 `json:"remainder"`
}

type CIChargeSegment struct {
	PeriodStart            time.Time `json:"periodStart"`
	FromBillableSeconds    int64     `json:"fromBillableSeconds"`
	ThroughBillableSeconds int64     `json:"throughBillableSeconds"`
	ChargeUnits            int64     `json:"chargeUnits"`
}

func CurrentCI() CICatalog {
	return CICatalog{
		Version: CIRateVersion, Currency: "USD", FreeSecondsPerRun: CIFreeSeconds,
		MaxVCPUs: 4, ReferenceURL: "https://www.blacksmith.sh/pricing",
		Sizes: []CISize{
			{ID: "linux-x64-2", Label: "canter-2vcpu-ubuntu-2404", VCPUs: 2, MemoryMiB: 8192, MicrosPerMinute: 2000, BlacksmithMicrosPerMinute: 4000},
			{ID: "linux-x64-4", Label: "canter-4vcpu-ubuntu-2404", VCPUs: 4, MemoryMiB: 16384, MicrosPerMinute: 4000, BlacksmithMicrosPerMinute: 8000},
		},
	}
}

func FindCI(id string) (CISize, bool) {
	for _, size := range CurrentCI().Sizes {
		if size.ID == id || size.Label == id {
			return size, true
		}
	}
	return CISize{}, false
}

func CalculateCI(sizeID string, elapsedSeconds int64) (CIUsage, error) {
	size, ok := FindCI(sizeID)
	if !ok || elapsedSeconds < 0 || elapsedSeconds > 366*86400 {
		return CIUsage{}, fmt.Errorf("invalid CI size or elapsed runtime")
	}
	free := min(elapsedSeconds, CIFreeSeconds)
	billable := elapsedSeconds - free
	units := billable * size.MicrosPerMinute
	return CIUsage{
		ElapsedSeconds: elapsedSeconds, FreeSeconds: free, BillableSeconds: billable,
		ChargeUnits: units, CostMicros: units / 60, Remainder: units % 60,
	}, nil
}

func CIChargeCents(chargeUnits int64) (cents, remainder int64, err error) {
	if chargeUnits < 0 {
		return 0, 0, fmt.Errorf("invalid CI charge units")
	}
	return chargeUnits / CIChargeUnitsPerCent, chargeUnits % CIChargeUnitsPerCent, nil
}

func CIChargeSegments(sizeID string, executionStartedAt time.Time, fromElapsedSeconds, toElapsedSeconds int64) ([]CIChargeSegment, error) {
	size, ok := FindCI(sizeID)
	if !ok || executionStartedAt.IsZero() || executionStartedAt.Year() < 1970 || executionStartedAt.Year() > 9998 || fromElapsedSeconds < 0 || fromElapsedSeconds > toElapsedSeconds || toElapsedSeconds > 366*86400 {
		return nil, fmt.Errorf("invalid CI metering interval")
	}
	out := []CIChargeSegment{}
	from := max(int64(0), fromElapsedSeconds-CIFreeSeconds)
	through := max(int64(0), toElapsedSeconds-CIFreeSeconds)
	base := executionStartedAt.UTC().Add(time.Duration(CIFreeSeconds) * time.Second)
	for from < through {
		first := base.Add(time.Duration(from+1) * time.Second)
		period := time.Date(first.Year(), first.Month(), 1, 0, 0, 0, 0, time.UTC)
		boundary := period.AddDate(0, 1, 0)
		last := min(through, int64((boundary.Sub(base)-time.Nanosecond)/time.Second))
		if last <= from {
			return nil, fmt.Errorf("invalid CI metering boundary")
		}
		out = append(out, CIChargeSegment{PeriodStart: period, FromBillableSeconds: from, ThroughBillableSeconds: last, ChargeUnits: (last - from) * size.MicrosPerMinute})
		from = last
	}
	return out, nil
}
