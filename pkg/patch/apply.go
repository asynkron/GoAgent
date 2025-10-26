package patch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"unicode"
)

type workspace interface {
	Ensure(path string, create bool) (*state, error)
	Delete(path string) error
	Commit() ([]Result, error)
}

type state struct {
	path                    string
	relativePath            string
	lines                   []string
	normalizedLines         []string
	originalContent         string
	originalEndsWithNewline *bool
	originalMode            fs.FileMode
	touched                 bool
	cursor                  int
	hunkStatuses            []HunkStatus
	isNew                   bool
	movePath                string
	options                 Options
}

func apply(ctx context.Context, operations []Operation, ws workspace) ([]Result, error) {
	if ws == nil {
		return nil, errors.New("nil workspace")
	}
	for _, op := range operations {
		if ctx.Err() != nil {
			return nil, &Error{Message: ctx.Err().Error()}
		}
		switch op.Type {
		case OperationDelete:
			if err := ws.Delete(op.Path); err != nil {
				var pe *Error
				if errors.As(err, &pe) {
					return nil, pe
				}
				return nil, &Error{Message: err.Error()}
			}
		case OperationUpdate, OperationAdd:
			state, err := ws.Ensure(op.Path, op.Type == OperationAdd)
			if err != nil {
				var pe *Error
				if errors.As(err, &pe) {
					return nil, pe
				}
				return nil, &Error{Message: err.Error()}
			}
			state.cursor = 0
			state.hunkStatuses = nil
			for index, hunk := range op.Hunks {
				if ctx.Err() != nil {
					return nil, &Error{Message: ctx.Err().Error()}
				}
				number := index + 1
				if err := applyHunk(state, hunk); err != nil {
					return nil, enhanceHunkError(err, state, hunk, number)
				}
				state.hunkStatuses = append(state.hunkStatuses, HunkStatus{Number: number, Status: "applied"})
				state.touched = true
			}
			trimmedMove := strings.TrimSpace(op.MovePath)
			if trimmedMove != "" {
				state.movePath = trimmedMove
				state.touched = true
			}
		default:
			return nil, &Error{Message: fmt.Sprintf("unsupported patch operation for %s: %s", op.Path, op.Type)}
		}
	}
	results, err := ws.Commit()
	if err != nil {
		var pe *Error
		if errors.As(err, &pe) {
			return nil, pe
		}
		return nil, &Error{Message: err.Error()}
	}
	return results, nil
}

func applyHunk(state *state, hunk Hunk) error {
	if state == nil {
		return errors.New("missing file state")
	}

	before := hunk.Before
	after := hunk.After

	if len(before) == 0 {
		insertionIndex := len(state.lines)
		if insertionIndex > 0 && state.lines[insertionIndex-1] == "" {
			insertionIndex--
		}
		state.lines = splice(state.lines, insertionIndex, 0, after)
		updateNormalizedLines(state, insertionIndex, 0, after)
		state.cursor = insertionIndex + len(after)
		return nil
	}

	matchIndex := findSubsequence(state.lines, before, state.cursor, hunk.AtEOF)
	if matchIndex == -1 {
		matchIndex = findSubsequence(state.lines, before, 0, hunk.AtEOF)
	}

	if matchIndex == -1 && state.options.IgnoreWhitespace {
		normalizedBefore := make([]string, len(before))
		for i, line := range before {
			normalizedBefore[i] = normalizeLine(line)
		}
		normalizedLines := ensureNormalizedLines(state)
		matchIndex = findSubsequence(normalizedLines, normalizedBefore, state.cursor, hunk.AtEOF)
		if matchIndex == -1 {
			matchIndex = findSubsequence(normalizedLines, normalizedBefore, 0, hunk.AtEOF)
		}
	}

	if matchIndex == -1 {
		message := fmt.Sprintf("Hunk not found in %s.", state.relativePath)
		original := state.originalContent
		if original == "" {
			original = strings.Join(state.lines, "\n")
		}
		return &Error{
			Message:         message,
			Code:            "HUNK_NOT_FOUND",
			RelativePath:    state.relativePath,
			OriginalContent: original,
		}
	}

	state.lines = splice(state.lines, matchIndex, len(before), after)
	updateNormalizedLines(state, matchIndex, len(before), after)
	state.cursor = matchIndex + len(after)
	return nil
}

func splice(target []string, index, deleteCount int, replacement []string) []string {
	if deleteCount == 0 && len(replacement) == 0 {
		return target
	}
	result := make([]string, 0, len(target)-deleteCount+len(replacement))
	result = append(result, target[:index]...)
	result = append(result, replacement...)
	result = append(result, target[index+deleteCount:]...)
	return result
}

