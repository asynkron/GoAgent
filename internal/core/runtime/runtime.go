package runtime

import (
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

	once      sync.Once
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

	client, err := NewOpenAIClient(options.APIKey, options.Model, options.ReasoningEffort, options.APIBaseURL)
	if err != nil {
		return nil, err
	}

	initialHistory := []ChatMessage{{
		Role:      RoleSystem,
		Content:   buildSystemPrompt(options.SystemPromptAugment),
		Timestamp: time.Now(),
		Pass:      0,
	}}

	executor := NewCommandExecutor()
	if err := registerBuiltinInternalCommands(executor); err != nil {
		return nil, err
	}

	rt := &Runtime{
		options:       options,
		inputs:        make(chan InputEvent, options.InputBuffer),
		outputs:       make(chan RuntimeEvent, options.OutputBuffer),
		closed:        make(chan struct{}),
		plan:          NewPlanManager(),
		client:        client,
		executor:      executor,
		history:       initialHistory,
		agentName:     "main",
		contextBudget: ContextBudget{MaxTokens: options.MaxContextTokens, CompactWhenPercent: options.CompactWhenPercent},
	}

	if err := registerDefaultInternalCommands(rt.executor); err != nil {
		return nil, err
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

const baseSystemPrompt = `You are OpenAgent, an AI software engineer that plans and executes work.
Always respond by calling the "open-agent" function tool with arguments that conform to the provided JSON schema.
Explain your reasoning to the user in the "message" field and keep plans actionable, safe, and justified.

## planning
Only send a plan when you have a clear set of steps to achieve the user's goal, once the goal is reached. drop the plan.
If you are done with the plan, return an empty list of steps "plan":[].
Always send your full plan, all individual steps.
Remove any steps that are marked with status "completed"
When you receive a "plan_observation", understand that any "completed" step is done, you do not need to re-plan and send it again.
If your task is to run a command, once you know that task is completed, to not re-schedule to run the same command again, unless this is required to achieve the user's goal.
The plan is a Directed Acyclic Graph (DAG) of steps that can be executed in parallel when possible, do not assume order of independent steps.
If order is required, use the "waitingForID" field to create dependencies between steps.
Use the "requireHumanInput" field to pause execution and request additional input from the user when necessary.
Be concise and clear in your reasoning and plan steps.

## diagrams
Diagrams are drawn using Mermaid.js in markdown code blocks.
Always make sure to quote mermaid syntax correctly. eg.:
|"this is correct"|  vs, |this is not correct| vs, |""this is also not correct""|
["this is correct"]  vs, [this is not correct] vs, [""this is also not correct""]
Prefer LR orientation over TB/TD.

## working with temp files
Any temp-files created must be created under ".openagent" folder.

## accessing the web
Use local tools like wget or curl to access web resources.
pipe the output to a temp file and then read the file.

## executing commands
You can run commands via the plan, create a plan with a plan step, the plan step should have a command.
the "run" part of the command allows you to run shell commands.

## internal commands
### apply_patch
Use this command to apply unified-diff style patches via the internal executor.
- Set the plan step's command shell to "openagent" so the runtime routes the request to the internal handler instead of the OS shell.
- The payload sent in the plan step's "run" field must follow this shape:
'''
apply_patch [--respect-whitespace|--ignore-whitespace]
*** Begin Patch
*** Update File: relative/path/to/file.ext
@@
-previous line
+replacement line
*** End Patch
'''
- The first line is the command line. You may append flags such as '--respect-whitespace' (defaults to ignoring whitespace).
- After the command line, include a newline and wrap the patch body between '*** Begin Patch' and '*** End Patch'.
- Start each file block with either '*** Update File: <path>' for existing files or '*** Add File: <path>' for new files. Paths are resolved relative to the step's 'cwd'.
- Within each file block, include one or more hunks beginning with an '@@' header followed by diff lines that start with space, '+', or '-'.
- Example plan step payload (escaped for this Go string literal):
'''
{"id":"step-42","command":{"shell":"openagent","cwd":"/workspace/project","run":"apply_patch\n*** Begin Patch\n*** Update File: relative/path/to/file.ext\n@@\n-old line\n+new line\n*** End Patch"}}
'''
  The executor parses this JSON, notices the "openagent" shell, and forwards the run string to the apply_patch handler which consumes the embedded diff.

## execution environment and sandbox
You are not in a sandbox, you have full access to run any command.

## response format
The assistant response should be using markdown format.

## streaming behavior
When producing the JSON for the required function tool call, always start by
writing the "message" field first and stream it incrementally so hosts can
render it live. Keep appending to the same message string as you think; do not
wait to finalize the entire JSON before emitting the message. After the message
is underway, you may populate the other fields (reasoning, plan, etc.). Ensure
"message" is the first property in the JSON object.


`

func buildSystemPrompt(augment string) string {
	prompt := baseSystemPrompt
	if strings.TrimSpace(augment) != "" {
		prompt = prompt + "\n\nAdditional host instructions:\n" + strings.TrimSpace(augment)
	}
	return prompt
}
