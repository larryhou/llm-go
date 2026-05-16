// Package llm provides ReplayProvider — a replay provider that drives a session
// from a recording file produced by RecordingProvider.
//
// Each Stream() call consumes the next Record from the file and synthesises
// the recorded events as a live event stream, including real token usage.
// The provider is stateless beyond the read cursor; it knows nothing about
// turns or sessions.
package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
)

// ReplayProvider replays a sequence of Records as live event streams.
// Each call to Stream() consumes the next Record in order.
// Construct with NewReplayProvider (from file) or NewReplayProviderFromRecords (from slice).
type ReplayProvider struct {
	steps []Record
	idx   atomic.Int64
}

// NewReplayProvider loads all Records from the ndjson recording at path.
func NewReplayProvider(path string) (*ReplayProvider, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("replay provider: open %s: %w", path, err)
	}
	defer f.Close()

	var steps []Record
	dec := json.NewDecoder(bufio.NewReader(f))
	for {
		var s Record
		if err := dec.Decode(&s); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("replay provider: decode %s: %w", path, err)
		}
		steps = append(steps, s)
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("replay provider: %s has no steps", path)
	}
	return &ReplayProvider{steps: steps}, nil
}

// NewReplayProviderFromRecords creates a ReplayProvider from an in-memory slice.
func NewReplayProviderFromRecords(records []Record) *ReplayProvider {
	return &ReplayProvider{steps: records}
}

// ID satisfies llm.Provider.
func (p *ReplayProvider) ID() string { return "replay" }

// Len returns the total number of recorded steps.
func (p *ReplayProvider) Len() int { return len(p.steps) }

// Records returns a copy of the underlying Record slice.
// Used by cmd/replay to group steps into turns without re-parsing the file.
func (p *ReplayProvider) Records() []Record { return p.steps }

// Stream satisfies llm.Provider. It replays the next recorded Record as a
// live event stream, synthesising low-level lifecycle events from the stored
// higher-level ones so that session.Processor behaves identically to a real run.
func (p *ReplayProvider) Stream(_ context.Context, _ Request) (<-chan Event, error) {
	i := int(p.idx.Add(1) - 1)
	if i >= len(p.steps) {
		return nil, fmt.Errorf("replay provider: no more steps (have %d, requested %d)", len(p.steps), i)
	}
	step := p.steps[i]

	ch := make(chan Event, 256)
	go func() {
		defer close(ch)
		for _, re := range step.Events {
			for _, ev := range expand(re) {
				ch <- ev
			}
		}
	}()
	return ch, nil
}

// expand converts one RecordEvent back into the sequence of live Events that
// session.Processor expects. Low-level lifecycle events that were omitted
// during recording are re-synthesised here.
func expand(re RecordEvent) []Event {
	switch re.Type {
	case EventRequestStart:
		return []Event{{Type: EventRequestStart}}

	case EventStepStart:
		return []Event{{Type: EventStepStart}}

	case EventTextDelta:
		return []Event{
			{Type: EventTextStart},
			{Type: EventTextDelta, Text: re.Text},
			{Type: EventTextEnd},
		}

	case EventReasoningDelta:
		return []Event{
			{Type: EventReasoningDelta, Text: re.Text},
			{Type: EventReasoningEnd, Signature: re.Signature},
		}

	case EventReasoningEnd:
		// Already emitted by EventReasoningDelta expansion; skip standalone.
		return nil

	case EventToolCall:
		return []Event{
			{Type: EventToolInputStart, ToolName: re.ToolName, ToolCallID: re.ToolCallID},
			{Type: EventToolCall, ToolName: re.ToolName, ToolCallID: re.ToolCallID, Input: re.Input},
		}

	case EventStepFinish:
		usage := TokenUsage{}
		if re.Usage != nil {
			usage = *re.Usage
		}
		return []Event{{Type: EventStepFinish, FinishReason: re.FinishReason, Usage: usage}}

	case EventRequestFinish:
		return []Event{{Type: EventRequestFinish, FinishReason: re.FinishReason}}

	case EventError:
		return []Event{{Type: EventError, Err: errors.New(re.Error)}}
	}
	return nil
}
