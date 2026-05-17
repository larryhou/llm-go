# 问题清单与优化方向

覆盖范围：`llm/`、`provider/`、`session/`（含 processor、compaction、prompt、context、system、reset_tool）、`store/`（含 memory）包的代码审查结果。
严重级别：**P0** = 逻辑错误/数据损坏，**P1** = 行为不符合预期，**P2** = 代码质量/维护隐患。

---

## P0 — 逻辑错误

### Issue-01 · 工具参数 JSON 解析失败被静默忽略

> **✅ 已修复** (commit `80b440e`) — 解析失败时发送 `EventToolError` 事件，不再以空参数执行工具。

| 属性 | 值 |
|------|----|
| 文件 | `provider/anthropic/anthropic.go:345` |
| 文件 | `provider/openai/openai.go:411` |
| 类型 | 错误静默丢弃 |

**问题**

流式接收到的工具参数（tool input）完成后做 JSON 解析，失败时错误被 `_ =` 丢弃，
`input` 保持 `nil`，工具以空参数执行，结果完全错误。LLM 无法感知，可能陷入反复
重试的 doom loop。

```go
// 当前写法
_ = json.Unmarshal(toolInputBuf, &input)  // anthropic.go:345
_ = json.Unmarshal(ts.args, &input)       // openai.go:411
```

**优化方向**

解析失败时发送 `EventToolError` 事件（携带 ToolCallID + ToolName + 错误信息），
由 `session/processor.go` 的 `handleEvent` 将该工具标记为 `ToolStatusError`，
LLM 收到 error tool-result 后可自行决定是否重试。

---

### Issue-02 · `isAlreadyInterrupted` 读取失败时策略不保守

| 属性 | 值 |
|------|----|
| 文件 | `session/processor.go:367-377` |
| 类型 | 竞态 / 错误处理方向错误 |

**问题**

`GetPart` 失败（DB 错误、网络抖动）时返回 `false`，导致 `executeTool` goroutine
继续用 `ToolStatusCompleted` 覆盖 `cleanup` 已写入的 `interrupted` 标记，
破坏了 cleanup 的权威状态。

```go
func isAlreadyInterrupted(s store.Store, partID string) bool {
    p, err := s.GetPart(context.Background(), partID)
    if err != nil {
        return false  // ← 读取失败时放行覆盖，行为与预期相反
    }
    ...
}
```

**优化方向**

读取失败时保守地返回 `true`，阻止 `executeTool` 覆盖；cleanup 的
interrupted 标记优先级高于任何后续写入。

---

### Issue-03 · OpenAI `buildParams` 工具 schema unmarshal 错误被忽略

> **✅ 已修复** (commit `80b440e`) — unmarshal 失败时返回包装错误，中止 `buildParams`，与 Anthropic 侧对齐。

| 属性 | 值 |
|------|----|
| 文件 | `provider/openai/openai.go:148` |
| 类型 | 错误静默丢弃 |

**问题**

工具 InputSchema 的 JSON marshal 有错误处理，但紧随其后的 unmarshal 错误被 `_ =`
丢弃，`schemaMap` 为 `nil`，发给 OpenAI 的工具定义是空的，工具调用将静默失败或产
生不可预期行为。Anthropic 侧同一操作有完整错误处理，两侧不对称。

```go
schemaBytes, err := json.Marshal(t.InputSchema)
if err != nil { return params, fmt.Errorf(...) }  // ← 有处理

var schemaMap map[string]any
_ = json.Unmarshal(schemaBytes, &schemaMap)       // ← 无处理
```

**优化方向**

与 Anthropic 侧对齐，unmarshal 失败时返回包装错误，中止 `buildParams`。

---

## P1 — 行为不符合预期

### Issue-04 · Transport 错误硬编码为不可重试

> **✅ 已修复** (commit `80b440e`) — 新增 `isRetryableTransportError`；`io.EOF`/超时/read-write op 标记为可重试，dial/TLS/DNS 保持不可重试。

| 属性 | 值 |
|------|----|
| 文件 | `provider/anthropic/anthropic.go:415-419` |
| 文件 | `provider/openai/openai.go:480-484` |
| 类型 | 重试策略缺失 |

**问题**

非 API 错误（connection reset、EOF、TLS 握手失败等）被包装为
`ErrTransport` 且 `IsRetryable: false`，`client.go` 的 `ShouldRetry` 不会重试。
网络瞬断是典型的可重试场景，当前实现对不稳定 proxy 或弱网环境不友好。

```go
return &llm.LLMError{
    Kind:        llm.ErrTransport,
    Message:     err.Error(),
    IsRetryable: false,  // ← 网络错误不可重试
}
```

**优化方向**

根据具体错误类型区分：`io.EOF`、`connection reset`、`context deadline exceeded`
（非调用方主动取消）等应标记为 `IsRetryable: true`；TLS 证书错误、DNS 解析失败等
非瞬态错误保持 `false`。可引入 `errors.Is` 匹配 `net.Error.Temporary()` 或
`net.Error.Timeout()` 作为判断依据。

---

### Issue-05 · `markAssistantCancelled` 存在已知竞态未修复

| 属性 | 值 |
|------|----|
| 文件 | `session/prompt.go:494-512` |
| 类型 | 竞态条件（已有文档，未修复） |

