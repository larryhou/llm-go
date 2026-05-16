---
name: compact-test
description: Test double-compaction topic continuity for llm-go — verifies PartTypeRecentContext preserves the active topic across two successive compaction rounds via the knowledge-api /chat HTTP interface
---

# Compact-Test Skill

Tests one critical end-to-end behaviour:

> After **two successive compaction rounds**, the LLM must still recall a
> specific technical decision established before the first compaction.

This exercises `PartTypeRecentContext` (Issue J fix) — a verbatim excerpt of the
2 turns immediately before the tail, written into the compaction boundary message
at Step 6 of `session/compaction.go:Compact()`.

---

## Execution Rules

**You MUST follow these rules or the test is invalid:**

1. **Run every step in sequence.** Do not skip or reorder.
2. **Check the ABORT condition after every step.** If it triggers, stop
   immediately and report `ABORTED at step N: <reason>`. Do not proceed.
3. **Save each curl response in a named variable** (`R1`, `R2`, …, `R_RECALL`).
   You will need them for feature verification.
4. **Per-call timeout is 120 seconds** (`--max-time 120`). If curl exits with
   code 28 (timeout), that is an ABORT condition.
5. Use `$SESS` for all `/chat` calls. Never change it mid-test.
6. After the test, print a structured **Test Report** (see end of this document).

---

## Server

```
Base URL  : http://127.0.0.1:7700
Provider  : openai (timi proxy)
LLM URL   : http://192.168.3.119:8080/timi-claude/v1
API key   : sk-zzz6FtyLMyuobNNOukwgobP0l1F3TjMO
```

Start if not running:
```bash
lsof -ti:7700 | xargs kill -9 2>/dev/null; sleep 1
cd /Users/larryhou/Documents/opencode/llm-go
nohup go run ./cmd/knowledge-api/ \
  -skills .opencode -addr 127.0.0.1:7700 \
  -provider openai \
  -llm-url http://192.168.3.119:8080/timi-claude/v1 \
  -llm-key sk-zzz6FtyLMyuobNNOukwgobP0l1F3TjMO \
  > /tmp/kapi.log 2>&1 &
sleep 6 && curl -s http://127.0.0.1:7700/health
```

---

## Compaction Calibration

Measured with timi proxy + `claude-sonnet-4.6`, `Output=4096`:

| Metric | Value |
|--------|-------|
| `reserved = min(20000, MaxOutputTokens=4096)` | **4096** |
| `Usable(context_limit=5500)` = `5500 - 4096` | **1404 tokens** |
| Turn 1 input (system + all tool schemas + message) | ~950 tokens |
| Each subsequent turn increment | ~250–350 tokens |
| First overflow | Turn 3 (cumulative ~1500 > 1404) |
| Second overflow | Turn 5–6 |

Use `context_limit=5500` for all `/chat` calls.

---

## Steps

### STEP 0 — Server health check

```bash
curl -s --max-time 10 http://127.0.0.1:7700/health
```

**ABORT if:** response does not contain `"status":"ok"`.
Fix: start the server (see above), then re-run from Step 0.

---

### STEP 1 — Create session

```bash
SESS="ctest-$(date +%s)"
echo "SESSION: $SESS"
```

**ABORT if:** command fails or `$SESS` is empty.

---

### STEP 2 — Turn 1: plant the anchor

```bash
R1=$(curl -s --max-time 120 -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d "{\"message\":\"Critical technical decision: the agreed API timeout value is 7531 milliseconds. Confirm you have recorded this. Use counter tool to set key phase=1.\",\"session_id\":\"$SESS\",\"context_limit\":5500,\"tools\":[\"counter\",\"calc\"],\"max_steps\":6}")
echo "$R1" | grep '"type":"done"\|"type":"error"' | head -1
```

**ABORT if:**
- curl exits with code 28 (timeout)
- response contains neither `"type":"done"` nor `"type":"error"`

