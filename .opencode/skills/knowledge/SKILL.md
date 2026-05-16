---
name: knowledge
description: Work with the knowledge manager package in github.com/larryhou/llm-go — register new Sources, maintain existing ones, and understand the full dispatch architecture
---

# Knowledge Manager

The `knowledge/` package is the LLM's pluggable information retrieval layer.
It exposes external knowledge backends to the model as two tool calls
(`knowledge_search`, `knowledge_fetch`) that are injected into context
**only when the LLM explicitly invokes them** — never proactively.

## Package Layout

```
llm-go/
└── knowledge/
    ├── knowledge.go             QueryType, Query, Result, CompactionHook — core types
    ├── source.go                Source interface           — backend contract
    ├── manager.go               Manager                   — routing & dispatch
    ├── search_tool.go           knowledge_search          — tool.Tool impl
    ├── fetch_tool.go            knowledge_fetch           — tool.Tool impl
    ├── session_history.go       SessionHistorySource      — compacted history recall
    ├── knowledge_test.go        unit tests (14 cases)
    ├── gsetokenizer/
    │   └── gsetokenizer.go      gse bleve tokenizer adapter (Chinese segmentation)
    └── source/
        └── bleve/
            └── bleve.go         BleveSource               — reference impl
```

---

## Core Types (`knowledge.go`)

```go
type QueryType string
const (
    QueryTypeSearch QueryType = "search"  // broad exploration → Peek()
    QueryTypeFetch  QueryType = "fetch"   // precise retrieval  → Fetch()
)

type Query struct {
    Type       QueryType
    Input      string         // search terms | RefID | URL
    Filters    map[string]any // optional field constraints
    MaxResults int            // 0 = source default
}

type Result struct {
    RefID    string         // "{sourceID}:{internal-key}" — used by Fetch routing
    Title    string
    Source   string         // matches Source.ID()
    Score    float64        // [0,1]; -1 if unavailable
    Metadata map[string]any
    Snippet  string         // populated by Peek (Content empty)
    Content  string         // populated by Fetch (Snippet optional)
}

// CompactionHook is called after a successful Compact() with the head messages
// and their parts. Used by SessionHistorySource to index compacted history.
// Note: parts contains all session parts, not just head — only access parts[m.ID]
// for m in head.
type CompactionHook func(head []*store.Message, parts map[string][]*store.Part)
```

---

## Architecture Flow

### Search (knowledge_search → Peek)

```mermaid
sequenceDiagram
    participant LLM
    participant searchTool as knowledge_search
    participant Manager
    participant G0 as Group(pri=0)
    participant G1 as Group(pri=1)
    participant S0 as Source A (pri=0)
    participant S1 as Source B (pri=0)
    participant S2 as Source C (pri=1)

    LLM->>searchTool: Execute({query:"golang concurrency", max_results:5})
    searchTool->>Manager: peek(Query{Type:Search, Input:"golang concurrency"})
    Manager->>Manager: groupByPriority() → [[A,B],[C]]

    rect rgb(220,240,255)
        note over G0: Priority group 0 — concurrent
        Manager->>S0: Peek(ctx+timeout, q)
        Manager->>S1: Peek(ctx+timeout, q)
        S0-->>Manager: []Result{score:0.9, snippet:...}
        S1-->>Manager: []Result{score:0.7, snippet:...}
    end

    Manager->>Manager: sort by Score desc, truncate Snippets
    Manager->>Manager: len(accumulated)=2 < MaxResults=5 → continue

    rect rgb(255,240,220)
        note over G1: Priority group 1 — concurrent
        Manager->>S2: Peek(ctx+timeout, q)
        S2-->>Manager: []Result{score:0.5, snippet:...}
    end

    Manager->>Manager: accumulated=3, cap to MaxResults
    Manager-->>searchTool: []Result (3 items)
    searchTool-->>LLM: Markdown snippet list\n(RefIDs embedded for follow-up)
```

### Fetch (knowledge_fetch → Fetch)

