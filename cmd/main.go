package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/joho/godotenv"

	"github.com/asynkron/goagent/internal/core/runtime"
)

// main bootstraps the Go translation of the GoAgent runtime.
func main() {
	if err := godotenv.Load(); err != nil {
		// A missing .env file is fine, but other errors should be surfaced to help with debugging.
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) {
			fmt.Fprintf(os.Stderr, "failed to load .env: %v\n", err)
			os.Exit(1)
		}
	}

	defaultModel := os.Getenv("OPENAI_MODEL")
	if defaultModel == "" {
		defaultModel = "gpt-5"
	}

	defaultReasoning := os.Getenv("OPENAI_REASONING_EFFORT")

	var (
		model              = flag.String("model", defaultModel, "OpenAI model identifier to use for responses")
		reasoningEffort    = flag.String("reasoning-effort", defaultReasoning, "Reasoning effort hint forwarded to OpenAI (low, medium, high)")
		promptAugmentation = flag.String("augment", "", "additional system prompt instructions appended after the default prompt")
	)
	flag.Parse()

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "OPENAI_API_KEY must be set in the environment.")
		os.Exit(1)
	}

	options := runtime.RuntimeOptions{
		APIKey:                  apiKey,
		Model:                   *model,
		ReasoningEffort:         *reasoningEffort,
		SystemPromptAugment:     *promptAugmentation,
		DisableOutputForwarding: true,
	}

	agent, err := runtime.NewRuntime(options)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create runtime: %v\n", err)
		os.Exit(1)
	}

	outputs := agent.Outputs()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for evt := range outputs {
			level := string(evt.Level)
			if level != "" {
				fmt.Fprintf(os.Stdout, "[%s:%s] %s\n", evt.Type, level, evt.Message)
			} else {
				fmt.Fprintf(os.Stdout, "[%s] %s\n", evt.Type, evt.Message)
			}

			if len(evt.Metadata) == 0 {
				continue
			}

			data, err := json.MarshalIndent(evt.Metadata, "", "  ")
			if err != nil {
				fmt.Fprintf(os.Stdout, "  metadata: %+v\n", evt.Metadata)
				continue
			}

			fmt.Fprintln(os.Stdout, "  metadata:")
			for _, line := range strings.Split(string(data), "\n") {
				fmt.Fprintf(os.Stdout, "    %s\n", line)
			}
		}
	}()

	if err := agent.Run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "runtime error: %v\n", err)
		wg.Wait()
		os.Exit(1)
	}

	wg.Wait()
}
