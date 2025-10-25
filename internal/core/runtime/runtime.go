package runtime

import (
	"bufio"
	"context"
	"encoding/json"
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

	plan     *PlanManager
	client   *OpenAIClient
	executor *CommandExecutor

	historyMu sync.RWMutex
	history   []ChatMessage

	executingMu sync.Mutex
	executing   map[string]struct{}

	commandTrigger chan struct{}

	toolCallMu      sync.RWMutex
	currentToolCall *ToolCall
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
		options:        options,
		inputs:         make(chan InputEvent, options.InputBuffer),
		outputs:        make(chan RuntimeEvent, options.OutputBuffer),
		closed:         make(chan struct{}),
		plan:           NewPlanManager(),
		client:         client,
		executor:       NewCommandExecutor(),
		history:        initialHistory,
		executing:      make(map[string]struct{}),
		commandTrigger: make(chan struct{}, 1),
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

	wg.Add(1)
	go func() {
		defer wg.Done()
		r.commandWorker(ctx)
	}()

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

	plan, toolCall, err := r.client.RequestPlan(ctx, r.historySnapshot())
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

	assistantMessage := ChatMessage{
		Role:      RoleAssistant,
		Timestamp: time.Now(),
		ToolCalls: []ToolCall{toolCall},
	}
	r.appendHistory(assistantMessage)

	planBytes, err := json.Marshal(plan)
	if err != nil {
		r.emit(RuntimeEvent{
			Type:    EventTypeError,
			Message: fmt.Sprintf("Failed to encode plan: %v", err),
			Level:   StatusLevelError,
		})
	} else {
		toolMessage := ChatMessage{
			Role:       RoleTool,
			Content:    string(planBytes),
			ToolCallID: toolCall.ID,
			Name:       toolCall.Name,
			Timestamp:  time.Now(),
		}
		r.appendHistory(toolMessage)
	}

	r.setCurrentToolCall(toolCall)
	r.plan.Replace(plan.Plan)
	r.triggerCommandWorker()

	planMetadata := map[string]any{
		"plan":         plan.Plan,
		"tool_call_id": toolCall.ID,
		"tool_name":    toolCall.Name,
	}
	planMetadata["auto_execute"] = len(plan.Plan) > 0

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

	r.emit(RuntimeEvent{
		Type:    EventTypeRequestInput,
		Message: "Awaiting the next instruction.",
		Level:   StatusLevelInfo,
	})

	return nil
}

// commandWorker waits for plan execution signals and runs ready steps.
func (r *Runtime) commandWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.closed:
			return
		case <-r.commandTrigger:
			r.processReadySteps(ctx)
		}
	}
}

// processReadySteps drains the queue of ready plan steps and executes them sequentially.
func (r *Runtime) processReadySteps(ctx context.Context) {
	for {
		if err := ctx.Err(); err != nil {
			return
		}

		step, ok := r.plan.Ready()
		if !ok {
			return
		}

		if !r.markExecuting(step.ID) {
			continue
		}

		r.executePlanStep(ctx, step)
	}
}

