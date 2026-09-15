package controlplane

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/canter0/canter/pricing"
)

type BillingConfig struct {
	PortalConfigurationID                                             string
	Enabled                                                           bool
	SecretKey, WebhookSecret, IngestToken                             string
	PaygPriceID, ProPriceID, ProUsagePriceID, MeterID, MeterEventName string
}
type BillingGateway struct {
	Config    BillingConfig
	client    *http.Client
	baseURL   string
	mu        sync.Mutex
	validated bool
}

func NewBillingGateway(config BillingConfig) *BillingGateway {
	return &BillingGateway{Config: config, client: &http.Client{Timeout: 15 * time.Second}, baseURL: "https://api.stripe.com"}
}
func (b *BillingGateway) Ready() bool {
	return b != nil && b.Config.Enabled && b.Config.SecretKey != "" && b.Config.WebhookSecret != "" && len(b.Config.IngestToken) >= 32 && b.Config.PaygPriceID != "" && b.Config.ProPriceID != "" && b.Config.ProUsagePriceID != "" && b.Config.MeterID != "" && b.Config.MeterEventName != "" && b.Config.PortalConfigurationID != ""
}
func (b *BillingGateway) request(ctx context.Context, method, path string, values url.Values, key string, out any) error {
	address := b.baseURL + path
	var body io.Reader
	if method == http.MethodGet {
		if len(values) > 0 {
			address += "?" + values.Encode()
		}
	} else {
		body = strings.NewReader(values.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, address, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(b.Config.SecretKey, "")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Pin the API shape used by the adapter; upgrading this requires fixture tests.
	req.Header.Set("Stripe-Version", "2025-03-31.basil")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("payment provider unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("payment provider returned status %d", resp.StatusCode)
	}
	if out == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return err
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(out)
}

// Validate the configured prices before opening Checkout. An accidental price ID
// must never turn the advertised $20 credit into a second $20 usage charge.
func (b *BillingGateway) ValidatePrices(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.validated {
		return nil
	}
	pro, _ := pricing.Find("pro")
	for _, spec := range []struct{ id, kind string }{{b.Config.PaygPriceID, "payg"}, {b.Config.ProPriceID, "base"}, {b.Config.ProUsagePriceID, "overage"}} {
		var p struct {
			Active        bool            `json:"active"`
			Currency      string          `json:"currency"`
			BillingScheme string          `json:"billing_scheme"`
			TiersMode     string          `json:"tiers_mode"`
			UnitAmount    *int64          `json:"unit_amount"`
			Transform     json.RawMessage `json:"transform_quantity"`
			Recurring     struct {
				Interval      string `json:"interval"`
				IntervalCount int    `json:"interval_count"`
				UsageType     string `json:"usage_type"`
				Meter         string `json:"meter"`
			} `json:"recurring"`
			Tiers []struct {
				UpTo       *int64 `json:"up_to"`
				UnitAmount *int64 `json:"unit_amount"`
				FlatAmount *int64 `json:"flat_amount"`
			} `json:"tiers"`
		}
		if err := b.request(ctx, http.MethodGet, "/v1/prices/"+url.PathEscape(spec.id), url.Values{"expand[]": {"tiers"}}, "", &p); err != nil {
			return err
		}
		valid := p.Active && p.Currency == pricing.Current().Currency && p.Recurring.Interval == "month" && p.Recurring.IntervalCount == 1 && (len(p.Transform) == 0 || string(p.Transform) == "null")
		if spec.kind == "base" {
			valid = valid && p.BillingScheme == "per_unit" && p.UnitAmount != nil && *p.UnitAmount == pro.MonthlyCents && p.Recurring.UsageType == "licensed"
		} else {
			valid = valid && p.Recurring.UsageType == "metered" && p.Recurring.Meter == b.Config.MeterID
			if spec.kind == "payg" {
				valid = valid && p.BillingScheme == "per_unit" && p.UnitAmount != nil && *p.UnitAmount == 1
			} else {
				valid = valid && p.BillingScheme == "tiered" && p.TiersMode == "graduated" && len(p.Tiers) == 2
				if valid {
					a, z := p.Tiers[0], p.Tiers[1]
					valid = a.UpTo != nil && *a.UpTo == pro.IncludedUsageCents && a.UnitAmount != nil && *a.UnitAmount == 0 && (a.FlatAmount == nil || *a.FlatAmount == 0) && z.UpTo == nil && z.UnitAmount != nil && *z.UnitAmount == 1 && (z.FlatAmount == nil || *z.FlatAmount == 0)
				}
			}
		}
		if !valid {
			return fmt.Errorf("billing price configuration does not match the Canter catalog")
		}
	}
	var meter struct {
		EventName   string `json:"event_name"`
		Status      string `json:"status"`
		Aggregation struct {
			Formula string `json:"formula"`
		} `json:"default_aggregation"`
		Customer struct {
			Key  string `json:"event_payload_key"`
			Type string `json:"type"`
		} `json:"customer_mapping"`
		Value struct {
			Key string `json:"event_payload_key"`
		} `json:"value_settings"`
		Window string `json:"event_time_window"`
	}
	if err := b.request(ctx, http.MethodGet, "/v1/billing/meters/"+url.PathEscape(b.Config.MeterID), nil, "", &meter); err != nil {
		return err
	}
	if meter.EventName != b.Config.MeterEventName || meter.Status != "active" || meter.Aggregation.Formula != "sum" || meter.Customer.Key != "stripe_customer_id" || meter.Customer.Type != "by_id" || meter.Value.Key != "value" || meter.Window != "" {
		return fmt.Errorf("billing meter configuration does not match the usage ledger")
	}
	b.validated = true
	return nil
}

func verifyBillingSignature(body []byte, signature, secret string, now time.Time) bool {
	if secret == "" {
		return false
	}
	var stamp string
	var signatures []string
	for _, part := range strings.Split(signature, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		if k == "t" {
			stamp = v
		}
		if k == "v1" {
			signatures = append(signatures, v)
		}
	}
	seconds, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil || seconds < now.Add(-5*time.Minute).Unix() || seconds > now.Add(5*time.Minute).Unix() {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(stamp + "."))
	mac.Write(body)
	for _, candidate := range signatures {
		decoded, err := hex.DecodeString(candidate)
		if err == nil && hmac.Equal(decoded, mac.Sum(nil)) {
			return true
		}
	}
	return false
}

type stripeSubscription struct {
	ID                string          `json:"id"`
	Customer          string          `json:"customer"`
	Status            string          `json:"status"`
	CancelAtPeriodEnd bool            `json:"cancel_at_period_end"`
	PeriodStart       int64           `json:"current_period_start"`
	PeriodEnd         int64           `json:"current_period_end"`
	LatestInvoice     json.RawMessage `json:"latest_invoice"`
	Items             struct {
		Data []struct {
			Quantity int64 `json:"quantity"`
			Price    struct {
				ID string `json:"id"`
			} `json:"price"`
			PeriodStart int64 `json:"current_period_start"`
			PeriodEnd   int64 `json:"current_period_end"`
		} `json:"data"`
	} `json:"items"`
}

func (b *BillingGateway) subscriptionPlan(s stripeSubscription) (string, error) {
	var base, overage, payg bool
	for _, item := range s.Items.Data {
		switch item.Price.ID {
		case b.Config.ProPriceID:
			if item.Quantity != 1 {
				return "", ErrConflict
			}
			base = true
		case b.Config.ProUsagePriceID:
			overage = true
		case b.Config.PaygPriceID:
			payg = true
		default:
			return "", ErrConflict
		}
	}
	if len(s.Items.Data) == 1 && payg {
		return "payg", nil
	}
	if len(s.Items.Data) == 2 && base && overage {
		return "pro", nil
	}
	return "", ErrConflict
}
