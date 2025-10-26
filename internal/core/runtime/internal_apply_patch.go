package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	gitdiff "github.com/bluekeyes/go-gitdiff/gitdiff"
)

type applyPatchOptions struct {
	ignoreWhitespace bool
}

type patchOperationType string

const (
	patchOpUpdate patchOperationType = "update"
	patchOpAdd    patchOperationType = "add"
)

type patchOperation struct {
	Type  patchOperationType
	Path  string
	Hunks []patchHunk
}

type patchHunk struct {
	Before        []string
	After         []string
	RawLines      []string
	Header        string
	RawPatchLines []string
}

type hunkStatus struct {
	Number int
	Status string
}

type failedHunk struct {
	Number        int
	RawPatchLines []string
}

type applyPatchError struct {
	msg             string
	code            string
	relativePath    string
	originalContent string
	hunkStatuses    []hunkStatus
	failedHunk      *failedHunk
}

func (e *applyPatchError) Error() string {
	if e.msg != "" {
		return e.msg
	}
	return "apply_patch failed"
}

// newApplyPatchCommand constructs the built-in apply_patch handler.
func newApplyPatchCommand() InternalCommandHandler {
	return func(ctx context.Context, req InternalCommandRequest) (PlanObservationPayload, error) {
		_ = ctx

		header := extractApplyPatchHeader(req.Raw)
		options, err := parseApplyPatchOptions(header)
		if err != nil {
			return PlanObservationPayload{}, err
		}

		patchInput, err := extractPatchInput(req)
		if err != nil {
			return PlanObservationPayload{}, err
		}

		operations, err := parsePatch(patchInput)
		if err != nil {
			return PlanObservationPayload{}, err
		}
		if len(operations) == 0 {
			return PlanObservationPayload{}, errors.New("apply_patch: no patch operations detected")
		}

		workingDir, err := resolveWorkingDirectory(req.Step.Command.Cwd)
		if err != nil {
			return PlanObservationPayload{}, err
		}

		results, applyErr := applyPatchOperations(workingDir, operations, options)
		if applyErr != nil {
			formatted := formatApplyPatchError(applyErr)
			exitCode := 1
			return PlanObservationPayload{
				Stderr:   formatted,
				Details:  formatted,
				ExitCode: &exitCode,
			}, applyErr
		}

		if len(results) == 0 {
			return PlanObservationPayload{Stdout: "No changes applied."}, nil
		}

		sort.Slice(results, func(i, j int) bool {
			return results[i].Path < results[j].Path
		})

		var builder strings.Builder
		builder.WriteString("Success. Updated the following files:\n")
		for _, res := range results {
			builder.WriteString(res.Status)
			builder.WriteByte(' ')
			builder.WriteString(res.Path)
			builder.WriteByte('\n')
		}

		stdout := strings.TrimRight(builder.String(), "\n")
		return PlanObservationPayload{Stdout: stdout}, nil
	}
}

func extractApplyPatchHeader(raw string) string {
	if idx := strings.IndexAny(raw, "\r\n"); idx >= 0 {
		return raw[:idx]
	}
	return raw
}

func parseApplyPatchOptions(header string) (applyPatchOptions, error) {
	opts := applyPatchOptions{ignoreWhitespace: true}
	trimmed := strings.TrimSpace(header)
	if trimmed == "" {
		return opts, nil
	}
	tokens, err := tokenizeInternalCommand(trimmed)
	if err != nil {
		return opts, err
	}
	if len(tokens) <= 1 {
		return opts, nil
	}

	for _, token := range tokens[1:] {
		switch token {
		case "-w":
			opts.ignoreWhitespace = true
			continue
		case "-W":
			opts.ignoreWhitespace = false
			continue
		}

		lower := strings.ToLower(token)
		switch lower {
		case "--ignore-whitespace":
			opts.ignoreWhitespace = true
			continue
		case "--respect-whitespace", "--no-ignore-whitespace":
			opts.ignoreWhitespace = false
			continue
		}

		if value, ok := parseBoolAssignment(lower, "ignore_whitespace"); ok {
			opts.ignoreWhitespace = value
			continue
		}
		if value, ok := parseBoolAssignment(lower, "ignore-whitespace"); ok {
			opts.ignoreWhitespace = value
			continue
		}
		if value, ok := parseBoolAssignment(lower, "respect_whitespace"); ok {
			opts.ignoreWhitespace = !value
			continue
		}
		if value, ok := parseBoolAssignment(lower, "respect-whitespace"); ok {
			opts.ignoreWhitespace = !value
			continue
		}
	}

	return opts, nil
}

