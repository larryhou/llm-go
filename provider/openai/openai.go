// Package openai implements the llm.Provider interface using the official
// openai-go SDK, which natively supports option.WithBaseURL for custom endpoints.
package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/ssestream"

	"github.com/larryhou/llm-go/auth"
	"github.com/larryhou/llm-go/config"
	"github.com/larryhou/llm-go/llm"
)

const ProviderID = "openai"

// EnvVars lists the environment variables checked for the OpenAI API key.
var EnvVars = []string{"OPENAI_API_KEY"}

// Provider implements llm.Provider for OpenAI and OpenAI-compatible endpoints.
type Provider struct {
	client     openai.Client
	providerID string
}

// New creates an OpenAI provider.
func New(apiKey, baseURL, providerID string, extraHeaders map[string]string) *Provider {
	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	for k, v := range extraHeaders {
		opts = append(opts, option.WithHeader(k, v))
	}
	pid := providerID
	if pid == "" {
		pid = ProviderID
	}
	return &Provider{client: openai.NewClient(opts...), providerID: pid}
}

// NewFromConfig builds a Provider from config + auth store.
func NewFromConfig(pid string, envVars []string, cfg *config.ProviderInfo, authStore *auth.Store) (*Provider, error) {
	if envVars == nil {
		envVars = EnvVars
	}
	apiKey, _ := auth.ResolveKey(pid, envVars, authStore)
	baseURL := ""
	var extraHeaders map[string]string

	if cfg != nil {
		if cfg.Options != nil {
			if cfg.Options.APIKey != "" {
				apiKey = cfg.Options.APIKey
			}
			if cfg.Options.BaseURL != "" {
				baseURL = cfg.Options.BaseURL
			}
		}
		if cfg.API != "" {
			baseURL = cfg.API
		}
		if cfg.Options != nil && len(cfg.Options.Extra) > 0 {
			extraHeaders = make(map[string]string)
			for k, v := range cfg.Options.Extra {
				if s, ok := v.(string); ok {
					extraHeaders[k] = s
				}
			}
		}
	}

	if apiKey == "" {
		return nil, fmt.Errorf("%s: no API key found", pid)
	}
	return New(apiKey, baseURL, pid, extraHeaders), nil
}

func (p *Provider) ID() string { return p.providerID }

// Factory is a provider.Factory-compatible function for this package.
// It uses ProviderID and EnvVars as defaults.
// Register it with a provider.Registry via registry.RegisterFactory(ProviderID, openai.Factory).
var Factory = func(cfg *config.ProviderInfo, authStore *auth.Store) (llm.Provider, error) {
	return NewFromConfig(ProviderID, EnvVars, cfg, authStore)
}

// Stream implements llm.Provider.
func (p *Provider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	params, err := buildParams(req)
	if err != nil {
		return nil, err
	}

	stream := p.client.Chat.Completions.NewStreaming(ctx, params)
	ch := make(chan llm.Event, 32)
	go func() {
		defer close(ch)
		runStream(ctx, p.providerID, stream, ch)
	}()
	return ch, nil
}

func buildParams(req llm.Request) (openai.ChatCompletionNewParams, error) {
	params := openai.ChatCompletionNewParams{
		Model: openai.ChatModel(req.Model.APIID),
		// Request usage statistics in the final stream chunk (OpenAI spec).
		// Providers that don't support this simply ignore the field.
		StreamOptions: openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: openai.Bool(true),
		},
	}

	if req.Options.MaxTokens > 0 {
		params.MaxCompletionTokens = openai.Int(int64(req.Options.MaxTokens))
	} else {
		params.MaxCompletionTokens = openai.Int(int64(llm.MaxOutputTokens(req.Model)))
	}

	if req.Options.Temperature != nil {
		params.Temperature = openai.Float(*req.Options.Temperature)
	}

	msgs, err := convertMessages(req.System, req.Messages)
	if err != nil {
		return params, err
	}
	params.Messages = msgs

	if len(req.Tools) > 0 {
		tools := make([]openai.ChatCompletionToolParam, 0, len(req.Tools))
		for _, t := range req.Tools {
			schemaBytes, err := json.Marshal(t.InputSchema)
			if err != nil {
				return params, fmt.Errorf("tool %q: marshal schema: %w", t.Name, err)
			}
			var schemaMap map[string]any
			_ = json.Unmarshal(schemaBytes, &schemaMap)
			tools = append(tools, openai.ChatCompletionToolParam{
				Function: openai.FunctionDefinitionParam{
					Name:        t.Name,
					Description: openai.String(t.Description),
					Parameters:  openai.FunctionParameters(schemaMap),
				},
			})
		}
		params.Tools = tools

		switch req.ToolChoice {
		case llm.ToolChoiceNone:
			params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{
				OfAuto: openai.String("none"),
			}
		case llm.ToolChoiceRequired:
			params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{
				OfAuto: openai.String("required"),
			}
		}
	}

	return params, nil
}

