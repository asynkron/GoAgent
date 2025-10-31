package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/asynkron/goagent/internal/core/schema"
	"github.com/xeipuuv/gojsonschema"
)

var (
	planSchemaLoader     gojsonschema.JSONLoader
	planSchemaLoaderErr  error
	planSchemaLoaderOnce sync.Once
)

const (
	validationDetailLimit   = 512
	validationBackoffBase   = 250 * time.Millisecond
	validationBackoffMax    = 4 * time.Second
	validationBackoffMaxExp = 5
)

type schemaValidationError struct {
	issues []string
}

func (e schemaValidationError) Error() string {
	if len(e.issues) == 0 {
		return "plan response failed schema validation"
	}
	return strings.Join(e.issues, "; ")
}

// validatePlanToolCall ensures the assistant response is valid JSON and
// satisfies the plan schema before we hydrate a PlanResponse structure.
// Returning retry=true signals that the helper produced feedback for the
// assistant and the runtime should request a new plan immediately.
func (r *Runtime) validatePlanToolCall(toolCall ToolCall) (*PlanResponse, bool, error) {
	trimmedArgs := strings.TrimSpace(toolCall.Arguments)
	if trimmedArgs == "" {
		payload := PlanObservationPayload{
			JSONParseError:          true,
			ResponseValidationError: true,
			Summary:                 "Assistant called the tool without providing arguments.",
			Details:                 "tool arguments were empty",
		}
		r.handlePlanValidationFailure(toolCall, payload, r.buildValidationAutoPrompt(payload))
		return nil, true, nil
	}

	var plan PlanResponse
	if err := json.Unmarshal([]byte(toolCall.Arguments), &plan); err != nil {
		payload := PlanObservationPayload{
			JSONParseError:          true,
			ResponseValidationError: true,
			Summary:                 "Tool call arguments were not valid JSON.",
			Details:                 err.Error(),
		}
		r.handlePlanValidationFailure(toolCall, payload, r.buildValidationAutoPrompt(payload))
		return nil, true, nil
	}

	if err := validatePlanAgainstSchema(toolCall.Arguments); err != nil {
		var schemaErr schemaValidationError
		if errors.As(err, &schemaErr) {
			payload := PlanObservationPayload{
				SchemaValidationError:   true,
				ResponseValidationError: true,
				Summary:                 "Tool call arguments failed schema validation.",
				Details:                 schemaErr.Error(),
			}
			r.handlePlanValidationFailure(toolCall, payload, r.buildValidationAutoPrompt(payload))
			return nil, true, nil
		}
		// Non-schema validation error (e.g., schema loading error)
		return nil, false, fmt.Errorf("validatePlanToolCall: schema validation error: %w", err)
	}

	return &plan, false, nil
}

func validatePlanAgainstSchema(raw string) error {
	loader, err := loadPlanSchema()
	if err != nil {
		return fmt.Errorf("runtime: load plan schema: %w", err)
	}

	result, err := gojsonschema.Validate(loader, gojsonschema.NewStringLoader(raw))
	if err != nil {
		return fmt.Errorf("runtime: schema validation error: %w", err)
	}
	if result.Valid() {
		return nil
	}

	issues := make([]string, 0, len(result.Errors()))
	for _, desc := range result.Errors() {
		issues = append(issues, desc.String())
	}
	return schemaValidationError{issues: issues}
}

func loadPlanSchema() (gojsonschema.JSONLoader, error) {
	planSchemaLoaderOnce.Do(func() {
		schemaMap, err := schema.PlanResponseSchema()
		if err != nil {
			planSchemaLoaderErr = err
			return
		}
		planSchemaLoader = gojsonschema.NewGoLoader(schemaMap)
	})
	if planSchemaLoaderErr != nil {
		return nil, planSchemaLoaderErr
	}
	return planSchemaLoader, nil
}

func (r *Runtime) handlePlanValidationFailure(toolCall ToolCall, payload PlanObservationPayload, autoPrompt string) {
	payload.Details = strings.TrimSpace(payload.Details)

	metadata := map[string]any{
		"details": payload.Details,
	}
	if toolCall.ID != "" {
		metadata["tool_call_id"] = toolCall.ID
	}
	if toolCall.Name != "" {
		metadata["tool_name"] = toolCall.Name
	}

	message := payload.Summary
	if details := strings.TrimSpace(payload.Details); details != "" {
		message = fmt.Sprintf("%s Details: %s", message, details)
	}

	r.emit(RuntimeEvent{
		Type:     EventTypeStatus,
		Message:  message,
		Level:    StatusLevelWarn,
		Metadata: metadata,
	})

	r.appendHistory(ChatMessage{
		Role:      RoleAssistant,
		Timestamp: time.Now(),
		ToolCalls: []ToolCall{toolCall},
	})

	if toolCall.ID != "" {
		if toolMessage, err := BuildToolMessage(payload); err != nil {
			r.emit(RuntimeEvent{
				Type:    EventTypeError,
				Message: fmt.Sprintf("Failed to encode validation feedback: %v", err),
				Level:   StatusLevelError,
			})
		} else {
			r.appendHistory(ChatMessage{
				Role:       RoleTool,
				Content:    toolMessage,
				ToolCallID: toolCall.ID,
				Name:       toolCall.Name,
				Timestamp:  time.Now(),
			})
		}
	}

	if strings.TrimSpace(autoPrompt) != "" {
		r.appendHistory(ChatMessage{
			Role:      RoleUser,
			Content:   autoPrompt,
			Timestamp: time.Now(),
		})
	}
}

func (r *Runtime) buildValidationAutoPrompt(payload PlanObservationPayload) string {
	summary := strings.TrimSpace(payload.Summary)
	if summary == "" {
		summary = "The previous tool call response could not be processed."
	}
	details := truncateForPrompt(strings.TrimSpace(payload.Details), validationDetailLimit)

	builder := strings.Builder{}
	builder.WriteString(summary)
	if details != "" {
		builder.WriteString(" Details: ")
		builder.WriteString(details)
	}
	builder.WriteString(" Please call ")
	builder.WriteString(schema.ToolName)
	builder.WriteString(" again with JSON that strictly matches the provided schema.")
	return builder.String()
}

func truncateForPrompt(value string, limit int) string {
	if limit <= 0 {
		return value
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

func computeValidationBackoff(attempt int) time.Duration {
	if attempt <= 0 {
		attempt = 1
	}
	exp := attempt - 1
	if exp > validationBackoffMaxExp {
		exp = validationBackoffMaxExp
	}

	multiplier := 1 << exp
	delay := validationBackoffBase * time.Duration(multiplier)
	if delay > validationBackoffMax {
		return validationBackoffMax
	}
	if delay < validationBackoffBase {
		return validationBackoffBase
	}
	return delay
}
