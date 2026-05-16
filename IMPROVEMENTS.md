# Architecture Improvement Notes

Findings from a full read of all core packages (`llm`, `provider`, `session`, `store`, `tool`, `knowledge`).
Each item has been verified against the current codebase. Items marked **[FIXED]** are confirmed resolved.

---

## llm / provider

### 1. Retry emits partial events to caller — `llm/client.go`

**Location:** `client.go:74–89`  
**Status:** **FIXED** — `doStream` now buffers all events; flushes only on `EventRequestFinish`; discards buffer on retry.

---

### 2. Anthropic `reasoning` part replayed as `text` block — `provider/anthropic/anthropic.go`

**Location:** `anthropic.go:253–256`  
**Status:** **FIXED** — `assistantBlocks` now emits `ThinkingBlockParam{Thinking, Signature}` for `PartTypeReasoning`.

---

### 3. Anthropic `ThinkingBlock` start event and `signature` not captured — `provider/anthropic/anthropic.go`

**Location:** `anthropic.go:283–298`  
**Status:** **FIXED** — `ThinkingBlock` case added in `ContentBlockStartEvent`; `ContentBlockStopEvent` emits `EventReasoningEnd` with signature; `llm.Event.Signature` and `store.ReasoningPartData.Signature` added.

---

### 4. OpenAI `NewFromConfig` ignores extra headers from config — `provider/openai/openai.go`

**Location:** `openai.go:75`  
**Status:** **FIXED** — `NewFromConfig` now reads `cfg.Options.Extra` (string values) and passes them as `extraHeaders` to `New()`.

---

### 5. OpenAI `StreamOptions.IncludeUsage` sent to all compatible providers — `provider/openai/openai.go`

**Location:** `openai.go:101–103`  
**Status:** **FIXED** — `StreamOptions.IncludeUsage` now only added when `req.Model.ProviderID == ProviderID` ("openai"). Third-party compatible providers skip the field entirely.

---

## session

### 6. `Compact()` writes boundary to store before LLM call — `session/compaction.go`

**Location:** `compaction.go:237–299`
**Status:** **FIXED** — boundary user message created first (no CompactionPart), summary message created second; LLM runs; only on success is CompactionPart written to boundary. LLM failure leaves a part-less boundary invisible to `FilterCompacted`.

---

### 7. `processorState` concurrent map access — `session/processor.go`

**Location:** `processor.go:147–158`, `processor.go:296–313`, `processor.go:337–368`
**Status:** **FIXED** — `activeToolParts` protected by `sync.Mutex`; `toolCtx`/`toolCancel` added so `cleanup()` cancels all in-flight goroutines; `sync.WaitGroup` tracks goroutine exit; single 250ms shared deadline.

---

### 8. `processorState` cleanup grace period multiplied by N tools — `session/processor.go`

**Location:** `processor.go:341–358`
**Status:** **FIXED** — see #7 above; single `time.After(250ms)` select covers all tools.

---

### 9. `RunLoop` loads all messages on every agentic step — `session/prompt.go`

**Location:** `prompt.go:122–131`
**Status:** **FIXED** — `msgs`/`allParts` loaded once before the loop and cached. Each step appends the new assistant message in-memory and calls `ListParts(assistantMsgID)` to refresh only new parts. Full reload only after `ProcessCompact` (compaction restructures history).

---

### 10. `Select()` uses position heuristic to identify compaction boundaries — `session/compaction.go`

**Location:** `compaction.go:96–103`
**Status:** **FIXED** — `Select` now accepts `allParts map[string][]*store.Part` and uses `hasPartType(allParts[m.ID], PartTypeCompaction)` — identical logic to `FilterCompacted`. Positional `msgs[i+1].Summary` heuristic removed.

---

### 11. `Prune()` is implemented but never called — `session/compaction.go` / `session/prompt.go`

**Location:** `compaction.go:424`, `prompt.go:198–208`
**Status:** **FIXED** — `RunLoop` fires a goroutine calling `Prune()` on every `ProcessStop`. `Prune()` guards with `cfg.Compaction.Prune` (opt-in, default false), so the call is always made but is a no-op unless explicitly enabled via `cfg.compaction.prune = true`.

---

## store

### 12. `ListSessions` has no insertion-order index — re-sorts on every call — `store/memory/memory.go`

**Location:** `memory.go:78–90`
**Status:** **FIXED** — `sessionOrder []string` slice added; `CreateSession` appends to it; `ListSessions` iterates `sessionOrder` O(n) with no sort; `sort` import removed.

---

### 13. `DataAs` breaks on JSON round-trip — future store implementations — `store/store.go`

**Location:** `store/store.go:167–170`
**Status:** **FIXED** — `DataAs` now tries direct type assertion first; falls back to `json.Marshal` + `json.Unmarshal` when assertion fails (e.g. `map[string]any` from SQL deserialisation).

---

### 14. Weak type safety on `Part.Data` — `store/store.go`

**Location:** `store.go:83`, all call sites across `session/`
**Status:** Partially addressed — all bare `p.Data.(T)` assertions replaced with `store.DataAs[T](p)`; JSON fallback now handles round-trips. Sealed interface (Option A) remains as a future improvement.

---

## knowledge

### 13. `max_results` upper bound declared in schema but not enforced at runtime — `knowledge/search_tool.go`

