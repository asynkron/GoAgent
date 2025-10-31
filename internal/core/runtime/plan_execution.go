package runtime

import (
	"context"
	"fmt"
	"strings"
)

// planExecutionLoop runs the main execution loop, requesting plans and executing steps
// until completion, error, or interruption.
func (r *Runtime) planExecutionLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		pass := r.incrementPassCount()
		r.options.Metrics.RecordPass(pass)
		r.options.Logger.Info(ctx, "Starting plan execution pass",
			Field("pass", pass),
		)

		if shouldStop := r.checkPassLimit(ctx, pass); shouldStop {
			return
		}

		r.emit(RuntimeEvent{
			Type:    EventTypeStatus,
			Message: fmt.Sprintf("Starting plan execution pass #%d.", pass),
			Level:   StatusLevelInfo,
		})

		plan, toolCall, err := r.requestPlan(ctx)
		if err != nil {
			r.handlePlanRequestError(ctx, err, pass)
			return
		}

		if plan == nil {
			r.handleNilPlanResponse(ctx, pass)
			return
		}

		execCount := r.recordPlanResponse(plan, toolCall)

		if shouldStop := r.handlePlanState(ctx, plan, toolCall, execCount, pass); shouldStop {
			return
		}

		r.executePendingCommands(ctx, toolCall)
		if ctx.Err() != nil {
			return
		}
	}
}

// checkPassLimit validates if the maximum pass limit has been reached.
// Returns true if execution should stop.
func (r *Runtime) checkPassLimit(ctx context.Context, pass int) bool {
	if r.options.MaxPasses > 0 && pass > r.options.MaxPasses {
		message := fmt.Sprintf("Maximum pass limit (%d) reached. Stopping execution.", r.options.MaxPasses)
		r.options.Logger.Warn(ctx, "Maximum pass limit reached",
			Field("max_passes", r.options.MaxPasses),
			Field("pass", pass),
		)
		r.emit(RuntimeEvent{
			Type:     EventTypeError,
			Message:  message,
			Level:    StatusLevelError,
			Metadata: map[string]any{"max_passes": r.options.MaxPasses, "pass": pass},
		})
		r.emitRequestInput("Pass limit reached. Provide additional guidance to continue.")
		if r.options.HandsFree {
			r.close()
		}
		return true
	}
	return false
}

// handlePlanRequestError handles errors during plan request.
func (r *Runtime) handlePlanRequestError(ctx context.Context, err error, pass int) {
	r.options.Logger.Error(ctx, "Failed to request plan from OpenAI", err,
		Field("pass", pass),
		Field("model", r.options.Model),
	)
	r.emit(RuntimeEvent{
		Type:    EventTypeError,
		Message: fmt.Sprintf("Failed to contact OpenAI (pass %d): %v", pass, err),
		Level:   StatusLevelError,
		Metadata: map[string]any{
			"pass":  pass,
			"error": err.Error(),
		},
	})
	r.emitRequestInput("You can provide another prompt.")
}

// handleNilPlanResponse handles the case when a nil plan is received.
func (r *Runtime) handleNilPlanResponse(ctx context.Context, pass int) {
	r.options.Logger.Error(ctx, "Received nil plan response", nil,
		Field("pass", pass),
	)
	r.emit(RuntimeEvent{
		Type:    EventTypeError,
		Message: "Received nil plan response.",
		Level:   StatusLevelError,
	})
	r.emitRequestInput("Unable to continue plan execution. Provide the next instruction.")
}

// handlePlanState processes the plan state and determines if execution should continue.
// Returns true if execution should stop.
func (r *Runtime) handlePlanState(ctx context.Context, plan *PlanResponse, toolCall ToolCall, execCount int, pass int) bool {
	if plan.RequireHumanInput {
		return r.handleHumanInputRequest(ctx, toolCall)
	}

	if execCount == 0 {
		return r.handleEmptyPlan(ctx, plan, pass)
	}

	return false
}

// handleHumanInputRequest handles when the assistant requests human input.
// Returns true to stop execution and wait for user input.
func (r *Runtime) handleHumanInputRequest(ctx context.Context, toolCall ToolCall) bool {
	r.appendToolObservation(toolCall, PlanObservationPayload{
		Summary: "Assistant requested additional input before continuing the plan.",
	})
	r.emitRequestInput("Assistant requested additional input before continuing.")
	return true
}

// handleEmptyPlan handles when the plan has no executable steps.
// Returns true if execution should stop.
func (r *Runtime) handleEmptyPlan(ctx context.Context, plan *PlanResponse, pass int) bool {
	r.appendToolObservation(ToolCall{}, PlanObservationPayload{
		Summary: "Assistant returned a plan without executable steps.",
	})
	r.emit(RuntimeEvent{
		Type:    EventTypeStatus,
		Message: "Plan has no executable steps.",
		Level:   StatusLevelInfo,
	})

	if r.options.HandsFree {
		summary := fmt.Sprintf("Hands-free session complete after %d pass(es); assistant reported no further work.", pass)
		if trimmed := strings.TrimSpace(plan.Message); trimmed != "" {
			summary = fmt.Sprintf("%s Summary: %s", summary, trimmed)
		}
		r.emit(RuntimeEvent{
			Type:    EventTypeStatus,
			Message: summary,
			Level:   StatusLevelInfo,
		})
		r.close()
		return true
	}

	r.emitRequestInput("Plan has no executable steps. Provide the next instruction.")
	return true
}
