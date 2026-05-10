// Package session implements the LLM conversation orchestration layer.
// Aligned with packages/opencode/src/session/.
package session

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/larryhou/llm-go/config"
	"github.com/larryhou/llm-go/llm"
	"github.com/larryhou/llm-go/store"
	"github.com/larryhou/llm-go/tool"
)

// DoomLoopThreshold is the number of identical tool+args calls before asking
// for permission. Aligned with processor.ts DOOM_LOOP_THRESHOLD = 3.
const DoomLoopThreshold = 3

// ProcessResult indicates the outcome of one LLM streaming turn.
type ProcessResult string

const (
	ProcessContinue ProcessResult = "continue"
	ProcessStop     ProcessResult = "stop"
	ProcessCompact  ProcessResult = "compact"
)

// ProcessInput is the input to one LLM turn.
type ProcessInput struct {
	SessionID string
	Model     llm.Model
	System    []string
	Messages  []llm.Message
	Tools     []tool.Tool
	Provider  llm.Provider
	// SummaryProvider, when set, is used for compaction summary generation
	// instead of Provider. This allows using an unwrapped provider for the
	// compaction call (e.g. without usage-injection middleware).
	SummaryProvider llm.Provider
	Config          *config.Info
}

// Processor handles one LLM streaming turn, managing the lifecycle of
// text parts, reasoning parts, and tool call parts in the store.
// Aligned with packages/opencode/src/session/processor.ts.
type Processor struct {
	store store.Store
}

// NewProcessor creates a Processor backed by the given store.
func NewProcessor(s store.Store) *Processor {
	return &Processor{store: s}
}

// Process runs one LLM streaming turn for an assistant message.
// It creates the assistant message in the store, streams events from the
// provider, executes tool calls, and persists all parts.
// Returns the ProcessResult indicating whether to continue, stop, or compact.
func (p *Processor) Process(ctx context.Context, assistantMsgID string, input ProcessInput) (ProcessResult, error) {
	// Build the LLM request
	req := llm.Request{
		Model:    input.Model,
		System:   input.System,
		Messages: input.Messages,
		Options:  llm.GenerationOptions{},
	}

	// Add tool definitions to request
	for _, t := range input.Tools {
		req.Tools = append(req.Tools, llm.ToolDefinition{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.InputSchema(),
		})
	}

	// Build tool lookup map
	toolMap := make(map[string]tool.Tool, len(input.Tools))
	for _, t := range input.Tools {
		toolMap[t.Name()] = t
	}

	client := llm.NewClient(input.Provider)
	events := client.Stream(ctx, req)

	state := &processorState{
		store:          p.store,
		sessionID:      input.SessionID,
		assistantMsgID: assistantMsgID,
		toolMap:        toolMap,
		cfg:            input.Config,
		model:          input.Model,
		// track last N tool calls for doom-loop detection
		recentCalls:    make([]recentCall, 0, DoomLoopThreshold),
	}

	result := ProcessContinue
	for ev := range events {
		r, err := state.handleEvent(ctx, ev)
		if err != nil {
			state.cleanup(ctx)
			return ProcessStop, err
		}
		if r != ProcessContinue {
			result = r
			// drain remaining events
			for range events {
			}
			break
		}
	}

	state.cleanup(ctx)
	return result, nil
}

type recentCall struct {
	toolName string
	inputKey string // JSON of input for comparison
}

type processorState struct {
	store          store.Store
	sessionID      string
	assistantMsgID string
	toolMap        map[string]tool.Tool
	cfg            *config.Info
	model          llm.Model

	// current streaming text part
	currentTextPartID string
	currentTextBuf    string
	currentTextStart  int64

	// current streaming reasoning part
	currentReasoningPartID string
	currentReasoningBuf    string
	currentReasoningStart  int64

	// active tool calls: callID -> partID
	activeToolParts map[string]string

	// doom-loop detection
	recentCalls []recentCall

	// token usage accumulation
	totalUsage llm.TokenUsage

	// flags
	needsCompaction bool
	blocked         bool
}

