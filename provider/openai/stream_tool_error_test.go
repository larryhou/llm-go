package openai

// stream_tool_error_test.go — white-box tests for EventToolError on JSON unmarshal failure.
// Uses an httptest.Server that returns a streaming OpenAI-compatible response
// containing a tool call with deliberately invalid JSON arguments.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/larryhou/llm-go/llm"
)

// fakeOpenAISSE writes an OpenAI streaming response containing one tool call
// whose arguments are the given argsJSON string (may be invalid JSON).
func fakeOpenAISSE(w http.ResponseWriter, toolID, toolName, argsJSON string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	f := w.(http.Flusher)

	send := func(data string) {
		fmt.Fprintf(w, "data: %s\n\n", data)
		f.Flush()
	}

	// Chunk 1: tool call delta with the tool name and id.
	send(fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":""}}]},"finish_reason":null}],"usage":null}`,
		toolID, toolName))

	// Chunk 2: arguments delta (the potentially-invalid JSON).
	send(fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":%q}}]},"finish_reason":null}],"usage":null}`,
		argsJSON))

	// Chunk 3: finish_reason=tool_calls — triggers finalisation.
	send(fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))

	send("[DONE]")
}

// drainEvents collects all events from a Provider.Stream call.
func drainEvents(t *testing.T, prov *Provider, req llm.Request) []llm.Event {
	t.Helper()
	ch, err := prov.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var events []llm.Event
	for ev := range ch {
		events = append(events, ev)
	}
	return events
}

func minimalRequest(baseURL string) llm.Request {
	return llm.Request{
		Model: llm.Model{
			ID:         "gpt-4o",
			ProviderID: "openai",
			APIID:      "gpt-4o",
			Limit:      llm.ModelLimit{Context: 128000, Output: 4096},
		},
		Messages: []llm.Message{llm.NewUserMessage("hello")},
	}
}

// TestStream_InvalidToolArgs_EmitsEventToolError verifies that when the OpenAI
// stream delivers a tool call with invalid JSON arguments, the provider emits
// EventToolError (not EventToolCall with nil Input, and not a panic).
func TestStream_InvalidToolArgs_EmitsEventToolError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fakeOpenAISSE(w, "call-1", "my_tool", "NOT VALID JSON {{{{")
	}))
	defer srv.Close()

	prov := New("test-key", srv.URL+"/v1", "openai", nil)
	events := drainEvents(t, prov, minimalRequest(srv.URL))

	// Must have at least one EventToolError.
	var toolErrors []llm.Event
	var toolCalls []llm.Event
	for _, ev := range events {
		switch ev.Type {
		case llm.EventToolError:
			toolErrors = append(toolErrors, ev)
		case llm.EventToolCall:
			toolCalls = append(toolCalls, ev)
		}
	}

	if len(toolErrors) == 0 {
		t.Error("expected at least one EventToolError for invalid tool args, got none")
	}
	if len(toolErrors) > 0 {
		ev := toolErrors[0]
		if ev.ToolCallID == "" {
			t.Error("EventToolError should carry ToolCallID")
		}
		if ev.ToolName == "" {
			t.Error("EventToolError should carry ToolName")
		}
		if ev.Err == nil {
			t.Error("EventToolError should have non-nil Err")
		}
	}

	// EventToolCall must NOT be emitted for this (invalid) call.
	for _, ev := range toolCalls {
		if ev.ToolCallID == "call-1" {
			t.Errorf("EventToolCall emitted for invalid-JSON tool call (id=call-1); should be EventToolError only")
		}
	}
}

// TestStream_ValidToolArgs_EmitsEventToolCall verifies that a valid tool call
// still produces EventToolCall (not EventToolError).
func TestStream_ValidToolArgs_EmitsEventToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fakeOpenAISSE(w, "call-2", "search", `{"query":"golang"}`)
	}))
	defer srv.Close()

	prov := New("test-key", srv.URL+"/v1", "openai", nil)
	events := drainEvents(t, prov, minimalRequest(srv.URL))

	var toolCalls []llm.Event
	var toolErrors []llm.Event
	for _, ev := range events {
		switch ev.Type {
		case llm.EventToolCall:
			toolCalls = append(toolCalls, ev)
		case llm.EventToolError:
			toolErrors = append(toolErrors, ev)
		}
	}

	if len(toolErrors) > 0 {
		t.Errorf("unexpected EventToolError for valid tool args: %v", toolErrors[0].Err)
	}
	if len(toolCalls) == 0 {
		t.Error("expected EventToolCall for valid tool args, got none")
	}
	if len(toolCalls) > 0 && toolCalls[0].Input == nil {
		t.Error("EventToolCall.Input should not be nil for valid tool args")
	}
}