**Location:** `search_tool.go:56–65`  
**Status:** ~~Real~~ — **FIXED**, confirmed in current code.

`maxResultsCap = 20` is enforced at runtime. No action needed.

---

### 14. Cross-priority-group results have no global score sort — `knowledge/manager.go`

**Location:** `manager.go:113–133`  
**Status:** **FIXED** — global `sort.Slice` by score descending added after all groups are accumulated in `peek()`.

---

## provider / Registry

### 10. `NewFromConfig` has no callers — `llm.json` provider config is inert — `cmd/control/main.go`, `cmd/llm-api/main.go`

**Location:** `cmd/control/main.go:238–247`
**Status:** **FIXED** — both cmd entrypoints now load `config.Load()` + `auth.Load()`, build a `provider.Registry`, register anthropic/openai/timi factories, and call `registry.BuildProvider()`. CLI flags override file config via a merged `ProviderInfo`.

---

### 11. `provider.Registry` never instantiated — entire registry system is dead code — `provider/provider.go`

**Location:** `provider/provider.go:72–122`
**Status:** **FIXED** — `Registry` gains `RegisterFactory(id, Factory)` and `BuildProvider(id, cfg, authStore)`; each provider package exposes a `Factory` var; both cmd entrypoints instantiate and use the registry.

---

## Priority Summary

| # | Package | Issue | Severity | Status |
|---|---------|-------|----------|--------|
| 1 | `llm` | Retry emits partial events — corrupt processor state on retry | **High** | **FIXED** |
| 2 | `provider/anthropic` | Reasoning replayed as `text` block — API 400 / broken multi-turn reasoning | **High** | **FIXED** |
| 3 | `provider/anthropic` | `ThinkingBlock` signature not captured — multi-turn reasoning broken | **High** | **FIXED** |
| 4 | `provider/openai` | `NewFromConfig` ignores extra headers from config | Low | **FIXED** |
| 5 | `provider/openai` | `IncludeUsage` sent to all compatible providers — proxy rejection risk | Low | **FIXED** |
| 6 | `session` | `Compact()` writes boundary before LLM call — data loss on partial failure | **High** | **FIXED** |
| 7 | `session` | `processorState` concurrent map access — data race under concurrent tools | Medium | **FIXED** |
| 8 | `session` | Cleanup grace period multiplied by N tools — incorrect timeout logic | Medium | **FIXED** |
| 9 | `session` | `RunLoop` reloads all messages on every agentic step — unnecessary I/O | Medium | **FIXED** |
| 10 | `session` | `Select()` uses position heuristic for compaction boundary detection | Low | **FIXED** |
| 11 | `session` | `Prune()` never called by default — dead feature | Low | **FIXED** |
| 12 | `store` | `ListSessions` re-sorts on every call — no insertion-order index | Low | **FIXED** |
| 13 | `store` | `DataAs` breaks on JSON round-trip — future non-memory stores silently broken | Medium | **FIXED** |
| 14 | `store` | Weak `Part.Data` typing — bare assertions replaced; sealed interface still future work | Low | Partial |
| 15 | `knowledge` | Cross-group results have no global score sort — sub-optimal ranking | Low | **FIXED** |
| 16 | `cmd` | `NewFromConfig` never called — `llm.json` provider config inert | Medium | **FIXED** |
| 17 | `provider` | `Registry` never instantiated — extensibility blocked | Medium | **FIXED** |
| KN-1 | `knowledge` | `AllowPartialFailure=false` default — one timeout aborts entire query | **High** | Open |
| KN-2 | `knowledge` | `dispatchGroup` waits for all goroutines — slow source blocks group | Medium | Open |
| KN-3 | `knowledge` | `oldestSeq()` O(n) scan — should be O(1) field read | Low | Open |
| KN-4 | `knowledge` | `fetch` fallback silently strips prefix — wrong source gets corrupt input | Medium | Open |

**#14** (weak `Part.Data` typing — sealed interface) and **KN-1 through KN-4** (knowledge package items) remain as future work.

---

## Previously Reported — Confirmed Fixed

