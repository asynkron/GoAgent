package patch

import (
	"errors"
	"fmt"
	"strings"
)

// OperationType identifies the kind of change described by a patch operation.
type OperationType string

const (
	// OperationAdd represents an "*** Add File" directive.
	OperationAdd OperationType = "add"
	// OperationUpdate represents an "*** Update File" directive.
	OperationUpdate OperationType = "update"
	// OperationDelete represents an "*** Delete File" directive.
	OperationDelete OperationType = "delete"
)

// Operation describes a high-level instruction contained in a patch payload.
//
// The exported fields make it possible to inspect the parsed structure when
// building tooling around the parser.
type Operation struct {
	Type     OperationType
	Path     string
	MovePath string
	Hunks    []Hunk
}

// Hunk captures a unified-diff hunk belonging to an Operation.
type Hunk struct {
	Header        string
	Lines         []string
	RawPatchLines []string
	Before        []string
	After         []string
	AtEOF         bool
}

// HunkStatus tracks how a hunk was applied when processing a patch.
type HunkStatus struct {
	Number int    `json:"number"`
	Status string `json:"status"`
}

// FailedHunk stores the raw lines of the hunk that could not be applied.
type FailedHunk struct {
	Number        int      `json:"number"`
	RawPatchLines []string `json:"rawPatchLines"`
}

// Error represents a structured failure while applying a patch. It satisfies
// the error interface so it can be returned directly from Apply* helpers.
type Error struct {
	Message         string
	Code            string
	RelativePath    string
	OriginalContent string
	HunkStatuses    []HunkStatus
	FailedHunk      *FailedHunk
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return "patch error"
}

// Options configure how the patch application behaves for both filesystem and
// in-memory operations.
type Options struct {
	IgnoreWhitespace bool
}

// FilesystemOptions augments Options with a working directory used to resolve
// relative paths when touching the local filesystem.
type FilesystemOptions struct {
	Options
	WorkingDir string
}

// Result describes the outcome for a single file when applying a patch.
type Result struct {
	Status string
	Path   string
}

// Parse converts the textual representation of an apply_patch payload into a
// slice of operations that can later be applied.
func Parse(input string) ([]Operation, error) {
	lines := splitLines(input)
	var (
		operations  []Operation
		currentOp   *Operation
		currentHunk *Hunk
		inside      bool
	)

	flushHunk := func() error {
		if currentHunk == nil {
			return nil
		}
		if currentOp == nil {
			return errors.New("hunk encountered before file directive")
		}
		parsed, err := parseHunk(currentHunk.Lines, currentOp.Path, currentHunk.Header)
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
		if len(currentOp.Hunks) == 0 && (currentOp.Type != OperationUpdate || strings.TrimSpace(currentOp.MovePath) == "") {
			return fmt.Errorf("no hunks provided for %s", currentOp.Path)
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

		trimmed := strings.TrimSpace(line)

		if trimmed == "*** End of File" {
			if currentOp == nil {
				return nil, fmt.Errorf("end-of-file marker encountered before a file directive")
			}
			if currentHunk == nil {
				currentHunk = &Hunk{}
			}
			currentHunk.Lines = append(currentHunk.Lines, line)
			continue
		}

		if strings.HasPrefix(trimmed, "*** Move to: ") {
			if currentOp == nil {
				return nil, fmt.Errorf("move directive encountered before a file directive")
			}
			if currentOp.Type != OperationUpdate {
				return nil, fmt.Errorf("move directive only allowed for update operations")
			}
			currentOp.MovePath = strings.TrimSpace(strings.TrimPrefix(trimmed, "*** Move to: "))
			continue
		}

		if strings.HasPrefix(trimmed, "*** Delete File: ") {
			if err := flushOp(); err != nil {
				return nil, err
			}
			path := strings.TrimSpace(strings.TrimPrefix(trimmed, "*** Delete File: "))
			operations = append(operations, Operation{Type: OperationDelete, Path: path})
			currentOp = nil
			currentHunk = nil
			continue
		}

		if strings.HasPrefix(trimmed, "*** ") {
			if err := flushOp(); err != nil {
				return nil, err
			}
			if updatePath, ok := strings.CutPrefix(trimmed, "*** Update File: "); ok {
				path := strings.TrimSpace(updatePath)
				currentOp = &Operation{Type: OperationUpdate, Path: path}
				continue
			}
			if addPath, ok := strings.CutPrefix(trimmed, "*** Add File: "); ok {
				path := strings.TrimSpace(addPath)
				currentOp = &Operation{Type: OperationAdd, Path: path}
				continue
			}
			return nil, fmt.Errorf("unsupported patch directive: %s", line)
		}

		if currentOp == nil {
			if trimmed == "" {
				continue
			}
			return nil, fmt.Errorf("diff content appeared before a file directive: %q", line)
		}

		if strings.HasPrefix(line, "@@") {
			if err := flushHunk(); err != nil {
				return nil, err
			}
			currentHunk = &Hunk{Header: line}
			continue
		}

		if currentHunk == nil {
			currentHunk = &Hunk{}
		}
		currentHunk.Lines = append(currentHunk.Lines, line)
	}

	if inside {
		return nil, errors.New("missing *** End Patch terminator")
	}

	if err := flushOp(); err != nil {
		return nil, err
	}

	return operations, nil
}

func parseHunk(lines []string, filePath, header string) (Hunk, error) {
	hunk := Hunk{Header: header}
	hunk.Lines = append([]string(nil), lines...)
	for _, raw := range lines {
		switch {
		case strings.HasPrefix(raw, "+"):
			hunk.After = append(hunk.After, raw[1:])
		case strings.HasPrefix(raw, "-"):
			hunk.Before = append(hunk.Before, raw[1:])
		case strings.HasPrefix(raw, " "):
			value := raw[1:]
			hunk.Before = append(hunk.Before, value)
			hunk.After = append(hunk.After, value)
		case strings.TrimSpace(raw) == "*** End of File":
			hunk.AtEOF = true
		case raw == "\\ No newline at end of file":
			// ignore marker
		default:
			return Hunk{}, fmt.Errorf("unsupported hunk line in %s: %q", filePath, raw)
		}
	}
	if header != "" {
		hunk.RawPatchLines = append(hunk.RawPatchLines, header)
	}
	hunk.RawPatchLines = append(hunk.RawPatchLines, lines...)
	return hunk, nil
}

func splitLines(input string) []string {
	normalized := strings.ReplaceAll(input, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	return strings.Split(normalized, "\n")
}
