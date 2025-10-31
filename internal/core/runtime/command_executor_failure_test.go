package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCommandFailureLogging verifies that when a shell command fails, the executor
// writes a failure-<timestamp>.txt file under the .goagent folder that contains
// the run command and captured stdout/stderr.
func TestCommandFailureLogging(t *testing.T) {
	t.Parallel()

	// Create an isolated temp working directory so logs don’t leak into the repo.
	tmp := t.TempDir()

	// Ensure no pre-existing .goagent files.
	goagentDir := filepath.Join(tmp, ".goagent")
	if err := os.MkdirAll(goagentDir, 0o755); err != nil {
		t.Fatalf("failed to create .goagent dir: %v", err)
	}
	// Clean up any files the runtime might have created earlier in this directory.
	// (Should be empty since tmp is fresh.)

	// Build a failing command. Use a portable approach: run 'bash -lc' with a known failing command.
	// We also emit some stdout/stderr to assert they are captured.
	step := PlanStep{
		ID:     "fail-step",
		Title:  "failing command",
		Status: "pending",
		Command: CommandDraft{
			Shell:      "bash -lc",
			Run:        "echo ok-on-stdout; echo err-on-stderr 1>&2; exit 3",
			Cwd:        tmp,
			TimeoutSec: 5,
			MaxBytes:   1 << 20,
			TailLines:  0,
		},
	}

	exec := NewCommandExecutor(nil, nil)

	// Execute and expect an error.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	obs, err := exec.Execute(ctx, step)
	if err == nil {
		t.Fatalf("expected error from failing command, got nil (obs=%+v)", obs)
	}

	// A small delay to allow any async file writes (implementation should be sync, but be generous).
	time.Sleep(50 * time.Millisecond)

	// Find a failure-*.txt file under .goagent.
	entries, err := os.ReadDir(goagentDir)
	if err != nil {
		t.Fatalf("failed to read .goagent dir: %v", err)
	}
	var failureFile string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "failure-") && strings.HasSuffix(name, ".txt") {
			// Ensure it’s recent (written within the last minute) to avoid false positives.
			info, statErr := e.Info()
			if statErr == nil && time.Since(info.ModTime()) < time.Minute {
				failureFile = filepath.Join(goagentDir, name)
				break
			}
		}
	}
	if failureFile == "" {
		t.Fatalf("expected a failure-*.txt to be written in %s", goagentDir)
	}

	content, err := os.ReadFile(failureFile)
	if err != nil {
		t.Fatalf("failed to read failure log %s: %v", failureFile, err)
	}
	body := string(content)

	// The log should include the original run string and captured stdout/stderr markers.
	if !strings.Contains(body, "Run:") || !strings.Contains(body, step.Command.Run) {
		t.Fatalf("failure log missing Run section or command; got:\n%s", body)
	}
	// The executor writes section headers in uppercase with explicit markers.
	if !strings.Contains(body, "===== STDOUT (raw) =====") || !strings.Contains(body, "ok-on-stdout") {
		t.Fatalf("failure log missing STDOUT section or expected stdout; got:\n%s", body)
	}
	if !strings.Contains(body, "===== STDERR (raw) =====") || !strings.Contains(body, "err-on-stderr") {
		t.Fatalf("failure log missing STDERR section or expected stderr; got:\n%s", body)
	}
}