func (s *processorState) handleEvent(ctx context.Context, ev llm.Event) (ProcessResult, error) {
	switch ev.Type {
	case llm.EventError:
		if llmErr, ok := llm.AsLLMError(ev.Err); ok && llmErr.IsContextOverflow() {
			s.needsCompaction = true
			return ProcessCompact, nil
		}
		return ProcessStop, ev.Err

	case llm.EventTextStart:
		s.currentTextStart = nowMS()
		s.currentTextBuf = ""
		id, err := s.createPart(ctx, store.PartTypeText, &store.TextPartData{
			TimeStart: s.currentTextStart,
		})
		if err != nil {
			return ProcessStop, err
		}
		s.currentTextPartID = id

	case llm.EventTextDelta:
		s.currentTextBuf += ev.Text
		if s.currentTextPartID != "" {
			_ = s.updateTextPart(ctx, s.currentTextPartID, s.currentTextBuf, s.currentTextStart, 0)
		}

	case llm.EventTextEnd:
		if s.currentTextPartID != "" {
			end := nowMS()
			_ = s.updateTextPart(ctx, s.currentTextPartID, s.currentTextBuf, s.currentTextStart, end)
			s.currentTextPartID = ""
			s.currentTextBuf = ""
		}

	case llm.EventReasoningDelta:
		if s.currentReasoningPartID == "" {
			s.currentReasoningStart = nowMS()
			id, err := s.createPart(ctx, store.PartTypeReasoning, &store.ReasoningPartData{
				TimeStart: s.currentReasoningStart,
			})
			if err != nil {
				return ProcessStop, err
			}
			s.currentReasoningPartID = id
		}
		s.currentReasoningBuf += ev.Text
		_ = s.updateReasoningPart(ctx, s.currentReasoningPartID, s.currentReasoningBuf, s.currentReasoningStart, 0)

	case llm.EventToolInputStart:
		if s.activeToolParts == nil {
			s.activeToolParts = make(map[string]string)
		}
		id, err := s.createPart(ctx, store.PartTypeTool, &store.ToolPartData{
			Tool:      ev.ToolName,
			CallID:    ev.ToolCallID,
			Status:    store.ToolStatusPending,
			TimeStart: nowMS(),
		})
		if err != nil {
			return ProcessStop, err
		}
		s.activeToolParts[ev.ToolCallID] = id

	case llm.EventToolCall:
		partID, ok := s.activeToolParts[ev.ToolCallID]
		if !ok {
			break
		}
		inputKey := marshalInput(ev.Input)

		// Doom-loop detection: same tool + same args 3 times in a row
		if s.checkDoomLoop(ev.ToolName, inputKey) {
			// Mark as error and stop
			_ = s.updateToolStatus(ctx, partID, store.ToolStatusError, nil, "Doom loop detected: repeated identical tool call")
			return ProcessStop, fmt.Errorf("doom loop detected for tool %q", ev.ToolName)
		}

		inputMap := toInputMap(ev.Input)
		_ = s.updateToolRunning(ctx, partID, ev.ToolName, ev.ToolCallID, inputMap)

		// Execute the tool
		go s.executeTool(ctx, ev.ToolCallID, ev.ToolName, partID, inputMap)

	case llm.EventToolResult:
		// Tool results are handled asynchronously via executeTool goroutines
		// This event is emitted after our own tool execution completes

	case llm.EventToolError:
		partID, ok := s.activeToolParts[ev.ToolCallID]
		if !ok {
			break
		}
		_ = s.updateToolStatus(ctx, partID, store.ToolStatusError, nil, ev.Text)

	case llm.EventStepFinish:
		s.totalUsage = s.totalUsage.Add(ev.Usage)

		// Persist step-finish part
		_, _ = s.createPart(ctx, store.PartTypeStepFinish, &store.StepFinishData{
			FinishReason: string(ev.FinishReason),
			Usage: store.TokenSummary{
				Input:      ev.Usage.Input,
				Output:     ev.Usage.Output,
				CacheRead:  ev.Usage.CacheRead,
				CacheWrite: ev.Usage.CacheWrite,
			},
		})

		// Check context overflow after each step
		if llm.IsOverflow(ev.Usage, s.model, s.cfg) {
			s.needsCompaction = true
			return ProcessCompact, nil
		}

	case llm.EventRequestFinish:
		// Final event — update assistant message with total usage
		_ = s.finaliseAssistantMessage(ctx)
	}

	return ProcessContinue, nil
}

