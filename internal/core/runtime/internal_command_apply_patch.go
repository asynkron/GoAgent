package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

type applyPatchOptions struct {
	ignoreWhitespace bool
}

type patchOperationType string

const (
	patchOperationUpdate patchOperationType = "update"
	patchOperationAdd    patchOperationType = "add"
)

type patchOperation struct {
	typeName patchOperationType
	path     string
	hunks    []patchHunk
}

type patchHunk struct {
	before        []string
	after         []string
	rawLines      []string
	header        string
	rawPatchLines []string
}

type hunkStatus struct {
	number int
	status string
}

type applyPatchResult struct {
	status string
	path   string
}

type applyPatchError struct {
	Code            string
	Message         string
	RelativePath    string
	OriginalContent string
	HunkStatuses    []hunkStatus
	FailedHunkLines []string
}

func (e *applyPatchError) Error() string {
	if e == nil {
		return "unknown apply_patch error"
	}
	msg := strings.TrimSpace(e.Message)
	if msg == "" {
		msg = "apply_patch failed"
	}
	return msg
}

func applyPatchCommand(ctx context.Context, req InternalCommandRequest) (PlanObservationPayload, error) {
	options, patchInput, err := parseApplyPatchRequest(req)
	if err != nil {
		return buildApplyPatchFailure(err), err
	}

	operations, err := parsePatch(patchInput)
	if err != nil {
		return buildApplyPatchFailure(err), err
	}
	if len(operations) == 0 {
		err := errors.New("apply_patch: no patch operations detected")
		return buildApplyPatchFailure(err), err
	}

	results, err := applyOperations(operations, options)
	if err != nil {
		return buildApplyPatchFailure(err), err
	}

	stdout := "No changes applied."
	if len(results) > 0 {
		sort.Slice(results, func(i, j int) bool {
			return results[i].path < results[j].path
		})
		lines := []string{"Success. Updated the following files:"}
		for _, result := range results {
			lines = append(lines, fmt.Sprintf("%s %s", result.status, result.path))
		}
		stdout = strings.Join(lines, "\n")
	}

	zero := 0
	return PlanObservationPayload{Stdout: stdout, ExitCode: &zero}, nil
}

func buildApplyPatchFailure(err error) PlanObservationPayload {
	message := formatApplyPatchError(err)
	one := 1
	return PlanObservationPayload{Stderr: message, ExitCode: &one, Details: message}
}

func parseApplyPatchRequest(req InternalCommandRequest) (applyPatchOptions, string, error) {
	options := applyPatchOptions{ignoreWhitespace: true}

	raw := req.Step.Command.Run
	raw = strings.TrimLeft(raw, "\r\n\t ")
	if raw == "" {
		return options, "", errors.New("apply_patch: no input provided")
	}

	patchStart := strings.Index(raw, "*** Begin Patch")
	commandSection := raw
	patchSection := ""
	if patchStart >= 0 {
		commandSection = strings.TrimSpace(raw[:patchStart])
		patchSection = raw[patchStart:]
	} else if idx := strings.Index(raw, "\n"); idx >= 0 {
		commandSection = strings.TrimSpace(raw[:idx])
		patchSection = raw[idx+1:]
	}

	if commandSection == "" {
		return options, "", errors.New("apply_patch: missing command arguments")
	}

	tokens, err := tokenizeInternalCommand(commandSection)
	if err != nil {
		return options, "", fmt.Errorf("apply_patch: %w", err)
	}
	if len(tokens) == 0 {
		return options, "", errors.New("apply_patch: missing command name")
	}

	for _, token := range tokens[1:] {
		switch token {
		case "--ignore-whitespace", "-w":
			options.ignoreWhitespace = true
		case "--respect-whitespace", "-W", "--no-ignore-whitespace":
			options.ignoreWhitespace = false
		case "--help", "-h":
			return options, "", errors.New("Usage: apply_patch [--respect-whitespace]\n\nReads a *** Begin Patch block from the command body and applies it to the workspace.\n  --ignore-whitespace, -w   Match hunks without considering whitespace differences (default).\n  --respect-whitespace, -W  Require whitespace to match before applying hunks.")
		default:
			return options, "", fmt.Errorf("apply_patch: unknown option: %s", token)
		}
	}

	if patchSection == "" {
		if value, ok := req.Args["patch"]; ok {
			patchSection = fmt.Sprint(value)
		} else if value, ok := req.Args["diff"]; ok {
			patchSection = fmt.Sprint(value)
		}
	}

	if strings.TrimSpace(patchSection) == "" {
		return options, "", errors.New("apply_patch: no patch provided")
	}

	return options, patchSection, nil
}

