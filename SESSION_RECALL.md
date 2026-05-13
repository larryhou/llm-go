# session_recall — Session History Recall

## Background

After `Compact()`, the LLM only sees `[compaction boundary + summary + recent N turns]`.
All messages before the boundary are still in the store but invisible to the LLM —
`FilterCompacted` hides them. The LLM has no way to reach older context.

The goal is to let the LLM **recall** specific older conversations on demand, without
loading everything back into context.

---

## Design

Implement `session_recall` as a `knowledge.Source` that indexes compacted history
and exposes it via the existing `knowledge_search` / `knowledge_fetch` tools.
No new tool types, no new LLM-facing APIs.

```
LLM calls knowledge_search("上次讨论的接口设计")
  → Manager dispatches to all registered Sources
  → SessionHistorySource.Peek() queries the Bleve index
  → returns matching turn snippets + ref_ids

LLM calls knowledge_fetch("session-history:{messageID}")
  → Manager routes to SessionHistorySource.Fetch()
  → returns full original message text
```

---

## Index approach: index individual messages (not summaries)

At compaction time, index every message in `sel.Head` (the turns being compacted)
as an individual document. This gives per-message retrieval granularity — the LLM
can locate a specific turn, not just a summary blob.

**Indexed document structure (`HistoryDoc`):**

```go
type HistoryDoc struct {
    ID            string   // store message ID
    Role          string   // "user" | "assistant"
    Text          string   // all text parts concatenated
    ToolCalls     []string // tool names invoked in this turn
    TurnIndex     int      // position in conversation
    CompactionSeq int      // which compaction round produced this doc (for pruning)
    CreatedAt     int64    // unix timestamp
}
```

`CompactionSeq` is required for `maxCompactions` cleanup: when the number of
compaction rounds exceeds the limit, all documents from the oldest round are
deleted via `index.Delete(docID)`.

---

## Chinese-first indexing: gse analyzer

The primary use case is Chinese conversation. The default Bleve analyzer (English
porter stemmer) degrades to single-character tokenization for CJK text, producing
noisy tokens like `口设` from `接口设计` and causing poor recall.

Use **gse** (Go Segment, the Go port of jieba) via the official Bleve integration:

```
github.com/blevesearch/blevex/analysis/lang/cjk/gse
```

Index mapping:
- `Text` field: analyzer `"gse"` — proper Chinese word segmentation
- `ToolCalls` field: analyzer `"keyword"` — tool names must not be split
- `Role`, `ID` fields: analyzer `"keyword"`

This gives accurate segmentation for queries like `接口设计`, `上次讨论`, `那个函数` etc.

---

## Memory bound: maxCompactions = 8 (~50 MB per session)

Each compaction round indexes ~300–400 messages at ~15 KB/doc in Bleve's
gtreap in-memory store, totalling ~5–6 MB per round.

`maxCompactions = 8` caps the index at **~50 MB per session**.

On a 64 GB server with 100 concurrent users: 100 × 50 MB = **5 GB**, well within budget.

When a new compaction round would exceed `maxCompactions`, the oldest round's
documents are deleted first:

```go
if len(src.compactionDocs) >= src.maxCompactions {
    oldest := src.oldestSeq()
    for _, id := range src.compactionDocs[oldest] {
        src.index.Delete(id)
    }
    delete(src.compactionDocs, oldest)
}
```

`compactionDocs map[int][]string` tracks doc IDs per compaction round.

---

## Session isolation — final design decision

Each `SessionHistorySource` owns its own `bleve.NewMemOnly` index. The source is
created once per session by the caller, registered into a session-private
`knowledge.Manager`, and passed into `RunLoop` via `RunInput.OnCompact`.

**Why not a shared Store-level index with `sessionID` filter:**
- Physical isolation needs no per-query `sessionID` filter — fewer moving parts
- Session lifetime maps cleanly to object lifetime — no explicit cleanup needed
- Multiple concurrent sessions never contend on the same index lock
- Sessions are by design isolated; no cross-session merge is planned

### Source ID

```go
func (s *SessionHistorySource) ID() string { return "session-history" }
```

The ID is a fixed constant `"session-history"`. Since each session has its own
private `knowledge.Manager`, there is no collision. The ID signals to the LLM
that results come from *this session's* compacted history, distinct from
knowledge-base sources.

### Complete lifecycle

```
session created
  └── NewSessionHistorySource(sessID, maxCompactions)  → private Bleve in-memory index
      └── km := knowledge.NewManager()
          └── km.Register(src)

RunLoop called with:
  Tools:     km.Tools()          ← knowledge_search/fetch scoped to this session
  OnCompact: src.Hook()          ← writes head messages into this session's index

Compact() fires
  ├── original logic (summary generation, CompactionPart stored)
  └── input.OnCompact(sel.Head, allParts)
        └── prune oldest round if len(compactionDocs) >= maxCompactions
            src.index.Index(m.ID, buildDoc(m, parts, seq)) for each head message

LLM calls knowledge_search("...")
  └── km → src.Peek()
        └── Bleve search on *this session's index only*
              → zero cross-session leakage

session ends
  └── src goes out of scope → GC releases Bleve index
```

---

## Peek: snippet definition

`Peek` uses Bleve's built-in **highlight** to return keyword-in-context snippets:

- **Fragment size**: 150 characters around each matched term
- **Max fragments per document**: 3
- **Max results returned**: 5 documents

This gives the LLM enough context to judge relevance (~2250 chars total) without
consuming excessive context window. If a snippet is relevant, the LLM calls
`knowledge_fetch` with the `ref_id` to retrieve the full message text.