// executeTool runs a tool and updates the part with the result.
func (s *processorState) executeTool(ctx context.Context, callID, toolName, partID string, input map[string]any) {
	t, ok := s.toolMap[toolName]
	if !ok {
		_ = s.updateToolStatus(ctx, partID, store.ToolStatusError, nil, fmt.Sprintf("tool %q not found", toolName))
		return
	}

	result, err := t.Execute(ctx, input)
	if err != nil {
		if tf, ok := tool.IsToolFailure(err); ok {
			_ = s.updateToolStatus(ctx, partID, store.ToolStatusError, nil, tf.Message)
		} else {
			_ = s.updateToolStatus(ctx, partID, store.ToolStatusError, nil, err.Error())
		}
		return
	}

	_ = s.updateToolCompleted(ctx, partID, toolName, callID, input, result)
}

// checkDoomLoop returns true if the same tool with same args has been called
// DoomLoopThreshold times consecutively.
func (s *processorState) checkDoomLoop(toolName, inputKey string) bool {
	s.recentCalls = append(s.recentCalls, recentCall{toolName: toolName, inputKey: inputKey})
	if len(s.recentCalls) > DoomLoopThreshold {
		s.recentCalls = s.recentCalls[len(s.recentCalls)-DoomLoopThreshold:]
	}
	if len(s.recentCalls) < DoomLoopThreshold {
		return false
	}
	first := s.recentCalls[0]
	for _, c := range s.recentCalls[1:] {
		if c.toolName != first.toolName || c.inputKey != first.inputKey {
			return false
		}
	}
	return true
}

// cleanup finalises any open parts when the stream ends (normally or abnormally).
// Aligned with processor.ts cleanup().
func (s *processorState) cleanup(ctx context.Context) {
	// Finalise open text part
	if s.currentTextPartID != "" {
		_ = s.updateTextPart(ctx, s.currentTextPartID, s.currentTextBuf, s.currentTextStart, nowMS())
		s.currentTextPartID = ""
	}

	// Finalise open reasoning part
	if s.currentReasoningPartID != "" {
		_ = s.updateReasoningPart(ctx, s.currentReasoningPartID, s.currentReasoningBuf, s.currentReasoningStart, nowMS())
		s.currentReasoningPartID = ""
	}

	// Give in-flight tool calls a brief grace period (250ms), then mark as interrupted
	if len(s.activeToolParts) > 0 {
		deadline := time.Now().Add(250 * time.Millisecond)
		for callID, partID := range s.activeToolParts {
			_ = callID
			timeoutCtx, cancel := context.WithDeadline(ctx, deadline)
			p, err := s.store.GetPart(timeoutCtx, partID)
			cancel()
			if err != nil {
				continue
			}
			data, ok := p.Data.(*store.ToolPartData)
			if !ok || data.Status == store.ToolStatusCompleted || data.Status == store.ToolStatusError {
				continue
			}
			_ = s.updateToolStatus(ctx, partID, store.ToolStatusError, map[string]any{"interrupted": true}, "Tool execution aborted")
		}
	}
}

// --- store helpers ---

