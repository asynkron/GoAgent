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

func TestRequestPlanIncludesResponseFormat(t *testing.T) {
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

	format, ok := captured["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("expected response_format to be present in request")
	}

	if got := format["type"]; got != "json_schema" {
		t.Fatalf("expected response_format.type=json_schema, got %v", got)
	}

	schemaEnvelope, ok := format["json_schema"].(map[string]any)
	if !ok {
		t.Fatalf("expected response_format.json_schema to be an object")
	}

	if strict, _ := schemaEnvelope["strict"].(bool); !strict {
		t.Fatalf("expected response_format.json_schema.strict to be true")
	}

	if name, _ := schemaEnvelope["name"].(string); name != schema.ToolName {
		t.Fatalf("expected schema name %q, got %q", schema.ToolName, name)
	}

	if _, ok := schemaEnvelope["schema"].(map[string]any); !ok {
		t.Fatalf("expected embedded schema to be present")
	}
}
