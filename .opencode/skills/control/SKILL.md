---
name: control
description: Work with cmd/control — an interactive REPL coding assistant built on agent.Client, and the agent package that provides the ready-to-run Client
---

# cmd/control + agent package

`cmd/control` is an interactive terminal REPL **and web UI** coding assistant.
It is built on top of `agent.Client`, which provides all the wiring a coding
session needs out of the box. `main.go` is now thin: flag parsing, store
selection, optional debug recording, and the REPL loop.

Files:
- `agent/agent.go` — `agent.Client`, `Config`, `RunOptions`, `hookProvider`, `OpenSQLiteStore`
- `cmd/control/main.go` — CLI flags, REPL loop, `toolPath` display helper
- `cmd/control/web.go` — HTTP server, `/chat` SSE endpoint, `/cancel`, `/context`
- `cmd/control/ui.go` — Embedded single-page web UI (`uiHTML` constant)

---

## `agent` package

### What `agent.New()` wires by default

| Component | Default behaviour | Opt-out |
|-----------|-------------------|---------|
| Provider | registry (env / llm.json) | `Config.Provider` |
| Store | `memory.New()` | `Config.Store` |
| SessionID | `"agent-"+sha256(WorkDir)[:8]` | `Config.SessionID` |
| Builtin file tools | glob, grep, read, write, edit, bash (scoped to WorkDir) | `NoBuiltinTools: true` |
| SessionHistorySource | always; SQLite L2 if store is PersistStore | — |
| Knowledge manager | history source (priority 0) + skills Bleve index (priority 1) | `NoSkillsIndex: true` |
| Skills Bleve index | walks `<WorkDir>/.opencode` for `*.md` | `SkillsDir: "-"` or `NoSkillsIndex` |
| session_reset tool | soft + hard reset | `NoResetTool: true` |
| System prompt | coding-assistant preamble + knowledge-first instructions | `ExtraSystem` to append |

Extension points: `ExtraTools`, `ExtraSources`, `ExtraSystem`.

### Config fields

```go
type Config struct {
    // Provider credentials (fall back to LLM_PROVIDER/LLM_BASE_URL/LLM_API_KEY/LLM_MODEL)
    ProviderID string
    BaseURL    string
    APIKey     string
    ModelID    string
    Provider   llm.Provider  // inject pre-built provider

    // Model limits
    MaxSteps     int  // default 20
    ContextLimit int  // default 128000

    // Session
    SessionID string       // default sha256(WorkDir)[:8]
    Store     store.Store  // default memory.New()
    WorkDir   string       // default os.Getwd()

    // Skills
    SkillsDir string  // default "<WorkDir>/.opencode"; "-" to skip

    // Extension
    ExtraTools   []tool.Tool
    ExtraSources []knowledge.Source
    ExtraSystem  []string

    // Opt-outs
    NoBuiltinTools bool
    NoResetTool    bool
    NoSkillsIndex  bool
}
```

### Client exported fields

```go
type Client struct {
    Store      store.Store               // session store
    SessionID  string                    // stable session ID
    Model      llm.Model                 // LLM model
    Provider   llm.Provider              // raw provider; replaceable before first RunAsync
    HistorySrc *store.SessionHistorySource  // exposed for RollbackTo / direct access
}
```

### Client methods

```go
// Blocking — returns nil on success, context.Canceled on cancel.
func (c *Client) Run(ctx, userMsg, opts RunOptions, on func(llm.Event)) error

// Non-blocking — returns *session.RunHandle immediately.
func (c *Client) RunAsync(ctx, userMsg, opts RunOptions, on func(llm.Event)) *session.RunHandle

// Channel — returns <-chan llm.Event closed when turn finishes.
func (c *Client) RunChan(ctx, userMsg, opts RunOptions) <-chan llm.Event

// Returns a shallow copy of the default RunOptions (Tools/ExtraSystem are copied).
func (c *Client) DefaultRunOptions() RunOptions
```

### RunOptions

```go
type RunOptions struct {
    Tools       []tool.Tool
    ExtraSystem []string
    OnCompact   store.CompactionHook
    WaitFor     <-chan struct{}  // pass prev.StoreDone to pipeline turns
}
```

Zero value means "use all client defaults". `mergeOpts` fills nil fields from
`Client.opts` before passing to `session.RunLoopAsync`.

### hookProvider — event fan-out

