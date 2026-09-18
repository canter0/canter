package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
)

// BillingPaymentMethod contains display details only, never payment credentials.
type BillingPaymentMethod struct {
	Brand string `json:"brand"`
	Last4 string `json:"last4"`
}

// ConfigureBilling is called before workers start. Disabled billing retains the
// existing beta behavior; enabling billing enforces payment readiness server-side.
func (s *Store) ConfigureBilling(b *BillingGateway) { s.billing = b }

func (s *Store) requireBillingPayment(ctx context.Context, workspace string) error {
	if s.billing == nil || !s.billing.Config.Enabled {
		return nil
	}
	if !s.billing.Ready() {
		return fmt.Errorf("%w: billing is unavailable", ErrConflict)
	}
	_, ready, err := s.billingPaymentReadiness(ctx, workspace, s.billing)
	if err != nil {
		return err
	}
	if !ready {
		return fmt.Errorf("%w: a workspace owner must add or update the payment method in Billing before provisioning paid resources", ErrConflict)
	}
	return nil
}

func (s *Store) billingPaymentReadiness(ctx context.Context, workspace string, b *BillingGateway) (*BillingPaymentMethod, bool, error) {
	var customer, subscription string
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(customer_id,''),COALESCE(subscription_id,'') FROM workspace_billing WHERE workspace_id=$1`, workspace).Scan(&customer, &subscription)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if customer == "" || subscription == "" {
		return nil, false, nil
	}
	return b.paymentReadiness(ctx, customer, subscription, s.now())
}

type stripeCardMethod struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Customer string `json:"customer"`
	Card     struct {
		Brand    string `json:"brand"`
		Last4    string `json:"last4"`
		ExpMonth int    `json:"exp_month"`
		ExpYear  int    `json:"exp_year"`
	} `json:"card"`
}

func (b *BillingGateway) paymentReadiness(ctx context.Context, customer, subscription string, now time.Time) (*BillingPaymentMethod, bool, error) {
	var sub struct {
		stripeSubscription
		CollectionMethod     string            `json:"collection_method"`
		DefaultPaymentMethod *stripeCardMethod `json:"default_payment_method"`
	}
	if err := b.request(ctx, http.MethodGet, "/v1/subscriptions/"+url.PathEscape(subscription), url.Values{"expand[]": {"default_payment_method", "latest_invoice"}}, "", &sub); err != nil {
		return nil, false, err
	}
	if sub.Customer != customer {
		return nil, false, fmt.Errorf("%w: billing customer mismatch", ErrConflict)
	}
	plan, err := b.subscriptionPlan(sub.stripeSubscription)
	if err != nil {
		return nil, false, err
	}
	if plan == "pro" {
		var invoice struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(sub.LatestInvoice, &invoice) != nil || invoice.Status != "paid" {
			return nil, false, nil
		}
	}
	pm := sub.DefaultPaymentMethod
	if pm == nil {
		var c struct {
			InvoiceSettings struct {
				DefaultPaymentMethod *stripeCardMethod `json:"default_payment_method"`
			} `json:"invoice_settings"`
		}
		if err := b.request(ctx, http.MethodGet, "/v1/customers/"+url.PathEscape(customer), url.Values{"expand[]": {"invoice_settings.default_payment_method"}}, "", &c); err != nil {
			return nil, false, err
		}
		pm = c.InvoiceSettings.DefaultPaymentMethod
	}
	if pm == nil || pm.ID == "" || pm.Customer != customer || pm.Type != "card" || len(pm.Card.Last4) != 4 {
		return nil, false, nil
	}
	validCard := pm.Card.ExpMonth >= 1 && pm.Card.ExpMonth <= 12 && (pm.Card.ExpYear > now.Year() || pm.Card.ExpYear == now.Year() && pm.Card.ExpMonth >= int(now.Month()))
	return &BillingPaymentMethod{Brand: pm.Card.Brand, Last4: pm.Card.Last4}, validCard && sub.Status == "active" && sub.CollectionMethod == "charge_automatically", nil
}