func findSubsequence(haystack, needle []string, startIndex int, requireEOF bool) int {
	if len(needle) == 0 {
		return -1
	}
	if startIndex < 0 {
		startIndex = 0
	}
	if startIndex > len(haystack) {
		startIndex = len(haystack)
	}
	for i := startIndex; i <= len(haystack)-len(needle); i++ {
		matched := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				matched = false
				break
			}
		}
		if matched {
			if requireEOF && !matchSatisfiesEOF(haystack, i, len(needle)) {
				continue
			}
			return i
		}
	}
	return -1
}

func matchSatisfiesEOF(lines []string, start, length int) bool {
	end := start + length
	if end >= len(lines) {
		return true
	}
	for _, line := range lines[end:] {
		if line != "" {
			return false
		}
	}
	return true
}

func ensureNormalizedLines(state *state) []string {
	if state == nil {
		return nil
	}
	if !state.options.IgnoreWhitespace {
		return state.lines
	}
	if state.normalizedLines != nil {
		return state.normalizedLines
	}
	normalized := make([]string, len(state.lines))
	for i, line := range state.lines {
		normalized[i] = normalizeLine(line)
	}
	state.normalizedLines = normalized
	return normalized
}

func updateNormalizedLines(state *state, index, deleteCount int, replacement []string) {
	if state == nil || !state.options.IgnoreWhitespace {
		return
	}
	normalized := ensureNormalizedLines(state)
	replacementNormalized := make([]string, len(replacement))
	for i, line := range replacement {
		replacementNormalized[i] = normalizeLine(line)
	}
	state.normalizedLines = splice(normalized, index, deleteCount, replacementNormalized)
}

func normalizeLine(line string) string {
	if line == "" {
		return ""
	}
	var builder strings.Builder
	builder.Grow(len(line))
	for _, r := range line {
		if unicode.IsSpace(r) {
			continue
		}
		builder.WriteRune(r)
	}
	return builder.String()
}

func enhanceHunkError(err error, state *state, hunk Hunk, number int) *Error {
	var pe *Error
	if errors.As(err, &pe) {
		// Use existing instance to preserve metadata.
	} else {
		pe = &Error{Message: err.Error()}
	}

	statuses := append([]HunkStatus{}, state.hunkStatuses...)
	if pe != nil && len(pe.HunkStatuses) > 0 {
		statuses = append(statuses, pe.HunkStatuses...)
	}
	statuses = append(statuses, HunkStatus{Number: number, Status: "no-match"})
	pe.HunkStatuses = statuses

	if pe.Code == "" {
		pe.Code = "HUNK_NOT_FOUND"
	}
	if pe.RelativePath == "" && state != nil {
		pe.RelativePath = state.relativePath
	}
	if pe.OriginalContent == "" && state != nil {
		if state.originalContent != "" {
			pe.OriginalContent = state.originalContent
		} else {
			pe.OriginalContent = strings.Join(state.lines, "\n")
		}
	}
	if pe.FailedHunk == nil {
		rawLines := append([]string(nil), hunk.RawPatchLines...)
		pe.FailedHunk = &FailedHunk{Number: number, RawPatchLines: rawLines}
	}
	return pe
}

func describeHunkStatuses(statuses []HunkStatus) string {
	if len(statuses) == 0 {
		return ""
	}
	var applied []string
	var failed string
	for _, status := range statuses {
		if status.Status == "applied" {
			applied = append(applied, fmt.Sprintf("%d", status.Number))
			continue
		}
		if failed == "" {
			failed = fmt.Sprintf("No match for hunk %d.", status.Number)
		}
	}

	parts := make([]string, 0, 2)
	if len(applied) > 0 {
		parts = append(parts, fmt.Sprintf("Hunks applied: %s.", strings.Join(applied, ", ")))
	}
	if failed != "" {
		parts = append(parts, failed)
	}
	return strings.Join(parts, "\n")
}

// FormatError renders Error values into a human readable message suitable for
// surfacing to end users.
func FormatError(err *Error) string {
	if err == nil {
		return "Unknown error occurred."
	}
	message := err.Message
	if message == "" {
		message = "Unknown error occurred."
	}
	code := err.Code
	if code == "HUNK_NOT_FOUND" || strings.Contains(strings.ToLower(message), "hunk not found") {
		relativePath := err.RelativePath
		if relativePath == "" {
			relativePath = "unknown file"
		}
		displayPath := relativePath
		if !strings.HasPrefix(displayPath, "./") {
			displayPath = "./" + displayPath
		}
		var parts []string
		parts = append(parts, message)
		if summary := describeHunkStatuses(err.HunkStatuses); summary != "" {
			parts = append(parts, "", summary)
		}
		if err.FailedHunk != nil && len(err.FailedHunk.RawPatchLines) > 0 {
			parts = append(parts, "", "Offending hunk:")
			parts = append(parts, strings.Join(err.FailedHunk.RawPatchLines, "\n"))
		}
		if err.OriginalContent != "" {
			parts = append(parts, "", fmt.Sprintf("Full content of file: %s::::", displayPath), err.OriginalContent)
		}
		return strings.Join(parts, "\n")
	}
	return message
}
