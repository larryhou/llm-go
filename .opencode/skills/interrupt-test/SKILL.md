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

STEP H — HTTP Chat interrupt tests:
  H-1  disconnect isolation             : PASS / FAIL / INDETERMINATE
  H-2  slow tool survives disconnect    : PASS / FAIL / INDETERMINATE
  H-3  max_steps normal stop (no interrupt) : PASS / FAIL
  H-4  disconnect then next turn clean  : PASS / FAIL
  H-5  session state after normal turn  : PASS / FAIL

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

---

# HTTP Chat 中断测试 (STEP H)

通过 `/chat` HTTP 接口验证中断相关行为。

> **为什么使用动态 curl 测试而非固定脚本？**
> `RunLoopAsync.Cancel()` 在 HTTP 层不可调用（无 `/cancel` 端点），且
> `context.WithoutCancel` 屏蔽了 HTTP disconnect 对 RunLoop 的影响。
> 因此 HTTP 层的"中断"只能通过 **连接超时/断开** 和 **max_steps** 间接观察，
> 需要结合 `/sessions/{id}/messages` 检查 store 内的 status 字段来判定。

---

## 执行规则

**必须遵守，否则测试无效：**

1. **按顺序执行每个步骤**，不得跳过或重排。
2. **每步检查 ABORT 条件**，触发则立即停止并报告 `ABORTED at H-N: <原因>`。
3. **将每次 curl 响应保存在命名变量** (`RH1`, `RH2`, …)。
4. **每次请求超时 120 秒** (`--max-time 120`)。curl 退出码 28 = ABORT。
5. **使用 `$SESS_H` 贯穿所有 `/chat` 调用**，不得中途更换。
6. 测试结束后打印结构化 **H 测试报告**（见本节末尾）。

---

## 服务器启动

```bash
# LLM 连接解析顺序：
#   1. 环境变量（优先）：
#        TIMI_PROVIDER  — "openai" 或 "anthropic"  (默认: anthropic)
#        TIMI_BASE_URL  — provider base URL
#        TIMI_API_KEY   — API key
#        TIMI_MODEL     — 模型 ID                  (默认: claude-sonnet-4.6)
#   2. cmd/knowledge-api/main.go 中 flag.StringVar 的硬编码默认值
#   3. 以上均不可用时询问用户

lsof -ti:7700 | xargs kill -9 2>/dev/null; sleep 1
nohup go run ./cmd/knowledge-api/ \
  -skills .opencode -addr 127.0.0.1:7700 \
  > /tmp/kapi.log 2>&1 &
sleep 6 && curl -s http://127.0.0.1:7700/health
```

健康响应：`{"status":"ok","doc_count":N,"session_count":N}`

**ABORT if:** 响应不含 `"status":"ok"`。

---

## STEP H-0 — 服务器健康检查

```bash
curl -s --max-time 10 http://127.0.0.1:7700/health
```

**ABORT if:** 响应不含 `"status":"ok"`。

---

## STEP H-1 — 断开连接隔离：RunLoop 不受 HTTP disconnect 影响

**验证核心不变式：** `context.WithoutCancel` 保证 HTTP 客户端断开后，
服务端 RunLoop 仍正常完成，session store 中留下完整 assistant 消息（`status=""`）。

```bash
SESS_H="itest-$(date +%s)"
echo "SESSION: $SESS_H"

# 启动请求后 2 秒强制断开（用 timeout 模拟客户端离开）
timeout 2 curl -sN --max-time 30 -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d "{\"message\":\"Please count from 1 to 20, one number per line, slowly.\",\"session_id\":\"$SESS_H\",\"context_limit\":20000,\"tools\":[],\"max_steps\":2}" \
  || true   # timeout/kill 是预期的，不是错误

# 等待服务端完成（RunLoop 在后台继续运行）
echo "等待服务端完成（约 15 秒）..."
sleep 15

# 检查 session 状态
RH1_STATE=$(curl -s http://127.0.0.1:7700/sessions/$SESS_H/messages)
echo "$RH1_STATE" | python3 -c "
import json, sys
d = json.load(sys.stdin)
print('messages:', d['message_count'])
for m in d['messages']:
    role = m['role']
    status = m.get('status', '')
    ptypes = [p['type'] for p in m['parts']]
    summaries = [p.get('summary','') for p in m['parts']]
    print(f'  {role} status={status!r} parts={ptypes}')
    for s in summaries:
        if s: print(f'    {s[:100]}')
"
```