**问题**

`cleanup()` 等待工具 goroutine 最多 250ms，超时后 goroutine 可能仍在运行。
`markAssistantCancelled` 调用 `ListParts` 时，已完成的工具结果可能在 250ms 后才写入，
导致该结果被误判为 `Cancelled` 而不是 `Interrupted`，工具结果对下一轮 LLM 不可见。
代码注释已承认此问题但标注为 future work。

**优化方向**

在 `cleanup()` 内无条件等待所有工具 goroutine 退出（去掉 250ms 超时上限），
或将 `markAssistantCancelled` 的调用移到 `toolWg.Wait()` 之后，
确保读取 `ListParts` 时所有工具写入均已完成。

---

### Issue-06 · `Prune` 阈值比较存在双重换算导致语义模糊

> **✅ 已修复** (commit `80b440e`) — 去掉多余的 `/4`；`totalPruned` 已是 token 估算值，直接与 `PruneMinimum` 比较。

| 属性 | 值 |
|------|----|
| 文件 | `session/compaction.go:712` |
| 类型 | 逻辑缺陷 |

**问题**

`totalPruned` 在累加时已做一次 `/4`（字符→token 估算），比较阈值时又对
`PruneMinimum` 做一次 `/4`，导致实际生效阈值是 `20_000 / 4 / 4 = 1_250` 个估算
token（约 5KB 工具输出），远低于设计意图的 20_000 token。

```go
outputTokens := len(d.Output) / 4        // 第一次 /4：字符→token
...
if totalPruned < PruneMinimum/4 {        // 第二次 /4：意图不清
```

**优化方向**

统一单位：要么 `totalPruned` 以字符计，阈值用 `PruneMinimum * 4`；
要么 `totalPruned` 以 token 估算计，阈值直接用 `PruneMinimum`，去掉 `/4`。
同时补充单位注释，明确 `totalPruned` 的量纲。

---

## P2 — 代码质量 / 维护隐患

### Issue-07 · `overflow.go` 重复定义 `max`/`min`

| 属性 | 值 |
|------|----|
| 文件 | `llm/overflow.go:121-133` |
| 类型 | 冗余代码 |

**问题**

`go.mod` 声明 `go 1.25.0`，Go 1.21+ 已内置 `min`/`max` 泛型函数，
`overflow.go` 底部仍手动定义了两个仅支持 `int` 的版本，属于冗余代码。

**优化方向**

删除手动定义的 `max`/`min`，直接使用内置函数。

---

### Issue-08 · `PruneMinimum`/`PruneProtect` 常量在两个包中重复定义

| 属性 | 值 |
|------|----|
| 文件 | `llm/overflow.go:13-14` |
| 文件 | `session/compaction.go:52-53` |
| 类型 | 重复定义，存在漂移风险 |

**问题**

`PruneMinimum = 20_000` 和 `PruneProtect = 40_000` 在 `llm` 和 `session` 两个包中
各定义一份，且 `session` 包没有引用 `llm` 包中的版本。两处若不同步修改会静默产生行为差异。

**优化方向**

将常量统一保留在 `llm/overflow.go`，`session` 包直接引用 `llm.PruneMinimum` 和
`llm.PruneProtect`，删除 `session/compaction.go` 中的重复定义。

---

### Issue-09 · `provider.Registry` 无并发保护

| 属性 | 值 |
|------|----|
| 文件 | `provider/provider.go:78-100` |
| 类型 | 并发安全隐患 |

**问题**

`Registry` 的 `providers` 和 `factories` 两个 map 的读写均无 mutex 保护。
若在多 goroutine 环境下并发调用 `Register`/`RegisterFactory`/`Get`/`GetModel`
会触发 data race。虽然实际注册通常在启动阶段完成，但没有任何文档约束或编译期保证。

**优化方向**

为 `Registry` 添加 `sync.RWMutex`：写操作（`Register`、`RegisterFactory`）使用写锁，
读操作（`Get`、`GetModel`、`List`）使用读锁。或在文档中明确标注
"注册必须在任何读取发生前完成，非并发安全"。

---

### Issue-10 · `provider.Model` 嵌入 `llm.Model` 导致 key 与字段不一致风险

| 属性 | 值 |
|------|----|
| 文件 | `provider/provider.go:53-63` |
| 类型 | 数据一致性隐患 |

**问题**

`provider.Info.Models` 用 `modelID` 作 map key，但 `provider.Model` 嵌入的
`llm.Model.ID` 字段值与 map key 是否一致完全靠调用方自律，没有任何约束或校验。
构造时若写错 key，`GetModel` 查到的 model 的 `ID` 字段会与查询参数不匹配。

**优化方向**

在 `Register` 或 `GetModel` 时做防御性校验：若 `model.ID != key` 则返回错误或 panic。
或改用方法封装注册，强制从 `model.ID` 派生 key，消除不一致的可能。

---

### Issue-11 · `openai/openai.go` 中 `extractText` 函数未使用

| 属性 | 值 |
|------|----|
| 文件 | `provider/openai/openai.go:269-277` |
| 类型 | Dead code |

**问题**

`extractText` 函数定义后从未在任何地方调用，是纯 dead code。

**优化方向**

直接删除。

---

