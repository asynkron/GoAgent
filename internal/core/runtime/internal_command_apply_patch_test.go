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

	// A delete should fail when the target file does not exist.
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

	// A delete should fail when the target file does not exist.
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

func TestApplyPatchAppliesMixedOperations(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	modifyPath := filepath.Join(dir, "modify.txt")
	deletePath := filepath.Join(dir, "delete.txt")
	duplicatePath := filepath.Join(dir, "duplicate.txt")

	if err := os.WriteFile(modifyPath, []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatalf("failed to seed modify file: %v", err)
	}
	if err := os.WriteFile(deletePath, []byte("obsolete\n"), 0o644); err != nil {
		t.Fatalf("failed to seed delete file: %v", err)
	}
	if err := os.WriteFile(duplicatePath, []byte("old\n"), 0o644); err != nil {
		t.Fatalf("failed to seed duplicate file: %v", err)
	}

	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Add File: nested/new.txt",
		"@@",
		"+created",
		"*** Delete File: delete.txt",
		"*** Update File: modify.txt",
		"@@",
		"-line2",
		"+changed",
		"*** Add File: duplicate.txt",
		"@@",
		"+replacement",
		"*** End Patch",
	}, "\n")

	run := "apply_patch\n" + patch
	step := PlanStep{ID: "mixed", Command: CommandDraft{Shell: agentShell, Run: run, Cwd: dir}}
	req := InternalCommandRequest{Name: applyPatchCommandName, Raw: run, Step: step}

	payload, err := newApplyPatchCommand()(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if payload.ExitCode == nil || *payload.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %+v", payload.ExitCode)
	}
	if !strings.Contains(payload.Stdout, "A duplicate.txt") ||
		!strings.Contains(payload.Stdout, "D delete.txt") ||
		!strings.Contains(payload.Stdout, "A nested/new.txt") ||
		!strings.Contains(payload.Stdout, "M modify.txt") {
		t.Fatalf("stdout missing expected summary: %q", payload.Stdout)
	}

	createdData, err := os.ReadFile(filepath.Join(dir, "nested", "new.txt"))
	if err != nil {
		t.Fatalf("failed to read created file: %v", err)
	}
	if string(createdData) != "created" {
		t.Fatalf("unexpected created file contents: %q", string(createdData))
	}

	modifiedData, err := os.ReadFile(modifyPath)
	if err != nil {
		t.Fatalf("failed to read modified file: %v", err)
	}
	if string(modifiedData) != "line1\nchanged\n" {
		t.Fatalf("unexpected modified file contents: %q", string(modifiedData))
	}

	if _, err := os.Stat(deletePath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected delete.txt to be removed, stat err=%v", err)
	}

	dupData, err := os.ReadFile(duplicatePath)
	if err != nil {
		t.Fatalf("failed to read overwritten file: %v", err)
	}
	if string(dupData) != "replacement" {
		t.Fatalf("unexpected duplicate file contents: %q", string(dupData))
	}
}

func TestApplyPatchMovesFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	source := filepath.Join(dir, "old", "name.txt")
	destination := filepath.Join(dir, "renamed", "dir", "name.txt")

	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatalf("failed to create source directory: %v", err)
	}
	if err := os.WriteFile(source, []byte("from\n"), 0o644); err != nil {
		t.Fatalf("failed to seed source file: %v", err)
	}

	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: old/name.txt",
		"*** Move to: renamed/dir/name.txt",
		"@@",
		"-from",
		"+to",
		"*** End Patch",
	}, "\n")

	run := "apply_patch\n" + patch
	step := PlanStep{ID: "move", Command: CommandDraft{Shell: agentShell, Run: run, Cwd: dir}}
	req := InternalCommandRequest{Name: applyPatchCommandName, Raw: run, Step: step}

	payload, err := newApplyPatchCommand()(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if payload.ExitCode == nil || *payload.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %+v", payload.ExitCode)
	}
	if !strings.Contains(payload.Stdout, "M renamed/dir/name.txt") {
		t.Fatalf("stdout missing move summary: %q", payload.Stdout)
	}

	if _, err := os.Stat(source); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected source to be removed, stat err=%v", err)
	}
	movedData, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("failed to read moved file: %v", err)
	}
	if string(movedData) != "to\n" {
		t.Fatalf("unexpected moved contents: %q", string(movedData))
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

	step := PlanStep{ID: "missing-delete", Command: CommandDraft{Shell: agentShell, Run: run, Cwd: dir}}
	req := InternalCommandRequest{Name: applyPatchCommandName, Raw: run, Step: step}

	payload, err := newApplyPatchCommand()(context.Background(), req)
	if err == nil {
		t.Fatalf("expected delete of missing file to fail")
	}
	if payload.ExitCode == nil || *payload.ExitCode == 0 {
		t.Fatalf("expected non-zero exit code")
	}
	if !strings.Contains(payload.Stderr, "Failed to delete file missing.txt") {
		t.Fatalf("stderr missing delete error: %q", payload.Stderr)
	}
}

func TestApplyPatchDeleteDirectoryFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "target"), 0o755); err != nil {
		t.Fatalf("failed to create directory: %v", err)
	}

	run := strings.Join([]string{
		"apply_patch",
		"*** Begin Patch",
		"*** Delete File: target",
		"*** End Patch",
	}, "\n")

	step := PlanStep{ID: "delete-dir", Command: CommandDraft{Shell: agentShell, Run: run, Cwd: dir}}
	req := InternalCommandRequest{Name: applyPatchCommandName, Raw: run, Step: step}

	payload, err := newApplyPatchCommand()(context.Background(), req)
	if err == nil {
		t.Fatalf("expected delete of directory to fail")
	}
	if payload.ExitCode == nil || *payload.ExitCode == 0 {
		t.Fatalf("expected non-zero exit code")
	}
	if !strings.Contains(payload.Stderr, "Failed to delete file target") {
		t.Fatalf("stderr missing directory delete error: %q", payload.Stderr)
	}
}

func TestApplyPatchEndOfFileMarker(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "tail.txt")
	if err := os.WriteFile(target, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatalf("failed to seed tail file: %v", err)
	}

	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: tail.txt",
		"@@",
		"-beta",
		"+omega",
		"*** End of File",
		"*** End Patch",
	}, "\n")

	run := "apply_patch\n" + patch
	step := PlanStep{ID: "eof", Command: CommandDraft{Shell: agentShell, Run: run, Cwd: dir}}
	req := InternalCommandRequest{Name: applyPatchCommandName, Raw: run, Step: step}

	payload, err := newApplyPatchCommand()(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if payload.ExitCode == nil || *payload.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %+v", payload.ExitCode)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("failed to read tail file: %v", err)
	}
	if string(data) != "alpha\nomega\n" {
		t.Fatalf("unexpected tail contents: %q", string(data))
	}
}
