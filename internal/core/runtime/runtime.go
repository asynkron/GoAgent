package runtime

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Runtime is the Go counterpart to the TypeScript AgentRuntime. It exposes two
// channels – Inputs and Outputs – that mirror the asynchronous queues used in
// the original implementation. Inputs receive InputEvents, Outputs surfaces
// RuntimeEvents.
type Runtime struct {
	options RuntimeOptions

	inputs  chan InputEvent
	outputs chan RuntimeEvent

	closeOnce sync.Once
	closed    chan struct{}

	plan      *PlanManager
	client    *OpenAIClient
	executor  *CommandExecutor
	commandMu sync.Mutex

	workMu  sync.Mutex
	working bool

	historyMu sync.RWMutex
	history   []ChatMessage

	passMu    sync.Mutex
	passCount int

	agentName string

	contextBudget ContextBudget
}

// NewRuntime configures a new runtime with the provided options.
func NewRuntime(options RuntimeOptions) (*Runtime, error) {
	options.setDefaults()
	if err := options.validate(); err != nil {
		return nil, err
	}

	client, err := NewOpenAIClient(options.APIKey, options.Model, options.ReasoningEffort, options.APIBaseURL, options.Logger, options.Metrics)
	if err != nil {
		return nil, err
	}

	initialHistory := []ChatMessage{{
		Role:      RoleSystem,
		Content:   buildSystemPrompt(options.SystemPromptAugment),
		Timestamp: time.Now(),
		Pass:      0,
	}}

	rt := &Runtime{
		options:       options,
		inputs:        make(chan InputEvent, options.InputBuffer),
		outputs:       make(chan RuntimeEvent, options.OutputBuffer),
		closed:        make(chan struct{}),
		plan:          NewPlanManager(),
		client:        client,
		history:       initialHistory,
		agentName:     "main",
		contextBudget: ContextBudget{MaxTokens: options.MaxContextTokens, CompactWhenPercent: options.CompactWhenPercent},
	}
	executor := NewCommandExecutor(options.Logger, options.Metrics)
	if err := registerBuiltinInternalCommands(rt, executor); err != nil {
		return nil, err
	}
	rt.executor = executor

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

// Outputs expose the outbound queue which delivers RuntimeEvents in order.
func (r *Runtime) Outputs() <-chan RuntimeEvent {
	return r.outputs
}

// SubmitPrompt is a convenience wrapper that enqueues a prompt input.
func (r *Runtime) SubmitPrompt(prompt string) {
	if r.isWorking() {
		r.emit(RuntimeEvent{
			Type:    EventTypeStatus,
			Message: "Agent is currently executing a plan. Please wait before submitting another prompt.",
			Level:   StatusLevelWarn,
		})
		return
	}
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

func (r *Runtime) queueHandsFreePrompt() {
	if !r.options.HandsFree {
		return
	}

	topic := strings.TrimSpace(r.options.HandsFreeTopic)
	if topic == "" {
		return
	}

	r.enqueue(InputEvent{Type: InputTypePrompt, Prompt: topic})
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

func (r *Runtime) emit(evt RuntimeEvent) {
	if evt.Pass == 0 {
		evt.Pass = r.currentPassCount()
	}
	if evt.Agent == "" {
		evt.Agent = r.agentName
	}

	select {
	case <-r.closed:
		return
	default:
	}

	if r.options.EmitTimeout <= 0 {
		// No timeout: block until sent or runtime is closed
		select {
		case r.outputs <- evt:
		case <-r.closed:
		}
		return
	}

	// With timeout: attempt to send with a deadline
	timer := time.NewTimer(r.options.EmitTimeout)
	defer timer.Stop()

	select {
	case r.outputs <- evt:
		// Successfully sent
	case <-timer.C:
		// Timeout: channel is full or consumer is blocked
		// Log warning and track metrics, but don't block the runtime
		r.options.Logger.Warn(context.Background(), "Event dropped: output channel full or consumer blocked",
			Field("event_type", evt.Type),
			Field("timeout_ms", r.options.EmitTimeout.Milliseconds()),
			Field("output_buffer_size", r.options.OutputBuffer),
		)
		r.options.Metrics.RecordDroppedEvent(string(evt.Type))
	case <-r.closed:
		// Runtime is shutting down
	}
}

func (r *Runtime) close() {
	r.closeOnce.Do(func() {
		close(r.closed)
		close(r.outputs)
	})
}

func (r *Runtime) currentPassCount() int {
	r.passMu.Lock()
	defer r.passMu.Unlock()
	return r.passCount
}

func (r *Runtime) resetPassCount() {
	r.passMu.Lock()
	r.passCount = 0
	r.passMu.Unlock()
}

// buildSystemPrompt is now implemented in system_prompt.go
