package controlplane

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/canter0/canter/pricing"
	"github.com/jackc/pgx/v5"
)

type BillingState struct {
	PendingPlanID        string                `json:"pendingPlanId,omitempty"`
	PendingPlanAt        *time.Time            `json:"pendingPlanAt,omitempty"`
	PaymentMethod        *BillingPaymentMethod `json:"paymentMethod"`
	PaymentReady         bool                  `json:"paymentReady"`
	Usage                BillingUsage          `json:"usage"`
	HasBillingAccount    bool                  `json:"hasBillingAccount"`
	PlanID               string                `json:"planId"`
	Status               string                `json:"status"`
	CheckoutEnabled      bool                  `json:"checkoutEnabled"`
	PeriodStart          *time.Time            `json:"periodStart"`
	PeriodEnd            *time.Time            `json:"periodEnd"`
	CancelAtPeriodEnd    bool                  `json:"cancelAtPeriodEnd"`
	Bill                 pricing.Bill          `json:"bill"`
	PendingEvents        int                   `json:"pendingEvents"`
	ReconciliationEvents int                   `json:"reconciliationEvents"`
}

func (s *Store) billingState(ctx context.Context, workspace string) (BillingState, error) {
	out := BillingState{PlanID: "payg", Status: "not_started"}
	var subscription string
	err := s.pool.QueryRow(ctx, `SELECT plan_id,status,COALESCE(subscription_id,''),period_start,period_end,cancel_at_period_end,customer_id IS NOT NULL FROM workspace_billing WHERE workspace_id=$1`, workspace).Scan(&out.PlanID, &out.Status, &subscription, &out.PeriodStart, &out.PeriodEnd, &out.CancelAtPeriodEnd, &out.HasBillingAccount)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	var usage int64
	if out.PeriodStart != nil && out.PeriodEnd != nil {
		err = s.pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount_cents),0),COUNT(*) FILTER(WHERE sent_at IS NULL),COUNT(*) FILTER(WHERE needs_reconciliation) FROM billing_usage_events WHERE workspace_id=$1 AND subscription_id=$2 AND occurred_at>=$3 AND occurred_at<$4`, workspace, subscription, out.PeriodStart, out.PeriodEnd).Scan(&usage, &out.PendingEvents, &out.ReconciliationEvents)
		if err != nil {
			return out, err
		}
	}
	out.Bill, err = pricing.Calculate(out.PlanID, usage)
	if err != nil {
		return out, err
	}
	out.Usage, err = s.billingUsage(ctx, workspace, subscription, out)
	return out, err
}

func (h *HTTPServer) billingPublic(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) == 1 && parts[0] == "plans" && r.Method == http.MethodGet {
		catalog := pricing.Current()
		writeJSON(w, http.StatusOK, map[string]any{"catalog": catalog, "checkoutEnabled": h.config.Billing.Ready()})
		return
	}
	if len(parts) == 1 && parts[0] == "webhook" && r.Method == http.MethodPost {
		h.billingWebhook(w, r)
		return
	}
	if len(parts) == 1 && parts[0] == "usage" && r.Method == http.MethodPost {
		b := h.config.Billing
		if !b.Ready() {
			writeError(w, http.StatusServiceUnavailable, fmt.Errorf("billing is not enabled"))
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), []byte(b.Config.IngestToken)) != 1 {
			writeStoreError(w, ErrUnauthorized)
			return
		}
		var event BillingUsageEvent
		if !decodeLimit(w, r, &event, 16<<10) {
			return
		}
		if err := h.service.Store.RecordBillingUsage(r.Context(), event); err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued", "id": event.ID})
		return
	}
	writeError(w, http.StatusNotFound, ErrNotFound)
}

func (h *HTTPServer) workspaceBilling(w http.ResponseWriter, r *http.Request, p Principal, workspace string, parts []string) {
	if len(parts) == 0 && r.Method == http.MethodGet {
		state, err := h.service.Store.billingState(r.Context(), workspace)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		state.CheckoutEnabled = h.config.Billing.Ready()
		if state.CheckoutEnabled && state.HasBillingAccount {
			state.PaymentMethod, state.PaymentReady, err = h.service.Store.billingPaymentReadiness(r.Context(), workspace, h.config.Billing)
			if err != nil {
				writeStoreError(w, err)
				return
			}
		}
		if state.CheckoutEnabled && state.Status == "active" {
			state.PendingPlanID, state.PendingPlanAt, err = h.pendingBillingPlan(r.Context(), workspace)
			if err != nil {
				writeStoreError(w, err)
				return
			}
		}
		writeJSON(w, http.StatusOK, state)
		return
	}
	// Payment authority is always human-owned, even if a request also supplies
	// a bearer header. Enforce Origin here as well as at the general boundary.
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if p.Account == nil || p.Installation != nil || r.Header.Get("Authorization") != "" || !h.trustedHumanOrigin(r) {
		writeStoreError(w, ErrForbidden)
		return
	}
	role, err := h.service.Store.Membership(r.Context(), p.Account.ID, workspace)
	if err != nil || role != "owner" {
		writeStoreError(w, ErrForbidden)
		return
	}
	if !h.config.Billing.Ready() {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("Payments are not open yet. No subscription has been started."))
		return
	}
	if len(parts) == 1 && parts[0] == "checkout" {
		var request struct {
			PlanID string `json:"planId"`
		}
		if !decodeLimit(w, r, &request, 4096) {
			return
		}
		if request.PlanID == "" {
			request.PlanID = "payg"
		}
		if _, ok := pricing.Find(request.PlanID); !ok {
			writeError(w, http.StatusBadRequest, fmt.Errorf("unknown billing plan"))
			return
		}
		address, err := h.startBillingCheckout(r.Context(), workspace, p.Account.Email, request.PlanID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"url": address})
		return
	}
	if len(parts) == 1 && parts[0] == "plan-change" {
		var input struct {
			PlanID    string `json:"planId"`
			PeriodEnd int64  `json:"periodEnd"`
		}
		if !decodeLimit(w, r, &input, 4096) {
			return
		}
		if _, ok := pricing.Find(input.PlanID); !ok {
			writeError(w, http.StatusBadRequest, fmt.Errorf("unknown billing plan"))
			return
		}
		if err := h.changeBillingPlan(r.Context(), workspace, input.PlanID, input.PeriodEnd); err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"scheduled": true})
		return
	}
	if len(parts) == 1 && parts[0] == "portal" {
		var customer string
		err := h.service.Store.pool.QueryRow(r.Context(), `SELECT customer_id FROM workspace_billing WHERE workspace_id=$1 AND customer_id IS NOT NULL`, workspace).Scan(&customer)
		if err != nil {
			writeStoreError(w, ErrNotFound)
			return
		}
		var result struct {
			URL string `json:"url"`
		}
		err = h.config.Billing.request(r.Context(), http.MethodPost, "/v1/billing_portal/sessions", url.Values{"customer": {customer}, "configuration": {h.config.Billing.Config.PortalConfigurationID}, "return_url": {strings.TrimRight(h.config.PublicURL, "/") + "/app/billing"}}, "", &result)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	writeError(w, http.StatusNotFound, ErrNotFound)
}

func (h *HTTPServer) startBillingCheckout(ctx context.Context, workspace, email, plan string) (string, error) {
	b := h.config.Billing
	if err := b.ValidatePrices(ctx); err != nil {
		return "", err
	}
	tx, err := h.service.Store.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO workspace_billing(workspace_id) VALUES($1) ON CONFLICT DO NOTHING`, workspace); err != nil {
		return "", err
	}
	var customer, checkoutID, checkoutURL, checkoutPlan string
	var expires *time.Time
	var attempt int64
	err = tx.QueryRow(ctx, `SELECT COALESCE(customer_id,''),checkout_id,checkout_url,checkout_plan,checkout_expires_at,checkout_attempt FROM workspace_billing WHERE workspace_id=$1 FOR UPDATE`, workspace).Scan(&customer, &checkoutID, &checkoutURL, &checkoutPlan, &expires, &attempt)
	if err != nil {
		return "", err
	}
	// Confirm against Stripe too: a completed Checkout can precede its webhook.
	if customer != "" {
		var list struct {
			Data    []stripeSubscription `json:"data"`
			HasMore bool                 `json:"has_more"`
		}
		if err = b.request(ctx, http.MethodGet, "/v1/subscriptions", url.Values{"customer": {customer}, "status": {"all"}, "limit": {"100"}}, "", &list); err != nil {
			return "", err
		}
		if list.HasMore {
			return "", fmt.Errorf("%w: billing history needs reconciliation", ErrConflict)
		}
		for _, sub := range list.Data {
			if sub.Status != "canceled" && sub.Status != "incomplete_expired" {
				return "", fmt.Errorf("%w: this workspace already has billing; use Manage billing", ErrConflict)
			}
		}
	}
	if checkoutURL != "" && expires != nil && expires.After(time.Now()) {
		if checkoutPlan == plan {
			return checkoutURL, tx.Commit(ctx)
		}
		if err = b.request(ctx, http.MethodPost, "/v1/checkout/sessions/"+url.PathEscape(checkoutID)+"/expire", nil, "", nil); err != nil {
			return "", err
		}
	}
	if customer == "" {
		var result struct {
			ID string `json:"id"`
		}
		err = b.request(ctx, http.MethodPost, "/v1/customers", url.Values{"email": {email}, "metadata[canter_workspace_id]": {workspace}}, "canter-customer-"+workspace, &result)
		if err != nil {
			return "", err
		}
		customer = result.ID
		if customer == "" {
			return "", fmt.Errorf("payment provider returned an incomplete customer")
		}
		// Link the customer durably before creating anything that can take a
		// payment. A Checkout timeout must not roll this association back.
		if _, err = tx.Exec(ctx, `UPDATE workspace_billing SET customer_id=$2,updated_at=now() WHERE workspace_id=$1`, workspace, customer); err != nil {
			return "", err
		}
		if err = tx.Commit(ctx); err != nil {
			return "", err
		}
		return h.startBillingCheckout(ctx, workspace, email, plan)
	}
	attempt++
	// Keep provider parameters identical when retrying after an uncertain result.
	// In particular, a wall-clock expires_at would break Stripe idempotency.
	values := url.Values{"mode": {"subscription"}, "customer": {customer}, "client_reference_id": {workspace}, "payment_method_collection": {"always"}, "payment_method_types[]": {"card"}, "success_url": {strings.TrimRight(h.config.PublicURL, "/") + "/app/billing?checkout=complete"}, "cancel_url": {strings.TrimRight(h.config.PublicURL, "/") + "/app/billing?checkout=canceled"}, "subscription_data[metadata][canter_workspace_id]": {workspace}, "subscription_data[metadata][canter_plan]": {plan}}
	if plan == "pro" {
		values.Set("line_items[0][price]", b.Config.ProPriceID)
		values.Set("line_items[0][quantity]", "1")
		values.Set("line_items[1][price]", b.Config.ProUsagePriceID)
	} else {
		values.Set("line_items[0][price]", b.Config.PaygPriceID)
	}
	var result struct {
		ID      string `json:"id"`
		URL     string `json:"url"`
		Expires int64  `json:"expires_at"`
	}
	if err = b.request(ctx, http.MethodPost, "/v1/checkout/sessions", values, "canter-checkout-"+workspace+"-"+plan+"-"+strconv.FormatInt(attempt, 10), &result); err != nil {
		return "", err
	}
	if result.ID == "" || result.URL == "" || result.Expires == 0 {
		return "", fmt.Errorf("payment provider returned an incomplete checkout")
	}
	_, err = tx.Exec(ctx, `UPDATE workspace_billing SET customer_id=$2,checkout_id=$3,checkout_url=$4,checkout_plan=$5,checkout_expires_at=$6,checkout_attempt=$7,updated_at=now() WHERE workspace_id=$1`, workspace, customer, result.ID, result.URL, plan, time.Unix(result.Expires, 0), attempt)
	if err != nil {
		return "", err
	}
	return result.URL, tx.Commit(ctx)
}

