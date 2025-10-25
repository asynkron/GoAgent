package runtime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Run starts the runtime loop and optionally bridges stdin/stdout to the
// respective channels so the binary is immediately useful in a terminal.
func (r *Runtime) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup

	if !r.options.DisableOutputForwarding {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.forwardOutputs(ctx)
		}()
	}

	if !r.options.DisableInputReader {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.consumeInput(ctx); err != nil {
				r.emit(RuntimeEvent{
					Type:    EventTypeError,
					Message: err.Error(),
					Level:   StatusLevelError,
				})
			}
		}()
	}

	err := r.loop(ctx)
	cancel()
	wg.Wait()

	return err
}

func (r *Runtime) loop(ctx context.Context) error {
	r.emit(RuntimeEvent{
		Type:    EventTypeStatus,
		Message: "Agent runtime started",
		Level:   StatusLevelInfo,
	})
	r.emit(RuntimeEvent{
		Type:    EventTypeRequestInput,
		Message: "Enter a prompt to begin.",
		Level:   StatusLevelInfo,
	})

	for {
		select {
		case <-ctx.Done():
			r.emit(RuntimeEvent{
				Type:    EventTypeStatus,
				Message: "Context cancelled. Shutting down runtime.",
				Level:   StatusLevelWarn,
			})
			r.close()
			return ctx.Err()
		case evt, ok := <-r.inputs:
			if !ok {
				r.close()
				return nil
			}
			if err := r.handleInput(ctx, evt); err != nil {
				r.emit(RuntimeEvent{
					Type:    EventTypeError,
					Message: err.Error(),
					Level:   StatusLevelError,
				})
				r.close()
				return err
			}
		}
	}
}

func (r *Runtime) handleInput(ctx context.Context, evt InputEvent) error {
	switch evt.Type {
	case InputTypePrompt:
		return r.handlePrompt(ctx, evt)
	case InputTypeCancel:
		r.emit(RuntimeEvent{
			Type:    EventTypeStatus,
			Message: fmt.Sprintf("Cancel requested: %s", strings.TrimSpace(evt.Reason)),
			Level:   StatusLevelWarn,
		})
		r.emit(RuntimeEvent{
			Type:    EventTypeRequestInput,
			Message: "Ready for the next instruction.",
			Level:   StatusLevelInfo,
		})
		return nil
	case InputTypeShutdown:
		r.emit(RuntimeEvent{
			Type:    EventTypeStatus,
			Message: "Shutdown requested. Goodbye!",
			Level:   StatusLevelInfo,
		})
		r.close()
		return errors.New("runtime shutdown requested")
	default:
		return fmt.Errorf("unknown input type: %s", evt.Type)
	}
}

func (r *Runtime) handlePrompt(ctx context.Context, evt InputEvent) error {
	prompt := strings.TrimSpace(evt.Prompt)
	if prompt == "" {
		r.emit(RuntimeEvent{
			Type:    EventTypeStatus,
			Message: "Ignoring empty prompt.",
			Level:   StatusLevelWarn,
		})
		r.emit(RuntimeEvent{
			Type:    EventTypeRequestInput,
			Message: "Awaiting a non-empty prompt.",
			Level:   StatusLevelInfo,
		})
		return nil
	}

	if !r.beginWork() {
		r.emit(RuntimeEvent{
			Type:    EventTypeStatus,
			Message: "Agent is already processing another prompt.",
			Level:   StatusLevelWarn,
		})
		return nil
	}
	defer r.endWork()

	r.emit(RuntimeEvent{
		Type:    EventTypeStatus,
		Message: fmt.Sprintf("Processing prompt with model %s…", r.options.Model),
		Level:   StatusLevelInfo,
	})

	userMessage := ChatMessage{Role: RoleUser, Content: prompt, Timestamp: time.Now()}
	r.appendHistory(userMessage)

	r.planExecutionLoop(ctx)

	return nil
}

