---
name: interrupt-test
description: Test user interruption (Cancel) boundary conditions for llm-go — covers store state after cancel, interrupted status classification, history consistency for the next turn, tool cleanup behaviour, and idempotency
---

# Interrupt-Test Skill

Tests the `RunLoopAsync` / `Cancel()` interruption mechanism and all edge cases
around what the store contains after a cancelled or interrupted turn.

> **Why unit tests, not HTTP tests?**
> `cmd/knowledge-api` uses `context.WithoutCancel(r.Context())` — a deliberate
> design decision that makes HTTP client disconnect unable to cancel a running
> `RunLoop`. There is also **no `/cancel` HTTP endpoint**. All interrupt paths
> go through `session.RunLoopAsync(…).Cancel()`, which is only exercisable at
> the Go test layer. The HTTP integration test (STEP H) verifies this isolation
> property explicitly.

---

## Execution Rules

**You MUST follow these rules or the test is invalid:**

1. **Run every step in sequence.** Do not skip or reorder.
2. **Check the ABORT condition after every step.** If it triggers, stop
   immediately and report `ABORTED at step N: <reason>`.
3. **Run each `go test` with `-count=1`** to prevent cached results.
4. **Capture the full test output** in a named variable (`OUT_A`, `OUT_B`, …).
5. Per-test timeout is **30 seconds** (`-timeout 30s`). A build failure is
   also an ABORT condition.

---

## Prerequisites

```bash
# Working directory must be the repo root
go build ./session/... && echo "BUILD OK"
```

**ABORT if:** build fails.

---

## Test Architecture

All tests live in `session/interrupt_boundary_test.go` (created by this skill
as needed) in `package session_test`. They reuse the helpers already in
`session/session_test.go` and `session/cancel_test.go`:

| Helper | Defined in |
|--------|-----------|
| `newSession(t)` | `cancel_test.go:35` |
| `cancelInput(sessID, prov, tools)` | `cancel_test.go:45` |
| `assistantMessages(t, s, sessID)` | `cancel_test.go:58` |
| `testModel()` | `session_test.go:63` |
| `simpleTextProvider(text)` | `session_test.go:33` |
| `toolCallProvider(name, id, input)` | `session_test.go:49` |
| `blockingProvider{}` | `session_test.go:508` |
| `textThenBlockProvider{text}` | `cancel_test.go:474` |
| `hasTextPart(parts)` | `cancel_test.go:462` |

---

## STEP A — Existing cancel tests pass (baseline)

```bash
go test ./session/... -run "TestToModelMessages|TestRunLoopAsync" -v -count=1 -timeout 30s 2>&1
```
Store output in `OUT_A`.

**ABORT if:** any `FAIL` line in output, or non-zero exit code.

**CHECK A:** Count `--- PASS` lines — must be **≥ 10** (the 5 unit + 5 integration
tests already in `cancel_test.go`).

---

## Boundary Conditions to Test

The following table lists the gaps in the existing test suite. Each one maps to
a new test to write in STEP B.

| ID  | Scenario | Key assertion |
|-----|----------|---------------|
| BC-1 | Cancel during tool call (tool is `running`) | `Message.Status=interrupted`; tool part has `Status=error`, `Interrupted=true`, `Error="Tool execution aborted"` |
| BC-2 | Cancel immediately after tool completes (race window) | `Message.Status=interrupted`; completed tool part preserved with `Status=completed` |
| BC-3 | Two consecutive cancels, then a normal turn | Third turn's LLM request has valid alternating roles (no protocol violation) |
| BC-4 | Cancel during second step of a multi-step loop | History after cancel: only first step (completed) and second step (cancelled/interrupted) visible |
| BC-5 | Interrupted turn with **only** incomplete tools followed by new turn | New turn sees ` ` placeholder (not the dropped assistant turn); LLM gets valid history |
| BC-6 | `Cancel()` called after `RunLoop` already completed (idempotent across completion) | No panic; `h.Done` already closed; second `Cancel()` is a no-op |
| BC-7 | Cancel with `MaxSteps=1` on a tool-calling provider — step limit reached before cancel | Result is `RunResultStop`, not an interrupt; `Message.Status=""` (normal stop, not cancelled) |
| BC-8 | `<-h.Done` closes **before** `ListParts` sees stale pending tool parts | After `<-h.Done`, every tool part is in `completed`, `error`, or `pending` state — never `running` |

