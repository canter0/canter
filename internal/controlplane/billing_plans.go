package controlplane

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type billingSchedule struct {
	ID           string            `json:"id"`
	Status       string            `json:"status"`
	Metadata     map[string]string `json:"metadata"`
	CurrentPhase struct {
		Start int64 `json:"start_date"`
		End   int64 `json:"end_date"`
	} `json:"current_phase"`
	Phases []struct {
		Start    int64             `json:"start_date"`
		Metadata map[string]string `json:"metadata"`
	} `json:"phases"`
}

type scheduledSubscription struct {
	stripeSubscription
	Schedule *billingSchedule `json:"schedule"`
}

func (h *HTTPServer) pendingBillingPlan(ctx context.Context, workspace string) (string, *time.Time, error) {
	var id string
	if err := h.service.Store.pool.QueryRow(ctx, `SELECT COALESCE(subscription_id,'') FROM workspace_billing WHERE workspace_id=$1`, workspace).Scan(&id); err != nil {
		return "", nil, err
	}
	if id == "" {
		return "", nil, nil
	}
	var sub scheduledSubscription
	if err := h.config.Billing.request(ctx, http.MethodGet, "/v1/subscriptions/"+url.PathEscape(id), url.Values{"expand[]": {"schedule"}}, "", &sub); err != nil {
		return "", nil, err
	}
	if sub.Schedule == nil || sub.Schedule.Status != "active" || sub.Schedule.Metadata["canter_workspace_id"] != workspace {
		return "", nil, nil
	}
	for _, phase := range sub.Schedule.Phases {
		if phase.Start > time.Now().Unix() && (phase.Metadata["canter_plan"] == "pro" || phase.Metadata["canter_plan"] == "payg") {
			at := time.Unix(phase.Start, 0)
			return phase.Metadata["canter_plan"], &at, nil
		}
	}
	return "", nil, nil
}

// Schedule at the existing renewal boundary: no mid-period credit reset,
// proration, second subscription, or immediate charge.
func (h *HTTPServer) changeBillingPlan(ctx context.Context, workspace, target string, expectedEnd int64) error {
	b := h.config.Billing
	if err := b.ValidatePrices(ctx); err != nil {
		return err
	}
	if target == "pro" && (b.Config.ProPriceID == "" || b.Config.ProUsagePriceID == "") {
		return fmt.Errorf("%w: Pro is not configured", ErrConflict)
	}
	tx, err := h.service.Store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var customer, id string
	var attempt int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE(customer_id,''),COALESCE(subscription_id,''),checkout_attempt FROM workspace_billing WHERE workspace_id=$1 FOR UPDATE`, workspace).Scan(&customer, &id, &attempt); err != nil {
		return err
	}
	if id == "" {
		return fmt.Errorf("%w: set up billing first", ErrConflict)
	}
	var sub scheduledSubscription
	if err = b.request(ctx, http.MethodGet, "/v1/subscriptions/"+url.PathEscape(id), url.Values{"expand[]": {"schedule"}}, "", &sub); err != nil {
		return err
	}
	current, err := b.subscriptionPlan(sub.stripeSubscription)
	if err != nil {
		return err
	}
	if sub.Customer != customer || sub.Status != "active" || sub.CancelAtPeriodEnd {
		return fmt.Errorf("%w: resolve the current subscription status first", ErrConflict)
	}
	start, end := sub.PeriodStart, sub.PeriodEnd
	if start == 0 && len(sub.Items.Data) > 0 {
		start, end = sub.Items.Data[0].PeriodStart, sub.Items.Data[0].PeriodEnd
	}
	if expectedEnd != end || end <= time.Now().Unix() || start == 0 {
		return fmt.Errorf("%w: billing period changed; refresh and review again", ErrConflict)
	}
	schedule := sub.Schedule
	key := "canter-plan-" + workspace + "-" + strconv.FormatInt(attempt+1, 10)
	if schedule != nil && schedule.Metadata["canter_workspace_id"] != workspace {
		// Recover a create that succeeded before its metadata update timed out.
		var recovered billingSchedule
		if err = b.request(ctx, http.MethodPost, "/v1/subscription_schedules", url.Values{"from_subscription": {id}}, key+"-create", &recovered); err != nil || recovered.ID != schedule.ID {
			return fmt.Errorf("%w: existing billing schedule requires reconciliation", ErrConflict)
		}
	}
	if target == current {
		if schedule != nil {
			if err = b.request(ctx, http.MethodPost, "/v1/subscription_schedules/"+url.PathEscape(schedule.ID)+"/release", nil, key+"-release", nil); err != nil {
				return err
			}
		}
	} else {
		if schedule == nil {
			schedule = &billingSchedule{}
			if err = b.request(ctx, http.MethodPost, "/v1/subscription_schedules", url.Values{"from_subscription": {id}}, key+"-create", schedule); err != nil {
				return err
			}
		}
		if schedule.CurrentPhase.Start != 0 {
			start = schedule.CurrentPhase.Start
		}
		v := url.Values{"end_behavior": {"release"}, "proration_behavior": {"none"}, "metadata[canter_workspace_id]": {workspace}, "phases[0][start_date]": {strconv.FormatInt(start, 10)}, "phases[0][end_date]": {strconv.FormatInt(end, 10)}, "phases[0][proration_behavior]": {"none"}, "phases[0][metadata][canter_plan]": {current}, "phases[1][start_date]": {strconv.FormatInt(end, 10)}, "phases[1][iterations]": {"1"}, "phases[1][proration_behavior]": {"none"}, "phases[1][metadata][canter_plan]": {target}}
		for i, item := range sub.Items.Data {
			prefix := "phases[0][items][" + strconv.Itoa(i) + "]"
			v.Set(prefix+"[price]", item.Price.ID)
			if item.Price.ID == b.Config.ProPriceID {
				v.Set(prefix+"[quantity]", "1")
			}
		}
		if target == "pro" {
			v.Set("phases[1][items][0][price]", b.Config.ProPriceID)
			v.Set("phases[1][items][0][quantity]", "1")
			v.Set("phases[1][items][1][price]", b.Config.ProUsagePriceID)
		} else {
			v.Set("phases[1][items][0][price]", b.Config.PaygPriceID)
		}
		if err = b.request(ctx, http.MethodPost, "/v1/subscription_schedules/"+url.PathEscape(schedule.ID), v, key+"-update-"+target, nil); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE workspace_billing SET checkout_attempt=checkout_attempt+1,updated_at=now() WHERE workspace_id=$1`, workspace); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
