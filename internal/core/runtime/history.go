package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
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
			beforeLen := len(r.history)
			// Add safeguard: limit iterations to prevent infinite loops
			// If summarization doesn't reduce tokens enough, we'll stop after max iterations
			const maxCompactionIterations = 10
			iterations := 0
			for total > limit && iterations < maxCompactionIterations {
				var changed bool
				total, per, changed = compactHistory(r.history, per, total, limit)
				iterations++
				if !changed {
					// No progress made - all eligible messages already summarized
					// or we can't make progress. Break to avoid infinite loop.
					break
				}
			}
			afterLen := len(r.history)
			removed := beforeLen - afterLen
			// Note: removed might be 0 if we just summarized without removing entries
			r.options.Metrics.RecordContextCompaction(removed, afterLen)

			if iterations >= maxCompactionIterations && total > limit {
				r.options.Logger.Warn(context.Background(), "History compaction reached max iterations without meeting budget",
					Field("total_tokens", total),
					Field("limit", limit),
					Field("iterations", iterations),
				)
			}
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

	var historyPath string
	if r.options.HistoryLogPath != nil {
		historyPath = strings.TrimSpace(*r.options.HistoryLogPath)
	}
	if historyPath == "" {
		return
	}

	if err := os.WriteFile(historyPath, data, 0o644); err != nil {
		r.emit(RuntimeEvent{
			Type:    EventTypeStatus,
			Message: fmt.Sprintf("Failed to write history log: %v", err),
			Level:   StatusLevelWarn,
		})
	}
}