```mermaid
sequenceDiagram
    participant LLM
    participant fetchTool as knowledge_fetch
    participant Manager
    participant SA as Source A (id="wiki")
    participant SB as Source B (id="web")

    LLM->>fetchTool: Execute({ref_id:"wiki:doc-42"})
    fetchTool->>Manager: fetch(Query{Type:Fetch, Input:"wiki:doc-42"})
    Manager->>Manager: strings.Cut("wiki:doc-42",":") → sourceID="wiki", key="doc-42"
    Manager->>SA: Fetch(ctx, Query{Input:"doc-42"})
    note over SB: NOT called — direct routing via prefix
    SA-->>Manager: []Result{Content:"full text..."}
    Manager->>Manager: truncateContent(ContentMaxChars)
    Manager-->>fetchTool: []Result
    fetchTool-->>LLM: Markdown full-content block
```

### Priority Group Dispatch (detail)

```mermaid
flowchart TD
    A[peek called] --> B[groupByPriority\nfilter Accepts=true\nsort by Priority asc]
    B --> C{next group?}
    C -- yes --> D[dispatchGroup\nconcurrent goroutines\none per source]
    D --> E[callSource\nwith SourceTimeout ctx]
    E --> F{source error?}
    F -- AllowPartialFailure=true --> G[skip, continue collecting]
    F -- AllowPartialFailure=false --> H[return error]
    G --> I[sort group results by Score desc]
    I --> J{accumulated >= MaxResults?}
    J -- yes --> K[stop — skip lower-priority groups]
    J -- no --> C
    C -- no more groups --> L[cap to MaxResults\nreturn results]
    K --> L
```

---

## Session History Recall (`session_history.go`)

`SessionHistorySource` is a `knowledge.Source` that indexes compacted messages
into a private in-memory Bleve index per session. After each `Compact()`, the
head messages are indexed; the LLM can then retrieve them via the standard
`knowledge_search` / `knowledge_fetch` tools without any new LLM-facing APIs.

### Why it exists

After `Compact()`, `FilterCompacted` hides all messages before the compaction
boundary. The LLM only sees `[summary + recent N turns]` and cannot reach older
context. `SessionHistorySource` makes those hidden turns searchable on demand.

### Memory model

- Each session owns one `bleve.NewMemOnly` index — no cross-session leakage.
- **`maxCompactions = 8`** (default): retains at most 8 compaction rounds.
  Each round ≈ 5–6 MB → index cap ≈ **50 MB per session**.
- On overflow, the oldest round's documents are deleted via `index.Delete(docID)`.
- Index is released when the `SessionHistorySource` is GC'd (session end).
- 100 concurrent sessions × 50 MB = 5 GB — safe on a 64 GB server.

### Chinese-first indexing

The `gsetokenizer` package (in `knowledge/gsetokenizer/`) registers a Bleve
tokenizer backed by `go-ego/gse` (Go port of jieba). The `Text` field uses a
custom `gse_lowercase` analyzer (gse tokenizer + lowercase filter), giving
accurate Chinese word segmentation instead of the noisy CJK unigram fallback.

### HistoryDoc structure

```go
type HistoryDoc struct {
    ID            string   // store message ID
    Role          string   // "user" | "assistant"
    Text          string   // all text parts concatenated
    ToolCalls     []string // tool names invoked in this turn
    TurnIndex     int      // position in compaction head (0-based)
    CompactionSeq int      // which compaction round (for pruning)
    CreatedAt     int64    // unix ms
}
```

### Lifecycle

```mermaid
flowchart TD
    A[session created] --> B["NewSessionHistorySource(sessID, maxCompactions=8)\nprivate bleve.NewMemOnly index\ngse_lowercase analyzer"]
    B --> C["km := knowledge.NewManager()\nkm.Register(historySrc)  ← priority 0\nkm.Register(skillsSrc)   ← priority 1"]
    C --> D["session.RunLoop(..., RunInput{\n  Tools:     km.Tools(),\n  OnCompact: sess.hook,\n})"]
    D --> E{LLM turn}
    E --> F{context full?}
    F -- no --> E
    F -- yes --> G["Compact() fires\n→ summary generated\n→ CompactionPart stored"]
    G --> H["input.OnCompact(sel.Head, allParts)\n→ SessionHistorySource.Hook()"]
    H --> I{len compactionDocs\n>= maxCompactions?}
    I -- yes --> J["delete oldest round docs\nfrom Bleve index"]
    I -- no --> K["index each head message\nas HistoryDoc\ntrack docIDs by CompactionSeq"]
    J --> K
    K --> E
    E --> L[session ends]
    L --> M[src GC'd → Bleve index released]
```

### Recall flow (LLM retrieves compacted history)

