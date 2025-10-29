package runtime

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/sergi/go-diff/diffmatchpatch"
)

const applyPatchUsage = "Usage: apply_patch [--respect-whitespace]\n\n" +
	"Reads a *** Begin Patch block from the command string and applies it to the workspace.\n" +
	"  --ignore-whitespace, -w   Match hunks without considering whitespace differences (default).\n" +
	"  --respect-whitespace, -W  Require whitespace to match before applying hunks.\n"

type applyPatchOptions struct {
	ignoreWhitespace bool
}

type patchOperation struct {
	typeLabel string
	path      string
	hunks     []patchHunk
}

type patchHunk struct {
	header        string
	before        []string
	after         []string
	rawPatchLines []string
}

type hunkStatus struct {
	number int
	status string
}

type applyResult struct {
	status string
	path   string
}

type fileState struct {
	path                string
	relativePath        string
	lines               []string
	normalizedLines     []string
	originalContent     string
	originalEndsWithNew *bool
	touched             bool
	cursor              int
	hunkStatuses        []hunkStatus
	isNew               bool
	options             applyPatchOptions
}

type hunkApplyError struct {
	err          error
	relativePath string
	original     string
	hunkStatuses []hunkStatus
	failedHunk   patchHunk
}

func (e *hunkApplyError) Error() string {
	if e == nil {
		return ""
	}
	return e.err.Error()
}