### Issue-12 · `openai/openai.go` `convertMessages` 中 `switch` 缩进格式错误

| 属性 | 值 |
|------|----|
| 文件 | `provider/openai/openai.go:190-208` |
| 类型 | 格式问题（未经 `gofmt`） |

**问题**

`for` 循环内的 `switch` 语句缩进与外层对齐（少一级），而部分 `case` 分支却多缩进一级，
不符合 `gofmt` 标准，说明该段代码未经格式化工具检查。

**优化方向**

运行 `gofmt -w provider/openai/openai.go` 统一修复，并在 CI 中加入
`gofmt` 检查防止回归。

---

### Issue-13 · `record_provider.go` ctx 取消时录制记录静默丢失

| 属性 | 值 |
|------|----|
| 文件 | `llm/record_provider.go:104-110` |
| 类型 | 可观测性缺失 |

**问题**

ctx 取消时 goroutine 排空 inner channel 后直接 return，当次 `Record` 不写入文件。
调用方无法区分"正常结束录制"和"ctx 取消导致录制中断"，录制文件可能静默缺失某些 step。

```go
case <-ctx.Done():
    for range inner {}
    return  // ← 无日志，无标记，Record 静默丢失
```

**优化方向**

ctx 取消时写入一条带 `EventError`（携带 `ctx.Err()`）的不完整 Record，
或至少输出一条 `log.Printf` 标明哪个 step 因取消而未录制，保持录制文件的可追溯性。

---

## store/ 包

### Issue-21 · `CreatePart` / `CreateMessage` 不验证外键，孤儿记录静默写入

| 属性 | 值 |
|------|----|
| 文件 | `store/memory/memory.go:122`, `store/memory/memory.go:177` |
| 类型 | 约束缺失，测试与生产行为不一致 |

**问题**

`CreateMessage` 不检查 `m.SessionID` 是否在 `sessions` 中存在；
`CreatePart` 不检查 `p.MessageID` 是否在 `messages` 中存在。
可以写入指向不存在父记录的孤儿数据，`List*` 方法都能正常返回这些孤儿记录。

SQLite 实现会有外键约束，两者行为不一致，使得依赖内存 store 的测试可能产生假通过。

**优化方向**

在 `CreateMessage` 中检查 `s.sessions[m.SessionID]` 存在；
在 `CreatePart` 中检查 `s.messages[p.MessageID]` 存在；
不存在时返回明确错误，与 SQLite 的外键行为对齐。

---

### Issue-22 · `Part.Data` 指针浅拷贝，调用方可绕过 `UpdatePart` 静默污染 store

| 属性 | 值 |
|------|----|
| 文件 | `store/memory/memory.go:187`, `store/memory/memory.go:200` |
| 类型 | 数据隔离失效 |

**问题**

`CreatePart` 和 `GetPart` 均做 `cp := *p`（值拷贝），但 `Part.Data` 是 `any`，
实际存储的是 `*ToolPartData` 等指针。值拷贝只复制指针，内部数据仍共享：

```go
// GetPart 返回后，调用方可静默修改 store 内部状态：
p, _ := s.GetPart(ctx, "p1")
p.Data.(*store.ToolPartData).Output = "injected"  // store 内部被改！
```

`TestPart_isolation` 只测了修改 `p.Type`（值字段），未覆盖修改 `p.Data` 内部的场景，
测试实际上是不充分的。

**优化方向**

在 `CreatePart`/`GetPart`/`UpdatePart` 中对 `Data` 做深拷贝（JSON round-trip 或手动拷贝各类型），
或在接口文档中明确"返回值不可修改"（但这违反了 store 接口的一般期望）。

---

### Issue-23 · `DeleteSession` 线性扫描 `sessionOrder`

| 属性 | 值 |
|------|----|
| 文件 | `store/memory/memory.go:111-116` |
| 类型 | 性能（测试场景影响小） |

**问题**

删除 session 时需线性扫描 `sessionOrder` slice 找到对应下标，O(n)。
对测试场景无实际影响，但如作为长期运行的内存缓存使用，大量 session 时有性能隐患。

**优化方向**

可接受现状并在文档注明"仅用于测试/短期场景"；或改用 `ordermap` 维护索引。

---

### ~~Issue-24~~ · ~~`PartTypeStepStart` 无数据类型~~ — **已撤销**

经核实，`context.go:305-307` 明确注释 `// step-start parts are structural; not sent to LLM`，
有意忽略该类型，不需要数据结构体。设计正确，非问题。

---

### Issue-25 · 多个 PartType 常量从未使用，`PartTypeAgent` 有读无写

| 属性 | 值 |
|------|----|
| 文件 | `store/store.go:117-130` |
| 类型 | Dead code / 意图不明 |

**问题**

`PartTypeSnapshot`、`PartTypeRetry`、`PartTypeSubtask`、`PartTypePatch` 在整个代码库中
未见写入或读取，无注释说明是预留接口还是历史遗留。

`PartTypeAgent` 情况更严重：`context.go:251` 有读取分支，但全库没有任何地方写入该类型的 Part，
意味着这个 `case` 分支**永远不会执行**，是 dead branch。

**优化方向**

- Snapshot/Retry/Subtask/Patch：若是预留接口，补充注释 `// reserved for future use`；若是遗留，删除。
- Agent：补充写入路径，或删除 `context.go:251` 的读取分支。

