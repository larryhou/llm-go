package openai

// build_params_test.go — white-box tests for buildParams tool schema error handling.
// Lives in package openai (not openai_test) because buildParams is unexported.

import (
	"strings"
	"testing"

	"github.com/larryhou/llm-go/llm"
)

func TestBuildParams_SchemaWithUnencodableValue_Error(t *testing.T) {
	model := llm.Model{
		ID:    "gpt-4o",
		APIID: "gpt-4o",
		Limit: llm.ModelLimit{Context: 128000, Output: 4096},
	}

	req := llm.Request{
		Model: model,
		Tools: []llm.ToolDefinition{
			{
				Name:        "bad_tool",
				Description: "a tool with an unencodable schema value",
				// A channel cannot be marshalled to JSON, causing marshal to fail.
				InputSchema: map[string]any{
					"type": make(chan int),
				},
			},
		},
	}

	_, err := buildParams(req)
	if err == nil {
		t.Fatal("buildParams should return error when tool schema marshal fails, got nil")
	}
	if !strings.Contains(err.Error(), "bad_tool") {
		t.Errorf("error should mention tool name %q, got: %v", "bad_tool", err)
	}
}

func TestBuildParams_ValidSchema_NoError(t *testing.T) {
	model := llm.Model{
		ID:    "gpt-4o",
		APIID: "gpt-4o",
		Limit: llm.ModelLimit{Context: 128000, Output: 4096},
	}

	req := llm.Request{
		Model: model,
		Tools: []llm.ToolDefinition{
			{
				Name:        "good_tool",
				Description: "a tool with a valid object schema",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{"type": "string"},
					},
					"required": []string{"query"},
				},
			},
		},
	}

	_, err := buildParams(req)
	if err != nil {
		t.Errorf("buildParams with valid schema should succeed, got: %v", err)
	}
}

