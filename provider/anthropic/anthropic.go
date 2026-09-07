// Package anthropic implements the llm.Provider interface using the official
// anthropic-sdk-go, which natively supports option.WithBaseURL for custom endpoints.
package anthropic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

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

// DefaultTimeout is the default total request timeout (5 minutes), matching
// opencode's DEFAULT_BUFFER / headerTimeout defaults.
const DefaultTimeout = 5 * time.Minute

// DefaultChunkTimeout is the default per-chunk idle timeout (5 minutes).
// If no SSE chunk arrives within this window the stream is aborted.
const DefaultChunkTimeout = 5 * time.Minute

// Provider implements llm.Provider for Anthropic.
type Provider struct {
	client       anthropic.Client
	chunkTimeout time.Duration // 0 = disabled
}

// New creates an Anthropic provider.
// baseURL is optional (empty = use default https://api.anthropic.com/).
// timeout is the total request timeout (0 = use DefaultTimeout, -1 = disabled).
// chunkTimeout is the per-chunk idle timeout (0 = use DefaultChunkTimeout, -1 = disabled).
func New(apiKey, baseURL string, extraHeaders map[string]string, timeout, chunkTimeout time.Duration) *Provider {
	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	for k, v := range extraHeaders {
		opts = append(opts, option.WithHeader(k, v))
	}
	// Apply total request timeout. -1 disables it; 0 uses the default.
	t := timeout
	if t == 0 {
		t = DefaultTimeout
	}
	if t > 0 {
		opts = append(opts, option.WithRequestTimeout(t))
	}
	ct := chunkTimeout
	if ct == 0 {
		ct = DefaultChunkTimeout
	}
	if ct < 0 {
		ct = 0
	}
	return &Provider{client: anthropic.NewClient(opts...), chunkTimeout: ct}
}

// NewFromConfig builds a Provider from config + auth store.
func NewFromConfig(cfg *config.ProviderInfo, authStore *auth.Store) (*Provider, error) {
	apiKey, _ := auth.ResolveKey(ProviderID, EnvVars, authStore)
	baseURL := ""
	var timeout, chunkTimeout time.Duration

	if cfg != nil {
		if cfg.Options != nil {
			if cfg.Options.APIKey != "" {
				apiKey = cfg.Options.APIKey
			}
			if cfg.Options.BaseURL != "" {
				baseURL = cfg.Options.BaseURL
			}
			if cfg.Options.Timeout != nil {
				switch *cfg.Options.Timeout {
				case -1:
					timeout = -1 // disabled
				case 0:
					// use default
				default:
					timeout = time.Duration(*cfg.Options.Timeout) * time.Millisecond
				}
			}
			if cfg.Options.ChunkTimeout > 0 {
				chunkTimeout = time.Duration(cfg.Options.ChunkTimeout) * time.Millisecond
			} else if cfg.Options.ChunkTimeout < 0 {
				chunkTimeout = -1 // disabled
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
	return New(apiKey, baseURL, extraHeaders, timeout, chunkTimeout), nil
}

func (p *Provider) ID() string { return ProviderID }

// Factory is a provider.Factory-compatible function for this package.
// Register it with a provider.Registry via registry.RegisterFactory(ProviderID, anthropic.Factory).
var Factory = func(cfg *config.ProviderInfo, authStore *auth.Store) (llm.Provider, error) {
	return NewFromConfig(cfg, authStore)
}

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
		runStream(ctx, stream, ch, p.chunkTimeout)
	}()
	return ch, nil
}

// buildParams converts a canonical llm.Request into anthropic.MessageNewParams.
func buildParams(req llm.Request) (anthropic.MessageNewParams, error) {
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model.APIID),
		MaxTokens: int64(effectiveMaxTokens(req)),
	}

	// System prompt — keep as separate blocks to preserve per-entry cache granularity.
	if len(req.System) > 0 {
		sysBlocks := make([]anthropic.TextBlockParam, 0, len(req.System))
		for _, s := range req.System {
			if s != "" {
				sysBlocks = append(sysBlocks, anthropic.TextBlockParam{Text: s})
			}
		}
		params.System = sysBlocks
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
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfThinking: &anthropic.ThinkingBlockParam{
						Type:      "thinking",
						Thinking:  p.Text,
						Signature: p.Signature,
					},
				})
			}
		}
	}
	return blocks, nil
}