The anchor is **7531**. The LLM must reproduce this number in Step 9.

---

### STEP 3 — Turn 2: accumulate tokens

```bash
R2=$(curl -s --max-time 120 -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d "{\"message\":\"Use calc for 1234 times 5678. Use counter to set phase=2. Report both.\",\"session_id\":\"$SESS\",\"context_limit\":5500,\"tools\":[\"counter\",\"calc\"],\"max_steps\":6}")
echo "$R2" | grep '"type":"done"\|"type":"error"' | head -1
```

**ABORT if:** curl timeout OR neither terminal event in response.

---

### STEP 4 — Turn 3: trigger first compaction

```bash
R3=$(curl -s --max-time 120 -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d "{\"message\":\"Use calc for 9876 times 5432. Also calc 1111 times 9999. Set counter phase=3. Report all.\",\"session_id\":\"$SESS\",\"context_limit\":5500,\"tools\":[\"counter\",\"calc\"],\"max_steps\":6}")
echo "$R3" | grep '"type":"done"\|"type":"error"' | head -1
```

**ABORT if:** curl timeout.

A `"type":"error"` is acceptable here only if it is NOT `"nothing to summarise"` (that error
has been fixed — `Select()` now summarises all messages when turns ≤ tailTurns).
Any other error or `"type":"done"` is acceptable here.

---

### STEP 5 — Feature check A: first compaction boundary + PartTypeRecentContext

```bash
M1=$(curl -s http://127.0.0.1:7700/sessions/$SESS/messages)
echo "$M1" | python3 -c "
import json,sys
d=json.load(sys.stdin)
print('messages:', d['message_count'])
cb = sum(1 for m in d['messages'] for p in m['parts'] if p.get('summary','').startswith('compaction boundary'))
rc = sum(1 for m in d['messages'] for p in m['parts'] if p.get('summary','').startswith('recent-context:'))
print('compaction_boundaries:', cb)
print('recent_context_parts:', rc)
for m in d['messages']:
    for p in m['parts']:
        s = p.get('summary','')
        if s.startswith('recent-context:'):
            print('  RC excerpt:', s[:300])
"
```

**ABORT if:** `compaction_boundaries` = 0 after running extra accumulation turns
(see note below). At least one compaction MUST occur before continuing.

**FEATURE CHECK A — record results:**
- `compaction_boundaries` ≥ 1 → ✓ PASS
- `recent_context_parts` ≥ 1 → ✓ PASS (confirms Step 6 of `Compact()` ran)
- `recent_context_parts` = 0 → ✗ FAIL: `PartTypeRecentContext` not written
  (check `tailStartTurnIdx >= 1` guard in `Select()`)
- RC excerpt contains recognizable content (not empty) → ✓ PASS
- RC excerpt does NOT contain "7531" is acceptable here — the anchor turn
  may have been kept in the tail verbatim, not in the head

**Note — if no compaction yet:** run one extra accumulation turn, then re-run
Step 5. Maximum 2 extra turns before aborting.

```bash
# Extra accumulation turn if needed:
curl -s --max-time 120 -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d "{\"message\":\"Use calc for 7777 times 8888. Also calc 3333 times 4444. Set counter phase=3b. Report.\",\"session_id\":\"$SESS\",\"context_limit\":5500,\"tools\":[\"counter\",\"calc\"],\"max_steps\":6}"
```

---

### STEP 6 — Turn 4: accumulate toward second compaction

```bash
R4=$(curl -s --max-time 120 -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d "{\"message\":\"Use calc for 2222 times 3333. Also calc 4444 times 5555. Set counter phase=4. Report both.\",\"session_id\":\"$SESS\",\"context_limit\":5500,\"tools\":[\"counter\",\"calc\"],\"max_steps\":6}")
echo "$R4" | grep '"type":"done"\|"type":"error"' | head -1
```

**ABORT if:** curl timeout OR neither terminal event.

---

