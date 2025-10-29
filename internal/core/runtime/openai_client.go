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
	"os"
	"strconv"
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
	// Optional debug streaming: set GOAGENT_DEBUG_STREAM=1 to enable verbose prints
	debugStream := strings.TrimSpace(os.Getenv("GOAGENT_DEBUG_STREAM")) != ""
	if debugStream {
		fmt.Println("====== STREAM: entering RequestPlanStreamingResponses")
	}
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
	if debugStream {
		fmt.Println("====== STREAM: HTTP connected; starting SSE read loop")
	}

	reader := bufio.NewReader(resp.Body)
	var toolID, toolName, toolArgs string
	var lastEmittedMessage string
	var lastEmittedReasoningCount int

	// Debug streaming is already enabled at function entry.

	// Extract the "message" field from the partially-streamed JSON arguments.
	// Emit only new suffix of the decoded message to simulate streaming.
	emitMessageDelta := func(buf string) {
		if onDelta == nil {
			return
		}
		if raw, _, ok := extractPartialJSONStringField(buf, "message"); ok {
			decoded := decodePartialJSONString(raw)
			if decoded == "" {
				return
			}
			if lastEmittedMessage == "" {
				onDelta(decoded)
				lastEmittedMessage = decoded
				return
			}
			if strings.HasPrefix(decoded, lastEmittedMessage) {
				onDelta(decoded[len(lastEmittedMessage):])
				lastEmittedMessage = decoded
			} else if decoded != lastEmittedMessage {
				onDelta(decoded)
				lastEmittedMessage = decoded
			}
		}
	}

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
			if debugStream {
				fmt.Println("------ STREAM: [DONE]")
			}
			break
		}

		var evt map[string]any
		if err := json.Unmarshal([]byte(chunkData), &evt); err != nil {
			if debugStream {
				fmt.Println("------ STREAM: decode-error", err)
			}
			continue
		}
		t, _ := evt["type"].(string)
		if debugStream {
			if t == "" {
				fmt.Println("------ STREAM: event ?")
			} else {
				fmt.Println("------ STREAM:", t)
			}
		}
		switch t {
		case "response.output_text.delta":
			if s, _ := evt["delta"].(string); s != "" {
				if onDelta != nil {
					onDelta(s)
				}
			}
		case "response.function_call.delta", "response.tool_call.delta", "message.function_call.delta", "message.tool_call.delta",
			// underscore variants observed in live streams
			"response_function_call_delta", "response_tool_call_delta", "response_function_call_arguments_delta":
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
				emitMessageDelta(toolArgs)
				emitReasoningDeltas := func(buf string) {
					if onDelta == nil {
						return
					}
					if vals, _, ok := extractPartialJSONStringArrayField(buf, "reasoning"); ok {
						// Emit any newly completed entries.
						if lastEmittedReasoningCount < len(vals) {
							for i := lastEmittedReasoningCount; i < len(vals); i++ {
								if v := strings.TrimSpace(vals[i]); v != "" {
									onDelta("\n" + v)
								}
							}
							lastEmittedReasoningCount = len(vals)
						}
					}
				}
				emitReasoningDeltas(toolArgs)
			} else if ds, _ := evt["delta"].(string); ds != "" {
				toolArgs += ds
				emitMessageDelta(toolArgs)
				// See above: stream any completed reasoning entries
				if onDelta != nil {
					if vals, _, ok := extractPartialJSONStringArrayField(toolArgs, "reasoning"); ok {
						if lastEmittedReasoningCount < len(vals) {
							for i := lastEmittedReasoningCount; i < len(vals); i++ {
								if v := strings.TrimSpace(vals[i]); v != "" {
									onDelta("\n" + v)
								}
							}
							lastEmittedReasoningCount = len(vals)
						}
					}
				}
			} else if dm, _ := evt["delta"].(map[string]any); dm != nil {
				if s, _ := dm["arguments"].(string); s != "" {
					toolArgs += s
					emitMessageDelta(toolArgs)
					if onDelta != nil {
						if vals, _, ok := extractPartialJSONStringArrayField(toolArgs, "reasoning"); ok {
							if lastEmittedReasoningCount < len(vals) {
								for i := lastEmittedReasoningCount; i < len(vals); i++ {
									if v := strings.TrimSpace(vals[i]); v != "" {
										onDelta("\n" + v)
									}
								}
								lastEmittedReasoningCount = len(vals)
							}
						}
					}
				}
				if n, _ := dm["name"].(string); n != "" {
					toolName = n
				}
			}
		case "response.function_call.arguments.delta", "response.tool_call.arguments.delta", "message.function_call.arguments.delta", "message.tool_call.arguments.delta",
			// underscore variants observed in live streams
			"response.function_call_arguments.delta", "response.tool_call_arguments.delta":
			// Some servers emit a dedicated arguments.delta event.
			if s, _ := evt["delta"].(string); s != "" {
				toolArgs += s
				emitMessageDelta(toolArgs)
				if onDelta != nil {
					if vals, _, ok := extractPartialJSONStringArrayField(toolArgs, "reasoning"); ok {
						if lastEmittedReasoningCount < len(vals) {
							for i := lastEmittedReasoningCount; i < len(vals); i++ {
								if v := strings.TrimSpace(vals[i]); v != "" {
									onDelta("\n" + v)
								}
							}
							lastEmittedReasoningCount = len(vals)
						}
					}
				}
			}
		case "message.delta", "response.message.delta":
			// Some gateways emit structured message delta arrays; try to extract text.
			if dm, _ := evt["delta"].(map[string]any); dm != nil {
				// Common pattern: { type: "output_text.delta", text: "..." }
				if t, _ := dm["type"].(string); t == "output_text.delta" {
					if s, _ := dm["text"].(string); s != "" && onDelta != nil {
						onDelta(s)
					}
				}
			} else if arr, _ := evt["delta"].([]any); arr != nil {
				for _, it := range arr {
					if m, _ := it.(map[string]any); m != nil {
						if t, _ := m["type"].(string); t == "output_text.delta" {
							if s, _ := m["text"].(string); s != "" && onDelta != nil {
								onDelta(s)
							}
						}
					}
				}
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

// extractPartialJSONStringField scans a partial JSON object for a given field name
// and returns the raw (still JSON-escaped) string value content if found.
// complete=true when an unescaped closing quote was found.
func extractPartialJSONStringField(buf, field string) (raw string, complete bool, ok bool) {
	// Find the last occurrence to favor the most recent partial chunk.
	key := "\"" + field + "\""
	idx := strings.LastIndex(buf, key)
	if idx == -1 {
		return "", false, false
	}
	// Scan forward: key" : "
	i := idx + len(key)
	// skip whitespace
	for i < len(buf) && (buf[i] == ' ' || buf[i] == '\n' || buf[i] == '\t' || buf[i] == '\r') {
		i++
	}
	if i >= len(buf) || buf[i] != ':' {
		return "", false, false
	}
	i++
	for i < len(buf) && (buf[i] == ' ' || buf[i] == '\n' || buf[i] == '\t' || buf[i] == '\r') {
		i++
	}
	if i >= len(buf) || buf[i] != '"' {
		return "", false, false
	}
	// value starts after the opening quote
	start := i + 1
	// Walk until an unescaped closing quote or end of buffer
	for i = start; i < len(buf); i++ {
		c := buf[i]
		if c == '\\' {
			// Skip escaped char if present
			if i+1 < len(buf) {
				if buf[i+1] == 'u' {
					// Attempt to skip \uXXXX if complete, otherwise break
					if i+6 <= len(buf) {
						i += 5 // loop will i++ -> total +6
						continue
					}
					// incomplete unicode escape
					return buf[start:i], false, true
				}
				i++
				continue
			}
			// trailing backslash
			return buf[start:i], false, true
		}
		if c == '"' {
			// Found terminating quote
			return buf[start:i], true, true
		}
	}
	// Incomplete string reaches buffer end
	return buf[start:], false, true
}

// decodePartialJSONString decodes a JSON string content (without surrounding quotes)
// while tolerating truncated/incomplete trailing escapes.
func decodePartialJSONString(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		// Escape sequence
		if i+1 >= len(s) {
			// trailing backslash, stop
			break
		}
		esc := s[i+1]
		switch esc {
		case '"', '\\', '/':
			b.WriteByte(esc)
			i++
		case 'b':
			b.WriteByte('\b')
			i++
		case 'f':
			b.WriteByte('\f')
			i++
		case 'n':
			b.WriteByte('\n')
			i++
		case 'r':
			b.WriteByte('\r')
			i++
		case 't':
			b.WriteByte('\t')
			i++
		case 'u':
			if i+6 <= len(s) {
				hex := s[i+2 : i+6]
				if v, err := strconv.ParseInt(hex, 16, 32); err == nil {
					b.WriteRune(rune(v))
					i += 5 // account for \uXXXX (loop adds +1)
				} else {
					// invalid hex, write literally
					b.WriteString("\\u")
					i++
				}
			} else {
				// incomplete unicode escape; stop
				i = len(s)
			}
		default:
			// unknown escape, write literally
			b.WriteByte('\\')
			// if last char was backslash and next is unknown, write it too if present
			if i+1 < len(s) {
				b.WriteByte(esc)
				i++
			}
		}
	}
	return b.String()
}

