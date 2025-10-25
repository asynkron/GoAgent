package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asynkron/goagent/internal/core/schema"
)

type stubTransport struct {
	body       []byte
	statusCode int
	calls      int
}

func (s *stubTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	s.calls++
	resp := &http.Response{
		StatusCode: s.statusCode,
		Body:       io.NopCloser(bytes.NewReader(s.body)),
		Header:     make(http.Header),
	}
	return resp, nil
}

func TestPlanExecutionLoopPausesForHumanInput(t *testing.T) {
	t.Parallel()

	plan := PlanResponse{
		Message:           "Need clarification",
		Reasoning:         "Reviewing the prompt requires clarification.",
		RequireHumanInput: true,
		Plan: []PlanStep{{
			ID:           "step-1",
			Title:        "Gather context",
			Status:       PlanPending,
			WaitingForID: []string{},
			Command: CommandDraft{
				Reason:      "Collect details before continuing",
				Shell:       "/bin/bash",
				Run:         "echo collecting",
				Cwd:         "",
				TimeoutSec:  60,
				FilterRegex: "",
				TailLines:   200,
				MaxBytes:    16384,
			},
		}},
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("failed to marshal plan: %v", err)
	}

	completion := chatCompletionResponse{Choices: []struct {
		Message struct {
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	}{{}}}
	completion.Choices[0].Message.ToolCalls = []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}{{ID: "call-1"}}
	completion.Choices[0].Message.ToolCalls[0].Function.Name = schema.ToolName
	completion.Choices[0].Message.ToolCalls[0].Function.Arguments = string(planJSON)

	body, err := json.Marshal(completion)
	if err != nil {
		t.Fatalf("failed to marshal completion: %v", err)
	}

	transport := &stubTransport{body: body, statusCode: http.StatusOK}

	client, err := NewOpenAIClient("test-key", "gpt-4o", "")
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.httpClient = &http.Client{Transport: transport}

	rt := &Runtime{
		options: RuntimeOptions{
			Model:        "gpt-4o",
			OutputBuffer: 16,
			OutputWriter: io.Discard,
		},
		inputs:    make(chan InputEvent, 1),
		outputs:   make(chan RuntimeEvent, 16),
		closed:    make(chan struct{}),
		plan:      NewPlanManager(),
		client:    client,
		executor:  NewCommandExecutor(),
		history:   []ChatMessage{{Role: RoleSystem, Content: "system"}},
		agentName: "main",
	}

	ctx := context.Background()
	rt.planExecutionLoop(ctx)
	rt.close()

	t.Cleanup(func() {
		_ = os.Remove("history.json")
	})

	if transport.calls != 1 {
		t.Fatalf("expected a single plan request, got %d", transport.calls)
	}

	history := rt.historySnapshot()
	if len(history) != 3 {
		t.Fatalf("expected history length 3, got %d", len(history))
	}
	if history[1].Role != RoleAssistant {
		t.Fatalf("expected second history entry to be assistant, got %s", history[1].Role)
	}
	if history[2].Role != RoleTool {
		t.Fatalf("expected third history entry to be tool, got %s", history[2].Role)
	}

	var events []RuntimeEvent
	for evt := range rt.outputs {
		events = append(events, evt)
	}

	var requestEvent *RuntimeEvent
	for i := range events {
		if events[i].Type == EventTypeRequestInput {
			requestEvent = &events[i]
		}
	}
	if requestEvent == nil {
		t.Fatalf("expected request input event, got %+v", events)
	}
	if !strings.Contains(requestEvent.Message, "Assistant requested additional input") {
		t.Fatalf("unexpected request input message: %s", requestEvent.Message)
	}
}

func TestPlanningHistorySnapshotCompactsHistory(t *testing.T) {
	t.Parallel()

	payload := PlanObservationPayload{
		Summary: "Executed 2 plan steps without errors.",
		Details: "Validation succeeded.",
		PlanObservation: []StepObservation{{
			ID:     "step-1",
			Status: PlanCompleted,
		}, {
			ID:     "step-2",
			Status: PlanCompleted,
		}},
		Stdout: strings.Repeat("raw-output ", 40),
	}
	toolMessage, err := BuildToolMessage(payload)
	if err != nil {
		t.Fatalf("failed to marshal tool message: %v", err)
	}

	now := time.Now()
	rt := &Runtime{
		history: []ChatMessage{
			{Role: RoleSystem, Content: "system", Timestamp: now},
			{Role: RoleUser, Content: strings.Repeat("user instruction ", 80), Timestamp: now},
			{Role: RoleAssistant, Content: strings.Repeat("assistant reasoning ", 80), Timestamp: now},
			{Role: RoleTool, Content: toolMessage, Timestamp: now},
		},
		contextBudget: ContextBudget{MaxTokens: 320, CompactWhenPercent: 0.5},
	}

	original := append([]ChatMessage(nil), rt.history...)
	beforeTotal, _ := estimateHistoryTokenUsage(original)
	if beforeTotal <= rt.contextBudget.triggerTokens() {
		t.Fatalf("expected oversized history for compaction test")
	}

	history := rt.planningHistorySnapshot()

	if len(history) != len(original) {
		t.Fatalf("expected history length %d, got %d", len(original), len(history))
	}
	if history[0].Role != RoleSystem || history[0].Summarized {
		t.Fatalf("system prompt should remain untouched: %+v", history[0])
	}
	if !history[1].Summarized || history[1].Role != RoleAssistant {
		t.Fatalf("expected first user message to be summarized, got %+v", history[1])
	}
	if !strings.Contains(history[1].Content, summaryPrefix) {
		t.Fatalf("expected summary marker in content: %s", history[1].Content)
	}

	if !history[3].Summarized {
		t.Fatalf("expected tool message to be summarized when exceeding budget")
	}
	if !strings.Contains(history[3].Content, payload.Summary) {
		t.Fatalf("expected tool summary to retain payload summary, got %s", history[3].Content)
	}
	if strings.Contains(history[3].Content, "raw-output") {
		t.Fatalf("expected tool summary to drop raw stdout, got %s", history[3].Content)
	}

	afterTotal, _ := estimateHistoryTokenUsage(history)
	if afterTotal > rt.contextBudget.triggerTokens() {
		t.Fatalf("expected compacted history to be within budget, got %d tokens", afterTotal)
	}

	// Running the compactor again should not rewrite already summarized entries.
	second := rt.planningHistorySnapshot()
	if second[1].Content != history[1].Content {
		t.Fatalf("expected stable summary, got %q vs %q", second[1].Content, history[1].Content)
	}
	if second[3].Content != history[3].Content {
		t.Fatalf("expected tool summary to remain stable")
	}
}