**ABORT if:** curl timeout（exit 28）在 2 秒强制断开之前触发（说明服务未启动）。

**CHECK H-1:**

```bash
echo "$RH1_STATE" | python3 -c "
import json, sys
d = json.load(sys.stdin)
msgs = d['messages']
assistant_msgs = [m for m in msgs if m['role'] == 'assistant']
if not assistant_msgs:
    print('INDETERMINATE: no assistant message yet (server still processing?)')
    sys.exit(0)
last = assistant_msgs[-1]
status = last.get('status', '')
if status == '':
    print('PASS: assistant status=empty (normal completion despite disconnect)')
elif status in ('cancelled', 'interrupted'):
    print(f'FAIL: assistant status={status!r} — HTTP disconnect propagated to RunLoop!')
    print('Check: context.WithoutCancel in handleChat')
else:
    print(f'UNKNOWN: assistant status={status!r}')
"
```

| 结果 | 含义 |
|------|------|
| `status=""` | ✓ **PASS** — `context.WithoutCancel` 生效，RunLoop 正常完成 |
| `status="cancelled"` 或 `"interrupted"` | ✗ **FAIL** — disconnect 传播到了 RunLoop |
| 无 assistant 消息 | **INDETERMINATE** — 增加 sleep 时间后重试（最多 2 次） |

---

## STEP H-2 — 慢工具存活：disconnect 期间工具继续执行直到完成

**验证：** 客户端断开时 `slow_calc` 工具（睡 2 秒）能正常完成，
结果写入 store，不被 cleanup 的 250ms 超时截断。

> **原理：** `slow_calc` 用 `select { case <-time.After(2s): case <-ctx.Done(): }`
> 实现。`context.WithoutCancel` 保证工具 ctx 不会因 HTTP disconnect 取消，
> 所以工具能跑完整 2 秒。

```bash
SESS_H2="itest-slow-$(date +%s)"

# 启动后 1 秒断开（工具还在睡眠中）
timeout 1 curl -sN --max-time 30 -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d "{\"message\":\"Use slow_calc to compute 13 * 17. Please wait for the result.\",\"session_id\":\"$SESS_H2\",\"context_limit\":20000,\"tools\":[\"slow_calc\"],\"max_steps\":3}" \
  || true

# 等待工具完成 + RunLoop 结束（工具需 2s，再加处理时间）
echo "等待慢工具完成（约 10 秒）..."
sleep 10

RH2_STATE=$(curl -s http://127.0.0.1:7700/sessions/$SESS_H2/messages)
echo "$RH2_STATE" | python3 -c "
import json, sys
d = json.load(sys.stdin)
print('messages:', d['message_count'])
for m in d['messages']:
    role = m['role']
    status = m.get('status', '')
    for p in m['parts']:
        s = p.get('summary', '')
        if 'tool=' in s:
            print(f'  {role} status={status!r} | {s}')
        elif s:
            print(f'  {role} status={status!r} | text: {s[:80]}')
"
```

**CHECK H-2:**

```bash
echo "$RH2_STATE" | python3 -c "
import json, sys
d = json.load(sys.stdin)
msgs = d['messages']

# 找 slow_calc 工具 part
tool_parts = []
for m in msgs:
    for p in m['parts']:
        s = p.get('summary', '')
        if 'tool=slow_calc' in s:
            tool_parts.append((m['role'], m.get('status',''), s))

if not tool_parts:
    print('INDETERMINATE: no slow_calc tool part found (LLM may not have called it yet)')
    sys.exit(0)

for role, mstatus, summary in tool_parts:
    print(f'slow_calc part: {summary}')
    if 'status=completed' in summary:
        print('PASS: slow_calc completed — tool survived disconnect (context.WithoutCancel working)')
    elif 'status=error' in summary:
        # 可能是工具执行出错（不是 interrupt）
        print(f'CHECK: slow_calc status=error — verify Interrupted field')
    else:
        print(f'UNKNOWN: {summary}')

# 检查 assistant 消息的 status
assistant_msgs = [m for m in msgs if m['role'] == 'assistant']
if assistant_msgs:
    last_status = assistant_msgs[-1].get('status', '')
    if last_status == '':
        print('PASS: assistant message completed normally')
    else:
        print(f'FAIL: assistant status={last_status!r} — unexpected interrupt')
"
```

