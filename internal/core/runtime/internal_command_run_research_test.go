package runtime

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestRunResearchCommand(t *testing.T) {
	t.Skip("Skipping test that requires OpenAI API key")
	t.Parallel()

	// 1. Setup a mock runtime
	options := RuntimeOptions{
		APIKey:                  os.Getenv("OPENAI_API_KEY"),
		Model:                   "gpt-4o",
		DisableOutputForwarding: true,
		UseStreaming:            true,
	}
	rt, err := NewRuntime(options)
	if err != nil {
		t.Fatalf("failed to create runtime: %v", err)
	}

	// 2. Create a request for the run_research command
	researchGoal := "write a haiku about testing"
	researchTurns := 5
	rawCommand := `{"goal":"` + researchGoal + `","turns":` + strconv.Itoa(researchTurns) + `}`
	req := InternalCommandRequest{
		Name: runResearchCommandName,
		Raw:  rawCommand,
		Step: PlanStep{ID: "step-1", Command: CommandDraft{Shell: agentShell, Run: rawCommand}},
	}

	// 3. Execute the command

	payload, err := newRunResearchCommand(rt)(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	// 4. Assert the results
	if payload.ExitCode == nil || *payload.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %+v", payload.ExitCode)
	}

	if !strings.Contains(payload.Stdout, "test") {
		t.Fatalf("expected stdout to contain 'test', got %q", payload.Stdout)
	}
}