| Original # | Issue | Fix location |
|---|---|---|
| 1 | Retry emits partial events to caller | `llm/client.go` — buffer per attempt, flush on `EventRequestFinish` |
| 2 | Anthropic reasoning replayed as `text` block | `provider/anthropic/anthropic.go` — `ThinkingBlockParam` with `Signature` |
| 3 | `ThinkingBlock` signature not captured | `anthropic.go` — `ThinkingBlock` case + `EventReasoningEnd`; `store.ReasoningPartData.Signature` |
| 4 | OpenAI `NewFromConfig` ignores extra headers | `provider/openai/openai.go` — reads `cfg.Options.Extra` |
| 6 | `Compact()` writes boundary before LLM call | `session/compaction.go` — boundary created first; CompactionPart written only on LLM success |
| 7+8 | `processorState` concurrent map + N×250ms cleanup | `session/processor.go` — `sync.Mutex`, `toolCtx`/`toolCancel`, `sync.WaitGroup` |
| 9 | `RunLoop` reloads all messages each step | `session/prompt.go` — msgs/allParts cached; only new parts appended per step |
| 10 | `Select()` positional boundary heuristic | `session/compaction.go` — `hasPartType(PartTypeCompaction)` check; `allParts` param added |
| 11 | `Prune()` dead code — never called | `prompt.go` — unconditional goroutine on `ProcessStop`; opt-in via `cfg.compaction.prune` |
| 12 | `ListSessions` O(n log n) sort | `store/memory/memory.go` — `sessionOrder []string` insertion-order index |
| 13 | `DataAs` breaks on JSON round-trip | `store/store.go` — JSON marshal/unmarshal fallback |
| 14 | Bare `p.Data.(T)` assertions across session | `session/` — all replaced with `store.DataAs[T](p)` |
| 15 | Cross-group knowledge score sort | `knowledge/manager.go` — global `sort.Slice` after accumulation |
| 16 | `NewFromConfig` never called in cmd | `cmd/control/main.go`, `cmd/llm-api/main.go` — registry wired |
| 17 | `provider.Registry` dead code | `provider/provider.go` — `RegisterFactory` + `BuildProvider` added |
| 2 (orig) | `classifyStatus` case 529 unreachable | `error.go` — `case 529` now precedes `>= 500` |
| 3 (orig) | `MaxOutputTokens` returns 0 for unset output | `overflow.go` — `fallback = 4096` added |
| 4 (orig) | Anthropic image block uses wrong constructor | `anthropic.go` — `NewImageBlockBase64` / `NewImageBlock` correctly routed |
| 5 (orig) | Anthropic system prompt loses cache granularity | `anthropic.go` — individual `TextBlockParam` entries preserved |
| 6 (orig) | OpenAI `EventToolInputStart` may never be emitted | `openai.go` — `ts.started` flag defers until both id and name are known |
| 10 (orig) | `newID()` non-unique (UnixNano) | `processor.go` — `crypto/rand` hex |
| 13 (orig) | Token estimation placeholder (constant 100) | `compaction.go` — uses `m.Tokens` with 100 fallback |
| 14 (orig) | `EventToolResult` silently ignored | `processor.go` — `log.Printf` warning emitted |
| 15 (orig) | `FilterCompacted` may mis-pair boundary and summary | `context.go` — backward walk finds most recent complete pair |
| 16 (orig) | Double `loadMessages` per loop iteration | `prompt.go` — cache across steps; full reload only after compaction |
| 23 (orig) | `max_results` upper bound not enforced at runtime | `search_tool.go` — `maxResultsCap = 20` enforced |
| 25 (orig) | `fetch` fallback passes un-stripped `q.Input` to `Accepts` | `manager.go` — prefix stripped before fallback loop |
| 26 (orig) | `groupByPriority` dead `peekMode` parameter | `manager.go` — parameter removed |
| 25 (orig) | `fetch` fallback passes un-stripped `q.Input` to `Accepts` | `manager.go` — prefix stripped before fallback loop |
| 26 (orig) | `groupByPriority` dead `peekMode` parameter | `manager.go` — parameter removed |

---

## Knowledge Package — Detailed Weakness Analysis

### KN-1. `AllowPartialFailure` defaults to `false` — single source timeout aborts entire query — `knowledge/manager.go`

**Location:** `manager.go:34–35`, `manager.go:117`

```go
// ManagerConfig — zero value
AllowPartialFailure bool   // default false

// peek()
if err != nil && !m.cfg.AllowPartialFailure {
    return nil, err        // aborts on first group error
}
```

`ManagerConfig` is a plain struct; its zero value leaves `AllowPartialFailure = false`.
When a caller constructs `NewManager(ManagerConfig{})` without explicitly setting the flag
(the common case shown in every test that needs robustness uses `AllowPartialFailure: true`),
any single source error — including a `SourceTimeout` expiry — aborts the entire query and
returns nil results to the LLM.

In a production setup with even two sources (e.g. a local Bleve index + a remote web
search), a momentary network hiccup in the web source silently removes all search
capability from the LLM. The LLM receives a tool error and has no results at all, even
though the local index may have answered the query perfectly.

**Impact:** High operational risk in any multi-source deployment. The safe default for a
knowledge retrieval layer should be degraded-but-functional, not fail-fast.

**Fix:** Flip the default or rename to opt-out:

Option A — safe default (`AllowPartialFailure = true` unless explicitly disabled):
```go
// ManagerConfig
DisablePartialFailure bool  // opt-out; default = partial results allowed
```

Option B — keep the field name, change the default by always initialising via constructor:
```go
func DefaultManagerConfig() ManagerConfig {
    return ManagerConfig{AllowPartialFailure: true}
}
```

Callers that want strict all-or-nothing can pass `AllowPartialFailure: false` explicitly.

---

### KN-2. `dispatchGroup` waits for all goroutines — slow source blocks group completion — `knowledge/manager.go`

**Location:** `manager.go:213–248`

```go
ch := make(chan outcome, len(group))
for _, s := range group {
    go func() { ch <- outcome{m.callSource(ctx, s, q, peek)} }()
}
var all []Result
for range group {          // blocks until every goroutine sends
    o := <-ch
    ...
}
```

All goroutines in a priority group are started concurrently, but `dispatchGroup` then
blocks on a plain `for range group` loop that receives every outcome before returning.
This means the group's latency is `max(source latencies)`, not `median`.

If `SourceTimeout > 0`, each source has an individual deadline, so the worst-case group
latency is `SourceTimeout × 1` (all sources time out at the same wall time). However if
`SourceTimeout == 0`, a single unresponsive source stalls the entire group indefinitely,
even if `ctx` has no deadline — there is no intra-group short-circuit.