func (r *Runtime) planExecutionLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		plan, toolCall, err := r.requestPlan(ctx)
		if err != nil {
			r.emit(RuntimeEvent{
				Type:    EventTypeError,
				Message: fmt.Sprintf("Failed to contact OpenAI: %v", err),
				Level:   StatusLevelError,
			})
			r.emit(RuntimeEvent{
				Type:    EventTypeRequestInput,
				Message: "You can provide another prompt.",
				Level:   StatusLevelInfo,
			})
			return
		}

		if plan == nil {
			r.emit(RuntimeEvent{
				Type:    EventTypeError,
				Message: "Received nil plan response.",
				Level:   StatusLevelError,
			})
			r.emit(RuntimeEvent{
				Type:    EventTypeRequestInput,
				Message: "Unable to continue plan execution. Provide the next instruction.",
				Level:   StatusLevelInfo,
			})
			return
		}

		execCount := r.recordPlanResponse(plan, toolCall)

		if plan.RequireHumanInput {
			// The assistant explicitly requested help from the human, so surface the
			// request and pause automated execution until the user responds.
			r.appendToolObservation(toolCall, PlanObservationPayload{
				Summary: "Assistant requested additional input before continuing the plan.",
			})
			r.emit(RuntimeEvent{
				Type:    EventTypeRequestInput,
				Message: "Assistant requested additional input before continuing.",
				Level:   StatusLevelInfo,
			})
			return
		}

		if execCount == 0 {
			r.appendToolObservation(toolCall, PlanObservationPayload{
				Summary: "Assistant returned a plan without executable steps.",
			})
			r.emit(RuntimeEvent{
				Type:    EventTypeStatus,
				Message: "Plan has no executable steps.",
				Level:   StatusLevelInfo,
			})
			r.emit(RuntimeEvent{
				Type:    EventTypeRequestInput,
				Message: "Plan has no executable steps. Provide the next instruction.",
				Level:   StatusLevelInfo,
			})
			return
		}

		r.executePendingCommands(ctx, toolCall)
		if ctx.Err() != nil {
			return
		}
	}
}

// requestPlan centralizes the logic for requesting a new plan from the assistant.
// It snapshots the history to guarantee a consistent view, forwards the request
// to the OpenAI client, and emits a status update so hosts can surface that a
// response was received.
func (r *Runtime) requestPlan(ctx context.Context) (*PlanResponse, ToolCall, error) {
	var retryCount int
	for {
		history := r.historySnapshot()

		r.writeHistoryLog(history)

		toolCall, err := r.client.RequestPlan(ctx, history)
		if err != nil {
			return nil, ToolCall{}, err
		}

		plan, retry, validationErr := r.validatePlanToolCall(toolCall)
		if validationErr != nil {
			return nil, ToolCall{}, validationErr
		}
		if retry {
			retryCount++
			delay := computeValidationBackoff(retryCount)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ToolCall{}, ctx.Err()
			}
			continue
		}

		retryCount = 0

		r.emit(RuntimeEvent{
			Type:    EventTypeStatus,
			Message: "Assistant response received.",
			Level:   StatusLevelInfo,
		})

		return plan, toolCall, nil
	}
}

func (r *Runtime) beginWork() bool {
	r.workMu.Lock()
	defer r.workMu.Unlock()
	if r.working {
		return false
	}
	r.working = true
	return true
}

func (r *Runtime) endWork() {
	r.workMu.Lock()
	r.working = false
	r.workMu.Unlock()
}

func (r *Runtime) isWorking() bool {
	r.workMu.Lock()
	defer r.workMu.Unlock()
	return r.working
}

func (r *Runtime) consumeInput(ctx context.Context) error {
	scanner := bufio.NewScanner(r.options.InputReader)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("failed to read input: %w", err)
			}
			r.Shutdown("stdin closed")
			return nil
		}

		line := strings.TrimSpace(scanner.Text())
		if r.isExitCommand(line) {
			r.Shutdown("exit command received")
			return nil
		}

		if strings.EqualFold(line, "cancel") {
			r.Cancel("user requested cancel")
			continue
		}

		r.SubmitPrompt(line)
	}
}

func (r *Runtime) forwardOutputs(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-r.outputs:
			if !ok {
				return
			}
			fmt.Fprintf(r.options.OutputWriter, "[%s] %s\n", evt.Type, evt.Message)
		}
	}
}

func (r *Runtime) isExitCommand(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return false
	}
	for _, candidate := range r.options.ExitCommands {
		if strings.EqualFold(trimmed, candidate) {
			return true
		}
	}
	return false
}