func parseBoolAssignment(token, key string) (bool, bool) {
	if !strings.HasPrefix(token, key+"=") {
		return false, false
	}
	raw := strings.TrimPrefix(token, key+"=")
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, false
	}
	return value, true
}

func extractPatchInput(req InternalCommandRequest) (string, error) {
	candidates := []string{req.Raw, req.Step.Command.Reason}
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		if block, ok := collectPatchBlock(candidate); ok {
			return block, nil
		}
	}
	return "", errors.New("apply_patch: no *** Begin Patch block provided")
}

func collectPatchBlock(input string) (string, bool) {
	normalized := strings.ReplaceAll(input, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	var builder strings.Builder
	inside := false
	for _, line := range lines {
		if line == "*** Begin Patch" {
			inside = true
		}
		if inside {
			if builder.Len() > 0 {
				builder.WriteByte('\n')
			}
			builder.WriteString(line)
			if line == "*** End Patch" {
				return builder.String(), true
			}
		}
	}
	return "", false
}

func parsePatch(input string) ([]patchOperation, error) {
	normalized := strings.ReplaceAll(input, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	var (
		operations  []patchOperation
		currentOp   *patchOperation
		currentHunk *patchHunk
		inside      bool
	)

	flushHunk := func() error {
		if currentHunk == nil {
			return nil
		}
		parsed, err := parseHunk(currentHunk.RawLines, currentOp.Path, currentHunk.Header)
		if err != nil {
			return err
		}
		currentOp.Hunks = append(currentOp.Hunks, parsed)
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
		if len(currentOp.Hunks) == 0 {
			return fmt.Errorf("apply_patch: no hunks provided for %s", currentOp.Path)
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
		}

		if !inside {
			continue
		}

		if strings.HasPrefix(line, "*** ") {
			if err := flushOp(); err != nil {
				return nil, err
			}
			if strings.HasPrefix(line, "*** Update File:") {
				path := strings.TrimSpace(strings.TrimPrefix(line, "*** Update File:"))
				currentOp = &patchOperation{Type: patchOpUpdate, Path: path}
				continue
			}
			if strings.HasPrefix(line, "*** Add File:") {
				path := strings.TrimSpace(strings.TrimPrefix(line, "*** Add File:"))
				currentOp = &patchOperation{Type: patchOpAdd, Path: path}
				continue
			}
			return nil, fmt.Errorf("apply_patch: unsupported patch directive: %s", line)
		}

		if currentOp == nil {
			if strings.TrimSpace(line) == "" {
				continue
			}
			return nil, fmt.Errorf("apply_patch: diff content appeared before a file directive: %q", line)
		}

		if strings.HasPrefix(line, "@@") {
			if err := flushHunk(); err != nil {
				return nil, err
			}
			currentHunk = &patchHunk{Header: line}
			continue
		}

		if currentHunk == nil {
			currentHunk = &patchHunk{}
		}
		currentHunk.RawLines = append(currentHunk.RawLines, line)
	}

	if inside {
		return nil, errors.New("apply_patch: missing *** End Patch terminator")
	}
	if err := flushOp(); err != nil {
		return nil, err
	}
	return operations, nil
}

func parseHunk(lines []string, filePath, header string) (patchHunk, error) {
	h := patchHunk{Header: header, RawLines: append([]string{}, lines...)}
	for _, raw := range lines {
		switch {
		case strings.HasPrefix(raw, "+"):
			h.After = append(h.After, raw[1:])
		case strings.HasPrefix(raw, "-"):
			h.Before = append(h.Before, raw[1:])
		case strings.HasPrefix(raw, " "):
			value := raw[1:]
			h.Before = append(h.Before, value)
			h.After = append(h.After, value)
		case raw == "\\ No newline at end of file":
			continue
		default:
			return patchHunk{}, fmt.Errorf("apply_patch: unsupported hunk line in %s: %q", filePath, raw)
		}
	}
	if header != "" {
		h.RawPatchLines = append(h.RawPatchLines, header)
	}
	h.RawPatchLines = append(h.RawPatchLines, lines...)
	return h, nil
}

type fileState struct {
	path                    string
	relativePath            string
	lines                   []string
	normalizedLines         []string
	endsWithNewline         bool
	originalContent         string
	originalEndsWithNewline *bool
	options                 applyPatchOptions
	cursor                  int
	touched                 bool
	isNew                   bool
	hunkStatuses            []hunkStatus
}

type applyPatchResult struct {
	Status string
	Path   string
}

func resolveWorkingDirectory(cwd string) (string, error) {
	trimmed := strings.TrimSpace(cwd)
	if trimmed == "" {
		dir, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("apply_patch: failed to determine working directory: %w", err)
		}
		return dir, nil
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("apply_patch: failed to resolve working directory %q: %w", trimmed, err)
	}
	return abs, nil
}

func applyPatchOperations(baseDir string, operations []patchOperation, options applyPatchOptions) ([]applyPatchResult, error) {
	states := make(map[string]*fileState)

	ensureState := func(relativePath string, create bool) (*fileState, error) {
		cleanRel := strings.TrimSpace(relativePath)
		if cleanRel == "" {
			return nil, errors.New("apply_patch: empty file path in patch")
		}
		absPath := cleanRel
		if !filepath.IsAbs(absPath) {
			absPath = filepath.Join(baseDir, cleanRel)
		}
		absPath = filepath.Clean(absPath)

		if state, ok := states[absPath]; ok {
			state.updateOptions(options)
			return state, nil
		}

		if create {
			if _, err := os.Stat(absPath); err == nil {
				return nil, fmt.Errorf("apply_patch: cannot add %s because it already exists", cleanRel)
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("apply_patch: failed to stat %s: %v", cleanRel, err)
			}
			state := &fileState{
				path:         absPath,
				relativePath: cleanRel,
				lines:        []string{},
				options:      options,
				touched:      false,
				cursor:       0,
				isNew:        true,
			}
			state.refreshNormalized()
			states[absPath] = state
			return state, nil
		}

		content, err := os.ReadFile(absPath)
		if err != nil {
			return nil, fmt.Errorf("apply_patch: failed to read %s: %w", cleanRel, err)
		}
		normalized := strings.ReplaceAll(string(content), "\r\n", "\n")
		endsWithNewline := strings.HasSuffix(normalized, "\n")
		var endPtr *bool
		endPtr = new(bool)
		*endPtr = endsWithNewline

		state := &fileState{
			path:                    absPath,
			relativePath:            cleanRel,
			originalContent:         string(content),
			originalEndsWithNewline: endPtr,
			options:                 options,
			cursor:                  0,
			touched:                 false,
			isNew:                   false,
		}
		state.setContentFromString(normalized)
		states[absPath] = state
		return state, nil
	}

	for _, op := range operations {
		if op.Type != patchOpUpdate && op.Type != patchOpAdd {
			return nil, fmt.Errorf("apply_patch: unsupported patch operation for %s: %s", op.Path, op.Type)
		}
		state, err := ensureState(op.Path, op.Type == patchOpAdd)
		if err != nil {
			return nil, err
		}
		state.cursor = 0
		state.hunkStatuses = nil

		for idx, hunk := range op.Hunks {
			number := idx + 1
			if err := applyHunk(state, hunk); err != nil {
				return nil, enhanceHunkError(err, state, hunk, number)
			}
			state.hunkStatuses = append(state.hunkStatuses, hunkStatus{Number: number, Status: "applied"})
			state.touched = true
		}
	}

	var results []applyPatchResult
	for _, state := range states {
		if !state.touched {
			continue
		}
		newContent := state.currentContent()
		if state.originalEndsWithNewline != nil {
			if *state.originalEndsWithNewline && !state.endsWithNewline {
				newContent += "\n"
			} else if !*state.originalEndsWithNewline && state.endsWithNewline {
				newContent = strings.TrimSuffix(newContent, "\n")
			}
		}
		if err := os.MkdirAll(filepath.Dir(state.path), 0o755); err != nil {
			return nil, fmt.Errorf("apply_patch: failed to create directories for %s: %w", state.relativePath, err)
		}
		if err := os.WriteFile(state.path, []byte(newContent), 0o644); err != nil {
			return nil, fmt.Errorf("apply_patch: failed to write %s: %w", state.relativePath, err)
		}
		status := "M"
		if state.isNew {
			status = "A"
		}
		results = append(results, applyPatchResult{Status: status, Path: state.relativePath})
	}

	return results, nil
}

func (s *fileState) setContentFromString(content string) {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	s.endsWithNewline = strings.HasSuffix(normalized, "\n")
	s.lines = splitLines(normalized)
	s.refreshNormalized()
}

func (s *fileState) currentContent() string {
	if len(s.lines) == 0 {
		if s.endsWithNewline {
			return "\n"
		}
		return ""
	}
	content := strings.Join(s.lines, "\n")
	if s.endsWithNewline && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if !s.endsWithNewline && strings.HasSuffix(content, "\n") {
		content = strings.TrimSuffix(content, "\n")
	}
	return content
}

func (s *fileState) refreshNormalized() {
	if s.options.ignoreWhitespace {
		s.normalizedLines = normalizeLines(s.lines, s.options)
	} else {
		s.normalizedLines = nil
	}
}

func (s *fileState) updateOptions(opts applyPatchOptions) {
	s.options = opts
	s.refreshNormalized()
}

func (s *fileState) ensureNormalizedLines() []string {
	if !s.options.ignoreWhitespace {
		return s.lines
	}
	if s.normalizedLines == nil {
		s.normalizedLines = normalizeLines(s.lines, s.options)
	}
	return s.normalizedLines
}

func splitLines(text string) []string {
	if text == "" {
		return []string{}
	}
	if strings.HasSuffix(text, "\n") {
		trimmed := strings.TrimSuffix(text, "\n")
		if trimmed == "" {
			return []string{""}
		}
		parts := strings.Split(trimmed, "\n")
		return append(parts, "")
	}
	return strings.Split(text, "\n")
}

func normalizeLines(lines []string, options applyPatchOptions) []string {
	if !options.ignoreWhitespace {
		return nil
	}
	normalized := make([]string, len(lines))
	for i, line := range lines {
		normalized[i] = normalizeLine(line)
	}
	return normalized
}

func applyHunk(state *fileState, hunk patchHunk) error {
	beforeLen := len(hunk.Before)
	var matchIndex int
	if beforeLen == 0 {
		matchIndex = len(state.lines)
		if matchIndex > 0 && state.lines[matchIndex-1] == "" {
			matchIndex--
		}
	} else {
		matchIndex = findSubsequence(state.lines, hunk.Before, state.cursor)
		if matchIndex == -1 {
			matchIndex = findSubsequence(state.lines, hunk.Before, 0)
		}
		if matchIndex == -1 && state.options.ignoreWhitespace {
			normalizedBefore := make([]string, len(hunk.Before))
			for i, line := range hunk.Before {
				normalizedBefore[i] = normalizeLine(line)
			}
			normalizedLines := state.ensureNormalizedLines()
			matchIndex = findSubsequence(normalizedLines, normalizedBefore, state.cursor)
			if matchIndex == -1 {
				matchIndex = findSubsequence(normalizedLines, normalizedBefore, 0)
			}
		}
		if matchIndex == -1 {
			return &applyPatchError{
				msg:          fmt.Sprintf("Hunk not found in %s.", state.relativePath),
				code:         "HUNK_NOT_FOUND",
				relativePath: state.relativePath,
				originalContent: func() string {
					if state.originalContent != "" {
						return state.originalContent
					}
					return state.currentContent()
				}(),
			}
		}
	}

	currentContent := state.currentContent()
	manualLines := applyLineUpdate(state.lines, matchIndex, beforeLen, hunk.After)
	manualContent, manualEndsWithNewline := assembleContentFromLines(manualLines)

	diffText := buildDiffForHunk(state, hunk, matchIndex)
	files, _, err := gitdiff.Parse(strings.NewReader(diffText))
	if err != nil {
		return fmt.Errorf("apply_patch: failed to materialize hunk for %s: %w", state.relativePath, err)
	}
	if len(files) == 0 {
		return fmt.Errorf("apply_patch: parsed diff for %s contained no file data", state.relativePath)
	}

	var buf bytes.Buffer
	if err := gitdiff.Apply(&buf, strings.NewReader(currentContent), files[0]); err != nil {
		return wrapGitDiffError(err)
	}

	if buf.String() == manualContent {
		state.setContentFromString(manualContent)
	} else {
		state.lines = manualLines
		state.endsWithNewline = manualEndsWithNewline
		state.refreshNormalized()
	}
	state.cursor = matchIndex + len(hunk.After)
	return nil
}

func buildDiffForHunk(state *fileState, hunk patchHunk, matchIndex int) string {
	oldStart := matchIndex + 1
	newStart := matchIndex + 1
	if len(hunk.Before) == 0 && state.isNew && len(state.lines) == 0 {
		oldStart = 0
		newStart = 1
	}

	oldLabel := "a/" + state.relativePath
	if len(hunk.Before) == 0 && state.isNew && len(state.lines) == 0 {
		oldLabel = "/dev/null"
	}

	var builder strings.Builder
	builder.WriteString("diff --git a/")
	builder.WriteString(state.relativePath)
	builder.WriteString(" b/")
	builder.WriteString(state.relativePath)
	builder.WriteByte('\n')
	builder.WriteString("--- ")
	builder.WriteString(oldLabel)
	builder.WriteByte('\n')
	builder.WriteString("+++ b/")
	builder.WriteString(state.relativePath)
	builder.WriteByte('\n')
	builder.WriteString(fmt.Sprintf("@@ -%d,%d +%d,%d @@\n", oldStart, len(hunk.Before), newStart, len(hunk.After)))
	beforeOffset := 0
	for _, line := range hunk.RawPatchLines {
		if strings.HasPrefix(line, "@@") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "-"):
			content := line[1:]
			if idx := matchIndex + beforeOffset; idx < len(state.lines) {
				content = state.lines[idx]
			}
			builder.WriteByte('-')
			builder.WriteString(content)
			builder.WriteByte('\n')
			beforeOffset++
		case strings.HasPrefix(line, "+"):
			builder.WriteByte('+')
			builder.WriteString(line[1:])
			builder.WriteByte('\n')
		case strings.HasPrefix(line, " "):
			content := line[1:]
			if idx := matchIndex + beforeOffset; idx < len(state.lines) {
				content = state.lines[idx]
			}
			builder.WriteByte(' ')
			builder.WriteString(content)
			builder.WriteByte('\n')
			beforeOffset++
		default:
			builder.WriteString(line)
			builder.WriteByte('\n')
		}
	}
	return builder.String()
}