---

## STEP B — Write the boundary test file

Write `session/interrupt_boundary_test.go` with tests for BC-1 through BC-8.

Use the following template for each test. Only write tests that are NOT already
covered by `cancel_test.go` (see the existing test list at the top of that file).

```go
package session_test

import (
    "context"
    "sync"
    "testing"
    "time"

    "github.com/larryhou/llm-go/llm"
    "github.com/larryhou/llm-go/session"
    "github.com/larryhou/llm-go/store"
    "github.com/larryhou/llm-go/store/memory"
    "github.com/larryhou/llm-go/tool"
)

// BC-1: Cancel while a slow tool is running (Status=running).
// After <-h.Done, the tool part must have Status=error, Interrupted=true.
func TestRunLoopAsync_cancelDuringToolExecution_toolMarkedInterrupted(t *testing.T) { … }

// BC-2: Cancel immediately after a tool completes.
// The race window between toolCancel() and cleanup()'s 250ms wait.
// After <-h.Done, the completed tool part MUST still have Status=completed.
func TestRunLoopAsync_cancelAfterToolCompletes_toolPreserved(t *testing.T) { … }

// BC-3: Two consecutive cancels, then a normal turn.
// Must produce valid alternating-role history for the third turn.
func TestRunLoopAsync_twoConsecutiveCancels_thenNormalTurn_validHistory(t *testing.T) { … }

// BC-4: Cancel during the second step of a multi-step loop.
// First step completed normally; second step is interrupted.
// After cancel, ListMessages must show exactly two assistant messages:
// one with Status="" (normal) and one with Status=interrupted/cancelled.
func TestRunLoopAsync_cancelDuringSecondStep_firstStepPreserved(t *testing.T) { … }

// BC-5: Interrupted turn with all-incomplete tools → dropped from context.
// Next turn must receive a " " placeholder assistant message, not a broken
// assistant message with dangling tool calls.
func TestRunLoopAsync_interruptedAllIncompleteTools_nextTurnSeesPlaceholder(t *testing.T) { … }

// BC-6: Cancel after RunLoop already completed — must not panic.
func TestRunLoopAsync_cancelAfterCompletion_noPanic(t *testing.T) { … }

// BC-7: MaxSteps=1 exhausted, no cancel — Status must be "" (normal stop).
func TestRunLoopAsync_maxStepsExhausted_notCancelled(t *testing.T) { … }

// BC-8: After <-h.Done, no tool part is in Status=running.
func TestRunLoopAsync_doneGuaranteesNoRunningToolParts(t *testing.T) { … }
```

### Provider helpers needed for BC-1, BC-2, BC-4

These go at the bottom of the file, or in a `_helpers_test.go` if you prefer:

```go
// slowToolProvider emits a tool call and then blocks until ctx is cancelled,
// simulating a slow external tool that never finishes.
type slowToolProvider struct {
    toolName string
    callID   string
    ready    chan struct{} // closed when tool call event is sent
}

func (p *slowToolProvider) ID() string { return "slow-tool" }
func (p *slowToolProvider) Stream(ctx context.Context, _ llm.Request) (<-chan llm.Event, error) {
    ch := make(chan llm.Event, 8)
    go func() {
        defer close(ch)
        ch <- llm.Event{Type: llm.EventRequestStart}
        ch <- llm.Event{Type: llm.EventStepStart}
        ch <- llm.Event{Type: llm.EventToolInputStart, ToolName: p.toolName, ToolCallID: p.callID}
        ch <- llm.Event{Type: llm.EventToolCall, ToolName: p.toolName, ToolCallID: p.callID,
            Input: map[string]any{"arg": "value"}}
        ch <- llm.Event{Type: llm.EventStepFinish, Usage: llm.TokenUsage{Input: 5, Output: 2},
            FinishReason: llm.FinishReasonToolCalls}
        ch <- llm.Event{Type: llm.EventRequestFinish, FinishReason: llm.FinishReasonToolCalls}
        if p.ready != nil {
            close(p.ready)
        }
        <-ctx.Done()
    }()
    return ch, nil
}

// twoStepProvider emits step 1 (text), then calls a tool in step 2, then blocks.
// Used to test cancel-during-second-step scenarios.
type twoStepProvider struct {
    step1Text string
    toolName  string
    callID    string
    step2Ready chan struct{} // closed when step 2 tool call event is sent
}
```