func (e *hunkApplyError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func newApplyPatchCommand() InternalCommandHandler {
	return func(ctx context.Context, req InternalCommandRequest) (PlanObservationPayload, error) {
		opts, help, err := parseApplyPatchInvocation(req.Raw)
		if err != nil {
			return PlanObservationPayload{}, err
		}
		if help {
			return PlanObservationPayload{Stdout: applyPatchUsage}, nil
		}

		operations, err := parsePatchOperations(req.Raw)
		if err != nil {
			return PlanObservationPayload{}, err
		}
		if len(operations) == 0 {
			return PlanObservationPayload{}, errors.New("apply_patch: no patch operations detected")
		}

		baseDir, err := resolveCommandDirectory(req.Step.Command.Cwd)
		if err != nil {
			return PlanObservationPayload{}, err
		}

		results, applyErr := applyPatchOperations(ctx, baseDir, operations, opts)
		if applyErr != nil {
			formatted := formatApplyPatchError(applyErr)
			payload := PlanObservationPayload{Stderr: formatted, Details: formatted}
			code := 1
			payload.ExitCode = &code
			return payload, applyErr
		}

		if len(results) == 0 {
			zero := 0
			return PlanObservationPayload{Stdout: "No changes applied.", ExitCode: &zero}, nil
		}

		sort.Slice(results, func(i, j int) bool {
			return results[i].path < results[j].path
		})
		lines := []string{"Success. Updated the following files:"}
		for _, res := range results {
			lines = append(lines, fmt.Sprintf("%s %s", res.status, res.path))
		}
		zero := 0
		return PlanObservationPayload{Stdout: strings.Join(lines, "\n"), ExitCode: &zero}, nil
	}
}

func parseApplyPatchInvocation(raw string) (applyPatchOptions, bool, error) {
	trimmed := strings.TrimSpace(raw)
	line := trimmed
	if idx := strings.Index(trimmed, "\n"); idx >= 0 {
		line = trimmed[:idx]
	}
	tokens := strings.Fields(line)
	opts := applyPatchOptions{ignoreWhitespace: true}
	if len(tokens) == 0 {
		return opts, false, errors.New("apply_patch: command invocation missing")
	}
	for _, token := range tokens[1:] {
		if strings.HasPrefix(token, "<<") {
			break
		}
		switch token {
		case "--ignore-whitespace", "-w":
			opts.ignoreWhitespace = true
		case "--respect-whitespace", "-W", "--no-ignore-whitespace":
			opts.ignoreWhitespace = false
		case "--help", "-h":
			return opts, true, nil
		default:
			return opts, false, fmt.Errorf("apply_patch: unknown option: %s", token)
		}
	}
	return opts, false, nil
}

func parsePatchOperations(raw string) ([]patchOperation, error) {
	if !strings.Contains(raw, "*** Begin Patch") {
		return nil, errors.New("apply_patch: no patch provided via stdin")
	}
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	var operations []patchOperation
	var currentOp *patchOperation
	var currentHunk *patchHunk
	inside := false

	flushHunk := func() error {
		if currentHunk == nil {
			return nil
		}
		if currentOp == nil {
			return errors.New("apply_patch: encountered hunk without active operation")
		}
		parsed, err := parseHunk(currentHunk, currentOp.path)
		if err != nil {
			return err
		}
		currentOp.hunks = append(currentOp.hunks, parsed)
		currentHunk = nil
		return nil
	}

	flushOp := func() error {
		if currentOp == nil {
			return nil
		}
		if err := flushHunk(); err != nil {
			return err
		}
		if len(currentOp.hunks) == 0 {
			return fmt.Errorf("apply_patch: no hunks provided for %s", currentOp.path)
		}
		operations = append(operations, *currentOp)
		currentOp = nil
		return nil
	}

	for _, line := range lines {
		switch {
		case line == "*** Begin Patch":
			inside = true
			continue
		case line == "*** End Patch":
			if inside {
				if err := flushOp(); err != nil {
					return nil, err
				}
			}
			inside = false
			continue
		case !inside:
			continue
		case strings.HasPrefix(line, "*** "):
			if err := flushOp(); err != nil {
				return nil, err
			}
			switch {
			case strings.HasPrefix(line, "*** Update File:"):
				path := strings.TrimSpace(strings.TrimPrefix(line, "*** Update File:"))
				currentOp = &patchOperation{typeLabel: "update", path: path}
			case strings.HasPrefix(line, "*** Add File:"):
				path := strings.TrimSpace(strings.TrimPrefix(line, "*** Add File:"))
				currentOp = &patchOperation{typeLabel: "add", path: path}
			default:
				return nil, fmt.Errorf("apply_patch: unsupported patch directive: %s", line)
			}
			continue
		case strings.HasPrefix(line, "@@"):
			if err := flushHunk(); err != nil {
				return nil, err
			}
			currentHunk = &patchHunk{header: line}
			continue
		}

		if currentOp == nil {
			if strings.TrimSpace(line) == "" {
				continue
			}
			return nil, fmt.Errorf("apply_patch: diff content appeared before a file directive: %q", line)
		}

		if currentHunk == nil {
			currentHunk = &patchHunk{}
		}
		currentHunk.rawPatchLines = append(currentHunk.rawPatchLines, line)
	}

	if inside {
		return nil, errors.New("apply_patch: missing *** End Patch terminator")
	}
	if err := flushOp(); err != nil {
		return nil, err
	}
	return operations, nil
}

func parseHunk(raw *patchHunk, path string) (patchHunk, error) {
	if raw == nil {
		return patchHunk{}, errors.New("apply_patch: missing hunk data")
	}
	var before []string
	var after []string
	var rawLines []string
	if raw.header != "" {
		rawLines = append(rawLines, raw.header)
	}
	for _, line := range raw.rawPatchLines {
		switch {
		case strings.HasPrefix(line, "+"):
			after = append(after, strings.TrimPrefix(line, "+"))
		case strings.HasPrefix(line, "-"):
			before = append(before, strings.TrimPrefix(line, "-"))
		case strings.HasPrefix(line, " "):
			value := strings.TrimPrefix(line, " ")
			before = append(before, value)
			after = append(after, value)
		case line == "\\ No newline at end of file":
			// ignore marker
			continue
		default:
			return patchHunk{}, fmt.Errorf("apply_patch: unsupported hunk line in %s: %q", path, line)
		}
		rawLines = append(rawLines, line)
	}
	return patchHunk{header: raw.header, before: before, after: after, rawPatchLines: rawLines}, nil
}

func resolveCommandDirectory(cwd string) (string, error) {
	if strings.TrimSpace(cwd) == "" {
		return os.Getwd()
	}
	if filepath.IsAbs(cwd) {
		return cwd, nil
	}
	base, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, cwd), nil
}

func applyPatchOperations(ctx context.Context, baseDir string, operations []patchOperation, opts applyPatchOptions) ([]applyResult, error) {
	fileStates := make(map[string]*fileState)
	for _, op := range operations {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		state, err := ensureFileState(baseDir, op, opts, fileStates)
		if err != nil {
			return nil, err
		}
		state.cursor = 0
		state.touched = false
		state.hunkStatuses = state.hunkStatuses[:0]
		for idx, hunk := range op.hunks {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
			if err := applyHunk(state, hunk); err != nil {
				return nil, enhanceHunkError(err, state, hunk, idx+1)
			}
			state.hunkStatuses = append(state.hunkStatuses, hunkStatus{number: idx + 1, status: "applied"})
			state.touched = true
		}
	}

	var results []applyResult
	for _, state := range fileStates {
		if !state.touched {
			continue
		}
		if err := writeFileState(state); err != nil {
			return nil, err
		}
		status := "M"
		if state.isNew {
			status = "A"
		}
		results = append(results, applyResult{status: status, path: filepath.ToSlash(state.relativePath)})
	}
	return results, nil
}

