---
name: llm-go-test
description: Automated integration testing of github.com/larryhou/llm-go via the knowledge-api /chat HTTP interface — covers all major design features including multi-turn sessions, tool execution, tool failure, doom-loop detection, max_steps, context compaction, async tools, SSE event types, and knowledge retrieval
---

# LLM-Go Integration Test Skill

This skill describes how to run **automated end-to-end tests** for
`github.com/larryhou/llm-go` through the `knowledge-api` HTTP server.
Tests exercise the system as a black box — no internal Go test harness needed —
by POSTing to `/chat` and asserting on the SSE event stream.

---

## Architecture Under Test

```
test script
    │  POST /chat  {message, session_id, context_limit, max_steps, tools}
    ▼
knowledge-api  (:7700)
    │  session.RunLoop
    ▼
┌─────────────────────────────────────────────────────┐
│  session/prompt.go  RunLoop                         │
│    ├── store/memory  (session + message + part CRUD)│
│    ├── session/processor.go  (streaming turn)       │
│    │     ├── llm/client.go  (retry wrapper)         │
│    │     ├── provider/openai  (SSE streaming)       │
│    │     ├── tool execution  (async goroutines)     │
│    │     ├── doom-loop detection                    │
│    │     └── overflow → ProcessCompact              │
│    ├── session/compaction.go  Compact / Prune       │
│    └── knowledge/manager.go  knowledge_search/fetch │
└─────────────────────────────────────────────────────┘
    │  SSE event stream
    ▼
sseProvider / sseToolWrapper  (in knowledge-api)
    │  data: {"type":"text"|"tool_call"|"tool_result"|"done"|"error", ...}
    ▼
test script assertions
```

---

## Prerequisites

### 1. Build and start knowledge-api

```bash
cd /path/to/llm-go

# Build
go build ./cmd/knowledge-api/

# Start — index the skills dir; listen on :7700
# Default provider is Anthropic. To use OpenAI, pass -provider openai
go run ./cmd/knowledge-api/ \
  -skills /path/to/.opencode/skills \
  -addr   127.0.0.1:7700 \
  -provider anthropic

# Optional: Override the default provider endpoint or key
#   -llm-url "http://custom-endpoint/claude"
#   -llm-key "sk-custom-key"

# Verify
curl -s http://127.0.0.1:7700/health
# → {"status":"ok","doc_count":2,"session_count":0}
```

### 2. Required test tools in knowledge-api

The server must expose these tools (implemented in `buildTestTools()`):

| Tool | Purpose |
|------|---------|
| `calc` | Arithmetic — exercises normal tool execution path |
| `slow_calc` | 2-second delayed calc — exercises async tool goroutine |
| `counter` | Stateful named counter — exercises multi-turn state accumulation |
| `tool_failure` | Always returns `tool.Fail(...)` — exercises recoverable error path |
| `doom_bait` | Echo tool — can be used to trigger doom-loop (3× same call) |

These are defined in `cmd/knowledge-api/main.go:buildTestTools()`.

### 3. Test script location

```
llm-go/cmd/knowledge-api/test_features.sh
```

---

## Running the Tests

```bash
# Option A — run directly
bash llm-go/cmd/knowledge-api/test_features.sh

# Option B — with custom server address
BASE=http://127.0.0.1:7700 bash test_features.sh

# Option C — with output to file for analysis
bash test_features.sh > /tmp/test_out.txt 2>&1
grep -E "✓|✗|===" /tmp/test_out.txt
```

Expected final output:
```
=== Summary ===
  Passed: 32 / 32
  ALL TESTS PASSED
```

---

## Test Catalogue

### [0] Health Check
**What it tests:** Server liveness and index readiness.

```bash
curl -s http://127.0.0.1:7700/health
# → {"status":"ok","doc_count":N,"session_count":N}
```

**Asserts:** `status == "ok"`. Exits immediately if server is not running.

---

### [1] Normal Tool Call
**What it tests:**
- `processor.go`: `EventToolInputStart` → `EventToolCall` → `go executeTool` → result
- `sseProvider`: `tool_call` SSE event forwarding
- `sseToolWrapper`: `tool_result` SSE event forwarding
- `done` terminal event

```bash
curl -sN -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d '{"message":"Use the calc tool to compute 999 * 888.", "tools":["calc"]}'
```

**Asserts:**
- SSE contains `{"type":"tool_call","tool":"calc",...}`
- SSE contains `{"type":"tool_result","tool":"calc","output":"887112"}`
- Response text mentions `887112`
- SSE contains `{"type":"done",...}`

---

### [2] Multi-Turn Session
**What it tests:**
- `session_id` reuse across HTTP requests
- `store/memory`: message and part persistence across turns
- `context.go:ToModelMessages`: history reconstruction from store
- `prompt.go:RunLoop`: `FilterCompacted` + message loading on each turn

