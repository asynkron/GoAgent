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
)

const (
	applyPatchCommandName = "apply_patch"
)

type applyPatchOptions struct {
	ignoreWhitespace bool
	workingDir       string
}

type patchOperation struct {
	opType string
	path   string
	hunks  []patchHunk
}

type patchHunk struct {
	before        []string
	after         []string
	header        string
	lines         []string
	rawPatchLines []string
}

type hunkStatus struct {
	Number int    `json:"number"`
	Status string `json:"status"`
}

type failedHunk struct {
	Number        int      `json:"number"`
	RawPatchLines []string `json:"rawPatchLines"`
}

type patchError struct {
	Message         string
	Code            string
	RelativePath    string
	OriginalContent string
	HunkStatuses    []hunkStatus
	FailedHunk      *failedHunk
}

func (e *patchError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return "patch error"
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

type fileResult struct {
	status string
	path   string
}

func newApplyPatchCommand() InternalCommandHandler {
	return func(ctx context.Context, req InternalCommandRequest) (PlanObservationPayload, error) {
		payload := PlanObservationPayload{}

		commandLine, patchInput := splitCommandAndPatch(req.Raw)
		if strings.TrimSpace(commandLine) == "" {
			return failApplyPatch(&payload, "internal command: apply_patch requires a command line"), errors.New("apply_patch: missing command line")
		}

		opts, err := parseApplyPatchOptions(commandLine, req.Step.Command.Cwd)
		if err != nil {
			return failApplyPatch(&payload, err.Error()), err
		}

		if strings.TrimSpace(patchInput) == "" {
			err := errors.New("apply_patch: no patch provided")
			return failApplyPatch(&payload, err.Error()), err
		}

		operations, err := parsePatch(patchInput)
		if err != nil {
			message := fmt.Sprintf("apply_patch: %v", err)
			return failApplyPatch(&payload, message), fmt.Errorf("apply_patch: %w", err)
		}

		if len(operations) == 0 {
			err := errors.New("apply_patch: no patch operations detected")
			return failApplyPatch(&payload, err.Error()), err
		}

		results, applyErr := applyPatchOperations(ctx, operations, opts)
		if applyErr != nil {
			formatted := formatPatchError(applyErr)
			return failApplyPatch(&payload, formatted), applyErr
		}

		if len(results) == 0 {
			payload.Stdout = "No changes applied."
			zero := 0
			payload.ExitCode = &zero
			return payload, nil
		}

		sort.Slice(results, func(i, j int) bool {
			return results[i].path < results[j].path
		})

		builder := strings.Builder{}
		builder.WriteString("Success. Updated the following files:\n")
		for _, entry := range results {
			builder.WriteString(entry.status)
			builder.WriteString(" ")
			builder.WriteString(entry.path)
			builder.WriteString("\n")
		}

		payload.Stdout = strings.TrimRight(builder.String(), "\n")
		zero := 0
		payload.ExitCode = &zero
		return payload, nil
	}
}

func failApplyPatch(payload *PlanObservationPayload, message string) PlanObservationPayload {
	if payload == nil {
		payload = &PlanObservationPayload{}
	}
	payload.Stderr = message
	payload.Details = message
	one := 1
	payload.ExitCode = &one
	return *payload
}

func splitCommandAndPatch(raw string) (commandLine, patch string) {
	trimmed := strings.TrimLeftFunc(raw, unicode.IsSpace)
	if trimmed == "" {
		return "", ""
	}
	line, rest, found := strings.Cut(trimmed, "\n")
	if !found {
		return trimmed, ""
	}
	return line, rest
}

func parseApplyPatchOptions(commandLine, cwd string) (applyPatchOptions, error) {
	tokens, err := tokenizeInternalCommand(commandLine)
	if err != nil {
		return applyPatchOptions{}, fmt.Errorf("failed to parse command line: %w", err)
	}
	if len(tokens) == 0 {
		return applyPatchOptions{}, errors.New("apply_patch: missing command name")
	}

	workingDir := strings.TrimSpace(cwd)
	if workingDir == "" {
		if wd, getErr := os.Getwd(); getErr == nil {
			workingDir = wd
		} else {
			return applyPatchOptions{}, fmt.Errorf("failed to determine working directory: %w", getErr)
		}
	}
	if abs, err := filepath.Abs(workingDir); err == nil {
		workingDir = abs
	}

	opts := applyPatchOptions{ignoreWhitespace: true, workingDir: workingDir}
	for _, token := range tokens[1:] {
		if eq := strings.IndexRune(token, '='); eq != -1 {
			key := strings.TrimSpace(token[:eq])
			value := strings.TrimSpace(token[eq+1:])
			switch strings.ToLower(key) {
			case "ignore_whitespace", "ignore-whitespace":
				if strings.EqualFold(value, "false") {
					opts.ignoreWhitespace = false
				} else if strings.EqualFold(value, "true") {
					opts.ignoreWhitespace = true
				}
			case "respect_whitespace", "respect-whitespace":
				if strings.EqualFold(value, "true") {
					opts.ignoreWhitespace = false
				}
			}
			continue
		}

		switch token {
		case "--ignore-whitespace", "-w":
			opts.ignoreWhitespace = true
		case "--respect-whitespace", "--no-ignore-whitespace":
			opts.ignoreWhitespace = false
		case "-W":
			opts.ignoreWhitespace = false
		default:
			lower := strings.ToLower(token)
			if lower == "--respect-whitespace" || lower == "--no-ignore-whitespace" {
				opts.ignoreWhitespace = false
			} else if lower == "--ignore-whitespace" {
				opts.ignoreWhitespace = true
			}
		}
	}
	return opts, nil
}

func parsePatch(input string) ([]patchOperation, error) {
	lines := splitLines(input)
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
		if currentOp == nil {
			return errors.New("hunk encountered before file directive")
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
			return fmt.Errorf("no hunks provided for %s", currentOp.path)
		}
		operations = append(operations, *currentOp)
		currentOp = nil
		return nil
	}

	for _, rawLine := range lines {
		line := rawLine
		switch line {
		case "*** Begin Patch":
			inside = true
			continue
		case "*** End Patch":
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
			if updatePath, ok := strings.CutPrefix(line, "*** Update File: "); ok {
				path := strings.TrimSpace(updatePath)
				currentOp = &patchOperation{opType: "update", path: path}
				continue
			}
			if addPath, ok := strings.CutPrefix(line, "*** Add File: "); ok {
				path := strings.TrimSpace(addPath)
				currentOp = &patchOperation{opType: "add", path: path}
				continue
			}
			return nil, fmt.Errorf("unsupported patch directive: %s", line)
		}

		if currentOp == nil {
			if strings.TrimSpace(line) == "" {
				continue
			}
			return nil, fmt.Errorf("diff content appeared before a file directive: %q", line)
		}

		if strings.HasPrefix(line, "@@") {
			if err := flushHunk(); err != nil {
				return nil, err
			}
			currentHunk = &patchHunk{header: line}
			continue
		}

		if currentHunk == nil {
			currentHunk = &patchHunk{}
		}
		currentHunk.lines = append(currentHunk.lines, line)
	}

	if inside {
		return nil, errors.New("missing *** End Patch terminator")
	}

	if err := flushOp(); err != nil {
		return nil, err
	}

	return operations, nil
}

