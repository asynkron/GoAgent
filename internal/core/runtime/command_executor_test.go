package runtime

import (
	"context"
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
