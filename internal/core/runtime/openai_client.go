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

// OpenAIClient wraps the HTTP client required to call the Chat Completions API.
type OpenAIClient struct {
	apiKey          string
	model           string
	reasoningEffort string
	httpClient      *http.Client
	tool            schema.ToolDefinition
	baseURL         string
}

const defaultOpenAIBaseURL = "https://api.openai.com/v1/chat/completions"

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
	payload := chatCompletionRequest{
		Model:    c.model,
		Messages: buildMessages(history),
		Tools: []toolSpecification{{
			Type:     "function",
			Function: functionDefinition{Name: c.tool.Name, Description: c.tool.Description, Parameters: c.tool.Parameters},
		}},
		ToolChoice: toolChoice{Type: "function", Function: &toolChoiceFunction{Name: c.tool.Name}},
		ResponseFormat: responseFormat{
			Type: "json_schema",
			JSONSchema: jsonSchemaDefinition{
				Name:   c.tool.Name,
				Strict: true,
				Schema: c.tool.Parameters,
			},
		},
	}

	if c.reasoningEffort != "" {
		payload.ReasoningEffort = &c.reasoningEffort
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return ToolCall{}, fmt.Errorf("openai: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return ToolCall{}, fmt.Errorf("openai: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ToolCall{}, fmt.Errorf("openai: do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return ToolCall{}, fmt.Errorf("openai: status %s: %s", resp.Status, string(msg))
	}

	var completion chatCompletionResponse
	decoder := json.NewDecoder(resp.Body)
	if err := decoder.Decode(&completion); err != nil {
		return ToolCall{}, fmt.Errorf("openai: decode response: %w", err)
	}

	if len(completion.Choices) == 0 {
		return ToolCall{}, errors.New("openai: response contained no choices")
	}
	choice := completion.Choices[0]
	if len(choice.Message.ToolCalls) == 0 {
		return ToolCall{}, errors.New("openai: assistant did not call the tool")
	}

	toolCall := choice.Message.ToolCalls[0]
	return ToolCall{
		ID:        toolCall.ID,
		Name:      toolCall.Function.Name,
		Arguments: toolCall.Function.Arguments,
	}, nil
}

func buildMessages(history []ChatMessage) []chatMessage {
	messages := make([]chatMessage, 0, len(history))
	for _, entry := range history {
		msg := chatMessage{
			Role:    string(entry.Role),
			Content: entry.Content,
		}
		if entry.Role == RoleTool {
			msg.Name = schema.ToolName
			msg.ToolCallID = entry.ToolCallID
		}
		if len(entry.ToolCalls) > 0 {
			calls := make([]assistantToolCall, 0, len(entry.ToolCalls))
			for _, call := range entry.ToolCalls {
				calls = append(calls, assistantToolCall{
					ID:   call.ID,
					Type: "function",
					Function: assistantToolFunction{
						Name:      call.Name,
						Arguments: call.Arguments,
					},
				})
			}
			msg.ToolCalls = calls
		}
		messages = append(messages, msg)
	}
	return messages
}

// chatCompletionRequest and related types are intentionally minimal mirrors of the
// OpenAI Chat Completions API payloads. They allow us to construct the request
// without pulling in a heavy client dependency.
type chatCompletionRequest struct {
	Model           string              `json:"model"`
	Messages        []chatMessage       `json:"messages"`
	Tools           []toolSpecification `json:"tools"`
	ToolChoice      toolChoice          `json:"tool_choice"`
	ReasoningEffort *string             `json:"reasoning_effort,omitempty"`
	ResponseFormat  responseFormat      `json:"response_format"`
	Stream          bool                `json:"stream,omitempty"`
}

type chatMessage struct {
	Role       string              `json:"role"`
	Content    string              `json:"content,omitempty"`
	Name       string              `json:"name,omitempty"`
	ToolCallID string              `json:"tool_call_id,omitempty"`
	ToolCalls  []assistantToolCall `json:"tool_calls,omitempty"`
}

type assistantToolCall struct {
	ID       string                `json:"id"`
	Type     string                `json:"type"`
	Function assistantToolFunction `json:"function"`
}

type assistantToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type toolSpecification struct {
	Type     string             `json:"type"`
	Function functionDefinition `json:"function"`
}

type functionDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type toolChoice struct {
	Type     string              `json:"type"`
	Function *toolChoiceFunction `json:"function,omitempty"`
}

type toolChoiceFunction struct {
	Name string `json:"name"`
}

type responseFormat struct {
	Type       string               `json:"type"`
	JSONSchema jsonSchemaDefinition `json:"json_schema"`
}

type jsonSchemaDefinition struct {
	Name   string         `json:"name"`
	Strict bool           `json:"strict"`
	Schema map[string]any `json:"schema"`
}

type chatCompletionResponse struct {
	Choices []struct {
		Message struct {
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
}

// RequestPlanStreaming behaves like RequestPlan but enables SSE streaming. It invokes
// onDelta for assistant-visible content deltas while buffering tool_call deltas
// until the stream completes. Only after the final chunk has been received does it
// return the ToolCall needed to proceed with validation and execution.
func (c *OpenAIClient) RequestPlanStreaming(ctx context.Context, history []ChatMessage, onDelta func(string)) (ToolCall, error) {
	payload := chatCompletionRequest{
		Model:    c.model,
		Messages: buildMessages(history),
		Tools: []toolSpecification{{
			Type:     "function",
			Function: functionDefinition{Name: c.tool.Name, Description: c.tool.Description, Parameters: c.tool.Parameters},
		}},
		ToolChoice: toolChoice{Type: "function", Function: &toolChoiceFunction{Name: c.tool.Name}},
		ResponseFormat: responseFormat{
			Type: "json_schema",
			JSONSchema: jsonSchemaDefinition{
				Name:   c.tool.Name,
				Strict: true,
				Schema: c.tool.Parameters,
			},
		},
		Stream: true,
	}
	if c.reasoningEffort != "" {
		payload.ReasoningEffort = &c.reasoningEffort
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return ToolCall{}, fmt.Errorf("openai: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return ToolCall{}, fmt.Errorf("openai: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ToolCall{}, fmt.Errorf("openai: do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return ToolCall{}, fmt.Errorf("openai: status %s: %s", resp.Status, string(msg))
	}

	reader := bufio.NewReader(resp.Body)
	var toolID, toolName, toolArgs string

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return ToolCall{}, fmt.Errorf("openai: stream read: %w", err)
		}
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "data: ") {
			continue
		}
		chunkData := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if chunkData == "[DONE]" {
			break
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(chunkData), &chunk); err != nil {
			// Ignore malformed keepalive lines
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		if onDelta != nil && delta.Content != "" {
			onDelta(delta.Content)
		}
		if len(delta.ToolCalls) > 0 {
			call := delta.ToolCalls[0]
			if call.ID != "" {
				toolID = call.ID
			}
			if call.Function.Name != "" {
				toolName = call.Function.Name
			}
			if call.Function.Arguments != "" {
				toolArgs += call.Function.Arguments
			}
		}
	}

	if toolName == "" {
		return ToolCall{}, errors.New("openai: assistant did not call the tool (stream)")
	}
	return ToolCall{ID: toolID, Name: toolName, Arguments: toolArgs}, nil
}
