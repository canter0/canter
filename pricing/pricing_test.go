package pricing

import "testing"

func TestBillingCreditBoundaries(t *testing.T) {
	for _, tc := range []struct {
		plan                string
		usage, extra, total int64
	}{
		{"payg", 0, 0, 0}, {"payg", 2100, 2100, 2100}, {"pro", 0, 0, 2000},
		{"pro", 1999, 0, 2000}, {"pro", 2000, 0, 2000}, {"pro", 2001, 1, 2001},
		{"pro", 2100, 100, 2100}, {"pro", 5000, 3000, 5000},
	} {
		got, err := Calculate(tc.plan, tc.usage)
		if err != nil || got.AdditionalUsageCents != tc.extra || got.TotalCents != tc.total {
			t.Fatalf("%+v: %+v, %v", tc, got, err)
		}
		if got.SubscriptionCents+got.UsageCents-got.CreditAppliedCents != got.TotalCents {
			t.Fatal("credit counted twice")
		}
	}
}
func TestInvalidBillingInput(t *testing.T) {
	for _, tc := range []struct {
		plan  string
		usage int64
	}{{"unknown", 0}, {"pro", -1}, {"payg", 1_000_000_001}} {
		if _, err := Calculate(tc.plan, tc.usage); err == nil {
			t.Fatalf("accepted %+v", tc)
		}
	}
}