---

### Issue-26 · `CompactionPartData.SummaryMessageID` 从未赋值

| 属性 | 值 |
|------|----|
| 文件 | `store/store.go:180-182`, `session/compaction.go:360-368` |
| 类型 | 字段语义失效 |

**问题**

`CompactionPartData` 定义了 `SummaryMessageID` 字段，但创建时始终传入零值：

```go
Data: &store.CompactionPartData{}  // SummaryMessageID = ""
```

该字段如果是为了做 boundary→summary 的关联查询而设，当前永远无法使用。

**优化方向**

在 `Compact()` 的 Step 4 创建 CompactionPart 时，将 `summaryMsgID` 填入该字段：

```go
Data: &store.CompactionPartData{SummaryMessageID: summaryMsgID}
```

---

### Issue-16 · `estimateTurnTokens` fallback 值 100 严重低估消息大小

> **✅ 已修复** (commit `80b440e`) — fallback 改为按角色估算：user=500 token，其他=300 token。

| 属性 | 值 |
|------|----|
| 文件 | `session/compaction.go:209` |
| 类型 | 计算错误 / 压缩效果失效 |

**问题**

没有 token 记录的消息（用户消息、未经 `finaliseAssistantMessage` 的 assistant 消息）
兜底估算为 100 token。实际上一条中等长度用户消息通常有 500-2000 token，低估会让
`Select` 误判 tail 能容纳更多内容，**导致本应进入摘要 head 的消息被保留在 tail，
压缩效果失效**，严重时历史永远无法收敛。

```go
total += 100 // fallback for messages without stored usage data
```

**优化方向**

将 fallback 提高到更保守的值（如 500），或改为按消息角色分类估算：
- 用户消息：按文本 part 字符数 `/4` 兜底
- assistant 消息：按 part 个数 × 200 token 估算

---

### Issue-17 · `buildRecentContextExcerpt` 工具调用不含 CallID，多次相同调用无法区分

| 属性 | 值 |
|------|----|
| 文件 | `session/compaction.go:494` |
| 类型 | 可观测性不足 |

**问题**

excerpt 中工具调用只记录 tool name 和 output，不含 CallID：

```go
fmt.Fprintf(&sb, "- 调用工具: %s → %s\n", d.Tool, output)
```

同一轮多次调用相同工具（如多次 `read_file`）时，LLM 无法区分各次调用，
excerpt 的上下文锚定价值下降。

**优化方向**

加入 CallID 前缀（可截取前 8 位）：
```go
fmt.Fprintf(&sb, "- 调用工具: %s(%s) → %s\n", d.Tool, d.CallID[:8], output)
```

---

### ~~Issue-18~~ · ~~`ProcessCompact` 失败路径死锁~~ — **已撤销**

经核实，`RunLoopAsync` 的 goroutine 有 `defer h.closeStoreDone()`（`prompt.go:145`），
`runLoopInternal` 任何路径返回后 goroutine 退出，defer 必然执行，`StoreDone` 会被关闭。
不存在永久阻塞。实际差异仅是 `ProcessCompact` 失败时 `StoreDone` 与 `Done` 几乎同时关闭
（失去提前优先），属于语义降级而非 bug，不单独记录。

---

### Issue-19 · `roleUser`/`roleAssistant` 常量定义但从未使用

| 属性 | 值 |
|------|----|
| 文件 | `session/context.go:395-398` |
| 类型 | Dead code |

**问题**

文件末尾定义了两个 package 级常量 `roleUser` 和 `roleAssistant`，
全包范围内没有任何代码引用它们，属于 dead code。

**优化方向**

直接删除。若确需复用可改为引用 `store.RoleUser` / `store.RoleAssistant`。

---

### Issue-20 · `SystemPromptForModel` 依赖 `model.APIID`，字段可能为空

| 属性 | 值 |
|------|----|
| 文件 | `session/system.go:55` |
| 类型 | 静默降级风险 |

**问题**

`SystemPromptForModel` 用 `model.APIID` 做匹配，但如果 provider 构造
`llm.Model` 时未填 `APIID`（或只填了 `ID`），所有 `Contains` 条件均不匹配，
静默走到 `default` 分支，使用错误的系统 prompt，且无任何日志或告警。

**优化方向**

当 `model.APIID == ""` 时 fallback 到 `model.ID` 进行匹配，并补充 `log.Printf`
说明 fallback 原因；或在构造 `llm.Model` 时校验 APIID 必填。

---

## store/ 包（续）

### Issue-27 · `SessionHistorySource` 持写锁期间执行 SQLite I/O，所有操作完全串行

| 属性 | 值 |
|------|----|
| 文件 | `store/session_history.go:227-228`, `store/session_history.go:283-284`, `store/session_history.go:330` |
| 类型 | 并发性能缺陷（已确认） |

**问题**

`Peek`、`Fetch`、`Hook` 三个公开方法均在持有 `s.mu`（写锁）期间直接调用 SQLite I/O：

