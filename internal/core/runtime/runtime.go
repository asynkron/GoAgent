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

// Runtime is the Go counterpart to the TypeScript AgentRuntime. It exposes two
// channels – Inputs and Outputs – that mirror the asynchronous queues used in
// the original implementation. Inputs receives InputEvents, Outputs surfaces
// RuntimeEvents.
type Runtime struct {
	options RuntimeOptions

	inputs  chan InputEvent
	outputs chan RuntimeEvent

	once      sync.Once
	closeOnce sync.Once
	closed    chan struct{}

	plan      *PlanManager
	client    *OpenAIClient
	executor  *CommandExecutor
	commandMu sync.Mutex

	historyMu sync.RWMutex
	history   []ChatMessage
}

// NewRuntime configures a new runtime with the provided options.
func NewRuntime(options RuntimeOptions) (*Runtime, error) {
	options.setDefaults()
	if err := options.validate(); err != nil {
		return nil, err
	}

	client, err := NewOpenAIClient(options.APIKey, options.Model, options.ReasoningEffort)
	if err != nil {
		return nil, err
	}

	initialHistory := []ChatMessage{{
		Role:      RoleSystem,
		Content:   buildSystemPrompt(options.SystemPromptAugment),
		Timestamp: time.Now(),
	}}

	rt := &Runtime{
		options:  options,
		inputs:   make(chan InputEvent, options.InputBuffer),
		outputs:  make(chan RuntimeEvent, options.OutputBuffer),
		closed:   make(chan struct{}),
		plan:     NewPlanManager(),
		client:   client,
		executor: NewCommandExecutor(),
		history:  initialHistory,
	}

	return rt, nil
}

// Inputs exposes the inbound queue so hosts can push messages programmatically.
func (r *Runtime) Inputs() chan<- InputEvent {
	return r.inputs
}

// Outputs exposes the outbound queue which delivers RuntimeEvents in order.
func (r *Runtime) Outputs() <-chan RuntimeEvent {
	return r.outputs
}

// SubmitPrompt is a convenience wrapper that enqueues a prompt input.
func (r *Runtime) SubmitPrompt(prompt string) {
	r.enqueue(InputEvent{Type: InputTypePrompt, Prompt: prompt})
}

// Cancel enqueues a cancel request, mirroring the TypeScript runtime API.
func (r *Runtime) Cancel(reason string) {
	r.enqueue(InputEvent{Type: InputTypeCancel, Reason: reason})
}

// Shutdown requests a graceful shutdown of the runtime loop.
func (r *Runtime) Shutdown(reason string) {
	r.enqueue(InputEvent{Type: InputTypeShutdown, Reason: reason})
}

func (r *Runtime) enqueue(evt InputEvent) {
	select {
	case <-r.closed:
		return
	default:
	}

	select {
	case r.inputs <- evt:
	case <-r.closed:
	}
}

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

	r.emit(RuntimeEvent{
		Type:    EventTypeStatus,
		Message: fmt.Sprintf("Processing prompt with model %s…", r.options.Model),
		Level:   StatusLevelInfo,
	})

	userMessage := ChatMessage{Role: RoleUser, Content: prompt, Timestamp: time.Now()}
	r.appendHistory(userMessage)

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
		return nil
	}

	go r.planExecutionLoop(ctx, plan, toolCall)

	return nil
}

