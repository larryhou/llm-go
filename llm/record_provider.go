// Package llm provides RecordProvider — a stateless transparent wrapper
// that appends one JSON record per Stream() call to a recording file.
//
// # Format
//
// The recording file is newline-delimited JSON (ndjson). Each line is one
// Record — the full request plus the filtered response events:
//
//	{"request":{...},"events":[{"type":"request-start"},{"type":"step-start"},{"type":"text-delta","text":"..."},{"type":"step-finish","finish_reason":"stop","usage":{"input":1234,"output":56}},{"type":"request-finish","finish_reason":"stop"}]}
//	{"request":{...},"events":[...]}
//
// One file covers the entire session lifetime. The replay provider reads the
// file and consumes one line per Stream() call, in order.
//
// Only semantically significant events are stored. Low-level lifecycle events
// (TextStart/End, ToolInputStart/Delta) are synthesised by the replay provider.
// EventStepFinish.Usage carries real token counts — critical for compaction.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// ── Types ─────────────────────────────────────────────────────────────────────

// RecordEvent is a JSON-serialisable snapshot of one llm.Event.
type RecordEvent struct {
	Type         EventType    `json:"type"`
	Text         string       `json:"text,omitempty"`
	Signature    string       `json:"signature,omitempty"`
	ToolCallID   string       `json:"tool_call_id,omitempty"`
	ToolName     string       `json:"tool_name,omitempty"`
	Input        any          `json:"input,omitempty"`
	FinishReason FinishReason `json:"finish_reason,omitempty"`
	Usage        *TokenUsage  `json:"usage,omitempty"`
	Error        string       `json:"error,omitempty"`
}

// Record is one line in the recording file: the request sent to the LLM
// and the significant response events.
type Record struct {
	Request Request         `json:"request"`
	Events  []RecordEvent `json:"events"`
}

// ── RecordProvider ─────────────────────────────────────────────────────────

// RecordProvider wraps a real Provider and appends one Record per
// Stream() call to path. It is fully transparent — no lifecycle methods, no
// turn-boundary logic. The file can be replayed with DebugProvider.
type RecordProvider struct {
	inner Provider
	mu    sync.Mutex
	f     *os.File
	enc   *json.Encoder
}

// NewRecordProvider wraps inner and records every Stream() call to path.
// The file is created (or truncated) immediately; an error is returned if it
// cannot be opened.
func NewRecordProvider(inner Provider, path string) (*RecordProvider, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("recording: create %s: %w", path, err)
	}
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	return &RecordProvider{inner: inner, f: f, enc: enc}, nil
}

// ID satisfies llm.Provider.
func (p *RecordProvider) ID() string { return p.inner.ID() }

// Close flushes and closes the recording file.
func (p *RecordProvider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.f.Close()
}

// Stream satisfies llm.Provider. It transparently proxies to the inner
// provider while recording the request and all significant response events.
// One Record is appended to the file when the stream closes.
func (p *RecordProvider) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	inner, err := p.inner.Stream(ctx, req)
	if err != nil {
		return nil, err
	}

	out := make(chan Event, 128)
	go func() {
		defer close(out)

		step := Record{
			Request: req,
			Events:  []RecordEvent{{Type: EventRequestStart}},
		}

		for ev := range inner {
			select {
			case out <- ev:
			case <-ctx.Done():
				for range inner {
				}
				return
			}
			recordEvent(&step, ev)
		}

		p.mu.Lock()
		_ = p.enc.Encode(step)
		p.mu.Unlock()
	}()

	return out, nil
}

// recordEvent appends significant events to step, accumulating text/reasoning
// deltas into a single record per LLM step.
func recordEvent(step *Record, ev Event) {
	switch ev.Type {
	case EventStepStart:
		step.Events = append(step.Events, RecordEvent{Type: EventStepStart})

	case EventTextDelta:
		accumulateDelta(step, EventTextDelta, ev.Text)

	case EventReasoningDelta:
		accumulateDelta(step, EventReasoningDelta, ev.Text)

	case EventReasoningEnd:
		step.Events = append(step.Events, RecordEvent{Type: EventReasoningEnd, Signature: ev.Signature})

	case EventToolCall:
		step.Events = append(step.Events, RecordEvent{
			Type:       EventToolCall,
			ToolName:   ev.ToolName,
			ToolCallID: ev.ToolCallID,
			Input:      ev.Input,
		})

	case EventStepFinish:
		u := ev.Usage
		step.Events = append(step.Events, RecordEvent{
			Type:         EventStepFinish,
			FinishReason: ev.FinishReason,
			Usage:        &u,
		})

	case EventRequestFinish:
		step.Events = append(step.Events, RecordEvent{
			Type:         EventRequestFinish,
			FinishReason: ev.FinishReason,
		})

	case EventError:
		errStr := ""
		if ev.Err != nil {
			errStr = ev.Err.Error()
		}
		step.Events = append(step.Events, RecordEvent{Type: EventError, Error: errStr})
	}
}

// accumulateDelta merges a text delta into the last matching RecordEvent within
// the current LLM step, or appends a new one if none exists yet.
func accumulateDelta(step *Record, t EventType, text string) {
	for i := len(step.Events) - 1; i >= 0; i-- {
		if step.Events[i].Type == EventStepStart {
			break
		}
		if step.Events[i].Type == t {
			step.Events[i].Text += text
			return
		}
	}
	step.Events = append(step.Events, RecordEvent{Type: t, Text: text})
}