### STEP 7 — Turn 5: trigger second compaction

```bash
R5=$(curl -s --max-time 120 -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d "{\"message\":\"Use calc for 6666 times 7777. Also calc 8888 times 1234. Also calc 2468 times 1357. Set counter phase=5. Report all three.\",\"session_id\":\"$SESS\",\"context_limit\":5500,\"tools\":[\"counter\",\"calc\"],\"max_steps\":6}")
echo "$R5" | grep '"type":"done"\|"type":"error"' | head -1
```

**ABORT if:** curl timeout.

---

### STEP 8 — Feature check B: second compaction boundary + RC on both boundaries

```bash
M2=$(curl -s http://127.0.0.1:7700/sessions/$SESS/messages)
echo "$M2" | python3 -c "
import json,sys
d=json.load(sys.stdin)
print('messages:', d['message_count'])
cb = sum(1 for m in d['messages'] for p in m['parts'] if p.get('summary','').startswith('compaction boundary'))
rc = sum(1 for m in d['messages'] for p in m['parts'] if p.get('summary','').startswith('recent-context:'))
print('compaction_boundaries:', cb)
print('recent_context_parts:', rc)
# Show boundary messages with their parts
for m in d['messages']:
    parts = m.get('parts',[])
    types = [p['type'] for p in parts]
    summaries = [p.get('summary','') for p in parts]
    has_cb = any(s.startswith('compaction boundary') for s in summaries)
    has_rc = any(s.startswith('recent-context:') for s in summaries)
    if has_cb:
        print(f'  boundary msg {m[\"id\"][:8]}: has_compaction={has_cb} has_recent_context={has_rc}')
        for p in parts:
            s = p.get('summary','')
            if s.startswith('recent-context:'):
                print(f'    RC excerpt: {s[:300]}')
"
```

**ABORT if:** `compaction_boundaries` = 0.

**FEATURE CHECK B — record results:**
- `compaction_boundaries` ≥ 2 → ✓ PASS (double compaction confirmed)
- `compaction_boundaries` = 1 → run one extra accumulation turn (same as Step 7 with different numbers), then re-run this check. **ABORT if still < 2 after 2 extra turns** — the test is invalid without two compaction rounds. Do not proceed to STEP 9.
- `recent_context_parts` ≥ 2 → ✓ PASS (RC written on both boundaries)
- `recent_context_parts` = 1 → ✗ PARTIAL: only one boundary got an RC part
- `recent_context_parts` = 0 → ✗ FAIL: Step 6 of `Compact()` is not running
- Each boundary message that has a `recent-context:` part: verify its excerpt
  is non-empty and contains recognizable text (user message content or tool names)
- Both boundary messages should have RC parts now (guard is `tailStartTurnIdx >= 1`)

**⚠️ HARD GATE: Do NOT proceed to STEP 9 or STEP 9b unless `compaction_boundaries` ≥ 2.
The entire point of this test is to verify topic continuity across TWO compaction rounds.
A single compaction is not sufficient — the anchor turn (turn 1) may still be in the
tail after the first compaction and would be recalled trivially. Only after the second
compaction is the anchor guaranteed to have been processed by the summary LLM twice,
making the RC excerpt the only reliable path for the LLM to access it.**

**FEATURE CHECK C — StripMedia verification:**
The RC excerpt must NOT appear in the summary message text. Check:
```bash
echo "$M2" | python3 -c "
import json,sys
d=json.load(sys.stdin)
# Collect all RC excerpts
excerpts = []
for m in d['messages']:
    for p in m['parts']:
        s = p.get('summary','')
        if s.startswith('recent-context:'):
            # Extract a distinctive fragment (first 40 chars after header)
            excerpts.append(s[len('recent-context: ---\n以下是压缩前最近的对话原文：'):].strip()[:40])

# Check no summary message contains an RC excerpt verbatim
summaries_with_rc = []
for m in d['messages']:
    if not m.get('summary'): continue  # only check Summary=true messages
    for p in m['parts']:
        text = p.get('summary','')
        for ex in excerpts:
            if ex and ex in text:
                summaries_with_rc.append(m['id'][:8])

print('RC excerpts found:', len(excerpts))
print('Summary messages containing RC text:', summaries_with_rc)
print('StripMedia working:', 'YES' if not summaries_with_rc else 'NO - excerpts leaked into summary')
"
```

