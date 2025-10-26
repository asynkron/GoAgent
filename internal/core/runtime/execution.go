package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func filterCompletedSteps(steps []PlanStep) []PlanStep {
	if len(steps) == 0 {
		return steps
	}

	completedIDs := make(map[string]struct{})
	filtered := make([]PlanStep, 0, len(steps))
	for _, step := range steps {
		if step.Status == PlanCompleted {
			completedIDs[step.ID] = struct{}{}
			continue
		}
		filtered = append(filtered, step)
	}

	if len(completedIDs) == 0 {
		return filtered
	}

	for i := range filtered {
		deps := filtered[i].WaitingForID
		if len(deps) == 0 {
			continue
		}

		trimNeeded := false
		for _, dep := range deps {
			if _, done := completedIDs[dep]; done {
				trimNeeded = true
				break
			}
		}
		if !trimNeeded {
			continue
		}

		pruned := make([]string, 0, len(deps))
		for _, dep := range deps {
			if _, done := completedIDs[dep]; done {
				continue
			}
			pruned = append(pruned, dep)
		}

		if len(pruned) == 0 {
			filtered[i].WaitingForID = nil
			continue
		}

		filtered[i].WaitingForID = pruned
	}

	return filtered
}

func (r *Runtime) recordPlanResponse(plan *PlanResponse, toolCall ToolCall) int {
	assistantMessage := ChatMessage{
		Role:      RoleAssistant,
		Timestamp: time.Now(),
		ToolCalls: []ToolCall{toolCall},
	}
	r.appendHistory(assistantMessage)

	trimmedPlan := filterCompletedSteps(plan.Plan)
	r.plan.Replace(trimmedPlan)

	planMetadata := map[string]any{
		"plan":                trimmedPlan,
		"tool_call_id":        toolCall.ID,
		"tool_name":           toolCall.Name,
		"require_human_input": plan.RequireHumanInput,
	}
	if strings.TrimSpace(plan.Reasoning) != "" {
		planMetadata["reasoning"] = plan.Reasoning
	}

	r.emit(RuntimeEvent{
		Type:    EventTypeStatus,
		Message: fmt.Sprintf("Received plan with %d step(s).", len(trimmedPlan)),
		Level:   StatusLevelInfo,
		Metadata: map[string]any{
			"tool_call_id": toolCall.ID,
		},
	})

	r.emit(RuntimeEvent{
		Type:     EventTypeAssistantMessage,
		Message:  plan.Message,
		Level:    StatusLevelInfo,
		Metadata: planMetadata,
	})

	return r.plan.ExecutableCount()
}

