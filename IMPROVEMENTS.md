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

### 6. `Compact()` writes boundary to store before LLM call — inconsistency window — `session/compaction.go`

**Location:** `compaction.go:237–299`
**Severity:** **High** — data loss risk on partial failure.

```go
// Step 1 — writes compaction boundary + summary placeholder to store
c.store.CreateMessage(ctx, &store.Message{...})   // boundary user msg
c.store.CreatePart(ctx, &store.Part{Type: PartTypeCompaction, ...})
c.store.CreateMessage(ctx, &store.Message{..., Summary: true}) // empty summary

// Step 2 — LLM call that may fail
result, err := c.processor.Process(ctx, summaryMsgID, summaryInput)
if err != nil {
    return "", fmt.Errorf("compaction: summary generation failed: %w", err)
}
```

If the LLM call fails (network error, provider error, context overflow), the
boundary user message and the empty summary message are already persisted.
On the next `RunLoop`, `FilterCompacted` walks backward, finds the summary
message (`Summary=true`), pairs it with the boundary, and returns only
`[boundary, empty-summary, tail]` — silently discarding all pre-compaction
history without any actual summary content. The session loses context
irreversibly.

**Fix:** reverse the order — run the LLM call first, then atomically write
both the boundary and the completed summary. If the store supports
transactions, wrap both writes. Otherwise buffer the summary text and only
write the boundary after the LLM succeeds:

```go
// Step 1: run LLM to get summary text (no store writes yet)
summaryText, err := generateSummary(ctx, headMsgs, summaryInput)
if err != nil {
    return "", fmt.Errorf("compaction: summary generation failed: %w", err)
}

// Step 2: only now write boundary + summary to store
c.store.CreateMessage(ctx, boundaryMsg)
c.store.CreatePart(ctx, compactionPart)
c.store.CreateMessage(ctx, summaryMsg)
c.store.CreatePart(ctx, summaryTextPart)
```

---

### 7. `processorState` concurrent map access — `session/processor.go`

**Location:** `processor.go:147–158`, `processor.go:296–313`, `processor.go:337–368`
**Severity:** **Medium** — data race under concurrent tool execution.

`executeTool` goroutines read `toolMap` and write back to the store concurrently
with `cleanup()` which iterates over `activeToolParts`. The `activeToolParts`
map is written by the main event loop (`handleEvent`) while goroutines may be
finishing and indirectly triggering `GetPart` / `UpdatePart`. Although the
current flow mostly serialises through the channel, there is no explicit
synchronisation protecting `activeToolParts` reads in `cleanup` from concurrent
goroutine writes:

```go
// handleEvent (main goroutine) — writes activeToolParts
s.activeToolParts[ev.ToolCallID] = id

// cleanup (called after stream ends) — reads activeToolParts
for callID, partID := range s.activeToolParts {
    // goroutines from executeTool may still be running here
    p, err := s.store.GetPart(timeoutCtx, partID)
}
```

**Fix:** protect `activeToolParts` with a `sync.Mutex`, or use a `sync.WaitGroup`
to ensure all tool goroutines have either completed or been cancelled before
`cleanup` iterates the map. Pass a cancellable context to tool goroutines so
cleanup can actually stop them, not just mark them interrupted.

---

### 8. `processorState` cleanup grace period multiplied by N tools — `session/processor.go`

**Location:** `processor.go:341–358`  
**Status:** Real bug — confirmed in current code.

```go
deadline := time.Now().Add(250 * time.Millisecond)
for callID, partID := range s.activeToolParts {
    timeoutCtx, cancel := context.WithDeadline(ctx, deadline)
    // GetPart call may block up to 250ms per tool
    p, err := s.store.GetPart(timeoutCtx, partID)
    cancel()
    ...
}
```

The single `deadline` is computed once, but each iteration of the loop may
block for the full remaining budget independently. With N concurrent tools the
worst-case wait is `N × 250 ms`. Additionally, tool goroutines that are still
running are only marked as interrupted in the store — they are not actually
cancelled and may continue writing to the store after `cleanup` returns.

**Fix:** create one shared deadline before the loop; cancel running tool
goroutines via a dedicated context stored in `processorState`.

---

### 9. `RunLoop` loads all messages on every agentic step — `session/prompt.go`

**Location:** `prompt.go:122–131`
**Severity:** **Medium** — unnecessary I/O overhead on long sessions.

