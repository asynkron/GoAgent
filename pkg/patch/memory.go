package patch

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// ApplyToMemory applies operations to an in-memory document store represented by a map.
// The provided map is copied before mutation and the updated snapshot is returned.
func ApplyToMemory(ctx context.Context, operations []Operation, files map[string]string, opts Options) (map[string]string, []Result, error) {
	snapshot := make(map[string]string, len(files))
	for k, v := range files {
		snapshot[k] = v
	}
	ws := newMemoryWorkspace(snapshot, opts)
	results, err := apply(ctx, operations, ws)
	if err != nil {
		return nil, nil, err
	}
	return ws.files, results, nil
}

// ApplyMemoryPatch parses a raw patch payload and applies it to an in-memory map of files.
func ApplyMemoryPatch(ctx context.Context, patchBody string, files map[string]string, opts Options) (map[string]string, []Result, error) {
	operations, err := Parse(patchBody)
	if err != nil {
		return nil, nil, err
	}
	return ApplyToMemory(ctx, operations, files, opts)
}

type memoryWorkspace struct {
	options   Options
	files     map[string]string
	states    map[string]*state
	deletions []Result
}

func newMemoryWorkspace(files map[string]string, opts Options) *memoryWorkspace {
	return &memoryWorkspace{
		options: opts,
		files:   files,
		states:  make(map[string]*state),
	}
}

func (ws *memoryWorkspace) Ensure(path string, create bool) (*state, error) {
	rel := filepath.Clean(strings.TrimSpace(path))
	if rel == "" || rel == "." {
		return nil, fmt.Errorf("invalid patch path")
	}
	if state, ok := ws.states[rel]; ok {
		state.options = ws.options
		if ws.options.IgnoreWhitespace {
			state.normalizedLines = ensureNormalizedLines(state)
		} else {
			state.normalizedLines = nil
		}
		return state, nil
	}

	content, ok := ws.files[rel]
	if !ok {
		if !create {
			return nil, fmt.Errorf("failed to read %s: file does not exist", rel)
		}
		state := &state{
			path:         rel,
			relativePath: rel,
			lines:        []string{},
			options:      ws.options,
			isNew:        true,
		}
		if ws.options.IgnoreWhitespace {
			state.normalizedLines = []string{}
		}
		ws.states[rel] = state
		return state, nil
	}

	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	lines := strings.Split(normalized, "\n")
	ends := strings.HasSuffix(normalized, "\n")
	state := &state{
		path:                    rel,
		relativePath:            rel,
		lines:                   lines,
		originalContent:         content,
		originalEndsWithNewline: &ends,
		options:                 ws.options,
	}
	if ws.options.IgnoreWhitespace {
		state.normalizedLines = ensureNormalizedLines(state)
	}
	ws.states[rel] = state
	return state, nil
}

func (ws *memoryWorkspace) Delete(path string) error {
	rel := filepath.Clean(strings.TrimSpace(path))
	if rel == "" || rel == "." {
		return fmt.Errorf("invalid patch path")
	}
	if _, ok := ws.files[rel]; !ok {
		return &Error{Message: fmt.Sprintf("Failed to delete file %s", rel)}
	}
	delete(ws.files, rel)
	delete(ws.states, rel)
	ws.deletions = append(ws.deletions, Result{Status: "D", Path: rel})
	return nil
}

func (ws *memoryWorkspace) Commit() ([]Result, error) {
	results := append([]Result{}, ws.deletions...)
	for key, state := range ws.states {
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

		writeKey := key
		display := state.relativePath
		moveTarget := strings.TrimSpace(state.movePath)
		if moveTarget != "" {
			cleaned := filepath.Clean(moveTarget)
			if cleaned == "" || cleaned == "." {
				return nil, fmt.Errorf("invalid patch path")
			}
			writeKey = cleaned
			display = cleaned
		}

		ws.files[writeKey] = newContent
		if moveTarget != "" && writeKey != key {
			delete(ws.files, key)
		}

		status := "M"
		if state.isNew {
			status = "A"
		}
		results = append(results, Result{Status: status, Path: display})
	}
	return results, nil
}