func ensureFileState(baseDir string, op patchOperation, opts applyPatchOptions, cache map[string]*fileState) (*fileState, error) {
	if strings.TrimSpace(op.path) == "" {
		return nil, errors.New("apply_patch: file path missing in patch operation")
	}
	absPath := op.path
	if !filepath.IsAbs(absPath) {
		absPath = filepath.Join(baseDir, op.path)
	}
	absPath = filepath.Clean(absPath)
	if state, ok := cache[absPath]; ok {
		state.options = opts
		if opts.ignoreWhitespace {
			state.normalizedLines = normalizeLines(state.lines, opts)
		} else {
			state.normalizedLines = nil
		}
		return state, nil
	}
	info, err := os.Stat(absPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if op.typeLabel != "add" {
				return nil, fmt.Errorf("apply_patch: failed to read %s: %v", op.path, err)
			}
			state := &fileState{
				path:                absPath,
				relativePath:        op.path,
				lines:               []string{},
				normalizedLines:     normalizeLines(nil, opts),
				originalContent:     "",
				originalEndsWithNew: nil,
				cursor:              0,
				hunkStatuses:        nil,
				isNew:               true,
				options:             opts,
			}
			cache[absPath] = state
			return state, nil
		}
		return nil, fmt.Errorf("apply_patch: failed to stat %s: %v", op.path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("apply_patch: %s is a directory", op.path)
	}
	if op.typeLabel == "add" {
		return nil, fmt.Errorf("apply_patch: cannot add %s because it already exists", op.path)
	}
	content, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("apply_patch: failed to read %s: %v", op.path, err)
	}
	normalized := strings.ReplaceAll(string(content), "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	state := &fileState{
		path:                absPath,
		relativePath:        op.path,
		lines:               lines,
		normalizedLines:     nil,
		originalContent:     string(content),
		originalEndsWithNew: ptrBool(strings.HasSuffix(normalized, "\n")),
		cursor:              0,
		hunkStatuses:        nil,
		options:             opts,
	}
	if opts.ignoreWhitespace {
		state.normalizedLines = normalizeLines(lines, opts)
	}
	cache[absPath] = state
	return state, nil
}

func writeFileState(state *fileState) error {
	if state == nil {
		return nil
	}
	content := strings.Join(state.lines, "\n")
	if state.originalEndsWithNew != nil {
		if *state.originalEndsWithNew && !strings.HasSuffix(content, "\n") {
			content += "\n"
		} else if !*state.originalEndsWithNew && strings.HasSuffix(content, "\n") {
			content = strings.TrimSuffix(content, "\n")
		}
	}
	if err := os.MkdirAll(filepath.Dir(state.path), 0o755); err != nil {
		return fmt.Errorf("apply_patch: failed to create parent directory for %s: %v", state.relativePath, err)
	}
	if err := os.WriteFile(state.path, []byte(content), 0o666); err != nil {
		return fmt.Errorf("apply_patch: failed to write %s: %v", state.relativePath, err)
	}
	return nil
}

func applyHunk(state *fileState, hunk patchHunk) error {
	if len(hunk.before) == 0 {
		insertion := len(state.lines)
		if insertion > 0 && state.lines[insertion-1] == "" {
			insertion--
		}
		state.lines = splice(state.lines, insertion, 0, hunk.after)
		updateNormalizedLines(state, insertion, 0, hunk.after)
		state.cursor = insertion + len(hunk.after)
		return nil
	}

	matchIndex := findSubsequence(state.lines, hunk.before, state.cursor)
	if matchIndex == -1 {
		matchIndex = findSubsequence(state.lines, hunk.before, 0)
	}

	if matchIndex == -1 && state.options.ignoreWhitespace {
		normalizedBefore := normalizeLines(hunk.before, state.options)
		if state.normalizedLines != nil {
			matchIndex = findSubsequence(state.normalizedLines, normalizedBefore, state.cursor)
			if matchIndex == -1 {
				matchIndex = findSubsequence(state.normalizedLines, normalizedBefore, 0)
			}
		}
		if matchIndex == -1 {
			matchIndex = findByDiffMatchPatch(state, normalizedBefore)
		}
	}
	if matchIndex == -1 {
		return fmt.Errorf("Hunk not found in %s.", state.relativePath)
	}

	state.lines = splice(state.lines, matchIndex, len(hunk.before), hunk.after)
	updateNormalizedLines(state, matchIndex, len(hunk.before), hunk.after)
	state.cursor = matchIndex + len(hunk.after)
	return nil
}

