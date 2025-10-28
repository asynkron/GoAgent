package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/asynkron/goagent/internal/core/schema"
)

// OpenAIClient wraps the HTTP client required to call the OpenAI Responses API.
type OpenAIClient struct {
	apiKey          string
	model           string
	reasoningEffort string
	httpClient      *http.Client
	tool            schema.ToolDefinition
	baseURL         string
}

const defaultOpenAIBaseURL = "https://api.openai.com/v1"

// NewOpenAIClient configures the client with the provided API key and model identifier.
func NewOpenAIClient(apiKey, model, reasoningEffort, baseURL string) (*OpenAIClient, error) {
	if apiKey == "" {
		return nil, errors.New("openai: API key is required")
	}
	if model == "" {
		return nil, errors.New("openai: model is required")
	}
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = defaultOpenAIBaseURL
	}
	tool, err := schema.Definition()
	if err != nil {
		return nil, err
	}
	return &OpenAIClient{
		apiKey:          apiKey,
		model:           model,
		reasoningEffort: reasoningEffort,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
		tool:    tool,
		baseURL: baseURL,
	}, nil
}

// RequestPlan sends the accumulated chat history to OpenAI and returns the
// resulting tool call payload so the runtime can perform validation before
// decoding it.
func (c *OpenAIClient) RequestPlan(ctx context.Context, history []ChatMessage) (ToolCall, error) {
	// Non-streaming path reuses the Responses API implementation without emitting deltas.
	return c.RequestPlanStreamingResponses(ctx, history, nil)
}

// Chat Completions helpers, types, and streaming have been removed.

// RequestPlanStreamingResponses streams using the modern OpenAI Responses API.
// It maps response.output_text.delta chunks to the onDelta callback and collects
// function_call deltas into a ToolCall to return on completion.
func (c *OpenAIClient) RequestPlanStreamingResponses(ctx context.Context, history []ChatMessage, onDelta func(string)) (ToolCall, error) {
	// Build request body from history using Responses API content types.
	// Map to supported roles and content types:
	// - system, user, developer -> input_text
	// - assistant -> output_text
	// Note: ChatCompletions "tool" role is not valid for Responses. We map
	// tool messages to the "developer" role so the model can consume them as
	// host-provided context.
	inputMsgs := make([]map[string]any, 0, len(history))
	for _, m := range history {
		// Map tool role to developer for Responses API
		finalRole := string(m.Role)
		if m.Role == RoleTool {
			finalRole = "developer"
		}
		// Determine content type expected by the Responses API for final role
		contentType := "input_text"
		if finalRole == "assistant" {
			contentType = "output_text"
		}

		msg := map[string]any{
			"role": finalRole,
			"content": []map[string]any{
				{
					"type": contentType,
					"text": m.Content,
				},
			},
		}

		inputMsgs = append(inputMsgs, msg)
	}

	reqBody := map[string]any{
		"model":  c.model,
		"input":  inputMsgs,
		"stream": true,
		// Define the function tool in the flat Responses shape and require a tool call.
		"tools": []map[string]any{
			{
				"type":        "function",
				"name":        c.tool.Name,
				"description": c.tool.Description,
				"parameters":  c.tool.Parameters,
			},
		},
		// Require a tool call; with only one tool defined, this forces the model
		// to call our tool with arguments.
		"tool_choice": "required",
	}
	if c.reasoningEffort != "" {
		reqBody["reasoning"] = map[string]any{"effort": c.reasoningEffort}
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return ToolCall{}, fmt.Errorf("openai(responses): encode request: %w", err)
	}

	// Derive API root from configured baseURL.
	apiRoot := strings.TrimRight(c.baseURL, "/")
	url := apiRoot + "/responses"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return ToolCall{}, fmt.Errorf("openai(responses): build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ToolCall{}, fmt.Errorf("openai(responses): do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return ToolCall{}, fmt.Errorf("openai(responses): status %s: %s", resp.Status, string(msg))
	}

	reader := bufio.NewReader(resp.Body)
	var toolID, toolName, toolArgs string

	for {
		line, rerr := reader.ReadString('\n')
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return ToolCall{}, fmt.Errorf("openai(responses): stream read: %w", rerr)
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ":") {
			continue // keepalive/comment
		}
		if !strings.HasPrefix(line, "data:") && !strings.HasPrefix(line, "data: ") {
			continue
		}
		chunkData := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		if chunkData == "[DONE]" {
			break
		}

		var evt map[string]any
		if err := json.Unmarshal([]byte(chunkData), &evt); err != nil {
			continue
		}
		t, _ := evt["type"].(string)
		switch t {
		case "response.output_text.delta":
			if s, _ := evt["delta"].(string); s != "" {
				if onDelta != nil {
					onDelta(s)
				}
			}
		case "response.function_call.delta", "response.tool_call.delta", "message.function_call.delta", "message.tool_call.delta":
			// Capture tool call metadata and accumulate streaming JSON arguments.
			if name, _ := evt["name"].(string); name != "" {
				toolName = name
			}
			if id, _ := evt["call_id"].(string); id != "" {
				toolID = id
			}
			// Arguments may be provided as top-level "arguments" string, as a
			// raw delta string, or nested under a delta object.
			if args, _ := evt["arguments"].(string); args != "" {
				toolArgs += args
			} else if ds, _ := evt["delta"].(string); ds != "" {
				toolArgs += ds
			} else if dm, _ := evt["delta"].(map[string]any); dm != nil {
				if s, _ := dm["arguments"].(string); s != "" {
					toolArgs += s
				}
				if n, _ := dm["name"].(string); n != "" {
					toolName = n
				}
			}
		case "response.function_call.arguments.delta", "response.tool_call.arguments.delta", "message.function_call.arguments.delta", "message.tool_call.arguments.delta":
			// Some servers emit a dedicated arguments.delta event.
			if s, _ := evt["delta"].(string); s != "" {
				toolArgs += s
			}
		case "response.completed", "response.output_text.done", "response.function_call.completed":
			// On completion, some servers include the final aggregated payload
			// instead of streaming arguments. Attempt a best-effort extraction
			// of tool name/id/arguments from the nested response object if we
			// haven't already captured them.
			if toolArgs == "" || toolName == "" || toolID == "" {
				if respObj, _ := evt["response"].(map[string]any); respObj != nil {
					// Helper to find first string for a key by DFS.
					var findString func(any, string) (string, bool)
					findString = func(v any, key string) (string, bool) {
						switch vv := v.(type) {
						case map[string]any:
							if s, ok := vv[key].(string); ok && s != "" {
								return s, true
							}
							for _, child := range vv {
								if s, ok := findString(child, key); ok {
									return s, true
								}
							}
						case []any:
							for _, child := range vv {
								if s, ok := findString(child, key); ok {
									return s, true
								}
							}
						}
						return "", false
					}
					if toolName == "" {
						if s, ok := findString(respObj, "name"); ok {
							toolName = s
						}
					}
					if toolID == "" {
						if s, ok := findString(respObj, "call_id"); ok {
							toolID = s
						}
					}
					if toolArgs == "" {
						if s, ok := findString(respObj, "arguments"); ok {
							toolArgs = s
						}
					}
				}
			}
			// ignore otherwise; loop will end on EOF or [DONE]
		default:
			// ignore other event types
		}
	}

	if toolName != "" {
		return ToolCall{ID: toolID, Name: toolName, Arguments: toolArgs}, nil
	}
	// No tool call is valid for plain text responses
	return ToolCall{}, nil
}