**Important implementation notes:**

- For **BC-1**: the tool must actually be registered with the `RunInput.Tools`
  slice. Use a `tool.Tool` implementation that blocks until ctx is cancelled.
  Then `Cancel()` after the `slowToolProvider.ready` channel is closed.
  After `<-h.Done`, check that the part has `Status=store.ToolStatusError`
  and `data.Interrupted == true`.

- For **BC-2**: use the same `slowToolProvider` but register a **fast tool**
  that completes in <1ms. Cancel immediately after the `ready` channel closes.
  Verify that the part that completed keeps `Status=store.ToolStatusCompleted`.

- For **BC-3** and **BC-5**: use the existing `captureProvider` pattern from
  `cancel_test.go:496` to inspect the LLM request after the third turn.

- For **BC-4**: use a `twoStepProvider` where step 1 completes and step 2
  triggers a tool call, then blocks. Cancel after `step2Ready` closes.
  After `<-h.Done`, `ListMessages` should return 2 assistant messages.

- For **BC-7**: confirm `h.Err == nil` (no error) and `h.Result ==
  session.RunResultStop` after `<-h.Done`. Message status must be `""`.

---

## STEP C — Build the test file

```bash
go build ./session/... 2>&1
```

**ABORT if:** build fails. Fix errors before proceeding.

---

## STEP D — Run all BC tests

```bash
OUT_D=$(go test ./session/... -run "TestRunLoopAsync_cancel|TestRunLoopAsync_interrupted|TestRunLoopAsync_max|TestRunLoopAsync_done|TestRunLoopAsync_two" -v -count=1 -timeout 30s 2>&1)
echo "$OUT_D"
```

**ABORT if:** any `FAIL` line in output, or non-zero exit code.

**CHECK D — record results for each test:**

| Test | Expected | Result |
|------|----------|--------|
| BC-1 `cancelDuringToolExecution` | tool `Status=error`, `Interrupted=true` | PASS / FAIL |
| BC-2 `cancelAfterToolCompletes` | completed tool preserved | PASS / FAIL |
| BC-3 `twoConsecutiveCancels` | no consecutive user msgs | PASS / FAIL |
| BC-4 `cancelDuringSecondStep` | first step preserved, second interrupted | PASS / FAIL |
| BC-5 `interruptedAllIncompleteTools` | ` ` placeholder seen by next turn | PASS / FAIL |
| BC-6 `cancelAfterCompletion` | no panic | PASS / FAIL |
| BC-7 `maxStepsExhausted` | `Status=""`, `Result=Stop` | PASS / FAIL |
| BC-8 `doneGuaranteesNoRunningToolParts` | no `running` parts after Done | PASS / FAIL |

---

## STEP E — Run the full session test suite (regression)

```bash
OUT_E=$(go test ./session/... -v -count=1 -timeout 60s 2>&1)
echo "$OUT_E" | grep -E "^=== RUN|^--- (PASS|FAIL)|^FAIL|^ok"
```

**ABORT if:** any `FAIL` in output.

**CHECK E:** all pre-existing tests still pass (baseline from STEP A).

---

## STEP F — Status classification boundary: empty-text part

This is a special edge case not captured by the above:

> An assistant message that has a `PartTypeText` part but the text is `""`
> (empty string) should be classified as **`cancelled`**, not `interrupted`,
> because `hasRealContent` requires non-empty text.

Write and run a targeted test:

```bash
go test ./session/... -run "TestMarkAssistantCancelled_emptyTextPart" -v -count=1 -timeout 10s 2>&1
```

**Test stub:**
```go
// TestMarkAssistantCancelled_emptyTextPart: a text part with Text="" must not
// prevent the message from being classified as cancelled (hasRealContent=false).
func TestMarkAssistantCancelled_emptyTextPart(t *testing.T) {
    // Use blockingProvider so LLM never sends events.
    // Then manually inject an empty text part into the assistant message.
    // Cancel → check Status=cancelled (not interrupted).
    // This tests the hasRealContent edge case in markAssistantCancelled.
}
```

