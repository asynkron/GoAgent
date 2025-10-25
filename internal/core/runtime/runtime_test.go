package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestExecutePendingCommands_AppendsSingleToolMessage(t *testing.T) {
	t.Parallel()

	rt := &Runtime{
		plan:     NewPlanManager(),
		executor: NewCommandExecutor(),
		outputs:  make(chan RuntimeEvent, 10),
		closed:   make(chan struct{}),
		history:  []ChatMessage{},
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
		plan:     NewPlanManager(),
		executor: NewCommandExecutor(),
		outputs:  make(chan RuntimeEvent, 10),
		closed:   make(chan struct{}),
		history:  []ChatMessage{},
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

func TestHandleInputCancelCancelsActivePlan(t *testing.T) {
	rt := &Runtime{
		plan:     NewPlanManager(),
		executor: NewCommandExecutor(),
		outputs:  make(chan RuntimeEvent, 10),
		closed:   make(chan struct{}),
		history:  []ChatMessage{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	rt.setActivePlanContext(ctx, cancel)

	if err := rt.handleInput(context.Background(), InputEvent{Type: InputTypeCancel, Reason: "please stop"}); err != nil {
		t.Fatalf("handleInput returned error: %v", err)
	}

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatalf("expected plan context to be canceled")
	}

	if !rt.planCanceledByUser() {
		t.Fatalf("expected cancel request to be recorded")
	}

	rt.clearActivePlanContext()
}

func TestExecutePendingCommands_RecordsCancellation(t *testing.T) {
	rt := &Runtime{
		plan:     NewPlanManager(),
		executor: NewCommandExecutor(),
		outputs:  make(chan RuntimeEvent, 10),
		closed:   make(chan struct{}),
		history:  []ChatMessage{},
	}

	started := make(chan struct{})
	if err := rt.executor.RegisterInternalCommand("wait", func(ctx context.Context, _ InternalCommandRequest) (PlanObservationPayload, error) {
		close(started)
		<-ctx.Done()
		return PlanObservationPayload{}, ctx.Err()
	}); err != nil {
		t.Fatalf("failed to register internal command: %v", err)
	}

	rt.plan.Replace([]PlanStep{{
		ID:      "step-1",
		Title:   "Blocking step",
		Status:  PlanPending,
		Command: CommandDraft{Shell: agentShell, Run: "wait"},
	}})

	ctx, cancel := context.WithCancel(context.Background())
	rt.setActivePlanContext(ctx, cancel)

	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-started:
			rt.cancelActivePlan()
		case <-time.After(2 * time.Second):
			t.Errorf("command did not start in time")
			rt.cancelActivePlan()
		}
	}()

	rt.executePendingCommands(ctx, ToolCall{ID: "call-cancel", Name: "open-agent"})

	<-done

	history := rt.historySnapshot()
	if got := len(history); got != 1 {
		t.Fatalf("expected one history entry, got %d", got)
	}

	var payload PlanObservationPayload
	if err := json.Unmarshal([]byte(history[0].Content), &payload); err != nil {
		t.Fatalf("failed to decode history payload: %v", err)
	}

	if !payload.OperationCanceled {
		t.Fatalf("expected operation to be marked canceled")
	}
	if !payload.CanceledByHuman {
		t.Fatalf("expected cancellation to be attributed to the user")
	}
	if payload.Summary == "" {
		t.Fatalf("expected summary to describe cancellation outcome")
	}

	rt.clearActivePlanContext()
}

func TestRecordPlanResponseFiltersCompletedSteps(t *testing.T) {
	t.Parallel()

	rt := &Runtime{
		plan:    NewPlanManager(),
		outputs: make(chan RuntimeEvent, 10),
		closed:  make(chan struct{}),
		history: []ChatMessage{},
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
