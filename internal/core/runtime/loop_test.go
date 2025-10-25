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
		inputs:   make(chan InputEvent, 1),
		outputs:  make(chan RuntimeEvent, 16),
		closed:   make(chan struct{}),
		plan:     NewPlanManager(),
		client:   client,
		executor: NewCommandExecutor(),
		history:  []ChatMessage{{Role: RoleSystem, Content: "system"}},
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
