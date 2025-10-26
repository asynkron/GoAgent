package runtime

import (
	"context"
	"errors"
	"io/fs"
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

func TestApplyPatchRestoresSpecialBits(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	binary := filepath.Join(dir, "setuid-bin")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\necho special\n"), 0o755); err != nil {
		t.Fatalf("failed to seed binary: %v", err)
	}
	// Apply a setuid bit that we expect to persist across patching.
	if err := os.Chmod(binary, 0o755|fs.ModeSetuid); err != nil {
		t.Fatalf("failed to mark binary setuid: %v", err)
	}

	run := strings.Join([]string{
		"apply_patch",
		"*** Begin Patch",
		"*** Update File: setuid-bin",
		"@@",
		"-echo special",
		"+echo restored",
		"*** End Patch",
	}, "\n")

	step := PlanStep{ID: "step-special", Command: CommandDraft{Shell: agentShell, Run: run, Cwd: dir}}
	req := InternalCommandRequest{Name: applyPatchCommandName, Raw: run, Step: step}

	payload, err := newApplyPatchCommand()(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if payload.ExitCode == nil || *payload.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %+v", payload.ExitCode)
	}

	info, err := os.Stat(binary)
	if err != nil {
		t.Fatalf("failed to stat patched binary: %v", err)
	}
	if info.Mode()&fs.ModeSetuid == 0 {
		t.Fatalf("setuid bit was not restored: mode=%v", info.Mode())
	}

	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatalf("failed to read patched binary: %v", err)
	}
	if got, want := string(data), "#!/bin/sh\necho restored\n"; got != want {
		t.Fatalf("patched binary mismatch: got %q want %q", got, want)
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
	if got, want := string(data), "hello\nworld\n"; got != want {
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

func TestApplyPatchDeletesFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "obsolete.txt")
	if err := os.WriteFile(target, []byte("gone soon\n"), 0o644); err != nil {
		t.Fatalf("failed to seed file: %v", err)
	}

	run := strings.Join([]string{
		"apply_patch",
		"*** Begin Patch",
		"*** Delete File: obsolete.txt",
		"*** End Patch",
	}, "\n")

	step := PlanStep{ID: "delete", Command: CommandDraft{Shell: agentShell, Run: run, Cwd: dir}}
	req := InternalCommandRequest{Name: applyPatchCommandName, Raw: run, Step: step}

	payload, err := newApplyPatchCommand()(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload.ExitCode == nil || *payload.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %+v", payload.ExitCode)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected file to be deleted, stat err=%v", err)
	}
	if !strings.Contains(payload.Stdout, "D obsolete.txt") {
		t.Fatalf("stdout missing delete summary: %q", payload.Stdout)
	}
}

func TestApplyPatchDeleteMissingFileFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	run := strings.Join([]string{
		"apply_patch",
		"*** Begin Patch",
		"*** Delete File: missing.txt",
		"*** End Patch",
	}, "\n")

	step := PlanStep{ID: "delete-missing", Command: CommandDraft{Shell: agentShell, Run: run, Cwd: dir}}
	req := InternalCommandRequest{Name: applyPatchCommandName, Raw: run, Step: step}

	payload, err := newApplyPatchCommand()(context.Background(), req)
	if err == nil {
		t.Fatalf("expected error when deleting missing file")
	}
	if payload.ExitCode == nil || *payload.ExitCode == 0 {
		t.Fatalf("expected non-zero exit code")
	}
	if got, want := payload.Stderr, "Failed to delete file missing.txt"; !strings.Contains(got, want) {
		t.Fatalf("stderr mismatch: got %q want substring %q", got, want)
	}
}

func TestApplyPatchMovesFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	original := filepath.Join(dir, "old", "name.txt")
	if err := os.MkdirAll(filepath.Dir(original), 0o755); err != nil {
		t.Fatalf("failed to create directory: %v", err)
	}
	if err := os.WriteFile(original, []byte("old content\n"), 0o644); err != nil {
		t.Fatalf("failed to seed file: %v", err)
	}

	run := strings.Join([]string{
		"apply_patch",
		"*** Begin Patch",
		"*** Update File: old/name.txt",
		"*** Move to: renamed/dir/name.txt",
		"@@",
		"-old content",
		"+new content",
		"*** End Patch",
	}, "\n")

	step := PlanStep{ID: "move", Command: CommandDraft{Shell: agentShell, Run: run, Cwd: dir}}
	req := InternalCommandRequest{Name: applyPatchCommandName, Raw: run, Step: step}

	payload, err := newApplyPatchCommand()(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload.ExitCode == nil || *payload.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %+v", payload.ExitCode)
	}
	if strings.Contains(payload.Stdout, "old/name.txt") {
		t.Fatalf("stdout reported old path: %q", payload.Stdout)
	}
	if !strings.Contains(payload.Stdout, "M renamed/dir/name.txt") {
		t.Fatalf("stdout missing move summary: %q", payload.Stdout)
	}
	if _, err := os.Stat(original); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected original file removed, stat err=%v", err)
	}
	moved := filepath.Join(dir, "renamed", "dir", "name.txt")
	data, err := os.ReadFile(moved)
	if err != nil {
		t.Fatalf("failed to read moved file: %v", err)
	}
	if got, want := string(data), "new content\n"; got != want {
		t.Fatalf("moved file content mismatch: got %q want %q", got, want)
	}
}

func TestApplyPatchAddOverwritesExistingFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "duplicate.txt")
	if err := os.WriteFile(target, []byte("old content\n"), 0o644); err != nil {
		t.Fatalf("failed to seed file: %v", err)
	}

	run := strings.Join([]string{
		"apply_patch",
		"*** Begin Patch",
		"*** Add File: duplicate.txt",
		"+new content",
		"*** End Patch",
	}, "\n")

	step := PlanStep{ID: "overwrite", Command: CommandDraft{Shell: agentShell, Run: run, Cwd: dir}}
	req := InternalCommandRequest{Name: applyPatchCommandName, Raw: run, Step: step}

	payload, err := newApplyPatchCommand()(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload.ExitCode == nil || *payload.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %+v", payload.ExitCode)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("failed to read overwritten file: %v", err)
	}
	if got, want := string(data), "new content\n"; got != want {
		t.Fatalf("overwritten content mismatch: got %q want %q", got, want)
	}
	if !strings.Contains(payload.Stdout, "A duplicate.txt") {
		t.Fatalf("stdout missing add summary: %q", payload.Stdout)
	}
}

func TestApplyPatchPartialSuccessLeavesChanges(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	run := strings.Join([]string{
		"apply_patch",
		"*** Begin Patch",
		"*** Add File: created.txt",
		"+hello",
		"*** Update File: missing.txt",
		"@@",
		"-old",
		"+new",
		"*** End Patch",
	}, "\n")

	step := PlanStep{ID: "partial", Command: CommandDraft{Shell: agentShell, Run: run, Cwd: dir}}
	req := InternalCommandRequest{Name: applyPatchCommandName, Raw: run, Step: step}

	payload, err := newApplyPatchCommand()(context.Background(), req)
	if err == nil {
		t.Fatalf("expected error from missing update target")
	}
	if payload.ExitCode == nil || *payload.ExitCode == 0 {
		t.Fatalf("expected non-zero exit code")
	}
	created := filepath.Join(dir, "created.txt")
	data, readErr := os.ReadFile(created)
	if readErr != nil {
		t.Fatalf("expected created file to persist, read err=%v", readErr)
	}
	if got, want := string(data), "hello\n"; got != want {
		t.Fatalf("created file content mismatch: got %q want %q", got, want)
	}
}
