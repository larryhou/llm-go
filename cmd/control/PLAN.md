# cmd/control 开发计划

## 目标

一个交互式命令行 REPL 工具，利用 llm-go 的 session.RunLoop，
注册 builtin 文件工具 + knowledge 技能索引，允许在当前目录进行任意探索。

---

## 文件结构

```
cmd/control/
├── PLAN.md         本文件
└── main.go         全部实现（单文件）
```

---

## CLI Flags

```go
type config struct {
    provider     string // -provider     default: "openai"  (env: TIMI_PROVIDER)
    baseURL      string // -llm-url      default: ""        (env: TIMI_BASE_URL)
    apiKey       string // -llm-key      default: ""        (env: TIMI_API_KEY)
    modelID      string // -model        default: "claude-sonnet-4.6" (env: TIMI_MODEL)
    maxSteps     int    // -max-steps    default: 20
    contextLimit int    // -context-limit default: 128000
    skillsDir    string // -skills       default: ".opencode"
}
```

---

## 实现步骤（main.go）

### 1. Flag 解析 + 环境变量 fallback

与 knowledge-api 完全相同的模式：
- flag.Parse() 先行
- 环境变量作为 fallback（TIMI_PROVIDER / TIMI_BASE_URL / TIMI_API_KEY / TIMI_MODEL）

### 2. Provider 初始化

```go
// openai（默认）
p = openaiProv.New(apiKey, baseURL, "openai", nil)

// anthropic
p = anthropicProv.New(apiKey, baseURL, map[string]string{
    "Authorization": "Bearer " + apiKey,
})
```

导入路径：
- `github.com/larryhou/llm-go/provider/openai`
- `github.com/larryhou/llm-go/provider/anthropic`

### 3. Builtin 工具注册

```go
cwd, _ := os.Getwd()
tools := []tool.Tool{
    &builtin.GlobTool{WorkDir: cwd},
    &builtin.GrepTool{WorkDir: cwd},
    &builtin.ReadTool{WorkDir: cwd},
    &builtin.WriteTool{WorkDir: cwd},
    &builtin.EditTool{WorkDir: cwd},
    &builtin.ShellTool{WorkDir: cwd},
}
```

导入路径：`github.com/larryhou/llm-go/tool/builtin`

### 4. Skills 内存 Bleve 索引

函数：`buildSkillsIndex(skillsDir string) (bleve.Index, int, error)`

逻辑（复用 knowledge-api 的 buildMemoryIndex）：
- `bleve.NewMemOnly(buildMapping())` 创建内存索引
- `filepath.WalkDir(skillsDir, ...)` 递归扫描所有 `*.md` 文件
- 每个文件：
  - `parseFrontmatter(content)` 提取 `name:` → title，`description:` → description
  - `skill` = 相对 skillsDir 的父目录名（filepath.Dir(rel) 的第一段）
  - `path` = 相对路径（rel）
  - doc ID = rel（如 `builtin/SKILL.md`）
  - 索引字段：`title`, `content`(body), `skill`, `path`
- skillsDir 不存在时：打印警告，返回空索引（不报错退出）

```go
mapping := bleve.NewIndexMapping()
// text field: title, content
// keyword field: skill, path
idx, _ := bleve.NewMemOnly(mapping)

km := knowledge.NewManager(knowledge.ManagerConfig{
    MaxResults:          5,
    SnippetMaxChars:     400,
    ContentMaxChars:     8000,
    AllowPartialFailure: true,
})
km.Register(blevesource.New(idx, "skills", 0, nil))
tools = append(tools, km.Tools()...)
```

导入路径：
- `github.com/blevesearch/bleve/v2`
- `github.com/larryhou/llm-go/knowledge`
- `github.com/larryhou/llm-go/knowledge/source/bleve`

### 5. Session Store + Session 创建

```go
store := memory.New()
sessionID := fmt.Sprintf("control-%d", time.Now().UnixNano())
store.CreateSession(ctx, &storeTypes.Session{
    ID:    sessionID,
    Model: cfg.modelID,
})
```

导入路径：
- `github.com/larryhou/llm-go/store/memory`
- `github.com/larryhou/llm-go/store` (Session 类型)

