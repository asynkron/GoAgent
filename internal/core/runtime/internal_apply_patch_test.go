package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyPatchCommandUpdatesFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "sample.txt")
	if err := os.WriteFile(file, []byte("first\nsecond\n"), 0o644); err != nil {
		t.Fatalf("failed to seed file: %v", err)
	}

	raw := "apply_patch\n*** Begin Patch\n*** Update File: sample.txt\n@@\n-second\n+updated\n*** End Patch\n"
	handler := newApplyPatchCommand()
	req := InternalCommandRequest{
		Name: "apply_patch",
		Raw:  raw,
		Step: PlanStep{Command: CommandDraft{Run: raw, Cwd: dir}},
	}

	payload, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(payload.Stdout, "sample.txt") {
		t.Fatalf("expected stdout to mention updated file, got %q", payload.Stdout)
	}

	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}
	if string(data) != "first\nupdated\n" {
		t.Fatalf("unexpected file content: %q", string(data))
	}
}

func TestApplyPatchCommandAddsFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	raw := "apply_patch\n*** Begin Patch\n*** Add File: new.txt\n@@\n+alpha\n+beta\n*** End Patch\n"
	handler := newApplyPatchCommand()
	req := InternalCommandRequest{
		Name: "apply_patch",
		Raw:  raw,
		Step: PlanStep{Command: CommandDraft{Run: raw, Cwd: dir}},
	}

	payload, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload.Stdout == "" {
		t.Fatalf("expected stdout summary, got empty string")
	}

	data, err := os.ReadFile(filepath.Join(dir, "new.txt"))
	if err != nil {
		t.Fatalf("failed to read new file: %v", err)
	}
	if string(data) != "alpha\nbeta" {
		t.Fatalf("unexpected file content: %q", string(data))
	}
}

func TestApplyPatchCommandWhitespaceOptions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "whitespace.txt")
	if err := os.WriteFile(file, []byte("foo bar\n"), 0o644); err != nil {
		t.Fatalf("failed to seed file: %v", err)
	}

	handler := newApplyPatchCommand()

	ignoreRaw := "apply_patch\n*** Begin Patch\n*** Update File: whitespace.txt\n@@\n-foo    bar\n+foo    baz\n*** End Patch\n"
	ignoreReq := InternalCommandRequest{
		Name: "apply_patch",
		Raw:  ignoreRaw,
		Step: PlanStep{Command: CommandDraft{Run: ignoreRaw, Cwd: dir}},
	}

	payload, err := handler(context.Background(), ignoreReq)
	if err != nil {
		t.Fatalf("unexpected error with default whitespace handling: %v", err)
	}
	if payload.Stdout == "" {
		t.Fatalf("expected summary output for ignored whitespace")
	}

	// Reset file content so the next patch only differs by whitespace.
	if err := os.WriteFile(file, []byte("foo baz\n"), 0o644); err != nil {
		t.Fatalf("failed to reset file before respect test: %v", err)
	}

	respectedRaw := "apply_patch --respect-whitespace\n*** Begin Patch\n*** Update File: whitespace.txt\n@@\n-foo    baz\n+foo	baz\n*** End Patch\n"
	respectReq := InternalCommandRequest{
		Name: "apply_patch",
		Raw:  respectedRaw,
		Step: PlanStep{Command: CommandDraft{Run: respectedRaw, Cwd: dir}},
	}

	failedPayload, err := handler(context.Background(), respectReq)
	if err == nil {
		t.Fatalf("expected error when respecting whitespace")
	}
	if failedPayload.ExitCode == nil || *failedPayload.ExitCode == 0 {
		t.Fatalf("expected non-zero exit code, got %v", failedPayload.ExitCode)
	}
	if !strings.Contains(failedPayload.Stderr, "Hunk not found") {
		t.Fatalf("expected stderr to describe missing hunk, got %q", failedPayload.Stderr)
	}
}
