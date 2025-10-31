// Package runtime implements the GoAgent runtime orchestration loop and configuration options.
package runtime

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RuntimeOptions configures the Go runtime wrapper. It mirrors the top level
// knobs exposed by the TypeScript runtime while keeping room for Go specific
// ergonomics like injecting alternative readers or writers during tests.
//
//revive:disable-next-line exported // Keep RuntimeOptions name for clarity across packages
type RuntimeOptions struct {
	APIKey              string
	APIBaseURL          string
	Model               string
	ReasoningEffort     string
	SystemPromptAugment string
	AmnesiaAfterPasses  int
	HandsFree           bool
	HandsFreeTopic      string
	// HandsFreeAutoReply holds a message that will be automatically
	// submitted as a user prompt whenever the runtime requests human input
	// while running in hands-free mode. When empty, no auto-reply is sent
	// and input requests are effectively ignored as before.
	HandsFreeAutoReply string
	MaxPasses          int
	// HistoryLogPath controls where the runtime persists the serialized
	// conversation history. A nil pointer defaults to "history.json" to
	// preserve the previous behaviour while allowing callers to override
	// or disable the log entirely.
	HistoryLogPath *string

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

	// UseStreaming enables SSE streaming for OpenAI responses. When true, the
	// runtime will stream assistant deltas to outputs and defer planning and
	// validation until the final chunk is received. Defaults to false to keep
	// existing test expectations unchanged.
	UseStreaming bool

	// EmitTimeout guards against blocking forever when no consumer drains the
	// output channel. Zero means wait indefinitely.
	EmitTimeout time.Duration

	// APIRetryConfig controls retry behavior for transient API failures.
	// If nil, no retries are attempted.
	APIRetryConfig *RetryConfig

	// HTTPTimeout sets the timeout for HTTP requests to the OpenAI API.
	// If zero, defaults to 120 seconds.
	HTTPTimeout time.Duration

	// ExitCommands are matched (case-insensitive) by the default input
	// reader to trigger a graceful shutdown.
	ExitCommands []string

	// InternalCommands registers agent scoped commands that bypass the host
	// shell. The key is the command name, matched case-insensitively.
	InternalCommands map[string]InternalCommandHandler

	// Logger provides structured logging. If nil, a NoOpLogger is used.
	Logger Logger
	// Metrics collects runtime metrics. If nil, a NoOpMetrics is used.
	Metrics Metrics
	// LogLevel sets the minimum log level when using the default logger.
	// Valid values: "DEBUG", "INFO", "WARN", "ERROR". Defaults to "INFO".
	LogLevel string
	// LogPath specifies a file path for logging. If set and Logger is nil,
	// logs will be written to this file. If empty and Logger is nil, logging
	// is disabled (NoOpLogger). This prevents logs from interfering with TUI.
	LogPath string
	// LogWriter allows specifying a custom writer for logs. If set, this takes
	// precedence over LogPath. If both are nil and Logger is nil, logging is disabled.
	LogWriter io.Writer
	// EnableMetrics enables metrics collection. When true and Metrics is nil,
	// an InMemoryMetrics instance is created automatically.
	EnableMetrics bool
}

// setDefaults applies reasonable defaults that match the behaviour of the
// TypeScript runtime while keeping Go specific knobs optional.
func (o *RuntimeOptions) setDefaults() {
	o.APIBaseURL = strings.TrimSpace(o.APIBaseURL)
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
	if o.HistoryLogPath == nil {
		defaultHistoryPath := "history.json"
		o.HistoryLogPath = &defaultHistoryPath
	}
	if o.HandsFree {
		o.HandsFreeTopic = strings.TrimSpace(o.HandsFreeTopic)
		if o.HandsFreeTopic == "" {
			o.HandsFreeTopic = "Hands-free session"
		}
	}
	// Default to streaming enabled so users see responses token-by-token unless explicitly disabled.
	// Tests that rely on non-streaming behavior should set UseStreaming: false.
	o.UseStreaming = true

	// Set up default logger if not provided
	if o.Logger == nil {
		var writer io.Writer

		// If LogWriter is specified, use it
		if o.LogWriter != nil {
			writer = o.LogWriter
		} else if strings.TrimSpace(o.LogPath) != "" {
			// If LogPath is specified, try to open/create the log file
			logPath := strings.TrimSpace(o.LogPath)
			// Create directory if needed
			dir := filepath.Dir(logPath)
			if dir != "." && dir != "" {
				_ = os.MkdirAll(dir, 0o755) // Ignore error, will fail on file open if dir can't be created
			}
			// Try to open the file for appending
			if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
				writer = f
				// Store the file handle so it can be closed later (will be set by runtime)
			}
			// If file open failed, silently fall back to NoOpLogger
		}
		// If no writer is configured, use NoOpLogger (default behavior)
		if writer == nil {
			o.Logger = &NoOpLogger{}
		} else {
			logLevel := LogLevelInfo
			switch strings.ToUpper(strings.TrimSpace(o.LogLevel)) {
			case "DEBUG":
				logLevel = LogLevelDebug
			case "INFO":
				logLevel = LogLevelInfo
			case "WARN":
				logLevel = LogLevelWarn
			case "ERROR":
				logLevel = LogLevelError
			}
			o.Logger = NewStdLogger(logLevel, writer)
		}
	}

	// Set up default metrics if enabled but not provided
	if o.EnableMetrics && o.Metrics == nil {
		o.Metrics = NewInMemoryMetrics()
	} else if o.Metrics == nil {
		o.Metrics = &NoOpMetrics{}
	}
}

// validate performs lightweight validation of user supplied options.
func (o *RuntimeOptions) validate() error {
	if o.APIKey == "" {
		return errors.New("OPENAI_API_KEY is required")
	}
	return nil
}