```mermaid
sequenceDiagram
    participant LLM
    participant searchTool as knowledge_search
    participant Manager
    participant Hist as SessionHistorySource\n(pri=0, id="session-history")
    participant Skills as BleveSource\n(pri=1, id="skills")

    note over LLM: Context only shows summary + recent turns.\nUser asks about earlier decision.

    LLM->>searchTool: Execute({query:"上次讨论的接口设计"})
    searchTool->>Manager: peek(Query{Input:"上次讨论的接口设计"})
    Manager->>Manager: groupByPriority → [[Hist],[Skills]]

    rect rgb(220,240,255)
        note over Hist: Priority 0 — queried first
        Manager->>Hist: Peek(ctx, q)
        note over Hist: gse segments "上次讨论的接口设计"\n→ [上次, 讨论, 接口, 设计]\nBleve full-text search
        Hist-->>Manager: []Result{snippet:"[来源：历史对话 第2轮 turn#14]...", refID:"session-history:msg-xyz"}
    end

    Manager->>Manager: accumulated=1 < MaxResults → continue to Skills
    rect rgb(255,240,220)
        Manager->>Skills: Peek(ctx, q)
        Skills-->>Manager: []Result{}
    end

    Manager-->>searchTool: []Result (1 item)
    searchTool-->>LLM: snippet + ref_id

    LLM->>searchTool: Execute({ref_id:"session-history:msg-xyz"})
    Note right of LLM: LLM decides to fetch full text
    searchTool->>Manager: fetch(Query{Input:"session-history:msg-xyz"})
    Manager->>Hist: Fetch(ctx, q)
    Hist-->>Manager: []Result{Content:"[来源：历史对话 第2轮 turn#14 role=user]\n完整原文..."}
    Manager-->>searchTool: result
    searchTool-->>LLM: full original message text
```

### Wiring in `cmd/knowledge-api/main.go`

Each session creates its state once and caches it on `chatSession`:

```go
// On first request for a session:
historySrc, _ := knowledge.NewSessionHistorySource(sessID, knowledge.DefaultMaxCompactions)

sessKM := knowledge.NewManager(knowledge.ManagerConfig{
    SourceTimeout:       10 * time.Second,
    MaxResults:          10,
    SnippetMaxChars:     cfg.snippetMax,
    ContentMaxChars:     cfg.contentMax,
    AllowPartialFailure: false,
})
sessKM.Register(blevesource.New(skillsIdx, "skills", 1, nil)) // priority 1
sessKM.Register(historySrc)                                    // priority 0

// resetFn holds s.mu so DeleteSession+CreateSession+Reset are atomic
// with respect to concurrent /chat requests for the same session.
resetFn := func(ctx context.Context) error {
    s.mu.Lock()
    defer s.mu.Unlock()
    if err := st.DeleteSession(ctx, sessID); err != nil {
        return err
    }
    if err := st.CreateSession(ctx, &store.Session{ID: sessID}); err != nil {
        return err
    }
    return historySrc.Reset()
}

sess = &chatSession{
    historySrc: historySrc,
    km:         sessKM,
    hook:       historySrc.Hook(),                    // cached — not re-created per request
    resetTool:  session.NewResetTool(resetFn),        // cached — not re-created per request
}

// In RunLoop:
kmTools := sess.km.Tools()
allTools := make([]tool.Tool, 0, len(kmTools)+1)
allTools = append(allTools, kmTools...)
allTools = append(allTools, sess.resetTool) // explicit make avoids km.Tools() array aliasing

session.RunLoop(ctx, store, session.RunInput{
    Tools:     allTools,
    OnCompact: sess.hook,
})
```

### Peek snippet format

- **Fragment size**: 150 characters around each matched term
- **Max fragments per document**: 3 (joined with ` … `)
- **Max results**: 5 documents
- **HTML tag stripping**: `<[^>]+>` regex (handles any highlighter style)
- **Fallback**: if no highlight fragments, truncates stored text to 150 chars

Snippet prefix:
```
[来源：历史对话 第{CompactionSeq}轮 turn#{TurnIndex} role={role}]
```

This tells the LLM the content is prior conversation (may be outdated), not
authoritative knowledge-base content.

### System prompt guidance

When `RunInput.OnCompact != nil`, `buildSystem` appends `PromptKnowledgeRecall`
(embedded from `session/knowledge-recall.txt`):