- `Peek`（`session_history.go:251`）：锁内调用 `src.Peek()` → SQLite `LIKE` 查询
- `Fetch`（`session_history.go:301`）：锁内调用 `FindSeqByDocID()` → SQLite 查询
- `Fetch`（`session_history.go:485`）：锁内调用 `LoadRecordsBySeq()` → SQLite 查询（page-in）
- `Hook`（`session_history.go:348`）：锁内调用 `SaveRecord()` → SQLite 写

后果：任意时刻只有一个 goroutine 能执行历史检索，`RunLoop` 的 `knowledge_search`
工具调用与 `Compact` 触发的 `Hook` 之间完全串行，SQLite I/O 延迟（通常 1-10ms）
被放大为整个历史系统的吞吐瓶颈。

**优化方向**

将 I/O 移到锁外：先无锁（或读锁）完成 SQLite 查询，再短暂加写锁修改内存状态
（`compactionDocs`、`loadedSeqs`、`lruOrder`）。`Hook` 的 `SaveRecord` 可在锁外
异步写入，或在锁外完成后再更新内存索引。

---

### Issue-28 · `seqForDoc` 双层线性扫描，缺少反向索引

| 属性 | 值 |
|------|----|
| 文件 | `store/session_history.go:499-508` |
| 类型 | 算法效率问题（已确认） |

**问题**

每次 `Fetch` 调用都触发对 `compactionDocs`（`map[int][]string`）的双层遍历：

```go
for seq, ids := range s.compactionDocs {   // 外层：所有 seq
    for _, id := range ids {               // 内层：每个 seq 的所有 doc ID
        if id == docID {
```

复杂度 O(seqs × docs_per_seq)，且在持 `s.mu` 写锁期间执行（见 Issue-27），阻塞
时间随历史规模线性增长。

**优化方向**

维护一个反向索引 `docIndex map[string]int`（docID → seq），在 `Hook` 写入时同步更新，
`seqForDoc` 退化为 O(1) map 查找。`evictL0IfNeeded` 删除 seq 时同步清除对应 doc ID。

---

### Issue-29 · `Compact()` 加载 parts 使用 N+1 查询

> **✅ 已修复** (commit `80b440e`) — N+1 循环替换为单次 `ListPartsBySession` 批量查询。

| 属性 | 值 |
|------|----|
| 文件 | `session/compaction.go:263-270` |
| 类型 | 性能问题（已确认） |

**问题**

`Compact()` 加载所有消息的 parts 时，对每条消息单独调用一次 `ListParts`：

```go
for _, m := range msgs {
    ps, err := c.store.ListParts(ctx, m.ID)   // ← 每条消息一次 SQLite 查询
    ...
    allParts[m.ID] = ps
}
```

消息数量为 N 时产生 N+1 次数据库查询。`runLoopInternal` 的 `loadMessages`
（`prompt.go:476`）已有 `ListPartsBySession` 单次批量查询的正确实现，
`Compact` 没有复用。

**优化方向**

将 `Compact` 中的 N+1 循环替换为 `ListPartsBySession` 单次查询：

```go
allParts, err := c.store.ListPartsBySession(ctx, sessionID)
```

与 `loadMessages` 保持一致。

---

### Issue-31 · `Reset()` 只删 L0 窗口内的 seq，SQLite 历史数据泄漏

> **✅ 已修复** (commit `80b440e`) — `Reset()` 改用新增的 `DeleteAllRecords` 接口方法，一次性删除该 session 全部历史，不依赖内存窗口快照。

| 属性 | 值 |
|------|----|
| 文件 | `store/session_history.go:188-215` |
| 类型 | 语义正确性 bug（P0） |

**问题**

`Reset()` 遍历 `s.compactionDocs` 逐 seq 调用 `DeleteRecordsBySeq`：

```go
for seq := range s.compactionDocs {
    _ = s.persistStore.DeleteRecordsBySeq(ctx, s.sessionID, seq)
}
```

`compactionDocs` 最多持有 `maxIndexedSeqs`（默认 80）个 seq。若历史上发生过 L0 驱逐
（累计超过 80 次 compaction），SQLite 中仍保有更老的 seq 记录——这些 seq 不在
`compactionDocs` 里，`Reset()` 不会删除它们。

调用方调用 `Reset()` 的语义预期是"清空该 session 的全部历史"，但实际上 SQLite 里留有
孤儿数据，下次重启后 `LoadSeqIndex` 会把它们重新加载回 L0，历史记录"复活"。

**优化方向**

`PersistStore` 补充 `DeleteAllRecords(ctx, sessionID)` 接口方法，`Reset()` 改为调用该方法
一次性删除该 session 的全部历史记录，不依赖 `compactionDocs` 的内存快照：

```go
func (s *SessionHistorySource) Reset() error {
    s.mu.Lock()
    defer s.mu.Unlock()
    if s.persistStore != nil {
        _ = s.persistStore.DeleteAllRecords(context.Background(), s.sessionID)
    }
    ...
}
```

---

### Issue-32 · `NewSessionHistorySource` L0 恢复时 `lruOrder` 可能含重复项

| 属性 | 值 |
|------|----|
| 文件 | `store/session_history.go:157-170` |
| 类型 | 防御性 bug（P2） |

**问题**

启动时从 SQLite 恢复 L0 的循环：

```go
for seq, ids := range seqIndex {
    src.compactionDocs[seq] = ids       // map 自动去重
    src.lruOrder = append(src.lruOrder, seq)  // slice 无去重
    ...
}
```