func (h *HTTPServer) billingWebhook(w http.ResponseWriter, r *http.Request) {
	b := h.config.Billing
	if b == nil || b.Config.WebhookSecret == "" {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("billing webhook not configured"))
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid webhook body"))
		return
	}
	if !verifyBillingSignature(raw, r.Header.Get("Stripe-Signature"), b.Config.WebhookSecret, time.Now()) {
		writeStoreError(w, ErrUnauthorized)
		return
	}
	var event struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Data struct {
			Object struct {
				Customer string `json:"customer"`
			} `json:"object"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &event) != nil || event.ID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid webhook event"))
		return
	}
	switch event.Type {
	case "checkout.session.completed", "customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted", "invoice.paid", "invoice.payment_failed":
	default:
		writeJSON(w, http.StatusOK, map[string]bool{"received": true})
		return
	}
	if event.Data.Object.Customer == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("missing billing customer"))
		return
	}
	tx, err := h.service.Store.pool.Begin(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	result, err := tx.Exec(r.Context(), `INSERT INTO billing_webhook_events(id) VALUES($1) ON CONFLICT DO NOTHING`, event.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if result.RowsAffected() == 0 {
		writeJSON(w, http.StatusOK, map[string]bool{"received": true})
		return
	}
	var workspace string
	err = tx.QueryRow(r.Context(), `SELECT workspace_id FROM workspace_billing WHERE customer_id=$1 FOR UPDATE`, event.Data.Object.Customer).Scan(&workspace)
	if errors.Is(err, pgx.ErrNoRows) {
		// A shared Stripe account also delivers Autodisc events. Ignore customers
		// outside this application's durable association; never import them.
		if err = tx.Commit(r.Context()); err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"received": true})
		return
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// Fetch the current provider state under the workspace lock. Replayed or
	// out-of-order webhook payloads cannot resurrect a canceled subscription.
	var list struct {
		Data    []stripeSubscription `json:"data"`
		HasMore bool                 `json:"has_more"`
	}
	err = b.request(r.Context(), http.MethodGet, "/v1/subscriptions", url.Values{"customer": {event.Data.Object.Customer}, "status": {"all"}, "limit": {"100"}, "expand[]": {"data.latest_invoice"}}, "", &list)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if list.HasMore {
		writeStoreError(w, fmt.Errorf("%w: billing history needs reconciliation", ErrConflict))
		return
	}
	var current *stripeSubscription
	for i := range list.Data {
		if list.Data[i].Status != "canceled" && list.Data[i].Status != "incomplete_expired" {
			if current != nil {
				writeStoreError(w, ErrConflict)
				return
			}
			current = &list.Data[i]
		}
	}
	if current == nil {
		_, err = tx.Exec(r.Context(), `UPDATE workspace_billing SET status='canceled',cancel_at_period_end=false,checkout_url='',checkout_expires_at=NULL,updated_at=now() WHERE workspace_id=$1`, workspace)
	} else {
		plan, planErr := b.subscriptionPlan(*current)
		if planErr != nil {
			writeStoreError(w, planErr)
			return
		}
		start, end := current.PeriodStart, current.PeriodEnd
		if len(current.Items.Data) > 0 && start == 0 {
			start, end = current.Items.Data[0].PeriodStart, current.Items.Data[0].PeriodEnd
		}
		if start == 0 || end <= start {
			writeError(w, http.StatusConflict, fmt.Errorf("invalid subscription period"))
			return
		}
		status := current.Status
		if plan == "pro" && status == "active" {
			var invoice struct {
				Status string `json:"status"`
			}
			// Basil removed invoice.paid; the invoice status confirms settlement.
			if json.Unmarshal(current.LatestInvoice, &invoice) != nil || invoice.Status != "paid" {
				status = "payment_pending"
			}
		}
		_, err = tx.Exec(r.Context(), `UPDATE workspace_billing SET subscription_id=$2,plan_id=$3,status=$4,period_start=$5,period_end=$6,cancel_at_period_end=$7,checkout_url='',checkout_expires_at=NULL,updated_at=now() WHERE workspace_id=$1`, workspace, current.ID, plan, status, time.Unix(start, 0), time.Unix(end, 0), current.CancelAtPeriodEnd)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"received": true})
}

type BillingUsageEvent struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspaceId"`
	AmountCents int64     `json:"amountCents"`
	Resource    string    `json:"resource"`
	RateVersion string    `json:"rateVersion"`
	OccurredAt  time.Time `json:"occurredAt"`
}

