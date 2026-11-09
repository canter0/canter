package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type WrenAIModelTariff struct {
	InputMicroUSDPerMillionTokens  int64  `json:"inputMicroUsdPerMillionTokens"`
	OutputMicroUSDPerMillionTokens int64  `json:"outputMicroUsdPerMillionTokens"`
	MaxInputBytes                  int    `json:"maxInputBytes"`
	MaxOutputTokens                int    `json:"maxOutputTokens"`
	ImageEstimateMicroUSD          int64  `json:"imageEstimateMicroUsd"`
	Source                         string `json:"source"`
	ObservedAt                     string `json:"observedAt"`
}
type WrenAIUsageConfig struct {
	InitialAllowanceMicroUSD int64                        `json:"initialAllowanceMicroUsd"`
	Enforce                  bool                         `json:"enforce"`
	ModelTariffs             map[string]WrenAIModelTariff `json:"modelTariffs"`
	KindEstimatesMicroUSD    map[string]int64             `json:"kindEstimatesMicroUsd"`
}

// Missing policy is never replaced with invented customer credit or prices.
func WrenAIUsageConfigFromEnv() WrenAIUsageConfig {
	var c WrenAIUsageConfig
	if raw := os.Getenv("CANTER_AI_USAGE_POLICY_JSON"); raw != "" {
		if json.Unmarshal([]byte(raw), &c) != nil {
			return WrenAIUsageConfig{Enforce: true, InitialAllowanceMicroUSD: -1}
		}
	}
	if raw := strings.TrimSpace(os.Getenv("CANTER_AI_INITIAL_ALLOWANCE_MICRO_USD")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 || n > maxInitialWrenAIAllowanceMicroUSD {
			c.InitialAllowanceMicroUSD = -1
			c.Enforce = true
		} else {
			c.InitialAllowanceMicroUSD = n
		}
	}
	if c.InitialAllowanceMicroUSD < 0 || c.InitialAllowanceMicroUSD > maxInitialWrenAIAllowanceMicroUSD {
		c.InitialAllowanceMicroUSD = -1
		c.Enforce = true
	}
	if strings.EqualFold(os.Getenv("CANTER_AI_USAGE_ENFORCED"), "true") {
		c.Enforce = true
	}
	return c
}

type WrenAIRequestEstimate struct {
	Kind, Model                             string
	InputBytes, MaxOutputTokens, ImageCount int
}

func (c WrenAIUsageConfig) estimate(in WrenAIRequestEstimate) (int64, json.RawMessage, error) {
	if in.Kind == "media" || in.Kind == "dictation" {
		n := c.KindEstimatesMicroUSD[in.Kind]
		if n <= 0 || n > 1_000_000_000_000 {
			return 0, nil, fmt.Errorf("%w: %s AI cost policy is not configured", ErrCapacity, in.Kind)
		}
		meta, _ := json.Marshal(map[string]any{"method": "operator_configured_request_estimate", "kind": in.Kind, "estimatedMicroUsd": n})
		return n, meta, nil
	}
	t, ok := c.ModelTariffs[in.Model]
	if !ok || t.InputMicroUSDPerMillionTokens <= 0 || t.OutputMicroUSDPerMillionTokens <= 0 || t.InputMicroUSDPerMillionTokens > 1_000_000_000 || t.OutputMicroUSDPerMillionTokens > 1_000_000_000 || t.MaxInputBytes < 1 || t.MaxInputBytes > 8<<20 || t.MaxOutputTokens < 1 || t.MaxOutputTokens > 128000 || t.Source == "" || t.ObservedAt == "" {
		return 0, nil, fmt.Errorf("%w: model AI tariff is not configured", ErrCapacity)
	}
	if in.InputBytes < 1 || in.InputBytes > t.MaxInputBytes || in.MaxOutputTokens < 1 || in.MaxOutputTokens > t.MaxOutputTokens || in.ImageCount < 0 || in.ImageCount > 32 {
		return 0, nil, fmt.Errorf("%w: model request exceeds its AI estimate policy", ErrCapacity)
	}
	if in.ImageCount > 0 && t.ImageEstimateMicroUSD <= 0 {
		return 0, nil, fmt.Errorf("%w: image AI cost policy is not configured", ErrCapacity)
	}
	if t.ImageEstimateMicroUSD > 1_000_000_000 {
		return 0, nil, fmt.Errorf("invalid image AI estimate")
	}
	// Byte-count token estimate is deliberately conservative for text and JSON;
	// it is an estimate, not a contractual supplier maximum. Images need an
	// explicit additional estimate. Store actual overruns and block future work.
	n := (int64(in.InputBytes)*t.InputMicroUSDPerMillionTokens+int64(in.MaxOutputTokens)*t.OutputMicroUSDPerMillionTokens+999999)/1000000 + int64(in.ImageCount)*t.ImageEstimateMicroUSD
	if n < 1 {
		n = 1
	}
	meta, _ := json.Marshal(map[string]any{"method": "serialized_byte_token_estimate", "inputBytes": in.InputBytes, "maxOutputTokens": in.MaxOutputTokens, "images": in.ImageCount, "tariff": t})
	return n, meta, nil
}

type WrenAIPaidRequest struct {
	store         *Store
	workspace, id string
	dispatched    bool
}

