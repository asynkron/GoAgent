package patch

import (
	"context"
	"testing"
)

func TestApplyToMemoryCopiesInput(t *testing.T) {
	t.Parallel()

	initial := map[string]string{"file.txt": "alpha"}
	operations := []Operation{{
		Type:  OperationUpdate,
		Path:  "file.txt",
		Hunks: []Hunk{{Before: []string{"alpha"}, After: []string{"beta"}}},
	}}

	updated, results, err := ApplyToMemory(ctxBackground(), operations, initial, Options{})
	if err != nil {
		t.Fatalf("ApplyToMemory returned error: %v", err)
	}
	if updated["file.txt"] != "beta" {
		t.Fatalf("unexpected updated value: %q", updated["file.txt"])
	}
	if initial["file.txt"] != "alpha" {
		t.Fatalf("initial map mutated: %q", initial["file.txt"])
	}
	if len(results) != 1 || results[0].Status != "M" {
		t.Fatalf("unexpected results: %#v", results)
	}
}

func TestMemoryWorkspaceDeleteMissing(t *testing.T) {
	t.Parallel()

	ws := newMemoryWorkspace(map[string]string{}, Options{})
	if err := ws.Delete("missing.txt"); err == nil {
		t.Fatalf("expected error when deleting missing file")
	}
}

func TestMemoryWorkspaceCommitMoveValidatesPath(t *testing.T) {
	t.Parallel()

	ws := newMemoryWorkspace(map[string]string{"a.txt": "one"}, Options{})
	st, err := ws.Ensure("a.txt", false)
	if err != nil {
		t.Fatalf("Ensure returned error: %v", err)
	}
	st.lines = []string{"one"}
	st.touched = true
	st.movePath = "."

	if _, err := ws.Commit(); err == nil {
		t.Fatalf("expected error for invalid move path")
	}
}

func ctxBackground() context.Context {
	return context.Background()
}
