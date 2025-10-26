package runtime

import "time"

// MessageRole enumerates the chat roles supported by the runtime.
type MessageRole string

const (
	RoleSystem    MessageRole = "system"
	RoleUser      MessageRole = "user"
	RoleAssistant MessageRole = "assistant"
	RoleTool      MessageRole = "tool"
)

// ChatMessage stores a single message exchanged with OpenAI.
type ChatMessage struct {
	Role       MessageRole
	Content    string
	ToolCallID string
	Name       string
	Timestamp  time.Time
	ToolCalls  []ToolCall
	Pass       int
	// Summarized marks messages that were synthesized by the compactor so we
	// avoid repeatedly summarizing the same entry.
	Summarized bool `json:"summarized,omitempty"`
}

// ToolCall stores metadata for an assistant tool invocation.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// CommandDraft replicates the shell command contract embedded in the plan schema.
type CommandDraft struct {
	Reason      string `json:"reason"`
	Shell       string `json:"shell"`
	Run         string `json:"run"`
	Cwd         string `json:"cwd"`
	TimeoutSec  int    `json:"timeout_sec"`
	FilterRegex string `json:"filter_regex"`
	TailLines   int    `json:"tail_lines"`
	MaxBytes    int    `json:"max_bytes"`
}

// PlanStatus represents execution status for a plan step.
type PlanStatus string

const (
	PlanPending   PlanStatus = "pending"
	PlanCompleted PlanStatus = "completed"
	PlanFailed    PlanStatus = "failed"
	PlanAbandoned PlanStatus = "abandoned"
)

// StepObservation summarizes the outcome for a specific plan step.
type StepObservation struct {
	ID        string     `json:"id"`
	Status    PlanStatus `json:"status"`
	Stdout    string     `json:"stdout,omitempty"`
	Stderr    string     `json:"stderr,omitempty"`
	ExitCode  *int       `json:"exit_code,omitempty"`
	Details   string     `json:"details,omitempty"`
	Truncated bool       `json:"truncated,omitempty"`
}

// PlanObservationPayload mirrors the JSON payload forwarded back to the model.
type PlanObservationPayload struct {
	PlanObservation         []StepObservation `json:"plan_observation,omitempty"`
	Stdout                  string            `json:"-"`
	Stderr                  string            `json:"-"`
	Truncated               bool              `json:"-"`
	ExitCode                *int              `json:"-"`
	JSONParseError          bool              `json:"json_parse_error,omitempty"`
	SchemaValidationError   bool              `json:"schema_validation_error,omitempty"`
	ResponseValidationError bool              `json:"response_validation_error,omitempty"`
	CanceledByHuman         bool              `json:"canceled_by_human,omitempty"`
	OperationCanceled       bool              `json:"operation_canceled,omitempty"`
	Summary                 string            `json:"summary,omitempty"`
	Details                 string            `json:"details,omitempty"`
}

// PlanObservation bundles the payload with optional metadata.
type PlanObservation struct {
	ObservationForLLM *PlanObservationPayload `json:"observation_for_llm,omitempty"`
}

// PlanStep describes an individual plan entry from OpenAI.
type PlanStep struct {
	ID           string           `json:"id"`
	Title        string           `json:"title"`
	Status       PlanStatus       `json:"status"`
	WaitingForID []string         `json:"waitingForId"`
	Command      CommandDraft     `json:"command"`
	Observation  *PlanObservation `json:"observation,omitempty"`
	Executing    bool             `json:"-"`
}

// PlanResponse captures the structured assistant output.
type PlanResponse struct {
	Message           string     `json:"message"`
	Reasoning         []string   `json:"reasoning,omitempty"`
	Plan              []PlanStep `json:"plan"`
	RequireHumanInput bool       `json:"requireHumanInput"`
}
