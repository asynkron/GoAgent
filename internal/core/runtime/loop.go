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
// respective channels, so the binary is immediately useful in a terminal.
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

	if r.options.HandsFree {
		r.queueHandsFreePrompt()
	}

	if !r.options.DisableInputReader && !r.options.HandsFree {
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
	traceID := generateTraceID()
	ctx = WithTraceID(ctx, traceID)
	r.options.Logger.Info(ctx, "Agent runtime started",
		Field("agent_name", r.agentName),
		Field("model", r.options.Model),
	)
	r.emit(RuntimeEvent{
		Type:    EventTypeStatus,
		Message: "Agent runtime started",
		Level:   StatusLevelInfo,
	})
	if !r.options.HandsFree {
		r.emitRequestInput("Enter a prompt to begin.")
	}

	for {
		select {
		case <-ctx.Done():
			r.options.Logger.Warn(ctx, "Context cancelled, shutting down runtime")
			r.emit(RuntimeEvent{
				Type:    EventTypeStatus,
				Message: "Context cancelled. Shutting down runtime.",
				Level:   StatusLevelWarn,
			})
			r.close()
			return ctx.Err()
		case <-r.closed:
			return nil
		case evt, ok := <-r.inputs:
			if !ok {
				r.close()
				return nil
			}
			if err := r.handleInput(ctx, evt); err != nil {
				r.options.Logger.Error(ctx, "Error handling input", err)
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
		r.emitRequestInput("Ready for the next instruction.")
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
		r.options.Logger.Warn(ctx, "Ignoring empty prompt")
		r.emit(RuntimeEvent{
			Type:    EventTypeStatus,
			Message: "Ignoring empty prompt.",
			Level:   StatusLevelWarn,
		})
		r.emitRequestInput("Awaiting a non-empty prompt.")
		return nil
	}

	if !r.beginWork() {
		r.options.Logger.Warn(ctx, "Agent is already processing another prompt")
		r.emit(RuntimeEvent{
			Type:    EventTypeStatus,
			Message: "Agent is already processing another prompt.",
			Level:   StatusLevelWarn,
		})
		return nil
	}
	defer r.endWork()

	r.resetPassCount()

	r.options.Logger.Info(ctx, "Processing user prompt",
		Field("prompt_length", len(prompt)),
	)

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

// planExecutionLoop is now implemented in plan_execution.go

// requestPlan centralizes the logic for requesting a new plan from the assistant.
// It snapshots the history to guarantee a consistent view, forwards the request
// to the OpenAI client, and emits a status update so hosts can surface that a
// response was received.
func (r *Runtime) requestPlan(ctx context.Context) (*PlanResponse, ToolCall, error) {
	var retryCount int
	for {
		history := r.planningHistorySnapshot()

		r.writeHistoryLog(history)

		var toolCall ToolCall
		var err error
		if r.options.UseStreaming {
			// Stream assistant response using the modern Responses API only.
			// Emit deltas as they arrive and accumulate them to emit a final
			// consolidated message when done.
			var finalBuilder strings.Builder
			streamFn := func(s string) {
				// Do not trim whitespace: models can stream newlines or spaces
				// as separate deltas for formatting. Only skip truly empty.
				if s == "" {
					return
				}
				finalBuilder.WriteString(s)
				r.emit(RuntimeEvent{Type: EventTypeAssistantDelta, Message: s})
			}

			toolCall, err = r.client.RequestPlanStreamingResponses(ctx, history, streamFn)
			// After streaming completes (no error), emit a final assistant message
			// with the consolidated content so hosts that don't handle deltas can
			// still present the assistant's reply.
			if err == nil {
				consolidated := strings.TrimSpace(finalBuilder.String())
				if consolidated != "" {
					r.emit(RuntimeEvent{Type: EventTypeAssistantMessage, Message: consolidated})
				}
			}
		} else {
			// Non-streaming path preserves historical behavior expected by tests.
			toolCall, err = r.client.RequestPlan(ctx, history)
		}
		if err != nil {
			r.options.Logger.Error(ctx, "Failed to request plan from OpenAI", err)
			return nil, ToolCall{}, fmt.Errorf("requestPlan: API request failed: %w", err)
		}

		plan, retry, validationErr := r.validatePlanToolCall(toolCall)
		if validationErr != nil {
			r.options.Logger.Error(ctx, "Plan validation failed", validationErr,
				Field("tool_call_id", toolCall.ID),
			)
			return nil, ToolCall{}, fmt.Errorf("requestPlan: validation failed: %w", validationErr)
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

		// reset unnecessary before return

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

// incrementPassCount increments the session pass counter and returns the latest total.
func (r *Runtime) incrementPassCount() int {
	r.passMu.Lock()
	defer r.passMu.Unlock()
	r.passCount++
	return r.passCount
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
			_, _ = fmt.Fprintf(r.options.OutputWriter, "[%s] %s\n", evt.Type, evt.Message)
		}
	}
}

func (r *Runtime) emitRequestInput(message string) {
	if r.options.HandsFree {
		// In hands-free mode, optionally auto-respond with a configured
		// message to keep execution going without human intervention.
		reply := strings.TrimSpace(r.options.HandsFreeAutoReply)
		if reply != "" {
			// Enqueue a synthetic user prompt to continue the session.
			r.enqueue(InputEvent{Type: InputTypePrompt, Prompt: reply})
		}
		return
	}
	r.emit(RuntimeEvent{
		Type:    EventTypeRequestInput,
		Message: message,
		Level:   StatusLevelInfo,
	})
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