More subtly: even with `SourceTimeout` set, the group that has already accumulated
`>= MaxResults` results from fast sources still waits for the slow one to time out before
returning. This wastes `SourceTimeout` on every request that a fast source already
satisfied.

**Impact:** Unnecessary tail latency on every query with heterogeneous source speeds.

**Fix — early-exit when results are sufficient:**

```go
func (m *Manager) dispatchGroup(ctx context.Context, group []Source, q Query, peek bool) ([]Result, error) {
    type outcome struct{ results []Result; err error }
    ch := make(chan outcome, len(group))
    for _, s := range group {
        s := s
        go func() { ch <- outcome{m.callSource(ctx, s, q, peek)} }()
    }
    var all []Result
    var firstErr error
    remaining := len(group)
    for remaining > 0 {
        o := <-ch
        remaining--
        if o.err != nil {
            if firstErr == nil { firstErr = o.err }
            continue
        }
        all = append(all, o.results...)
        // Early exit: already have enough, don't wait for stragglers.
        if m.cfg.MaxResults > 0 && len(all) >= m.cfg.MaxResults {
            break
        }
    }
    // Drain remaining goroutines into the buffered channel — they'll finish
    // on their own (SourceTimeout or ctx cancellation).
    sort.Slice(all, func(i, j int) bool { return all[i].Score > all[j].Score })
    return all, firstErr
}
```

The buffered `ch` ensures abandoned goroutines are never leaked — they write to the
channel (which has capacity) and exit.

---

### KN-3. `oldestSeq()` is O(n) linear scan — should be O(1) — `knowledge/session_history.go`

**Location:** `session_history.go:316–324`

```go
func (s *SessionHistorySource) oldestSeq() int {
    oldest := int(^uint(0) >> 1) // max int
    for seq := range s.compactionDocs {
        if seq < oldest {
            oldest = seq
        }
    }
    return oldest
}
```

`oldestSeq` is called inside `Hook()` every time a new compaction round triggers an
eviction (i.e. when `len(compactionDocs) >= maxCompactions`). With `maxCompactions = 8`
the map is tiny, so the cost is negligible today. However:

1. `compactionDocs` is a `map[int][]string` (not a sorted structure), so the scan cannot
   be avoided without a separate data structure.
2. Because `currentSeq` is a monotonically incrementing counter and evictions always
   remove the smallest key, the "oldest" seq is always `currentSeq - maxCompactions + 1`
   (or the actual minimum if there were gaps). This property is never exploited.
3. The method acquires no lock itself — it must be called under `s.mu`, which it is, but
   this is not documented and easy to miss.

**Fix — track min-seq explicitly:**

```go
type SessionHistorySource struct {
    ...
    minSeq int  // oldest retained compaction seq (0 = none)
}

// In Hook(), instead of calling oldestSeq():
if len(s.compactionDocs) >= s.maxCompactions {
    for _, id := range s.compactionDocs[s.minSeq] {
        _ = s.index.Delete(id)
    }
    delete(s.compactionDocs, s.minSeq)
    s.minSeq++ // advance to next oldest
}
```

This reduces `oldestSeq()` to an O(1) field read and removes the map scan entirely.
`oldestSeq()` itself can then be deleted.

---

### KN-4. `Fetch` fallback passes `internalKey` to `Accepts` — unrecognised `sourceID` corrupts fallback input — `knowledge/manager.go`

**Location:** `manager.go:151–171`

```go
sourceID, internalKey, hasPfx := strings.Cut(q.Input, ":")
if hasPfx {
    for _, s := range sources {
        if s.ID() == sourceID { ... return ... }  // direct route
    }
}
// Fallback: first accepting source.
fallbackQ := q
if hasPfx {
    fallbackQ.Input = internalKey  // ← strips sourceID prefix
}
for _, s := range sources {
    if !s.Accepts(fallbackQ) { continue }
    results, err := m.callSource(ctx, s, fallbackQ, false)
```

When the RefID contains a `:` but the `sourceID` portion does not match any registered
source, the code falls through to the fallback path and **silently strips the
`sourceID:` prefix**, passing only `internalKey` to `Accepts` and `Fetch`. 

Example: RefID = `"external-wiki:https://example.com/page"`. If `external-wiki` is not
registered, the fallback receives `"https://example.com/page"` — the URL — which may or
may not be what the fallback source expects. A web-fetch source that parses its input as
a URL will accidentally succeed; a vector-DB source will get a bare URL string with no
context.

The original query intent is `"fetch external-wiki doc"`. The fallback silently
reinterprets it as `"fetch https://example.com/page from first accepting source"`. If a
web source happens to accept it, the caller gets content from an entirely different
backend with no error or warning.

**Fix — do not strip the prefix in the fallback; pass the full original input:**

```go
fallbackQ := q
// Do NOT strip prefix — let each source's Accepts() and Fetch() handle it.
// Stripping was intended to avoid "sourceID:key" being passed to a source
// that expects only "key", but it silently changes the query semantics.
for _, s := range sources {
    if !s.Accepts(fallbackQ) { continue }
    ...
}
```