- `StripMedia working: YES` → ✓ PASS (RC stripped from summary LLM input)
- `StripMedia working: NO` → ✗ FAIL: `buildUserParts` StripMedia guard not working

---

### STEP 9 — Recall test (CRITICAL — hard PASS/FAIL)

```bash
R_RECALL=$(curl -s --max-time 120 -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d "{\"message\":\"What was the exact API timeout value in milliseconds that we agreed on at the very beginning of our conversation? State only the number.\",\"session_id\":\"$SESS\",\"context_limit\":5500,\"tools\":[],\"max_steps\":2}")

# Extract text response
RECALL_TEXT=$(echo "$R_RECALL" | grep '"type":"text"' | \
  sed 's/.*"delta":"\([^"]*\)".*/\1/' | tr -d '\n')
echo "Recall text: $RECALL_TEXT"
echo "---"
echo "$R_RECALL" | grep '"type":"done"\|"type":"error"' | head -1
```

**ABORT if:** curl timeout.

**CRITICAL ASSERTION:**
- Response contains `7531` → ✓ **PASS** — topic continuity confirmed across double compaction
- Response does NOT contain `7531` → ✗ **FAIL** — context continuity broken

**Prerequisite:** This step is only valid if STEP 8 confirmed `compaction_boundaries ≥ 2`.
If you reach here with fewer than 2 boundaries, the result is not meaningful — ABORT.

If FAIL: check whether `recent_context_parts` ≥ 1 from Step 8. If RC parts are
present but LLM still failed recall, the issue is the LLM not reading the
boundary message content — inspect what the boundary message looks like in the
model context by examining the RC excerpt and verifying it includes anchor-turn
text. If RC parts are absent, `PartTypeRecentContext` is not being written
(check `tailStartTurnIdx > 2` guard).

---

### STEP 9b — Seamless continuation test (CRITICAL — hard PASS/FAIL)

This tests a different and more demanding scenario than STEP 9:

> The user **does not ask** "what did we discuss". Instead they **directly
> reference** the earlier decision as if the conversation never paused.
> The LLM must respond coherently without asking for clarification.

This is the real-world "uninterrupted conversation" test. `PartTypeRecentContext`
works passively here — the excerpt is already in the boundary message, so the
LLM should know the context without being prompted.

```bash
R_CONT=$(curl -s --max-time 120 -X POST http://127.0.0.1:7700/chat \
  -H "Content-Type: application/json" \
  -d "{\"message\":\"OK so let's go with that timeout value we agreed on. Use the calc tool to convert it from milliseconds to seconds, and tell me how many minutes that would be.\",\"session_id\":\"$SESS\",\"context_limit\":5500,\"tools\":[\"calc\"],\"max_steps\":4}")

CONT_TEXT=$(echo "$R_CONT" | grep '"type":"text"' | \
  sed 's/.*"delta":"\([^"]*\)".*/\1/' | tr -d '\n')
echo "Continuation text: $CONT_TEXT"
echo "---"
echo "$R_CONT" | grep '"type":"done"\|"type":"error"' | head -1
```

**ABORT if:** curl timeout.

The correct answer requires the LLM to know the timeout is 7531ms, then:
- 7531 ms ÷ 1000 = **7.531 seconds**
- 7.531 s ÷ 60 = **~0.1255 minutes**

**Prerequisite:** This step is only valid if STEP 8 confirmed `compaction_boundaries ≥ 2`.
If you reach here with fewer than 2 boundaries, the result is not meaningful — ABORT.