func wrapGitDiffError(err error) error {
	if err == nil {
		return nil
	}
	var applyErr *gitdiff.ApplyError
	if errors.As(err, &applyErr) {
		return &applyPatchError{msg: applyErr.Error(), code: "HUNK_NOT_FOUND"}
	}
	if errors.Is(err, &gitdiff.Conflict{}) {
		return &applyPatchError{msg: err.Error(), code: "HUNK_NOT_FOUND"}
	}
	return err
}

func findSubsequence(haystack, needle []string, start int) int {
	if len(needle) == 0 {
		return -1
	}
	if start < 0 {
		start = 0
	}
	for i := start; i <= len(haystack)-len(needle); i++ {
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

func applyLineUpdate(lines []string, index, deleteCount int, replacement []string) []string {
	result := make([]string, 0, len(lines)-deleteCount+len(replacement))
	result = append(result, lines[:index]...)
	result = append(result, replacement...)
	if tail := lines[index+deleteCount:]; len(tail) > 0 {
		result = append(result, tail...)
	}
	return append([]string(nil), result...)
}

func assembleContentFromLines(lines []string) (string, bool) {
	if len(lines) == 0 {
		return "", false
	}
	endsWithNewline := lines[len(lines)-1] == ""
	content := strings.Join(lines, "\n")
	if endsWithNewline && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if !endsWithNewline && strings.HasSuffix(content, "\n") {
		content = strings.TrimSuffix(content, "\n")
	}
	return content, endsWithNewline
}

func normalizeLine(line string) string {
	var builder strings.Builder
	for _, r := range line {
		if unicode.IsSpace(r) {
			continue
		}
		builder.WriteRune(r)
	}
	return builder.String()
}

func enhanceHunkError(err error, state *fileState, hunk patchHunk, number int) error {
	var apErr *applyPatchError
	if errors.As(err, &apErr) {
		// Update existing error instance with richer metadata.
	} else {
		apErr = &applyPatchError{msg: err.Error()}
	}
	if apErr.code == "" {
		apErr.code = "HUNK_NOT_FOUND"
	}
	if apErr.relativePath == "" {
		apErr.relativePath = state.relativePath
	}
	if apErr.originalContent == "" {
		if state.originalContent != "" {
			apErr.originalContent = state.originalContent
		} else {
			apErr.originalContent = state.currentContent()
		}
	}
	statuses := append([]hunkStatus{}, state.hunkStatuses...)
	statuses = append(statuses, hunkStatus{Number: number, Status: "no-match"})
	apErr.hunkStatuses = statuses
	if apErr.failedHunk == nil {
		apErr.failedHunk = &failedHunk{Number: number, RawPatchLines: append([]string{}, hunk.RawPatchLines...)}
	}
	return apErr
}

func formatApplyPatchError(err error) string {
	if err == nil {
		return "Unknown error occurred."
	}
	var apErr *applyPatchError
	if errors.As(err, &apErr) {
		message := apErr.msg
		if message == "" {
			message = "Unknown error occurred."
		}
		if apErr.code == "HUNK_NOT_FOUND" || strings.Contains(strings.ToLower(message), "hunk not found") {
			relative := apErr.relativePath
			if !strings.HasPrefix(relative, "./") {
				relative = "./" + relative
			}
			parts := []string{message}
			if summary := describeHunkStatuses(apErr.hunkStatuses); summary != "" {
				parts = append(parts, "", summary)
			}
			if apErr.failedHunk != nil && len(apErr.failedHunk.RawPatchLines) > 0 {
				parts = append(parts, "", "Offending hunk:")
				parts = append(parts, strings.Join(apErr.failedHunk.RawPatchLines, "\n"))
			}
			parts = append(parts, "", fmt.Sprintf("Full content of file: %s::::", relative), apErr.originalContent)
			return strings.Join(parts, "\n")
		}
		return message
	}
	return err.Error()
}

func describeHunkStatuses(statuses []hunkStatus) string {
	if len(statuses) == 0 {
		return ""
	}
	var applied []string
	var failed *hunkStatus
	for _, status := range statuses {
		if status.Status == "applied" {
			applied = append(applied, strconv.Itoa(status.Number))
			continue
		}
		if failed == nil {
			s := status
			failed = &s
		}
	}
	var lines []string
	if len(applied) > 0 {
		lines = append(lines, fmt.Sprintf("Hunks applied: %s.", strings.Join(applied, ", ")))
	}
	if failed != nil {
		lines = append(lines, fmt.Sprintf("No match for hunk %d.", failed.Number))
	}
	return strings.Join(lines, "\n")
}