| 结果 | 含义 |
|------|------|
| `slow_calc status=completed` + `assistant status=""` | ✓ **PASS** — 工具在 disconnect 后存活 |
| `slow_calc status=error` (Interrupted=true) | ✗ **FAIL** — 工具被 cleanup 250ms 截断；检查 `context.WithoutCancel` |
| 无 slow_calc part | **INDETERMINATE** — LLM 可能未调用工具；增加 sleep 重试 |

---

## STEP H-3 — max_steps 正常停止：不产生 interrupt/cancelled 状态

**验证：** `max_steps=1` 不是中断，是正常停止。
assistant 消息 `status=""` 且 SSE 流有 `"type":"done"` 事件。

```bash
RH3=$(curl -sN --max-time 120 -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d '{"message":"Use calc for 2+2, then calc for 3+3, then calc for 4+4. Do all three.","tools":["calc"],"max_steps":1}')

echo "Terminal event:"
echo "$RH3" | grep '"type":"done"\|"type":"error"' | head -1

SESS_H3=$(echo "$RH3" | grep '"session_id"' | sed 's/.*"session_id":"\([^"]*\)".*/\1/' | head -1)
echo "Session: $SESS_H3"
```

**ABORT if:** curl exit 28（超时）。

**CHECK H-3:**

```bash
echo "$RH3" | python3 -c "
import json, sys
lines = sys.stdin.read()
# 检查终止事件
if '\"type\":\"done\"' in lines:
    print('PASS: done event received (normal stop)')
elif '\"type\":\"error\"' in lines:
    print('CHECK: error event — may be doom-loop or LLM error; check content')
else:
    print('FAIL: no terminal event')
    sys.exit(1)

# 统计 tool_call 数量
tc = lines.count('\"type\":\"tool_call\"')
print(f'tool_call events: {tc} (max_steps=1 limits LLM turns, not individual calls)')
"

# 检查 session store：assistant 消息 status 必须为空（正常完成）
RH3_STATE=$(curl -s "http://127.0.0.1:7700/sessions/$SESS_H3/messages")
echo "$RH3_STATE" | python3 -c "
import json, sys
d = json.load(sys.stdin)
for m in d['messages']:
    if m['role'] == 'assistant':
        status = m.get('status', '')
        if status == '':
            print(f'PASS: assistant status=empty — max_steps stop is NOT an interrupt')
        else:
            print(f'FAIL: assistant status={status!r} — max_steps incorrectly produced interrupt state')
"
```

| 结果 | 含义 |
|------|------|
| `done` 事件 + `assistant status=""` | ✓ **PASS** — max_steps 是正常停止，非中断 |
| `assistant status="interrupted"` 或 `"cancelled"` | ✗ **FAIL** — max_steps 被误分类为中断 |

---

## STEP H-4 — Disconnect 后续轮次连贯：第一轮断开不污染 history

**验证：** 第一轮被 disconnect（服务端完成后 status=""），
第二轮能正常看到第一轮的结果并继续对话。

