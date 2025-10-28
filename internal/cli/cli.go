package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/joho/godotenv"

	"github.com/asynkron/goagent/internal/bootprobe"
	"github.com/asynkron/goagent/internal/core/runtime"
)

// Run executes the GoAgent runtime using the provided CLI arguments.
// It returns a POSIX-style exit code indicating whether execution succeeded.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}

	if err := godotenv.Load(); err != nil {
		// A missing .env file is fine, but other errors should be surfaced to help with debugging.
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) {
			fmt.Fprintf(stderr, "failed to load .env: %v\n", err)
			return 1
		}
	}

	defaultModel := os.Getenv("OPENAI_MODEL")
	if defaultModel == "" {
		// Use a widely-supported, tool-capable model by default.
		defaultModel = "gpt-4o"
	}

	defaultReasoning := os.Getenv("OPENAI_REASONING_EFFORT")
	defaultBaseURL := os.Getenv("OPENAI_BASE_URL")

	flagSet := flag.NewFlagSet("goagent", flag.ContinueOnError)
	flagSet.SetOutput(stderr)
	model := flagSet.String("model", defaultModel, "OpenAI model identifier to use for responses")
	reasoningEffort := flagSet.String("reasoning-effort", defaultReasoning, "Reasoning effort hint forwarded to OpenAI (low, medium, high)")
	promptAugmentation := flagSet.String("augment", "", "additional system prompt instructions appended after the default prompt")
	baseURL := flagSet.String("openai-base-url", defaultBaseURL, "override the OpenAI API base URL (optional)")
	// Optional: submit a prompt immediately to see streaming deltas without extra wiring.
	prompt := flagSet.String("prompt", "", "submit this prompt immediately and stream the assistant response")

	if err := flagSet.Parse(args); err != nil {
		return 2
	}

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(stderr, "OPENAI_API_KEY must be set in the environment.")
		return 1
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "failed to determine working directory: %v\n", err)
		return 1
	}

	probeCtx := bootprobe.NewContext(cwd)
	probeResult, probeSummary, combinedAugment := bootprobe.BuildAugmentation(probeCtx, *promptAugmentation)
	if probeResult.HasCapabilities() && probeSummary != "" {
		fmt.Fprintln(stdout, probeSummary)
		fmt.Fprintln(stdout)
	}

	options := runtime.RuntimeOptions{
		APIKey:                  apiKey,
		APIBaseURL:              strings.TrimSpace(*baseURL),
		Model:                   *model,
		ReasoningEffort:         *reasoningEffort,
		SystemPromptAugment:     combinedAugment,
		DisableOutputForwarding: true,
		UseStreaming:            true,
	}

	agent, err := runtime.NewRuntime(options)
	if err != nil {
		fmt.Fprintf(stderr, "failed to create runtime: %v\n", err)
		return 1
	}

	outputs := agent.Outputs()
	var wg sync.WaitGroup
	// Track whether we've printed streaming deltas so we can avoid duplicating
	// content when the final assistant_message event arrives.
	var printedDelta bool
	wg.Add(1)
	go func() {
		defer wg.Done()
		for evt := range outputs {
			if evt.Type == runtime.EventTypeAssistantDelta {
				// Streamed chunk: print as-is for a smooth, incremental experience.
				// We intentionally avoid Glamour here because partial markdown often
				// renders poorly; the final message can be rendered nicely if needed.
				fmt.Fprint(stdout, evt.Message)
				printedDelta = true
				continue
			}
			if evt.Type == runtime.EventTypeAssistantMessage {
				if printedDelta {
					// Content already streamed; just end the line neatly.
					fmt.Fprintln(stdout)
					printedDelta = false
					continue
				}
				// Print plain content (no markdown rendering dependency).
				fmt.Fprintln(stdout, evt.Message)
			} else {
				level := string(evt.Level)
				if level != "" {
					fmt.Fprintf(stdout, "[%s:%s] %s\n", evt.Type, level, evt.Message)
				} else {
					fmt.Fprintf(stdout, "[%s] %s\n", evt.Type, evt.Message)
				}
			}

			if len(evt.Metadata) == 0 {
				continue
			}

			data, err := json.MarshalIndent(evt.Metadata, "", "  ")
			if err != nil {
				fmt.Fprintf(stdout, "  metadata: %+v\n", evt.Metadata)
				continue
			}

			fmt.Fprintln(stdout, "  metadata:")
			for _, line := range strings.Split(string(data), "\n") {
				fmt.Fprintf(stdout, "    %s\n", line)
			}
		}
	}()

	// Run the runtime in the background so we can optionally submit a prompt immediately
	// (similar to cmd/sse behavior where we call SubmitPrompt after starting Run).
	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- agent.Run(ctx)
	}()

	// If a prompt is provided, submit it right away so deltas stream to stdout.
	if p := strings.TrimSpace(*prompt); p != "" {
		agent.SubmitPrompt(p)
	}

	// Wait for the runtime to finish and handle any error.
	if err := <-runErrCh; err != nil {
		fmt.Fprintf(stderr, "runtime error: %v\n", err)
		wg.Wait()
		return 1
	}

	wg.Wait()
	return 0
}
