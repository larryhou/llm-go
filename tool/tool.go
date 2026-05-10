// Package tool defines the Tool interface and ToolResult types.
// Aligned with packages/opencode/src/tool/tool.ts.
package tool

import "context"

// Tool is the interface all tools must implement.
type Tool interface {
	// Name returns the unique tool identifier (snake_case).
	Name() string
	// Description returns the human-readable description sent to the LLM.
	Description() string
	// InputSchema returns the JSON Schema for the tool's input parameters.
	InputSchema() map[string]any
	// Execute runs the tool with the decoded input arguments.
	Execute(ctx context.Context, input map[string]any) (Result, error)
}

// Result is the output of a tool execution.
// Aligned with opencode's ToolResult / ExecuteResult types.
type Result struct {
	Output      string            // text content returned to the LLM
	Title       string            // short display title (optional)
	Truncated   bool              // true if output was truncated
	OutputPath  string            // path to the full output file when truncated
	Metadata    map[string]any    // arbitrary metadata (e.g. exit_code for shell)
	Attachments []Attachment      // binary attachments (images, PDFs)
}

// Attachment represents a binary file attached to a tool result.
type Attachment struct {
	MediaType string
	Data      []byte
	Name      string
}

// ToolFailure is a recoverable tool error.
// When a tool returns this, the LLM receives an error tool-result
// instead of the stream failing entirely.
type ToolFailure struct {
	Message string
}

func (e *ToolFailure) Error() string { return e.Message }

// IsToolFailure returns true if err is a *ToolFailure.
func IsToolFailure(err error) (*ToolFailure, bool) {
	f, ok := err.(*ToolFailure)
	return f, ok
}

// Fail constructs a ToolFailure error.
func Fail(msg string) error { return &ToolFailure{Message: msg} }