func (r *Runtime) executePendingCommands(ctx context.Context, toolCall ToolCall) {
	r.commandMu.Lock()
	defer r.commandMu.Unlock()

	var (
		executedSteps   int
		lastStepID      string
		lastObservation PlanObservationPayload
		haveObservation bool
		finalErr        error
	)

	var orderedResults []StepObservation

	type stepExecutionResult struct {
		step        PlanStep
		observation PlanObservationPayload
		err         error
	}

	results := make(chan stepExecutionResult)
	executing := 0
	haltScheduling := false

	// scheduleReadySteps launches goroutines for every currently-ready step.
	scheduleReadySteps := func() bool {
		started := false
		if haltScheduling {
			return started
		}

		for ctx.Err() == nil {
			stepPtr, ok := r.plan.Ready()
			if !ok {
				break
			}

			step := *stepPtr
			started = true

			title := strings.TrimSpace(step.Title)
			if title == "" {
				title = step.ID
			}

			r.emit(RuntimeEvent{
				Type:    EventTypeStatus,
				Message: fmt.Sprintf("Executing step %s: %s", step.ID, title),
				Level:   StatusLevelInfo,
				Metadata: map[string]any{
					"step_id": step.ID,
					"title":   step.Title,
					"command": step.Command.Run,
					"shell":   step.Command.Shell,
					"cwd":     step.Command.Cwd,
				},
			})

			executing++

			go func(step PlanStep) {
				// Each worker reports its outcome so the main loop can
				// record results and schedule additional ready steps.
				observation, err := r.executor.Execute(ctx, step)
				results <- stepExecutionResult{step: step, observation: observation, err: err}
			}(step)
		}

		return started
	}

	for {
		if ctxErr := ctx.Err(); ctxErr != nil && finalErr == nil {
			finalErr = ctxErr
		}

		started := scheduleReadySteps()
		if executing == 0 {
			if !started {
				if !r.plan.HasPending() {
					r.emit(RuntimeEvent{
						Type:    EventTypeStatus,
						Message: "Plan execution completed.",
						Level:   StatusLevelInfo,
					})
				}
				break
			}
		}

		if executing == 0 {
			break
		}

		result := <-results
		executing--

		step := result.step
		observation := result.observation
		err := result.err

		executedSteps++
		lastStepID = step.ID

		status := PlanCompleted
		level := StatusLevelInfo
		message := fmt.Sprintf("Step %s completed successfully.", step.ID)
		if err != nil {
			status = PlanFailed
			level = StatusLevelError
			if observation.Details == "" {
				observation.Details = err.Error()
			}
			message = fmt.Sprintf("Step %s failed: %v", step.ID, err)
			if finalErr == nil {
				finalErr = err
			}
			haltScheduling = true
		}

		stepResult := StepObservation{
			ID:        step.ID,
			Status:    status,
			Stdout:    observation.Stdout,
			Stderr:    observation.Stderr,
			ExitCode:  observation.ExitCode,
			Details:   observation.Details,
			Truncated: observation.Truncated,
		}

		planObservation := &PlanObservation{ObservationForLLM: &PlanObservationPayload{
			PlanObservation: []StepObservation{stepResult},
		}}
		if updateErr := r.plan.UpdateStatus(step.ID, status, planObservation); updateErr != nil {
			r.emit(RuntimeEvent{
				Type:    EventTypeError,
				Message: fmt.Sprintf("Failed to update plan status for step %s: %v", step.ID, updateErr),
				Level:   StatusLevelError,
			})
			if finalErr == nil {
				finalErr = updateErr
			}
			haltScheduling = true
		}

		lastObservation = observation
		haveObservation = true
		orderedResults = append(orderedResults, stepResult)

		metadata := map[string]any{
			"step_id":   step.ID,
			"title":     step.Title,
			"status":    status,
			"stdout":    observation.Stdout,
			"stderr":    observation.Stderr,
			"truncated": observation.Truncated,
		}
		if observation.ExitCode != nil {
			metadata["exit_code"] = *observation.ExitCode
		}
		if observation.Details != "" {
			metadata["details"] = observation.Details
		}

		r.emit(RuntimeEvent{
			Type:     EventTypeStatus,
			Message:  message,
			Level:    level,
			Metadata: metadata,
		})
	}

	payload := PlanObservationPayload{PlanObservation: orderedResults}
	if haveObservation {
		payload.Stdout = lastObservation.Stdout
		payload.Stderr = lastObservation.Stderr
		payload.Truncated = lastObservation.Truncated
		payload.ExitCode = lastObservation.ExitCode
		payload.Details = lastObservation.Details
	}

	if payload.Summary == "" {
		switch {
		case executedSteps == 0 && finalErr != nil:
			payload.Summary = "Failed before executing plan steps."
		case executedSteps == 0:
			payload.Summary = "No plan steps were executed."
		case finalErr != nil:
			payload.Summary = fmt.Sprintf("Execution halted during step %s.", lastStepID)
		default:
			payload.Summary = fmt.Sprintf("Executed %d plan step(s).", executedSteps)
		}
	}

	r.appendToolObservation(toolCall, payload)
}

func (r *Runtime) appendToolObservation(toolCall ToolCall, payload PlanObservationPayload) {
	if toolCall.ID == "" {
		return
	}

	enforceObservationLimit(&payload)

	toolMessage, err := BuildToolMessage(payload)
	if err != nil {
		r.emit(RuntimeEvent{
			Type:    EventTypeError,
			Message: fmt.Sprintf("Failed to encode tool observation: %v", err),
			Level:   StatusLevelError,
		})
		return
	}

	r.appendHistory(ChatMessage{
		Role:       RoleTool,
		Content:    toolMessage,
		ToolCallID: toolCall.ID,
		Name:       toolCall.Name,
		Timestamp:  time.Now(),
	})
}
