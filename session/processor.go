// Package session implements the LLM conversation orchestration layer.
// Aligned with packages/opencode/src/session/.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sync"
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

	// OnCompact, when non-nil, is called after each successful Compact().
	// Passed through from RunInput so the compaction hook reaches Compactor.Compact().
	OnCompact store.CompactionHook
}

// Processor handles one LLM streaming turn, managing the lifecycle of
// text parts, reasoning parts, and tool call parts in the store.
// Aligned with packages/opencode/src/session/processor.ts.
//
// Each Processor instance is scoped to a single RunLoop call (one user turn).
// Do NOT share a Processor across multiple sessions or RunLoop calls; the
// doom-loop recentCalls window would span unrelated sessions and produce
// false positives.
type Processor struct {
	store store.Store

	// recentCalls is shared across Process calls within one RunLoop so that
	// doom-loop detection spans multiple agentic steps within the same turn.
	// It is intentionally not reset between Process calls.
	recentCallsMu sync.Mutex
	recentCalls   []recentCall
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

	toolCtx, toolCancel := context.WithCancel(ctx)
	state := &processorState{
		store:          p.store,
		sessionID:      input.SessionID,
		assistantMsgID: assistantMsgID,
		toolMap:        toolMap,
		cfg:            input.Config,
		model:          input.Model,
		toolCtx:        toolCtx,
		toolCancel:     toolCancel,
		// recentCalls shared from Processor so doom-loop detection spans steps
		sharedRecentCalls:   &p.recentCalls,
		sharedRecentCallsMu: &p.recentCallsMu,
	}

	result := ProcessContinue
	for ev := range events {
		r, err := state.handleEvent(ctx, ev)
		if err != nil {
			state.cleanup(true) // error path: cancel in-flight tools immediately
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

	// Normal stream end: do NOT cancel toolCtx here — tools launched by the
	// LLM are still executing asynchronously and must be allowed to complete.
	// Only wait for them; cancelTools=false skips toolCancel().
	cancelled := ctx.Err() != nil
	state.cleanup(cancelled)
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
	currentReasoningPartID    string
	currentReasoningBuf       string
	currentReasoningStart     int64
	currentReasoningSignature string

	// active tool calls: callID -> partID
	// protected by toolMu; written by handleEvent (main goroutine) and read
	// by cleanup (main goroutine after stream ends). executeTool goroutines
	// do NOT write to this map — they only read toolMap and call store methods.
	toolMu          sync.Mutex
	activeToolParts map[string]string

	// toolCtx / toolCancel allow cleanup to cancel all in-flight tool goroutines.
	// toolWg tracks when all goroutines have exited.
	toolCtx    context.Context
	toolCancel context.CancelFunc
	toolWg     sync.WaitGroup

	// doom-loop detection: shared with Processor so it spans agentic steps
	sharedRecentCalls   *[]recentCall
	sharedRecentCallsMu *sync.Mutex

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
		_ = s.updateReasoningPart(ctx, s.currentReasoningPartID, s.currentReasoningBuf, s.currentReasoningSignature, s.currentReasoningStart, 0)

	case llm.EventReasoningEnd:
		if s.currentReasoningPartID != "" {
			s.currentReasoningSignature = ev.Signature
			end := nowMS()
			_ = s.updateReasoningPart(ctx, s.currentReasoningPartID, s.currentReasoningBuf, ev.Signature, s.currentReasoningStart, end)
			s.currentReasoningPartID = ""
			s.currentReasoningBuf = ""
			s.currentReasoningSignature = ""
		}

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
		s.toolMu.Lock()
		s.activeToolParts[ev.ToolCallID] = id
		s.toolMu.Unlock()

	case llm.EventToolCall:
		s.toolMu.Lock()
		partID, ok := s.activeToolParts[ev.ToolCallID]
		s.toolMu.Unlock()
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

		// Execute the tool in a goroutine tracked by toolWg.
		// Use toolCtx so cleanup() can cancel all in-flight goroutines.
		s.toolWg.Add(1)
		go s.executeTool(s.toolCtx, ev.ToolCallID, ev.ToolName, partID, inputMap)

	case llm.EventToolResult:
		// Tool results are normally handled asynchronously via executeTool goroutines.
		// Some OpenAI-compatible proxies emit tool_result events directly in the stream;
		// log a warning so unexpected providers are visible rather than silently ignored.
		log.Printf("[session] unexpected EventToolResult for tool %q (id=%s) — provider may be emitting tool results directly in stream",
			ev.ToolName, ev.ToolCallID)

	case llm.EventToolError:
		s.toolMu.Lock()
		partID, ok := s.activeToolParts[ev.ToolCallID]
		s.toolMu.Unlock()
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
	defer s.toolWg.Done()

	t, ok := s.toolMap[toolName]
	if !ok {
		_ = s.updateToolStatus(ctx, partID, store.ToolStatusError, nil, fmt.Sprintf("tool %q not found", toolName))
		return
	}

	result, err := t.Execute(ctx, input)
	// Guard: if cleanup already marked this part as interrupted (Status=error, Interrupted=true),
	// do not overwrite — cleanup's mark takes precedence because the tool goroutine
	// outlived the cleanup window and is no longer considered part of the session.
	if isAlreadyInterrupted(s.store, partID) {
		return
	}
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

// isAlreadyInterrupted returns true if the part has already been marked as
// interrupted by cleanup (Status=error AND Interrupted=true). Used to prevent
// executeTool from overwriting cleanup's authoritative interrupted state.
//
// On GetPart failure (DB error, network blip) we conservatively return true to
// block executeTool from overwriting cleanup's interrupted mark. Cleanup's
// interrupted state takes precedence over any subsequent tool-goroutine write.
func isAlreadyInterrupted(s store.Store, partID string) bool {
	p, err := s.GetPart(context.Background(), partID)
	if err != nil {
		return true // conservative: block overwrite when store is unreachable
	}
	d, ok := store.DataAs[*store.ToolPartData](p)
	if !ok {
		return false
	}
	return d.Status == store.ToolStatusError && d.Interrupted
}

// checkDoomLoop returns true if the same tool with same args has been called
// DoomLoopThreshold times consecutively. Uses the shared cross-step slice.
func (s *processorState) checkDoomLoop(toolName, inputKey string) bool {
	s.sharedRecentCallsMu.Lock()
	defer s.sharedRecentCallsMu.Unlock()

	*s.sharedRecentCalls = append(*s.sharedRecentCalls, recentCall{toolName: toolName, inputKey: inputKey})
	if len(*s.sharedRecentCalls) > DoomLoopThreshold {
		*s.sharedRecentCalls = (*s.sharedRecentCalls)[len(*s.sharedRecentCalls)-DoomLoopThreshold:]
	}
	if len(*s.sharedRecentCalls) < DoomLoopThreshold {
		return false
	}
	first := (*s.sharedRecentCalls)[0]
	for _, c := range (*s.sharedRecentCalls)[1:] {
		if c.toolName != first.toolName || c.inputKey != first.inputKey {
			return false
		}
	}
	return true
}

// cleanup finalises any open parts when the stream ends (normally or abnormally).
// Aligned with processor.ts cleanup().
//
// cancelTools distinguishes the two exit paths:
//
//   - cancelTools=true  (session cancelled or stream error): call toolCancel()
//     immediately so all tool goroutines receive a cancellation signal, then
//     wait up to 250ms for them to exit. Any parts still pending/running after
//     the timeout are marked Status=error/Interrupted ("Tool execution aborted").
//     The isAlreadyInterrupted guard in executeTool prevents a late-finishing
//     goroutine from overwriting that mark.
//
//   - cancelTools=false (normal stream end): tools are legitimate LLM dispatches
//     with their own timeouts (e.g. ShellTool defaults to 120s). Do NOT call
//     toolCancel() — that would abort them prematurely and produce spurious
//     "Tool execution aborted" errors (the bug introduced in e62476b). Instead,
//     wait unconditionally for all goroutines to finish before returning, so
//     the caller always sees consistent store state (all parts completed/error).
//
// IMPORTANT: cleanup always uses context.Background() for store writes, never
// the caller's ctx. When triggered by cancellation the caller's ctx is already
// done; using it for store operations would cause every write to fail silently
// in a real database backend, leaving tool parts permanently in pending state.
func (s *processorState) cleanup(cancelTools bool) {
	ctx := context.Background()

	// Finalise open text part
	if s.currentTextPartID != "" {
		_ = s.updateTextPart(ctx, s.currentTextPartID, s.currentTextBuf, s.currentTextStart, nowMS())
		s.currentTextPartID = ""
	}

	// Finalise open reasoning part
	if s.currentReasoningPartID != "" {
		_ = s.updateReasoningPart(ctx, s.currentReasoningPartID, s.currentReasoningBuf, s.currentReasoningSignature, s.currentReasoningStart, nowMS())
		s.currentReasoningPartID = ""
	}

	// Cancel all in-flight tool goroutines, then wait up to 250ms for them to exit.
	// A single shared deadline avoids the N×250ms problem: total wait is at most
	// 250ms regardless of how many tools are running.
	//
	// Tools receive ctx cancellation and normally exit in microseconds.
	// The 250ms cap is a safety net for tools that are slow to observe
	// cancellation (e.g. blocked on a system call).
	//
	// If the timeout fires, any goroutines still running will eventually call
	// updateToolCompleted/updateToolStatus after cleanup has already marked the
	// part as interrupted. The isAlreadyInterrupted guard (Issue-02 fix) ensures
	// that late writes from timed-out goroutines do not overwrite the interrupted
	// mark. Parts that are still pending/running after process exit are recovered
	// at next startup by RecoverOrphanedTools (Issue-36 fix).
	waitDone := make(chan struct{})
	go func() {
		s.toolWg.Wait()
		close(waitDone)
	}()

	if cancelTools {
		// Cancel path: signal all tool goroutines to stop, then give them a
		// brief grace period (250ms) to exit cleanly before we forcibly mark
		// any stragglers as interrupted.
		// A single shared deadline avoids the N×250ms problem.
		if s.toolCancel != nil {
			s.toolCancel()
		}
		select {
		case <-waitDone:
			// all goroutines finished cleanly within the grace period
		case <-time.After(250 * time.Millisecond):
			// timed out — goroutines received cancel signal but may not have
			// exited yet; mark any still-pending/running parts as interrupted.
		}
	} else {
		// Normal path: tools are legitimate LLM dispatches with their own
		// timeouts (e.g. ShellTool defaults to 120s). Wait for all of them to
		// finish so the caller always sees consistent store state.
		// toolCancel is NOT called here — that would abort them prematurely.
		//
		// However, if the session is cancelled while we are waiting (e.g.
		// h.Cancel() arrives after the stream ended but before all tools
		// completed), we must not block forever. In that case fall through to
		// the interrupted-mark logic below as if cancelTools were true.
		select {
		case <-waitDone:
			// all goroutines finished — fast path, no interruption needed
			if s.toolCancel != nil {
				s.toolCancel() // release context resources
			}
			return
		case <-s.toolCtx.Done():
			// session was cancelled while we were waiting; treat as cancel path
		}
		// fall through to the interrupted-mark logic below
	}

	// Mark any parts that are still pending/running as interrupted.
	// Only reached on the cancel path (cancelTools=true) or when a late
	// cancel arrives during a normal-path wait (the select above fell through).
	s.toolMu.Lock()
	activeSnapshot := make(map[string]string, len(s.activeToolParts))
	for k, v := range s.activeToolParts {
		activeSnapshot[k] = v
	}
	s.toolMu.Unlock()

	for _, partID := range activeSnapshot {
		p, err := s.store.GetPart(ctx, partID)
		if err != nil {
			continue
		}
		data, ok := store.DataAs[*store.ToolPartData](p)
		if !ok || data.Status == store.ToolStatusCompleted || data.Status == store.ToolStatusError {
			continue
		}
		_ = s.updateToolStatus(ctx, partID, store.ToolStatusError, map[string]any{"interrupted": true}, "Tool execution aborted")
	}

	// Always release the toolCtx child-context entry from the parent chain,
	// regardless of path. On the cancel path toolCancel() was already called
	// above; calling it again is a no-op (CancelFunc is idempotent).
	if s.toolCancel != nil {
		s.toolCancel()
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

func (s *processorState) updateReasoningPart(ctx context.Context, id, text, signature string, start, end int64) error {
	p, err := s.store.GetPart(ctx, id)
	if err != nil {
		return err
	}
	p.Data = &store.ReasoningPartData{Text: text, Signature: signature, TimeStart: start, TimeEnd: end}
	return s.store.UpdatePart(ctx, p)
}

func (s *processorState) updateToolRunning(ctx context.Context, partID, toolName, callID string, input map[string]any) error {
	p, err := s.store.GetPart(ctx, partID)
	if err != nil {
		return err
	}
	existing, _ := store.DataAs[*store.ToolPartData](p)
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
	existing, _ := store.DataAs[*store.ToolPartData](p)
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
	existing, _ := store.DataAs[*store.ToolPartData](p)
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

// newID generates a cryptographically random 16-byte hex ID.
func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
