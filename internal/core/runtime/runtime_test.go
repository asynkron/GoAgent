package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestQueueHandsFreePromptEnqueuesConfiguredTopic(t *testing.T) {
	t.Parallel()

	rt := &Runtime{
		options: RuntimeOptions{
			HandsFree:      true,
			HandsFreeTopic: "  Investigate logs   ",
		},
		inputs: make(chan InputEvent, 1),
		closed: make(chan struct{}),
	}

	rt.queueHandsFreePrompt()

	select {
	case evt := <-rt.inputs:
		if evt.Type != InputTypePrompt {
			t.Fatalf("expected prompt input, got %s", evt.Type)
		}
		if evt.Prompt != "Investigate logs" {
			t.Fatalf("expected trimmed topic, got %q", evt.Prompt)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for hands-free prompt")
	}

	select {
	case extra := <-rt.inputs:
		t.Fatalf("unexpected additional input event: %+v", extra)
	default:
	}
}

func TestExecutePendingCommands_AppendsSingleToolMessage(t *testing.T) {
	t.Parallel()

	rt := &Runtime{
		plan:      NewPlanManager(),
		executor:  NewCommandExecutor(),
		outputs:   make(chan RuntimeEvent, 10),
		closed:    make(chan struct{}),
		history:   []ChatMessage{},
		agentName: "main",
	}

	rt.plan.Replace([]PlanStep{
		{
			ID:      "step-1",
			Title:   "First",
			Status:  PlanPending,
			Command: CommandDraft{Shell: "/bin/bash", Run: "echo step-one"},
		},
		{
			ID:           "step-2",
			Title:        "Second",
			Status:       PlanPending,
			WaitingForID: []string{"step-1"},
			Command:      CommandDraft{Shell: "/bin/bash", Run: "echo step-two"},
		},
	})

	rt.executePendingCommands(context.Background(), ToolCall{ID: "call-1", Name: "open-agent"})

	// The tool history should record a single message summarizing executed steps.
	history := rt.historySnapshot()
	if got := len(history); got != 1 {
		t.Fatalf("expected exactly one tool message, got %d", got)
	}

	toolMessage := history[0]
	if toolMessage.Role != RoleTool {
		t.Fatalf("expected role %q, got %q", RoleTool, toolMessage.Role)
	}

	var observation PlanObservationPayload
	if err := json.Unmarshal([]byte(toolMessage.Content), &observation); err != nil {
		t.Fatalf("failed to decode tool message: %v", err)
	}

	if got := len(observation.PlanObservation); got != 2 {
		t.Fatalf("expected plan length 2, got %d", got)
	}

	for _, step := range observation.PlanObservation {
		if step.Status != PlanCompleted {
			t.Fatalf("expected step %s to complete, got %s", step.ID, step.Status)
		}
		if step.ExitCode == nil || *step.ExitCode != 0 {
			t.Fatalf("expected zero exit code, got %v", step.ExitCode)
		}
	}
}

func TestExecutePendingCommands_FailureStillRecordsSingleToolMessage(t *testing.T) {
	t.Parallel()

	rt := &Runtime{
		plan:      NewPlanManager(),
		executor:  NewCommandExecutor(),
		outputs:   make(chan RuntimeEvent, 10),
		closed:    make(chan struct{}),
		history:   []ChatMessage{},
		agentName: "main",
	}

	rt.plan.Replace([]PlanStep{
		{
			ID:      "step-1",
			Title:   "First",
			Status:  PlanPending,
			Command: CommandDraft{Shell: "/bin/bash", Run: "echo ok"},
		},
		{
			ID:           "step-2",
			Title:        "Second",
			Status:       PlanPending,
			WaitingForID: []string{"step-1"},
			Command:      CommandDraft{Shell: "/bin/bash", Run: "exit 7"},
		},
	})

	rt.executePendingCommands(context.Background(), ToolCall{ID: "call-2", Name: "open-agent"})

	// Even when a step fails, the runtime should only surface one tool message per call.
	history := rt.historySnapshot()
	if got := len(history); got != 1 {
		t.Fatalf("expected exactly one tool message, got %d", got)
	}

	toolMessage := history[0]
	if toolMessage.Role != RoleTool {
		t.Fatalf("expected role %q, got %q", RoleTool, toolMessage.Role)
	}

	var observation PlanObservationPayload
	if err := json.Unmarshal([]byte(toolMessage.Content), &observation); err != nil {
		t.Fatalf("failed to decode tool message: %v", err)
	}

	if got := len(observation.PlanObservation); got != 2 {
		t.Fatalf("expected plan length 2, got %d", got)
	}

	var failedStep *StepObservation
	for i := range observation.PlanObservation {
		step := observation.PlanObservation[i]
		if step.ID == "step-2" {
			failedStep = &step
			break
		}
	}

	if failedStep == nil {
		t.Fatalf("failed to locate failed step in observations")
	}

	if failedStep.Status != PlanFailed {
		t.Fatalf("expected step-2 to fail, got %s", failedStep.Status)
	}

	if failedStep.ExitCode == nil || *failedStep.ExitCode == 0 {
		t.Fatalf("expected non-zero exit code, got %v", failedStep.ExitCode)
	}

	if observation.Summary == "" {
		t.Fatalf("expected summary to describe execution outcome")
	}
}

func TestComputeValidationBackoff(t *testing.T) {
	t.Parallel()

	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{0, validationBackoffBase},
		{1, validationBackoffBase},
		{2, validationBackoffBase * 2},
		{3, validationBackoffBase * 4},
		{10, validationBackoffMax},
	}

	for _, tc := range tests {
		if got := computeValidationBackoff(tc.attempt); got != tc.want {
			t.Fatalf("attempt %d: expected %s, got %s", tc.attempt, tc.want, got)
		}
	}
}

