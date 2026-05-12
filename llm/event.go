package llm

// EventType discriminators for LLMEvent.
// Aligned with packages/llm/src/schema/events.ts.
type EventType string

const (
	EventRequestStart   EventType = "request-start"
	EventStepStart      EventType = "step-start"
	EventTextStart      EventType = "text-start"
	EventTextDelta      EventType = "text-delta"
	EventTextEnd        EventType = "text-end"
	EventReasoningDelta EventType = "reasoning-delta"
	EventReasoningEnd   EventType = "reasoning-end"
	EventToolInputStart EventType = "tool-input-start"
	EventToolInputDelta EventType = "tool-input-delta"
	EventToolCall       EventType = "tool-call"       // complete tool call with parsed input
	EventToolResult     EventType = "tool-result"     // tool execution result
	EventToolError      EventType = "tool-error"      // tool execution failed
	EventStepFinish     EventType = "step-finish"
	EventRequestFinish  EventType = "request-finish"
	EventError          EventType = "error"
)

// FinishReason indicates why the LLM stopped generating.
type FinishReason string

const (
	FinishReasonStop       FinishReason = "stop"
	FinishReasonToolCalls  FinishReason = "tool-calls"
	FinishReasonLength     FinishReason = "length"
	FinishReasonError      FinishReason = "error"
	FinishReasonOther      FinishReason = "other"
)

// Event is the canonical streaming event emitted during an LLM response.
// It is a flat struct; valid fields depend on Type.
type Event struct {
	Type EventType

	// EventTextDelta, EventReasoningDelta
	Text string

	// EventReasoningEnd: Anthropic thinking block signature, must be echoed back
	Signature string

	// EventToolInputStart, EventToolInputDelta, EventToolCall, EventToolResult, EventToolError
	ToolCallID string
	ToolName   string

	// EventToolCall: parsed input arguments
	Input any

	// EventToolResult: the tool output text
	Output string

	// EventStepFinish, EventRequestFinish
	Usage        TokenUsage
	FinishReason FinishReason

	// EventError
	Err error
}

// IsText returns true for text delta events.
func (e Event) IsText() bool { return e.Type == EventTextDelta }

// IsToolCall returns true for a complete tool call event.
func (e Event) IsToolCall() bool { return e.Type == EventToolCall }

// IsToolResult returns true for a tool result event.
func (e Event) IsToolResult() bool { return e.Type == EventToolResult }

// IsFinish returns true for step or request finish events.
func (e Event) IsFinish() bool {
	return e.Type == EventStepFinish || e.Type == EventRequestFinish
}

// IsError returns true for error events.
func (e Event) IsError() bool { return e.Type == EventError }
