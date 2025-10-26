package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyPatchCommandUpdatesFile(t *testing.T) {
	dir := t.TempDir()
	prevDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("failed to chdir to temp dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(prevDir)
	})

	original := "hello\nworld\n"
	if err := os.WriteFile("file.txt", []byte(original), 0o644); err != nil {
		t.Fatalf("failed to write seed file: %v", err)
	}

	patch := "*** Begin Patch\n*** Update File: file.txt\n@@\n-hello\n+hello there\n*** End Patch\n"
	run := "apply_patch\n" + patch
	req := InternalCommandRequest{Step: PlanStep{Command: CommandDraft{Run: run}}}

	payload, err := applyPatchCommand(context.Background(), req)
	if err != nil {
		t.Fatalf("applyPatchCommand returned error: %v", err)
	}
	if payload.ExitCode == nil || *payload.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %v", payload.ExitCode)
	}
	if payload.Stdout == "" || payload.Stderr != "" {
		t.Fatalf("expected stdout message only, got stdout=%q stderr=%q", payload.Stdout, payload.Stderr)
	}

	data, err := os.ReadFile("file.txt")
	if err != nil {
		t.Fatalf("failed to read patched file: %v", err)
	}
	if string(data) != "hello there\nworld\n" {
		t.Fatalf("unexpected file content: %q", string(data))
	}
}

func TestApplyPatchCommandIgnoresWhitespace(t *testing.T) {
	dir := t.TempDir()
	prevDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("failed to chdir to temp dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(prevDir)
	})

	if err := os.WriteFile("space.go", []byte("value := 1\n"), 0o644); err != nil {
		t.Fatalf("failed to write seed file: %v", err)
	}

	patch := "*** Begin Patch\n*** Update File: space.go\n@@\n-value:=1\n+value := 2\n*** End Patch\n"
	run := "apply_patch --ignore-whitespace\n" + patch
	req := InternalCommandRequest{Step: PlanStep{Command: CommandDraft{Run: run}}}

	payload, err := applyPatchCommand(context.Background(), req)
	if err != nil {
		t.Fatalf("applyPatchCommand returned error: %v", err)
	}
	if payload.ExitCode == nil || *payload.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %v", payload.ExitCode)
	}

	data, err := os.ReadFile("space.go")
	if err != nil {
		t.Fatalf("failed to read patched file: %v", err)
	}
	if string(data) != "value := 2\n" {
		t.Fatalf("unexpected file content: %q", string(data))
	}
}

func TestApplyPatchCommandRespectsWhitespace(t *testing.T) {
	dir := t.TempDir()
	prevDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("failed to chdir to temp dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(prevDir)
	})

	if err := os.WriteFile("strict.txt", []byte("alpha  beta\n"), 0o644); err != nil {
		t.Fatalf("failed to write seed file: %v", err)
	}

	patch := "*** Begin Patch\n*** Update File: strict.txt\n@@\n-alpha beta\n+alpha beta gamma\n*** End Patch\n"
	run := "apply_patch --respect-whitespace\n" + patch
	req := InternalCommandRequest{Step: PlanStep{Command: CommandDraft{Run: run}}}

	payload, err := applyPatchCommand(context.Background(), req)
	if err == nil {
		t.Fatalf("expected error but got success with payload: %#v", payload)
	}
	if payload.ExitCode == nil || *payload.ExitCode == 0 {
		t.Fatalf("expected non-zero exit code, got %v", payload.ExitCode)
	}
	if !strings.Contains(payload.Stderr, "Hunk not found") {
		t.Fatalf("expected stderr to mention missing hunk, got %q", payload.Stderr)
	}

	data, err := os.ReadFile("strict.txt")
	if err != nil {
		t.Fatalf("failed to read strict.txt: %v", err)
	}
	if string(data) != "alpha  beta\n" {
		t.Fatalf("file content should remain unchanged, got %q", string(data))
	}
}

func TestApplyPatchCommandAddsFile(t *testing.T) {
	dir := t.TempDir()
	prevDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("failed to chdir to temp dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(prevDir)
	})

	patch := "*** Begin Patch\n*** Add File: new/file.txt\n@@\n+hello world\n*** End Patch\n"
	run := "apply_patch\n" + patch
	req := InternalCommandRequest{Step: PlanStep{Command: CommandDraft{Run: run}}}

	payload, err := applyPatchCommand(context.Background(), req)
	if err != nil {
		t.Fatalf("applyPatchCommand returned error: %v", err)
	}
	if payload.ExitCode == nil || *payload.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %v", payload.ExitCode)
	}

	data, err := os.ReadFile(filepath.Join("new", "file.txt"))
	if err != nil {
		t.Fatalf("failed to read added file: %v", err)
	}
	if string(data) != "hello world" {
		t.Fatalf("unexpected file content: %q", string(data))
	}
}