若 `LoadSeqIndex` 返回的 `seqIndex` 存在重复 seq（异常 SQLite 数据或实现 bug），
`compactionDocs`（map）会自动覆盖，而 `lruOrder`（slice）会有重复项。
后续 `touchLRU` / `removeLRU` 均只处理第一次出现的位置，导致 `lruOrder` 长度与
`compactionDocs` 大小永久不一致，LRU 驱逐行为错乱。

**优化方向**

恢复循环中追加 `lruOrder` 前做存在性检查，或在 `sortInts` 之后做一次去重：

```go
seen := make(map[int]struct{})
for seq, ids := range seqIndex {
    if _, dup := seen[seq]; dup { continue }
    seen[seq] = struct{}{}
    src.compactionDocs[seq] = ids
    src.lruOrder = append(src.lruOrder, seq)
    ...
}
```

---

### Issue-33 · `Peek` SQLite 查询不受 Bleve 结果数影响，可能执行无效查询

| 属性 | 值 |
|------|----|
| 文件 | `store/session_history.go:248-253` |
| 类型 | 性能浪费（P2） |

**问题**

`Peek` 中 Bleve 和 SQLite 搜索并行独立执行，两者均以 `size`（`q.MaxResults`）为上限：

```go
bleveResults, _ := s.bleveSearch(ctx, q.Input, size)   // 最多 size 条
// ...
if src, ok := s.persistStore.(knowledge.Source); ok {
    sqlResults, _ = src.Peek(ctx, q)   // 也是 size 条（q.MaxResults 未调整）
}
```

当 Bleve 已返回 `size` 条结果时，合并阶段 `merged` 已满，SQLite 结果一条也不会被采用，
但 SQLite 查询依然已经发出并完成，造成一次无效 I/O。

**优化方向**

先执行 Bleve 搜索，若 `len(bleveResults) >= size` 则跳过 SQLite 查询：

```go
bleveResults, bleveSeqs := s.bleveSearch(ctx, q.Input, size)
var sqlResults []knowledge.Result
if len(bleveResults) < size && s.persistStore != nil {
    if src, ok := s.persistStore.(knowledge.Source); ok {
        remaining := knowledge.Query{...q, MaxResults: size - len(bleveResults)}
        sqlResults, _ = src.Peek(ctx, remaining)
    }
}
```

---

### Issue-30 · `PersistStore.(knowledge.Source)` 类型断言静默失败，L2 检索无提示降级

| 属性 | 值 |
|------|----|
| 文件 | `store/session_history.go:250` |
| 类型 | 隐式接口耦合，可观测性缺失（已确认） |

**问题**

`Peek` 中通过 duck-typing 将 `PersistStore` 当作 `knowledge.Source` 使用：

```go
if src, ok := s.persistStore.(knowledge.Source); ok {
    sqlResults, _ = src.Peek(ctx, q)
}
```

`PersistStore` 接口定义（`store/persist.go`）不要求实现 `knowledge.Source`，
该断言是隐式合约。若某个 `PersistStore` 实现不满足断言，L2 全量检索静默跳过，
`Peek` 只返回 L1（Bleve）结果，没有任何 warning 或 error，调用方无从感知降级。

**优化方向**

将 L2 Source 作为独立字段注入 `SessionHistorySource`：

```go
type SessionHistorySource struct {
    ...
    l2Source knowledge.Source   // 可为 nil
}
```

由调用方在构造时显式传入，去掉运行时类型断言，接口契约在编译期可见。

---

## 持久化完整性（进程崩溃 / 意外退出）

> 以下 Issue-34 ～ Issue-37 专门针对"程序退出或崩溃时数据是否安全落盘"这一问题。

### Issue-34 · `Hook()` N 条 `SaveRecord` 无事务，崩溃导致 `history_docs` 残缺 seq

> **✅ 已修复** (commit `80b440e`) — 新增 `PersistStore.SaveRecords` 接口，sqlite 实现用单事务批量写入；`Hook()` 改为先构造完整 `[]Record` 再一次性提交，且 Bleve 写入移到 SQLite 确认后，失败时回滚 `currentSeq`。

| 属性 | 值 |
|------|----|
| 文件 | `store/session_history.go:342-349` |
| 类型 | 数据完整性 bug（P0） |

**问题**

`Hook()` 内对每条 head message 单独调用一次 `SaveRecord`（独立 INSERT），没有显式事务：

```go
for i, m := range head {
    rec := buildDoc(m, parts[m.ID], seq, i)
    _ = s.index.Index(m.ID, rec)
    if s.persistStore != nil {
        _ = s.persistStore.SaveRecord(ctx, s.sessionID, rec)  // 独立 INSERT
    }
}
```

SQLite WAL 模式保证单条 SQL 的原子性，但不保证多条 SQL 之间的原子性。
若进程在第 k 条 INSERT 完成、第 k+1 条之前收到 SIGKILL：
- `history_docs` 里留下一个只有前 k 行的残缺 seq
- 重启后 `LoadSeqIndex` 会把这个残缺 seq 加载进 L0
- 后续 `Fetch` 只能找到部分文档，历史召回内容不完整且无法感知

**修复方案**