**CHECK F:**
- `Status=cancelled` when only empty text part exists → ✓ PASS
- `Status=interrupted` → ✗ FAIL: `hasRealContent` incorrectly counting empty string

---

## STEP G — `<-h.Done` ordering: concurrent read safety

This verifies that reading store state after `<-h.Done` is safe from all
goroutines without additional synchronisation.

```bash
go test ./session/... -run "TestRunLoopAsync_doneAfterStoreConsistent" \
    -v -count=10 -timeout 60s -race 2>&1 | tail -5
```

**CHECK G:**
- No `DATA RACE` in output → ✓ PASS
- `DATA RACE` reported → ✗ FAIL: store writes not properly sequenced before Done closes

---

## STEP H — HTTP layer isolation: disconnect does NOT cancel RunLoop

This is a documentation/verification test. Since `handleChat` uses
`context.WithoutCancel(r.Context())`, the RunLoop is deliberately shielded
from HTTP client disconnects.

**Verification procedure** (requires server running):

```bash
# Start server if not running
# LLM connection — resolved in this order:
#   1. Environment variables (preferred):
#        TIMI_PROVIDER  — "openai" or "anthropic"  (default: anthropic)
#        TIMI_BASE_URL  — provider base URL
#        TIMI_API_KEY   — API key
#        TIMI_MODEL     — model ID                  (default: claude-sonnet-4.6)
#   2. Hardcoded defaults in cmd/knowledge-api/main.go (flag.StringVar lines)
#   3. If still unresolved (e.g. main.go defaults unavailable), ask the user.

lsof -ti:7700 | xargs kill -9 2>/dev/null; sleep 1
nohup go run ./cmd/knowledge-api/ \
  -skills .opencode -addr 127.0.0.1:7700 \
  > /tmp/kapi.log 2>&1 &
sleep 6 && curl -s http://127.0.0.1:7700/health
```

**Procedure:**

```bash
SESS_H="itest-$(date +%s)"

# Start a long-running turn and kill curl after 2 seconds
timeout 2 curl -sN --max-time 30 -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d "{\"message\":\"Count from 1 to 100, one number per line.\",\"session_id\":\"$SESS_H\",\"context_limit\":20000,\"tools\":[],\"max_steps\":2}" \
  || true   # timeout/kill is expected, not an error

# Wait for server to finish (it should, despite disconnect)
sleep 10

# Check session state
curl -s http://127.0.0.1:7700/sessions/$SESS_H/messages | python3 -c "
import json,sys
d=json.load(sys.stdin)
print('messages:', d['message_count'])
for m in d['messages']:
    role = m['role']
    status = m.get('status','')
    ptypes = [p['type'] for p in m['parts']]
    print(f'  {role} status={status!r} parts={ptypes}')
"
```

**CHECK H:**
- Assistant message has `status=""` (not `"cancelled"` or `"interrupted"`) → ✓ PASS: `context.WithoutCancel` worked; RunLoop completed despite disconnect
- Assistant message has `status="cancelled"` or `status="interrupted"` → ✗ FAIL: HTTP disconnect propagated to RunLoop — check `context.WithoutCancel` in `handleChat`
- No assistant message at all → INDETERMINATE: server may still be processing; increase `sleep` and retry

---

## STEP I — Run all tests one final time (full suite)

```bash
go test ./session/... -count=1 -timeout 60s 2>&1 | tail -3
```

Must end with `ok  <module-path>/session`.

---

## Test Report Template