// runStream processes the Anthropic streaming response and emits canonical events.
// chunkTimeout is the maximum idle time between consecutive SSE chunks; 0 disables it.
func runStream(ctx context.Context, stream *ssestream.Stream[anthropic.MessageStreamEventUnion], out chan<- llm.Event, chunkTimeout time.Duration) {
	defer stream.Close()

	out <- llm.Event{Type: llm.EventRequestStart}

	var usage llm.TokenUsage
	currentToolID := ""
	currentToolName := ""
	var toolInputBuf []byte
	inText := false
	inReasoning := false
	reasoningSignature := ""

	// Per-chunk idle timeout: if no SSE chunk arrives within chunkTimeout the
	// stream is force-closed, causing stream.Next() to return false and
	// stream.Err() to return a non-nil error that is reported as EventError.
	// This mirrors opencode's wrapSSE / setTimeout-per-pull mechanism.
	var idleTimer *time.Timer
	if chunkTimeout > 0 {
		idleTimer = time.AfterFunc(chunkTimeout, func() { stream.Close() })
		defer idleTimer.Stop()
	}

	for stream.Next() {
		if idleTimer != nil {
			idleTimer.Reset(chunkTimeout)
		}
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
			case anthropic.ThinkingBlock:
				reasoningSignature = block.Signature
				inReasoning = true
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
			} else if inReasoning {
				out <- llm.Event{Type: llm.EventReasoningEnd, Signature: reasoningSignature}
				inReasoning = false
				reasoningSignature = ""
			} else if currentToolID != "" {
				var input any
				if len(toolInputBuf) > 0 {
					if err := json.Unmarshal(toolInputBuf, &input); err != nil {
						out <- llm.Event{
							Type:       llm.EventToolError,
							ToolCallID: currentToolID,
							ToolName:   currentToolName,
							Err:        fmt.Errorf("tool %q: unmarshal input: %w", currentToolName, err),
						}
						currentToolID = ""
						currentToolName = ""
						toolInputBuf = nil
						continue
					}
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
	case "end_turn", "stop_sequence":
		return llm.FinishReasonStop
	case "tool_use":
		return llm.FinishReasonToolCalls
	case "max_tokens", "model_context_window_exceeded":
		// model_context_window_exceeded added in anthropic-sdk-go v1.60.0:
		// the model's context window was exceeded during generation.
		return llm.FinishReasonLength
	case "pause_turn":
		// pause_turn is an Anthropic extension for adaptive thinking pauses;
		// treat like stop so the loop continues normally.
		return llm.FinishReasonStop
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
		IsRetryable: isRetryableTransportError(err),
	}
}

// isRetryableTransportError returns true for transient network errors that are
// worth retrying (connection reset, EOF, timeouts) and false for permanent
// failures (TLS certificate errors, DNS resolution failures, etc.).
func isRetryableTransportError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// *json.SyntaxError with "unexpected end of JSON input" is produced by the
	// SSE stream decoder when the connection is dropped mid-frame. It is
	// semantically equivalent to io.ErrUnexpectedEOF and should be retried.
	var jsonSyn *json.SyntaxError
	if errors.As(err, &jsonSyn) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		// Check OpError before generic net.Error so that Op field takes
		// precedence. "dial" means endpoint unreachable (not transient);
		// "read"/"write" (ECONNRESET, EPIPE, mid-stream timeout) are retryable.
		switch opErr.Op {
		case "read", "write":
			return true
		}
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}