```bash
# Turn 1 — new session, get session_id from X-Session-ID header
RESP=$(curl -sN -X POST .../chat -d '{"message":"Remember the number 42."}')
SID=$(echo "$RESP" | grep '"session_id"' | sed 's/.*"session_id":"\([^"]*\)".*/\1/')

# Turn 2 — same session
curl -sN -X POST .../chat \
  -d "{\"message\":\"What number did I tell you?\",\"session_id\":\"$SID\"}"
```

**Asserts:** Second turn response contains `42`.

---

### [3] Knowledge Search + Fetch
**What it tests:**
- `knowledge/manager.go`: priority-grouped dispatch, `peek()`, `fetch()`
- `knowledge/source/bleve/bleve.go`: full-text search, document retrieval
- `knowledge_search` and `knowledge_fetch` tool execution
- Two-level retrieval: LLM searches → decides to fetch full doc

```bash
curl -sN -X POST .../chat -d '{
  "message": "Use knowledge_search to find the Source interface, then knowledge_fetch for full content.",
  "tools": ["knowledge_search","knowledge_fetch"]
}'
```

**Asserts:**
- At least 1 `tool_call` event
- `knowledge_search` tool name appears in SSE
- Response mentions `Peek`, `snippet`, or retrieval concepts

---

### [4] Tool Failure (Recoverable Error)
**What it tests:**
- `tool.Fail(msg)` → `ToolFailure` error type
- `processor.go:executeTool`: `tool.IsToolFailure(err)` → error tool result (no crash)
- `sseToolWrapper`: emits `tool_result` with `error:true` flag
- Session continues to `done` after tool failure
- LLM receives the error as a tool result and reports it to the user

```bash
curl -sN -X POST .../chat -d '{
  "message": "Call the tool_failure tool and tell me the error.",
  "tools": ["tool_failure"]
}'
```

**Asserts:**
- SSE contains `tool_call` for `tool_failure`
- SSE does **not** contain `{"type":"error"}` (session not crashed)
- SSE contains `{"type":"done"}`
- Response text mentions the failure

---

### [5] max_steps Enforcement
**What it tests:**
- `prompt.go:RunLoop`: `if input.MaxSteps > 0 && step >= input.MaxSteps { return }`
- Session terminates cleanly when step limit reached
- `/chat` request parameter `max_steps` override

```bash
curl -sN -X POST .../chat -d '{
  "message": "Use calc for 2+2, then 3+3, then 4+4.",
  "tools": ["calc"],
  "max_steps": 2
}'
```

**Asserts:**
- Number of `tool_call` events ≤ 2
- SSE contains `{"type":"done"}`

---

### [6] Stateful Counter + Multi-Turn Accumulation
**What it tests:**
- Tool with server-side state (`counter` increments persist across calls)
- Multi-turn session state: second `/chat` call reuses `session_id`
- `ToModelMessages` correctly reconstructs prior tool results in history

```bash
# Turn 1
RESP=$(curl -sN ... -d '{"message":"Increment counter score by 10, then by 5.","tools":["counter"]}')
SID=$(session_id_from "$RESP")

# Turn 2
curl -sN ... -d "{\"message\":\"Increment score by 100.\",\"session_id\":\"$SID\",\"tools\":[\"counter\"]}"
```

**Asserts:**
- Turn 1: `counter` tool called ≥ 1 time; response mentions a numeric value
- Turn 2: tool called or value mentioned

---

### [7] Context Compaction
**What it tests:**
- `llm/overflow.go:IsOverflow`: detects when `usage.Effective() >= Usable(model)`
- `processor.go`: `EventStepFinish` → `IsOverflow` → `ProcessCompact`
- `compaction.go:Compact`: head/tail split, summary generation, boundary insertion
- `compaction.go:Select`: tail-turn selection within `PreserveRecentBudget`
- `context.go:FilterCompacted`: post-compaction view of message history
- `/chat` request parameter `context_limit` override

```bash
SESS="compact-test-$$"
for msg in \
  "Increment counter x by 10. Use calc for 100*200." \
  "Increment x by 20. Use calc for 300*400." \
  "Increment x by 30. Use calc for 500*600." \
  "Summarise counter x history." \
  "Increment x by 1 and compute 999+1."; do
  curl -sN -X POST .../chat -d \
    "{\"message\":\"$msg\",\"session_id\":\"$SESS\",\"context_limit\":8000,\"tools\":[\"counter\",\"calc\"]}"
done
```

**Asserts:**
- All 5 turns complete with `done` or `error` terminal event
- `/sessions/{id}/messages` returns ≥ 5 messages
- If `context_limit` was small enough: compaction boundary (`"compaction boundary"`) appears in session messages

