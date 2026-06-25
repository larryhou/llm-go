---
name: builtin
description: Work with the built-in tools in github.com/larryhou/llm-go — glob, grep, read, write, edit, and shell (bash). Covers tool behaviour, parameters, output format, limits, and how to register them.
---

# Skill: builtin

The `tool/builtin` package provides six standard file-system and shell tools,
directly ported from opencode's TypeScript implementations in
`packages/opencode/src/tool/`.

All tools implement `tool.Tool` and can be registered individually with a
`tool.Registry` — or passed directly in `session.RunInput.Tools`. No tool is
auto-registered; callers opt in per tool.

## Package location

```
github.com/larryhou/llm-go/tool/builtin/
├── glob.go         GlobTool   — file pattern matching
├── grep.go         GrepTool   — regex content search
├── read.go         ReadTool   — file / directory reader
├── write.go        WriteTool  — file writer
├── edit.go         EditTool   — surgical string replacement
├── shell.go        ShellTool  — shell command execution (tool name: "bash")
└── builtin_test.go unit tests (51 cases)
```

## Registration pattern

```go
import "github.com/larryhou/llm-go/tool/builtin"

registry := tool.NewRegistry()
registry.Register(&builtin.GlobTool{WorkDir: projectRoot})
registry.Register(&builtin.GrepTool{WorkDir: projectRoot})
registry.Register(&builtin.ReadTool{WorkDir: projectRoot})
registry.Register(&builtin.WriteTool{WorkDir: projectRoot})
registry.Register(&builtin.EditTool{WorkDir: projectRoot})
registry.Register(&builtin.ShellTool{WorkDir: projectRoot})

result, err := session.RunLoop(ctx, store, session.RunInput{
    Tools: registry.All(),
    // ...
})
```

`WorkDir` is the default search / working root when the LLM omits a path.
It falls back to `os.Getwd()` if empty.

---

## Tool Reference

### GlobTool — `glob`

**Source:** `glob.go` · Port of `packages/opencode/src/tool/glob.ts`

Finds files matching a glob pattern inside a directory tree, sorted by
modification time (newest first). Backed by `ripgrep --files` when `rg` is
available in `$PATH`; falls back to a pure-Go `filepath.WalkDir`.

**Parameters**

| Name | Type | Required | Description |
|------|------|:--------:|-------------|
| `pattern` | string | ✓ | Glob pattern, e.g. `**/*.go`, `src/**/*.ts` |
| `path` | string | — | Directory to search. Omit to use `WorkDir` / CWD. Must be a directory. |

**Limits**

- Hard cap of **100 results**. Excess items are discarded; a truncation notice is appended.
- `path` must be a directory — passing a file path returns a `ToolFailure`.

**Output format**

```
/abs/path/to/file1.go
/abs/path/to/file2.go
...
(Results are truncated: showing first 100 results. ...)   ← only when truncated
```

Empty result: `No files found`

**Metadata**

```json
{ "count": 42, "truncated": false }
```

---

### GrepTool — `grep`

**Source:** `grep.go` · Port of `packages/opencode/src/tool/grep.ts`

Searches file contents with a regular expression via `ripgrep`. Requires `rg`
in `$PATH` (returns `ToolFailure` otherwise). Results are sorted by file
modification time (newest first) and grouped by file.

**Parameters**

| Name | Type | Required | Description |
|------|------|:--------:|-------------|
| `pattern` | string | ✓ | Regex, e.g. `func\s+\w+`, `log\.Error` |
| `path` | string | — | Directory **or file** to search. Omit for `WorkDir` / CWD. |
| `include` | string | — | File glob filter, e.g. `*.go`, `*.{ts,tsx}` |

**Limits**

- Up to **100 matches** shown. `metadata.matches` always reflects the true total.
- Lines longer than **2,000 characters** are truncated with `...`.

**Output format**

```
Found 7 matches
/path/to/a.go:
  Line 12: func hello() {}
  Line 34: func world() {}

/path/to/b.go:
  Line 5: func foo() {}

(Results truncated: showing 100 of 142 matches (42 hidden). ...)   ← only when truncated
```

Empty result: `No files found`. `Title` is always set to the search pattern.

**Metadata**

```json
{ "matches": 7, "truncated": false }
```

---

### ReadTool — `read`

**Source:** `read.go` · Port of `packages/opencode/src/tool/read.ts`

Reads a file (text, image, PDF) or lists a directory. Supports pagination via
`offset` / `limit`.

**Parameters**

| Name | Type | Required | Description |
|------|------|:--------:|-------------|
| `filePath` | string | ✓ | Absolute path to file or directory |
| `offset` | integer | — | 1-indexed line to start from (default: 1) |
| `limit` | integer | — | Max lines to return (default: 2,000) |

**File reading**