func (r *Runtime) planExecutionLoop(ctx context.Context, initialPlan *PlanResponse, initialToolCall ToolCall) {
	plan := initialPlan
	toolCall := initialToolCall

	for {
		if ctx.Err() != nil {
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
		if execCount == 0 {
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

		nextPlan, nextToolCall, err := r.requestPlan(ctx)
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

		plan = nextPlan
		toolCall = nextToolCall
	}
}

// requestPlan centralizes the logic for requesting a new plan from the assistant.
// It snapshots the history to guarantee a consistent view, forwards the request
// to the OpenAI client, and emits a status update so hosts can surface that a
// response was received.
func (r *Runtime) requestPlan(ctx context.Context) (*PlanResponse, ToolCall, error) {
	history := r.historySnapshot()

	plan, toolCall, err := r.client.RequestPlan(ctx, history)
	if err != nil {
		return nil, ToolCall{}, err
	}

	r.emit(RuntimeEvent{
		Type:    EventTypeStatus,
		Message: "Assistant response received.",
		Level:   StatusLevelInfo,
	})

	return plan, toolCall, nil
}

func (r *Runtime) recordPlanResponse(plan *PlanResponse, toolCall ToolCall) int {
	assistantMessage := ChatMessage{
		Role:      RoleAssistant,
		Timestamp: time.Now(),
		ToolCalls: []ToolCall{toolCall},
	}
	r.appendHistory(assistantMessage)

	r.plan.Replace(plan.Plan)

	planMetadata := map[string]any{
		"plan":         plan.Plan,
		"tool_call_id": toolCall.ID,
		"tool_name":    toolCall.Name,
	}

	r.emit(RuntimeEvent{
		Type:    EventTypeStatus,
		Message: fmt.Sprintf("Received plan with %d step(s).", len(plan.Plan)),
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

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		step, ok := r.plan.Ready()
		if !ok {
			if !r.plan.HasPending() {
				r.emit(RuntimeEvent{
					Type:    EventTypeStatus,
					Message: "Plan execution completed.",
					Level:   StatusLevelInfo,
				})
			}
			return
		}

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

		observation, err := r.executor.Execute(ctx, *step)
		if ctx.Err() != nil {
			return
		}

		status := PlanCompleted
		level := StatusLevelInfo
		message := fmt.Sprintf("Step %s completed successfully.", step.ID)
		if err != nil {
			status = PlanFailed
			level = StatusLevelError
			if observation.Details == "" && err != nil {
				observation.Details = err.Error()
			}
			message = fmt.Sprintf("Step %s failed: %v", step.ID, err)
		}

		planObservation := &PlanObservation{ObservationForLLM: &observation}
		if updateErr := r.plan.UpdateStatus(step.ID, status, planObservation); updateErr != nil {
			r.emit(RuntimeEvent{
				Type:    EventTypeError,
				Message: fmt.Sprintf("Failed to update plan status for step %s: %v", step.ID, updateErr),
				Level:   StatusLevelError,
			})
			return
		}

		observation.Plan = r.plan.SortOrder()

		if toolCall.ID != "" {
			if toolMessage, buildErr := BuildToolMessage(observation); buildErr != nil {
				r.emit(RuntimeEvent{
					Type:    EventTypeError,
					Message: fmt.Sprintf("Failed to encode tool observation for step %s: %v", step.ID, buildErr),
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

		if err != nil {
			return
		}
	}
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

func (r *Runtime) emit(evt RuntimeEvent) {
	select {
	case <-r.closed:
		return
	default:
	}

	if r.options.EmitTimeout <= 0 {
		select {
		case r.outputs <- evt:
		case <-r.closed:
		}
		return
	}

	timer := time.NewTimer(r.options.EmitTimeout)
	defer timer.Stop()

	select {
	case r.outputs <- evt:
	case <-timer.C:
	case <-r.closed:
	}
}

func (r *Runtime) close() {
	r.closeOnce.Do(func() {
		close(r.closed)
		close(r.outputs)
	})
}

func (r *Runtime) appendHistory(message ChatMessage) {
	r.historyMu.Lock()
	defer r.historyMu.Unlock()
	r.history = append(r.history, message)
}

func (r *Runtime) historySnapshot() []ChatMessage {
	r.historyMu.RLock()
	defer r.historyMu.RUnlock()
	copyHistory := make([]ChatMessage, len(r.history))
	copy(copyHistory, r.history)
	return copyHistory
}

const baseSystemPrompt = `You are OpenAgent, an AI software engineer that plans and executes work in a sandboxed environment.
Always respond by calling the "open-agent" function tool with arguments that conform to the provided JSON schema.
Explain your reasoning to the user in the "message" field and keep plans actionable, safe, and justified.`

func buildSystemPrompt(augment string) string {
	prompt := baseSystemPrompt
	if strings.TrimSpace(augment) != "" {
		prompt = prompt + "\n\nAdditional host instructions:\n" + strings.TrimSpace(augment)
	}
	return prompt
}
