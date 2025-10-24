package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/joho/godotenv"

	"github.com/asynkron/goagent/internal/core/runtime"
)

// main bootstraps the Go translation of the GoAgent runtime.
func main() {
	var (
		model              = flag.String("model", "gpt-4.1", "OpenAI model identifier to use for responses")
		autoApprove        = flag.Bool("auto-approve", false, "execute plan steps without manual confirmation")
		noHuman            = flag.Bool("no-human", false, "operate without waiting for user input between passes")
		promptAugmentation = flag.String("augment", "", "additional system prompt instructions appended after the default prompt")
		planReminder       = flag.String("plan-reminder", "", "message sent when the plan stalls with no human present")
		autoMessage        = flag.String("auto-message", "", "auto-response sent when no human is available")
	)
	flag.Parse()

	if err := godotenv.Load(); err != nil {
		// A missing .env file is fine, but other errors should be surfaced to help with debugging.
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) {
			fmt.Fprintf(os.Stderr, "failed to load .env: %v\n", err)
			os.Exit(1)
		}
	}

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "OPENAI_API_KEY must be set in the environment.")
		os.Exit(1)
	}

	options := runtime.RuntimeOptions{
		APIKey:              apiKey,
		Model:               *model,
		AutoApprove:         *autoApprove,
		NoHuman:             *noHuman,
		SystemPromptAugment: *promptAugmentation,
		PlanReminderMessage: *planReminder,
		NoHumanAutoMessage:  *autoMessage,
	}

	agent, err := runtime.NewRuntime(options)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create runtime: %v\n", err)
		os.Exit(1)
	}

	if err := agent.Run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "runtime error: %v\n", err)
		os.Exit(1)
	}
}
