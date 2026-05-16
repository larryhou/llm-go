---
name: llm-test
description: Automated integration testing of the llm-go llm-api /chat HTTP interface — covers all major design features including multi-turn sessions, tool execution, tool failure, doom-loop detection, max_steps, context compaction, async tools, SSE event types, and knowledge retrieval
---

# LLM Integration Test Skill

End-to-end tests for the `llm-api` HTTP server. The system is treated as
a black box — tests POST to `/chat` and assert on the SSE event stream and
`/sessions` state.

---

## Key Files

| Item | Path |
|------|------|
| llm-api source | `cmd/llm-api/main.go` |
| Test script | `cmd/llm-api/test_features.sh` |
| Skills dir | `.opencode/skills` |

---

## Architecture Under Test

```
test script / test_features.sh
    │  POST /chat  {message, session_id, context_limit, max_steps, tools}
    ▼
llm-api  (127.0.0.1:7700)
    │  session.RunLoop  (context.WithoutCancel — disconnect-safe)
    ▼
┌─────────────────────────────────────────────────────┐
│  session/prompt.go      RunLoop                     │
│    ├── store/memory     (session + message + part)  │
│    ├── session/processor.go  (streaming turn)       │
│    │     ├── llm/client.go   (retry wrapper)        │
│    │     ├── provider/openai (SSE streaming)        │
│    │     ├── tool execution  (async goroutines)     │
│    │     ├── doom-loop detection (threshold=3)      │
│    │     └── overflow → ProcessCompact              │
│    ├── session/compaction.go  Compact / Prune       │
│    └── knowledge/manager.go  knowledge_search/fetch │
└─────────────────────────────────────────────────────┘
    │  SSE event stream
    ▼
sseProvider / sseToolWrapper
    │  data: {"type":"text"|"tool_call"|"tool_result"|"done"|"error", ...}
    ▼
test assertions
```

---

## Server

The server listens on `http://127.0.0.1:7700` by default. Start if not running:

```bash
# LLM connection — resolved in this order:
#   1. Environment variables (preferred):
#        TIMI_PROVIDER  — "openai" or "anthropic"  (default: anthropic)
#        TIMI_BASE_URL  — provider base URL
#        TIMI_API_KEY   — API key
#        TIMI_MODEL     — model ID                  (default: claude-sonnet-4.6)
#   2. Hardcoded defaults in cmd/llm-api/main.go (flag.StringVar lines)
#   3. If still unresolved (e.g. main.go defaults unavailable), ask the user.

lsof -ti:7700 | xargs kill -9 2>/dev/null; sleep 1
nohup go run ./cmd/llm-api/ \
  -skills .opencode -addr 127.0.0.1:7700 \
  > /tmp/kapi.log 2>&1 &
sleep 6 && curl -s http://127.0.0.1:7700/health
```

Healthy response: `{"status":"ok","doc_count":N,"session_count":0}`

**ABORT if:** response does not contain `"status":"ok"`.

---

## Test Tools

Defined in `cmd/llm-api/main.go:buildTestTools()`:

| Tool | Description | Tests |
|------|-------------|-------|
| `calc` | Arithmetic expression evaluator | [1][5][7] |
| `slow_calc` | Same as calc but sleeps 2 s | [8] |
| `counter` | Stateful named counter (persists within server process) | [6][7] |
| `tool_failure` | Always returns `tool.Fail(...)` — recoverable error | [4] |
| `doom_bait` | Echoes input unchanged — triggers doom-loop after 3 identical calls | doom-loop test |

---

## Running the Tests

```bash
# Run all tests (requires server running on :7700)
bash cmd/llm-api/test_features.sh

# With custom server address
BASE=http://127.0.0.1:7700 bash cmd/llm-api/test_features.sh

# Capture output for analysis
bash cmd/llm-api/test_features.sh > /tmp/test_out.txt 2>&1
grep -E "✓|✗|===" /tmp/test_out.txt
```

Expected final output:
```
=== Summary ===
  Passed: 32 / 32
  ALL TESTS PASSED
```