func TestRecordPlanResponseFiltersCompletedSteps(t *testing.T) {
	t.Parallel()

	rt := &Runtime{
		plan:      NewPlanManager(),
		outputs:   make(chan RuntimeEvent, 10),
		closed:    make(chan struct{}),
		history:   []ChatMessage{},
		agentName: "main",
	}

	resp := &PlanResponse{
		Message: "ack",
		Plan: []PlanStep{
			{ID: "step-done", Status: PlanCompleted},
			{ID: "step-pending", Status: PlanPending, WaitingForID: []string{"step-done"}},
			{ID: "step-blocked", Status: PlanPending, WaitingForID: []string{"step-done", "step-pending"}},
		},
	}

	execCount := rt.recordPlanResponse(resp, ToolCall{ID: "call-test", Name: "open-agent"})

	if execCount != 1 {
		t.Fatalf("expected executable count 1, got %d", execCount)
	}

	snapshot := rt.plan.Snapshot()
	if got := len(snapshot); got != 2 {
		t.Fatalf("expected plan snapshot length 2, got %d", got)
	}
	if snapshot[0].ID != "step-pending" {
		t.Fatalf("expected first remaining step to be step-pending, got %s", snapshot[0].ID)
	}
	if snapshot[0].Status != PlanPending {
		t.Fatalf("expected step-pending status pending, got %s", snapshot[0].Status)
	}
	if snapshot[0].WaitingForID != nil {
		t.Fatalf("expected dependencies on completed step to be removed, got %v", snapshot[0].WaitingForID)
	}

	if snapshot[1].ID != "step-blocked" {
		t.Fatalf("expected second remaining step to be step-blocked, got %s", snapshot[1].ID)
	}
	if snapshot[1].Status != PlanPending {
		t.Fatalf("expected step-blocked status pending, got %s", snapshot[1].Status)
	}
	if want := []string{"step-pending"}; len(snapshot[1].WaitingForID) != len(want) || snapshot[1].WaitingForID[0] != want[0] {
		t.Fatalf("expected dependencies %v, got %v", want, snapshot[1].WaitingForID)
	}
}

