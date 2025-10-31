package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const runResearchCommandName = "run_research"

func newRunResearchCommand(rt *Runtime) InternalCommandHandler {
	return func(ctx context.Context, req InternalCommandRequest) (PlanObservationPayload, error) {
		payload := PlanObservationPayload{}

		// 1. Parse the research spec from the raw command
		type researchSpec struct {
			Goal  string `json:"goal"`
			Turns int    `json:"turns"`
		}
		var rs researchSpec
		jsonInput := strings.TrimSpace(strings.TrimPrefix(req.Raw, runResearchCommandName))
		if err := json.Unmarshal([]byte(jsonInput), &rs); err != nil {
			return failApplyPatch(&payload, "internal command: run_research invalid JSON"), err
		}
		rs.Goal = strings.TrimSpace(rs.Goal)
		if rs.Goal == "" {
			return failApplyPatch(&payload, "internal command: run_research requires non-empty goal"), errors.New("run_research: missing goal")
		}
		if rs.Turns <= 0 {
			rs.Turns = 10 // Default to 10 turns if not specified or invalid
		}

		// 2. Configure new runtime options for the sub-agent
		subOptions := rt.options
		subOptions.HandsFree = true
		subOptions.HandsFreeTopic = rs.Goal
		subOptions.MaxPasses = rs.Turns
		subOptions.HandsFreeAutoReply = fmt.Sprintf("Please continue to work on the set goal. No human available. Goal: %s", rs.Goal)
		subOptions.DisableInputReader = true
		subOptions.DisableOutputForwarding = true

		// 3. Create and run the sub-agent
		subAgent, err := NewRuntime(subOptions)
		if err != nil {
			return failApplyPatch(&payload, "failed to create sub-agent"), err
		}

		runCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() { _ = subAgent.Run(runCtx) }()

		// 4. Capture the output of the sub-agent
		var lastAssistant string
		var success bool
		for evt := range subAgent.Outputs() {
			switch evt.Type {
			case EventTypeAssistantMessage:
				if m := strings.TrimSpace(evt.Message); m != "" {
					lastAssistant = m
				}
			case EventTypeStatus:
				if strings.Contains(evt.Message, "Hands-free session complete") {
					success = true
				}
			}
		}

		// 5. Populate the payload with the result
		if success {
			payload.Stdout = lastAssistant
			zero := 0
			payload.ExitCode = &zero
		} else {
			payload.Stderr = lastAssistant
			one := 1
			payload.ExitCode = &one
		}

		return payload, nil
	}
}
