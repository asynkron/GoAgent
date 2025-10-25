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
	copyHistory := make([]ChatMessage, len(r.history))
	copy(copyHistory, r.history)
	return copyHistory
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
