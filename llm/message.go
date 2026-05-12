// Package llm defines canonical message types for LLM conversations.
// Aligned with packages/llm/src/schema/messages.ts.
package llm

// Role constants for message roles.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
	RoleSystem    = "system"
)

// PartType discriminators.
const (
	PartTypeText       = "text"
	PartTypeToolCall   = "tool-call"
	PartTypeToolResult = "tool-result"
	PartTypeReasoning  = "reasoning"
	PartTypeImage      = "image"
	PartTypeFile       = "file"
)

// ToolChoice constants.
const (
	ToolChoiceAuto     = "auto"
	ToolChoiceNone     = "none"
	ToolChoiceRequired = "required"
)

// ToolResultType discriminators for ToolResult.Type.
const (
	ToolResultTypeJSON  = "json"
	ToolResultTypeText  = "text"
	ToolResultTypeError = "error"
)

// Message is a canonical LLM conversation message.
type Message struct {
	ID      string        `json:"id,omitempty"`
	Role    string        `json:"role"`
	Content []ContentPart `json:"content"`
}

// ContentPart is a discriminated union of all content part types.
type ContentPart struct {
	Type string `json:"type"`

	// PartTypeText / PartTypeReasoning
	Text string `json:"text,omitempty"`

	// PartTypeReasoning: Anthropic thinking block signature, must be echoed back verbatim
	Signature string `json:"signature,omitempty"`

	// PartTypeToolCall
	ToolCallID string `json:"toolCallId,omitempty"`
	ToolName   string `json:"toolName,omitempty"`
	Input      any    `json:"input,omitempty"`

	// PartTypeToolResult
	Result *ToolResult `json:"result,omitempty"`

	// PartTypeImage / PartTypeFile
	MediaType string `json:"mediaType,omitempty"`
	Data      []byte `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// ToolResult holds the result of a tool execution.
type ToolResult struct {
	Type  string `json:"type"`  // json | text | error
	Value any    `json:"value"` // string for text/error, any for json
}

// ToolDefinition is the schema sent to the LLM describing a tool.
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"` // JSON Schema object
}

// Request is the canonical LLM request, provider-agnostic.
type Request struct {
	Model      Model
	System     []string
	Messages   []Message
	Tools      []ToolDefinition
	ToolChoice string // auto | none | required | <tool name>
	Options    GenerationOptions
}

// GenerationOptions controls LLM sampling behaviour.
type GenerationOptions struct {
	MaxTokens   int      `json:"maxTokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"topP,omitempty"`
	TopK        *int     `json:"topK,omitempty"`
}

// TokenUsage tracks token consumption for cost/overflow calculation.
// Aligned with opencode's Assistant["tokens"] type.
type TokenUsage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cache_read"`
	CacheWrite int `json:"cache_write"`
	Total      int `json:"total"` // may be directly reported by provider
}

// Add returns the sum of two TokenUsage values.
func (u TokenUsage) Add(other TokenUsage) TokenUsage {
	return TokenUsage{
		Input:      u.Input + other.Input,
		Output:     u.Output + other.Output,
		CacheRead:  u.CacheRead + other.CacheRead,
		CacheWrite: u.CacheWrite + other.CacheWrite,
		Total:      u.Total + other.Total,
	}
}

// Effective returns the token count to use for overflow checks.
// Prefers Total if non-zero; otherwise sums all components.
// Aligned with overflow.ts isOverflow() count logic.
func (u TokenUsage) Effective() int {
	if u.Total > 0 {
		return u.Total
	}
	return u.Input + u.Output + u.CacheRead + u.CacheWrite
}

// NewTextPart constructs a text ContentPart.
func NewTextPart(text string) ContentPart {
	return ContentPart{Type: PartTypeText, Text: text}
}

// NewReasoningPart constructs a reasoning ContentPart.
func NewReasoningPart(text, signature string) ContentPart {
	return ContentPart{Type: PartTypeReasoning, Text: text, Signature: signature}
}

// NewToolCallPart constructs a tool-call ContentPart.
func NewToolCallPart(id, name string, input any) ContentPart {
	return ContentPart{Type: PartTypeToolCall, ToolCallID: id, ToolName: name, Input: input}
}

// NewToolResultPart constructs a tool-result ContentPart.
func NewToolResultPart(id, name string, result *ToolResult) ContentPart {
	return ContentPart{Type: PartTypeToolResult, ToolCallID: id, ToolName: name, Result: result}
}

// NewTextResult constructs a text ToolResult.
func NewTextResult(text string) *ToolResult {
	return &ToolResult{Type: ToolResultTypeText, Value: text}
}

// NewErrorResult constructs an error ToolResult.
func NewErrorResult(msg string) *ToolResult {
	return &ToolResult{Type: ToolResultTypeError, Value: msg}
}

// NewJSONResult constructs a JSON ToolResult.
func NewJSONResult(val any) *ToolResult {
	return &ToolResult{Type: ToolResultTypeJSON, Value: val}
}

// NewUserMessage constructs a user message with text content.
func NewUserMessage(text string) Message {
	return Message{Role: RoleUser, Content: []ContentPart{NewTextPart(text)}}
}

// NewAssistantMessage constructs an assistant message.
func NewAssistantMessage(parts ...ContentPart) Message {
	return Message{Role: RoleAssistant, Content: parts}
}