```go
for {
    // Re-loads every message + every part on every step
    msgs, allParts, err := loadMessages(ctx, s, input.SessionID)
    ...
    msgs = FilterCompacted(msgs, allParts)
    modelMsgs, err := ToModelMessages(msgs, allParts)
}
```

Each agentic step calls `ListMessages` + `ListParts` for every message in the
session. For a session with 50 messages and 200 parts this means 51 store
queries per step. On a remote store (SQLite over network, SQL DB) this is
measurable latency. Only new messages are added between steps; the reload of
existing messages is pure waste.

**Fix:** cache the message list and parts map across the loop, and append only
newly created messages/parts after each step. Invalidate the cache only after
compaction (which restructures the history).

---

### 10. `Select()` uses position heuristic to identify compaction boundaries — `session/compaction.go`

**Location:** `compaction.go:96–103`
**Severity:** **Low** — brittle assumption on message ordering.

```go
// heuristic: a user message is a compaction anchor if the next message is a summary
isCompactionBoundary := false
if i+1 < len(msgs) && msgs[i+1].Summary {
    isCompactionBoundary = true
}
```

This relies on the summary assistant message being at index `i+1` immediately
after the boundary user message. The logic is order-dependent. A real user
message inserted between the boundary and the summary (e.g. by a concurrent
write or a bug in Compact's ordering) would break detection, causing the
compaction anchor to be treated as a real turn and included in the head to
be summarised again.

`FilterCompacted` already does the correct part-level check
(`PartTypeCompaction`). `Select()` should reuse that mechanism via the `parts`
map rather than relying on index proximity.

**Fix:** pass `allParts` to `Select()` and check the part type directly:

```go
func Select(msgs []*store.Message, parts map[string][]*store.Part, model llm.Model, cfg *config.Info) SelectResult {
    for i, m := range msgs {
        if m.Role != store.RoleUser {
            continue
        }
        isCompactionBoundary := hasPartType(parts[m.ID], store.PartTypeCompaction)
        if isCompactionBoundary {
            continue
        }
        turns = append(turns, Turn{...})
    }
}
```

---

### 11. `Prune()` is implemented but never called — `session/compaction.go` / `session/prompt.go`

**Location:** `compaction.go:424`, `prompt.go:198–208`
**Status:** **FIXED** — `RunLoop` fires a goroutine calling `Prune()` on every `ProcessStop`. `Prune()` guards with `cfg.Compaction.Prune` (opt-in, default false), so the call is always made but is a no-op unless explicitly enabled via `cfg.compaction.prune = true`.

---

## store

### 12. Weak type safety on `Part.Data` — `store/store.go`

**Location:** `store.go:83`, and 14 call sites across `session/`  
**Status:** Real — low severity, maintenance risk.

```go
Data any
```

Every caller performs an unchecked runtime type assertion:

```go
d, ok := p.Data.(*store.ToolPartData)   // processor.go:350, 396, 416, 440
d, ok := p.Data.(*store.TextPartData)   // context.go:76, 103, 184
d, ok := p.Data.(*store.ReasoningPartData) // context.go:114, 189
```

Mismatches from JSON round-trips fail silently (the `ok=false` branch is
typically ignored) with no compile-time protection. A new part type added
without updating all switch/assertion sites will silently produce empty data.

**Fix (option A):** sealed interface:

```go
type PartData interface{ partData() }
func (*TextPartData) partData()      {}
func (*ToolPartData) partData()      {}
func (*ReasoningPartData) partData() {}
```

**Fix (option B):** generic helper that centralises assertion failures:

```go
func DataAs[T any](p *Part) (T, bool) { v, ok := p.Data.(T); return v, ok }
```

---

## knowledge

### 13. `max_results` upper bound declared in schema but not enforced at runtime — `knowledge/search_tool.go`

**Location:** `search_tool.go:56–65`  
**Status:** ~~Real~~ — **FIXED**, confirmed in current code.

`maxResultsCap = 20` is enforced at runtime. No action needed.

---

### 14. Cross-priority-group results have no global score sort — `knowledge/manager.go`

**Location:** `manager.go:113–133`  
**Status:** Real — low severity, intentional trade-off or oversight.

Results within a group are sorted by score descending via `dispatchGroup`.
Results from different priority groups are appended in priority order only. A
lower-priority group may contain higher-scoring results than the tail of a
higher-priority group, causing sub-optimal result ranking.

**Fix:** apply a final global sort after accumulating all groups, or document
that priority ordering intentionally overrides score.

```go
sort.Slice(accumulated, func(i, j int) bool {
    return accumulated[i].Score > accumulated[j].Score
})
```

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
| 6 | `session` | Cleanup grace period multiplied by N tools — incorrect timeout logic | Medium | Open |
| 7 | `store` | Weak `Part.Data` typing — silent assertion failures, no compile-time safety | Low | Open |
| 8 | `knowledge` | Cross-group results have no global score sort — sub-optimal ranking | Low | Open |
| 9 | `session` | `Compact()` writes boundary before LLM call — data loss on partial failure | **High** | Open |
| 10 | `session` | `processorState` concurrent map access — data race under concurrent tools | Medium | Open |
| 11 | `session` | `RunLoop` reloads all messages on every agentic step — unnecessary I/O | Medium | Open |
| 12 | `session` | `Select()` uses position heuristic for compaction boundary detection | Low | Open |
| 13 | `session` | `Prune()` never called by default — dead feature | Low | **FIXED** |
| 14 | `cmd` | `NewFromConfig` never called — `llm.json` provider config inert | Medium | **FIXED** |
| 15 | `provider` | `Registry` never instantiated — extensibility blocked | Medium | **FIXED** |

---

## Previously Reported — Confirmed Fixed

| Original # | Issue | Fix location |
|---|---|---|
| 1 | Retry emits partial events to caller | `llm/client.go` — buffer per attempt, flush on `EventRequestFinish` |
| 2 | Anthropic reasoning replayed as `text` block | `provider/anthropic/anthropic.go` — `ThinkingBlockParam` with `Signature` |
| 3 | `ThinkingBlock` signature not captured | `anthropic.go` — `ThinkingBlock` case + `EventReasoningEnd`; `store.ReasoningPartData.Signature` |
| 4 | OpenAI `NewFromConfig` ignores extra headers | `provider/openai/openai.go` — reads `cfg.Options.Extra` |
| 9 (old) | `NewFromConfig` never called in cmd | `cmd/control/main.go`, `cmd/knowledge-api/main.go` — registry wired |
| 10 (old) | `provider.Registry` dead code | `provider/provider.go` — `RegisterFactory` + `BuildProvider` added |
| 13 (orig) | `Prune()` dead code — never called | `prompt.go` — unconditional goroutine on `ProcessStop`; `Prune()` guards with opt-in flag internally |
| 2 (orig) | `classifyStatus` case 529 unreachable | `error.go` — `case 529` now precedes `>= 500` |
| 3 (orig) | `MaxOutputTokens` returns 0 for unset output | `overflow.go` — `fallback = 4096` added |
| 4 (orig) | Anthropic image block uses wrong constructor | `anthropic.go` — `NewImageBlockBase64` / `NewImageBlock` correctly routed |
| 5 (orig) | Anthropic system prompt loses cache granularity | `anthropic.go` — individual `TextBlockParam` entries preserved |
| 6 (orig) | OpenAI `EventToolInputStart` may never be emitted | `openai.go` — `ts.started` flag defers until both id and name are known |
| 10 (orig) | `newID()` non-unique (UnixNano) | `processor.go` — `crypto/rand` hex |
| 13 (orig) | Token estimation placeholder (constant 100) | `compaction.go` — uses `m.Tokens` with 100 fallback |
| 14 (orig) | `EventToolResult` silently ignored | `processor.go` — `log.Printf` warning emitted |
| 15 (orig) | `FilterCompacted` may mis-pair boundary and summary | `context.go` — backward walk finds most recent complete pair |
| 16 (orig) | Double `loadMessages` per loop iteration | `prompt.go` — `ProcessContinue` uses `s.ListParts(assistantMsgID)` directly |
| 23 (orig) | `max_results` upper bound not enforced at runtime | `search_tool.go` — `maxResultsCap = 20` enforced |
| 25 (orig) | `fetch` fallback passes un-stripped `q.Input` to `Accepts` | `manager.go` — prefix stripped before fallback loop |
| 26 (orig) | `groupByPriority` dead `peekMode` parameter | `manager.go` — parameter removed |