### 6. ExtraSystem Prompt

```go
extraSystem := []string{fmt.Sprintf(
    "You are an interactive coding assistant running in directory: %s\n"+
    "You have file tools (glob, grep, read, write, edit, bash) to explore and modify the codebase freely.\n"+
    "You also have knowledge_search and knowledge_fetch to look up skill documentation.\n"+
    "Always work within %s unless explicitly instructed otherwise.",
    cwd, cwd,
)}
```

### 7. REPL 主循环

```
启动时打印：
  Control — interactive coding assistant
  Working directory: <cwd>
  Skills indexed: <N> documents from <skillsDir>  (或 "no skills indexed")
  Type 'exit' or Ctrl-D to quit.
  >

每轮：
  1. fmt.Print("> ") 打印提示符
  2. bufio.Scanner.Scan() 读取一行
  3. 空行跳过；"exit"/"quit" 退出
  4. session.RunLoop(ctx, store, RunInput{...})
  5. 遍历事件 channel：
     - EventTextDelta   → fmt.Print(ev.Text)  (流式)
     - EventToolCall    → fmt.Printf("\n[tool: %s]\n", ev.ToolName)
     - EventStepFinish  → fmt.Println()
     - EventRequestFinish → fmt.Println()  换行后等待下一轮
     - EventError       → fmt.Fprintf(os.Stderr, "error: %v\n", ev.Err)
```

RunInput 参数：
```go
session.RunInput{
    SessionID: sessionID,         // 全程复用同一 session
    UserMsg:   userInput,
    Model: llm.Model{
        ID:         cfg.modelID,
        ProviderID: cfg.provider,
        APIID:      cfg.modelID,
        Limit: llm.ModelLimit{
            Context: cfg.contextLimit,
            Output:  8192,
        },
    },
    Provider:  prov,
    Tools:     tools,
    ExtraSystem: extraSystem,
    MaxSteps:    cfg.maxSteps,
}
```

---

## 导入清单（完整）

```go
import (
    "bufio"
    "context"
    "flag"
    "fmt"
    "os"
    "path/filepath"
    "strings"
    "time"

    bleve       "github.com/blevesearch/bleve/v2"
    "github.com/larryhou/llm-go/knowledge"
    blevesource "github.com/larryhou/llm-go/knowledge/source/bleve"
    "github.com/larryhou/llm-go/llm"
    anthropicProv "github.com/larryhou/llm-go/provider/anthropic"
    openaiProv    "github.com/larryhou/llm-go/provider/openai"
    "github.com/larryhou/llm-go/session"
    "github.com/larryhou/llm-go/store"
    "github.com/larryhou/llm-go/store/memory"
    "github.com/larryhou/llm-go/tool"
    "github.com/larryhou/llm-go/tool/builtin"
)
```

---

## 复用自 knowledge-api 的函数（直接拷贝，不改逻辑）

| 函数 | 说明 |
|------|------|
| `buildMapping()` | Bleve IndexMapping（text + keyword fields） |
| `parseFrontmatter(src string) (name, body string)` | 提取 YAML frontmatter name: 字段 |
| `buildSkillsIndex(dir string) (bleve.Index, int, error)` | 递归扫描 .md 文件建内存索引 |

---

## 编译验证

```bash
go build ./cmd/control/...
```

## 注意事项

1. `session.RunLoop` 返回的是 `(RunResult, error)`，不是 channel——
   需确认实际签名（knowledge-api 中是直接调用并在 wrappedProv 里拦截事件）。
   → 若 RunLoop 是同步的，则流式输出需在 Provider wrapper 层或事件 hook 处理。
   → 若存在 `RunLoopStream` 或 channel 变体，优先使用。
   → 否则用 `sseProvider` 同款 wrapper 模式，将事件写入本地 channel。

2. builtin.EditTool 的 `locks` 字段是私有 map，需要在 REPL 全程复用同一实例
   （已经是这样，不重复创建）。

3. Skills 目录不存在时不退出，仅打印：
   `[warn] skills directory not found: <path>, skipping knowledge index`