```bash
SESS_H4="itest-cont-$(date +%s)"

# 第一轮：植入锚点 9973，然后 disconnect
timeout 2 curl -sN --max-time 30 -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d "{\"message\":\"Remember the special code 9973. Use counter to set key anchor=9973. Confirm.\",\"session_id\":\"$SESS_H4\",\"context_limit\":20000,\"tools\":[\"counter\"],\"max_steps\":3}" \
  || true

echo "等待第一轮完成（约 15 秒）..."
sleep 15

# 检查第一轮状态
RH4_STATE1=$(curl -s "http://127.0.0.1:7700/sessions/$SESS_H4/messages")
echo "第一轮后 session 状态:"
echo "$RH4_STATE1" | python3 -c "
import json, sys
d = json.load(sys.stdin)
print('messages:', d['message_count'])
for m in d['messages']:
    print(f'  {m[\"role\"]} status={m.get(\"status\",\"\")!r}')
    for p in m['parts']:
        s = p.get('summary','')
        if s: print(f'    {s[:100]}')
"

# 第二轮：正常请求，验证 LLM 能回忆第一轮内容
RH4=$(curl -sN --max-time 120 -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d "{\"message\":\"What was the special code I gave you? And what is the current value of counter key anchor?\",\"session_id\":\"$SESS_H4\",\"context_limit\":20000,\"tools\":[\"counter\"],\"max_steps\":3}")

echo "第二轮终止事件:"
echo "$RH4" | grep '"type":"done"\|"type":"error"' | head -1
```

**ABORT if:** 第二轮 curl exit 28。

**CHECK H-4:**

```bash
echo "$RH4" | python3 -c "
import json, sys
lines = sys.stdin.read()
if '9973' in lines:
    print('PASS: LLM recalled code 9973 — first turn (post-disconnect) preserved in history')
else:
    print('FAIL: LLM did not recall 9973 — disconnect may have broken history')
    sys.exit(1)

if '\"type\":\"done\"' in lines:
    print('PASS: second turn completed cleanly')
else:
    print('WARN: no done event in second turn')
"

# 验证两轮均为 status="" (不是 cancelled/interrupted)
RH4_STATE2=$(curl -s "http://127.0.0.1:7700/sessions/$SESS_H4/messages")
echo "$RH4_STATE2" | python3 -c "
import json, sys
d = json.load(sys.stdin)
print('final messages:', d['message_count'])
bad = []
for m in d['messages']:
    if m['role'] == 'assistant':
        s = m.get('status','')
        if s in ('cancelled','interrupted'):
            bad.append(f'{m[\"id\"][:8]} status={s!r}')
if bad:
    print('FAIL: interrupted assistant messages:', bad)
else:
    print('PASS: all assistant messages status=empty (clean history)')
"
```

| 结果 | 含义 |
|------|------|
| 第二轮包含 `9973` + 所有 assistant `status=""` | ✓ **PASS** — disconnect 后 history 完整连贯 |
| 第二轮未提到 `9973` | ✗ **FAIL** — 第一轮 history 未被正确保存 |
| 有 assistant `status="cancelled"/"interrupted"` | ✗ **FAIL** — disconnect 触发了中断状态 |

---

## STEP H-5 — Session 状态结构验证：正常完成后 store 内容正确

**验证：** 一次含工具调用的正常对话结束后，`/sessions/{id}/messages`
中 assistant 消息 `status=""`，工具 part `status=completed`，
不存在 `status=running` 的工具 part（Done 保证）。

```bash
RH5=$(curl -sN --max-time 120 -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d '{"message":"Use calc to compute 42 * 100. Tell me the result.","tools":["calc"],"max_steps":3}')

echo "终止事件:"
echo "$RH5" | grep '"type":"done"\|"type":"error"' | head -1

SESS_H5=$(echo "$RH5" | grep '"session_id"' | sed 's/.*"session_id":"\([^"]*\)".*/\1/' | head -1)
echo "Session: $SESS_H5"

RH5_STATE=$(curl -s "http://127.0.0.1:7700/sessions/$SESS_H5/messages")
echo "$RH5_STATE" | python3 -c "
import json, sys
d = json.load(sys.stdin)
print('messages:', d['message_count'])

issues = []
for m in d['messages']:
    role = m['role']
    mstatus = m.get('status', '')
    for p in m['parts']:
        s = p.get('summary', '')
        # 检查工具 part 状态
        if 'tool=' in s:
            if 'status=running' in s:
                issues.append(f'{role} tool part still running: {s}')
            elif 'status=completed' in s:
                print(f'  PASS: {role} | {s}')
            elif 'status=error' in s and 'Interrupted=true' not in s:
                print(f'  CHECK: {role} | {s} (tool error, not interrupt)')
    # 检查 assistant message status
    if role == 'assistant':
        if mstatus == '':
            print(f'  PASS: assistant status=empty (normal completion)')
        else:
            issues.append(f'assistant status={mstatus!r}')

if issues:
    print()
    for i in issues: print(f'FAIL: {i}')
else:
    print()
    print('PASS: all parts in terminal state, no running tools after Done')
"
```