func convertMessages(system []string, msgs []llm.Message) ([]openai.ChatCompletionMessageParamUnion, error) {
	var out []openai.ChatCompletionMessageParamUnion

	for _, s := range system {
		if s != "" {
			out = append(out, openai.SystemMessage(s))
		}
	}

	for _, m := range msgs {
		switch m.Role {
		case llm.RoleUser:
			text := extractText(m.Content)
			out = append(out, openai.UserMessage(text))

		case llm.RoleAssistant:
			text := ""
			var toolCalls []openai.ChatCompletionMessageToolCallParam
			for _, p := range m.Content {
				switch p.Type {
				case llm.PartTypeText, llm.PartTypeReasoning:
					text += p.Text
				case llm.PartTypeToolCall:
					inputBytes, _ := json.Marshal(p.Input)
					toolCalls = append(toolCalls, openai.ChatCompletionMessageToolCallParam{
						ID: p.ToolCallID,
						Function: openai.ChatCompletionMessageToolCallFunctionParam{
							Name:      p.ToolName,
							Arguments: string(inputBytes),
						},
					})
				}
			}
			if len(toolCalls) > 0 {
				out = append(out, openai.ChatCompletionMessageParamUnion{
					OfAssistant: &openai.ChatCompletionAssistantMessageParam{
						Content:   openai.ChatCompletionAssistantMessageParamContentUnion{OfString: openai.String(text)},
						ToolCalls: toolCalls,
					},
				})
			} else {
				out = append(out, openai.AssistantMessage(text))
			}

		case llm.RoleTool:
			for _, p := range m.Content {
				if p.Type != llm.PartTypeToolResult {
					continue
				}
				resultText := ""
				if p.Result != nil {
					switch v := p.Result.Value.(type) {
					case string:
						resultText = v
					default:
						b, _ := json.Marshal(p.Result.Value)
						resultText = string(b)
					}
				}
				out = append(out, openai.ToolMessage(resultText, p.ToolCallID))
			}
		}
	}
	return out, nil
}

func extractText(parts []llm.ContentPart) string {
	text := ""
	for _, p := range parts {
		if p.Type == llm.PartTypeText {
			text += p.Text
		}
	}
	return text
}