**Note on `context_limit`:**
- `3000` → fires after turn 1 (before enough history to summarise → `"nothing to summarise"` error)
- `8000` → fires after 4–5 tool-heavy turns (reliable compaction trigger)
- `200000` → never fires (normal mode)

---

### [8] Async Tool Execution
**What it tests:**
- `processor.go:executeTool`: runs in goroutine (`go executeTool(...)`)
- 250ms grace period in `cleanup()` for in-flight tools
- `slow_calc` tool: 2-second `time.Sleep` before returning

```bash
time curl -sN -X POST .../chat -d '{
  "message": "Use slow_calc to compute 7 * 8.",
  "tools": ["slow_calc"]
}'
```

**Asserts:**
- SSE contains `tool_call` for `slow_calc`
- Response mentions `56`
- Wall time ≥ 2s

---

### [9] SSE Event Type Coverage
**What it tests:** All SSE event types emitted by `sseProvider` and `sseToolWrapper`.

| Event type | Source | When |
|------------|--------|------|
| `text` | `sseProvider` | `EventTextDelta` from provider stream |
| `tool_call` | `sseProvider` | `EventToolCall` from provider stream |
| `tool_result` | `sseToolWrapper` | After `tool.Execute()` completes |
| `done` | `handleChat` | After `RunLoop` returns |
| `error` | `handleChat` | When `RunLoop` returns non-nil error |

**Asserts:** All of `text`, `tool_call`, `tool_result`, `done` observed across test responses.

---

### [10] Session Message Inspection
**What it tests:**
- `GET /sessions/{id}/messages` endpoint
- `store.Store.ListMessages` + `ListParts`
- Part type rendering: `TextPartData`, `ToolPartData`, `StepFinishData`, `CompactionPartData`

```bash
curl -s http://127.0.0.1:7700/sessions/$SID/messages
```

**Asserts:**
- Response JSON contains `session_id`, `message_count`, `messages`
- Messages include `role:"user"` and `role:"assistant"`
- At least one part with `tool=<name>` (ToolPartData)

---

## Feature Coverage Matrix

| Feature | Code Location | Test |
|---------|--------------|------|
| Normal streaming turn | `session/processor.go` | [1] |
| Tool async execution | `processor.go:executeTool` | [1][8] |
| ToolFailure (recoverable) | `tool/tool.go`, `processor.go` | [4] |
| Doom-loop detection | `processor.go:checkDoomLoop` | (manual: call doom_bait 3×) |
| Multi-turn memory | `context.go:ToModelMessages` | [2][6] |
| FilterCompacted | `context.go:FilterCompacted` | [7] |
| Context overflow detection | `llm/overflow.go:IsOverflow` | [7] |
| Context compaction | `session/compaction.go:Compact` | [7] |
| SummaryProvider separation | `compaction.go`, `handleChat` | [7] |
| MaxSteps guard | `prompt.go:RunLoop` | [5] |
| Knowledge search (Peek) | `knowledge/manager.go` | [3] |
| Knowledge fetch | `knowledge/manager.go` | [3] |
| Bleve full-text search | `knowledge/source/bleve` | [3] |
| SSE text event | `sseProvider` | [9] |
| SSE tool_call event | `sseProvider` | [1][9] |
| SSE tool_result event | `sseToolWrapper` | [1][9] |
| SSE done event | `handleChat` | [9] |
| SSE error event | `handleChat` | (compaction edge case) |
| session_id reuse | `handleChat`, `store/memory` | [2][6][7] |
| Store CRUD | `store/memory/memory.go` | [10] |
| /sessions inspection | `handleSession` | [10] |
| context_limit override | `handleChat` | [7] |
| max_steps override | `handleChat` | [5] |
| tools subset filter | `handleChat` | all tests |
| Concurrent tool dispatch | `processor.go` | [7][8] |

---

## Doom-Loop Manual Test

Doom-loop detection (`DoomLoopThreshold = 3`) fires when the **same tool
with identical input** is called 3 times consecutively. To trigger:

```bash
curl -sN -X POST .../chat -d '{
  "message": "Call doom_bait with value=hello. Then call it again with value=hello. Then again with value=hello. Keep repeating until I tell you to stop.",
  "tools": ["doom_bait"],
  "max_steps": 20
}'
```

**Expected:** Session ends with `{"type":"error","error":"...doom loop..."}` before step 20.

**Code path:**
```
processor.go:handleEvent(EventToolCall)
  → checkDoomLoop(toolName, inputKey)
    → recentCalls sliding window full + all identical
  → updateToolStatus(..., "Doom loop detected...")
  → return ProcessStop
```

---

## Compaction Deep Dive

### When compaction fires