**ABORT if:** curl exit 28。

**CHECK H-5 判定：**

| 检查项 | 期望 | 结果 |
|--------|------|------|
| `done` 事件 | 存在 | PASS / FAIL |
| `assistant status=""` | 空字符串 | PASS / FAIL |
| `calc` tool part `status=completed` | completed | PASS / FAIL |
| 无 `status=running` 的 tool part | 不存在 | PASS / FAIL |

---

## STEP H-6 — 最终 session 状态汇总

```bash
# 汇总所有测试 session 的最终状态
for sess in "$SESS_H" "$SESS_H2" "$SESS_H4" "$SESS_H5"; do
  echo "=== session: $sess ==="
  curl -s "http://127.0.0.1:7700/sessions/$sess/messages" | python3 -c "
import json, sys
d = json.load(sys.stdin)
print('  total messages:', d['message_count'])
for m in d['messages']:
    role = m['role']
    status = m.get('status', '')
    ptypes = [p['type'] for p in m['parts']]
    print(f'  {role} status={status!r} parts={ptypes}')
  " 2>/dev/null || echo "  (session not found)"
done
```

此步骤仅供参考，无 ABORT 条件。

---

## H 测试报告模板

```
=== INTERRUPT-TEST STEP H REPORT ===
Date    : <date>
Server  : http://127.0.0.1:7700
Session : SESS_H=<id>  SESS_H2=<id>  SESS_H4=<id>  SESS_H5=<id>

H-1  断开连接隔离 (context.WithoutCancel)
     assistant status="" after disconnect  : PASS / FAIL / INDETERMINATE

H-2  慢工具存活 (slow_calc 2s, disconnect after 1s)
     slow_calc status=completed            : PASS / FAIL / INDETERMINATE
     assistant status="" (normal finish)   : PASS / FAIL / INDETERMINATE

H-3  max_steps 正常停止 (不产生 interrupt)
     done event received                   : PASS / FAIL
     assistant status="" (not interrupted) : PASS / FAIL

H-4  Disconnect 后续轮次连贯
     second turn recalled code 9973        : PASS / FAIL
     all assistant messages status=""      : PASS / FAIL

H-5  Session 状态结构验证
     done event                            : PASS / FAIL
     assistant status=""                   : PASS / FAIL
     calc tool status=completed            : PASS / FAIL
     no running tool parts after Done      : PASS / FAIL

VERDICT: PASS / FAIL / ABORTED at H-N
```

---

## H 测试中断条件汇总

| 步骤 | ABORT 触发条件 |
|------|----------------|
| H-0 | 服务器不健康 |
| H-1 | curl exit 28 在 2s 强制断开之前 |
| H-2 | curl exit 28 在 1s 强制断开之前 |
| H-3 | curl exit 28（120s 超时） |
| H-4 | 第二轮 curl exit 28 |
| H-5 | curl exit 28 |

---

## H 测试关键代码位置

| 概念 | 文件 | 位置 |
|------|------|------|
| `context.WithoutCancel` 屏蔽 disconnect | `cmd/knowledge-api/main.go` | `handleChat()` ctx 初始化 |
| `slow_calc` 工具（ctx-aware 睡眠） | `cmd/knowledge-api/main.go` | `buildTestTools()` |
| cleanup 250ms 工具超时 | `session/processor.go` | `cleanup()` |
| `isAlreadyInterrupted()` 防竞争写 | `session/processor.go` | `executeTool()` |
| `MessageStatus*` 常量 | `store/store.go` | `MessageStatus*` block |
| `ToolStatus*` + `ToolPartData.Interrupted` | `store/store.go` | `ToolStatus*` block |
| `/sessions/{id}/messages` 状态渲染 | `cmd/knowledge-api/main.go` | `handleSession()` |