**Note:** All `/chat` calls in `test_features.sh` use `curl -sN` (`--no-buffer`)
to prevent SSE output buffering. The `dc_turn` helper in test [11] uses
`curl -s` (without `-N`) — this is an open issue: if the SSE `done` event is
not captured, the turn may be judged as missing a terminal event even though the
server completed the request. Use `GET /sessions/{id}/messages` to verify actual
server state if terminal event checks fail.

---

## Test Catalogue

### [0] Health Check

Asserts `"status":"ok"`. Hard exit if server is down.

```bash
curl -s http://127.0.0.1:7700/health
```

---

### [1] Normal Tool Call

**Code path:** `processor.go:EventToolCall` → `go executeTool` →
`sseProvider:tool_call` → `sseToolWrapper:tool_result`

```bash
curl -sN -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d '{"message":"Use the calc tool to compute 999 * 888.","tools":["calc"]}'
```

**Asserts:** `tool_call` event; `tool_result` with `887112`; `done` event.

---

### [2] Multi-Turn Session

**Code path:** `session_id` reuse → `store/memory` persistence →
`context.go:ToModelMessages` history reconstruction

```bash
# Turn 1 — no session_id → server creates one
resp=$(curl -sN -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d '{"message":"Remember the secret number 42."}')
SID=$(echo "$resp" | grep '"session_id"' | sed 's/.*"session_id":"\([^"]*\)".*/\1/' | head -1)

# Turn 2 — reuse session
curl -sN -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d "{\"message\":\"What was the secret number?\",\"session_id\":\"$SID\"}"
```

**Asserts:** Second turn response contains `42`.

---

### [3] Knowledge Search + Fetch

**Code path:** `knowledge/manager.go` → `peek()` → `fetch()` →
`knowledge/source/bleve` full-text search

```bash
curl -sN -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d '{"message":"Use knowledge_search to find the Source interface, then knowledge_fetch for full content.","tools":["knowledge_search","knowledge_fetch"]}'
```

**Asserts:** ≥1 `tool_call`; `knowledge_search` invoked; response mentions
`Peek`, `fetch`, or retrieval concepts.

---

### [4] Tool Failure (Recoverable)

**Code path:** `tool.Fail(msg)` → `tool.IsToolFailure(err)` in `processor.go`
→ error tool result (no crash) → `sseToolWrapper` emits `tool_result` with
`error:true` → session continues to `done`

```bash
curl -sN -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d '{"message":"Call the tool_failure tool and tell me the error.","tools":["tool_failure"]}'
```

**Asserts:** `tool_call` for `tool_failure`; **no** `"type":"error"` event;
`"type":"done"` event; response mentions the failure.

---

### [5] max_steps Enforcement

**Code path:** `prompt.go:RunLoop` `if step >= input.MaxSteps { return }`

```bash
curl -sN -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d '{"message":"Use calc for 2+2, then 3+3, then 4+4.","tools":["calc"],"max_steps":2}'
```

**Asserts:** `tool_call` count ≤ 2; `"type":"done"` event.

---

### [6] Stateful Counter + Multi-Turn Accumulation

**Code path:** tool with in-process mutable state; second turn reconstructs
prior tool results via `ToModelMessages`

```bash
# Turn 1
resp=$(curl -sN -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d '{"message":"Increment counter \"score\" by 10, then by 5. Tell me the final value.","tools":["counter"]}')
SID=$(echo "$resp" | grep '"session_id"' | sed 's/.*"session_id":"\([^"]*\)".*/\1/' | head -1)

# Turn 2
curl -sN -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d "{\"message\":\"Now increment score by 100.\",\"session_id\":\"$SID\",\"tools\":[\"counter\"]}"
```

**Asserts:** Turn 1: `counter` called ≥1 time; Turn 2: tool called or
accumulated value mentioned.

---

### [7] Context Compaction

**Code path:** `processor.go:EventStepFinish` → `llm.IsOverflow` →
`ProcessCompact` → `compaction.go:Compact` → `Select` (head/tail split) →
summary LLM call → `CompactionPartData` boundary → `FilterCompacted`

