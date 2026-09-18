package controlplane

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/canter0/canter/pricing"
)

type UsageDay struct {
	Date   string `json:"date"`
	Cents  int64  `json:"cents"`
	Events int    `json:"events"`
}
type UsageResource struct {
	Resource      string `json:"resource"`
	Cents         int64  `json:"cents"`
	RecentCents   int64  `json:"recentCents"`
	PreviousCents int64  `json:"previousCents"`
}
type UsageCapacity struct {
	Name      string `json:"name"`
	Hosts     int    `json:"hosts"`
	MemoryMiB int    `json:"memoryMiB"`
	VCPU      int    `json:"vcpu"`
	Instances int    `json:"instances"`
}
type UsageForecast struct {
	UsageCents    int64   `json:"usageCents"`
	TotalCents    int64   `json:"totalCents"`
	DailyCents    float64 `json:"dailyCents"`
	SampleDays    int     `json:"sampleDays"`
	RemainingDays float64 `json:"remainingDays"`
}
type BillingUsage struct {
	Start          time.Time       `json:"start"`
	End            time.Time       `json:"end"`
	AsOf           time.Time       `json:"asOf"`
	Days           []UsageDay      `json:"days"`
	Resources      []UsageResource `json:"resources"`
	Capacity       []UsageCapacity `json:"capacity"`
	RecordedCents  int64           `json:"recordedCents"`
	EventCount     int             `json:"eventCount"`
	LastRecordedAt *time.Time      `json:"lastRecordedAt"`
	Forecast       *UsageForecast  `json:"forecast"`
	TrendReady     bool            `json:"trendReady"`
	RecentCents    int64           `json:"recentCents"`
	PreviousCents  int64           `json:"previousCents"`
}

func (s *Store) billingUsage(ctx context.Context, workspace, subscription string, state BillingState) (BillingUsage, error) {
	now := s.now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	if state.PeriodStart != nil && state.PeriodEnd != nil && state.PeriodEnd.After(*state.PeriodStart) && state.PeriodEnd.Sub(*state.PeriodStart) <= 366*24*time.Hour {
		start = state.PeriodStart.UTC()
		end = state.PeriodEnd.UTC()
	}
	out := BillingUsage{Start: start, End: end, AsOf: now, Days: []UsageDay{}, Resources: []UsageResource{}, Capacity: []UsageCapacity{}}
	today := now.Truncate(24 * time.Hour)
	recent := today.AddDate(0, 0, -7)
	previous := today.AddDate(0, 0, -14)
	rows, err := s.pool.Query(ctx, `SELECT resource,to_char(occurred_at AT TIME ZONE 'UTC','YYYY-MM-DD'),SUM(amount_cents),COUNT(*),MAX(occurred_at) FROM billing_usage_events WHERE workspace_id=$1 AND occurred_at>=$2 AND occurred_at<$3 AND occurred_at<=$4 AND ($5='' OR subscription_id=$5) GROUP BY resource,2 ORDER BY 2,resource`, workspace, start, end, now, subscription)
	if err != nil {
		return out, err
	}
	days := map[string]UsageDay{}
	resources := map[string]UsageResource{}
	for rows.Next() {
		var resource, date string
		var cents int64
		var count int
		var last time.Time
		if err = rows.Scan(&resource, &date, &cents, &count, &last); err != nil {
			rows.Close()
			return out, err
		}
		day := days[date]
		day.Date = date
		day.Cents += cents
		day.Events += count
		days[date] = day
		item := resources[resource]
		item.Resource = resource
		item.Cents += cents
		when, _ := time.Parse("2006-01-02", date)
		if !when.Before(recent) && when.Before(today) {
			item.RecentCents += cents
			out.RecentCents += cents
		} else if !when.Before(previous) && when.Before(recent) {
			item.PreviousCents += cents
			out.PreviousCents += cents
		}
		resources[resource] = item
		out.RecordedCents += cents
		out.EventCount += count
		if out.LastRecordedAt == nil || last.After(*out.LastRecordedAt) {
			value := last
			out.LastRecordedAt = &value
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	for day := start.Truncate(24 * time.Hour); !day.After(today) && day.Before(end); day = day.AddDate(0, 0, 1) {
		key := day.Format("2006-01-02")
		value := days[key]
		value.Date = key
		out.Days = append(out.Days, value)
	}
	for _, value := range resources {
		out.Resources = append(out.Resources, value)
	}
	sort.Slice(out.Resources, func(i, j int) bool {
		if out.Resources[i].Cents == out.Resources[j].Cents {
			return out.Resources[i].Resource < out.Resources[j].Resource
		}
		return out.Resources[i].Cents > out.Resources[j].Cents
	})
	systems, err := s.ListSystems(ctx, workspace)
	if err != nil {
		return out, err
	}
	for _, system := range systems {
		spec := system.Contract.Spec
		item := UsageCapacity{Name: system.Contract.Metadata.Name, Hosts: spec.Constraints.Host.Count, MemoryMiB: spec.Constraints.Host.MemoryMiB * spec.Constraints.Host.Count}
		for _, service := range spec.Services {
			item.VCPU += service.Resources.VCPU * service.Instances
			item.Instances += service.Instances
		}
		out.Capacity = append(out.Capacity, item)
	}
	out.Forecast, out.TrendReady = forecastUsage(out, state.PlanID, state.PeriodEnd != nil && (state.Status == "active" || state.Status == "trialing"))
	return out, nil
}
func forecastUsage(usage BillingUsage, plan string, active bool) (*UsageForecast, bool) {
	today := usage.AsOf.UTC().Truncate(24 * time.Hour)
	fullStart := usage.Start.UTC().Truncate(24 * time.Hour)
	if fullStart.Before(usage.Start) {
		fullStart = fullStart.AddDate(0, 0, 1)
	}
	elapsed := int(today.Sub(fullStart).Hours() / 24)
	trend := elapsed >= 14 && usage.EventCount > 0
	window := min(elapsed, 7)
	if !active || window < 3 || !usage.End.After(usage.AsOf) {
		return nil, trend
	}
	cutoff := today.AddDate(0, 0, -window)
	var total int64
	sampleDays := 0
	for _, day := range usage.Days {
		date, err := time.Parse("2006-01-02", day.Date)
		if err == nil && !date.Before(cutoff) && date.Before(today) {
			total += day.Cents
			if day.Events > 0 {
				sampleDays++
			}
		}
	}
	if sampleDays < 3 {
		return nil, trend
	}
	average := float64(total) / float64(window)
	remaining := usage.End.Sub(usage.AsOf).Hours() / 24
	projected := int64(math.Round(float64(usage.RecordedCents) + average*remaining))
	if projected < 0 || projected > 1_000_000_000 {
		return nil, trend
	}
	bill, err := pricing.Calculate(plan, projected)
	if err != nil {
		return nil, trend
	}
	return &UsageForecast{UsageCents: projected, TotalCents: bill.TotalCents, DailyCents: average, SampleDays: window, RemainingDays: remaining}, trend
}
