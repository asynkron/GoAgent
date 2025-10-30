package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBuildShellCommand(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		shell     string
		run       string
		wantPath  string
		wantArgs  []string
		wantError bool
	}{
		"defaults to dash c": {
			shell:    "/bin/bash",
			run:      "echo hi",
			wantPath: "/bin/bash",
			wantArgs: []string{"/bin/bash", "-lc", "echo hi"},
		},
		"preserves provided flags": {
			shell:    "/bin/bash -lc",
			run:      "echo hi",
			wantPath: "/bin/bash",
			wantArgs: []string{"/bin/bash", "-lc", "echo hi"},
		},
		"supports additional args": {
			shell:    "/bin/bash -O extglob -c",
			run:      "echo hi",
			wantPath: "/bin/bash",
			wantArgs: []string{"/bin/bash", "-O", "extglob", "-c", "echo hi"},
		},
		"rejects empty shell": {
			shell:     "   ",
			run:       "anything",
			wantError: true,
		},
	}

	for name, tc := range tests {
		tc := tc
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cmd, err := buildShellCommand(context.Background(), tc.shell, tc.run)
			if tc.wantError {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cmd.Path != tc.wantPath {
				t.Fatalf("Path mismatch: got %q, want %q", cmd.Path, tc.wantPath)
			}
			if len(cmd.Args) != len(tc.wantArgs) {
				t.Fatalf("Args length mismatch: got %d, want %d (%v)", len(cmd.Args), len(tc.wantArgs), cmd.Args)
			}
			for i, arg := range cmd.Args {
				if arg != tc.wantArgs[i] {
					t.Fatalf("Arg %d mismatch: got %q, want %q", i, arg, tc.wantArgs[i])
				}
			}
		})
	}
}

func TestEnforceObservationLimit(t *testing.T) {
	t.Parallel()

	payload := PlanObservationPayload{
		Stdout: strings.Repeat("a", maxObservationBytes+10),
		Stderr: strings.Repeat("b", maxObservationBytes+5),
	}

	enforceObservationLimit(&payload)

	if !payload.Truncated {
		t.Fatalf("expected payload to be marked truncated")
	}
	if len(payload.Stdout) != maxObservationBytes {
		t.Fatalf("expected stdout to be %d bytes, got %d", maxObservationBytes, len(payload.Stdout))
	}
	if len(payload.Stderr) != maxObservationBytes {
		t.Fatalf("expected stderr to be %d bytes, got %d", maxObservationBytes, len(payload.Stderr))
	}
	if !strings.HasSuffix(payload.Stdout, "aaaaaaaaaa") {
		t.Fatalf("expected stdout to retain tail of original data")
	}
	if !strings.HasSuffix(payload.Stderr, "bbbbb") {
		t.Fatalf("expected stderr to retain tail of original data")
	}
}

func TestCommandExecutorExecuteInternal(t *testing.T) {
	t.Parallel()

	executor := NewCommandExecutor()
	if err := executor.RegisterInternalCommand("beep", func(_ context.Context, req InternalCommandRequest) (PlanObservationPayload, error) {
		if req.Name != "beep" {
			return PlanObservationPayload{}, fmt.Errorf("unexpected name %q", req.Name)
		}
		if len(req.Positionals) != 1 {
			return PlanObservationPayload{}, fmt.Errorf("unexpected positional args: %v", req.Positionals)
		}
		if req.Positionals[0] != int64(123) {
			return PlanObservationPayload{}, fmt.Errorf("unexpected positional value %#v", req.Positionals[0])
		}
		if req.Args["message"] != "hello world" {
			return PlanObservationPayload{}, fmt.Errorf("unexpected named value: %v", req.Args)
		}
		if req.Step.ID != "step-1" {
			return PlanObservationPayload{}, fmt.Errorf("unexpected step id %q", req.Step.ID)
		}
		return PlanObservationPayload{Stdout: "beep beep"}, nil
	}); err != nil {
		t.Fatalf("failed to register internal command: %v", err)
	}

	step := PlanStep{ID: "step-1", Command: CommandDraft{Shell: agentShell, Run: `beep 123 message="hello world"`}}
	payload, err := executor.Execute(context.Background(), step)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if payload.Stdout != "beep beep" {
		t.Fatalf("unexpected stdout %q", payload.Stdout)
	}
	if payload.ExitCode == nil || *payload.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %v", payload.ExitCode)
	}
}

func TestCommandExecutorExecuteBuiltinApplyPatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(target, []byte("alpha\n"), 0o644); err != nil {
		t.Fatalf("failed to seed file: %v", err)
	}

	executor := NewCommandExecutor()
	if err := registerBuiltinInternalCommands(executor); err != nil {
		t.Fatalf("failed to register builtins: %v", err)
	}

	run := strings.Join([]string{
		"apply_patch",
		"*** Begin Patch",
		"*** Update File: note.txt",
		"@@",
		"-alpha",
		"+beta",
		"*** End Patch",
	}, "\n")

	step := PlanStep{ID: "patch", Command: CommandDraft{Shell: agentShell, Run: run, Cwd: dir}}
	payload, err := executor.Execute(context.Background(), step)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if payload.ExitCode == nil || *payload.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %v", payload.ExitCode)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("failed to read patched file: %v", err)
	}
	if got, want := string(data), "beta\n"; got != want {
		t.Fatalf("patched content mismatch: got %q want %q", got, want)
	}
}

func TestCommandExecutorExecuteInternalUnknown(t *testing.T) {
	t.Parallel()

	executor := NewCommandExecutor()
	step := PlanStep{ID: "step-1", Command: CommandDraft{Shell: agentShell, Run: "noop"}}
	_, err := executor.Execute(context.Background(), step)
	if err == nil || !strings.Contains(err.Error(), "unknown internal command") {
		t.Fatalf("expected unknown command error, got %v", err)
	}
}

func TestParseInternalInvocation(t *testing.T) {
	t.Parallel()

	step := PlanStep{ID: "x", Command: CommandDraft{Run: `echo 42 value=3.14 flag=true text="hello world"`}}
	req, err := parseInternalInvocation(step)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Name != "echo" {
		t.Fatalf("unexpected name %q", req.Name)
	}
	wantPositionals := []any{int64(42)}
	if !reflect.DeepEqual(req.Positionals, wantPositionals) {
		t.Fatalf("positionals mismatch: got %#v want %#v", req.Positionals, wantPositionals)
	}
	if req.Args["flag"] != true {
		t.Fatalf("expected flag true, got %#v", req.Args["flag"])
	}
	if req.Args["value"] != 3.14 {
		t.Fatalf("expected value 3.14, got %#v", req.Args["value"])
	}
	if req.Args["text"] != "hello world" {
		t.Fatalf("expected text 'hello world', got %#v", req.Args["text"])
	}
}

func TestTokenizeInternalCommandErrors(t *testing.T) {
	t.Parallel()

	_, err := tokenizeInternalCommand("echo \"unterminated")
	if err == nil || !strings.Contains(err.Error(), "unmatched quote") {
		t.Fatalf("expected unmatched quote error, got %v", err)
	}

	_, err = tokenizeInternalCommand("echo \\")
	if err == nil || !strings.Contains(err.Error(), "unfinished escape sequence") {
		t.Fatalf("expected unfinished escape error, got %v", err)
	}
}
