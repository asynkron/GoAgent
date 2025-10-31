// Package runtime implements the GoAgent runtime orchestration loop and command execution.
package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const maxObservationBytes = 50 * 1024

const agentShell = "openagent"

// InternalCommandHandler executes agent scoped commands that are not forwarded to the
// host shell. Implementations can inspect the parsed arguments and return a
// PlanObservationPayload describing the outcome.
type InternalCommandHandler func(ctx context.Context, req InternalCommandRequest) (PlanObservationPayload, error)

// InternalCommandRequest represents a parsed internal command invocation.
type InternalCommandRequest struct {
	// Name is the normalized command identifier.
	Name string
	// Raw contains the original run string after trimming whitespace.
	Raw string
	// Args stores named arguments (key=value pairs) parsed from the run string.
	Args map[string]any
	// Positionals stores ordered positional arguments parsed from the run string.
	Positionals []any
	// Step contains the original plan step for reference.
	Step PlanStep
}

// CommandExecutor runs shell commands described by plan steps and also supports
// a registry of agent internal commands that bypass the OS shell.
type CommandExecutor struct {
	internal map[string]InternalCommandHandler
}

// NewCommandExecutor builds the default executor that shells out using exec.CommandContext.
func NewCommandExecutor() *CommandExecutor {
	return &CommandExecutor{internal: make(map[string]InternalCommandHandler)}
}

// RegisterInternalCommand installs a handler for the provided command name. Names are
// matched case-insensitively and must be non-empty.
func (e *CommandExecutor) RegisterInternalCommand(name string, handler InternalCommandHandler) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return errors.New("internal command: name must be non-empty")
	}
	if handler == nil {
		return errors.New("internal command: handler must not be nil")
	}
	if e.internal == nil {
		e.internal = make(map[string]InternalCommandHandler)
	}
	e.internal[strings.ToLower(trimmed)] = handler
	return nil
}

// Execute runs the provided command and returns stdout/stderr observations.
func (e *CommandExecutor) Execute(ctx context.Context, step PlanStep) (PlanObservationPayload, error) {
	if strings.TrimSpace(step.Command.Shell) == "" || strings.TrimSpace(step.Command.Run) == "" {
		return PlanObservationPayload{}, fmt.Errorf("command: invalid shell or run for step %s", step.ID)
	}

	if strings.EqualFold(strings.TrimSpace(step.Command.Shell), agentShell) {
		return e.executeInternal(ctx, step)
	}

	// Derive a timeout-scoped context before building the command so the exec.Cmd
	// inherits the cancellation behavior directly.
	timeout := time.Duration(step.Command.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = time.Minute
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	execCmd, err := buildShellCommand(runCtx, step.Command.Shell, step.Command.Run)
	if err != nil {
		return PlanObservationPayload{}, fmt.Errorf("command: %w", err)
	}
	cmd := execCmd
	if step.Command.Cwd != "" {
		cmd.Dir = step.Command.Cwd
	}

	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	runErr := cmd.Run()
	// Preserve the previous timeout message while letting other context cancellations
	// bubble up naturally for the caller to inspect.
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		runErr = fmt.Errorf("command: timeout after %s", timeout)
	} else if err := runCtx.Err(); err != nil {
		runErr = err
	}

	stdout := stdoutBuf.Bytes()
	stderr := stderrBuf.Bytes()

	filteredStdout := applyFilter(stdout, step.Command.FilterRegex)
	filteredStderr := applyFilter(stderr, step.Command.FilterRegex)

	truncatedStdout, truncated := truncateOutput(filteredStdout, step.Command.MaxBytes, step.Command.TailLines)
	truncatedStderr, stderrTruncated := truncateOutput(filteredStderr, step.Command.MaxBytes, step.Command.TailLines)
	truncated = truncated || stderrTruncated

	observation := PlanObservationPayload{
		Stdout:    string(truncatedStdout),
		Stderr:    string(truncatedStderr),
		Truncated: truncated,
	}

	enforceObservationLimit(&observation)

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		code := exitErr.ExitCode()
		observation.ExitCode = &code
	} else if runErr == nil {
		zero := 0
		observation.ExitCode = &zero
	}

	if runErr != nil && exitErr == nil {
		observation.Details = runErr.Error()
	}

	// If the command failed, persist a detailed failure report for inspection.
	if runErr != nil {
		_ = writeFailureLog(step, stdout, stderr, runErr)
	}

	return observation, runErr
}

