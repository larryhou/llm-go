// Package anthropic implements the llm.Provider interface using the official
// anthropic-sdk-go, which natively supports option.WithBaseURL for custom endpoints.
package anthropic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"

	"github.com/larryhou/llm-go/auth"
	"github.com/larryhou/llm-go/config"
	"github.com/larryhou/llm-go/llm"
)

const ProviderID = "anthropic"

// EnvVars lists the environment variables checked for the Anthropic API key.
var EnvVars = []string{"ANTHROPIC_API_KEY"}

// Provider implements llm.Provider for Anthropic.
type Provider struct {
	client anthropic.Client
}

// New creates an Anthropic provider.
// baseURL is optional (empty = use default https://api.anthropic.com/).
func New(apiKey, baseURL string, extraHeaders map[string]string) *Provider {
	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	for k, v := range extraHeaders {
		opts = append(opts, option.WithHeader(k, v))
	}
	return &Provider{client: anthropic.NewClient(opts...)}
}

// NewFromConfig builds a Provider from config + auth store.
func NewFromConfig(cfg *config.ProviderInfo, authStore *auth.Store) (*Provider, error) {
	apiKey, _ := auth.ResolveKey(ProviderID, EnvVars, authStore)
	baseURL := ""

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
	}

	if apiKey == "" {
		return nil, fmt.Errorf("anthropic: no API key found (set ANTHROPIC_API_KEY or configure in opencode.json)")
	}

	extraHeaders := map[string]string{
		"anthropic-beta": "interleaved-thinking-2025-05-14",
	}
	return New(apiKey, baseURL, extraHeaders), nil
}

func (p *Provider) ID() string { return ProviderID }

// Stream implements llm.Provider.
func (p *Provider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	params, err := buildParams(req)
	if err != nil {
		return nil, err
	}

	stream := p.client.Messages.NewStreaming(ctx, params)
	ch := make(chan llm.Event, 32)
	go func() {
		defer close(ch)
		runStream(ctx, stream, ch)
	}()
	return ch, nil
}

// buildParams converts a canonical llm.Request into anthropic.MessageNewParams.
func buildParams(req llm.Request) (anthropic.MessageNewParams, error) {
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model.APIID),
		MaxTokens: int64(effectiveMaxTokens(req)),
	}

	// System prompt
	if len(req.System) > 0 {
		system := ""
		for _, s := range req.System {
			system += s + "\n"
		}
		params.System = []anthropic.TextBlockParam{{Text: system}}
	}

	// Temperature
	if req.Options.Temperature != nil {
		params.Temperature = anthropic.Float(*req.Options.Temperature)
	}

	// Tool choice
	switch req.ToolChoice {
	case llm.ToolChoiceNone:
		params.ToolChoice = anthropic.ToolChoiceUnionParam{
			OfNone: &anthropic.ToolChoiceNoneParam{Type: "none"},
		}
	case llm.ToolChoiceRequired:
		params.ToolChoice = anthropic.ToolChoiceUnionParam{
			OfAny: &anthropic.ToolChoiceAnyParam{Type: "any"},
		}
	}

	// Tools
	if len(req.Tools) > 0 {
		tools := make([]anthropic.ToolUnionParam, 0, len(req.Tools))
		for _, t := range req.Tools {
			schemaBytes, err := json.Marshal(t.InputSchema)
			if err != nil {
				return params, fmt.Errorf("tool %q: marshal schema: %w", t.Name, err)
			}
			var schemaParam anthropic.ToolInputSchemaParam
			if err := json.Unmarshal(schemaBytes, &schemaParam); err != nil {
				return params, fmt.Errorf("tool %q: unmarshal schema param: %w", t.Name, err)
			}
			tools = append(tools, anthropic.ToolUnionParam{
				OfTool: &anthropic.ToolParam{
					Name:        t.Name,
					Description: anthropic.String(t.Description),
					InputSchema: schemaParam,
				},
			})
		}
		params.Tools = tools
	}

	// Messages
	msgs, err := convertMessages(req.Messages)
	if err != nil {
		return params, err
	}
	params.Messages = msgs

	return params, nil
}