```
如果你需要回忆当前上下文中已看不到的早期对话内容，使用 knowledge_search 检索历史记录。
...
注意：session-history 来源反映的是当时的讨论内容，可能已经过时。
```

---

## Source Interface (`source.go`)

Every backend must implement four methods:

```go
type Source interface {
    ID()       string            // unique, URL-safe, stable  e.g. "internal-wiki"
    Priority() int               // lower = higher priority; same value = same group
    Accepts(q Query) bool        // routing filter — called before every Peek/Fetch
    Peek(ctx, q Query) ([]Result, error)   // lightweight: snippets only
    Fetch(ctx, q Query) ([]Result, error)  // full content by RefID
}
```

### Accepts() routing matrix

| Source type | QueryTypeSearch | QueryTypeFetch |
|-------------|:--------------:|:--------------:|
| BleveSource | ✓ | ✓ |
| SessionHistorySource | ✓ | ✓ |
| WebSource (future) | ✓ | ✓ |
| VectorDB (future) | ✓ | ✗ |
| SQL DB (future) | ✗ | ✓ |

### RefID convention

```
Result.RefID = "{Source.ID()}:{internal-key}"
examples:
  "internal-wiki:page-42"
  "session-history:msg-abc123"
  "web:https://example.com/doc"
  "pg:users/row/12345"
```

Manager.fetch() uses `strings.Cut(refID, ":")` to extract the sourceID prefix
and route directly to the matching source — O(n) scan only on unknown prefixes.

---

## Registering a New Source

### Step 1 — Create the package

```
knowledge/source/
└── mybackend/
    └── mybackend.go
```

### Step 2 — Implement the interface

```go
package mybackend

import (
    "context"
    "github.com/larryhou/llm-go/knowledge"
)

type Source struct {
    id       string
    priority int
    // ... backend-specific fields (client, connection, config)
}

func New(id string, priority int /* backend args */) *Source {
    return &Source{id: id, priority: priority}
}

func (s *Source) ID() string    { return s.id }
func (s *Source) Priority() int { return s.priority }

func (s *Source) Accepts(q knowledge.Query) bool {
    return q.Type == knowledge.QueryTypeSearch
}

func (s *Source) Peek(ctx context.Context, q knowledge.Query) ([]knowledge.Result, error) {
    results := []knowledge.Result{}
    // ... backend call
    return results, nil
}

func (s *Source) Fetch(ctx context.Context, q knowledge.Query) ([]knowledge.Result, error) {
    docID := strings.TrimPrefix(q.Input, s.id+":")
    // ... backend call
    return []knowledge.Result{{
        RefID:   s.id + ":" + docID,
        Title:   "...",
        Source:  s.id,
        Score:   -1,
        Content: "...",
    }}, nil
}
```

### Step 3 — Register with the Manager

```go
km := knowledge.NewManager(knowledge.ManagerConfig{
    SourceTimeout:       10 * time.Second,
    MaxResults:          5,
    SnippetMaxChars:     300,
    ContentMaxChars:     8000,
    AllowPartialFailure: false,
})

km.Register(blevesource.New(bleveIdx, "wiki", 0, nil))
km.Register(mybackend.New("my-source", 1))

session.RunLoop(ctx, store, session.RunInput{
    Tools: append(existingTools, km.Tools()...),
})
```

### Step 4 — Write tests

Use a `stubSource` (see `knowledge_test.go`) to test routing logic without
a real backend. Add backend-specific integration tests in your source package.

```go
func TestMySource_Peek(t *testing.T) {
    src := mybackend.New("test", 0)
    q := knowledge.Query{Type: knowledge.QueryTypeSearch, Input: "foo", MaxResults: 3}
    results, err := src.Peek(context.Background(), q)
    // assert RefID prefix, non-empty Snippet, empty Content
}
```

---

## Manager Configuration Reference

| Field | Type | Default | Effect |
|-------|------|---------|--------|
| `SourceTimeout` | `time.Duration` | 0 (inherit ctx) | Per-source deadline; ctx.Done() on timeout |
| `MaxResults` | `int` | 0 (unlimited) | Caps total Peek results; stops querying lower-priority groups early |
| `SnippetMaxChars` | `int` | 0 (no truncation) | Truncates `Result.Snippet` after Peek |
| `ContentMaxChars` | `int` | 0 (no truncation) | Truncates `Result.Content` after Fetch |
| `AllowPartialFailure` | `bool` | false | When true, a failing source is skipped; others still return results |