// RecordBillingUsage accepts already-rated usage only from the trusted producer.
// No human/agent API can set amounts, grant credits, or mark a plan paid.
func (s *Store) RecordBillingUsage(ctx context.Context, e BillingUsageEvent) error {
	if len(e.ID) < 8 || len(e.ID) > 100 || e.AmountCents <= 0 || e.AmountCents > 100_000_000 || strings.TrimSpace(e.Resource) == "" || len(e.Resource) > 200 || strings.TrimSpace(e.RateVersion) == "" || len(e.RateVersion) > 100 || e.OccurredAt.IsZero() || e.OccurredAt.After(s.now().Add(time.Minute)) || e.OccurredAt.Before(s.now().Add(-34*24*time.Hour)) {
		return fmt.Errorf("invalid usage event")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var customer, subscription, status string
	var start, end *time.Time
	err = tx.QueryRow(ctx, `SELECT COALESCE(customer_id,''),COALESCE(subscription_id,''),status,period_start,period_end FROM workspace_billing WHERE workspace_id=$1 FOR UPDATE`, e.WorkspaceID).Scan(&customer, &subscription, &status, &start, &end)
	if err != nil {
		return err
	}
	// Resolve a retry before checking the current period: a previously accepted
	// event remains a successful retry after the billing cycle has advanced.
	var existing BillingUsageEvent
	err = tx.QueryRow(ctx, `SELECT workspace_id,amount_cents,resource,rate_version,occurred_at FROM billing_usage_events WHERE id=$1`, e.ID).Scan(&existing.WorkspaceID, &existing.AmountCents, &existing.Resource, &existing.RateVersion, &existing.OccurredAt)
	if err == nil {
		if existing.WorkspaceID != e.WorkspaceID || existing.AmountCents != e.AmountCents || existing.Resource != e.Resource || existing.RateVersion != e.RateVersion || !existing.OccurredAt.Equal(e.OccurredAt.Truncate(time.Microsecond)) {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if status != "active" || customer == "" || subscription == "" || start == nil || end == nil || e.OccurredAt.Before(*start) || !e.OccurredAt.Before(*end) {
		return fmt.Errorf("%w: usage must belong to an active paid billing period", ErrConflict)
	}
	_, err = tx.Exec(ctx, `INSERT INTO billing_usage_events(id,workspace_id,customer_id,subscription_id,amount_cents,resource,rate_version,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, e.ID, e.WorkspaceID, customer, subscription, e.AmountCents, e.Resource, e.RateVersion, e.OccurredAt)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (b *BillingGateway) DispatchUsage(ctx context.Context, s *Store) error {
	if !b.Ready() {
		return nil
	}
	if err := b.ValidatePrices(ctx); err != nil {
		return err
	}
	// A durable first-attempt timestamp bounds provider deduplication. Events
	// whose result is uncertain for 23 hours require reconciliation, not replay.
	_, err := s.pool.Exec(ctx, `UPDATE billing_usage_events SET needs_reconciliation=true WHERE sent_at IS NULL AND first_attempt_at < now()-interval '23 hours'`)
	if err != nil {
		return err
	}
	for i := 0; i < 100; i++ {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return err
		}
		var id, customer string
		var amount int64
		var occurred time.Time
		err = tx.QueryRow(ctx, `SELECT id,customer_id,amount_cents,occurred_at FROM billing_usage_events WHERE sent_at IS NULL AND NOT needs_reconciliation ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&id, &customer, &amount, &occurred)
		if errors.Is(err, pgx.ErrNoRows) {
			tx.Rollback(ctx)
			return nil
		}
		if err != nil {
			tx.Rollback(ctx)
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE billing_usage_events SET first_attempt_at=COALESCE(first_attempt_at,now()) WHERE id=$1`, id)
		if err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
		// Multiple dispatchers may attempt the same event; Stripe's stable
		// identifier and idempotency key make those concurrent retries safe.
		values := url.Values{"event_name": {b.Config.MeterEventName}, "identifier": {id}, "payload[stripe_customer_id]": {customer}, "payload[value]": {strconv.FormatInt(amount, 10)}, "timestamp": {strconv.FormatInt(occurred.Unix(), 10)}}
		if err = b.request(ctx, http.MethodPost, "/v1/billing/meter_events", values, "canter-usage-"+id, nil); err != nil {
			return err
		}
		if _, err = s.pool.Exec(ctx, `UPDATE billing_usage_events SET sent_at=now() WHERE id=$1 AND sent_at IS NULL`, id); err != nil {
			return err
		}
	}
	return nil
}