`RunAsync` wraps the provider in a `hookProvider` when `on != nil`. It fans
events to two destinations:

1. **`ch`** (buf 64) — delivered to the session loop; never dropped.
2. **`obsCh`** (buf 128) → `onEvent` — best-effort; dropped when `obsCh` is full.

**Per-`Stream`-call lifecycle** (critical): `observerDone` is allocated inside
`Stream`, not as a struct field. This is necessary because the session loop calls
`Stream` once per agentic step, so a multi-tool-call turn invokes `Stream` N
times on the same `hookProvider`. A shared channel would be closed by step 1
and panic on `close` in step 2+.

**Ordering guarantee**: `ch` is closed only *after* `<-observerDone`, so when
`h.Done` fires (which requires `ch` to be fully drained by the session loop),
`onEvent` is guaranteed to never be called again. It is safe for the caller to
`close(evCh)` immediately after `<-h.Done`.

```
inner → fan goroutine ──► ch (session loop, never dropped)
                      ──► obsCh (non-blocking) → observer goroutine → onEvent
                                                      ↓ close(observerDone)
                            close(obsCh) → <-observerDone → close(ch)
```

### OpenSQLiteStore

```go
// agent.Store embeds store.Store + Close() error.
func OpenSQLiteStore(path string) (agent.Store, error)
```

Returns `agent.Store` (not bare `store.Store`) so callers can `defer st.Close()`
without a type assertion.

---

## cmd/control

### Architecture

```mermaid
flowchart TD
    stdin["User input (stdin)"]
    scanner["bufio.Scanner"]
    quit["quit / EOF (Ctrl-D)"]
    cancel["Ctrl-C → h.Cancel()"]
    agentClient["agent.Client.RunAsync()"]
    hookprov["hookProvider.Stream()\nobsCh fan-out"]
    printer["printer goroutine\nstdout / stderr"]
    processor["session.Processor"]
    tools["builtin + knowledge + reset tools\n(wired by agent.New)"]

    stdin --> scanner
    scanner -->|"exit/quit/EOF"| quit
    scanner --> agentClient
    agentClient --> hookprov
    hookprov -->|evCh (best-effort)| printer
    hookprov -->|ch (reliable)| processor
    agentClient --> tools
    cancel -->|"SIGINT"| agentClient
```

### Per-turn lifecycle (REPL)

A **fresh** `evCh` and `onEvent` closure are created at the top of each loop
iteration. This prevents stale references from a previous turn's goroutines from
sending into a closed channel.

```mermaid
sequenceDiagram
    participant REPL
    participant printer as printer goroutine
    participant Handle as RunHandle
    participant hook as hookProvider

    REPL->>REPL: evCh := make(..., 128); onEvent := func...
    REPL->>printer: go func() { for ev := range evCh }
    REPL->>Handle: client.RunAsync(ctx, line, RunOptions{}, onEvent) → h
    Handle->>hook: Stream(ctx, req)  [called once per agentic step]
    hook-->>printer: obsCh → onEvent(ev) → evCh ← ev
    hook-->>Handle: ch ← ev  [reliable]
    Note over REPL: select { h.Done | intSig }
    alt Ctrl-C
        REPL->>Handle: h.Cancel(); <-h.Done
    else normal finish
        Handle-->>REPL: h.Done closed
    end
    REPL->>printer: close(evCh)  [safe: h.Done guarantees onEvent won't fire again]
    REPL->>REPL: <-done
```

### CLI Flags

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `-provider` | `LLM_PROVIDER` | `openai` | LLM provider: `openai` or `anthropic` |
| `-llm-url` | `LLM_BASE_URL` | hardcoded proxy | Base URL |
| `-llm-key` | `LLM_API_KEY` | hardcoded key | API key |
| `-model` | `LLM_MODEL` | `claude-sonnet-4.6` | Model ID |
| `-max-steps` | — | `20` | Max agentic steps per turn |
| `-context-limit` | — | `128000` | Context window token limit |
| `-skills` | — | `.opencode` | Skills root dir (relative to cwd) |
| `-store` | — | `sqlite:memory.db` | Store DSN: `"memory"` or `"sqlite:<path>"` |
| `-web` | — | `false` | Start web UI |
| `-debug` | — | `false` | Wrap provider with RecordProvider → `debug-<ts>.ndjson` |

### `-debug` recording

