package session

// observer.go — RunLoop 运行时可观测性钩子。
//
// RunObserver 让调用方订阅一次 RunLoop（含其 delegate 子 session）执行过程中的
// 关键状态变迁，用于实时监控、卡住诊断、指标上报等。它是纯被动的旁路通知，
// 不影响 RunLoop 的控制流。
//
// 设计原则：
//   - 单方法接口 OnRunEvent(RunEvent)：事件用 Kind 区分，新增事件种类无需改接口。
//   - 完全可选：RunInput.Observer 为 nil 时零开销（所有回调点先判 nil）。
//   - 向后兼容：现有调用方不传 Observer 即可，行为不变。
//   - 回调必须快速返回：OnRunEvent 在 RunLoop 的执行 goroutine 上同步调用，
//     实现方只应做轻量的状态更新/入队，严禁阻塞，否则拖慢 LLM 循环。
//   - delegate 子 session 自动继承父 Observer，事件带 ParentSessionID 以便区分层级。

import "time"

// RunEventKind 是一次 RunLoop 生命周期事件的种类。
type RunEventKind string

const (
	// RunKindTurnStart：一次用户轮次开始（user message 已落库，即将进入 agentic 循环）。
	RunKindTurnStart RunEventKind = "turn_start"
	// RunKindRequestLLM：已向 LLM 发出一次请求，等待其流式响应（首字/工具决策）。
	// 每个 agentic step 触发一次。长时间停留即"等 LLM 卡住"。
	RunKindRequestLLM RunEventKind = "request_llm"
	// RunKindGenerating：LLM 开始流式输出文本/思考（收到首个 text/reasoning delta）。
	RunKindGenerating RunEventKind = "generating"
	// RunKindToolStart：开始执行某个工具调用（Tool 字段为工具名）。
	RunKindToolStart RunEventKind = "tool_start"
	// RunKindToolEnd：某个工具调用结束（Tool 为工具名，ToolError 非空表示失败）。
	RunKindToolEnd RunEventKind = "tool_end"
	// RunKindStepFinish：一个 agentic step 完成（收到 step-finish 事件）。
	RunKindStepFinish RunEventKind = "step_finish"
	// RunKindTurnEnd：整轮 RunLoop 结束（StopReason/Err 描述结束原因）。
	RunKindTurnEnd RunEventKind = "turn_end"
	// RunKindDelegateStart：delegate_task 启动一个子 session（SessionID 为子 session ID，
	// ParentSessionID 为发起方）。子 session 自身的事件随后照常上报，带同一 ParentSessionID。
	RunKindDelegateStart RunEventKind = "delegate_start"
	// RunKindDelegateEnd：delegate_task 的子 session 结束。
	RunKindDelegateEnd RunEventKind = "delegate_end"
	// RunKindCompactStart：触发上下文压缩，开始一次压缩 LLM 调用。
	// 压缩可能耗时较久，期间 session 看似"卡住"，此事件让下游可解释该停顿。
	RunKindCompactStart RunEventKind = "compact_start"
	// RunKindCompactEnd：上下文压缩结束（Err 非空表示压缩失败）。
	RunKindCompactEnd RunEventKind = "compact_end"
	// RunKindToolsOmitted：已消费的工具结果被从后续 LLM 上下文中剔除
	// （OmitConsumedTools 优化），用于解释 context 体积的变化。
	RunKindToolsOmitted RunEventKind = "tools_omitted"
	// RunKindOverflow：上下文溢出后的降级处理（替换超大工具输出 / 截断），
	// 长时间反复 overflow 是一种异常信号。
	RunKindOverflow RunEventKind = "overflow"
	// RunKindDoomLoop：检测到同一工具+同一参数连续重复调用（doom loop），
	// RunLoop 据此中止。这本身就是一种"卡住"。Tool 为涉事工具名。
	RunKindDoomLoop RunEventKind = "doom_loop"
	// RunKindPrune：一轮结束后的 store 清理（删除旧的已剪枝 part）。
	RunKindPrune RunEventKind = "prune"
)

// RunEvent 描述一次运行时状态变迁。字段按 Kind 取用：
//   - Tool：ToolStart/ToolEnd 时为工具名；delegate 事件时为 "delegate_task"。
//   - ToolError：ToolEnd 时若工具失败则为错误信息。
//   - StopReason/Err：仅 TurnEnd（及 DelegateEnd）有意义。
//   - ParentSessionID：子 session 事件非空，标识其父 session。
type RunEvent struct {
	SessionID       string
	ParentSessionID string
	Kind            RunEventKind
	Tool            string
	ToolInput       map[string]any // tool_start: parsed tool call arguments
	ToolOutput      string         // tool_end: tool result text (on success)
	ToolError       string
	StopReason      StopReason
	Err             error
	Detail          string // 附加描述：overflow 原因 / 剔除条数 / compact 触发原因等
	At              time.Time
}

// RunObserver 接收一次 RunLoop（含 delegate 子 session）的运行时事件。
// 实现方必须并发安全且快速返回，不得阻塞 RunLoop 的执行 goroutine。
type RunObserver interface {
	OnRunEvent(RunEvent)
}

// emitRun 是内部辅助：obs 为 nil 时零开销跳过；否则补全时间戳并通知。
// 所有回调点统一经此函数，保证 nil 安全与 At 填充一致。
func emitRun(obs RunObserver, ev RunEvent) {
	if obs == nil {
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	obs.OnRunEvent(ev)
}