If sources genuinely need clean input, they should strip the prefix themselves in
`Accepts` / `Fetch` when they recognise it. Alternatively, add a `fallbackInput` field
to `Query` that the Manager populates and sources can use when they do not understand
the full `Input`.

---

## New Findings (Independent Code Review)

### Findings Summary

| # | Package | Issue | Severity | Action |
|---|---------|-------|----------|--------|
| A | `provider/openai` | User messages silently drop image/file parts | **High** | Fix |
| B | `provider/openai` | `EventToolInputStart` not emitted when first delta has empty `tc.ID` | **High** | Fix |
| C | `tool` | `init()` spawns an unstoppable background goroutine | Medium | Fix |
| D | `store` | No batch query on `Store` interface — N+1 when backed by a real DB | Medium | Fix interface |
| E | `store` | `DataAs` JSON round-trip silently returns zero-value when `Data` is nil | Low–Medium | Fix |
| F | `session` | Doom-loop detection resets per `Process` call — does not span agentic steps | Low | Clarify / Fix |
| G | `config` | `Load()` does shallow JSON unmarshal merge — project config cannot unset global fields | Low | Document / Fix |
| H | `session` | `system.go` model-ID matching uses short substrings (`o1`, `o3`) — false positives on custom IDs | Low | Fix |
| I | `session` | `RunLoop` is a blocking call with no preemption — user cannot interrupt an in-flight agentic loop | **High** | Design |
| J | `session` | Compaction swallows recent context — LLM loses track of the active topic after compact | **High** | Design |

---

### A. OpenAI provider silently drops image/file parts — `provider/openai/openai.go`

**Location:** `openai.go:183–185`

```go
case llm.RoleUser:
    text := extractText(m.Content)
    out = append(out, openai.UserMessage(text))
```

`extractText` only concatenates `PartTypeText` parts. `PartTypeImage` and `PartTypeFile`
parts are silently discarded with no error or warning. The Anthropic provider correctly
handles images via `userBlocks` / `NewImageBlockBase64` / `NewImageBlock`.

**Impact:** Any multimodal user message sent via an OpenAI-compatible provider loses all
attachments. The LLM receives only the text portion; no error is surfaced to the caller.

**Fix:** Extend `convertMessages` user-role branch to build a multi-part content array
(using `openai.ImagePart` / `openai.TextPart`) when the message contains image or file
parts, mirroring the Anthropic `userBlocks` implementation.

---

### B. `EventToolInputStart` not emitted when first delta has empty `tc.ID` — `provider/openai/openai.go`

**Location:** `openai.go:319–329`

```go
if !exists {
    ts = &toolState{id: tc.ID, name: tc.Function.Name}
    toolByIndex[idx] = ts
    if tc.ID != "" {
        out <- llm.Event{Type: llm.EventToolInputStart, ...}
    }
}
```

Some OpenAI-compatible providers send the first tool-call delta with an empty `tc.ID`,
filling it in a subsequent delta. When that happens `EventToolInputStart` is never emitted.
`processor.go` creates the tool `Part` on `EventToolInputStart`; if the event is absent,
`activeToolParts[callID]` has no entry and the entire tool call part is lost silently.

**Impact:** Tool calls from certain providers are not persisted to the store; the agentic
loop continues without a record of the tool execution.

**Fix:** Decouple part creation from `EventToolInputStart`. Either emit a deferred
`EventToolInputStart` once `tc.ID` becomes known, or have the processor create the part
lazily on `EventToolCall` if no prior start event was received.

---

### C. `init()` spawns an unstoppable background goroutine — `tool/truncate.go`

**Location:** `truncate.go:37–41`

```go
func init() {
    truncDir = filepath.Join(os.TempDir(), "opencode-tool-output")
    go cleanupLoop()
}
```

`cleanupLoop` runs an infinite `time.Sleep / cleanupOldFiles` loop. It starts
automatically on package import, has no shutdown channel, and cannot be stopped.
In test binaries every `go test` run leaks this goroutine. In long-running processes
it is benign but uncontrollable.

**Fix:** Replace `init()` with an explicit `StartCleanup(ctx context.Context)` function
that respects context cancellation. Callers (e.g. `cmd/llm-api`) call it once at
startup and pass the root context.

---

### D. `Store` interface lacks batch query — N+1 pattern when backed by a real DB — `store/store.go`

**Location:** `store/store.go:12–30`, `session/prompt.go:317–330`

`loadMessages` calls `ListParts(ctx, m.ID)` once per message:

```go
for _, m := range msgs {
    ps, err := s.ListParts(ctx, m.ID)
    ...
}
```

With an in-memory store this is cheap. Against a SQL or remote store each call is a
round-trip, resulting in N+1 queries per agentic step. The interface has no
`ListPartsBySession(sessionID)` method to retrieve all parts in one query.

**Impact:** Acceptable today (only `memory` store exists). Any future SQL/Redis
implementation will inherit this N+1 without a compile-time signal.

**Fix:** Add `ListPartsBySession(ctx context.Context, sessionID string) ([]*Part, error)`
to the `Store` interface. Update `loadMessages` to use it. Implement in `memory.Store`
by iterating `sessionMsgs` and collecting all parts.

---

### E. `DataAs` returns zero-value silently when `Part.Data` is nil — `store/store.go`

**Location:** `store/store.go:174–189`