// writeFailureLog persists a diagnostic file under .goagent/ whenever a command
// fails. The log captures the run string and the full, unfiltered stdout/stderr.
// Any errors while writing the log are swallowed to avoid impacting the runtime.
func writeFailureLog(step PlanStep, fullStdout, fullStderr []byte, runErr error) error {
	// Resolve the base directory for logs. Prefer the step-specific Cwd when provided
	// so test invocations and sandboxed executions keep logs local to their workspace.
	baseDir := strings.TrimSpace(step.Command.Cwd)
	if baseDir == "" {
		if wd, err := os.Getwd(); err == nil {
			baseDir = wd
		} else {
			baseDir = "."
		}
	}

	// Ensure target directory exists relative to the resolved base directory.
	dir := filepath.Join(baseDir, ".goagent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	// Timestamped filename to avoid collisions.
	filename := fmt.Sprintf("failure-%s.txt", time.Now().Format("20060102-150405"))
	path := filepath.Join(dir, filename)

	// Compose a human-readable report. We intentionally include unfiltered,
	// untruncated outputs to aid debugging.
	var b bytes.Buffer
	_, _ = fmt.Fprintf(&b, "Timestamp: %s\n", time.Now().Format(time.RFC3339))
	_, _ = fmt.Fprintf(&b, "Shell: %s\n", step.Command.Shell)
	_, _ = fmt.Fprintf(&b, "Cwd: %s\n", step.Command.Cwd)
	_, _ = fmt.Fprintf(&b, "Run: %s\n", step.Command.Run)
	if step.Command.TimeoutSec > 0 {
		_, _ = fmt.Fprintf(&b, "TimeoutSec: %d\n", step.Command.TimeoutSec)
	}
	if step.Command.FilterRegex != "" {
		_, _ = fmt.Fprintf(&b, "FilterRegex: %s\n", step.Command.FilterRegex)
	}
	if step.Command.MaxBytes > 0 {
		_, _ = fmt.Fprintf(&b, "MaxBytes: %d\n", step.Command.MaxBytes)
	}
	if step.Command.TailLines > 0 {
		_, _ = fmt.Fprintf(&b, "TailLines: %d\n", step.Command.TailLines)
	}
	if runErr != nil {
		_, _ = fmt.Fprintf(&b, "Error: %v\n", runErr)
	}
	if step.ID != "" {
		_, _ = fmt.Fprintf(&b, "StepID: %s\n", step.ID)
	}
	_, _ = fmt.Fprintln(&b)
	_, _ = fmt.Fprintln(&b, "===== STDOUT (raw) =====")
	_, _ = b.Write(fullStdout)
	if len(fullStdout) > 0 && fullStdout[len(fullStdout)-1] != '\n' {
		_, _ = b.Write([]byte("\n"))
	}
	_, _ = fmt.Fprintln(&b, "===== STDERR (raw) =====")
	_, _ = b.Write(fullStderr)
	if len(fullStderr) > 0 && fullStderr[len(fullStderr)-1] != '\n' {
		_, _ = b.Write([]byte("\n"))
	}

	return os.WriteFile(path, b.Bytes(), 0o644)
}

func (e *CommandExecutor) executeInternal(ctx context.Context, step PlanStep) (PlanObservationPayload, error) {
	invocation, err := parseInternalInvocation(step)
	if err != nil {
		return PlanObservationPayload{}, fmt.Errorf("command: %w", err)
	}

	handler, ok := e.internal[invocation.Name]
	if !ok {
		return PlanObservationPayload{}, fmt.Errorf("command: unknown internal command %q", invocation.Name)
	}

	payload, execErr := handler(ctx, invocation)
	if execErr != nil {
		if payload.Details == "" {
			payload.Details = execErr.Error()
		}
		return payload, execErr
	}
	if payload.ExitCode == nil {
		zero := 0
		payload.ExitCode = &zero
	}
	return payload, nil
}

func parseInternalInvocation(step PlanStep) (InternalCommandRequest, error) {
	run := strings.TrimSpace(step.Command.Run)
	tokens, err := tokenizeInternalCommand(run)
	if err != nil {
		return InternalCommandRequest{}, err
	}
	if len(tokens) == 0 {
		return InternalCommandRequest{}, errors.New("internal command: missing command name")
	}

	name := strings.ToLower(tokens[0])
	args := make(map[string]any)
	var positionals []any
	for _, token := range tokens[1:] {
		key, value, found := strings.Cut(token, "=")
		if found && strings.TrimSpace(key) != "" {
			args[strings.TrimSpace(key)] = parseInternalValue(value)
			continue
		}
		positionals = append(positionals, parseInternalValue(token))
	}

	return InternalCommandRequest{
		Name:        name,
		Raw:         run,
		Args:        args,
		Positionals: positionals,
		Step:        step,
	}, nil
}

func tokenizeInternalCommand(input string) ([]string, error) {
	var (
		tokens  []string
		current strings.Builder
		quote   rune
		escape  bool
	)

	flush := func() {
		if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}

	for _, r := range input {
		switch {
		case escape:
			current.WriteRune(r)
			escape = false
		case r == '\\':
			escape = true
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			current.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
		case unicode.IsSpace(r):
			flush()
		default:
			current.WriteRune(r)
		}
	}

	if escape {
		return nil, errors.New("internal command: unfinished escape sequence")
	}
	if quote != 0 {
		return nil, errors.New("internal command: unmatched quote")
	}
	flush()
	return tokens, nil
}

func parseInternalValue(raw string) any {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}

	lower := strings.ToLower(trimmed)
	if lower == "true" {
		return true
	}
	if lower == "false" {
		return false
	}

	if i, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(trimmed, 64); err == nil {
		return f
	}

	return trimmed
}