```bash
SESS="compact-test-$$"
for msg in \
  "Increment counter x by 10. Use calc for 100*200." \
  "Increment x by 20. Use calc for 300*400." \
  "Increment x by 30. Use calc for 500*600." \
  "Summarise counter x history." \
  "Increment x by 1 and compute 999+1."; do
  curl -sN -X POST http://127.0.0.1:7700/chat \
    -H "Content-Type: application/json" \
    -d "{\"message\":\"$msg\",\"session_id\":\"$SESS\",\"context_limit\":8000,\"tools\":[\"counter\",\"calc\"]}"
done
```

**Asserts:** All 5 turns end with `done` or `error`; `GET /sessions/$SESS/messages`
returns ≥5 messages; if compaction triggered, `"compaction boundary"` appears
in session messages.

**`context_limit` calibration:**

| Value | Behaviour |
|-------|-----------|
| `5500` | Fires after turn 2–3 (used in compact-test and interrupt-test) |
| `8000` | Fires after 4–5 tool-heavy turns (reliable for this test) |
| `200000` | Never fires (production mode) |

For deep compaction testing see the **compact-test** skill.

---

### [8] Async Tool Execution

**Code path:** `processor.go:executeTool` runs in goroutine; `cleanup()` waits
up to 250 ms; `slow_calc` sleeps 2 s before returning

```bash
time curl -sN -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d '{"message":"Use slow_calc to compute 7 * 8.","tools":["slow_calc"]}'
```

**Asserts:** `tool_call` for `slow_calc`; result contains `56`; wall time ≥ 2 s.

---

### [9] SSE Event Type Coverage

Verifies all event types are observed across the test run:

| Event type | Source | When |
|------------|--------|------|
| `text` | `sseProvider` | `EventTextDelta` |
| `tool_call` | `sseProvider` | `EventToolCall` |
| `tool_result` | `sseToolWrapper` | After `tool.Execute()` |
| `done` | `handleChat` | RunLoop returns normally |
| `error` | `handleChat` | RunLoop returns non-nil error |

**Asserts:** `text`, `tool_call`, `tool_result`, `done` all observed across [1][3][4][8].

---

### [10] Session Message Inspection

**Code path:** `GET /sessions/{id}/messages` → `handleSession` →
`store.ListMessages` + `ListParts` → JSON rendering of all part types

```bash
curl -s http://127.0.0.1:7700/sessions/$SID/messages
```

**Asserts:** Response contains `session_id`, `message_count`, `messages`;
`role:"user"` and `role:"assistant"` both present; ≥1 part with `tool=<name>`.

---

### [11] Double Compaction — Topic Continuity

Tests `PartTypeRecentContext` (Issue J fix): after two compaction rounds the
LLM must still recall the anchor value `7531`.

This test is covered in detail by the **compact-test** skill. Here it runs
as a single automated pass within `test_features.sh` using `context_limit=5500`.

**Quick summary of what happens:**
1. Turn 1 plants `7531` as the API timeout anchor
2. Turns 2–3 accumulate tokens to trigger compaction twice
3. Final turn asks the LLM to recall `7531`

**Asserts:** Final response contains `7531`; ≥1 `"compaction boundary"` in
session store.

---

## Doom-Loop Manual Test

`DoomLoopThreshold = 3` (`session/processor.go:22`): same tool + same input
called 3 consecutive times → `ProcessStop` + error.

```bash
curl -sN -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d '{"message":"Call doom_bait with value=hello. Then again with value=hello. Then again with value=hello. Keep repeating.","tools":["doom_bait"],"max_steps":20}'
```

**Asserts:** Session ends with `{"type":"error","error":"...doom loop..."}` before step 20.

---

## Feature Coverage Matrix