func (s *processorState) createPart(ctx context.Context, partType string, data any) (string, error) {
	id := newID()
	p := &store.Part{
		ID:        id,
		MessageID: s.assistantMsgID,
		SessionID: s.sessionID,
		Type:      partType,
		Data:      data,
	}
	return id, s.store.CreatePart(ctx, p)
}

func (s *processorState) updateTextPart(ctx context.Context, id, text string, start, end int64) error {
	p, err := s.store.GetPart(ctx, id)
	if err != nil {
		return err
	}
	p.Data = &store.TextPartData{Text: text, TimeStart: start, TimeEnd: end}
	return s.store.UpdatePart(ctx, p)
}

func (s *processorState) updateReasoningPart(ctx context.Context, id, text string, start, end int64) error {
	p, err := s.store.GetPart(ctx, id)
	if err != nil {
		return err
	}
	p.Data = &store.ReasoningPartData{Text: text, TimeStart: start, TimeEnd: end}
	return s.store.UpdatePart(ctx, p)
}

func (s *processorState) updateToolRunning(ctx context.Context, partID, toolName, callID string, input map[string]any) error {
	p, err := s.store.GetPart(ctx, partID)
	if err != nil {
		return err
	}
	existing, _ := p.Data.(*store.ToolPartData)
	start := nowMS()
	if existing != nil && existing.TimeStart > 0 {
		start = existing.TimeStart
	}
	p.Data = &store.ToolPartData{
		Tool:      toolName,
		CallID:    callID,
		Status:    store.ToolStatusRunning,
		Input:     input,
		TimeStart: start,
	}
	return s.store.UpdatePart(ctx, p)
}

func (s *processorState) updateToolCompleted(ctx context.Context, partID, toolName, callID string, input map[string]any, result tool.Result) error {
	p, err := s.store.GetPart(ctx, partID)
	if err != nil {
		return err
	}
	existing, _ := p.Data.(*store.ToolPartData)
	start := nowMS()
	if existing != nil && existing.TimeStart > 0 {
		start = existing.TimeStart
	}
	p.Data = &store.ToolPartData{
		Tool:      toolName,
		CallID:    callID,
		Status:    store.ToolStatusCompleted,
		Input:     input,
		Output:    result.Output,
		Title:     result.Title,
		Metadata:  result.Metadata,
		TimeStart: start,
		TimeEnd:   nowMS(),
	}
	return s.store.UpdatePart(ctx, p)
}

func (s *processorState) updateToolStatus(ctx context.Context, partID, status string, metadata map[string]any, errMsg string) error {
	p, err := s.store.GetPart(ctx, partID)
	if err != nil {
		return err
	}
	existing, _ := p.Data.(*store.ToolPartData)
	data := &store.ToolPartData{Status: status}
	if existing != nil {
		data = existing
		data.Status = status
	}
	if errMsg != "" {
		data.Error = errMsg
	}
	if metadata != nil {
		data.Metadata = metadata
		if v, ok := metadata["interrupted"].(bool); ok {
			data.Interrupted = v
		}
	}
	p.Data = data
	return s.store.UpdatePart(ctx, p)
}

func (s *processorState) finaliseAssistantMessage(ctx context.Context) error {
	m, err := s.store.GetMessage(ctx, s.assistantMsgID)
	if err != nil {
		return err
	}
	m.Tokens = store.TokenSummary{
		Input:      s.totalUsage.Input,
		Output:     s.totalUsage.Output,
		CacheRead:  s.totalUsage.CacheRead,
		CacheWrite: s.totalUsage.CacheWrite,
	}
	return s.store.UpdateMessage(ctx, m)
}

// --- utilities ---

func nowMS() int64 {
	return time.Now().UnixMilli()
}

func marshalInput(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func toInputMap(v any) map[string]any {
	if v == nil {
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	// round-trip through JSON
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// newID generates a simple time-based unique ID.
func newID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