// runStream processes the OpenAI streaming response and emits canonical events.
//
// Usage handling: with stream_options.include_usage=true, the provider sends
// a final chunk with choices=[] and usage={...} AFTER the finish_reason chunk.
// We defer EventStepFinish until we've seen that usage chunk (or the stream ends),
// so that usage is always populated when the event is emitted.
func runStream(ctx context.Context, providerID string, stream *ssestream.Stream[openai.ChatCompletionChunk], out chan<- llm.Event) {
	defer stream.Close()

	out <- llm.Event{Type: llm.EventRequestStart}
	out <- llm.Event{Type: llm.EventStepStart}

	type toolState struct {
		id   string
		name string
		args []byte
	}
	toolByIndex := map[int64]*toolState{}
	inText := false

	var usage llm.TokenUsage

	// pendingFinish holds a StepFinish event whose usage may still be arriving.
	// It is flushed when the usage chunk arrives or the stream ends.
	var pendingFinish *llm.Event

	flushPending := func() {
		if pendingFinish != nil {
			pendingFinish.Usage = usage
			out <- *pendingFinish
			pendingFinish = nil
		}
	}

	for stream.Next() {
		chunk := stream.Current()

		// Usage-only chunk (choices=[]) — sent by providers after the finish chunk
		// when stream_options.include_usage=true is set.
		if len(chunk.Choices) == 0 {
			if chunk.Usage.TotalTokens > 0 || chunk.JSON.Usage.Valid() {
				usage.Input = int(chunk.Usage.PromptTokens)
				usage.Output = int(chunk.Usage.CompletionTokens)
				usage.Total = int(chunk.Usage.TotalTokens)
			}
			// Now that we have usage, flush the deferred StepFinish.
			flushPending()
			continue
		}

		choice := chunk.Choices[0]
		delta := choice.Delta

		if delta.Content != "" {
			if !inText {
				out <- llm.Event{Type: llm.EventTextStart}
				inText = true
			}
			out <- llm.Event{Type: llm.EventTextDelta, Text: delta.Content}
		}

		// Also capture inline usage if the provider sends it alongside choices.
		if chunk.Usage.TotalTokens > 0 {
			usage.Input = int(chunk.Usage.PromptTokens)
			usage.Output = int(chunk.Usage.CompletionTokens)
			usage.Total = int(chunk.Usage.TotalTokens)
		}

		for _, tc := range delta.ToolCalls {
			idx := tc.Index
			ts, exists := toolByIndex[idx]
			if !exists {
				ts = &toolState{id: tc.ID, name: tc.Function.Name}
				toolByIndex[idx] = ts
				if tc.ID != "" {
					out <- llm.Event{
						Type:       llm.EventToolInputStart,
						ToolCallID: tc.ID,
						ToolName:   tc.Function.Name,
					}
				}
			}
			if tc.ID != "" && ts.id == "" {
				ts.id = tc.ID
			}
			if tc.Function.Name != "" && ts.name == "" {
				ts.name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				ts.args = append(ts.args, tc.Function.Arguments...)
				out <- llm.Event{
					Type:       llm.EventToolInputDelta,
					ToolCallID: ts.id,
					ToolName:   ts.name,
					Text:       tc.Function.Arguments,
				}
			}
		}

		if choice.FinishReason != "" {
			if inText {
				out <- llm.Event{Type: llm.EventTextEnd}
				inText = false
			}

			// Finalise all tool calls.
			for _, ts := range toolByIndex {
				var input any
				if len(ts.args) > 0 {
					_ = json.Unmarshal(ts.args, &input)
				}
				out <- llm.Event{
					Type:       llm.EventToolCall,
					ToolCallID: ts.id,
					ToolName:   ts.name,
					Input:      input,
				}
			}
			toolByIndex = map[int64]*toolState{}

			// Defer StepFinish: usage may arrive in the next chunk.
			ev := llm.Event{
				Type:         llm.EventStepFinish,
				Usage:        usage, // may be zero; updated when usage chunk arrives
				FinishReason: mapFinishReason(string(choice.FinishReason)),
			}
			pendingFinish = &ev
		}

		select {
		case <-ctx.Done():
			out <- llm.Event{Type: llm.EventError, Err: ctx.Err()}
			return
		default:
		}
	}

	if err := stream.Err(); err != nil {
		out <- llm.Event{Type: llm.EventError, Err: classifyError(providerID, err)}
		return
	}

	// Flush any deferred StepFinish (usage chunk may have been last or absent).
	flushPending()

	out <- llm.Event{
		Type:         llm.EventRequestFinish,
		Usage:        usage,
		FinishReason: llm.FinishReasonStop,
	}
}

func mapFinishReason(reason string) llm.FinishReason {
	switch reason {
	case "stop":
		return llm.FinishReasonStop
	case "tool_calls", "function_call":
		return llm.FinishReasonToolCalls
	case "length", "max_tokens":
		return llm.FinishReasonLength
	case "content_filter":
		return llm.FinishReasonError
	}
	return llm.FinishReasonOther
}

func classifyError(providerID string, err error) error {
	if err == nil {
		return nil
	}
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		headers := http.Header{}
		if apiErr.Response != nil {
			headers = apiErr.Response.Header
		}
		return llm.ClassifyHTTPError(providerID, int(apiErr.StatusCode), apiErr.Error(), headers)
	}
	return &llm.LLMError{
		Kind:        llm.ErrTransport,
		Message:     err.Error(),
		IsRetryable: false,
	}
}
