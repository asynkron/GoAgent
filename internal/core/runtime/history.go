package runtime

import (
	"encoding/json"
	"fmt"
	"os"
)

func (r *Runtime) appendHistory(message ChatMessage) {
	pass := r.currentPassCount()
	message.Pass = pass

	r.historyMu.Lock()
	defer r.historyMu.Unlock()

	r.history = append(r.history, message)
	r.applyHistoryAmnesiaLocked(pass)
}

func (r *Runtime) historySnapshot() []ChatMessage {
	r.historyMu.RLock()
	defer r.historyMu.RUnlock()

	return append([]ChatMessage(nil), r.history...)
}

// planningHistorySnapshot prepares the history for a plan request. It compacts
// the in-memory slice when the estimated token usage exceeds the configured
// budget and returns a copy so callers can safely hand it to external clients.
func (r *Runtime) planningHistorySnapshot() []ChatMessage {
	r.historyMu.Lock()
	defer r.historyMu.Unlock()

	limit := r.contextBudget.triggerTokens()
	if limit > 0 {
		total, per := estimateHistoryTokenUsage(r.history)
		if total > limit {
			compactHistory(r.history, per, total, limit)
		}
	}

	return append([]ChatMessage(nil), r.history...)
}

func (r *Runtime) writeHistoryLog(history []ChatMessage) {
	// Persist the exact payload forwarded to the model so hosts can inspect it.
	data, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		r.emit(RuntimeEvent{
			Type:    EventTypeStatus,
			Message: fmt.Sprintf("Failed to encode history log: %v", err),
			Level:   StatusLevelWarn,
		})
		return
	}

	if err := os.WriteFile("history.json", data, 0o644); err != nil {
		r.emit(RuntimeEvent{
			Type:    EventTypeStatus,
			Message: fmt.Sprintf("Failed to write history log: %v", err),
			Level:   StatusLevelWarn,
		})
	}
}
