// Package main runs a minimal HTTP SSE server that streams agent output.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	runtimepkg "github.com/asynkron/goagent/internal/core/runtime"
)

// sseWrite sends a single SSE event with the given name and data, followed by a flush.
func sseWrite(w http.ResponseWriter, flusher http.Flusher, event string, data string) error {
	if event != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
			return err
		}
	}
	// data lines must not contain raw newlines; split and prefix each line.
	for _, line := range strings.Split(data, "\n") {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprint(w, "\n"); err != nil { // end of event
		return err
	}
	flusher.Flush()
	return nil
}

func streamHandler(w http.ResponseWriter, r *http.Request) {
	// Basic SSE headers and anti-buffering flags
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	// Disable proxy buffering (nginx, etc.)
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		http.Error(w, "OPENAI_API_KEY not set", http.StatusInternalServerError)
		return
	}

	prompt := strings.TrimSpace(r.URL.Query().Get("q"))
	if prompt == "" {
		prompt = "Say hello with a few words."
	}

	// Build a fresh runtime instance per request to avoid multiplexing outputs
	// across multiple clients for this simple example.
	opts := runtimepkg.RuntimeOptions{
		APIKey:                  apiKey,
		Model:                   os.Getenv("OPENAI_MODEL"),
		ReasoningEffort:         os.Getenv("OPENAI_REASONING_EFFORT"),
		APIBaseURL:              os.Getenv("OPENAI_BASE_URL"),
		DisableOutputForwarding: true, // we will forward via SSE
		UseStreaming:            true,
		// Keep generous defaults
		EmitTimeout: 0,
	}

	agent, err := runtimepkg.NewRuntime(opts)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to create runtime: %v", err), http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	outputs := agent.Outputs()

	// Kick off the agent
	go func() {
		if err := agent.Run(ctx); err != nil {
			log.Printf("runtime error: %v", err)
		}
	}()

	// Submit the prompt
	agent.SubmitPrompt(prompt)

	// Initial comment to open the stream for some clients
	if _, err := fmt.Fprint(w, ": connected\n\n"); err == nil {
		flusher.Flush()
	}

	// Forward events until the request is canceled or the runtime closes.
	for {
		select {
		case <-r.Context().Done():
			return
		case evt, ok := <-outputs:
			if !ok {
				// Signal end-of-stream
				_ = sseWrite(w, flusher, "end", "runtime closed")
				return
			}
			// Marshal metadata if present for debugging
			var meta string
			if len(evt.Metadata) > 0 {
				if b, err := json.Marshal(evt.Metadata); err == nil {
					meta = string(b)
				}
			}
			switch evt.Type {
			case runtimepkg.EventTypeAssistantDelta:
				_ = sseWrite(w, flusher, "assistant_delta", evt.Message)
			case runtimepkg.EventTypeAssistantMessage:
				_ = sseWrite(w, flusher, "assistant_message", evt.Message)
			case runtimepkg.EventTypeStatus:
				_ = sseWrite(w, flusher, "status", evt.Message)
			case runtimepkg.EventTypeError:
				_ = sseWrite(w, flusher, "error", evt.Message)
			case runtimepkg.EventTypeRequestInput:
				_ = sseWrite(w, flusher, "request_input", evt.Message)
			default:
				// Unknown types as generic data
				payload := evt.Message
				if meta != "" {
					payload = payload + "\nmeta=" + meta
				}
				_ = sseWrite(w, flusher, "event", payload)
			}
		}
	}
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/stream", streamHandler)

	addr := ":8080"
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("SSE server listening on %s (GET /stream?q=your+prompt)", addr)
	log.Fatal(srv.ListenAndServe())
}
