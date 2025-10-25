package runtime

import (
	"encoding/json"
	"strings"
)

const (
	// Keep the retained content compact so older messages remain lightweight.
	amnesiaAssistantContentLimit = 512
	amnesiaToolContentLimit      = 512
)

// applyHistoryAmnesiaLocked trims bulky history entries once they age beyond the
// configured pass threshold. Callers must hold historyMu.
func (r *Runtime) applyHistoryAmnesiaLocked(currentPass int) {
	threshold := r.options.AmnesiaAfterPasses
	if threshold <= 0 {
		return
	}

	for i := range r.history {
		entry := &r.history[i]
		if entry.Role != RoleAssistant && entry.Role != RoleTool {
			continue
		}
		if currentPass-entry.Pass < threshold {
			continue
		}

		switch entry.Role {
		case RoleAssistant:
			scrubAssistantHistoryEntry(entry)
		case RoleTool:
			scrubToolHistoryEntry(entry)
		}
	}
}

func scrubAssistantHistoryEntry(entry *ChatMessage) {
	if entry.Content != "" {
		entry.Content = truncateForPrompt(entry.Content, amnesiaAssistantContentLimit)
	}
	if len(entry.ToolCalls) == 0 {
		return
	}

	for i := range entry.ToolCalls {
		call := &entry.ToolCalls[i]
		if strings.TrimSpace(call.Arguments) == "" {
			continue
		}
		call.Arguments = truncateForPrompt(call.Arguments, amnesiaAssistantContentLimit)
	}
}

func scrubToolHistoryEntry(entry *ChatMessage) {
	raw := strings.TrimSpace(entry.Content)
	if raw == "" {
		return
	}

	var payload PlanObservationPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		entry.Content = truncateForPrompt(raw, amnesiaToolContentLimit)
		return
	}

	payload.Stdout = ""
	payload.Stderr = ""

	for i := range payload.PlanObservation {
		obs := &payload.PlanObservation[i]
		obs.Stdout = ""
		obs.Stderr = ""
		if obs.Details != "" {
			obs.Details = truncateForPrompt(obs.Details, amnesiaToolContentLimit)
		}
	}

	if payload.Details != "" {
		payload.Details = truncateForPrompt(payload.Details, amnesiaToolContentLimit)
	}

	sanitized, err := BuildToolMessage(payload)
	if err != nil {
		entry.Content = truncateForPrompt(raw, amnesiaToolContentLimit)
		return
	}
	entry.Content = sanitized
}
