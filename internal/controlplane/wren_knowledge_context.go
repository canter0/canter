package controlplane

import "context"

// Generated knowledge is refreshed at model-step boundaries. A recovered
// checkpoint cannot keep using a disabled skill or a forgotten memory block.
func (s *Store) refreshWrenKnowledge(ctx context.Context, c Conversation, run OperatorRun, messages []modelMessage) ([]modelMessage, error) {
	if err := s.ValidateKnowledgeRun(ctx, c, run); err != nil {
		return nil, err
	}
	memory, err := s.MemoryContext(ctx, c, run)
	if err != nil {
		return nil, err
	}
	project, err := s.ProjectContext(ctx, c, run)
	if err != nil {
		return nil, err
	}
	reply, err := s.ReplyRunContext(ctx, c, run)
	if err != nil {
		return nil, err
	}
	skills, err := s.SkillContext(ctx, c, run)
	if err != nil {
		return nil, err
	}
	filtered := make([]modelMessage, 0, len(messages)+2)
	for _, m := range messages {
		if m.ContextKey != "wren-projects" && m.ContextKey != "wren-memory" && m.ContextKey != "wren-skills" && m.ContextKey != "reply" {
			filtered = append(filtered, m)
		}
	}
	blocks := []modelMessage{}
	if reply != "" {
		blocks = append(blocks, modelMessage{Role: "user", Content: reply, ContextKey: "reply"})
	}
	if project != "" {
		blocks = append(blocks, modelMessage{Role: "user", Content: project, ContextKey: "wren-projects"})
	}
	if memory != "" {
		blocks = append(blocks, modelMessage{Role: "user", Content: memory, ContextKey: "wren-memory"})
	}
	if skills != "" {
		blocks = append(blocks, modelMessage{Role: "user", Content: skills, ContextKey: "wren-skills"})
	}
	if len(blocks) == 0 {
		return filtered, nil
	}
	at := 0
	if len(filtered) > 0 && filtered[0].Role == "system" {
		at = 1
	}
	out := make([]modelMessage, 0, len(filtered)+len(blocks))
	out = append(out, filtered[:at]...)
	out = append(out, blocks...)
	out = append(out, filtered[at:]...)
	return out, nil
}
