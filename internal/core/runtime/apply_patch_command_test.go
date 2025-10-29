package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyPatchCommandIgnoreWhitespace(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "sample.txt")
	if err := os.WriteFile(filePath, []byte("value = 42\n"), 0o644); err != nil {
		t.Fatalf("failed to seed file: %v", err)
	}

	executor := NewCommandExecutor()
	if err := registerDefaultInternalCommands(executor); err != nil {
		t.Fatalf("failed to register default internal commands: %v", err)
	}

	run := "apply_patch <<'PATCH'\n*** Begin Patch\n*** Update File: sample.txt\n@@\n-value=42\n+value=43\n*** End Patch\nPATCH"
	step := PlanStep{ID: "apply", Command: CommandDraft{Shell: "agent", Run: run, Cwd: dir}}
	payload, err := executor.Execute(context.Background(), step)
	if err != nil {
		t.Fatalf("apply_patch failed: %v", err)
	}
	if payload.ExitCode == nil || *payload.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %v", payload.ExitCode)
	}
	if !strings.Contains(payload.Stdout, "Success. Updated the following files:") {
		t.Fatalf("unexpected stdout: %q", payload.Stdout)
	}
	content, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed to read patched file: %v", err)
	}
	if string(content) != "value=43\n" {
		t.Fatalf("unexpected file content: %q", string(content))
	}
}

func TestApplyPatchCommandRespectWhitespaceFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "format.txt")
	if err := os.WriteFile(filePath, []byte("value = 42\n"), 0o644); err != nil {
		t.Fatalf("failed to seed file: %v", err)
	}

	executor := NewCommandExecutor()
	if err := registerDefaultInternalCommands(executor); err != nil {
		t.Fatalf("failed to register default internal commands: %v", err)
	}

	run := "apply_patch --respect-whitespace <<'PATCH'\n*** Begin Patch\n*** Update File: format.txt\n@@\n-value=42\n+value=43\n*** End Patch\nPATCH"
	step := PlanStep{ID: "apply", Command: CommandDraft{Shell: "agent", Run: run, Cwd: dir}}
	payload, err := executor.Execute(context.Background(), step)
	if err == nil {
		t.Fatalf("expected failure, got success with payload %+v", payload)
	}
	if payload.ExitCode == nil || *payload.ExitCode == 0 {
		t.Fatalf("expected non-zero exit code, got %v", payload.ExitCode)
	}
	if !strings.Contains(payload.Stderr, "Hunk not found") {
		t.Fatalf("expected stderr to reference missing hunk, got: %q", payload.Stderr)
	}
	content, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed to read file after failure: %v", err)
	}
	if string(content) != "value = 42\n" {
		t.Fatalf("file should remain untouched, got %q", string(content))
	}
}

func TestApplyPatchCommandAddFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	executor := NewCommandExecutor()
	if err := registerDefaultInternalCommands(executor); err != nil {
		t.Fatalf("failed to register default internal commands: %v", err)
	}

	run := "apply_patch <<'PATCH'\n*** Begin Patch\n*** Add File: new.txt\n@@\n+hello world\n*** End Patch\nPATCH"
	step := PlanStep{ID: "add", Command: CommandDraft{Shell: "agent", Run: run, Cwd: dir}}
	payload, err := executor.Execute(context.Background(), step)
	if err != nil {
		t.Fatalf("apply_patch add failed: %v", err)
	}
	if payload.ExitCode == nil || *payload.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %v", payload.ExitCode)
	}
	content, err := os.ReadFile(filepath.Join(dir, "new.txt"))
	if err != nil {
		t.Fatalf("failed to read added file: %v", err)
	}
	if string(content) != "hello world" {
		t.Fatalf("unexpected file content: %q", string(content))
	}
}

func TestApplyPatchCommandMissingPatch(t *testing.T) {
	t.Parallel()

	executor := NewCommandExecutor()
	if err := registerDefaultInternalCommands(executor); err != nil {
		t.Fatalf("failed to register default internal commands: %v", err)
	}

	run := "apply_patch"
	step := PlanStep{ID: "noop", Command: CommandDraft{Shell: "agent", Run: run}}
	_, err := executor.Execute(context.Background(), step)
	if err == nil {
		t.Fatalf("expected error for missing patch input")
	}
	if !strings.Contains(err.Error(), "no patch provided") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestApplyPatchCommandHelp(t *testing.T) {
	t.Parallel()

	executor := NewCommandExecutor()
	if err := registerDefaultInternalCommands(executor); err != nil {
		t.Fatalf("failed to register default internal commands: %v", err)
	}

	run := "apply_patch --help"
	step := PlanStep{ID: "help", Command: CommandDraft{Shell: "agent", Run: run}}
	payload, err := executor.Execute(context.Background(), step)
	if err != nil {
		t.Fatalf("help invocation should succeed: %v", err)
	}
	if payload.ExitCode == nil || *payload.ExitCode != 0 {
		t.Fatalf("expected zero exit code, got %v", payload.ExitCode)
	}
	if !strings.Contains(payload.Stdout, "Usage: apply_patch") {
		t.Fatalf("unexpected usage output: %q", payload.Stdout)
	}
}