在 `PersistStore` 接口增加 `SaveRecords(ctx, sessionID, []Record) error`，`sqlite.Store` 实现使用单个显式事务（`BeginTx` → `PrepareContext` → 逐行 `Exec` → `Commit`，失败时 `Rollback`）。`Hook()` 改为先构造完整 `[]Record` 切片，再调用一次 `SaveRecords`，保证整个 seq 的写入是原子的。

---

### Issue-35 · `Hook()` 中 `SaveRecord` 错误全部静默丢弃

> **✅ 已修复** (commit `80b440e`) — 配合 Issue-34 改为 `SaveRecords`，失败时 `log.Printf` 记录错误并回滚内存状态，不再静默丢弃。

| 属性 | 值 |
|------|----|
| 文件 | `store/session_history.go:348` |
| 类型 | 可观测性缺失（P1） |

**问题**

```go
_ = s.persistStore.SaveRecord(ctx, s.sessionID, rec)
```

`SaveRecord` 失败（磁盘满、SQLite 锁超时、I/O 错误）时错误被 `_ =` 完全丢弃。
内存层（Bleve / `compactionDocs`）认为写入成功，SQLite 实际上没有写。
重启后该 seq 从 SQLite 恢复为空，Bleve 也已清空，该次 compaction 的所有历史文档
**永久消失**，且调用方无任何感知。

**修复方案**

配合 Issue-34 改为 `SaveRecords`，失败时：
1. 不更新内存层（`compactionDocs` / `loadedSeqs` / `lruOrder`），回滚 `currentSeq`
2. 至少 `log.Printf` 记录错误，保证可观测性
3. 可选：返回 error 给 `Hook` 调用方（`Compact` step 5），让 compaction 感知持久化失败

---

### Issue-36 · 流式中途 SIGKILL：part `TimeEnd=0` / `Status=pending`

| 属性 | 值 |
|------|----|
| 文件 | `session/processor.go:203-223` |
| 类型 | 状态字段不完整（P1） |

**问题**

流式过程中每个 delta 实时调用 `UpdatePart`，文本内容会落盘，但 `TimeEnd` 和
最终 `Status` 只在 `EventTextEnd` / `EventRequestFinish` 时写入。
若进程在流式传输中途收到 SIGKILL：
- `TextPartData.TimeEnd == 0`（未关闭计时）
- `ToolPartData.Status` 留在 `pending` 或 `running`
- `Message.Tokens` 未更新（`finaliseAssistantMessage` 未执行）

消息文本内容本身已落盘，但这些状态字段不干净，`ToModelMessages`
处理 `pending` 工具 part 时行为依赖上层逻辑，可能产生格式错误的 LLM 请求。

**修复方案**

启动时对当前 session 的所有 `Status=pending/running` 的 tool part 做一次修复扫描：
将其标记为 `ToolStatusError`（携带 `interrupted=true`），与 `cleanup()` 的处理逻辑对齐。
`TimeEnd=0` 的 text part 可在加载时以 `UpdatedAt` 作为兜底时间戳填充。

---

### Issue-37 · tool goroutine 250ms 超时后进程退出，飞行中 `UpdatePart` 可能丢失工具 Output

| 属性 | 值 |
|------|----|
| 文件 | `session/processor.go:422-439` |
| 类型 | 竞态 / 数据丢失（P1） |

**问题**

`cleanup()` 等待 tool goroutine 最多 250ms，超时后继续执行（`processor.go:437`）。
此时 goroutine 仍在运行，持有的 `UpdatePart` 调用可能正在飞行中。
若进程在 goroutine 完成 `UpdatePart` 之前退出（如 `os.Exit` 或 SIGKILL），
该工具的 `Output` 字段丢失，重启后该 part 永久留在 `interrupted` 状态。

与 Issue-05（已记录的 race）不同，Issue-05 关注的是状态覆盖；此处关注的是进程退出时
工具结果的持久化窗口。

**修复方案**

最彻底的方案是移除 250ms 上限，改为无条件 `toolWg.Wait()`（工具已收到 ctx 取消信号，
正常情况下会快速退出）。若必须保留超时，可在超时后将飞行中 goroutine 的 part 标记为
`interrupted`，防止后续写入覆盖该标记（Issue-02 的修复也有助于此）。

---

## 汇总