---

## Two-Level Retrieval Design

```mermaid
flowchart LR
    LLM -->|"1. knowledge_search(query)"| Peek
    Peek -->|"compact snippets\n+ RefIDs"| LLM
    LLM -->|"2. knowledge_fetch(ref_id)\nonly if needed"| Fetch
    Fetch -->|"full content\n(truncated)"| LLM
    LLM -->|"3. answer using\ninjected context"| User

    style Peek fill:#d4edda,stroke:#28a745
    style Fetch fill:#fff3cd,stroke:#ffc107
```

**Why two levels?**

- `Peek` injects only compact snippets — minimal context growth per search.
- `Fetch` is called only when the LLM decides it needs full content.
- A search returning 5 snippets × 300 chars = 1500 chars added to context.
- Only the 1–2 items actually needed get fetched (8000 chars each, on demand).
- Compaction handles old tool results automatically via `ToolOutputMaxChars`.

---

## Maintenance Checklist

### Adding a new QueryType
1. Add constant to `knowledge.go` with doc comment.
2. Update `Source.Accepts()` in all existing sources that should handle it.
3. Decide whether it routes to `Peek` or `Fetch` in `Manager.peek()`/`Manager.fetch()`.
4. Add a test in `knowledge_test.go` verifying routing.

### Changing Manager dispatch logic
- `groupByPriority()` — partitions sources; changing this affects all Peek calls.
- `dispatchGroup()` — concurrent fan-out; keep goroutine-safe.
- `fetch()` — `strings.Cut` prefix routing; keep the fallback path.

### Changing Result fields
- `Snippet` is for Peek output; keep compact (< `SnippetMaxChars`).
- `Content` is for Fetch output; may be large; always truncated by Manager.
- `RefID` format `"{sourceID}:{key}"` is a routing contract — do not change the separator.
- `Score` must be in `[0,1]` or `-1`; Manager sorts by Score descending within groups.

### BleveSource field mapping
If the Bleve index uses non-default field names, pass a `*bleve.Config`:

```go
blevesource.New(idx, "docs", 0, &blevesource.Config{
    TitleField:   "name",    // default: "title"
    ContentField: "body",    // default: "content"
})
```

### SessionHistorySource tuning
- `maxCompactions` controls memory cap per session (default 8 ≈ 50 MB).
- The gse tokenizer loads its dictionary on first use via `seg.LoadDict()`.
- `CompactionHook` is called synchronously inside `Compact()` — keep it fast.
- `sess.hook` must be cached on `chatSession` (not re-created per request).

### Session reset (`session_reset` tool)
- `session.NewResetTool(resetFn)` — the `resetFn` callback must be wired under the server's session lock to prevent concurrent request races between `DeleteSession` and `CreateSession`.
- `historySrc.Reset()` sets `s.index = nil` before rebuilding — all `Peek`/`Fetch`/`Hook` calls guard against nil index.
- Cache `resetTool` on `chatSession` alongside `hook`; do not recreate per request.
- Build `allTools` with an explicit `make([]tool.Tool, 0, cap)` to avoid aliasing `km.Tools()`'s backing array.

---

## Key Files

| File | Purpose |
|------|---------|
| `knowledge/knowledge.go` | `QueryType`, `Query`, `Result`, `CompactionHook` — edit when adding query types or result fields |
| `knowledge/source.go` | `Source` interface — the only contract all backends must satisfy |
| `knowledge/manager.go` | Routing, priority groups, concurrency, truncation — core dispatch engine |
| `knowledge/search_tool.go` | `knowledge_search` tool exposed to LLM — input schema + Peek invocation |
| `knowledge/fetch_tool.go` | `knowledge_fetch` tool exposed to LLM — input schema + Fetch invocation |
| `knowledge/session_history.go` | `SessionHistorySource` — per-session compaction history recall with gse |
| `knowledge/gsetokenizer/gsetokenizer.go` | gse Bleve tokenizer adapter — registered via `init()` |
| `knowledge/knowledge_test.go` | 14 unit tests covering routing, priority, truncation, timeout, partial failure |
| `knowledge/source/bleve/bleve.go` | Reference Source implementation — use as template for new sources |
| `session/reset_tool.go` | `session_reset` built-in tool — atomic store delete + index reset via callback |
| `session/knowledge-recall.txt` | Chinese system prompt guidance for `knowledge_search` recall |
