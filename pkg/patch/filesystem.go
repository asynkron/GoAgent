package patch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ApplyFilesystem applies operations to the OS filesystem.
func ApplyFilesystem(ctx context.Context, operations []Operation, opts FilesystemOptions) ([]Result, error) {
	ws, err := newFilesystemWorkspace(opts)
	if err != nil {
		return nil, err
	}
	return apply(ctx, operations, ws)
}

// ApplyFilesystemPatch parses a raw patch payload and applies it to the filesystem.
func ApplyFilesystemPatch(ctx context.Context, patchBody string, opts FilesystemOptions) ([]Result, error) {
	operations, err := Parse(patchBody)
	if err != nil {
		return nil, err
	}
	return ApplyFilesystem(ctx, operations, opts)
}

type filesystemWorkspace struct {
	options    Options
	workingDir string
	states     map[string]*state
	deletions  []Result
}

func newFilesystemWorkspace(opts FilesystemOptions) (*filesystemWorkspace, error) {
	workingDir := strings.TrimSpace(opts.WorkingDir)
	if workingDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("failed to determine working directory: %w", err)
		}
		workingDir = wd
	}
	if abs, err := filepath.Abs(workingDir); err == nil {
		workingDir = abs
	}
	return &filesystemWorkspace{
		options:    opts.Options,
		workingDir: workingDir,
		states:     make(map[string]*state),
	}, nil
}

func (ws *filesystemWorkspace) Ensure(path string, create bool) (*state, error) {
	abs, rel, err := ws.resolvePath(path)
	if err != nil {
		return nil, err
	}
	if state, ok := ws.states[abs]; ok {
		state.options = ws.options
		if ws.options.IgnoreWhitespace {
			state.normalizedLines = ensureNormalizedLines(state)
		} else {
			state.normalizedLines = nil
		}
		return state, nil
	}

	info, err := os.Stat(abs)
	switch {
	case err == nil && create:
		if info.IsDir() {
			return nil, fmt.Errorf("cannot add directory %s", rel)
		}
		state := &state{
			path:         abs,
			relativePath: rel,
			lines:        []string{},
			options:      ws.options,
			isNew:        true,
		}
		if ws.options.IgnoreWhitespace {
			state.normalizedLines = []string{}
		}
		ws.states[abs] = state
		return state, nil
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
		state := &state{
			path:                    abs,
			relativePath:            rel,
			lines:                   lines,
			originalContent:         string(content),
			originalEndsWithNewline: &ends,
			originalMode:            info.Mode(),
			options:                 ws.options,
		}
		if ws.options.IgnoreWhitespace {
			state.normalizedLines = ensureNormalizedLines(state)
		}
		ws.states[abs] = state
		return state, nil
	case errors.Is(err, fs.ErrNotExist):
		if !create {
			return nil, fmt.Errorf("failed to read %s: file does not exist", rel)
		}
		state := &state{
			path:         abs,
			relativePath: rel,
			lines:        []string{},
			options:      ws.options,
			isNew:        true,
		}
		if ws.options.IgnoreWhitespace {
			state.normalizedLines = []string{}
		}
		ws.states[abs] = state
		return state, nil
	default:
		return nil, fmt.Errorf("failed to stat %s: %v", rel, err)
	}
}

func (ws *filesystemWorkspace) Delete(path string) error {
	abs, rel, err := ws.resolvePath(path)
	if err != nil {
		return err
	}
	info, statErr := os.Stat(abs)
	if statErr != nil || info.IsDir() {
		return &Error{Message: fmt.Sprintf("Failed to delete file %s", rel)}
	}
	if err := os.Remove(abs); err != nil {
		return &Error{Message: fmt.Sprintf("Failed to delete file %s", rel)}
	}
	ws.deletions = append(ws.deletions, Result{Status: "D", Path: rel})
	return nil
}

func (ws *filesystemWorkspace) Commit() ([]Result, error) {
	results := append([]Result{}, ws.deletions...)
	for _, state := range ws.states {
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

		writePath := state.path
		displayPath := state.relativePath
		moveTarget := strings.TrimSpace(state.movePath)
		if moveTarget != "" {
			abs, rel, err := ws.resolvePath(moveTarget)
			if err != nil {
				return nil, err
			}
			writePath = abs
			displayPath = rel
		}

		if err := os.MkdirAll(filepath.Dir(writePath), 0o755); err != nil {
			return nil, &Error{Message: fmt.Sprintf("failed to create directory for %s: %v", displayPath, err)}
		}

		perm := state.originalMode & fs.ModePerm
		if perm == 0 {
			perm = 0o644
		}

		if err := os.WriteFile(writePath, []byte(newContent), perm); err != nil {
			return nil, &Error{Message: fmt.Sprintf("failed to write %s: %v", displayPath, err)}
		}

		if state.originalMode != 0 {
			desired := (state.originalMode & fs.ModePerm) | (state.originalMode & (fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky))
			if desired == 0 {
				desired = perm
			}

			specialBits := state.originalMode & (fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
			needsChmod := specialBits != 0
			if !needsChmod {
				info, statErr := os.Stat(writePath)
				if statErr != nil {
					return nil, &Error{Message: fmt.Sprintf("failed to stat %s after write: %v", displayPath, statErr)}
				}
				current := info.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
				if current != desired {
					needsChmod = true
				}
			}

			if needsChmod {
				if err := os.Chmod(writePath, desired); err != nil {
					return nil, &Error{Message: fmt.Sprintf("failed to restore permissions for %s: %v", displayPath, err)}
				}
			}
		}

		if moveTarget != "" && writePath != state.path {
			if err := os.Remove(state.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, &Error{Message: fmt.Sprintf("failed to remove %s after move: %v", state.relativePath, err)}
			}
		}

		status := "M"
		if state.isNew {
			status = "A"
		}
		results = append(results, Result{Status: status, Path: displayPath})
	}
	return results, nil
}

func (ws *filesystemWorkspace) resolvePath(relative string) (string, string, error) {
	rel := strings.TrimSpace(relative)
	if rel == "" {
		return "", "", fmt.Errorf("invalid patch path")
	}
	cleaned := filepath.Clean(rel)
	var abs string
	if filepath.IsAbs(cleaned) {
		abs = filepath.Clean(cleaned)
	} else {
		abs = filepath.Clean(filepath.Join(ws.workingDir, cleaned))
	}
	return abs, cleaned, nil
}
