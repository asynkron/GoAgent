package patch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestApplyFilesystemUpdatesFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "foo.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatalf("failed to write fixture: %v", err)
	}

	ops := []Operation{{
		Type:  OperationUpdate,
		Path:  "foo.txt",
		Hunks: []Hunk{{Before: []string{"one"}, After: []string{"two"}}},
	}}

	results, err := ApplyFilesystem(context.Background(), ops, FilesystemOptions{WorkingDir: dir})
	if err != nil {
		t.Fatalf("ApplyFilesystem returned error: %v", err)
	}
	if len(results) != 1 || results[0].Status != "M" {
		t.Fatalf("unexpected results: %#v", results)
	}
	content, err := os.ReadFile(filepath.Join(dir, "foo.txt"))
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}
	if string(content) != "two\n" {
		t.Fatalf("unexpected content: %q", content)
	}
}

func TestApplyFilesystemAddsAndMovesFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ops := []Operation{{
		Type:  OperationAdd,
		Path:  "new.txt",
		Hunks: []Hunk{{After: []string{"hello"}}},
	}, {
		Type:     OperationUpdate,
		Path:     "new.txt",
		MovePath: "nested/moved.txt",
		Hunks: []Hunk{{
			Before: []string{"hello"},
			After:  []string{"world"},
		}},
	}}

	results, err := ApplyFilesystem(context.Background(), ops, FilesystemOptions{WorkingDir: dir})
	if err != nil {
		t.Fatalf("ApplyFilesystem returned error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("unexpected results: %#v", results)
	}
	if results[0].Status != "A" || results[0].Path != "nested/moved.txt" {
		t.Fatalf("unexpected result entry: %#v", results[0])
	}

	content, err := os.ReadFile(filepath.Join(dir, "nested", "moved.txt"))
	if err != nil {
		t.Fatalf("failed to read moved file: %v", err)
	}
	if string(content) != "world" {
		t.Fatalf("unexpected moved content: %q", content)
	}
}
