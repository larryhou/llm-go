---
name: control
description: Work with cmd/control — an interactive REPL coding assistant built on llm-go session.RunLoop with builtin file tools and in-memory Bleve skill index
---

# cmd/control

`cmd/control` is an interactive terminal REPL that runs a persistent LLM session
in the current directory. It registers the six builtin file tools and optionally
a knowledge index of skill documentation, then enters a read-evaluate-print loop
backed by `session.RunLoop`.

Single file: `cmd/control/main.go`

---

## Architecture

```
User input (stdin)
      │
      ▼
  bufio.Scanner  ──────── "exit"/"quit"/EOF → quit
      │
      ▼
session.RunLoop(ctx, store, RunInput)
      │
      ├── replProvider.Stream()   ← wraps real Provider
      │       │  tee goroutine
      │       ├── evCh ──────────► printer goroutine  → stdout/stderr
      │       └── inner ch ──────► session Processor
      │
      ├── builtin tools  (glob, grep, read, write, edit, bash)
      └── knowledge tools (knowledge_search, knowledge_fetch)  [optional]
```

### replProvider — event tee

`RunLoop` is **synchronous** and drives the LLM via `session.Processor`, which
consumes events from `Provider.Stream()`. To print streaming output while
`RunLoop` is blocking, `replProvider` wraps the real provider and tees every
event to a local `chan llm.Event` (`evCh`).

Per-turn lifecycle:

```
1. start printer goroutine  (reads evCh)
2. session.RunLoop(...)     (blocks; tees events into evCh via replProvider)
3. close(evCh)              (signals printer to drain and exit)
4. <-done                   (wait for printer goroutine)
5. evCh = make(...)         (reset for next turn)
   prov.out = evCh
```

### Skills index

On startup, `buildSkillsIndex(skillsDir)` recursively walks all `*.md` files
under the skills directory and builds an **in-memory Bleve index** with four
fields:

| Field | Type | Content |
|-------|------|---------|
| `title` | text | `name:` from YAML frontmatter, or filename stem |
| `content` | text | document body (after frontmatter) |
| `skill` | keyword | first path segment relative to skillsDir |
| `path` | keyword | absolute file path |

If the skills directory does not exist, a warning is printed and the
knowledge tools are simply not registered — the REPL still works without them.

---

## CLI Flags

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `-provider` | `TIMI_PROVIDER` | `openai` | LLM provider: `openai` or `anthropic` |
| `-llm-url` | `TIMI_BASE_URL` | `http://192.168.3.119:8080/timi-claude/v1` | Base URL (openai needs `/v1`; anthropic must NOT end with `/v1`) |
| `-llm-key` | `TIMI_API_KEY` | `sk-zzz6...` | API key |
| `-model` | `TIMI_MODEL` | `claude-sonnet-4.6` | Model ID |
| `-max-steps` | — | `20` | Max agentic steps per REPL turn |
| `-context-limit` | — | `128000` | Context window token limit |
| `-skills` | — | `.opencode` | Skills root directory to index |

Priority: CLI flag > env var > hardcoded default (only in `flag.StringVar`).

### Anthropic base URL

`anthropic-sdk-go` auto-appends `/v1/messages`. When `-provider anthropic` is
used, control strips a trailing `/v1` from the resolved URL before passing it
to the provider:

```go
baseURL := strings.TrimSuffix(cfg.baseURL, "/v1")
```

So `-llm-url http://host/claude/v1` and `-llm-url http://host/claude` both work.

---

## Registered Tools

### Builtin file tools (always registered)

| Tool name | Struct | Description |
|-----------|--------|-------------|
| `glob` | `builtin.GlobTool` | Pattern file search |
| `grep` | `builtin.GrepTool` | Regex content search |
| `read` | `builtin.ReadTool` | Read file or directory |
| `write` | `builtin.WriteTool` | Write file |
| `edit` | `builtin.EditTool` | Exact string replace in file |
| `bash` | `builtin.ShellTool` | Execute shell commands |

All tools are initialized with `WorkDir: cwd` and **reused for the entire
session** (important for `EditTool` which holds a per-file mutex map).

### Knowledge tools (registered when skills dir exists)

| Tool name | Description |
|-----------|-------------|
| `knowledge_search` | Full-text search returning snippets + RefIDs |
| `knowledge_fetch` | Fetch full content of a document by RefID |

Source ID: `"skills"` → RefIDs have the form `skills:subdir/SKILL.md`.

---

## Session Persistence

A single `store/memory.Store` and a single `sessionID` are used for the entire
REPL lifetime. Every user turn appends to the same session, so the LLM has full
conversation history across turns (subject to context compaction).

```go
sessionID = "control-<unix-nano>"
```

---

## System Prompt

`DisableProviderPrompt` is **not set** (defaults to false), so the embedded
per-provider prompt is always included. `ExtraSystem` appends:

```
You are an interactive coding assistant running in directory: <cwd>
You have file tools (glob, grep, read, write, edit, bash) to explore and modify the codebase freely.
You also have knowledge_search and knowledge_fetch to look up skill documentation.
Always work within <cwd> unless explicitly instructed otherwise.
```

---

## Running

```bash
# with defaults (openai, local endpoint)
go run ./cmd/control

# explicit flags
go run ./cmd/control \
    -provider anthropic \
    -llm-url  http://192.168.3.119:8080/claude \
    -llm-key  sk-... \
    -model    claude-sonnet-4.6 \
    -skills   .opencode

# via env vars
TIMI_PROVIDER=openai TIMI_API_KEY=sk-... go run ./cmd/control
```

Build:

```bash
go build ./cmd/control/...
```

---

## Key Files

| File | Purpose |
|------|---------|
| `cmd/control/main.go` | Full implementation (single file) |
| `cmd/control/PLAN.md` | Development plan and design notes |
| `tool/builtin/` | Six builtin file tools |
| `knowledge/source/bleve/bleve.go` | BleveSource used for skills index |
| `store/memory/memory.go` | In-memory session store |
| `session/prompt.go` | `RunLoop` + `RunInput` — main agentic loop |