```go
func DataAs[T any](p *Part) (T, bool) {
    if v, ok := p.Data.(T); ok {
        return v, ok
    }
    b, err := json.Marshal(p.Data)   // json.Marshal(nil) → "null"
    ...
    var v T
    _ = json.Unmarshal(b, &v)        // json.Unmarshal("null", &v) → v stays zero
    return v, true                   // ok=true even though Data was nil
}
```

When `p.Data` is `nil`, `json.Marshal` produces `"null"`, `json.Unmarshal` sets the
pointer target to `nil`, and the function returns `(nil, true)`. The caller gets
`ok=true` with a nil pointer, which may panic on dereference or silently produce
incorrect behaviour.

**Fix:** Add an explicit nil check before the JSON fallback:

```go
if p.Data == nil {
    var zero T
    return zero, false
}
```

---

### F. Doom-loop detection resets per `Process` call — `session/processor.go`

**Location:** `processor.go:93–104`

`processorState` (including `recentCalls`) is created fresh inside each `Process` call.
The doom-loop counter therefore resets between agentic steps. A tool called once per
step across three consecutive steps will never trigger the threshold, even though the
LLM is clearly stuck.

**Impact:** Low — most actual doom loops occur within a single step (multiple tool
calls in one LLM response). Cross-step loops are caught eventually by `MaxSteps`.

**Fix (if desired):** Move `recentCalls` to `Processor` (shared across calls) or pass it
in via `ProcessInput`. Add a reset condition when a different tool is called.

---

### G. `config.Load()` shallow merge cannot unset global fields — `config/config.go`

**Location:** `config.go:259–274`

```go
cfg := &Info{}
for _, p := range paths {
    json.Unmarshal(data, cfg)   // second file merges into same struct
}
```

Go's `json.Unmarshal` only overwrites fields present in the JSON; absent fields keep
their previous value. A project-local config cannot reset a field set by the global
config to its zero value (e.g. cannot clear a string or reset a `*bool` to `nil`).

**Impact:** Low for current two-layer setup. Becomes confusing as config complexity grows.

**Fix:** Document the merge semantics explicitly. For a proper deep-merge, unmarshal each
file into a separate `Info` and merge field-by-field with explicit "unset" sentinel support
(e.g. JSON `null` clears a pointer field).

---

### H. Model-ID matching uses short substrings — false positives — `session/system.go`

**Location:** `system.go:56`

```go
case strings.Contains(id, "gpt-4") || strings.Contains(id, "o1") || strings.Contains(id, "o3"):
    return promptBeast
```

`"o1"` and `"o3"` are two-character strings that will match any model ID containing
those characters (e.g. `"moonshot-o1-mini"`, `"custom-o3-turbo"`, any UUID containing
`o1`). The wrong system prompt is injected silently.

**Impact:** Low for standard OpenAI/Anthropic model naming. Affects users with custom
or third-party model IDs.

**Fix:** Use word-boundary or prefix matching:

```go
case id == "o1" || id == "o3" || strings.HasPrefix(id, "o1-") || strings.HasPrefix(id, "o3-") ||
     strings.Contains(id, "gpt-4"):
```

---

### I. `RunLoop` is a blocking call with no preemption — user cannot interrupt an in-flight agentic loop — `session/prompt.go`

**Location:** `prompt.go:75–271`, `processor.go:81–141`

**Scenario:**

A user sends a message that triggers a long-running agentic loop (e.g. LLM calls a
slow tool — file indexing, a network fetch, a shell command). Mid-execution the user
realises the LLM is doing the wrong thing and sends a correction message. There is no
way to deliver that message to the running session without first terminating the entire
loop and losing its in-progress state.

**Root cause — `RunLoop` is fully synchronous and blocking:**

```go
// Caller blocks here until stop / error / compaction:
result, err := RunLoop(ctx, store, input)
// ← only here can a second message be accepted
```

The only external control surface is `ctx.cancel()`, which is a **hard global cancel**:
it kills the LLM stream, all tool goroutines, and leaves no channel for a new message
to be injected into the same session.

**What happens if a caller forces concurrent `RunLoop` calls on the same session:**

1. **Store data race** — two goroutines simultaneously read and write messages/parts for
   the same `sessionID`. The memory store has per-collection locks, but the ordering of
   `CreateMessage` calls across two loops is non-deterministic, corrupting `ListMessages`
   results.

2. **Incomplete assistant message visible to the second loop** — the first `RunLoop`
   calls `CreateMessage` for an assistant placeholder before the LLM stream finishes.
   The second loop loads history via `loadMessages` and sees this empty assistant message.
   Sending an assistant message with no content (and no tool-result follow-up) to the LLM
   is a protocol error on both Anthropic and OpenAI.