func effectiveMaxTokens(req llm.Request) int {
	if req.Options.MaxTokens > 0 {
		return req.Options.MaxTokens
	}
	return llm.MaxOutputTokens(req.Model)
}

// convertMessages converts canonical messages to Anthropic's format.
func convertMessages(msgs []llm.Message) ([]anthropic.MessageParam, error) {
	var out []anthropic.MessageParam
	for _, m := range msgs {
		switch m.Role {
		case llm.RoleUser:
			blocks, err := userBlocks(m.Content)
			if err != nil {
				return nil, err
			}
			out = append(out, anthropic.NewUserMessage(blocks...))
		case llm.RoleAssistant:
			blocks, err := assistantBlocks(m.Content)
			if err != nil {
				return nil, err
			}
			out = append(out, anthropic.NewAssistantMessage(blocks...))
		case llm.RoleTool:
			// Tool results are injected as a user message in Anthropic's protocol
			blocks, err := toolResultBlocks(m.Content)
			if err != nil {
				return nil, err
			}
			if len(blocks) > 0 {
				out = append(out, anthropic.NewUserMessage(blocks...))
			}
		}
	}
	return out, nil
}

func userBlocks(parts []llm.ContentPart) ([]anthropic.ContentBlockParamUnion, error) {
	var blocks []anthropic.ContentBlockParamUnion
	for _, p := range parts {
		switch p.Type {
		case llm.PartTypeText:
			if p.Text != "" {
				blocks = append(blocks, anthropic.NewTextBlock(p.Text))
			}
		case llm.PartTypeImage:
			if len(p.Data) > 0 {
				encoded := base64.StdEncoding.EncodeToString(p.Data)
				blocks = append(blocks, anthropic.NewImageBlockBase64(p.MediaType, encoded))
			} else if p.URL != "" {
				blocks = append(blocks, anthropic.NewImageBlock(anthropic.URLImageSourceParam{URL: p.URL}))
			}
		}
	}
	return blocks, nil
}

func toolResultBlocks(parts []llm.ContentPart) ([]anthropic.ContentBlockParamUnion, error) {
	var blocks []anthropic.ContentBlockParamUnion
	for _, p := range parts {
		if p.Type != llm.PartTypeToolResult {
			continue
		}
		resultText := ""
		isError := false
		if p.Result != nil {
			isError = p.Result.Type == llm.ToolResultTypeError
			switch v := p.Result.Value.(type) {
			case string:
				resultText = v
			default:
				b, _ := json.Marshal(p.Result.Value)
				resultText = string(b)
			}
		}
		blocks = append(blocks, anthropic.NewToolResultBlock(p.ToolCallID, resultText, isError))
	}
	return blocks, nil
}

func assistantBlocks(parts []llm.ContentPart) ([]anthropic.ContentBlockParamUnion, error) {
	var blocks []anthropic.ContentBlockParamUnion
	for _, p := range parts {
		switch p.Type {
		case llm.PartTypeText:
			if p.Text != "" {
				blocks = append(blocks, anthropic.NewTextBlock(p.Text))
			}
		case llm.PartTypeToolCall:
			blocks = append(blocks, anthropic.NewToolUseBlock(p.ToolCallID, p.Input, p.ToolName))
		case llm.PartTypeReasoning:
			if p.Text != "" {
				blocks = append(blocks, anthropic.NewTextBlock(p.Text))
			}
		}
	}
	return blocks, nil
}

