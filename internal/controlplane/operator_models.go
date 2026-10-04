package controlplane

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
)

// Shared with the web picker so its choices and the server allowlist stay aligned.
//
//go:embed operator_models.json
var operatorModelsJSON []byte

type operatorModelSpec struct {
	ID                string   `json:"id"`
	ReasoningRequired bool     `json:"reasoningRequired"`
	ReasoningEfforts  []string `json:"reasoningEfforts"`
}

var operatorModelSpecs = func() map[string]operatorModelSpec {
	var models []operatorModelSpec
	if err := json.Unmarshal(operatorModelsJSON, &models); err != nil {
		panic(err)
	}
	ids := make(map[string]operatorModelSpec, len(models))
	for _, model := range models {
		ids[model.ID] = model
	}
	return ids
}()

func selectedOperatorModel(requested, configured string) (string, error) {
	if requested == "" || requested == configured {
		return configured, nil
	}
	if _, ok := operatorModelSpecs[requested]; ok {
		return requested, nil
	}
	return "", fmt.Errorf("select a model available in the model picker")
}

func (c OperatorConfig) modelReasoningEffort() string {
	effort := c.ReasoningEffort
	if effort == "" {
		effort = "none"
	}
	// These endpoints reject disabled thinking. Keep its default budget low.
	if effort == "none" && operatorModelSpecs[c.Model].ReasoningRequired {
		return "low"
	}
	return effort
}

// Options are persisted with each run so retries and queued turns keep the
// settings the user chose at send time.
type OperatorModelOptions struct {
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
}

func validateOperatorModelOptions(model string, options OperatorModelOptions) error {
	spec, known := operatorModelSpecs[model]
	if model == "openai/gpt-5.6-luna" {
		spec, known = operatorModelSpecs["openai/gpt-6-luna"]
	}
	if !known && options.ReasoningEffort != "" {
		return fmt.Errorf("this model does not support picker settings")
	}
	if options.ReasoningEffort != "" && !slices.Contains(spec.ReasoningEfforts, options.ReasoningEffort) {
		return fmt.Errorf("select a reasoning level supported by this model")
	}
	return nil
}
