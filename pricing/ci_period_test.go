package pricing

import (
	"reflect"
	"testing"
	"time"
)

func TestCIMonthlyChargeSegments(t *testing.T) {
	tests := []struct {
		name    string
		start   string
		from    int64
		through int64
		periods []string
		seconds []int64
	}{
		{"free", "2026-11-30T22:59:58Z", 0, 3600, nil, nil},
		{"crossing", "2026-11-30T22:59:58Z", 0, 3604, []string{"2026-11-01", "2026-12-01"}, []int64{1, 3}},
		{"retry-after-first", "2026-11-30T22:59:58Z", 3601, 3604, []string{"2026-12-01"}, []int64{3}},
		{"timezone", "2026-12-01T01:59:58+03:00", 0, 3604, []string{"2026-11-01", "2026-12-01"}, []int64{1, 3}},
		{"subsecond", "2026-11-30T22:59:58.5Z", 0, 3604, []string{"2026-11-01", "2026-12-01"}, []int64{1, 3}},
		{"midnight-second", "2026-11-30T22:59:59Z", 3600, 3601, []string{"2026-12-01"}, []int64{1}},
		{"free-until-midnight", "2026-11-30T23:00:00Z", 0, 3660, []string{"2026-12-01"}, []int64{60}},
		{"leap-day", "2028-02-29T22:59:58Z", 3600, 3604, []string{"2028-02-01", "2028-03-01"}, []int64{1, 3}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			start, err := time.Parse(time.RFC3339Nano, test.start)
			if err != nil {
				t.Fatal(err)
			}
			got, err := CIChargeSegments("linux-x64-2", start, test.from, test.through)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(test.periods) {
				t.Fatalf("unexpected segments: %+v", got)
			}
			var units int64
			for i, segment := range got {
				if segment.PeriodStart.Format(time.DateOnly) != test.periods[i] || segment.ThroughBillableSeconds-segment.FromBillableSeconds != test.seconds[i] || segment.ChargeUnits != test.seconds[i]*1000 {
					t.Fatalf("segment %d: %+v", i, segment)
				}
				units += segment.ChargeUnits
			}
			before, _ := CalculateCI("linux-x64-2", test.from)
			after, _ := CalculateCI("linux-x64-2", test.through)
			if units != after.ChargeUnits-before.ChargeUnits {
				t.Fatalf("lost charge units: %d", units)
			}
		})
	}
}

func TestCIIncrementalMeterMatchesWholeInterval(t *testing.T) {
	for _, size := range CurrentCI().Sizes {
		start := time.Date(2026, time.November, 30, 22, 59, 58, 123456000, time.UTC)
		whole, err := CIChargeSegments(size.ID, start, 0, 7501)
		if err != nil {
			t.Fatal(err)
		}
		expected := map[string]int64{}
		for _, segment := range whole {
			expected[segment.PeriodStart.Format(time.DateOnly)] += segment.ChargeUnits
		}
		got := map[string]int64{}
		for from := int64(0); from < 7501; from += 17 {
			segments, err := CIChargeSegments(size.ID, start, from, min(from+17, 7501))
			if err != nil {
				t.Fatal(err)
			}
			for _, segment := range segments {
				got[segment.PeriodStart.Format(time.DateOnly)] += segment.ChargeUnits
			}
		}
		if !reflect.DeepEqual(got, expected) {
			t.Fatalf("%s: incremental %v != whole %v", size.ID, got, expected)
		}
	}
}

func TestCIChargeSegmentsRejectInvalidIntervals(t *testing.T) {
	start := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		size    string
		start   time.Time
		from    int64
		through int64
	}{
		{"linux-x64-2", time.Time{}, 0, 1},
		{"linux-x64-2", start, -1, 1},
		{"linux-x64-2", start, 2, 1},
		{"linux-x64-2", start, 0, 366*86400 + 1},
		{"linux-x64-8", start, 0, 3601},
	} {
		if _, err := CIChargeSegments(test.size, test.start, test.from, test.through); err == nil {
			t.Fatalf("accepted invalid interval: %+v", test)
		}
	}
}