- Output is wrapped in XML tags and lines are prefixed with `N: `:

  ```
  <path>/abs/path/file.go</path>
  <type>file</type>
  <content>
  1: package main
  2: 
  3: func main() {}

  (End of file - total 3 lines)
  </content>
  ```

- Three possible footers:
  - `(End of file - total N lines)` — fully read
  - `(Showing lines X-Y of N. Use offset=Z to continue.)` — limit hit, more lines remain
  - `(Output capped at 50 KB. Showing lines X-Y. Use offset=Z to continue.)` — byte cap hit

- **Binary detection** — two-stage:
  1. Extension allowlist (`.zip`, `.exe`, `.dll`, `.so`, `.class`, `.jar`, `.wasm`, `.pyc`, and more) → always rejected as binary.
  2. Byte heuristic on first 4096 bytes: null byte present, or >30% non-printable → rejected.
  - Error message: `Cannot read binary file: <path>`

- **Line truncation** — lines exceeding **2,000 characters** are truncated with suffix `... (line truncated to 2000 chars)`.

- **Byte cap** — 50 KB per call; truncation footer added automatically.

- **File not found** — suggests up to 3 case-insensitively similar filenames from the parent directory.

**Image / PDF reading**

MIME type is sniffed from first 4096 bytes. Supported: `image/jpeg`, `image/png`, `image/gif`, `image/webp`, `application/pdf`. File is returned as a binary `tool.Attachment`; `Output` contains only the `<path>` / `<type>` tags.

**Directory listing**

```
<path>/abs/dir</path>
<type>directory</type>
<entries>
file.go
subdir/
(5 entries)
</entries>
```

Subdirectories and directory-pointing symlinks have a trailing `/`. Entries are sorted alphabetically. Paginates with `offset` / `limit`.

---

### WriteTool — `write`

**Source:** `write.go` · Port of `packages/opencode/src/tool/write.ts`

Creates or overwrites a file with the given content. Intermediate directories
are created automatically.

**Parameters**

| Name | Type | Required | Description |
|------|------|:--------:|-------------|
| `filePath` | string | ✓ | Absolute path to write |
| `content` | string | ✓ | Full file content |

**Output**

Always: `Wrote file successfully.`

**Metadata**

```json
{ "filepath": "/abs/path/file.go", "exists": true }
```

(`exists` reflects whether the file existed **before** writing.)

---

### EditTool — `edit`

**Source:** `edit.go` · Port of `packages/opencode/src/tool/edit.ts`

Applies a surgical string replacement to an existing file. Uses a cascade of
9 matching strategies from exact to fuzzy, mirroring opencode's implementation.

**Parameters**

| Name | Type | Required | Description |
|------|------|:--------:|-------------|
| `filePath` | string | ✓ | Absolute path to modify |
| `oldString` | string | ✓ | Text to find and replace |
| `newString` | string | ✓ | Replacement text (must differ from `oldString`) |
| `replaceAll` | boolean | — | Replace every occurrence (default: false) |

**Special case — file creation**

When `oldString` is `""`, the tool creates (or overwrites) the file with
`newString`, mirroring the write tool.

**Replacement cascade** (9 strategies, tried in order)

| # | Strategy | Description |
|---|----------|-------------|
| 1 | `SimpleReplacer` | Exact substring match |
| 2 | `LineTrimmedReplacer` | Match after independently trimming each line's whitespace |
| 3 | `BlockAnchorReplacer` | First/last line as anchors; Levenshtein similarity on middle lines (single-candidate threshold: 0.0; multi-candidate: 0.3) |
| 4 | `WhitespaceNormalizedReplacer` | Collapse all whitespace runs to single space before matching |
| 5 | `IndentationFlexibleReplacer` | Strip common leading indentation from both sides |
| 6 | `EscapeNormalizedReplacer` | Unescape `\n`, `\t`, `\r`, `\\`, `\"`, `\`` etc. before matching |
| 7 | `TrimmedBoundaryReplacer` | Trim leading/trailing whitespace from `oldString` |
| 8 | `ContextAwareReplacer` | First/last line anchors + ≥50% matching middle lines |
| 9 | `MultiOccurrenceReplacer` | Yields all exact matches (for `replaceAll=true`) |

Uniqueness is enforced for `replaceAll=false`: if a strategy produces a match
that occurs more than once in the file, iteration continues to the next
strategy. If no unique match is ever found, the error is:

> `Found multiple matches for oldString. Provide more surrounding lines in oldString to identify the correct match.`

If no strategy matches at all:

> `Could not find oldString in the file. It must match exactly, including whitespace, indentation, and line endings.`

**Line-ending preservation**

`\r\n` (CRLF) is detected on read and restored on write; all matching is done
on LF-normalised content internally.

**Concurrency**

A per-file mutex prevents two concurrent edits to the same path.

**Output**

Always: `Edit applied successfully.`

**Metadata**

```json
{ "path": "/abs/path/file.go", "additions": 2, "deletions": 1 }
```