// executePlanStep runs a single plan step and records the observation metadata.
func (r *Runtime) executePlanStep(ctx context.Context, step *PlanStep) {
	defer r.finishExecuting(step.ID)

	if err := ctx.Err(); err != nil {
		return
	}

	command := strings.TrimSpace(step.Command.Run)
	shell := strings.TrimSpace(step.Command.Shell)

	baseMetadata := map[string]any{
		"step_id": step.ID,
		"title":   step.Title,
		"command": command,
		"shell":   shell,
	}
	if len(step.WaitingForID) > 0 {
		baseMetadata["waiting_for"] = append([]string{}, step.WaitingForID...)
	}
	if reason := strings.TrimSpace(step.Command.Reason); reason != "" {
		baseMetadata["reason"] = reason
	}
	if cwd := strings.TrimSpace(step.Command.Cwd); cwd != "" {
		baseMetadata["cwd"] = cwd
	}

	startMetadata := make(map[string]any, len(baseMetadata))
	for k, v := range baseMetadata {
		startMetadata[k] = v
	}

	r.emit(RuntimeEvent{
		Type:     EventTypeStatus,
		Message:  fmt.Sprintf("Executing plan step %s…", step.ID),
		Level:    StatusLevelInfo,
		Metadata: startMetadata,
	})

	observation, err := r.executor.Execute(ctx, *step)
	status := PlanCompleted
	message := fmt.Sprintf("Plan step %s completed successfully.", step.ID)
	level := StatusLevelInfo
	eventType := EventTypeStatus

	if err != nil {
		status = PlanFailed
		message = fmt.Sprintf("Plan step %s failed: %v", step.ID, err)
		level = StatusLevelError
		eventType = EventTypeError
	}

	resultMetadata := make(map[string]any, len(baseMetadata)+4)
	for k, v := range baseMetadata {
		resultMetadata[k] = v
	}
	if observation.ExitCode != nil {
		resultMetadata["exit_code"] = *observation.ExitCode
	}
	resultMetadata["stdout"] = observation.Stdout
	resultMetadata["stderr"] = observation.Stderr
	resultMetadata["truncated"] = observation.Truncated
	if observation.Details != "" {
		resultMetadata["details"] = observation.Details
	}

	planObservation := &PlanObservation{ObservationForLLM: &observation}
	if updateErr := r.plan.UpdateStatus(step.ID, status, planObservation); updateErr != nil {
		r.emit(RuntimeEvent{
			Type:    EventTypeError,
			Message: fmt.Sprintf("Failed to update plan status for %s: %v", step.ID, updateErr),
			Level:   StatusLevelError,
		})
		return
	}

	observation.Plan = r.plan.Snapshot()
	r.appendObservationToHistory(observation)

	r.emit(RuntimeEvent{
		Type:     eventType,
		Message:  message,
		Level:    level,
		Metadata: resultMetadata,
	})

	if !r.plan.HasPending() {
		var summary string
		if r.plan.Completed() {
			summary = "Plan execution completed successfully."
		} else {
			summary = "Plan execution finished with issues. Review the observations to decide the next action."
		}
		r.emit(RuntimeEvent{
			Type:    EventTypeStatus,
			Message: summary,
			Level:   StatusLevelInfo,
		})
		r.emit(RuntimeEvent{
			Type:    EventTypeRequestInput,
			Message: "Ready for the next instruction.",
			Level:   StatusLevelInfo,
		})
	}
}

// markExecuting guards against running the same plan step concurrently.
func (r *Runtime) markExecuting(id string) bool {
	r.executingMu.Lock()
	defer r.executingMu.Unlock()

	if _, exists := r.executing[id]; exists {
		return false
	}
	r.executing[id] = struct{}{}
	return true
}

func (r *Runtime) finishExecuting(id string) {
	r.executingMu.Lock()
	delete(r.executing, id)
	r.executingMu.Unlock()
}

// triggerCommandWorker notifies the worker that ready steps may be available.
func (r *Runtime) triggerCommandWorker() {
	select {
	case <-r.closed:
		return
	default:
	}

	select {
	case r.commandTrigger <- struct{}{}:
	default:
	}
}

// setCurrentToolCall stores the latest tool call metadata so observations can be threaded correctly.
func (r *Runtime) setCurrentToolCall(call ToolCall) {
	r.toolCallMu.Lock()
	defer r.toolCallMu.Unlock()

	copy := call
	r.currentToolCall = &copy
}

func (r *Runtime) currentToolCallSnapshot() (ToolCall, bool) {
	r.toolCallMu.RLock()
	defer r.toolCallMu.RUnlock()

	if r.currentToolCall == nil {
		return ToolCall{}, false
	}
	return *r.currentToolCall, true
}

// appendObservationToHistory records command output as a tool response for the next model turn.
func (r *Runtime) appendObservationToHistory(observation PlanObservationPayload) {
	call, ok := r.currentToolCallSnapshot()
	if !ok {
		return
	}

	message, err := BuildToolMessage(observation)
	if err != nil {
		r.emit(RuntimeEvent{
			Type:    EventTypeError,
			Message: fmt.Sprintf("Failed to encode observation payload: %v", err),
			Level:   StatusLevelError,
		})
		return
	}

	r.appendHistory(ChatMessage{
		Role:       RoleTool,
		Content:    message,
		ToolCallID: call.ID,
		Name:       call.Name,
		Timestamp:  time.Now(),
	})
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
