package controlplane

import (
	"context"
	"testing"
	"time"
)

func TestUsageForecastUsesCompleteDaysAndPlanCredit(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	usage := BillingUsage{Start: start, End: start.AddDate(0, 1, 0), AsOf: now, RecordedCents: 750, EventCount: 15}
	for i := 0; i < 15; i++ {
		usage.Days = append(usage.Days, UsageDay{Date: start.AddDate(0, 0, i).Format("2006-01-02"), Cents: 50, Events: 1})
	}
	f, trend := forecastUsage(usage, "pro", true)
	if f == nil || f.DailyCents != 50 || f.UsageCents != 1475 || f.TotalCents != 2000 || !trend {
		t.Fatalf("incorrect forecast/credit: %+v %v", f, trend)
	}
	usage.Days = append(usage.Days, UsageDay{Date: "2026-09-16", Cents: 10000, Events: 1})
	usage.RecordedCents += 10000
	f, _ = forecastUsage(usage, "payg", true)
	if f == nil || f.DailyCents != 50 || f.UsageCents != 11475 {
		t.Fatalf("partial day distorted future daily pace: %+v", f)
	}
	if f, _ = forecastUsage(usage, "payg", false); f != nil {
		t.Fatal("inactive period forecast as bill")
	}
	usage.Days = usage.Days[:1]
	if f, _ = forecastUsage(usage, "payg", true); f != nil {
		t.Fatal("sparse records produced a forecast")
	}
}
func TestUsageAnalyticsIsWorkspaceAndSubscriptionScoped(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	now := s.now().UTC()
	start := now.AddDate(0, 0, -20)
	end := now.AddDate(0, 0, 10)
	_, err := s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,plan_id,status,customer_id,subscription_id) VALUES ($1,'payg','active','cus_fixture','sub_current')`, c.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO billing_usage_events(id,workspace_id,customer_id,subscription_id,amount_cents,resource,rate_version,occurred_at) VALUES ('analytics-current',$1,'cus_fixture','sub_current',123,'app/example','test',$2),('analytics-other-sub',$1,'cus_fixture','sub_old',999,'app/old','test',$2)`, c.WorkspaceID, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	value, err := s.billingUsage(ctx, c.WorkspaceID, "sub_current", BillingState{PlanID: "payg", Status: "active", PeriodStart: &start, PeriodEnd: &end})
	if err != nil || value.RecordedCents != 123 || value.EventCount != 1 || len(value.Resources) != 1 || value.Resources[0].Resource != "app/example" || value.Forecast != nil {
		t.Fatalf("incorrect scoped analytics: %+v %v", value, err)
	}
	empty, err := s.billingUsage(ctx, "nonexistent-workspace", "sub_current", BillingState{PlanID: "payg"})
	if err != nil || empty.EventCount != 0 || empty.RecordedCents != 0 {
		t.Fatalf("usage leaked between workspaces: %+v %v", empty, err)
	}
}