// BeginWrenAIPaidRequest reserves the workspace's shared allowance. No provider
// call may occur until MarkDispatched succeeds. Nil means enforcement disabled.
func (h *HTTPServer) BeginWrenAIPaidRequest(ctx context.Context, meta WrenAIReservationInput, estimate WrenAIRequestEstimate) (*WrenAIPaidRequest, error) {
	if !h.config.AIUsage.Enforce {
		return nil, nil
	}
	n, policy, err := h.config.AIUsage.estimate(estimate)
	if err != nil {
		return nil, err
	}
	meta.AmountMicroUSD = n
	meta.Estimate = policy
	r, err := h.service.Store.ReserveWrenAIUsage(ctx, meta)
	if err != nil {
		return nil, err
	}
	if r.State != "reserved" {
		return nil, ErrConflict
	}
	return &WrenAIPaidRequest{store: h.service.Store, workspace: meta.WorkspaceID, id: r.ID}, nil
}
func (r *WrenAIPaidRequest) MarkDispatched(ctx context.Context) error {
	if r == nil {
		return nil
	}
	err := r.store.DispatchWrenAIUsage(ctx, r.workspace, r.id)
	if err == nil {
		r.dispatched = true
	}
	return err
}

// Finish accepts ONLY the provider usage object, never the entire response or
// error body. Unknown cost retains its hold, even on HTTP error or timeout.
func (r *WrenAIPaidRequest) Finish(ctx context.Context, usage json.RawMessage, providerID string) error {
	if r == nil {
		return nil
	}
	save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if !r.dispatched {
		return r.store.ReleaseWrenAIUsage(save, r.workspace, r.id)
	}
	var receipt struct {
		Cost *float64 `json:"cost"`
	}
	if json.Unmarshal(usage, &receipt) != nil || receipt.Cost == nil {
		return r.store.MarkWrenAIUsageUncertain(save, r.workspace, r.id)
	}
	actual, err := WrenAICostMicroUSD(*receipt.Cost)
	if err != nil {
		return r.store.MarkWrenAIUsageUncertain(save, r.workspace, r.id)
	}
	return r.store.SettleWrenAIUsage(save, r.workspace, r.id, actual, providerID, usage)
}

func (h *HTTPServer) withWrenAIModelUsage(config OperatorConfig, c Conversation, run OperatorRun) OperatorConfig {
	if run.ID != "" {
		config.projectAuthority = func(ctx context.Context) error { return h.service.Store.ValidateWrenProjectRun(ctx, c, run) }
	}
	if !h.config.AIUsage.Enforce {
		return config
	}
	config.aiUsage = func(ctx context.Context, model string, body []byte, maxOutput int, requestedPurpose string) (*WrenAIPaidRequest, error) {
		key, err := newID("model_request_")
		if err != nil {
			return nil, err
		}
		images, err := wrenAIModelImageCount(body)
		if err != nil {
			return nil, err
		}
		purpose := requestedPurpose
		if c.ID == "" && run.ID == "" {
			purpose = "welcome"
		}
		return h.BeginWrenAIPaidRequest(ctx, WrenAIReservationInput{WorkspaceID: c.WorkspaceID, AccountID: c.AccountID, ConversationID: c.ID, RunID: run.ID, RunLease: run.Lease, RequestKey: key, Kind: "operator", Model: model, Purpose: purpose}, WrenAIRequestEstimate{Kind: "operator", Model: model, InputBytes: len(body), MaxOutputTokens: maxOutput, ImageCount: images})
	}
	return config
}

// WrenAIRequestAvailable is a read-only readiness hint. Begin still performs
// the authoritative locked reservation, because concurrent work can spend it.
func (h *HTTPServer) WrenAIRequestAvailable(ctx context.Context, workspace string, estimate WrenAIRequestEstimate) error {
	if !h.config.AIUsage.Enforce {
		return nil
	}
	n, _, err := h.config.AIUsage.estimate(estimate)
	if err != nil {
		return err
	}
	v, err := h.service.Store.WrenAIAllowance(ctx, workspace)
	if err != nil {
		return fmt.Errorf("%w: workspace AI allowance is not configured", ErrCapacity)
	}
	if v.ClosedAt != nil {
		return ErrForbidden
	}
	if v.SpentMicroUSD > v.LimitMicroUSD || v.HeldMicroUSD > v.LimitMicroUSD-v.SpentMicroUSD || n > v.LimitMicroUSD-v.SpentMicroUSD-v.HeldMicroUSD {
		return fmt.Errorf("%w: shared workspace AI allowance is exhausted", ErrCapacity)
	}
	return nil
}

func wrenAIModelImageCount(body []byte) (int, error) {
	var input struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &input) != nil {
		return 0, fmt.Errorf("invalid model request")
	}
	images := 0
	for _, m := range input.Messages {
		raw := strings.TrimSpace(string(m.Content))
		if raw == "" || raw == "null" || strings.HasPrefix(raw, `"`) {
			continue
		}
		var parts []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(m.Content, &parts) != nil {
			return 0, fmt.Errorf("%w: multimodal AI estimate is unavailable", ErrCapacity)
		}
		for _, p := range parts {
			switch p.Type {
			case "text", "input_text":
			case "image_url", "input_image":
				images++
			default:
				return 0, fmt.Errorf("%w: this media type has no configured AI estimate", ErrCapacity)
			}
		}
	}
	return images, nil
}