func parseHunk(lines []string, filePath, header string) (patchHunk, error) {
	hunk := patchHunk{header: header}
	hunk.lines = append([]string(nil), lines...)
	for _, raw := range lines {
		switch {
		case strings.HasPrefix(raw, "+"):
			hunk.after = append(hunk.after, raw[1:])
		case strings.HasPrefix(raw, "-"):
			hunk.before = append(hunk.before, raw[1:])
		case strings.HasPrefix(raw, " "):
			value := raw[1:]
			hunk.before = append(hunk.before, value)
			hunk.after = append(hunk.after, value)
		case raw == "\\ No newline at end of file":
			// ignore marker
		default:
			return patchHunk{}, fmt.Errorf("unsupported hunk line in %s: %q", filePath, raw)
		}
	}
	if header != "" {
		hunk.rawPatchLines = append(hunk.rawPatchLines, header)
	}
	hunk.rawPatchLines = append(hunk.rawPatchLines, lines...)
	return hunk, nil
}

func splitLines(input string) []string {
	normalized := strings.ReplaceAll(input, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	return strings.Split(normalized, "\n")
}

func applyPatchOperations(ctx context.Context, operations []patchOperation, opts applyPatchOptions) ([]fileResult, *patchError) {
	states := make(map[string]*fileState)

	ensureState := func(relativePath string, create bool) (*fileState, error) {
		rel := strings.TrimSpace(relativePath)
		if rel == "" {
			return nil, fmt.Errorf("invalid patch path")
		}

		var abs string
		if filepath.IsAbs(rel) {
			abs = filepath.Clean(rel)
		} else {
			abs = filepath.Clean(filepath.Join(opts.workingDir, rel))
		}

		if state, ok := states[abs]; ok {
			state.options = opts
			if opts.ignoreWhitespace {
				state.normalizedLines = ensureNormalizedLines(state)
			} else {
				state.normalizedLines = nil
			}
			return state, nil
		}

		info, err := os.Stat(abs)
		switch {
		case err == nil && create:
			return nil, fmt.Errorf("cannot add %s because it already exists", rel)
		case err == nil:
			if info.IsDir() {
				return nil, fmt.Errorf("cannot patch directory %s", rel)
			}
			content, readErr := os.ReadFile(abs)
			if readErr != nil {
				return nil, fmt.Errorf("failed to read %s: %v", rel, readErr)
			}
			normalized := strings.ReplaceAll(string(content), "\r\n", "\n")
			normalized = strings.ReplaceAll(normalized, "\r", "\n")
			lines := strings.Split(normalized, "\n")
			ends := strings.HasSuffix(normalized, "\n")
			state := &fileState{
				path:                    abs,
				relativePath:            rel,
				lines:                   lines,
				normalizedLines:         nil,
				originalContent:         string(content),
				originalEndsWithNewline: &ends,
				touched:                 false,
				cursor:                  0,
				hunkStatuses:            nil,
				isNew:                   false,
				options:                 opts,
			}
			if opts.ignoreWhitespace {
				state.normalizedLines = ensureNormalizedLines(state)
			}
			states[abs] = state
			return state, nil
		case errors.Is(err, fs.ErrNotExist):
			if !create {
				return nil, fmt.Errorf("failed to read %s: file does not exist", rel)
			}
			state := &fileState{
				path:                    abs,
				relativePath:            rel,
				lines:                   []string{},
				normalizedLines:         nil,
				originalContent:         "",
				originalEndsWithNewline: nil,
				touched:                 false,
				cursor:                  0,
				hunkStatuses:            nil,
				isNew:                   true,
				options:                 opts,
			}
			if opts.ignoreWhitespace {
				state.normalizedLines = []string{}
			}
			states[abs] = state
			return state, nil
		default:
			return nil, fmt.Errorf("failed to stat %s: %v", rel, err)
		}
	}
	for _, op := range operations {
		if ctx.Err() != nil {
			return nil, &patchError{Message: ctx.Err().Error()}
		}

		if op.opType != "update" && op.opType != "add" {
			return nil, &patchError{Message: fmt.Sprintf("unsupported patch operation for %s: %s", op.path, op.opType)}
		}

		state, err := ensureState(op.path, op.opType == "add")
		if err != nil {
			return nil, &patchError{Message: err.Error()}
		}

		state.cursor = 0
		state.hunkStatuses = nil
		for index, hunk := range op.hunks {
			hunkNumber := index + 1
			if ctx.Err() != nil {
				return nil, &patchError{Message: ctx.Err().Error()}
			}
			if err := applyHunk(state, hunk); err != nil {
				return nil, enhanceHunkError(err, state, hunk, hunkNumber)
			}
			state.hunkStatuses = append(state.hunkStatuses, hunkStatus{Number: hunkNumber, Status: "applied"})
			state.touched = true
		}
	}

	var results []fileResult
	for _, state := range states {
		if !state.touched {
			continue
		}
		newContent := strings.Join(state.lines, "\n")
		if state.originalEndsWithNewline != nil {
			if *state.originalEndsWithNewline && !strings.HasSuffix(newContent, "\n") {
				newContent += "\n"
			}
			if !*state.originalEndsWithNewline && strings.HasSuffix(newContent, "\n") {
				newContent = strings.TrimSuffix(newContent, "\n")
			}
		}

		if err := os.MkdirAll(filepath.Dir(state.path), 0o755); err != nil {
			return nil, &patchError{Message: fmt.Sprintf("failed to create directory for %s: %v", state.relativePath, err)}
		}

		if err := os.WriteFile(state.path, []byte(newContent), 0o644); err != nil {
			return nil, &patchError{Message: fmt.Sprintf("failed to write %s: %v", state.relativePath, err)}
		}

		status := "M"
		if state.isNew {
			status = "A"
		}
		results = append(results, fileResult{status: status, path: state.relativePath})
	}

	return results, nil
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

func ensureNormalizedLines(state *fileState) []string {
	if state == nil {
		return nil
	}
	if !state.options.ignoreWhitespace {
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

func updateNormalizedLines(state *fileState, index, deleteCount int, replacement []string) {
	if state == nil || !state.options.ignoreWhitespace {
		return
	}
	normalized := ensureNormalizedLines(state)
	replacementNormalized := make([]string, len(replacement))
	for i, line := range replacement {
		replacementNormalized[i] = normalizeLine(line)
	}
	state.normalizedLines = splice(normalized, index, deleteCount, replacementNormalized)
}

func applyHunk(state *fileState, hunk patchHunk) error {
	if state == nil {
		return errors.New("missing file state")
	}

	before := hunk.before
	after := hunk.after

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

	matchIndex := findSubsequence(state.lines, before, state.cursor)
	if matchIndex == -1 {
		matchIndex = findSubsequence(state.lines, before, 0)
	}

	if matchIndex == -1 && state.options.ignoreWhitespace {
		normalizedBefore := make([]string, len(before))
		for i, line := range before {
			normalizedBefore[i] = normalizeLine(line)
		}
		normalizedLines := ensureNormalizedLines(state)
		matchIndex = findSubsequence(normalizedLines, normalizedBefore, state.cursor)
		if matchIndex == -1 {
			matchIndex = findSubsequence(normalizedLines, normalizedBefore, 0)
		}
	}

	if matchIndex == -1 {
		message := fmt.Sprintf("Hunk not found in %s.", state.relativePath)
		original := state.originalContent
		if original == "" {
			original = strings.Join(state.lines, "\n")
		}
		return &patchError{
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

func findSubsequence(haystack, needle []string, startIndex int) int {
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
			return i
		}
	}
	return -1
}

func enhanceHunkError(err error, state *fileState, hunk patchHunk, number int) *patchError {
	var pe *patchError
	if errors.As(err, &pe) {
		// Use the existing error instance so we preserve any preset metadata.
	} else {
		pe = &patchError{Message: err.Error()}
	}

	statuses := append([]hunkStatus{}, state.hunkStatuses...)
	if pe != nil && len(pe.HunkStatuses) > 0 {
		statuses = append(statuses, pe.HunkStatuses...)
	}
	statuses = append(statuses, hunkStatus{Number: number, Status: "no-match"})
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
		rawLines := append([]string(nil), hunk.rawPatchLines...)
		pe.FailedHunk = &failedHunk{Number: number, RawPatchLines: rawLines}
	}
	return pe
}

func describeHunkStatuses(statuses []hunkStatus) string {
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

func formatPatchError(err *patchError) string {
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

func registerBuiltinInternalCommands(executor *CommandExecutor) error {
	if executor == nil {
		return errors.New("nil executor")
	}
	return executor.RegisterInternalCommand(applyPatchCommandName, newApplyPatchCommand())
}