| Feature | Code Location | Test |
|---------|--------------|------|
| Normal streaming turn | `session/processor.go` | [1] |
| Tool async execution | `processor.go:executeTool` | [1][8] |
| ToolFailure (recoverable) | `tool/tool.go`, `processor.go` | [4] |
| Doom-loop detection | `processor.go:checkDoomLoop` | doom-loop |
| Multi-turn memory | `context.go:ToModelMessages` | [2][6] |
| FilterCompacted | `context.go:FilterCompacted` | [7] |
| Context overflow detection | `llm/overflow.go:IsOverflow` | [7] |
| Context compaction | `session/compaction.go:Compact` | [7][11] |
| PartTypeRecentContext | `session/compaction.go` Step 6 | [11] |
| MaxSteps guard | `prompt.go:RunLoop` | [5] |
| Knowledge search (Peek) | `knowledge/manager.go` | [3] |
| Knowledge fetch | `knowledge/manager.go` | [3] |
| Bleve full-text search | `knowledge/source/bleve` | [3] |
| SSE text event | `sseProvider` | [9] |
| SSE tool_call event | `sseProvider` | [1][9] |
| SSE tool_result event | `sseToolWrapper` | [1][9] |
| SSE done event | `handleChat` | [9] |
| SSE error event | `handleChat` | doom-loop |
| context.WithoutCancel | `handleChat`, `compaction.go` | (see interrupt-test) |
| session_id reuse | `handleChat`, `store/memory` | [2][6][7] |
| Store CRUD | `store/memory/memory.go` | [10] |
| /sessions inspection | `handleSession` | [10] |
| context_limit override | `handleChat` | [7][11] |
| max_steps override | `handleChat` | [5] |
| tools subset filter | `handleChat` | all |
| Concurrent tool dispatch | `processor.go` | [7][8] |

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

### Shell helpers (defined at top of test_features.sh)

| Helper | Signature | Description |
|--------|-----------|-------------|
| `chat_once` | `chat_once <json-body>` | POST /chat, return full SSE body |
| `session_id_from` | `session_id_from "$resp"` | Extract `session_id` from SSE stream |
| `count_events` | `count_events "$resp" "tool_call"` | Count events of a given type |
| `contains` | `contains "$resp" "pattern"` | `grep -q` wrapper (returns 0/1) |
| `pass` | `pass "description"` | Green ✓, increment PASS counter |
| `fail` | `fail "description"` | Red ✗, increment FAIL counter |

### Adding a new test tool

In `cmd/llm-api/main.go:buildTestTools()`:

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

## Common Failures and Fixes

| Failure | Cause | Fix |
|---------|-------|-----|
| `"nothing to summarise"` in compaction | `context_limit` too small (fires on turn 1) | Increase to `8000`+ |
| `tool_result` SSE events missing | `sseToolWrapper` not wrapping tools | Ensure `wrappedTools` loop in `handleChat` |
| Session not recalled in second turn | Wrong `session_id` sent | Extract from `done` event `session_id` field |
| Doom-loop not firing | LLM varies the input slightly | Use explicit `"always use value=hello"` wording |
| `set -e` causes early exit | `grep -q` returns 1 on miss in pipefail mode | Script uses `set -uo pipefail` (not `-e`) |
| SSE output leaks into stdout | `compact_turn` or `dc_turn` before `$(...)` | Redirect discarded calls to `/dev/null` |
| calc result format mismatch | LLM formats `887,112` not `887112` | Regex: `887[,. ]?112` already in test script |
| `dc_turn` misses `done` event | `curl -s` (buffered) used instead of `curl -sN` | Add `-N` to `dc_turn` in test_features.sh if intermittent |

---

## Maintenance Checklist

When changing `llm-go` internals, update tests as follows:

| Change | Affected tests | Action |
|--------|---------------|--------|
| New event type in `llm/event.go` | [9] | Add SSE forwarding in `sseProvider`; add assertion |
| New `ProcessResult` value | [1]–[8] | Handle in `handleChat` sendEvent logic |
| Change `DoomLoopThreshold` | doom-loop | Update call count in test prompt |
| Change `DefaultTailTurns` | [7][11] | May need more/fewer compaction turns |
| Change `CompactionBuffer` | [7][11] | Adjust `context_limit` value |
| New store `PartType` | [10] | Add rendering in `handleSession` |
| New `/chat` request field | — | Add to `handleChat` req struct and test |
| New tool in `buildTestTools()` | — | Add dedicated test section |