// extractPartialJSONStringArrayField finds a JSON array of strings under the given field
// name within a partial JSON object and returns the list of fully parsed elements
// encountered so far. The function is tolerant of truncated buffers and will stop
// before an incomplete string or missing closing bracket.
func extractPartialJSONStringArrayField(buf, field string) (values []string, complete bool, ok bool) {
	key := "\"" + field + "\""
	idx := strings.LastIndex(buf, key)
	if idx == -1 {
		return nil, false, false
	}
	i := idx + len(key)
	// skip to ':'
	for i < len(buf) && (buf[i] == ' ' || buf[i] == '\n' || buf[i] == '\t' || buf[i] == '\r') {
		i++
	}
	if i >= len(buf) || buf[i] != ':' {
		return nil, false, false
	}
	i++
	for i < len(buf) && (buf[i] == ' ' || buf[i] == '\n' || buf[i] == '\t' || buf[i] == '\r') {
		i++
	}
	if i >= len(buf) || buf[i] != '[' {
		return nil, false, false
	}
	// move past '['
	i++
	// Parse string entries until incomplete
	for i < len(buf) {
		// skip whitespace and commas
		for i < len(buf) {
			c := buf[i]
			if c == ' ' || c == '\n' || c == '\t' || c == '\r' || c == ',' {
				i++
				continue
			}
			break
		}
		if i >= len(buf) {
			return values, false, true
		}
		if buf[i] == ']' {
			return values, true, true
		}
		if buf[i] != '"' {
			return values, false, true
		}
		// parse quoted string
		start := i + 1
		j := start
		for j < len(buf) {
			c := buf[j]
			if c == '\\' {
				if j+1 < len(buf) {
					if buf[j+1] == 'u' {
						if j+6 <= len(buf) {
							j += 6
							continue
						} else {
							return values, false, true
						}
					}
					j += 2
					continue
				}
				return values, false, true
			}
			if c == '"' {
				raw := buf[start:j]
				values = append(values, decodePartialJSONString(raw))
				j++
				i = j
				break
			}
			j++
		}
		if j >= len(buf) {
			return values, false, true
		}
		// loop continues to parse next value or closing bracket
	}
	return values, false, true
}