| ID | 严重级别 | 状态 | 文件 | 一句话描述 |
|----|----------|------|------|-----------|
| Issue-01 | P0 | ✅ 已修复 | `provider/anthropic/anthropic.go:345`, `provider/openai/openai.go:411` | 工具参数 JSON 解析失败静默忽略，工具以空参数执行 |
| Issue-02 | P0 | 存疑 | `session/processor.go:367-377` | `isAlreadyInterrupted` 读取失败时错误放行，cleanup 状态被覆盖 |
| Issue-03 | P0 | ✅ 已修复 | `provider/openai/openai.go:148` | 工具 schema unmarshal 错误忽略，发送空 schema 给 OpenAI |
| Issue-04 | P1 | ✅ 已修复 | `provider/anthropic/anthropic.go:415`, `provider/openai/openai.go:480` | transport 错误硬编码不可重试，弱网环境下无法恢复 |
| Issue-05 | P1 | 待修复 | `session/prompt.go:494-512` | `markAssistantCancelled` 竞态：工具结果可能被误判为 Cancelled |
| Issue-06 | P1 | ✅ 已修复 | `session/compaction.go:712` | `Prune` 阈值双重 `/4` 导致实际阈值远低于设计值 |
| Issue-07 | P2 | 待修复 | `llm/overflow.go:121-133` | 冗余的 `max`/`min` 定义，Go 1.21+ 已内置 |
| Issue-08 | P2 | 待修复 | `llm/overflow.go:13-14`, `session/compaction.go:52-53` | `PruneMinimum`/`PruneProtect` 跨包重复定义，存在漂移风险 |
| Issue-09 | P2 | 待修复 | `provider/provider.go:78-100` | `Registry` 无并发保护，多 goroutine 注册会 data race |
| Issue-10 | P2 | 待修复 | `provider/provider.go:53-63` | `Model.ID` 与 map key 无约束，可静默不一致 |
| Issue-11 | P2 | 待修复 | `provider/openai/openai.go:269-277` | `extractText` dead code |
| Issue-12 | P2 | 待修复 | `provider/openai/openai.go:190-208` | `convertMessages` switch 缩进不符合 `gofmt` |
| Issue-13 | P2 | 待修复 | `llm/record_provider.go:104-110` | ctx 取消时录制 Record 静默丢失，无可观测性 |
| Issue-14 | P1 | ✅ 已修复 | `session/compaction.go:209` | `estimateTurnTokens` fallback 100 严重低估，压缩效果失效 |
| Issue-15 | P2 | 待修复 | `session/compaction.go:494` | `buildRecentContextExcerpt` 不含 CallID，多次相同工具调用无法区分 |
| Issue-16 | P1 | ~~已撤销~~ | `session/prompt.go:380-383` | ~~`ProcessCompact` 死锁~~ **已撤销**（defer 兜底，不会死锁）|
| Issue-17 | P2 | 待修复 | `session/context.go:395-398` | `roleUser`/`roleAssistant` 常量 dead code |
| Issue-18 | P2 | 待修复 | `session/system.go:55` | `SystemPromptForModel` 依赖 `APIID`，字段为空时静默走 default prompt |
| Issue-21 | P1 | 待修复 | `store/memory/memory.go:177`, `store/memory/memory.go:122` | `CreatePart`/`CreateMessage` 不验证外键，孤儿记录静默写入 |
| Issue-22 | P2 | 待修复 | `store/memory/memory.go:187` | `Part.Data` 指针浅拷贝，当前路径不出错但测试覆盖不充分 |
| Issue-23 | P2 | 待修复 | `store/memory/memory.go:111-116` | `DeleteSession` 线性扫描 `sessionOrder`，大量 session 时 O(n) |
| Issue-24 | P2 | ~~已撤销~~ | `store/store.go:121` | ~~`PartTypeStepStart` 无数据类型~~ **已撤销**（有意设计，structural marker）|
| Issue-25 | P2 | 待修复 | `store/store.go:117-130` | `PartTypeSnapshot`/`Retry`/`Subtask`/`Patch` 未使用；`PartTypeAgent` 有读无写 |
| Issue-26 | P2 | 待修复 | `store/store.go:180-182` | `CompactionPartData.SummaryMessageID` 字段从未赋值，始终为空 |
| Issue-27 | P1 | 待修复 | `store/session_history.go:227,283,330` | 持写锁期间执行 SQLite I/O，所有历史检索操作完全串行 |
| Issue-28 | P2 | 待修复 | `store/session_history.go:499-508` | `seqForDoc` 双层线性扫描，缺反向索引，Fetch 路径 O(n²) |
| Issue-29 | P1 | ✅ 已修复 | `session/compaction.go:263-270` | `Compact()` 加载 parts 使用 N+1 查询，应改用 `ListPartsBySession` |
| Issue-30 | P2 | 待修复 | `store/session_history.go:250` | `PersistStore.(Source)` 断言静默失败，L2 检索无提示降级 |
| Issue-31 | P0 | ✅ 已修复 | `store/session_history.go:193` | `Reset()` 只删 L0 窗口内 seq，SQLite 历史数据泄漏 |
| Issue-32 | P2 | 待修复 | `store/session_history.go:160` | L0 恢复时 `lruOrder` 无去重，重复 seq 导致 LRU 错乱 |
| Issue-33 | P2 | 待修复 | `store/session_history.go:248` | `Peek` SQLite 查询不受 Bleve 结果数影响，Bleve 命中满时仍发起无效 I/O |
| Issue-34 | P0 | ✅ 已修复 | `store/session_history.go:342-349` | Hook 内 N 条 `SaveRecord` 无事务包装，崩溃导致 history_docs 残缺 seq |
| Issue-35 | P1 | ✅ 已修复 | `store/session_history.go:348` | `SaveRecord` 错误被 `_ =` 全部丢弃，SQLite 写失败无任何告警 |
| Issue-36 | P1 | 待修复 | `session/processor.go:214,220` | 流式中途 SIGKILL：part `TimeEnd=0`/`Status=pending`，状态字段不完整 |
| Issue-37 | P1 | 待修复 | `session/processor.go:437` | tool goroutine 超时 250ms 后进程退出，飞行中的 `UpdatePart` 可能丢失工具 Output |
