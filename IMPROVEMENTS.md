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
**Status:** Real — low severity, confirmed in current code.

```go
StreamOptions: openai.ChatCompletionStreamOptionsParam{
    IncludeUsage: openai.Bool(true),
},
```

This is an OpenAI-specific extension. Some OpenAI-compatible proxies and local
model servers reject requests with unknown fields.

**Fix:** gate behind a known-safe provider allowlist, or make configurable via
a provider-level capability flag.

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

### 10. `NewFromConfig` has no callers — `llm.json` provider config is inert — `cmd/control/main.go`, `cmd/knowledge-api/main.go`

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
| 5 | `provider/openai` | `IncludeUsage` sent to all compatible providers — proxy rejection risk | Low | Open |
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

Only **#5** (IncludeUsage compatibility) remains fully open.

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
| 16 | `NewFromConfig` never called in cmd | `cmd/control/main.go`, `cmd/knowledge-api/main.go` — registry wired |
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
that respects context cancellation. Callers (e.g. `cmd/knowledge-api`) call it once at
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