---

### ShellTool — `bash`

**Source:** `shell.go` · Port of `packages/opencode/src/tool/shell.ts`

Executes a shell command in a subprocess. Tool name is permanently `"bash"` for
API / plugin compatibility even when a different shell is used at runtime.

**Shell selection** (auto-detected, overridable via `ShellTool.Shell`)

| Platform | Preference order |
|----------|-----------------|
| Unix / macOS | `bash` → `/bin/sh` |
| Windows | `pwsh` (PowerShell 7+) → `cmd.exe` |

**Parameters**

| Name | Type | Required | Description |
|------|------|:--------:|-------------|
| `command` | string | ✓ | Shell command to execute |
| `description` | string | ✓ | 5–10 word description of what the command does |
| `timeout` | integer (ms) | — | Execution timeout. Default: **120,000 ms** (2 min). Must be ≥ 0; negative → error. |
| `workdir` | string | — | Working directory. Use instead of `cd`. Defaults to `WorkDir` / CWD. |

**Output truncation**

When output exceeds the limits, the full content is written to a temp file
under `$TMPDIR/opencode-tool-output/` and the tool result is replaced with a
single hint message — **no preview is retained**. The LLM is expected to use
the `read` or `grep` tools to explore the file.

Hint message format:
```
Output is too large (N lines / M bytes, file size: X.X KB). Full content written to: /tmp/opencode-tool-output/bash-<ts>.txt
Use the read tool (with offset/limit) or grep tool to explore the content.
```

- Max **2,000 lines** before triggering truncation.
- Max **100 KB** before triggering truncation.
- `result.OutputPath` is set to the temp file path so the session layer can
  surface the path in context even after compaction (`ToolPartData.OutputPath`).

When output is empty: `(no output)`.

**`<shell_metadata>` block**

Appended (to the hint message when truncated, or to the output when not) on timeout or abort:

```
<shell_metadata>
shell tool terminated command after exceeding timeout 5000 ms. ...
</shell_metadata>
```

**Metadata**

```json
{
  "exit":      0,
  "timed_out": false,
  "aborted":   false,
  "truncated": false
}
```

`stdin` is always closed (non-interactive).

---

## Limits Summary

| Tool | Key limit | Value |
|------|-----------|-------|
| `glob` | Max results | 100 files |
| `grep` | Max results | 100 matches |
| `grep` | Max line length | 2,000 chars |
| `read` | Default limit | 2,000 lines |
| `read` | Byte cap per call | 50 KB |
| `read` | Max line length | 2,000 chars |
| `shell` | Default timeout | 120,000 ms |
| `shell` | Truncation trigger (lines) | 2,000 |
| `shell` | Truncation trigger (bytes) | 100 KB |

---

## Output truncation — `tool.Truncate`

All tools that produce large text outputs should use `tool.Truncate` from the
`tool` package. When the content exceeds `DefaultMaxLines` (2,000) or
`DefaultMaxBytes` (50 KB), the full content is written to a temp file and the
tool result is replaced with a hint message. **No preview is kept.**

```
Output is too large (N lines / M bytes, file size: X.X KB). Full content written to: /tmp/opencode-tool-output/<tool>-<ts>.txt
Use the read tool (with offset/limit) or grep tool to explore the content.
```

The `TruncateResult.OutputPath` must be forwarded in `tool.Result.OutputPath`
so the session layer (`ToolPartData.OutputPath`) can preserve the path in LLM
context even after compaction.

```go
tr := tool.Truncate(t.Name(), output, nil)
return tool.Result{
    Output:     tr.Content,
    Truncated:  tr.Truncated,
    OutputPath: tr.OutputPath, // must propagate
}, nil
```

`tool.BuildTruncHint(outputPath, lines, bytes)` is the exported helper that
generates the hint string. Sub-packages (e.g. `tool/builtin`) use it directly
when they manage their own file writing (e.g. `ShellTool`).

Temp files are cleaned up automatically after `RetentionDays` (7 days) by the
`tool.StartCleanup(ctx)` goroutine — call once at process startup.

---

## Error handling

All tools return errors as `*tool.ToolFailure` (recoverable) via `tool.Fail()`.
The session processor sends these back to the LLM as error tool-results rather
than aborting the stream.

```go
// Check in a tool wrapper:
if _, ok := tool.IsToolFailure(err); ok {
    // LLM will receive the error message and can retry / adjust
}
```

---

## Testing

Unit tests live in `tool/builtin/builtin_test.go` (51 cases). Run with:

```bash
go test ./tool/builtin/... -v
```

Tests cover: glob truncation, grep grouping & title, read pagination / binary
rejection / directory XML format, write idempotency, edit exact/fuzzy/CRLF/
replaceAll/ambiguous, shell exit code / timeout / workdir, tailOutput line &
byte limits, and levenshtein distance.
