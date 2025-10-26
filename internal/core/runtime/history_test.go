package runtime

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// helper to avoid repeating pointer boilerplate.
func stringPtr(s string) *string {
	return &s
}

func TestWriteHistoryLog_UsesDefaultPath(t *testing.T) {
	tempDir := t.TempDir()

	options := RuntimeOptions{}
	options.setDefaults()
	if options.HistoryLogPath == nil {
		t.Fatalf("expected default history path to be configured")
	}
	if got := *options.HistoryLogPath; got != "history.json" {
		t.Fatalf("expected default history path 'history.json', got %q", got)
	}

	historyPath := filepath.Join(tempDir, *options.HistoryLogPath)
	options.HistoryLogPath = &historyPath

	rt := &Runtime{
		options:   options,
		outputs:   make(chan RuntimeEvent, 1),
		closed:    make(chan struct{}),
		agentName: "test",
	}

	messages := []ChatMessage{{Role: RoleSystem, Content: "seed"}}
	rt.writeHistoryLog(messages)

	content, err := os.ReadFile(historyPath)
	if err != nil {
		t.Fatalf("failed to read history log: %v", err)
	}

	var logged []ChatMessage
	if err := json.Unmarshal(content, &logged); err != nil {
		t.Fatalf("failed to decode history log: %v", err)
	}
	if len(logged) != len(messages) || logged[0].Content != messages[0].Content {
		t.Fatalf("unexpected history log contents: %+v", logged)
	}
}

func TestWriteHistoryLog_DisabledSkipsWrite(t *testing.T) {
	tempDir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to snapshot working directory: %v", err)
	}
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("failed to enter temp directory: %v", err)
	}
	defer func() {
		if chdirErr := os.Chdir(cwd); chdirErr != nil {
			t.Errorf("failed to restore working directory: %v", chdirErr)
		}
	}()

	options := RuntimeOptions{HistoryLogPath: stringPtr("")}
	options.setDefaults()
	if options.HistoryLogPath == nil {
		t.Fatalf("expected history path pointer to remain set")
	}
	if got := *options.HistoryLogPath; got != "" {
		t.Fatalf("expected history path to remain blank when disabled, got %q", got)
	}

	rt := &Runtime{
		options:   options,
		outputs:   make(chan RuntimeEvent, 1),
		closed:    make(chan struct{}),
		agentName: "test",
	}

	rt.writeHistoryLog([]ChatMessage{{Role: RoleUser, Content: "skip"}})

	if _, err := os.Stat(filepath.Join(tempDir, "history.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected history log to be skipped, got err=%v", err)
	}
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("failed to read temp directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no files when history logging disabled, found %d", len(entries))
	}
}
