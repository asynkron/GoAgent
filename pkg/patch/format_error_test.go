package patch

import (
	"strings"
	"testing"
)

func TestDescribeHunkStatuses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		statuses []HunkStatus
		want     string
	}{
		{
			name: "empty",
			want: "",
		},
		{
			name:     "only applied",
			statuses: []HunkStatus{{Number: 1, Status: "applied"}, {Number: 2, Status: "applied"}},
			want:     "Hunks applied: 1, 2.",
		},
		{
			name:     "mixed",
			statuses: []HunkStatus{{Number: 1, Status: "applied"}, {Number: 3, Status: "no-match"}},
			want:     "Hunks applied: 1.\nNo match for hunk 3.",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := describeHunkStatuses(tc.statuses); got != tc.want {
				t.Fatalf("describeHunkStatuses() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFormatErrorForHunkNotFound(t *testing.T) {
	t.Parallel()

	err := &Error{
		Message:      "Hunk not found in file.",
		Code:         "HUNK_NOT_FOUND",
		RelativePath: "src/app.go",
		HunkStatuses: []HunkStatus{{Number: 2, Status: "applied"}, {Number: 5, Status: "no-match"}},
		FailedHunk: &FailedHunk{
			Number:        5,
			RawPatchLines: []string{"@@", "-before", "+after"},
		},
		OriginalContent: "line1\nline2",
	}

	got := FormatError(err)
	if !containsAll(got, []string{
		"Hunk not found in file.",
		"./src/app.go",
		"Hunks applied: 2.",
		"No match for hunk 5.",
		"Offending hunk:",
		"@@",
		"line1\nline2",
	}) {
		t.Fatalf("unexpected formatted output:\n%s", got)
	}
}

func TestFormatErrorForUnknown(t *testing.T) {
	t.Parallel()

	if got := FormatError(nil); got != "Unknown error occurred." {
		t.Fatalf("unexpected message for nil error: %q", got)
	}

	err := &Error{Message: "custom failure"}
	if got := FormatError(err); got != "custom failure" {
		t.Fatalf("unexpected message: %q", got)
	}
}

func containsAll(haystack string, needles []string) bool {
	for _, needle := range needles {
		if !strings.Contains(haystack, needle) {
			return false
		}
	}
	return true
}