```
real provider → RecordProvider (client.Provider = rec) → hookProvider → session loop
```

`client.Provider` is replaced after `agent.New()` but before the first
`RunAsync` call. `defer rec.Close()` flushes the ndjson file on exit.

---

## Web Server Mode (`-web`)

HTTP server on a random local port. `web.go` uses `client.DefaultRunOptions()`
+ sets `WaitFor: prev.StoreDone` for atomic cancel-and-start.

### Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/` | GET | Embedded single-page UI |
| `/chat` | POST | SSE — `{"message":"..."}` → event stream |
| `/cancel` | POST | Cancel in-flight turn; 204 |
| `/context` | GET | Session context window as JSON |

### `/chat` flow

1. Decode `{"message":"..."}`, set SSE headers, call `w.WriteHeader(200)`.
2. Under `s.mu`: `prev.Cancel()`, build `opts = DefaultRunOptions(); opts.WaitFor = prev.StoreDone`, call `client.RunAsync(ctx, msg, opts, onEvent)`, assign `s.activeHandle`.
3. `<-h.Done`; send `cancelled` / `done` SSE event.

### SSE event types

| `type` | Fields | Description |
|--------|--------|-------------|
| `text` | `delta` | Streaming text delta |
| `tool_call` | `tool`, `input` | Tool invoked |
| `usage` | `input`, `output`, `total` | Token counts (per step) |
| `error` | `error` | Error (not sent for `context.Canceled`) |
| `cancelled` | — | Turn cancelled |
| `done` | — | Always last |

---

## Session Persistence

`SessionID` is derived from `sha256(WorkDir)[:8]` with prefix `"agent-"` so the
same directory always resumes the same session across restarts (when SQLite store
is used). `SessionHistorySource` provides L0/L1/L2 caching with SQLite as the
durable backend.

---

## System Prompt

Built by `agent.New()`:

```
You are an interactive coding assistant running in directory: <WorkDir>
Tool usage priority:
1. Always call knowledge_search first ...
2. If results insufficient, use file tools ...
Never skip the knowledge lookup step ...
Always work within <WorkDir> unless explicitly instructed otherwise.
```

Append additional instructions via `Config.ExtraSystem`.

---

## Minimal Usage

```go
// Zero-config: reads LLM_* env vars, uses cwd, memory store.
client, err := agent.New(agent.Config{})

// SQLite persistence + custom skills dir.
st, _ := agent.OpenSQLiteStore("session.db")
defer st.Close()
client, err := agent.New(agent.Config{
    Store:     st,
    SkillsDir: "/path/to/skills",
})

// Inject extra tools.
client, err := agent.New(agent.Config{
    ExtraTools: []tool.Tool{myTool},
})

// Blocking run with event callback.
err = client.Run(ctx, "Explain this codebase", agent.RunOptions{}, func(ev llm.Event) {
    if ev.Type == llm.EventTextDelta { fmt.Print(ev.Text) }
})

// Non-blocking with Ctrl-C support.
h := client.RunAsync(ctx, line, agent.RunOptions{}, onEvent)
select {
case <-h.Done:
case <-intSig:
    h.Cancel(); <-h.Done
}
```

---

## Running cmd/control

```bash
go run ./cmd/control                          # reads LLM_* env vars
go run ./cmd/control -provider anthropic      # anthropic provider
go run ./cmd/control -debug                   # record to debug-<ts>.ndjson
go run ./cmd/control -web                     # web UI on random port
go run ./cmd/control -store sqlite:session.db # SQLite persistence
```

---

## Key Files

| File | Purpose |
|------|---------|
| `agent/agent.go` | `Client`, `Config`, `RunOptions`, `hookProvider`, `OpenSQLiteStore` |
| `cmd/control/main.go` | CLI flags, REPL loop, `toolPath` helper |
| `cmd/control/web.go` | HTTP server, SSE `/chat`, `/cancel`, `/context` |
| `cmd/control/ui.go` | Embedded single-page web UI |
| `tool/builtin/` | Six builtin file tools (wired by agent.New) |
| `knowledge/source/bleve/` | BleveSource for skills index |
| `store/session_history.go` | `SessionHistorySource` — L0/L1/L2 knowledge cache |
| `session/prompt.go` | `RunLoop`, `RunLoopAsync`, `RunHandle`, `RunInput` |
| `llm/record_provider.go` | `RecordProvider` for `-debug` recording |
