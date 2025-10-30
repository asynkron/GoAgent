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

	"github.com/joho/godotenv"

	"github.com/asynkron/goagent/internal/bootprobe"
	"github.com/asynkron/goagent/internal/core/runtime"
	tuiui "github.com/asynkron/goagent/internal/tui"
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
	// Optional: submit a prompt immediately. In TUI mode this will be enqueued
	// on startup.
	prompt := flagSet.String("prompt", "", "submit this prompt immediately")
	// Research hands-free mode: pass a JSON object {"goal":"...","turns":N}
	research := flagSet.String("research", "", "hands-free mode: JSON {\"goal\":\"...\", \"turns\":N}")

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

	// Research mode takes precedence over --prompt.
	if spec := strings.TrimSpace(*research); spec != "" {
		// Accept a compact JSON like {"goal":"...","turns":20}
		type researchSpec struct {
			Goal  string `json:"goal"`
			Turns int    `json:"turns"`
		}
		var rs researchSpec
		if err := json.Unmarshal([]byte(spec), &rs); err != nil {
			fmt.Fprintf(stderr, "invalid --research JSON: %v\n", err)
			return 2
		}
		rs.Goal = strings.TrimSpace(rs.Goal)
		if rs.Goal == "" {
			fmt.Fprintln(stderr, "--research requires non-empty goal")
			return 2
		}
		if rs.Turns < 0 {
			rs.Turns = 0
		}
		options.HandsFree = true
		options.HandsFreeTopic = rs.Goal
		if rs.Turns > 0 {
			options.MaxPasses = rs.Turns
		}
		options.HandsFreeAutoReply = fmt.Sprintf("Please continue to work on the set goal. No human available. Goal: %s", rs.Goal)
	} else if p := strings.TrimSpace(*prompt); p != "" {
		// TUI is the only UI. If a prompt is provided, set hands-free so the
		// runtime will submit it immediately on startup.
		options.HandsFree = true
		options.HandsFreeTopic = p
	}
	return tuiui.Run(ctx, options)
}