```
RunLoop step N:
  processor.Process(...)
    → EventStepFinish arrives
    → llm.IsOverflow(usage, model{Limit:{Context:contextLimit}}, cfg)
      = usage.Effective() >= Usable(model, cfg)
      = usage.Effective() >= contextLimit - CompactionBuffer(20000)
    → return ProcessCompact
  → compactor.Compact(sessionID, input)
    → Select(msgs) → head (to summarise) + tail (to keep verbatim)
    → LLM summary call with SummaryProvider (no SSE middleware)
    → insert CompactionPartData boundary message
    → insert Summary=true assistant message
  → RunLoop continues from new context
```

### context_limit tuning

| Value | Behavior |
|-------|----------|
| `3000` | Fires on turn 1 (too small to accumulate; summary fails: "nothing to summarise") |
| `8000` | Fires after 4–5 tool-heavy turns (reliable for integration testing) |
| `15000` | Fires after 8–12 normal turns |
| `200000` | Never fires (production mode) |

### FilterCompacted output structure

After compaction the store contains:
```
[compaction_user_msg]   ← role=user, parts=[{type:"compaction"}]
[summary_assistant_msg] ← role=assistant, Summary=true
[turn_N_user_msg]
[turn_N_assistant_msg]
...
```

`FilterCompacted` returns only the last compaction boundary onward.
Turns before the boundary are invisible to subsequent LLM calls.

---

## Adding New Tests

### Test template

```bash
bold "=== [N] My new test ==="
body='{"message":"...","tools":["calc"]}'
resp=$(chat_once "$body")

if contains "$resp" '"type":"tool_call"'; then
  pass "tool_call emitted"
else
  fail "no tool_call"
fi
if contains "$resp" "expected_output"; then
  pass "correct output"
else
  fail "wrong output"
fi
```

### Helper functions

```bash
chat_once '{"message":"...","tools":["calc"]}'     # POST /chat, return SSE body
session_id_from "$resp"                             # extract session_id from SSE
count_events "$resp" "tool_call"                    # count events of a type
contains "$resp" "pattern"                          # grep -q (returns 0/1)
pass "description"                                  # green ✓, increment PASS
fail "description"                                  # red ✗, increment FAIL
```

### Adding a new test tool to knowledge-api

In `cmd/knowledge-api/main.go:buildTestTools()`:

```go
&simpleTool{
    name:        "my_tool",
    description: "What it does — be specific for the LLM",
    schema: map[string]any{
        "type":     "object",
        "required": []string{"input"},
        "properties": map[string]any{
            "input": map[string]any{"type": "string"},
        },
    },
    fn: func(ctx context.Context, input map[string]any) (tool.Result, error) {
        v, _ := input["input"].(string)
        // return tool.Fail("...") for recoverable errors
        return tool.Result{Output: v, Title: "my_tool"}, nil
    },
},
```

---

## Maintenance Checklist

When changing `llm-go` internals, update tests as follows:

| Change | Affected tests | Action |
|--------|---------------|--------|
| New event type in `llm/event.go` | [9] | Add SSE forwarding in `sseProvider`; add assertion |
| New `ProcessResult` value | [1]–[8] | Handle in `handleChat` sendEvent logic |
| Change `DoomLoopThreshold` | doom-loop manual test | Update call count in test prompt |
| Change `DefaultTailTurns` | [7] | May need more/fewer compaction turns |
| Change `CompactionBuffer` | [7] | Adjust `context_limit` value |
| New store `PartType` | [10] | Add rendering in `handleSession` |
| New `/chat` request field | — | Add to `handleChat` req struct and test |
| New tool added to `buildTestTools()` | — | Add dedicated test section |

---

## Common Failures and Fixes

| Failure | Cause | Fix |
|---------|-------|-----|
| `"nothing to summarise"` error in compaction | `context_limit` too small (fires on turn 1) | Increase to `8000`+ |
| `tool_result` SSE events missing | `sseToolWrapper` not wrapping tools | Ensure `wrappedTools` loop in `handleChat` |
| Session not recalled in second turn | Wrong `session_id` sent | Extract from `X-Session-ID` header or `done` event |
| Doom-loop not firing | LLM varies the input slightly | Use explicit `"always use value=hello"` wording |
| `set -e` causes early exit | `grep -q` returns 1 on miss in pipefail mode | Use `set -uo pipefail` (not `-e`) in test script |
| `\|\|` short-circuit fails under pipefail | `contains A \|\| contains B` with `grep -q` | Use `grep -c ... \|\| true` and compare with `[ N -ge 1 ]` |
| SSE output leaks into stdout | `compact_turn` calls before `$(...)` capture | Redirect discarded calls to `/dev/null` |
| calc result format mismatch | LLM formats `887,112` not `887112` | Use regex: `887[,. ]?112` |
