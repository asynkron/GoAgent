package patch

import "testing"

func TestApplyHunkInsertsAtEnd(t *testing.T) {
	t.Parallel()

	st := &state{
		lines:   []string{"alpha", ""},
		options: Options{},
	}
	hunk := Hunk{After: []string{"beta"}}

	if err := applyHunk(st, hunk); err != nil {
		t.Fatalf("applyHunk returned error: %v", err)
	}
	if len(st.lines) < 2 || st.lines[1] != "beta" {
		t.Fatalf("unexpected lines: %#v", st.lines)
	}
	if st.cursor != 2 {
		t.Fatalf("cursor not updated, got %d", st.cursor)
	}
}

func TestApplyHunkMatchesWithWhitespaceNormalization(t *testing.T) {
	t.Parallel()

	st := &state{
		relativePath: "example.txt",
		lines:        []string{"value    one", "next"},
		options:      Options{IgnoreWhitespace: true},
	}
	hunk := Hunk{Before: []string{"valueone"}, After: []string{"value two"}}

	if err := applyHunk(st, hunk); err != nil {
		t.Fatalf("applyHunk returned error: %v", err)
	}
	if st.lines[0] != "value two" {
		t.Fatalf("line not replaced: %#v", st.lines)
	}
}

func TestApplyHunkReturnsDetailedErrorWhenMissing(t *testing.T) {
	t.Parallel()

	st := &state{
		relativePath: "missing.txt",
		lines:        []string{"first"},
		options:      Options{},
	}
	hunk := Hunk{Before: []string{"other"}, After: []string{"new"}}

	err := applyHunk(st, hunk)
	if err == nil {
		t.Fatalf("expected error")
	}
	perr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T", err)
	}
	if perr.OriginalContent == "" {
		t.Fatalf("expected original content to be included")
	}
}

func TestSplice(t *testing.T) {
	t.Parallel()

	if got := splice([]string{"a", "b", "c"}, 1, 1, []string{"x", "y"}); len(got) != 4 || got[1] != "x" || got[2] != "y" {
		t.Fatalf("unexpected splice result: %#v", got)
	}
}