func normalizeLine(line string, options applyPatchOptions) string {
	if !options.ignoreWhitespace {
		return line
	}
	var b strings.Builder
	b.Grow(len(line))
	for _, r := range line {
		if !unicode.IsSpace(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func parsePatch(input string) ([]patchOperation, error) {
	normalized := strings.ReplaceAll(input, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")

	var operations []patchOperation
	inside := false
	var currentOp *patchOperation
	var currentHunk *struct {
		header string
		lines  []string
	}

	flushHunk := func() error {
		if currentHunk == nil {
			return nil
		}
		parsed, err := parseHunk(currentHunk.lines, currentOp.path, currentHunk.header)
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

	for _, rawLine := range lines {
		line := rawLine
		if line == "*** Begin Patch" {
			inside = true
			continue
		}
		if line == "*** End Patch" {
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
			if updateMatch := strings.TrimPrefix(line, "*** Update File:"); updateMatch != line {
				path := strings.TrimSpace(updateMatch)
				currentOp = &patchOperation{typeName: patchOperationUpdate, path: path}
				continue
			}
			if addMatch := strings.TrimPrefix(line, "*** Add File:"); addMatch != line {
				path := strings.TrimSpace(addMatch)
				currentOp = &patchOperation{typeName: patchOperationAdd, path: path}
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
			currentHunk = &struct {
				header string
				lines  []string
			}{header: line, lines: nil}
			continue
		}

		if currentHunk == nil {
			currentHunk = &struct {
				header string
				lines  []string
			}{header: "", lines: nil}
		}
		currentHunk.lines = append(currentHunk.lines, line)
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
	var before []string
	var after []string
	for _, raw := range lines {
		switch {
		case strings.HasPrefix(raw, "+"):
			after = append(after, raw[1:])
		case strings.HasPrefix(raw, "-"):
			before = append(before, raw[1:])
		case strings.HasPrefix(raw, " "):
			value := raw[1:]
			before = append(before, value)
			after = append(after, value)
		case raw == "\\ No newline at end of file":
			continue
		default:
			return patchHunk{}, fmt.Errorf("apply_patch: unsupported hunk line in %s: %q", filePath, raw)
		}
	}

	rawPatchLines := make([]string, 0, len(lines)+1)
	if header != "" {
		rawPatchLines = append(rawPatchLines, header)
	}
	rawPatchLines = append(rawPatchLines, lines...)

	return patchHunk{
		before:        before,
		after:         after,
		rawLines:      append([]string(nil), lines...),
		header:        header,
		rawPatchLines: rawPatchLines,
	}, nil
}

func findSubsequence(haystack, needle []string, startIndex int) int {
	if len(needle) == 0 {
		return -1
	}
	if startIndex < 0 {
		startIndex = 0
	}
	for i := startIndex; i <= len(haystack)-len(needle); i++ {
		matched := true
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				matched = false
				break
			}
		}
		if matched {
			return i
		}
	}
	return -1
}

type fileState struct {
	path                    string
	relativePath            string
	lines                   []string
	normalizedLines         []string
	originalContent         string
	originalEndsWithNewline *bool
	touched                 bool
	cursor                  int
	hunkStatuses            []hunkStatus
	isNew                   bool
	options                 applyPatchOptions
}

func ensureNormalizedLines(state *fileState) []string {
	if !state.options.ignoreWhitespace {
		return state.lines
	}
	if state.normalizedLines == nil {
		state.normalizedLines = make([]string, len(state.lines))
		for i, line := range state.lines {
			state.normalizedLines[i] = normalizeLine(line, state.options)
		}
	}
	return state.normalizedLines
}

func updateNormalizedLines(state *fileState, index, deleteCount int, replacement []string) {
	if !state.options.ignoreWhitespace {
		return
	}
	normalized := ensureNormalizedLines(state)
	replacementNormalized := make([]string, len(replacement))
	for i, line := range replacement {
		replacementNormalized[i] = normalizeLine(line, state.options)
	}
	state.normalizedLines = append(append(append([]string{}, normalized[:index]...), replacementNormalized...), normalized[index+deleteCount:]...)
}

func applyHunk(state *fileState, hunk patchHunk) error {
	before := hunk.before
	after := hunk.after

	if len(before) == 0 {
		insertionIndex := len(state.lines)
		if insertionIndex > 0 && state.lines[len(state.lines)-1] == "" {
			insertionIndex--
		}
		state.lines = append(append([]string{}, state.lines[:insertionIndex]...), append(after, state.lines[insertionIndex:]...)...)
		updateNormalizedLines(state, insertionIndex, 0, after)
		state.cursor = insertionIndex + len(after)
		return nil
	}

	matchIndex := findSubsequence(state.lines, before, state.cursor)
	if matchIndex == -1 {
		matchIndex = findSubsequence(state.lines, before, 0)
	}

	if matchIndex == -1 && state.options.ignoreWhitespace {
		normalizedBefore := make([]string, len(before))
		for i, line := range before {
			normalizedBefore[i] = normalizeLine(line, state.options)
		}
		normalizedLines := ensureNormalizedLines(state)
		matchIndex = findSubsequence(normalizedLines, normalizedBefore, state.cursor)
		if matchIndex == -1 {
			matchIndex = findSubsequence(normalizedLines, normalizedBefore, 0)
		}
	}

	if matchIndex == -1 {
		return &applyPatchError{
			Code:            "HUNK_NOT_FOUND",
			Message:         fmt.Sprintf("Hunk not found in %s.", state.relativePath),
			RelativePath:    state.relativePath,
			OriginalContent: state.originalContent,
		}
	}

	replacement := append([]string{}, after...)
	state.lines = append(append(append([]string{}, state.lines[:matchIndex]...), replacement...), state.lines[matchIndex+len(before):]...)
	updateNormalizedLines(state, matchIndex, len(before), replacement)
	state.cursor = matchIndex + len(after)
	return nil
}

func applyOperations(operations []patchOperation, options applyPatchOptions) ([]applyPatchResult, error) {
	states := make(map[string]*fileState)

	ensureFileState := func(relativePath string, create bool) (*fileState, error) {
		cleanedRel := strings.TrimSpace(relativePath)
		if cleanedRel == "" {
			return nil, errors.New("apply_patch: empty file path in patch")
		}
		absPath, err := filepath.Abs(filepath.FromSlash(cleanedRel))
		if err != nil {
			return nil, fmt.Errorf("apply_patch: failed to resolve %s: %w", relativePath, err)
		}

		if state, ok := states[absPath]; ok {
			state.options = options
			if options.ignoreWhitespace {
				state.normalizedLines = nil
				ensureNormalizedLines(state)
			} else {
				state.normalizedLines = nil
			}
			return state, nil
		}

		if create {
			if _, err := os.Stat(absPath); err == nil {
				return nil, fmt.Errorf("apply_patch: cannot add %s because it already exists", relativePath)
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("apply_patch: failed to stat %s: %w", relativePath, err)
			}
			state := &fileState{
				path:            absPath,
				relativePath:    cleanedRel,
				lines:           []string{},
				normalizedLines: nil,
				originalContent: "",
				touched:         false,
				cursor:          0,
				hunkStatuses:    nil,
				isNew:           true,
				options:         options,
			}
			if options.ignoreWhitespace {
				state.normalizedLines = []string{}
			}
			states[absPath] = state
			return state, nil
		}

		contentBytes, err := os.ReadFile(absPath)
		if err != nil {
			return nil, fmt.Errorf("apply_patch: failed to read %s: %w", relativePath, err)
		}
		content := string(contentBytes)
		normalized := strings.ReplaceAll(content, "\r\n", "\n")
		lines := strings.Split(normalized, "\n")
		var endsWithNewline *bool
		ends := strings.HasSuffix(normalized, "\n")
		endsWithNewline = &ends
		state := &fileState{
			path:                    absPath,
			relativePath:            cleanedRel,
			lines:                   append([]string{}, lines...),
			normalizedLines:         nil,
			originalContent:         content,
			originalEndsWithNewline: endsWithNewline,
			touched:                 false,
			cursor:                  0,
			hunkStatuses:            nil,
			isNew:                   false,
			options:                 options,
		}
		if options.ignoreWhitespace {
			ensureNormalizedLines(state)
		}
		states[absPath] = state
		return state, nil
	}

	for _, op := range operations {
		if op.typeName != patchOperationUpdate && op.typeName != patchOperationAdd {
			return nil, fmt.Errorf("apply_patch: unsupported patch operation for %s: %s", op.path, op.typeName)
		}
		create := op.typeName == patchOperationAdd
		state, err := ensureFileState(op.path, create)
		if err != nil {
			return nil, err
		}
		state.cursor = 0
		state.hunkStatuses = nil
		for idx, hunk := range op.hunks {
			number := idx + 1
			if err := applyHunk(state, hunk); err != nil {
				return nil, enhanceHunkError(err, state, hunk, number)
			}
			state.hunkStatuses = append(state.hunkStatuses, hunkStatus{number: number, status: "applied"})
			state.touched = true
		}
	}

	var results []applyPatchResult
	for _, state := range states {
		if !state.touched {
			continue
		}
		newContent := strings.Join(state.lines, "\n")
		if state.originalEndsWithNewline != nil {
			if *state.originalEndsWithNewline && !strings.HasSuffix(newContent, "\n") {
				newContent += "\n"
			} else if !*state.originalEndsWithNewline && strings.HasSuffix(newContent, "\n") {
				newContent = strings.TrimSuffix(newContent, "\n")
			}
		}

		if err := os.MkdirAll(filepath.Dir(state.path), 0o755); err != nil {
			return nil, fmt.Errorf("apply_patch: failed to create parent directories for %s: %w", state.relativePath, err)
		}

		mode := os.FileMode(0o644)
		if !state.isNew {
			if info, err := os.Stat(state.path); err == nil {
				mode = info.Mode()
			}
		}
		if err := os.WriteFile(state.path, []byte(newContent), mode); err != nil {
			return nil, fmt.Errorf("apply_patch: failed to write %s: %w", state.relativePath, err)
		}
		status := "M"
		if state.isNew {
			status = "A"
		}
		displayPath := state.relativePath
		if displayPath == "" {
			displayPath = state.path
		}
		results = append(results, applyPatchResult{status: status, path: filepath.ToSlash(displayPath)})
	}

	return results, nil
}

func enhanceHunkError(err error, state *fileState, hunk patchHunk, hunkNumber int) error {
	var apErr *applyPatchError
	if !errors.As(err, &apErr) {
		apErr = &applyPatchError{Message: err.Error()}
	}
	if apErr.Code == "" {
		apErr.Code = "HUNK_NOT_FOUND"
	}
	if apErr.RelativePath == "" {
		apErr.RelativePath = state.relativePath
	}
	if apErr.OriginalContent == "" {
		if state.originalContent != "" || !state.isNew {
			apErr.OriginalContent = state.originalContent
		} else {
			apErr.OriginalContent = strings.Join(state.lines, "\n")
		}
	}
	statuses := append([]hunkStatus{}, state.hunkStatuses...)
	statuses = append(statuses, hunkStatus{number: hunkNumber, status: "no-match"})
	apErr.HunkStatuses = statuses
	if len(apErr.FailedHunkLines) == 0 && len(hunk.rawPatchLines) > 0 {
		apErr.FailedHunkLines = append([]string{}, hunk.rawPatchLines...)
	}
	return apErr
}

func describeHunkStatuses(statuses []hunkStatus) string {
	if len(statuses) == 0 {
		return ""
	}
	var applied []string
	var failed *hunkStatus
	for i := range statuses {
		status := statuses[i]
		if status.status == "applied" {
			applied = append(applied, strconv.Itoa(status.number))
			continue
		}
		if failed == nil {
			copy := status
			failed = &copy
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

func formatApplyPatchError(err error) string {
	if err == nil {
		return "Unknown error occurred."
	}
	var apErr *applyPatchError
	if errors.As(err, &apErr) {
		return formatDetailedApplyPatchError(apErr)
	}
	msg := strings.TrimSpace(err.Error())
	if msg == "" {
		msg = "Unknown error occurred."
	}
	return msg
}

func formatDetailedApplyPatchError(err *applyPatchError) string {
	if err == nil {
		return "Unknown error occurred."
	}
	message := strings.TrimSpace(err.Message)
	if message == "" {
		message = "Unknown error occurred."
	}
	parts := []string{message}
	if summary := describeHunkStatuses(err.HunkStatuses); summary != "" {
		parts = append(parts, "", summary)
	}
	if len(err.FailedHunkLines) > 0 {
		parts = append(parts, "", "Offending hunk:")
		parts = append(parts, strings.Join(err.FailedHunkLines, "\n"))
	}
	relativePath := err.RelativePath
	if relativePath == "" {
		relativePath = "unknown file"
	}
	displayPath := relativePath
	if !strings.HasPrefix(displayPath, "./") {
		displayPath = "./" + displayPath
	}
	parts = append(parts, "", fmt.Sprintf("Full content of file: %s::::", displayPath), err.OriginalContent)
	return strings.Join(parts, "\n")
}