func applyFilter(output []byte, pattern string) []byte {
	if pattern == "" {
		return output
	}
	rx, err := regexp.Compile(pattern)
	if err != nil {
		return output
	}
	lines := strings.Split(string(output), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if rx.MatchString(line) {
			kept = append(kept, line)
		}
	}
	return []byte(strings.Join(kept, "\n"))
}

func truncateOutput(output []byte, maxBytes, tailLines int) ([]byte, bool) {
	if len(output) == 0 {
		return output, false
	}
	truncated := false
	if maxBytes > 0 && len(output) > maxBytes {
		output = output[len(output)-maxBytes:]
		truncated = true
	}

	if tailLines <= 0 {
		return output, truncated
	}

	lines := bytes.Split(output, []byte("\n"))
	if len(lines) > tailLines {
		lines = lines[len(lines)-tailLines:]
		truncated = true
	}

	return bytes.Join(lines, []byte("\n")), truncated
}

func enforceObservationLimit(payload *PlanObservationPayload) {
	if payload == nil {
		return
	}

	trimBuffer := func(value string) (string, bool) {
		if len(value) <= maxObservationBytes {
			return value, false
		}
		return value[len(value)-maxObservationBytes:], true
	}

	if trimmed, truncated := trimBuffer(payload.Stdout); truncated {
		payload.Stdout = trimmed
		payload.Truncated = true
	}
	if trimmed, truncated := trimBuffer(payload.Stderr); truncated {
		payload.Stderr = trimmed
		payload.Truncated = true
	}

	for i := range payload.PlanObservation {
		entry := &payload.PlanObservation[i]
		if trimmed, truncated := trimBuffer(entry.Stdout); truncated {
			entry.Stdout = trimmed
			entry.Truncated = true
			payload.Truncated = true
		}
		if trimmed, truncated := trimBuffer(entry.Stderr); truncated {
			entry.Stderr = trimmed
			entry.Truncated = true
			payload.Truncated = true
		}
	}
}

// buildShellCommand normalizes the shell string ("/bin/bash", "bash -lc", etc.)
// before wiring it up with the user's command. Supporting embedded flags lets
// us accept both shorthand forms like "bash" and explicit "/bin/bash -lc" strings
// returned by the assistant without failing at exec time.
func buildShellCommand(ctx context.Context, shell, run string) (*exec.Cmd, error) {
	parts := strings.Fields(shell)
	if len(parts) == 0 {
		return nil, fmt.Errorf("invalid shell: %q", shell)
	}

	execPath := parts[0]
	args := parts[1:]
	if len(args) == 0 {
		args = append(args, "-lc")
	}

	args = append(args, run)
	return exec.CommandContext(ctx, execPath, args...), nil
}

// BuildToolMessage marshals the observation into a JSON string ready for tool messages.
func BuildToolMessage(observation PlanObservationPayload) (string, error) {
	buf := bytes.Buffer{}
	encoder := jsonEncoder(&buf)
	if err := encoder.Encode(observation); err != nil {
		return "", err
	}
	result := strings.TrimSpace(buf.String())
	if result == "" {
		result = "{}"
	}
	return result, nil
}

// jsonEncoder wraps json.NewEncoder to delay importing encoding/json in callers without needing generics.
func jsonEncoder(w io.Writer) *json.Encoder {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc
}