func enhanceHunkError(err error, state *fileState, hunk patchHunk, number int) error {
	if err == nil {
		return nil
	}
	statuses := append([]hunkStatus{}, state.hunkStatuses...)
	statuses = append(statuses, hunkStatus{number: number, status: "no-match"})
	original := state.originalContent
	if original == "" {
		original = strings.Join(state.lines, "\n")
	}
	return &hunkApplyError{
		err:          err,
		relativePath: state.relativePath,
		original:     original,
		hunkStatuses: statuses,
		failedHunk:   hunk,
	}
}

func formatApplyPatchError(err error) string {
	if err == nil {
		return "Unknown error occurred."
	}
	var hunkErr *hunkApplyError
	if errors.As(err, &hunkErr) {
		lines := []string{hunkErr.err.Error()}
		desc := describeHunkStatuses(hunkErr.hunkStatuses)
		if desc != "" {
			lines = append(lines, "", desc)
		}
		if len(hunkErr.failedHunk.rawPatchLines) > 0 {
			lines = append(lines, "", "Offending hunk:")
			lines = append(lines, strings.Join(hunkErr.failedHunk.rawPatchLines, "\n"))
		}
		relative := filepath.ToSlash(hunkErr.relativePath)
		if !strings.HasPrefix(relative, "./") {
			relative = "./" + relative
		}
		lines = append(lines, "", fmt.Sprintf("Full content of file: %s::::", relative), hunkErr.original)
		return strings.Join(lines, "\n")
	}
	return err.Error()
}

func describeHunkStatuses(statuses []hunkStatus) string {
	if len(statuses) == 0 {
		return ""
	}
	var applied []string
	var failed *hunkStatus
	for _, st := range statuses {
		if st.status == "applied" {
			applied = append(applied, fmt.Sprintf("%d", st.number))
		} else if failed == nil {
			failed = &hunkStatus{number: st.number, status: st.status}
		}
	}
	var lines []string
	if len(applied) > 0 {
		lines = append(lines, fmt.Sprintf("Hunks applied: %s.", strings.Join(applied, ", ")))
	}
	if failed != nil {
		lines = append(lines, fmt.Sprintf("No match for hunk %d.", failed.number))
	}
	return strings.Join(lines, "\n")
}

func normalizeLines(lines []string, opts applyPatchOptions) []string {
	if !opts.ignoreWhitespace {
		return nil
	}
	normalized := make([]string, len(lines))
	for i, line := range lines {
		normalized[i] = normalizeLine(line)
	}
	return normalized
}

func normalizeLine(input string) string {
	if input == "" {
		return ""
	}
	runes := make([]rune, 0, len(input))
	for _, r := range input {
		if unicode.IsSpace(r) {
			continue
		}
		runes = append(runes, r)
	}
	return string(runes)
}

func updateNormalizedLines(state *fileState, index, deleteCount int, replacement []string) {
	if !state.options.ignoreWhitespace {
		return
	}
	if state.normalizedLines == nil {
		state.normalizedLines = normalizeLines(state.lines, state.options)
		return
	}
	repl := normalizeLines(replacement, state.options)
	state.normalizedLines = splice(state.normalizedLines, index, deleteCount, repl)
}

func splice[T any](input []T, index, deleteCount int, replacement []T) []T {
	if index < 0 {
		index = 0
	}
	if index > len(input) {
		index = len(input)
	}
	if deleteCount < 0 {
		deleteCount = 0
	}
	end := index + deleteCount
	if end > len(input) {
		end = len(input)
	}
	result := append([]T{}, input[:index]...)
	result = append(result, replacement...)
	result = append(result, input[end:]...)
	return result
}

func findSubsequence(haystack, needle []string, start int) int {
	if len(needle) == 0 {
		return -1
	}
	if start < 0 {
		start = 0
	}
	for i := start; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func ptrBool(value bool) *bool {
	return &value
}

func findByDiffMatchPatch(state *fileState, normalizedBefore []string) int {
	if state == nil || !state.options.ignoreWhitespace {
		return -1
	}
	if len(normalizedBefore) == 0 {
		return -1
	}
	normalized := state.normalizedLines
	if normalized == nil {
		normalized = normalizeLines(state.lines, state.options)
	}
	if len(normalized) == 0 {
		return -1
	}
	text := strings.Join(normalized, "\n")
	pattern := strings.Join(normalizedBefore, "\n")
	if text == "" || pattern == "" {
		return -1
	}
	matcher := diffmatchpatch.New()
	idx := matcher.MatchMain(text, pattern, 0)
	if idx == -1 {
		return -1
	}
	line := 0
	for i := 0; i < len(text) && i < idx; i++ {
		if text[i] == '\n' {
			line++
		}
	}
	return line
}
