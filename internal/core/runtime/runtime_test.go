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

	// The tool history should record a single message for the full plan snapshot.
	history := rt.historySnapshot()
	if got := len(history); got != 1 {
		t.Fatalf("expected exactly one tool message, got %d", got)
	}

	toolMessage := history[0]
	if toolMessage.Role != RoleTool {
		t.Fatalf("expected role %q, got %q", RoleTool, toolMessage.Role)
	}

	var observation PlanObservation
	if err := json.Unmarshal([]byte(toolMessage.Content), &observation); err != nil {
		t.Fatalf("failed to decode tool message: %v", err)
	}

	payload := observation.ObservationForLLM
	if payload == nil {
		t.Fatalf("expected payload to be present")
	}

	if got := len(payload.Plan); got != 2 {
		t.Fatalf("expected plan length 2, got %d", got)
	}

	for _, step := range payload.Plan {
		if step.Observation == nil || step.Observation.ObservationForLLM == nil {
			t.Fatalf("expected observation for step %s", step.ID)
		}
		if step.Status != PlanCompleted {
			t.Fatalf("expected step %s to complete, got %s", step.ID, step.Status)
		}
		if step.Command.Run != "" || step.Command.Shell != "" {
			t.Fatalf("expected sanitized command for step %s, got shell=%q run=%q", step.ID, step.Command.Shell, step.Command.Run)
		}

		obsPayload := step.Observation.ObservationForLLM
		if obsPayload.ExitCode == nil || *obsPayload.ExitCode != 0 {
			t.Fatalf("expected zero exit code, got %v", obsPayload.ExitCode)
		}
		for _, nested := range obsPayload.Plan {
			if nested.Command.Run != "" || nested.Command.Shell != "" {
				t.Fatalf("expected nested plan command sanitized for step %s", nested.ID)
			}
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

	var observation PlanObservation
	if err := json.Unmarshal([]byte(toolMessage.Content), &observation); err != nil {
		t.Fatalf("failed to decode tool message: %v", err)
	}

	payload := observation.ObservationForLLM
	if payload == nil {
		t.Fatalf("expected payload to be present")
	}

	if got := len(payload.Plan); got != 2 {
		t.Fatalf("expected plan length 2, got %d", got)
	}

	var failedStep *PlanStep
	for i := range payload.Plan {
		step := payload.Plan[i]
		if step.ID == "step-2" {
			failedStep = &step
			break
		}
	}

	if failedStep == nil {
		t.Fatalf("failed to locate failed step in plan")
	}

	if failedStep.Status != PlanFailed {
		t.Fatalf("expected step-2 to fail, got %s", failedStep.Status)
	}

	if failedStep.Observation == nil || failedStep.Observation.ObservationForLLM == nil {
		t.Fatalf("expected failed step observation payload")
	}

	if failedStep.Command.Run != "" || failedStep.Command.Shell != "" {
		t.Fatalf("expected sanitized command for failed step, got shell=%q run=%q", failedStep.Command.Shell, failedStep.Command.Run)
	}

	obsPayload := failedStep.Observation.ObservationForLLM
	if obsPayload.ExitCode == nil || *obsPayload.ExitCode == 0 {
		t.Fatalf("expected non-zero exit code, got %v", obsPayload.ExitCode)
	}
	for _, nested := range obsPayload.Plan {
		if nested.Command.Run != "" || nested.Command.Shell != "" {
			t.Fatalf("expected nested plan command sanitized, got shell=%q run=%q", nested.Command.Shell, nested.Command.Run)
		}
	}
	if payload.Summary == "" {
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