Result format:
```
[来源：历史对话 第{CompactionSeq}轮 turn#{TurnIndex}]
{snippet}
ref_id: session-history:{messageID}
```

The source annotation tells the LLM this is *prior conversation*, not authoritative
knowledge-base content. It may reflect a past decision that has since changed.

---

## Interface changes

### 1. `session/prompt.go` — new type + new field on `RunInput`

```go
// CompactionHook is called after a successful Compact(), receiving the head
// messages that were compacted and their associated parts.
// Used by SessionHistorySource to index compacted history without coupling
// compaction logic to the index implementation.
// Nil is a no-op — existing callers are unaffected.
type CompactionHook func(head []*store.Message, parts map[string][]*store.Part)

type RunInput struct {
    // ... existing fields unchanged ...

    // OnCompact, when non-nil, is called after each successful Compact().
    OnCompact CompactionHook
}
```

### 2. `session/compaction.go` — call the hook

```go
// At the end of Compact(), after CompactionPart is stored:
if input.OnCompact != nil {
    input.OnCompact(sel.Head, allParts)
}
```

No other changes to core paths.

---

## `SessionHistorySource` — `knowledge/session_history.go`

```go
type SessionHistorySource struct {
    sessionID      string
    index          bleve.Index          // private in-memory index with gse analyzer
    maxCompactions int                  // default 8, ~50 MB per session
    compactionDocs map[int][]string     // seq → []docID, for pruning
    currentSeq     int                  // incremented each Hook() call
    mu             sync.Mutex
}

func NewSessionHistorySource(sessionID string, maxCompactions int) (*SessionHistorySource, error) {
    mapping := buildGseIndexMapping()
    idx, err := bleve.NewMemOnly(mapping)
    if err != nil {
        return nil, err
    }
    return &SessionHistorySource{
        sessionID:      sessionID,
        index:          idx,
        maxCompactions: maxCompactions,
        compactionDocs: make(map[int][]string),
    }, nil
}

// knowledge.Source interface
func (s *SessionHistorySource) ID() string    { return "session-history" }
func (s *SessionHistorySource) Priority() int { return 0 } // highest priority
func (s *SessionHistorySource) Accepts(q Query) bool {
    return q.Type == QueryTypeSearch || q.Type == QueryTypeFetch
}
func (s *SessionHistorySource) Peek(ctx context.Context, q Query) ([]Result, error)
func (s *SessionHistorySource) Fetch(ctx context.Context, q Query) ([]Result, error)

// Hook returns a CompactionHook that indexes head messages into this source.
// Pass the result to RunInput.OnCompact.
func (s *SessionHistorySource) Hook() session.CompactionHook {
    return func(head []*store.Message, parts map[string][]*store.Part) {
        s.mu.Lock()
        defer s.mu.Unlock()
        s.currentSeq++
        // prune oldest round if over limit
        if len(s.compactionDocs) >= s.maxCompactions {
            oldest := s.oldestSeq()
            for _, id := range s.compactionDocs[oldest] {
                s.index.Delete(id)
            }
            delete(s.compactionDocs, oldest)
        }
        // index new round
        var ids []string
        for i, m := range head {
            doc := buildDoc(m, parts[m.ID], s.currentSeq, i)
            s.index.Index(m.ID, doc)
            ids = append(ids, m.ID)
        }
        s.compactionDocs[s.currentSeq] = ids
    }
}
```

- `Peek`: Bleve highlight search, fragment 150 chars × 3 frags, top 5 results,
  return snippets with `[来源：历史对话 第N轮 turn#M]` prefix and `ref_id`
- `Fetch`: retrieve full `HistoryDoc.Text` by doc ID, return as `Content`

---

## system prompt addition (`session/system.go`)

Add to the tools guidance section:

```
如果你需要回忆当前上下文中已看不到的早期对话内容，使用 knowledge_search 检索历史记录。
典型场景：
- 用户提到"上次说的方案"但当前上下文中找不到
- 需要确认某个早期决策或设计细节
- 当前摘要与你的记忆有出入，需要查证原文

检索到相关片段后，可用 knowledge_fetch 获取完整原文。
注意：session-history 来源反映的是当时的讨论内容，可能已经过时，
请结合当前上下文判断是否仍然适用。
```

---

## Caller wiring (`cmd/knowledge-api/main.go`)

```go
src, err := knowledge.NewSessionHistorySource(sessID, 8)
if err != nil {
    // handle
}
km := knowledge.NewManager(knowledge.ManagerConfig{...})
km.Register(src)

session.RunLoop(ctx, st, session.RunInput{
    SessionID: sessID,
    Tools:     km.Tools(),
    OnCompact: src.Hook(),
    // ... other fields unchanged
})
```

---

## Open issues

**1. Index lifecycle matches store lifetime.**
Memory store is in-process; if the store is cleared or the process restarts, the index
is also gone. No stale ref_ids. When `store/sqlite` is implemented, restore the index
on session load by iterating `PartTypeCompaction` parts and re-indexing their head messages.

**2. Recall content consumes context window.**
Recalled snippets/content are injected via tool results and count toward the context
budget. A large `Fetch` result can itself trigger a new compaction. Enforce
`SnippetMaxChars` and `ContentMaxChars` in `ManagerConfig` carefully.

**3. Index growth — RESOLVED.**
`maxCompactions = 8` caps per-session index at ~50 MB. Oldest round pruned on overflow.
100 concurrent sessions ≈ 5 GB on a 64 GB server.

**4. Chinese search quality — RESOLVED.**
gse analyzer used for `Text` field; keyword analyzer for `ToolCalls` and ID fields.
Proper Chinese word segmentation replaces noisy CJK bigram fallback.