func TestRuntimeEmitAnnotatesEvent(t *testing.T) {
	t.Parallel()

	rt := &Runtime{
		outputs:   make(chan RuntimeEvent, 2),
		closed:    make(chan struct{}),
		agentName: "main",
	}

	if pass := rt.incrementPassCount(); pass != 1 {
		t.Fatalf("expected pass count to be 1, got %d", pass)
	}

	rt.emit(RuntimeEvent{Type: EventTypeStatus, Message: "hello"})

	select {
	case evt := <-rt.outputs:
		if evt.Pass != 1 {
			t.Fatalf("expected pass to be 1, got %d", evt.Pass)
		}
		if evt.Agent != "main" {
			t.Fatalf("expected agent to be main, got %s", evt.Agent)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for event")
	}

	rt.emit(RuntimeEvent{Type: EventTypeStatus, Message: "child", Pass: 99, Agent: "worker"})

	select {
	case evt := <-rt.outputs:
		if evt.Pass != 99 {
			t.Fatalf("expected pass to remain 99, got %d", evt.Pass)
		}
		if evt.Agent != "worker" {
			t.Fatalf("expected agent to remain worker, got %s", evt.Agent)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for overridden event")
	}
}

func TestRuntimeHistoryAmnesia(t *testing.T) {
	t.Parallel()

	rt := &Runtime{
		options: RuntimeOptions{AmnesiaAfterPasses: 2},
		history: []ChatMessage{},
	}

	if pass := rt.incrementPassCount(); pass != 1 {
		t.Fatalf("expected first pass to be 1, got %d", pass)
	}

	assistantContent := strings.Repeat("A", 1024)
	rt.appendHistory(ChatMessage{
		Role:      RoleAssistant,
		Content:   assistantContent,
		Timestamp: time.Now(),
		ToolCalls: []ToolCall{{ID: "tool-old", Name: "open-agent", Arguments: strings.Repeat("B", 1024)}},
	})

	if pass := rt.incrementPassCount(); pass != 2 {
		t.Fatalf("expected second pass to be 2, got %d", pass)
	}

	toolPayload := PlanObservationPayload{
		Summary: "Executed planned commands.",
		Stdout:  strings.Repeat("S", 1024),
		Stderr:  strings.Repeat("E", 1024),
		PlanObservation: []StepObservation{{
			ID:      "step-1",
			Status:  PlanCompleted,
			Stdout:  "step output",
			Stderr:  "step error",
			Details: "Detailed output about the step.",
		}},
	}
	toolContent, err := BuildToolMessage(toolPayload)
	if err != nil {
		t.Fatalf("failed to build tool message: %v", err)
	}

	rt.appendHistory(ChatMessage{
		Role:       RoleTool,
		Content:    toolContent,
		ToolCallID: "tool-old",
		Timestamp:  time.Now(),
	})

	if pass := rt.incrementPassCount(); pass != 3 {
		t.Fatalf("expected third pass to be 3, got %d", pass)
	}

	rt.appendHistory(ChatMessage{Role: RoleUser, Content: "fresh input", Timestamp: time.Now()})

	if pass := rt.incrementPassCount(); pass != 4 {
		t.Fatalf("expected fourth pass to be 4, got %d", pass)
	}

	latestAssistant := "Latest assistant message"
	rt.appendHistory(ChatMessage{Role: RoleAssistant, Content: latestAssistant, Timestamp: time.Now()})

	history := rt.historySnapshot()
	if got := len(history); got != 4 {
		t.Fatalf("expected 4 history entries, got %d", got)
	}

	oldAssistant := history[0]
	if oldAssistant.Role != RoleAssistant {
		t.Fatalf("expected first history entry to be assistant, got %s", oldAssistant.Role)
	}
	if oldAssistant.Pass != 1 {
		t.Fatalf("expected assistant pass to remain 1, got %d", oldAssistant.Pass)
	}
	if oldAssistant.Content == assistantContent {
		t.Fatalf("expected assistant content to be truncated by amnesia")
	}
	if len(oldAssistant.Content) == 0 {
		t.Fatalf("expected assistant content to retain a short summary")
	}
	if len(oldAssistant.ToolCalls) == 0 {
		t.Fatalf("expected assistant tool calls to remain present")
	}
	if oldAssistant.ToolCalls[0].Arguments == strings.Repeat("B", 1024) {
		t.Fatalf("expected assistant tool call arguments to be truncated")
	}

	oldTool := history[1]
	if oldTool.Pass != 2 {
		t.Fatalf("expected tool message pass to remain 2, got %d", oldTool.Pass)
	}
	var sanitized PlanObservationPayload
	if err := json.Unmarshal([]byte(oldTool.Content), &sanitized); err != nil {
		t.Fatalf("failed to decode sanitized tool content: %v", err)
	}
	if sanitized.Stdout != "" || sanitized.Stderr != "" {
		t.Fatalf("expected tool stdout/stderr to be cleared, got %q / %q", sanitized.Stdout, sanitized.Stderr)
	}
	if len(sanitized.PlanObservation) == 0 {
		t.Fatalf("expected plan observation entries to remain present")
	}
	if sanitized.PlanObservation[0].Stdout != "" || sanitized.PlanObservation[0].Stderr != "" {
		t.Fatalf("expected nested stdout/stderr to be cleared, got %q / %q", sanitized.PlanObservation[0].Stdout, sanitized.PlanObservation[0].Stderr)
	}
	if sanitized.Summary != toolPayload.Summary {
		t.Fatalf("expected tool summary to remain, got %q", sanitized.Summary)
	}
	if sanitized.PlanObservation[0].Details == "" {
		t.Fatalf("expected step details to retain a short description")
	}

	recentUser := history[2]
	if recentUser.Role != RoleUser {
		t.Fatalf("expected third entry to be user, got %s", recentUser.Role)
	}
	if recentUser.Content != "fresh input" {
		t.Fatalf("expected user content to remain unchanged, got %q", recentUser.Content)
	}

	freshAssistant := history[3]
	if freshAssistant.Content != latestAssistant {
		t.Fatalf("expected most recent assistant content to remain untouched")
	}
}