3. **Tool goroutine leak** — the first loop's in-flight tool goroutines hold `toolCtx`
   (derived from the first loop's `ctx`). The second loop has no visibility into these
   goroutines and cannot cancel or wait for them.

**What is needed:**

Three changes across three layers are required to support this pattern cleanly:

**Layer 1 — `session/prompt.go`: async handle with clean preemption**

`RunLoop` must be splittable into a non-blocking start and a cancellable handle:

```go
type RunHandle struct {
    Interrupt func()        // signal: stop after current tool completes
    Cancel    func()        // signal: stop immediately (hard cancel)
    Done      <-chan struct{} // closed when RunLoop has fully exited
    Result    RunResult     // available after Done is closed
    Err       error
}

func RunLoopAsync(ctx context.Context, s store.Store, input RunInput) *RunHandle
```

`Interrupt()` sets a flag checked between agentic steps (after each `Process` call
returns), allowing the current tool to finish naturally before the loop exits cleanly.
`Cancel()` cancels the context immediately, triggering `cleanup()`.
Both must wait for `cleanup()` to complete (tool goroutines drained, parts marked)
before the caller is allowed to start a new `RunLoop` on the same session.

**Layer 2 — `store/store.go`: interrupted message state**

`Message` needs an `Interrupted bool` field (or equivalent `Status` field) so that
a loop that was preempted can mark its in-progress assistant message:

```go
type Message struct {
    ...
    Interrupted bool  // true if this assistant turn was cut short by user preemption
}
```

This allows the next `RunLoop` (carrying the user's correction) to load a clean history.

**Layer 3 — `session/context.go`: filter interrupted messages from history**

`ToModelMessages` (and `FilterCompacted`) must skip interrupted assistant messages when
building the message list sent to the LLM. An assistant message that has no complete
tool-result pair is a protocol error; it must not appear in the next request.

Optionally, a brief `[Assistant response interrupted by user]` text part can be
injected in place of the interrupted message so the LLM understands the context gap.

**Impact:** Without this capability, any UI that allows users to send a correction while
the agent is running must either (a) queue the message and wait for the current loop to
finish — potentially minutes later — or (b) hard-cancel the session and lose all
in-progress tool results. Neither is acceptable for interactive use.

---

### J. Compaction swallows recent context — LLM loses track of the active topic after compact — `session/compaction.go`, `session/context.go`

**Location:** `compaction.go:199–322`, `context.go:71–99`, `prompt.go:232–254`

**Scenario:**

A user is mid-conversation on a specific technical topic — e.g. debugging a particular
function, negotiating the details of an API contract, or tracking a sequence of code
changes. The context window fills up and compaction fires. After compaction the LLM
receives a high-level summary of the entire conversation, but the fine-grained details
of the *most recent* discussion are frequently lost: the summary LLM generalises and
omits specifics (exact function signatures, intermediate debugging findings, the last
decision that was just made). The LLM then continues the conversation as if those
details were still available, producing answers that contradict or ignore what was
just agreed upon.

**Root cause — compaction is fully transparent to the LLM:**

After `Compact()` succeeds, `RunLoop` immediately continues into the next agentic step:

```go
case ProcessCompact:
    compactor.Compact(ctx, ...)
    msgs, allParts, err = loadMessages(ctx, s, input.SessionID)
    continue   // ← LLM gets the new context with no awareness of what just happened
```

The LLM sees:

```
[boundary user msg: "What did we do so far?"]
[summary assistant msg: high-level summary of entire conversation]
[tail: last 2 turns verbatim]
```

The head — which includes the turns immediately preceding the tail — is completely
gone. The LLM has no way to know which specific details were dropped, so it cannot
know to call `knowledge_search` to recover them. The existing `knowledge-recall.txt`
system prompt hint ("use knowledge_search if you need to recall earlier content") only
works when the LLM *suspects* it is missing something; it cannot help when the LLM
believes the summary is complete.

**Why the summary is insufficient for fine-grained recall:**

The `SummaryTemplate` produces a structured Markdown summary with sections for Goal,
Progress, Decisions, Next Steps, and Relevant Files. This is adequate for re-orienting
the LLM at a high level. It is not adequate for preserving the verbatim details that
matter most in a technical conversation — the exact content of the last tool result,
the specific line of code that was just identified as problematic, the precise wording
of the last user requirement. Summary LLMs routinely generalise these away.

**Proposed fix — inject a recent-context excerpt into the compaction boundary message:**

The `Select()` function already splits messages into `head` (to summarise) and `tail`
(to keep verbatim). The last 2 turns of `head` — the turns immediately before the tail
— are the most information-dense and most likely to be mis-summarised. Their verbatim
content (truncated to a manageable size) should be written directly into the compaction
boundary message as a new `PartTypeRecentContext` part.

The boundary message the LLM sees after compaction would then become:

```
[boundary user msg]
  Content 1: "What did we do so far?"
  Content 2: ## 压缩前最近讨论的内容

             **[用户]**
             具体的用户消息原文（最多 300 字符）...

             **[助手]**
               - 调用工具: read_file → 文件内容前 120 字符...
             助手回复原文（最多 300 字符）...

[summary assistant msg: 整体摘要]
[tail: 最近 2 轮对话原文]
```

This gives the LLM an immediate, verbatim anchor for the active topic without
requiring any tool call, and without relying on the summary to preserve detail.

**Required changes across four locations:**

**`store/store.go`** — new part type and data struct:

```go
PartTypeRecentContext = "recent-context"

type RecentContextPartData struct {
    Excerpt string  // verbatim excerpt of last N turns before compaction
}
```

**`session/compaction.go`** — two additions:

1. `SelectResult` gains a `RecentHead []*store.Message` field, populated by `Select()`
   as the messages belonging to the last 2 user turns within `Head` (i.e.,
   `msgs[turns[len(turns)-tailTurns-2].StartIdx : tailMsgIdx]`, guarded by
   `len(turns) > tailTurns+2` — if head has ≤ 2 turns there is nothing extra to anchor,
   and `RecentHead` is nil, skipping Step 5 entirely):

```go
type SelectResult struct {
    Head        []*store.Message
    TailStartID string
    RecentHead  []*store.Message  // last 2 turns of Head closest to tail; nil if head is too short
}
```

2. `Compact()` gains a new Step 5 (after the existing `PartTypeCompaction` write) that
   calls `buildRecentContextExcerpt(sel.RecentHead, allParts)` and writes the result as
   a `PartTypeRecentContext` part on the boundary message. This step is **non-fatal**:
   failure does not roll back the compaction.

   `buildRecentContextExcerpt` renders each message as one block:
   ```
   ---
   以下是压缩前最近的对话原文：

   **[用户]**
   <user text, max 300 chars>...

   **[助手]**
   - 调用工具: <tool_name> → <output前120字符>...
   <assistant text, max 300 chars>...
   ```
   Text is explicitly truncated with `...` so the LLM knows content was cut.

**`session/context.go`** — two changes:

1. `buildUserParts()` gains one new case:

```go
case store.PartTypeRecentContext:
    d, ok := store.DataAs[*store.RecentContextPartData](p)
    if ok && d.Excerpt != "" {
        parts = append(parts, llm.NewTextPart(d.Excerpt))
    }
```

2. `buildUserParts()` must accept a `opts ToModelOptions` parameter (same as
   `buildAssistantPartsWithOpts`) and skip `PartTypeRecentContext` when
   `opts.StripMedia` is true. This is required because on the **second compaction**,
   the previous boundary message enters `sel.Head` and is passed to
   `ToModelMessagesWithOptions(StripMedia=true)` for summary generation — without this
   guard, the verbatim excerpt from the previous compaction round would be fed to the
   summary LLM, adding token cost and semantic noise.

   The updated signature:
   ```go
   func buildUserParts(ps []*store.Part, opts ToModelOptions) []llm.ContentPart
   ```
   All three call sites must be updated:
   - `ToModelMessages` (normal path) → pass `ToModelOptions{}`
   - `ToModelMessagesWithOptions` (compaction summary path) → pass the same `opts`
     that already carries `StripMedia: true`

**`session/compaction.go` — `ToModelMessagesWithOptions` call site** (third location):

No code change needed here beyond updating the `buildUserParts` call signature above.
The summary path already passes `StripMedia: true`; once `buildUserParts` respects it,
the excerpt is automatically stripped from the summary LLM input.

**Design decisions:**

| Decision | Rationale |
|----------|-----------|
| Written into the boundary message, not a new separate message | The boundary is the compaction anchor; no change to `FilterCompacted` or message-counting logic |
| Step 5 non-fatal | Compaction committed at Step 4; excerpt is enhancement, not correctness |
| Only last 2 turns of head, guarded by `len(turns) > tailTurns+2` | Head can be arbitrarily long; guard avoids duplicating the tail (which is already verbatim); short-head case produces nil RecentHead and skips Step 5 |
| Verbatim truncated text, not another LLM summarisation | No extra LLM call, no extra latency; preserves exact details a summary would abstract away |
| `buildUserParts` opts + `StripMedia` guard | On second compaction the boundary message enters `sel.Head`; stripping `PartTypeRecentContext` prevents the previous excerpt from entering the summary LLM and growing token cost across repeated compactions |
| excerpt token budget | excerpt is written after `Select()` completes and is attached to the boundary message, which is always in `sel.Head` (never in tail). `estimateTurnTokens` uses `m.Tokens` for budget calculation; boundary message has `Tokens=zero` → fallback 100 tokens (compaction.go:160). This fallback under-counts the excerpt on the **next** compaction's head token estimate. Mitigation: after writing the `PartTypeRecentContext` part, call `store.UpdateMessage` to set `boundary.Tokens.Input = len(excerpt)/4` (rough char-to-token estimate). This prevents the excerpt from being invisible to the next round's token budget. |

**Boundary cases confirmed safe by code review:**

| Case | Status |
|------|--------|
| `len(turns) <= tailTurns` (history too short to compact) | Already handled: `Select()` returns `Head:nil`, `Compact()` exits early at line 222 — Step 5 never reached |
| `len(turns) == tailTurns+1` (head has exactly 1 real turn) | `RecentHead` guard (`len(turns) > tailTurns+2`) is false → `RecentHead=nil` → Step 5 skipped |
| Second (and subsequent) compactions — boundary enters `sel.Head` | `buildUserParts` `StripMedia` guard strips `PartTypeRecentContext`; boundary renders as `"What did we do so far?"` only — semantically correct for the summary LLM |
| `PartTypeRecentContext` token budget on next compaction | Mitigated by updating `boundary.Tokens.Input` after Step 5 write |
| tail verbatim preservation | Confirmed: `ToModelMessages` (normal path) does not truncate tool output; Prune protects the last 2 turns |

**Impact:** With these four changes, every compaction round leaves a verbatim anchor of
the 2 turns immediately preceding the tail embedded in the boundary message. The LLM
receives the anchor without any tool call, without relying on the summary's fidelity,
and without token cost growing unboundedly across repeated compactions (excerpt stripped
from summary input on subsequent rounds). This directly enables the long uninterrupted
conversation experience: topic continuity is preserved across an unlimited number of
compaction rounds.

