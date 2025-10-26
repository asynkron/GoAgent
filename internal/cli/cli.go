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

	"github.com/charmbracelet/glamour"
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
		defaultModel = "gpt-5"
	}

	defaultReasoning := os.Getenv("OPENAI_REASONING_EFFORT")
	defaultBaseURL := os.Getenv("OPENAI_BASE_URL")

	flagSet := flag.NewFlagSet("goagent", flag.ContinueOnError)
	flagSet.SetOutput(stderr)
	model := flagSet.String("model", defaultModel, "OpenAI model identifier to use for responses")
	reasoningEffort := flagSet.String("reasoning-effort", defaultReasoning, "Reasoning effort hint forwarded to OpenAI (low, medium, high)")
	promptAugmentation := flagSet.String("augment", "", "additional system prompt instructions appended after the default prompt")
	baseURL := flagSet.String("openai-base-url", defaultBaseURL, "override the OpenAI API base URL (optional)")

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
	}

	agent, err := runtime.NewRuntime(options)
	if err != nil {
		fmt.Fprintf(stderr, "failed to create runtime: %v\n", err)
		return 1
	}

	outputs := agent.Outputs()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for evt := range outputs {
			if evt.Type == runtime.EventTypeAssistantMessage {
				rendered, err := glamour.Render(evt.Message, "dark")
				if err != nil {
					// Fall back to the plain message if Glamour rendering fails so the user still
					// sees the assistant output.
					fmt.Fprintln(stdout, evt.Message)
				} else {
					fmt.Fprint(stdout, rendered)
				}
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

	if err := agent.Run(ctx); err != nil {
		fmt.Fprintf(stderr, "runtime error: %v\n", err)
		wg.Wait()
		return 1
	}

	wg.Wait()
	return 0
}
