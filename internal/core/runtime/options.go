package runtime

import (
	"errors"
	"io"
	"os"
	"strings"
	"time"
)

// RuntimeOptions configures the Go runtime wrapper. It mirrors the top level
// knobs exposed by the TypeScript runtime while keeping room for Go specific
// ergonomics like injecting alternative readers or writers during tests.
type RuntimeOptions struct {
	APIKey              string
	Model               string
	ReasoningEffort     string
	SystemPromptAugment string
	AmnesiaAfterPasses  int
	HandsFree           bool
	HandsFreeTopic      string
	MaxPasses           int

	// MaxContextTokens defines the soft cap for the conversation history. When
	// the estimated usage exceeds CompactWhenPercent of this value, older
	// messages are summarized to stay within the budget.
	MaxContextTokens int
	// CompactWhenPercent controls when the compactor kicks in. Values are in
	// the 0-1 range (e.g. 0.85 triggers when the history is ~85% full).
	CompactWhenPercent float64

	// InputBuffer controls the capacity of the input channel. The default is
	// tuned for interactive usage where only a handful of messages are
	// pending at any given time.
	InputBuffer int
	// OutputBuffer controls the capacity of the output channel.
	OutputBuffer int

	// InputReader allows swapping stdin during tests.
	InputReader io.Reader
	// OutputWriter can be redirected for tests or alternative hosts.
	OutputWriter io.Writer

	// DisableInputReader prevents Run from consuming stdin. Useful when the
	// host application pushes values into the Inputs queue directly.
	DisableInputReader bool
	// DisableOutputForwarding prevents Run from printing to stdout and is
	// helpful when the host listens to Outputs directly.
	DisableOutputForwarding bool

	// EmitTimeout guards against blocking forever when no consumer drains the
	// output channel. Zero means wait indefinitely.
	EmitTimeout time.Duration

	// ExitCommands are matched (case-insensitive) by the default input
	// reader to trigger a graceful shutdown.
	ExitCommands []string

	// InternalCommands registers agent scoped commands that bypass the host
	// shell. The key is the command name, matched case-insensitively.
	InternalCommands map[string]InternalCommandHandler
}

// setDefaults applies reasonable defaults that match the behaviour of the
// TypeScript runtime while keeping Go specific knobs optional.
func (o *RuntimeOptions) setDefaults() {
	if o.Model == "" {
		o.Model = "gpt-4.1"
	}

	if o.AmnesiaAfterPasses < 0 {
		o.AmnesiaAfterPasses = 0
	}
	if o.MaxPasses < 0 {
		o.MaxPasses = 0
	}
	if o.MaxContextTokens <= 0 || o.CompactWhenPercent <= 0 {
		if budget, ok := defaultModelContextBudgets[strings.ToLower(o.Model)]; ok {
			if o.MaxContextTokens <= 0 {
				o.MaxContextTokens = budget.MaxTokens
			}
			if o.CompactWhenPercent <= 0 {
				o.CompactWhenPercent = budget.CompactWhenPercent
			}
		}
	}
	if o.MaxContextTokens <= 0 {
		o.MaxContextTokens = 128000
	}
	if o.CompactWhenPercent <= 0 {
		o.CompactWhenPercent = 0.85
	}
	if o.InputBuffer <= 0 {
		o.InputBuffer = 4
	}
	if o.OutputBuffer <= 0 {
		o.OutputBuffer = 16
	}
	if o.InputReader == nil {
		o.InputReader = os.Stdin
	}
	if o.OutputWriter == nil {
		o.OutputWriter = os.Stdout
	}
	if len(o.ExitCommands) == 0 {
		o.ExitCommands = []string{"exit", "quit", "/exit", "/quit"}
	}
	if o.HandsFree && strings.TrimSpace(o.HandsFreeTopic) == "" {
		o.HandsFreeTopic = "Hands-free session"
	}
}

// validate performs lightweight validation of user supplied options.
func (o *RuntimeOptions) validate() error {
	if o.APIKey == "" {
		return errors.New("OPENAI_API_KEY is required")
	}
	return nil
}