**CRITICAL ASSERTIONS:**
1. Response contains `7531` OR `7.531` → the LLM knew the value without being told
2. Response contains `7` AND (`second` OR `sec` OR `分` OR `秒`) → correct unit conversion attempted
3. Response does NOT say "I don't know", "could you remind me", "what timeout", "I'm not sure which value" → LLM did not lose context

```bash
# Check each assertion:
echo "Contains 7531 or 7.531:"
echo "$R_CONT" | grep -c "7531\|7\.531" || true

echo "Contains second/minute unit:"
echo "$R_CONT" | grep -ic "second\|minute\|sec\|min\|秒\|分" || true

echo "LLM asked for clarification (bad):"
echo "$R_CONT" | grep -ic "don't know\|could you remind\|what timeout\|which value\|not sure\|unclear" || true
```

**PASS condition:** assertions 1 and 2 true, assertion 3 false (no clarification request).
**FAIL condition:** LLM asked for clarification OR gave a number unrelated to 7531.

**Why this test matters separately from STEP 9:**
- STEP 9 asks the LLM to recall explicitly → tests memory retrieval
- STEP 9b references the value implicitly → tests passive context continuity
- STEP 9b can fail even if STEP 9 passes, if the LLM needs to be prompted to look up history
- A working `PartTypeRecentContext` should make STEP 9b pass without any tool calls

**Expected tool call behaviour:** The LLM should call `calc` for the division (7531/1000
and 7.531/60). It should NOT call `knowledge_search` to look up the timeout — the value
should already be in context via the boundary message RC excerpt.

```bash
# Verify no knowledge_search was called (that would indicate passive context failed):
echo "knowledge_search calls (should be 0):"
echo "$R_CONT" | grep -c '"tool":"knowledge_search"' || true
```

- knowledge_search calls = 0 → ✓ LLM had the value passively from RC excerpt
- knowledge_search calls > 0 → ✗ LLM had to retrieve — RC excerpt may be missing the anchor turn

---

### STEP 10 — Final session dump

```bash
curl -s http://127.0.0.1:7700/sessions/$SESS/messages | python3 -c "
import json,sys
d=json.load(sys.stdin)
print('=== Final Session State ===')
print('total messages       :', d['message_count'])
cb = sum(1 for m in d['messages'] for p in m['parts'] if p.get('summary','').startswith('compaction boundary'))
rc = sum(1 for m in d['messages'] for p in m['parts'] if p.get('summary','').startswith('recent-context:'))
sm = sum(1 for m in d['messages'] if m.get('summary'))
print('compaction boundaries:', cb)
print('recent-context parts :', rc)
print('summary messages     :', sm)
print()
for m in d['messages']:
    role = m['role']
    is_sum = m.get('summary',False)
    label = '[SUMMARY]' if is_sum else ''
    ptypes = [p['type'] for p in m['parts']]
    print(f'  {role:9s} {label:9s} parts={ptypes}')
    for p in m['parts']:
        s = p.get('summary','')
        if s: print(f'    {p[\"type\"]}: {s[:120]}')
"
```

No ABORT condition. This is informational only.

---

## Test Report Template

After completing all steps, print this report:

