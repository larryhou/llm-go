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

## STEP B — Verify boundary test file exists

All BC-1 through BC-8 tests (plus STEP F's `TestMarkAssistantCancelled_emptyTextPart`)
are pre-written in `session/interrupt_boundary_test.go`. Verify the file is present
and the package builds:

```bash
go build ./session/... && echo "BUILD OK"
```

**ABORT if:** build fails.

The tests in that file cover:

| Test function | BC |
|---------------|----|
| `TestRunLoopAsync_cancelDuringToolExecution_toolMarkedInterrupted` | BC-1 |
| `TestRunLoopAsync_cancelAfterToolCompletes_toolPreserved` | BC-2 |
| `TestRunLoopAsync_twoConsecutiveCancels_thenNormalTurn_validHistory` | BC-3 |
| `TestRunLoopAsync_cancelDuringSecondStep_firstStepPreserved` | BC-4 |
| `TestRunLoopAsync_interruptedAllIncompleteTools_nextTurnSeesPlaceholder` | BC-5 |
| `TestRunLoopAsync_cancelAfterCompletion_noPanic` | BC-6 |
| `TestRunLoopAsync_maxStepsExhausted_notCancelled` | BC-7 |
| `TestRunLoopAsync_doneGuaranteesNoRunningToolParts` | BC-8 |
| `TestMarkAssistantCancelled_emptyTextPart` | STEP F |

If the file is missing or incomplete, write the missing tests before proceeding.

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

## STEP H — HTTP 对话式中断 E2E 测试

通过 `/chat` HTTP 接口验证对话式中断的全链路行为。

> **执行方式：** 参考 compact-test skill 的风格——实时消费每轮的 SSE 流，
> 流结束即验证，agent 自主选择实现手段（curl、脚本、工具调用均可）。
> 不依赖 sleep + 轮询。

### 服务器

```
Base URL : http://127.0.0.1:7700
```

启动方式（若未运行）：

```bash
lsof -ti:7700 | xargs kill -9 2>/dev/null; sleep 1
nohup go run ./cmd/knowledge-api/ -skills .opencode -addr 127.0.0.1:7700 \
  > /tmp/kapi.log 2>&1 &
sleep 6 && curl -s http://127.0.0.1:7700/health
```

LLM 连接通过环境变量 `TIMI_PROVIDER` / `TIMI_BASE_URL` / `TIMI_API_KEY` / `TIMI_MODEL` 配置，
或读取 `cmd/knowledge-api/main.go` 中的默认值。

**ABORT if:** `/health` 响应不含 `"status":"ok"`。

---

### 测试序列

使用同一 session（格式 `hmix-<timestamp>`）贯穿全部轮次。
三种中断类型各出现**两次**，顺序随机打乱，中间穿插正常轮次植入和验证锚点。

| 轮次 | 类型 | 目的 | 关键数值 |
|------|------|------|----------|
| T1 | **normal** | `calc(7*8)` | 植入锚点 **56** |
| T2 | **cancelled** | LLM 响应前中断（0.3s 内发下一轮） | — |
| T2-int | **normal** | 验证 cancelled 后 history 连贯 | 回复应含 **56** |
| T3 | **normal** | `counter set anchor=9973` | 植入锚点 **9973** |
| T4 | **interrupted+tool** | `calc(99*99)` 后让 LLM 写长文；在 tool_result 出现、LLM step-2 开始输出后发下一轮 | — |
| T4-int | **normal** | 验证 tool 结果保留 | 回复应含 **9801** |
| T5 | **normal** | `calc(12*34)` | 植入锚点 **408** |
| T6 | **interrupted-text** | 让 LLM 写长文；第一个 text delta 出现后发下一轮 | — |
| T6-int | **normal** | 验证 history 连贯 | 回复应含 **9973** |
| T7 | **cancelled** | 再次在 LLM 响应前中断 | — |
| T7-int | **normal** | 验证多锚点均可知 | 回复应含 **56** 和 **408** |
| T8 | **interrupted+tool** | `calc(56+408)` 后让 LLM 写长文；tool_result 后 step-2 开始时发下一轮 | — |
| T8-int | **normal** | 验证 tool 结果保留 | 回复应含 **464** |
| T9 | **interrupted-text** | 再次在 text streaming 时中断 | — |
| T9-int | **normal** | 验证三个锚点均可知 | 回复应含 **56**、**9973**、**408** |
| T10 | **normal（CRITICAL）** | 要求 LLM 列出本会话所有 calc 结果和 counter 值 | 必须含 **56 / 9801 / 408 / 464 / 9973** |

---

### 执行规则

1. 按顺序执行，不得跳过或重排。
2. 每步检查 ABORT 条件；触发则立即停止并报告 `ABORTED at T<N>: <原因>`。
3. 每次 `/chat` 请求超时 120 秒。
4. 中断场景：被中断的轮次**后台**发出，中断触发请求（下一轮）**同步等待完成**并即时验证 SSE 输出。
5. 若 interrupted+tool 场景中 LLM 未调用工具（`tool_result` 未出现），agent 自行判断并记为 `SKIPPED`，继续下一步。
6. 服务器日志 `/tmp/kapi.log` 中可用 `[INTERRUPT]` 关键字确认中断触发。

---

### 中断触发时机说明

| 类型 | 何时发送下一轮请求 |
|------|--------------------|
| **cancelled** | T 轮请求发出后约 0.3s 内立即发，LLM 尚未响应 |
| **interrupted-text** | 监测 T 轮 SSE 流，出现第一个 `"type":"text"` 事件后立即发 |
| **interrupted+tool** | 监测 T 轮 SSE 流，出现 `"type":"tool_result"` 且随后出现 `"type":"text"`（LLM step-2 开始）后立即发 |

---

### 每轮验证要点

每轮（包括中断触发轮）结束后即时检查：

1. **SSE 终止事件**：含 `"type":"done"` → 正常完成；含 `"type":"error"` → 需检查原因
2. **SSE 文本内容**：中断触发轮的回复是否含预期锚点数值
3. **store 状态**（可选，辅助确认）：`GET /sessions/$SESS/messages` 查看 assistant `status` 字段

---

### STEP H 最终断言（T10 后执行）

查询 `GET /sessions/$SESS/messages`，对 assistant 消息序列验证：

| 断言 | 期望 |
|------|------|
| `cancelled` 出现次数 | ≥ 2 |
| `interrupted` 且含 tool part | ≥ 1 次 |
| `interrupted` 且含 text part | ≥ 1 次 |
| `status=''`（正常完成）出现次数 | ≥ 6 |
| 最后一轮 assistant status | `''`（正常完成） |

**CRITICAL：** T10 的 SSE 文本同时含 `56`、`9801`、`408`、`464`、`9973` → PASS

---

### STEP H 测试报告模板

```
=== INTERRUPT-TEST STEP H REPORT ===
Date    : <date>
Session : <SESS>

T1  normal  calc(7*8)=56                         : PASS / FAIL
T2  cancelled + T2-int recalled 56               : PASS / FAIL
T3  normal  counter anchor=9973                  : PASS / FAIL
T4  interrupted+tool + T4-int recalled 9801      : PASS / FAIL / SKIPPED
T5  normal  calc(12*34)=408                      : PASS / FAIL
T6  interrupted-text + T6-int recalled 9973      : PASS / FAIL
T7  cancelled + T7-int recalled 56+408           : PASS / FAIL
T8  interrupted+tool + T8-int recalled 464       : PASS / FAIL / SKIPPED
T9  interrupted-text + T9-int recalled 56/9973/408 : PASS / FAIL
T10 CRITICAL: recalled 56/9801/408/464/9973      : PASS / FAIL

Structure (final store):
  cancelled × 2              : PASS / FAIL
  interrupted+tool × 1       : PASS / FAIL
  interrupted+text × 1       : PASS / FAIL
  normal ≥ 6                 : PASS / FAIL
  last turn normal            : PASS / FAIL

VERDICT: PASS / FAIL / ABORTED at T<N>
```

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

STEP H — HTTP 对话式中断 E2E:
  T1  normal calc(7*8)=56                      : PASS / FAIL
  T2  cancelled + T2-int recalled 56           : PASS / FAIL
  T3  normal counter anchor=9973               : PASS / FAIL
  T4  interrupted+tool + T4-int recalled 9801  : PASS / FAIL / SKIPPED
  T5  normal calc(12*34)=408                   : PASS / FAIL
  T6  interrupted-text + T6-int recalled 9973  : PASS / FAIL
  T7  cancelled + T7-int recalled 56+408       : PASS / FAIL
  T8  interrupted+tool + T8-int recalled 464   : PASS / FAIL / SKIPPED
  T9  interrupted-text + T9-int 56/9973/408    : PASS / FAIL
  T10 CRITICAL: recalled 56/9801/408/464/9973  : PASS / FAIL
  Structure: cancelled×2 / int+tool×1 / int+text×1 / normal≥6 / last-normal : PASS / FAIL

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
| `handleSession` response missing `status` field | Added `Status string` to `msgSummary` in `cmd/knowledge-api/main.go` | — |
| `handleSession` response missing `Interrupted=true` flag on tool parts | Added `Interrupted=true` suffix to tool part summary | — |
| `handleChat` used synchronous `session.RunLoop` — impossible to cancel in-flight turn via second `/chat` | Replaced with `RunLoopAsync`; added `activeHandle *session.RunHandle` to `chatSession` | — |
| **`" "` space placeholder injected as assistant content after cancelled/error turns — proxies reject whitespace-only assistant content (400/EOF)** | **Root fix: pre-filter cancelled/error user+assistant pairs before building model messages in `ToModelMessages`. `cancelled` and `error` (no content) pairs are dropped entirely, never entering the alternating-role logic that inserts `" "`.** | — |
| LLM transport error left assistant `Message.Error=nil` — errored message looked like normal empty turn, consecutive-user guard kept inserting `" "` on every retry | Set `Message.Error` in `runLoopInternal` when `Process()` returns error and `ctx.Err()==nil` | — |

---

## " " Placeholder Bug (proxy rejects whitespace-only assistant content)

**症状：** 同一 session 内，cancel 或 LLM 错误之后，后续所有请求持续返回
`unexpected EOF`（实际是代理返回 400/422，SDK 解析成 EOF）。

**诊断过程：**
1. 添加 HTTP middleware 打印 request body → 发现 `messages` 里有 `{"role":"assistant","content":[{"text":" "}]}`
2. 代理端验证：纯空白 assistant content 被拒绝

**根本原因：**

`ToModelMessages` 在 `cancelled` assistant 被跳过后，两个相邻 user 消息触发
consecutive-role guard，插入 `" "` 占位符。这是为了满足 Anthropic/OpenAI 的
alternating-role 协议。但 `" "` 本身被代理拒绝，导致下一轮失败，再产生新的
errored assistant，再次触发 guard 插入新的 `" "`，形成自我强化的失败循环。

**修复（源头，非后处理）：**

在 `ToModelMessages` 入口处做一次预过滤：

```go
// Pre-filter: remove user+assistant pairs where assistant never produced content.
filtered := make([]*store.Message, 0, len(msgs))
for i := 0; i < len(msgs); i++ {
    m := msgs[i]
    if m.Role == store.RoleAssistant {
        ps := parts[m.ID]
        drop := m.Status == store.MessageStatusCancelled ||
                (m.Error != nil && !hasRealContent(ps))
        if drop {
            if len(filtered) > 0 && filtered[len(filtered)-1].Role == store.RoleUser {
                filtered = filtered[:len(filtered)-1]  // drop preceding user too
            }
            continue
        }
    }
    filtered = append(filtered, m)
}
```

这样 `cancelled`/`error` 的 user+assistant 对在进入 alternating-role 逻辑之前
就已移除，guard 永远看不到相邻 user 消息，`" "` 不会被插入。

**`" "` 保留的场景：** `interrupted` 且 `buildAssistantPartsInterrupted` 返回空
（所有 tool 均不完整）时，guard 仍可能插入 `" "`。但这种情况代理是否拒绝未验证；
若需要，可对此路径也做预过滤。

---

## Conversational Interrupt via /chat (对话式中断)

### 机制

`handleChat` 现在使用 `RunLoopAsync`，并在 `chatSession` 里存储当前的 `RunHandle`。
每次新的 `/chat` 请求进来时，若同一 session 有正在运行的 handle，先调用 `prev.Cancel()`
再启动新轮。这实现了**对话式中断**：用户发新消息即可中止上一轮正在进行的 LLM turn。

```
第一轮 /chat → RunLoopAsync → handle A → sess.activeHandle = A
                              LLM 流式输出中...
第二轮 /chat → Cancel(A) → RunLoopAsync → handle B → sess.activeHandle = B
              ↑ [INTERRUPT] 日志
              第一轮 → status='interrupted'（store）
```

### 关键日志锚点

服务器日志（`/tmp/kapi.log`）中观察：

```
[TURN-START] session=<id> handle=0x...      ← 第一轮开始
[INTERRUPT]  session=<id> new message arrived — cancelling previous turn  ← Cancel 触发
[TURN-START] session=<id> handle=0x...      ← 第二轮开始（新 handle）
[TURN-DONE]  session=<id> handle=0x... result=stop err=llm error [transport]: context canceled  ← 第一轮结束
[TURN-DONE]  session=<id> handle=0x... result=... err=<nil>  ← 第二轮结束
```

### 可中断的场景

| 场景 | 是否可对话式中断 | 说明 |
|------|-----------------|------|
| LLM 正在流式输出文字 | ✓ **可以** | Cancel 取消 LLM stream，`status=interrupted` |
| LLM 正在等待 tool 执行 | ✓ **可以** | Cancel → `toolCancel()` → 工具被截断，`status=interrupted` |
| Tool 执行耗时 < 250ms | ✓ 工具完成，LLM 下一步被取消 | |
| Tool 执行耗时 > 250ms | ✓ 工具被 cleanup 250ms 超时截断，`Interrupted=true` | |

### 无法通过 HTTP disconnect 中断

`handleChat` 使用 `context.WithoutCancel(r.Context())`，HTTP 客户端断开连接**不会**
取消 RunLoop。只有发送新的 `/chat` 消息（触发 `prev.Cancel()`）才能中断。

### long_task 工具的行为说明

`long_task`（30 秒阻塞）被 LLM 调用后，LLM stream 在发出 `tool_call` 事件后就结束
（`FinishReason=tool_calls`），随即 `cleanup()` 调用 `toolCancel()`，工具在 250ms
宽限期内被截断——**这发生在第二轮消息进来之前，不是对话式中断触发的**。

对话式中断触发的是 **LLM 流式输出阶段**（在 tool_call 之前，或在收到 tool result 后
LLM 开始第二步输出时）。

### 验证脚本

```bash
SESS="itest-interrupt-$(date +%s)"

# 第一轮后台：LLM 写长文（不调工具）
curl -sN --max-time 120 -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d "{\"message\":\"Write a very long essay (500+ words) about computing history.\",
       \"session_id\":\"$SESS\",\"tools\":[],\"max_steps\":1}" \
  > /tmp/turn1_sse.txt 2>&1 &
TURN1_PID=$!

# 等 LLM 开始输出（第一个 text delta 出现）
for i in $(seq 1 10); do
  sleep 1
  grep -q '"type":"text"' /tmp/turn1_sse.txt && { echo "LLM started at T+${i}s"; break; }
done

# 第二轮：触发中断
curl -sN --max-time 30 -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d "{\"message\":\"Stop. Just say OK.\",\"session_id\":\"$SESS\",\"tools\":[],\"max_steps\":1}"

wait $TURN1_PID 2>/dev/null

# 验证日志
grep -E "INTERRUPT|TURN-DONE" /tmp/kapi.log | tail -4

# 验证 store 状态
curl -s "http://127.0.0.1:7700/sessions/$SESS/messages" | python3 -c "
import json, sys
d = json.load(sys.stdin)
for m in d['messages']:
    if m['role'] == 'assistant':
        status = m.get('status', '')
        print(f'assistant status={status!r}')
        # 期望第一轮: interrupted, 第二轮: ''
"
```

**期望结果：**
- 服务器日志出现 `[INTERRUPT]`
- 第一轮 assistant `status='interrupted'`
- 第二轮 assistant `status=''`（正常完成）或第二轮本身也因 LLM 问题失败

---

## Lessons Learned (from test run)

### macOS: `timeout` 命令不可用

macOS 上没有 GNU `timeout` 命令，用 `curl --max-time N` 代替。

### SSE `"type":"done"` 不等于 LLM 内容正确

SSE 流以 `done` 结束只说明 RunLoop 正常退出，不代表 LLM 成功响应了内容。
LLM 调用因 401/网络错误失败时，RunLoop 同样以 `done` 正常结束（错误通过 `"type":"error"` 事件上报）。
验证 LLM 确实响应时，还需检查文本内容或 tool 调用是否出现。

### 代理服务器 X-Api-Key vs Authorization: Bearer

`provider/anthropic` 使用 `option.WithAPIKey` → header 为 `X-Api-Key: <key>`。
部分反向代理只识别 `Authorization: Bearer <key>`，会对 `X-Api-Key` 返回 401。
若遇到此问题应修复代理配置，不要修改 SDK 的 header 策略。

### cleanup 250ms 超时与 interrupted+tool

cleanup 在 250ms 宽限期后将仍在运行的工具标记为 `status=error, Interrupted=true`。
这是正确行为（保护 RunLoop 不被慢工具拖死），与 HTTP disconnect 无关。
区分方式：`message.status=''` 说明 RunLoop 正常完成，tool 被截断是 cleanup 行为而非中断。
```

---
