package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyPatchUpdatesFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(target, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatalf("failed to seed file: %v", err)
	}

	run := "apply_patch\n*** Begin Patch\n*** Update File: notes.txt\n@@\n-alpha\n+gamma\n*** End Patch"
	step := PlanStep{ID: "step-1", Command: CommandDraft{Shell: agentShell, Run: run, Cwd: dir}}
	req := InternalCommandRequest{Name: applyPatchCommandName, Raw: run, Step: step}

	payload, err := newApplyPatchCommand()(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if payload.ExitCode == nil || *payload.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %+v", payload.ExitCode)
	}
	if !strings.Contains(payload.Stdout, "Success. Updated the following files:") {
		t.Fatalf("unexpected stdout: %q", payload.Stdout)
	}

	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("failed to read patched file: %v", err)
	}
	if got, want := string(content), "gamma\nbeta\n"; got != want {
		t.Fatalf("patched content mismatch: got %q want %q", got, want)
	}
}

func TestApplyPatchPreservesPermissions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := filepath.Join(dir, "script.sh")
	original := "#!/bin/sh\necho hi\n"
	if err := os.WriteFile(script, []byte(original), 0o755); err != nil {
		t.Fatalf("failed to seed executable script: %v", err)
	}

	// Capture the mode to ensure we keep the executable bit after patching.
	info, err := os.Stat(script)
	if err != nil {
		t.Fatalf("failed to stat script: %v", err)
	}

	run := strings.Join([]string{
		"apply_patch",
		"*** Begin Patch",
		"*** Update File: script.sh",
		"@@",
		"-echo hi",
		"+echo bye",
		"*** End Patch",
	}, "\n")

	step := PlanStep{ID: "step-perm", Command: CommandDraft{Shell: agentShell, Run: run, Cwd: dir}}
	req := InternalCommandRequest{Name: applyPatchCommandName, Raw: run, Step: step}

	payload, err := newApplyPatchCommand()(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if payload.ExitCode == nil || *payload.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %+v", payload.ExitCode)
	}

	updated, err := os.Stat(script)
	if err != nil {
		t.Fatalf("failed to stat updated script: %v", err)
	}
	if got, want := updated.Mode().Perm(), info.Mode().Perm(); got != want {
		t.Fatalf("script permissions changed: got %v want %v", got, want)
	}

	content, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("failed to read updated script: %v", err)
	}
	if got, want := string(content), strings.Replace(original, "echo hi", "echo bye", 1); got != want {
		t.Fatalf("script contents mismatch: got %q want %q", got, want)
	}
}

func TestApplyPatchAddsFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	run := "apply_patch\n*** Begin Patch\n*** Add File: fresh.txt\n@@\n+hello\n+world\n*** End Patch"
	step := PlanStep{ID: "step-2", Command: CommandDraft{Shell: agentShell, Run: run, Cwd: dir}}
	req := InternalCommandRequest{Name: applyPatchCommandName, Raw: run, Step: step}

	payload, err := newApplyPatchCommand()(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if payload.ExitCode == nil || *payload.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %+v", payload.ExitCode)
	}

	data, err := os.ReadFile(filepath.Join(dir, "fresh.txt"))
	if err != nil {
		t.Fatalf("failed to read new file: %v", err)
	}
	if got, want := string(data), "hello\nworld"; got != want {
		t.Fatalf("new file mismatch: got %q want %q", got, want)
	}
}

func TestApplyPatchWhitespaceOptions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	original := "package main\n\nfunc demo() {\n    fmt.Println(\"hi\")\n}\n"
	if err := os.WriteFile(source, []byte(original), 0o644); err != nil {
		t.Fatalf("failed to seed file: %v", err)
	}

	patchBody := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: main.go",
		"@@",
		"-    fmt.Println(  \"hi\"  )",
		"+    fmt.Println(\"hi!\")",
		"*** End Patch",
	}, "\n")

	runIgnore := "apply_patch\n" + patchBody
	stepIgnore := PlanStep{ID: "ignore", Command: CommandDraft{Shell: agentShell, Run: runIgnore, Cwd: dir}}
	reqIgnore := InternalCommandRequest{Name: applyPatchCommandName, Raw: runIgnore, Step: stepIgnore}
	if _, err := newApplyPatchCommand()(context.Background(), reqIgnore); err != nil {
		t.Fatalf("unexpected error when ignoring whitespace: %v", err)
	}

	updated, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("failed to read updated file: %v", err)
	}
	if !strings.Contains(string(updated), "fmt.Println(\"hi!\")") {
		t.Fatalf("whitespace-tolerant patch did not apply: %q", string(updated))
	}

	// Revert file for respect whitespace test.
	if err := os.WriteFile(source, []byte(original), 0o644); err != nil {
		t.Fatalf("failed to reset file: %v", err)
	}

	runRespect := "apply_patch --respect-whitespace\n" + patchBody
	stepRespect := PlanStep{ID: "respect", Command: CommandDraft{Shell: agentShell, Run: runRespect, Cwd: dir}}
	reqRespect := InternalCommandRequest{Name: applyPatchCommandName, Raw: runRespect, Step: stepRespect}

	payload, err := newApplyPatchCommand()(context.Background(), reqRespect)
	if err == nil {
		t.Fatalf("expected respect-whitespace to fail")
	}
	if payload.ExitCode == nil || *payload.ExitCode == 0 {
		t.Fatalf("expected non-zero exit code on failure")
	}
	if !strings.Contains(payload.Stderr, "Hunk not found") {
		t.Fatalf("stderr missing hunk message: %q", payload.Stderr)
	}
}
