package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Discovery selects schemas; it never grants access. WrenExtensionTools and the
// invocation path both re-evaluate the caller and current connection state.
func rankPluginTools(tools []mcpTool, query string) []mcpTool {
	type match struct {
		tool  mcpTool
		score int
	}
	matches := []match{}
	for _, tool := range tools {
		score := 0
		text := strings.ToLower(tool.Name + " " + tool.Description)
		for _, word := range strings.Fields(strings.ToLower(query)) {
			if len(word) > 1 && strings.Contains(text, word) {
				score++
			}
		}
		if score > 0 {
			matches = append(matches, match{tool, score})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score == matches[j].score {
			return matches[i].tool.Name < matches[j].tool.Name
		}
		return matches[i].score > matches[j].score
	})
	out := []mcpTool{}
	for _, match := range matches {
		out = append(out, match.tool)
		if len(out) == 8 {
			break
		}
	}
	return out
}
func (o *OperatorRuntime) findPluginTools(ctx context.Context, c Conversation, run OperatorRun, args json.RawMessage) (any, error) {
	var input struct {
		Query string `json:"query"`
	}
	if json.Unmarshal(args, &input) != nil || strings.TrimSpace(input.Query) == "" || len(input.Query) > 300 {
		return nil, fmt.Errorf("describe the plugin or capability in a short search query")
	}
	tools, err := o.Server.WrenExtensionTools(ctx, c, run)
	if err != nil {
		return nil, err
	}
	selected := rankPluginTools(tools, input.Query)
	result := []map[string]string{}
	for _, tool := range selected {
		result = append(result, map[string]string{"name": tool.Name, "description": tool.Description})
	}
	return map[string]any{"tools": result, "next": "These authorized tools will be available on the next step. If no tools match, ask the user to install or connect a plugin in Customize. Never ask for passwords or API keys in chat."}, nil
}
func (s *Store) discoveredPluginTools(ctx context.Context, run OperatorRun, available []mcpTool) ([]mcpTool, error) {
	if len(available) <= 8 {
		return available, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT result FROM operator_tool_calls WHERE run_id=$1 AND name='canter_find_plugin_tools' AND result IS NOT NULL ORDER BY created_at DESC LIMIT 3`, run.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	selected := map[string]bool{}
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var found struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if json.Unmarshal(raw, &found) == nil {
			for _, tool := range found.Tools {
				selected[tool.Name] = true
			}
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := []mcpTool{}
	for _, tool := range available {
		if selected[tool.Name] {
			out = append(out, tool)
		}
	}
	return out, nil
}
