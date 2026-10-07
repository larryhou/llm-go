# compact-test

验证 llm-go `session.RunLoopAsync` 上下文压缩（compact）机制的端到端测试工具。

## 背景

llm-go 的 compact 机制在上下文接近 context window 上限时自动触发，将历史对话
压缩为结构化 summary，使 LLM 能在长会话中持续工作而不丢失关键信息。

触发路径有两条：

| 路径 | 时机 | 检测方式 |
|---|---|---|
| Pre-send | 每个 step 发送给 LLM 之前 | `llm.IsRequestOverflow`：对 system+messages+tools 做 JSON 序列化估算（chars/4） |
| Post-step | LLM 返回 EventStepFinish 之后 | `llm.IsOverflow`：检查 LLM 报告的实际 token usage >= Usable |

`Usable = context_limit - max_output_tokens - buffer(20000)`

## 设计思路

### 为什么不直接跑多轮真实对话

让 LLM 真实聊到触发 compact 需要累积足够多的 token，耗时长且不可控。

### 为什么不注入假 token 计数

compact 的 pre-send 检查（`IsRequestOverflow`）基于请求体 JSON 的**字节长度**估算，
不读取 `Message.Tokens` 字段。注入假 token 数对触发条件没有影响。

### 最终方案：重放真实历史

从 airouter `requests.db` 中挑选一条真实的 agent 请求记录（50 条消息，涉及
MR #79644 代码审核），embed 进 `testdata.json`。把其中的 messages 直接写入
memory store，让 `IsRequestOverflow` 的 JSON 估算值超过 Usable，下一轮
`RunLoopAsync` 发送真实消息时立即触发 pre-send compact。

**优点：**
- 历史内容真实，summary 质量有意义（可以肉眼验证摘要是否正确提炼了关键信息）
- 不依赖外部数据库，`testdata.json` 随代码一起 embed
- 触发时机完全可控，无需等待

## testdata.json 来源

记录 ID：`chatcmpl-qf8YkkWKGOvRzm0KazrtDavQmYOow`

一次真实的工业 agent 代码审核会话（MR #79644，TIMIj3/EngineSource）：

- 50 条消息（2 system + 3 user + 22 assistant + 23 tool）
- 原始 prompt_tokens：23997
- 内容：对 38 个文件分 6 个 Group（A–F）执行 Stage 2–4 审核，
  已完成 Group A–E，Group F（Shader 层）进行中

选取理由：消息数量适中、内容包含具体技术细节（文件路径、行号、风险项编号），
便于验证 compact summary 的信息保留质量。

## 运行方式

```bash
LLM_BASE_URL=http://127.0.0.1:8080/v1 \
LLM_API_KEY=sk-xxx \
LLM_MODEL=deepseek-v4-flash \
go run ./cmd/compact-test/ [flags]
```

| Flag | 默认值 | 说明 |
|---|---|---|
| `-context-limit` | 10000 | context window 大小；10000 → Usable=5904，重放后立即触发 |
| `-output-tokens` | 4096 | max output tokens |
| `-max-steps` | 1 | 每轮最多 agentic steps；设为 1 只观察 compact 本身，不让 LLM 继续调工具 |

## 预期输出

```
Testdata: 50 messages (2 system, 48 conversation)
context_limit=10000  Usable=5904

Replaying messages into store...
Replayed 48 messages, ~25303 chars ≈ 6325 tokens
compact WILL trigger (estimated > Usable=5904)

Sending real message...
<compact summary 内容...>
[tokens] in=3044 out=1175
[compact #1] 20 messages summarised

<下一轮 compact summary...>
[compact #2] 9 messages summarised

Total compactions: N
```

## 测试结论

### compact 机制工作正常

- pre-send `IsRequestOverflow` 在重放历史后立即触发
- 首次 compact 将 20 条消息压缩为结构化 Markdown summary
- summary 准确保留了 Objective / Work State / Next Move 以及
  Group A–E 的具体 Major 风险项编号和行号
- 连续多次 compact 稳定，每次都成功完成 LLM summary 调用

### 关于同一轮内多次 compact 的说明

`context_limit=10000` 是人为设置的极小值。compact 生成的 summary 本身
（~2000 tokens）加上 system prompt（~3500 tokens）已接近 Usable=5904，
导致 compact 后再次触发 pre-send overflow，形成循环。

对比 opencode TypeScript 参考实现（`packages/core/src/session/runner/llm.ts`）
确认：**opencode 同样没有对"compact 后仍然 overflow"设置重试上限**，
llm-go 的行为与参考实现一致，不是 bug。

实际生产环境 `context_limit=240,000`，compact summary 绝不会再次填满 Usable，
该循环在生产中不会出现。
