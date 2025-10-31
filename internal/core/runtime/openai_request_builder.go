package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// buildMessagesFromHistory converts chat messages to the format expected by
// the OpenAI Responses API. It maps tool role to developer and determines
// appropriate content types.
func buildMessagesFromHistory(history []ChatMessage) []map[string]any {
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
	return inputMsgs
}

// buildRequestBody constructs the request body for the OpenAI Responses API.
func (c *OpenAIClient) buildRequestBody(inputMsgs []map[string]any) ([]byte, error) {
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

	return json.Marshal(reqBody)
}

// executeRequest performs the HTTP request and returns the response.
// It handles request building, authentication, and error checking.
func (c *OpenAIClient) executeRequest(ctx context.Context, payload []byte, start time.Time) (*http.Response, error) {
	// Derive API root from configured baseURL.
	apiRoot := strings.TrimRight(c.baseURL, "/")
	url := apiRoot + "/responses"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		c.logger.Error(ctx, "Failed to build OpenAI request", err,
			Field("url", url),
		)
		return nil, fmt.Errorf("openai(responses): build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		duration := time.Since(start)
		c.metrics.RecordAPICall(duration, false)
		c.logger.Error(ctx, "OpenAI API request failed", err,
			Field("url", url),
			Field("duration_ms", duration.Milliseconds()),
		)
		return nil, fmt.Errorf("openai(responses): do request: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		duration := time.Since(start)
		c.metrics.RecordAPICall(duration, false)
		c.logger.Error(ctx, "OpenAI API returned error status", fmt.Errorf("status %s: %s", resp.Status, string(msg)),
			Field("status_code", resp.StatusCode),
			Field("duration_ms", duration.Milliseconds()),
		)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("openai(responses): status %s: %s", resp.Status, string(msg))
	}

	return resp, nil
}
