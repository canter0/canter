// Package pricing defines Canter's shared public and billing plan contract.
package pricing

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed catalog.json
var catalogJSON []byte

type Plan struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	MonthlyCents       int64  `json:"monthlyCents"`
	IncludedUsageCents int64  `json:"includedUsageCents"`
}
type Catalog struct {
	Version  string `json:"version"`
	Currency string `json:"currency"`
	Plans    []Plan `json:"plans"`
}
type Bill struct {
	PlanID               string `json:"planId"`
	Currency             string `json:"currency"`
	SubscriptionCents    int64  `json:"subscriptionCents"`
	UsageCents           int64  `json:"usageCents"`
	CreditAppliedCents   int64  `json:"creditAppliedCents"`
	CreditRemainingCents int64  `json:"creditRemainingCents"`
	AdditionalUsageCents int64  `json:"additionalUsageCents"`
	TotalCents           int64  `json:"totalCents"`
}

func Current() Catalog {
	var c Catalog
	if err := json.Unmarshal(catalogJSON, &c); err != nil {
		panic(err)
	}
	return c
}
func Find(id string) (Plan, bool) {
	for _, p := range Current().Plans {
		if p.ID == id {
			return p, true
		}
	}
	return Plan{}, false
}
func Calculate(planID string, usageCents int64) (Bill, error) {
	p, ok := Find(planID)
	if !ok || usageCents < 0 || usageCents > 1_000_000_000 {
		return Bill{}, fmt.Errorf("invalid plan or usage amount")
	}
	credit := min(usageCents, p.IncludedUsageCents)
	return Bill{PlanID: p.ID, Currency: Current().Currency, SubscriptionCents: p.MonthlyCents, UsageCents: usageCents, CreditAppliedCents: credit, CreditRemainingCents: p.IncludedUsageCents - credit, AdditionalUsageCents: usageCents - credit, TotalCents: p.MonthlyCents + usageCents - credit}, nil
}