```
=== COMPACT-TEST REPORT ===
Session  : <SESS>
Server   : http://127.0.0.1:7700

STEP-BY-STEP:
  Step 0  health check             : PASS / ABORT
  Step 2  turn 1 anchor planted    : PASS / ABORT
  Step 3  turn 2 accumulate        : PASS / ABORT
  Step 4  turn 3 first compact     : PASS / ABORT
  Step 6  turn 4 accumulate        : PASS / ABORT
  Step 7  turn 5 second compact    : PASS / ABORT

FEATURE CHECKS:
  [A] First compaction boundary present         : PASS / FAIL
  [A] PartTypeRecentContext on first boundary   : PASS / FAIL
  [A] RC excerpt non-empty                      : PASS / FAIL
  [B] Second compaction boundary present (≥2)  : PASS / FAIL  ← HARD GATE
  [B] PartTypeRecentContext on second boundary  : PASS / FAIL
  [C] StripMedia — RC not in summary text       : PASS / FAIL

CRITICAL (only valid when [B] PASS):
  Recall test (STEP 9)  — LLM says 7531         : PASS / FAIL
  Continuation (STEP 9b) — LLM knew value passively : PASS / FAIL
  Continuation (STEP 9b) — no clarification request : PASS / FAIL
  Continuation (STEP 9b) — no knowledge_search call : PASS / FAIL

FINAL COUNTS:
  total messages       : N
  compaction boundaries: N   (must be ≥ 2 for valid test)
  recent-context parts : N
  summary messages     : N

VERDICT: PASS / FAIL / ABORTED at step N
Note: FAIL or ABORTED at [B] means the test did not run — rerun with lower context_limit
```

---

## Abort Conditions Summary

| Step | Abort trigger |
|------|---------------|
| 0 | Server not healthy |
| 1 | `$SESS` empty |
| 2 | curl timeout; `"nothing to summarise"` on turn 1 |
| 3 | curl timeout; no terminal event |
| 4 | curl timeout |
| 5 | `compaction_boundaries < 1` after 2 extra turns |
| 6 | curl timeout; no terminal event |
| 7 | curl timeout |
| **8** | **`compaction_boundaries < 2` after 2 extra turns — hard gate, do not run STEP 9/9b** |
| 9 | curl timeout; reached here with `compaction_boundaries < 2` |
| 9b | curl timeout; reached here with `compaction_boundaries < 2` |

---

## Key Code Locations

| Feature | File | Location |
|---------|------|----------|
| `PartTypeRecentContext` constant | `store/store.go` | `PartType*` block |
| `RecentContextPartData` struct | `store/store.go` | after `CompactionPartData` |
| `SelectResult.RecentHead` | `session/compaction.go` | `Select()` return |
| `tailStartTurnIdx >= 1` guard | `session/compaction.go` | `Select()` RecentHead block |
| RecentHead in AllHead path | `session/compaction.go` | `Select()` `len(turns) <= tailTurns` branch |
| Step 6 excerpt write + token update | `session/compaction.go` | `Compact()` Step 6 |
| `context.WithoutCancel` in Compact | `session/compaction.go` | top of `Compact()` |
| `context.WithoutCancel` in handleChat | `cmd/knowledge-api/main.go` | `handleChat()` ctx setup |
| `buildRecentContextExcerpt` | `session/compaction.go` | after `Compact()` |
| `buildUserParts` opts + StripMedia | `session/context.go` | `buildUserParts()` |
| `/sessions/{id}/messages` RC rendering | `cmd/knowledge-api/main.go` | `handleSession()` |

## Known Fixed Bugs

| Bug | Root Cause | Fix |
|-----|-----------|-----|
| RC part missing on 2nd compaction | `tailStartTurnIdx >= 2` guard too strict | Changed to `>= 1`, clamp to 0 |
| SSE stream truncated after tool_result | `sseProvider` goroutine had `case <-ctx.Done(): return` | Removed; loop `for ev := range inner` only |
| "nothing to summarise" after repeated compaction | `len(turns) <= tailTurns` returned `Head: nil` | Changed to `Head: msgs` (AllHead) |
| Compaction fails with `context canceled` | `Compact()` used HTTP request context for LLM call | `ctx = context.WithoutCancel(ctx)` at top of `Compact()` |
| RunLoop aborted mid-step on SSE disconnect | `handleChat` passed `r.Context()` to RunLoop | `ctx := context.WithoutCancel(r.Context())` |
| RC part missing in AllHead path | AllHead returned `RecentHead: nil` | Compute last ≤2 turns as RecentHead in AllHead path |
