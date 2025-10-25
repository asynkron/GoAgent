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

	"github.com/asynkron/goagent/internal/core/schema"
	"github.com/xeipuuv/gojsonschema"
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

var (
	planSchemaLoader     gojsonschema.JSONLoader
	planSchemaLoaderErr  error
	planSchemaLoaderOnce sync.Once
)

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

	for name, handler := range options.InternalCommands {
		if err := rt.executor.RegisterInternalCommand(name, handler); err != nil {
			return nil, err
		}
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
	var retryCount int
	for {
		history := r.historySnapshot()

		_, toolCall, err := r.client.RequestPlan(ctx, history)
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
		return nil, false, err
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

func sanitizePlanForObservation(steps []PlanStep) []PlanStep {
	if len(steps) == 0 {
		return steps
	}

	sanitized := make([]PlanStep, len(steps))
	for i := range steps {
		step := steps[i]
		step.Command = CommandDraft{}
		if step.WaitingForID != nil {
			step.WaitingForID = append([]string{}, step.WaitingForID...)
		}
		if step.Observation != nil {
			obs := PlanObservation{}
			if step.Observation.ObservationForLLM != nil {
				payloadCopy := *step.Observation.ObservationForLLM
				enforceObservationLimit(&payloadCopy)
				if len(payloadCopy.Plan) > 0 {
					payloadCopy.Plan = sanitizePlanForObservation(payloadCopy.Plan)
				}
				obs.ObservationForLLM = &payloadCopy
			}
			step.Observation = &obs
		}
		sanitized[i] = step
	}
	return sanitized
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
		"plan":                plan.Plan,
		"tool_call_id":        toolCall.ID,
		"tool_name":           toolCall.Name,
		"require_human_input": plan.RequireHumanInput,
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

	var (
		executedSteps   int
		lastStepID      string
		lastObservation PlanObservationPayload
		haveObservation bool
		finalErr        error
	)

	for {
		if ctx.Err() != nil {
			finalErr = ctx.Err()
			break
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
			break
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
			finalErr = ctx.Err()
			break
		}

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
			finalErr = err
		}

		planObservation := &PlanObservation{ObservationForLLM: &observation}
		if updateErr := r.plan.UpdateStatus(step.ID, status, planObservation); updateErr != nil {
			r.emit(RuntimeEvent{
				Type:    EventTypeError,
				Message: fmt.Sprintf("Failed to update plan status for step %s: %v", step.ID, updateErr),
				Level:   StatusLevelError,
			})
			finalErr = updateErr
			break
		}

		observation.Plan = r.plan.SortOrder()
		lastObservation = observation
		haveObservation = true

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
			break
		}
	}

	payload := PlanObservationPayload{Plan: r.plan.SortOrder()}
	if haveObservation {
		payload = lastObservation
		payload.Plan = r.plan.SortOrder()
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

	if payload.Plan == nil {
		payload.Plan = r.plan.SortOrder()
	}

	payload.Plan = sanitizePlanForObservation(payload.Plan)
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