```
=== INTERRUPT-TEST REPORT ===
Date     : <date>
Revision : <git rev-parse --short HEAD>

STEP A — baseline (existing cancel tests):
  Total existing cancel/interrupt tests  : N   (must be ≥ 10)
  Result                                 : PASS / ABORT

STEP D — boundary condition tests:
  BC-1  cancelDuringToolExecution        : PASS / FAIL
  BC-2  cancelAfterToolCompletes         : PASS / FAIL
  BC-3  twoConsecutiveCancels            : PASS / FAIL
  BC-4  cancelDuringSecondStep           : PASS / FAIL
  BC-5  interruptedAllIncompleteTools    : PASS / FAIL
  BC-6  cancelAfterCompletion            : PASS / FAIL
  BC-7  maxStepsExhausted                : PASS / FAIL
  BC-8  doneGuaranteesNoRunningParts     : PASS / FAIL

STEP E — regression (all session tests):  PASS / ABORT

STEP F — emptyTextPart classification:    PASS / FAIL

STEP G — race detector (×10 runs):        PASS / FAIL

STEP H — HTTP disconnect isolation:
  RunLoop completed despite disconnect   : PASS / FAIL / INDETERMINATE

STEP I — final full suite:                PASS / ABORT

VERDICT: PASS / FAIL / ABORTED at step N
```

---

## Key Code Locations

| Concept | File | Location |
|---------|------|----------|
| `RunLoopAsync` / `RunHandle.Cancel()` | `session/prompt.go` | `:85–123` |
| `markAssistantCancelled` | `session/prompt.go` | `:435–446` |
| `hasRealContent` | `session/context.go` | `:322–338` |
| `hasToolCalls` | `session/prompt.go` | `:394–401` |
| `cleanup()` — 250ms tool goroutine timeout | `session/processor.go` | `:387–439` |
| `buildAssistantPartsInterrupted` | `session/context.go` | `:126–201` |
| `context.WithoutCancel` in handleChat | `cmd/knowledge-api/main.go` | `handleChat()` ctx setup |
| `DoomLoopThreshold` | `session/processor.go` | `:22–24` |
| Consecutive-user-message guard | `session/context.go` | `ToModelMessages` loop |
| `MessageStatusCancelled` / `MessageStatusInterrupted` | `store/store.go` | `MessageStatus*` consts |
| `ToolStatusCompleted` / `ToolStatusError` / `Interrupted` | `store/store.go` | `ToolStatus*` + `ToolPartData` |

## Known Edge Cases

| Edge Case | Where | Behaviour |
|-----------|-------|-----------|
| Cancel before LLM responds | `prompt.go:435` | `Status=cancelled`, 0 parts |
| Cancel mid-text | `processor.go:390–394` | text part finalised with `TimeEnd` set |
| Cancel during tool (running) | `processor.go:406–438` | tool part → `Status=error`, `Interrupted=true`, `Error="Tool execution aborted"` |
| Cancel with completed tool | `prompt.go:439` | `Status=interrupted`; completed tool preserved |
| Cancel with all-incomplete tools | `context.go:190` | entire turn dropped; ` ` placeholder inserted |
| Cancel during compaction | `compaction.go:254` | `context.WithoutCancel` shields compaction; cancel takes effect only after compaction finishes |
| Empty text part (`Text=""`) | `context.go:327` | `hasRealContent=false` → classified as `cancelled`, not `interrupted` |
| Tool goroutine outlives 250ms window | `processor.go` `isAlreadyInterrupted()` | cleanup marks part `error+Interrupted=true`; executeTool guards with `isAlreadyInterrupted()` to prevent overwrite |
| Cancel() after loop completion | `prompt.go:100–105` | `sync.Once` makes it a no-op; no panic |
| HTTP disconnect | `main.go` `handleChat` | `context.WithoutCancel` — RunLoop is NOT cancelled; completes normally |
| Cancel during cleanup 250ms wait | `prompt.go` `runLoopInternal` | Even when Process() returns (nil, nil), ctx.Err() check prevents a new iteration from starting |

## Fixed Bugs (discovered by interrupt-test run)

| Bug | Fix | Commit |
|-----|-----|--------|
| `executeTool` could overwrite `Interrupted=true` set by cleanup's 250ms timeout | Added `isAlreadyInterrupted()` guard in `session/processor.go` before writing tool result | `da15367` |
| RunLoop continued iterating after Cancel() when cleanup ran its 250ms wait (Process returned nil error) | Added `ctx.Err()` check after `Process()` returns in `runLoopInternal` | `da15367` |
| DATA RACE in `countingTool.Execute` (test helper) — `calls` field written by concurrent goroutines | Added `sync.Mutex` to `countingTool` | `da15367` |
