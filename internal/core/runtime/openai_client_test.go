package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/asynkron/goagent/internal/core/schema"
)

func TestRequestPlanUsesFunctionToolShape(t *testing.T) {
	t.Parallel()

	var (
		captured    map[string]any
		requestHost string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Record the host so the test can assert that the custom base URL is respected.
		requestHost = r.Host
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("failed to decode request: %v", err)
		}

		response := map[string]any{
			"choices": []any{
				map[string]any{
					"message": map[string]any{
						"tool_calls": []any{
							map[string]any{
								"id": "call-1",
								"function": map[string]any{
									"name":      schema.ToolName,
									"arguments": `{"message":"hi","plan":[],"requireHumanInput":false}`,
								},
							},
						},
					},
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Fatalf("failed to encode response: %v", err)
		}
	}))
	defer server.Close()

	client, err := NewOpenAIClient("test-key", "test-model", "", server.URL)
	if err != nil {
		t.Fatalf("unexpected client error: %v", err)
	}
	client.httpClient = server.Client()

	history := []ChatMessage{{Role: RoleUser, Content: "hi"}}
	_, err = client.RequestPlan(context.Background(), history)
	if err != nil {
		t.Fatalf("RequestPlan returned error: %v", err)
	}

	parsedURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("failed to parse server URL: %v", err)
	}
	if requestHost != parsedURL.Host {
		t.Fatalf("expected request host %s to match server host %s", requestHost, parsedURL.Host)
	}

	// Validate Responses API function tool shape (flat function entry)
	tools, ok := captured["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("expected tools to contain one entry, got %T (len=%d)", captured["tools"], len(tools))
	}
	t0, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("expected tools[0] to be an object")
	}
	if t0["type"] != "function" {
		t.Fatalf("expected tools[0].type=function, got %v", t0["type"])
	}
	if t0["name"] != schema.ToolName {
		t.Fatalf("expected tools[0].name=%s, got %v", schema.ToolName, t0["name"])
	}
	params, ok := t0["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("expected tools[0].parameters to be an object")
	}
	if _, ok := params["type"].(string); !ok {
		t.Fatalf("expected parameters schema to include a type field")
	}

	// Validate tool_choice required
	if captured["tool_choice"] != "required" {
		t.Fatalf("expected tool_choice=required, got %v", captured["tool_choice"])
	}
}
