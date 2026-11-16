package pricing

import (
	"encoding/json"
	"strconv"
	"testing"
)

func TestCIFreeHourAndOverage(t *testing.T) {
	tests := []struct {
		size     string
		seconds  int64
		billable int64
		units    int64
		micros   int64
	}{
		{"linux-x64-2", 0, 0, 0, 0},
		{"linux-x64-2", 3599, 0, 0, 0},
		{"linux-x64-2", 3600, 0, 0, 0},
		{"linux-x64-2", 3601, 1, 2000, 33},
		{"linux-x64-2", 3660, 60, 120000, 2000},
		{"linux-x64-2", 7200, 3600, 7200000, 120000},
		{"linux-x64-4", 3600, 0, 0, 0},
		{"linux-x64-4", 3601, 1, 4000, 66},
		{"linux-x64-4", 3660, 60, 240000, 4000},
		{"linux-x64-4", 4200, 600, 2400000, 40000},
		{"linux-x64-4", 7200, 3600, 14400000, 240000},
	}
	for _, test := range tests {
		t.Run(test.size+"/"+strconv.FormatInt(test.seconds, 10), func(t *testing.T) {
			got, err := CalculateCI(test.size, test.seconds)
			if err != nil {
				t.Fatal(err)
			}
			if got.ElapsedSeconds != test.seconds || got.FreeSeconds != min(test.seconds, CIFreeSeconds) || got.BillableSeconds != test.billable || got.ChargeUnits != test.units || got.CostMicros != test.micros || got.CostMicros*60+got.Remainder != got.ChargeUnits {
				t.Fatalf("unexpected usage: %+v", got)
			}
		})
	}
}

func TestCIEveryRunnerGetsFreeHourWithoutMonthlyQuota(t *testing.T) {
	for run := 0; run < 1000; run++ {
		usage, err := CalculateCI("linux-x64-4", 3600)
		if err != nil || usage.ChargeUnits != 0 {
			t.Fatalf("run %d: usage=%+v error=%v", run, usage, err)
		}
	}
	catalog := CurrentCI()
	if catalog.MonthlyMinuteLimit != nil || catalog.FreeSecondsPerRun != 3600 || catalog.MaxVCPUs != 4 {
		t.Fatalf("unexpected free policy: %+v", catalog)
	}
}

func TestCIRatesAreHalfMatchedBlacksmithRate(t *testing.T) {
	catalog := CurrentCI()
	if catalog.Version != CIRateVersion || catalog.Currency != "USD" {
		t.Fatalf("invalid catalog: %+v", catalog)
	}
	for _, size := range catalog.Sizes {
		if size.MicrosPerMinute*2 != size.BlacksmithMicrosPerMinute || size.VCPUs > catalog.MaxVCPUs {
			t.Fatalf("invalid rate: %+v", size)
		}
		byLabel, ok := FindCI(size.Label)
		if !ok || byLabel != size {
			t.Fatalf("label did not resolve: %s", size.Label)
		}
	}
	catalog.Sizes[0].MicrosPerMinute = 1
	if CurrentCI().Sizes[0].MicrosPerMinute != 2000 {
		t.Fatal("catalog mutation escaped the caller")
	}
}

func TestCIFractionalChargesSurviveAggregation(t *testing.T) {
	var total int64
	for run := 0; run < 300; run++ {
		usage, err := CalculateCI("linux-x64-2", 3601)
		if err != nil {
			t.Fatal(err)
		}
		total += usage.ChargeUnits
	}
	cents, remainder, err := CIChargeCents(total)
	if err != nil || cents != 1 || remainder != 0 {
		t.Fatalf("lost fractional charges: cents=%d remainder=%d error=%v", cents, remainder, err)
	}
	cents, remainder, err = CIChargeCents(599999)
	if err != nil || cents != 0 || remainder != 599999 {
		t.Fatalf("rounded a partial cent: %d %d %v", cents, remainder, err)
	}
}

func TestCIRejectsInvalidMetering(t *testing.T) {
	for _, test := range []struct {
		size    string
		seconds int64
	}{
		{"", 3600}, {"linux-x64-8", 3600}, {"linux-arm-2", 3600},
		{"linux-x64-2", -1}, {"linux-x64-2", 366*86400 + 1},
	} {
		if _, err := CalculateCI(test.size, test.seconds); err == nil {
			t.Fatalf("accepted invalid input: %+v", test)
		}
	}
	if _, _, err := CIChargeCents(-1); err == nil {
		t.Fatal("accepted negative charge")
	}
}

func TestCICatalogIncludesExplicitUnlimitedMonthlyMinutes(t *testing.T) {
	data, err := json.Marshal(CurrentCI())
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err = json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	limit, present := out["monthlyMinuteLimit"]
	if !present || limit != nil {
		t.Fatalf("monthly policy is missing or limited: %s", data)
	}
}
