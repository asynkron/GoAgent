package patch

import (
	"context"
	"strings"
	"testing"
)

func TestApplyToMemoryUpdatesDocument(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	patchBody := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: notes.txt",
		"@@",
		"-alpha",
		"+gamma",
		"*** End Patch",
	}, "\n")

	initial := map[string]string{"notes.txt": "alpha\nbeta\n"}
	updated, results, err := ApplyMemoryPatch(ctx, patchBody, initial, Options{IgnoreWhitespace: true})
	if err != nil {
		t.Fatalf("ApplyMemoryPatch returned error: %v", err)
	}
	if got, want := len(results), 1; got != want {
		t.Fatalf("unexpected result count: got %d want %d", got, want)
	}
	if results[0].Status != "M" || results[0].Path != "notes.txt" {
		t.Fatalf("unexpected result entry: %+v", results[0])
	}
	if got, want := updated["notes.txt"], "gamma\nbeta\n"; got != want {
		t.Fatalf("updated document mismatch: got %q want %q", got, want)
	}

	// Ensure the original map was not mutated.
	if got, want := initial["notes.txt"], "alpha\nbeta\n"; got != want {
		t.Fatalf("initial map mutated: got %q want %q", got, want)
	}
}

func TestApplyToMemoryAddsDocument(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	patchBody := strings.Join([]string{
		"*** Begin Patch",
		"*** Add File: new.txt",
		"@@",
		"+hello",
		"+world",
		"*** End Patch",
	}, "\n")

	updated, results, err := ApplyMemoryPatch(ctx, patchBody, map[string]string{}, Options{})
	if err != nil {
		t.Fatalf("ApplyMemoryPatch returned error: %v", err)
	}
	if _, ok := updated["new.txt"]; !ok {
		t.Fatalf("expected new file to exist")
	}
	if got, want := updated["new.txt"], "hello\nworld"; got != want {
		t.Fatalf("new file content mismatch: got %q want %q", got, want)
	}
	if len(results) != 1 || results[0].Status != "A" {
		t.Fatalf("unexpected results: %+v", results)
	}
}
