package runtime

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

const (
	summaryPrefix      = "[summary]"
	summarySnippetSize = 160
)

// estimateHistoryTokenUsage walks the history and returns the total estimated
// token usage together with the per-message contribution. The heuristic is
// intentionally simple (roughly four characters per token) which keeps the
// estimator fast while still providing a useful signal for trimming.
func estimateHistoryTokenUsage(history []ChatMessage) (int, []int) {
	totals := make([]int, len(history))
	var sum int
	for i := range history {
		tokens := estimateMessageTokens(history[i])
		totals[i] = tokens
		sum += tokens
	}
	return sum, totals
}

// estimateMessageTokens approximates the token usage of an individual message
// using a character based heuristic. We include a small base overhead so that
// very short messages still contribute to the budget.
func estimateMessageTokens(message ChatMessage) int {
	const baseOverhead = 4
	total := baseOverhead

	total += estimateStringTokens(string(message.Role))
	total += estimateStringTokens(message.Content)
	total += estimateStringTokens(message.ToolCallID)
	total += estimateStringTokens(message.Name)

	for _, call := range message.ToolCalls {
		total += baseOverhead
		total += estimateStringTokens(call.ID)
		total += estimateStringTokens(call.Name)
		total += estimateStringTokens(call.Arguments)
	}

	return total
}

func estimateStringTokens(value string) int {
	if value == "" {
		return 0
	}
	runes := utf8.RuneCountInString(value)
	tokens := int(math.Ceil(float64(runes) / 4))
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}

// compactHistory replaces the oldest non-system messages with summaries until
// the history drops below the provided limit or no further compaction is
// possible. The slice is modified in place, preserving ordering.
func compactHistory(history []ChatMessage, per []int, total, limit int) (int, []int, bool) {
	if limit <= 0 {
		return total, per, false
	}
	changed := false
	for i := range history {
		if total <= limit {
			break
		}
		message := history[i]
		if message.Role == RoleSystem || message.Summarized {
			continue
		}

		summary := synthesizeSummary(message)
		summaryTokens := estimateMessageTokens(summary)

		if i < len(per) {
			total -= per[i]
			per[i] = summaryTokens
		} else {
			per = append(per, summaryTokens)
		}
		total += summaryTokens
		history[i] = summary
		changed = true
	}
	return total, per, changed
}

func synthesizeSummary(message ChatMessage) ChatMessage {
	summary := ChatMessage{
		Role:       RoleAssistant,
		Timestamp:  message.Timestamp,
		Summarized: true,
	}

	switch message.Role {
	case RoleTool:
		summary.Content = buildToolSummary(message.Content)
	case RoleUser:
		summary.Content = buildConversationSummary("User", message.Content)
	case RoleAssistant:
		summary.Content = buildConversationSummary("Assistant", message.Content)
	default:
		summary.Content = buildConversationSummary("Message", message.Content)
	}

	if summary.Content == "" {
		summary.Content = fmt.Sprintf("%s Conversation context compressed.", summaryPrefix)
	}

	return summary
}

func buildConversationSummary(label, content string) string {
	snippet := compactSnippet(content)
	if snippet == "" {
		return ""
	}
	return fmt.Sprintf("%s %s recap: %s", summaryPrefix, strings.ToLower(label), snippet)
}

func buildToolSummary(content string) string {
	var payload PlanObservationPayload
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		snippet := compactSnippet(content)
		if snippet == "" {
			return fmt.Sprintf("%s tool observation compacted.", summaryPrefix)
		}
		return fmt.Sprintf("%s tool observation recap: %s", summaryPrefix, snippet)
	}

	var parts []string
	if payload.Summary != "" {
		parts = append(parts, payload.Summary)
	}
	if payload.Details != "" {
		parts = append(parts, payload.Details)
	}
	for _, step := range payload.PlanObservation {
		if step.ID == "" && step.Status == "" {
			continue
		}
		label := step.ID
		if label == "" {
			label = "step"
		}
		parts = append(parts, fmt.Sprintf("%s=%s", label, step.Status))
		if len(parts) >= 6 {
			break
		}
	}
	if payload.CanceledByHuman {
		parts = append(parts, "canceled by human")
	}
	if payload.OperationCanceled {
		parts = append(parts, "operation canceled")
	}
	if payload.Truncated {
		parts = append(parts, "output truncated")
	}

	snippet := compactSnippet(strings.Join(parts, "; "))
	if snippet == "" {
		return fmt.Sprintf("%s tool observation compacted.", summaryPrefix)
	}
	return fmt.Sprintf("%s tool observation: %s", summaryPrefix, snippet)
}

func compactSnippet(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return ""
	}
	// Collapse whitespace so we keep the snippet short and legible.
	trimmed = strings.Join(strings.Fields(trimmed), " ")
	runes := []rune(trimmed)
	if len(runes) <= summarySnippetSize {
		return trimmed
	}
	return string(runes[:summarySnippetSize]) + "…"
}
