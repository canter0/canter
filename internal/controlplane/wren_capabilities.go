package controlplane

import (
	"context"
	"strings"
)

// This describes current usable dependencies for one authorized conversation;
// installing a skill never adds names to this list or grants more authority.
func (h *HTTPServer) availableWrenCapabilities(ctx context.Context, workspace, account, conversation string) ([]string, error) {
	c, err := h.service.Store.Conversation(ctx, workspace, account, conversation)
	if err != nil {
		return nil, err
	}
	role, err := h.service.Store.Membership(ctx, account, workspace)
	if err != nil {
		return nil, err
	}
	writer := role != "viewer"
	modelReady := h.workspaceOperatorReady(ctx, workspace)
	computerReady, computerWritable := false, false
	client, computer, computerErr := h.ClientForConversation(ctx, c)
	if computerErr == nil && client != nil && computer.Phase == "ready" {
		computerReady = true
		computerWritable = writer && h.ComputerAgentWriteAllowed(ctx, c) == nil
	}
	runtime := OperatorRuntime{Server: h, Config: h.config.Operator}
	available := []string{}
	for _, tool := range runtime.tools() {
		policy, known := operatorPolicy(tool.Name)
		if !known {
			continue
		}
		if !writer && (policy.RequireDraft || policy.Effect == "delegation" || policy.Effect == "artifact" || policy.Effect == "context" || policy.Effect == "standing" || policy.Effect == "media") {
			continue
		}
		if policy.Effect == "media" {
			_, imageModel, e := h.mediaConfig(ctx, workspace, account)
			if e != nil || h.WrenAIRequestAvailable(ctx, workspace, WrenAIRequestEstimate{Kind: "media", Model: imageModel}) != nil {
				continue
			}
		}
		if !modelReady && tool.Name == "canter_delegate_job" {
			continue
		}
		if strings.HasPrefix(tool.Name, "canter_computer_") && tool.Name != "canter_computer_status" {
			if !computerReady {
				continue
			}
			if policy.RequireDraft && !computerWritable {
				continue
			}
		}
		if strings.HasPrefix(tool.Name, "canter_prepare_") || tool.Name == "canter_draft_change" {
			if h.service.Engine == nil {
				continue
			}
		}
		available = append(available, tool.Name)
	}
	external, err := h.WrenExtensionAvailableToolNames(ctx, workspace, account, conversation)
	if err != nil {
		return nil, err
	}
	available = append(available, external...)
	if computerReady {
		available = append(available, "computer", "browser", "files")
	}
	if computerWritable {
		available = append(available, "computer-write", "coding")
	}
	if computerWritable && h.HarnessConfigured() {
		available = append(available, "acp-opencode")
	}
	return available, nil
}