// runStream processes the Anthropic streaming response and emits canonical events.
func runStream(ctx context.Context, stream *ssestream.Stream[anthropic.MessageStreamEventUnion], out chan<- llm.Event) {
	defer stream.Close()

	out <- llm.Event{Type: llm.EventRequestStart}

	var usage llm.TokenUsage
	currentToolID := ""
	currentToolName := ""
	var toolInputBuf []byte
	inText := false

	for stream.Next() {
		raw := stream.Current()
		switch ev := raw.AsAny().(type) {
		case anthropic.MessageStartEvent:
			out <- llm.Event{Type: llm.EventStepStart}
			usage.Input = int(ev.Message.Usage.InputTokens)
			usage.CacheRead = int(ev.Message.Usage.CacheReadInputTokens)
			usage.CacheWrite = int(ev.Message.Usage.CacheCreationInputTokens)

		case anthropic.ContentBlockStartEvent:
			switch block := ev.ContentBlock.AsAny().(type) {
			case anthropic.TextBlock:
				_ = block
				inText = true
				out <- llm.Event{Type: llm.EventTextStart}
			case anthropic.ToolUseBlock:
				currentToolID = block.ID
				currentToolName = block.Name
				toolInputBuf = nil
				out <- llm.Event{
					Type:       llm.EventToolInputStart,
					ToolCallID: currentToolID,
					ToolName:   currentToolName,
				}
			}

		case anthropic.ContentBlockDeltaEvent:
			switch delta := ev.Delta.AsAny().(type) {
			case anthropic.TextDelta:
				out <- llm.Event{Type: llm.EventTextDelta, Text: delta.Text}
			case anthropic.InputJSONDelta:
				toolInputBuf = append(toolInputBuf, delta.PartialJSON...)
				out <- llm.Event{
					Type:       llm.EventToolInputDelta,
					ToolCallID: currentToolID,
					ToolName:   currentToolName,
					Text:       delta.PartialJSON,
				}
			case anthropic.ThinkingDelta:
				out <- llm.Event{Type: llm.EventReasoningDelta, Text: delta.Thinking}
			}

		case anthropic.ContentBlockStopEvent:
			_ = ev
			if inText {
				out <- llm.Event{Type: llm.EventTextEnd}
				inText = false
			} else if currentToolID != "" {
				var input any
				if len(toolInputBuf) > 0 {
					_ = json.Unmarshal(toolInputBuf, &input)
				}
				out <- llm.Event{
					Type:       llm.EventToolCall,
					ToolCallID: currentToolID,
					ToolName:   currentToolName,
					Input:      input,
				}
				currentToolID = ""
				currentToolName = ""
				toolInputBuf = nil
			}

		case anthropic.MessageDeltaEvent:
			usage.Output += int(ev.Usage.OutputTokens)
			out <- llm.Event{
				Type:         llm.EventStepFinish,
				Usage:        usage,
				FinishReason: mapStopReason(string(ev.Delta.StopReason)),
			}

		case anthropic.MessageStopEvent:
			_ = ev
			// stream done
		}

		select {
		case <-ctx.Done():
			out <- llm.Event{Type: llm.EventError, Err: ctx.Err()}
			return
		default:
		}
	}

	if err := stream.Err(); err != nil {
		out <- llm.Event{Type: llm.EventError, Err: classifyError(err)}
		return
	}

	out <- llm.Event{
		Type:         llm.EventRequestFinish,
		Usage:        usage,
		FinishReason: llm.FinishReasonStop,
	}
}

func mapStopReason(reason string) llm.FinishReason {
	switch reason {
	case "end_turn":
		return llm.FinishReasonStop
	case "tool_use":
		return llm.FinishReasonToolCalls
	case "max_tokens":
		return llm.FinishReasonLength
	}
	return llm.FinishReasonOther
}

func classifyError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		headers := http.Header{}
		if apiErr.Response != nil {
			headers = apiErr.Response.Header
		}
		return llm.ClassifyHTTPError(ProviderID, apiErr.StatusCode, apiErr.Error(), headers)
	}
	return &llm.LLMError{
		Kind:        llm.ErrTransport,
		Message:     err.Error(),
		IsRetryable: false,
	}
}
