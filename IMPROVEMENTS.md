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
