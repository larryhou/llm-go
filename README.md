# llm-go

A Go port of the LLM handling layer from [opencode](https://github.com/sst/opencode).
Provides streaming LLM conversations, agentic tool-call orchestration, automatic
context compaction, and a pluggable knowledge retrieval system — all as composable
Go packages.

```
github.com/larryhou/llm-go
```

---

## Features

- **Streaming client with retry** — provider-agnostic `llm.Provider` interface;
  automatic retry with exponential back-off on transient errors; mid-stream error
  recovery without losing buffered events.

- **Two provider adapters** — `provider/anthropic` (official SDK) and
  `provider/openai` (OpenAI-compatible, works with any proxy or local endpoint).

- **Full agentic loop** (`session.RunLoop`) — user message → LLM stream →
  parallel tool execution → tool results back to LLM → repeat until `stop`.

- **Context compaction** — when the session approaches the model's context limit,
  the oldest turns are summarised by a second LLM call and replaced by a compact
  summary, then the conversation continues seamlessly.

- **Doom-loop detection** — stops the loop when the same tool is called with
  identical arguments three consecutive times.

- **Pluggable knowledge retrieval** (`knowledge.Manager`) — register any number
  of `Source` backends; the manager dispatches `knowledge_search` and
  `knowledge_fetch` as native `tool.Tool` calls, with priority grouping,
  concurrent dispatch per group, and automatic fallback.

- **Bleve full-text source** — ready-made `knowledge/source/bleve` backed by
  an in-memory or on-disk Bleve index.

- **HTTP knowledge API** (`cmd/knowledge-api`) — self-contained HTTP server
  that indexes a skills directory, exposes REST search/fetch endpoints, and
  streams LLM chat responses as Server-Sent Events.

- **opencode-compatible config** — reads `~/.config/llm/llm.json` and
  `.llm/llm.json`; schema mirrors opencode's `opencode.json`.

---

## Package Layout

```
github.com/larryhou/llm-go/
├── llm/                  Canonical types, overflow math, error classification, retry
├── provider/
│   ├── provider.go       Registry, Model, Info, ParseModel()
│   ├── anthropic/        Anthropic Messages API (anthropic-sdk-go)
│   └── openai/           OpenAI-compatible Chat Completions (openai-go)
├── session/
│   ├── prompt.go         RunLoop() — main agentic loop
│   ├── processor.go      Event handler — tool lifecycle, doom-loop, overflow
│   ├── compaction.go     Compact() / Prune() / Select() / FilterCompacted()
│   ├── context.go        ToModelMessages() — store records → llm.Message
│   └── system.go         Per-provider embedded system prompts
├── tool/                 Tool interface, ToolFailure, Registry, output truncation
├── store/                Store interface (session / message / part CRUD)
│   └── memory/           In-memory Store implementation
├── knowledge/            Manager, Source interface, knowledge_search/fetch tools
│   └── source/bleve/     Bleve-backed Source (Peek = snippets, Fetch = full doc)
├── config/               Config schema, Load(), per-field accessors
├── auth/                 auth.json reader/writer (oauth / api / wellknown)
└── cmd/
    ├── knowledge-api/    HTTP server — Bleve index + /chat SSE endpoint
    └── index-skills/     CLI — build an on-disk Bleve index from a skills directory
```

---

## Quick Start

### 1. Single-turn streaming call

```go
package main

import (
    "context"
    "fmt"

    "github.com/larryhou/llm-go/llm"
    openaiProv "github.com/larryhou/llm-go/provider/openai"
)

func main() {
    prov := openaiProv.New(
        "sk-your-api-key",
        "https://api.openai.com/v1", // or any OpenAI-compatible URL
        "openai",
        nil,
    )
    client := llm.NewClient(prov)

    model := llm.Model{
        ID:         "gpt-4o",
        ProviderID: "openai",
        APIID:      "gpt-4o",
        Limit:      llm.ModelLimit{Context: 128_000, Output: 4096},
    }

    events := client.Stream(context.Background(), llm.Request{
        Model:    model,
        System:   []string{"You are a concise assistant."},
        Messages: []llm.Message{llm.NewUserMessage("What is the capital of France?")},
    })

    for ev := range events {
        if ev.IsText() {
            fmt.Print(ev.Text)
        }
    }
    fmt.Println()
}
```

### 2. Multi-turn agentic session with tools

```go
package main

import (
    "context"
    "fmt"
    "strconv"

    "github.com/larryhou/llm-go/llm"
    openaiProv "github.com/larryhou/llm-go/provider/openai"
    "github.com/larryhou/llm-go/session"
    "github.com/larryhou/llm-go/store/memory"
    "github.com/larryhou/llm-go/tool"
)

// calcTool evaluates simple arithmetic expressions.
type calcTool struct{}

func (calcTool) Name() string        { return "calc" }
func (calcTool) Description() string { return "Evaluate a math expression, e.g. '2 + 3 * 4'" }
func (calcTool) InputSchema() map[string]any {
    return map[string]any{
        "type":     "object",
        "required": []string{"expr"},
        "properties": map[string]any{
            "expr": map[string]any{"type": "string"},
        },
    }
}
func (calcTool) Execute(_ context.Context, input map[string]any) (tool.Result, error) {
    expr, _ := input["expr"].(string)
    // (real impl would parse the expression)
    return tool.Result{Output: "42", Title: "calc"}, nil
}

func main() {
    ctx := context.Background()
    st  := memory.New()
    prov := openaiProv.New("sk-your-api-key", "https://api.openai.com/v1", "openai", nil)

    model := llm.Model{
        ID: "gpt-4o", ProviderID: "openai", APIID: "gpt-4o",
        Limit: llm.ModelLimit{Context: 128_000, Output: 4096},
    }

    // Create a session in the store.
    sessID := "demo-session-1"
    _ = st.CreateSession(ctx, &store.Session{ID: sessID, Model: "openai/gpt-4o"})

    // Turn 1
    session.RunLoop(ctx, st, session.RunInput{
        SessionID: sessID,
        UserMsg:   "What is 123 * 456?",
        Model:     model,
        Provider:  prov,
        Tools:     []tool.Tool{calcTool{}},
    })

    // Turn 2 — history is loaded automatically from the store
    session.RunLoop(ctx, st, session.RunInput{
        SessionID: sessID,
        UserMsg:   "Double the result.",
        Model:     model,
        Provider:  prov,
        Tools:     []tool.Tool{calcTool{}},
    })
}
```

### 3. Knowledge retrieval (search + fetch)

```go
import (
    "time"
    bleve "github.com/blevesearch/bleve/v2"
    "github.com/larryhou/llm-go/knowledge"
    blevesrc "github.com/larryhou/llm-go/knowledge/source/bleve"
    "github.com/larryhou/llm-go/session"
)

// Build an in-memory Bleve index.
idx, _ := bleve.NewMemOnly(bleve.NewIndexMapping())
_ = idx.Index("doc-1", map[string]any{
    "title":   "Go concurrency patterns",
    "content": "Goroutines, channels, select...",
})

// Create the manager with a Bleve source.
km := knowledge.NewManager(knowledge.ManagerConfig{
    SourceTimeout:   5 * time.Second,
    MaxResults:      5,
    SnippetMaxChars: 300,
    ContentMaxChars: 8000,
})
km.Register(blevesrc.New(idx, "docs", 0, nil))

// Attach knowledge tools to RunLoop — no other changes needed.
session.RunLoop(ctx, st, session.RunInput{
    SessionID: sessID,
    UserMsg:   "What do you know about Go concurrency?",
    Model:     model,
    Provider:  prov,
    Tools:     km.Tools(), // injects knowledge_search + knowledge_fetch
})
```

### 4. Run the knowledge-api HTTP server

```bash
# Start — indexes .opencode/skills and listens on :7700
go run ./cmd/knowledge-api/ \
    -skills /path/to/.opencode/skills \
    -addr   127.0.0.1:7700 \
    -llm-url https://api.openai.com/v1 \
    -llm-key sk-your-api-key \
    -model  gpt-4o

# Health check
curl http://127.0.0.1:7700/health
# → {"status":"ok","doc_count":3,"session_count":0}

# Search the knowledge index
curl "http://127.0.0.1:7700/search?q=context+compaction&max_results=3"

# Chat with SSE streaming (session persists across requests)
curl -sN -X POST http://127.0.0.1:7700/chat \
    -H "Content-Type: application/json" \
    -d '{"message":"Explain how context compaction works."}'

# Continue the same session
curl -sN -X POST http://127.0.0.1:7700/chat \
    -H "Content-Type: application/json" \
    -d '{"message":"Give me a code example.","session_id":"sess-from-x-session-id-header"}'

# Inspect stored messages
curl http://127.0.0.1:7700/sessions/{session-id}/messages
```

**SSE event format:**

```
data: {"type":"text","delta":"Context compaction fires when..."}
data: {"type":"tool_call","tool":"knowledge_search","input":{"query":"compaction"}}
data: {"type":"tool_result","tool":"knowledge_search","output":"## Result 1\n..."}
data: {"type":"done","session_id":"sess-1234567890"}
```

---

## Configuration

### LLM provider (OpenAI-compatible proxy)

```bash
export TIMI_BASE_URL="http://your-proxy/v1"
export TIMI_API_KEY="sk-your-key"
export TIMI_MODEL="claude-sonnet-4.6"
```

### Config file (`~/.config/llm/llm.json`)

```json
{
  "providers": {
    "anthropic": {
      "options": { "apiKey": "sk-ant-..." }
    },
    "openai": {
      "options": {
        "apiKey": "sk-...",
        "baseURL": "https://api.openai.com/v1"
      }
    }
  },
  "compaction": {
    "auto": true,
    "tailTurns": 2
  }
}
```

### Auth file (`~/.config/llm/auth.json`, chmod 0600)

```json
{
  "anthropic": { "type": "api", "key": "sk-ant-..." },
  "openai":    { "type": "api", "key": "sk-..." }
}
```

---

## Testing

### Unit tests (no network required)

```bash
go test ./llm/... ./config/... ./session/... ./store/... ./tool/... ./knowledge/...
```

### Integration tests (requires a live LLM endpoint)

```bash
export LLM_INTEGRATION=1
export TIMI_BASE_URL="http://your-llm-proxy/v1"
export TIMI_API_KEY="sk-your-key"
export TIMI_MODEL="claude-sonnet-4.6"

# Core integration tests
go test ./integration/... -v -count=1 -timeout=120s

# Context overflow + compaction end-to-end
go test ./integration/overflow/... -v -count=1 -timeout=300s

# Knowledge retrieval + LLM usage
go test ./integration/knowledge/... -v -count=1 -timeout=120s
```

### knowledge-api feature test (requires running server)

```bash
go run ./cmd/knowledge-api/ -skills .opencode/skills -addr 127.0.0.1:7700 &

bash cmd/knowledge-api/test_features.sh
# → Passed: 32 / 32 — ALL TESTS PASSED
```

The shell test covers: normal tool call, multi-turn memory, knowledge search/fetch,
recoverable tool failure, `max_steps` enforcement, stateful counters, context
compaction, async tool execution, all SSE event types, and session inspection.

---

## Key Design Decisions

**Provider-agnostic core** — `llm.Provider` is a single interface (`ID()` +
`Stream()`). Anthropic and OpenAI adapters translate their native SDK events into
the same canonical `llm.Event` stream. New providers implement one interface.

**Compaction is transparent to the caller** — `RunLoop` detects overflow via
`IsOverflow(usage, model)` after every `EventStepFinish`. It calls `Compact()`
internally, inserts a summary message into the store, then retries the turn.
The caller sees no difference; the session simply continues.

**Tool failure vs. fatal error** — returning `tool.Fail("message")` from a
tool produces an error tool-result that the LLM receives and can reason about.
The session continues. Any other error stops the session immediately.

**Two-level knowledge retrieval** — `knowledge_search` calls `Source.Peek()`
and returns only snippets (minimal context growth). The LLM decides whether to
call `knowledge_fetch` with a `ref_id` to retrieve the full document. This keeps
the context window small by default.

**RefID routing** — every `Result.RefID` has the form `"{sourceID}:{key}"`.
The manager uses `strings.Cut` to route `knowledge_fetch` directly to the correct
source in O(1), avoiding a linear scan across all sources.

**SummaryProvider separation** — the compaction summary LLM call uses a separate
`SummaryProvider` (when provided). This prevents middleware (e.g., the SSE event
forwarder in `knowledge-api`) from leaking internal summary events to external
clients.

---

## Requirements

- Go 1.25+
- An OpenAI-compatible or Anthropic LLM endpoint
- No CGO required for the core library; Bleve's FAISS backend requires CGO if
  used (the in-memory Bleve index used by `knowledge-api` does not)
