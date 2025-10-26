package patch

import "testing"

func TestEnsureNormalizedLinesCachesResult(t *testing.T) {
	t.Parallel()

	st := &state{
		lines:   []string{" foo ", "bar"},
		options: Options{IgnoreWhitespace: true},
	}

	first := ensureNormalizedLines(st)
	if first == nil || len(first) != 2 {
		t.Fatalf("unexpected normalized lines: %#v", first)
	}
	if first[0] != "foo" || first[1] != "bar" {
		t.Fatalf("unexpected normalization: %#v", first)
	}

	st.lines[0] = "baz"
	second := ensureNormalizedLines(st)
	if second[0] != "foo" {
		t.Fatalf("cache should not reflect later mutations: %#v", second)
	}
}

func TestUpdateNormalizedLinesSyncsCache(t *testing.T) {
	t.Parallel()

	st := &state{
		lines:   []string{"alpha", "beta"},
		options: Options{IgnoreWhitespace: true},
	}
	ensureNormalizedLines(st)

	updateNormalizedLines(st, 1, 1, []string{"  gamma  "})
	if got := st.normalizedLines[1]; got != "gamma" {
		t.Fatalf("normalized line not updated, got %q", got)
	}
}

func TestNormalizeLineDropsWhitespace(t *testing.T) {
	t.Parallel()

	if got := normalizeLine(" \t hello world \r"); got != "helloworld" {
		t.Fatalf("normalizeLine() = %q", got)
	}
}

func TestFindSubsequenceWithEOFRequirement(t *testing.T) {
	t.Parallel()

	haystack := []string{"a", "", ""}
	if idx := findSubsequence(haystack, []string{""}, 0, true); idx != 1 {
		t.Fatalf("expected match at index 1, got %d", idx)
	}
	if idx := findSubsequence([]string{"a", "tail"}, []string{"a"}, 0, true); idx != -1 {
		t.Fatalf("expected no match due to EOF requirement, got %d", idx)
	}
}

func TestMatchSatisfiesEOF(t *testing.T) {
	t.Parallel()

	if !matchSatisfiesEOF([]string{"line", ""}, 1, 1) {
		t.Fatalf("blank tail should satisfy EOF")
	}
	if matchSatisfiesEOF([]string{"line", "tail"}, 0, 1) {
		t.Fatalf("non-empty tail should fail EOF check")
	}
}
