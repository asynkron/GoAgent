package patch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type stubWorkspace struct {
	ensureFunc func(path string, create bool) (*state, error)
	deleteFunc func(path string) error
	commitFunc func() ([]Result, error)
}

func (s *stubWorkspace) Ensure(path string, create bool) (*state, error) {
	if s.ensureFunc != nil {
		return s.ensureFunc(path, create)
	}
	return nil, errors.New("unexpected Ensure call")
}

func (s *stubWorkspace) Delete(path string) error {
	if s.deleteFunc != nil {
		return s.deleteFunc(path)
	}
	return errors.New("unexpected Delete call")
}

func (s *stubWorkspace) Commit() ([]Result, error) {
	if s.commitFunc != nil {
		return s.commitFunc()
	}
	return nil, errors.New("unexpected Commit call")
}

func TestApplyReturnsErrorForNilWorkspace(t *testing.T) {
	t.Parallel()

	_, err := apply(context.Background(), nil, nil)
	if err == nil {
		t.Fatalf("expected error when workspace is nil")
	}
}

func TestApplyHonorsContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ws := &stubWorkspace{}
	operations := []Operation{{Type: OperationDelete, Path: "file.txt"}}

	_, err := apply(ctx, operations, ws)
	if err == nil {
		t.Fatalf("expected cancellation error")
	}
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("expected *Error, got %T", err)
	}
	if !strings.Contains(pe.Message, "context canceled") {
		t.Fatalf("unexpected message: %q", pe.Message)
	}
}

func TestApplyWrapsWorkspaceErrors(t *testing.T) {
	t.Parallel()

	ws := &stubWorkspace{
		deleteFunc: func(string) error {
			return fmt.Errorf("boom")
		},
	}

	operations := []Operation{{Type: OperationDelete, Path: "ghost.txt"}}
	_, err := apply(context.Background(), operations, ws)
	if err == nil {
		t.Fatalf("expected error")
	}
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("expected *Error, got %T", err)
	}
	if pe.Message != "boom" {
		t.Fatalf("unexpected error message: %q", pe.Message)
	}
}

func TestApplyEnhancesFailedHunk(t *testing.T) {
	t.Parallel()

	st := &state{
		path:         "/tmp/project/example.txt",
		relativePath: "example.txt",
		lines:        []string{"alpha", "beta"},
		options:      Options{},
	}

	ws := &stubWorkspace{
		ensureFunc: func(path string, create bool) (*state, error) {
			if path != "example.txt" || create {
				t.Fatalf("unexpected Ensure args: %q create=%v", path, create)
			}
			return st, nil
		},
		commitFunc: func() ([]Result, error) {
			t.Fatalf("commit should not be reached on failure")
			return nil, nil
		},
	}

	operations := []Operation{{
		Type: OperationUpdate,
		Path: "example.txt",
		Hunks: []Hunk{{
			Header: "@@",
			Before: []string{"missing"},
			After:  []string{"replacement"},
			RawPatchLines: []string{
				"@@",
				"-missing",
				"+replacement",
			},
		}},
	}}

	_, err := apply(context.Background(), operations, ws)
	if err == nil {
		t.Fatalf("expected error")
	}
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("expected *Error, got %T", err)
	}
	if pe.Code != "HUNK_NOT_FOUND" {
		t.Fatalf("expected HUNK_NOT_FOUND, got %q", pe.Code)
	}
	if pe.RelativePath != "example.txt" {
		t.Fatalf("unexpected relative path: %q", pe.RelativePath)
	}
	if len(pe.HunkStatuses) != 1 || pe.HunkStatuses[0].Status != "no-match" {
		t.Fatalf("unexpected hunk statuses: %#v", pe.HunkStatuses)
	}
	if pe.FailedHunk == nil || len(pe.FailedHunk.RawPatchLines) == 0 {
		t.Fatalf("expected failed hunk details: %#v", pe.FailedHunk)
	}
}

func TestApplyTrimsMovePathAndCommits(t *testing.T) {
	t.Parallel()

	st := &state{
		path:         "/tmp/work/original.txt",
		relativePath: "original.txt",
		lines:        []string{"alpha"},
		options:      Options{},
	}

	committed := false
	ws := &stubWorkspace{
		ensureFunc: func(path string, create bool) (*state, error) {
			if path != "original.txt" {
				t.Fatalf("unexpected path: %q", path)
			}
			return st, nil
		},
		commitFunc: func() ([]Result, error) {
			committed = true
			if st.movePath != "moved.txt" {
				t.Fatalf("move path not trimmed: %q", st.movePath)
			}
			if got, want := st.lines, []string{"beta"}; len(got) != len(want) || got[0] != want[0] {
				t.Fatalf("unexpected lines: %#v", st.lines)
			}
			if !st.touched {
				t.Fatalf("state should be marked as touched")
			}
			return []Result{{Status: "M", Path: "moved.txt"}}, nil
		},
	}

	operations := []Operation{{
		Type:     OperationUpdate,
		Path:     "original.txt",
		MovePath: "  moved.txt  ",
		Hunks: []Hunk{{
			Before: []string{"alpha"},
			After:  []string{"beta"},
		}},
	}}

	results, err := apply(context.Background(), operations, ws)
	if err != nil {
		t.Fatalf("apply returned error: %v", err)
	}
	if !committed {
		t.Fatalf("commit was not called")
	}
	if len(results) != 1 || results[0].Path != "moved.txt" {
		t.Fatalf("unexpected results: %#v", results)
	}
}
