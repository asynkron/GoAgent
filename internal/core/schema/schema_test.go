package schema

import "testing"

func TestPlanResponseSchemaRequiresReasoning(t *testing.T) {
	t.Parallel()

	schemaMap, err := PlanResponseSchema()
	if err != nil {
		t.Fatalf("PlanResponseSchema returned error: %v", err)
	}

	required, ok := schemaMap["required"].([]any)
	if !ok {
		t.Fatalf("expected required list to be present")
	}

	var reasoningRequired bool
	for _, value := range required {
		if str, _ := value.(string); str == "reasoning" {
			reasoningRequired = true
			break
		}
	}
	if !reasoningRequired {
		t.Fatalf("expected reasoning to be marked as required")
	}

	properties, ok := schemaMap["properties"].(map[string]any)
	if !ok {
		t.Fatalf("expected schema properties to be present")
	}

	value, ok := properties["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("expected reasoning property to be defined")
	}

	if typ, _ := value["type"].(string); typ != "string" {
		t.Fatalf("expected reasoning to be a string, got %q", typ)
	}
}
