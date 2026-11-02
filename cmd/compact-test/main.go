// Command compact-test 把一条真实的 agent 请求历史（embed 在 testdata.json 中）
// 重放到 store，再发一条真实消息触发 compact，验证 compact 机制的完整流程。
//
// 用法：
//
//	LLM_BASE_URL=http://... LLM_API_KEY=sk-... go run ./cmd/compact-test/ [flags]
//
// Flags:
//
//	-context-limit  context window token 数（默认 10000，Usable≈5904，重放后即触发）
//	-output-tokens  max output tokens（默认 4096）
//	-max-steps      每轮最多 agentic steps（默认 1，只看 compact 本身）
package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/larryhou/llm-go/llm"
	openaiProv "github.com/larryhou/llm-go/provider/openai"
	"github.com/larryhou/llm-go/session"
	"github.com/larryhou/llm-go/store"
	"github.com/larryhou/llm-go/store/memory"
)

//go:embed testdata.json
var testdataJSON []byte

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// hookProvider fans LLM events to an observer while keeping the session loop alive.
type hookProvider struct {
	inner   llm.Provider
	onEvent func(llm.Event)
}

func (p *hookProvider) ID() string { return p.inner.ID() }

func (p *hookProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	inner, err := p.inner.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	ch := make(chan llm.Event, 64)
	obsCh := make(chan llm.Event, 128)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range obsCh {
			p.onEvent(ev)
		}
	}()
	go func() {
		for ev := range inner {
			ch <- ev
			select {
			case obsCh <- ev:
			default:
			}
		}
		close(obsCh)
		<-done
		close(ch)
	}()
	return ch, nil
}

// openaiMessage mirrors the OpenAI chat message JSON format.
type openaiMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// contentText extracts plain text from an OpenAI content field (string or []block).
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// try string
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// try []block
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if t, ok := b["text"]; ok {
				var txt string
				if json.Unmarshal(t, &txt) == nil {
					parts = append(parts, txt)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(raw)
}

func main() {
	var (
		contextLimit = flag.Int("context-limit", 10000, "context window token limit (Usable≈5904 at 10000, triggers compact immediately after replay)")
		outputTokens = flag.Int("output-tokens", 4096, "max output tokens")
		maxSteps     = flag.Int("max-steps", 1, "max agentic steps per turn (1 = just observe compact, no tool calls)")
	)
	flag.Parse()

	// ── parse embedded testdata ───────────────────────────────────────────────
	var reqData struct {
		Messages []openaiMessage `json:"messages"`
	}
	if err := json.Unmarshal(testdataJSON, &reqData); err != nil {
		fmt.Fprintf(os.Stderr, "parse testdata.json: %v\n", err)
		os.Exit(1)
	}

	// filter out system messages — they become AgentPrompt, not store messages
	var convMsgs []openaiMessage
	var systemParts []string
	for _, m := range reqData.Messages {
		if m.Role == "system" {
			systemParts = append(systemParts, contentText(m.Content))
		} else {
			convMsgs = append(convMsgs, m)
		}
	}
	systemPrompt := strings.Join(systemParts, "\n\n")
	if systemPrompt == "" {
		systemPrompt = "你是一个简洁的中文助手。"
	}

	fmt.Printf("Testdata: %d messages (%d system, %d conversation)\n",
		len(reqData.Messages), len(reqData.Messages)-len(convMsgs), len(convMsgs))

	// ── model ─────────────────────────────────────────────────────────────────
	modelID := envOr("LLM_MODEL", "deepseek-v4-flash")
	model := llm.Model{
		ID:         modelID,
		ProviderID: "openai",
		APIID:      modelID,
		Limit:      llm.ModelLimit{Context: *contextLimit, Output: *outputTokens},
	}
	usable := llm.Usable(model, nil)
	fmt.Printf("context_limit=%d  Usable=%d\n\n", *contextLimit, usable)

	// ── store + session ───────────────────────────────────────────────────────
	st := memory.New()
	ctx := context.Background()
	sessID := "compact-test-" + newID()
	now := time.Now()
	if err := st.CreateSession(ctx, &store.Session{
		ID: sessID, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "create session: %v\n", err)
		os.Exit(1)
	}

	// ── replay conversation messages into store ───────────────────────────────
	// OpenAI format: user / assistant / tool roles interleaved.
	// We flatten tool-role messages back into the preceding assistant message
	// as ToolPartData so the store structure matches what RunLoopAsync expects.
	fmt.Println("Replaying messages into store...")
	var lastAssistantID string
	totalChars := 0

	for i, m := range convMsgs {
		t := now.Add(time.Duration(i) * time.Millisecond)
		text := contentText(m.Content)
		totalChars += len(text)

		switch m.Role {
		case "user":
			msg := &store.Message{
				ID: newID(), SessionID: sessID, Role: store.RoleUser,
				CreatedAt: t, UpdatedAt: t,
			}
			if err := st.CreateMessage(ctx, msg); err != nil {
				fmt.Fprintf(os.Stderr, "create user msg: %v\n", err)
				os.Exit(1)
			}
			if err := st.CreatePart(ctx, &store.Part{
				ID: newID(), MessageID: msg.ID, SessionID: sessID,
				Type: store.PartTypeText, CreatedAt: t, UpdatedAt: t,
				Data: &store.TextPartData{Text: text},
			}); err != nil {
				fmt.Fprintf(os.Stderr, "create user part: %v\n", err)
				os.Exit(1)
			}
			lastAssistantID = ""

		case "assistant":
			msg := &store.Message{
				ID: newID(), SessionID: sessID, Role: store.RoleAssistant,
				CreatedAt: t, UpdatedAt: t,
			}
			if err := st.CreateMessage(ctx, msg); err != nil {
				fmt.Fprintf(os.Stderr, "create assistant msg: %v\n", err)
				os.Exit(1)
			}
			if text != "" {
				if err := st.CreatePart(ctx, &store.Part{
					ID: newID(), MessageID: msg.ID, SessionID: sessID,
					Type: store.PartTypeText, CreatedAt: t, UpdatedAt: t,
					Data: &store.TextPartData{Text: text},
				}); err != nil {
					fmt.Fprintf(os.Stderr, "create assistant part: %v\n", err)
					os.Exit(1)
				}
			}
			lastAssistantID = msg.ID

		case "tool":
			// Attach as a completed ToolPart on the last assistant message
			if lastAssistantID == "" {
				continue
			}
			if err := st.CreatePart(ctx, &store.Part{
				ID: newID(), MessageID: lastAssistantID, SessionID: sessID,
				Type: store.PartTypeText, CreatedAt: t, UpdatedAt: t,
				Data: &store.TextPartData{Text: "[tool result] " + text},
			}); err != nil {
				fmt.Fprintf(os.Stderr, "create tool part: %v\n", err)
				os.Exit(1)
			}
		}
	}

	estimatedTokens := totalChars / 4
	fmt.Printf("Replayed %d messages, ~%d chars ≈ %d tokens\n", len(convMsgs), totalChars, estimatedTokens)
	fmt.Printf("compact %s (estimated > Usable=%d)\n\n",
		map[bool]string{true: "WILL trigger", false: "might NOT trigger"}[estimatedTokens >= usable],
		usable)

	// ── compact hook ──────────────────────────────────────────────────────────
	historySrc, err := store.NewSessionHistorySource(sessID, 0, 0, 0, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "history source: %v\n", err)
		os.Exit(1)
	}
	var compactCount int
	builtinHook := historySrc.Hook()
	onCompact := store.CompactionHook(func(head []*store.Message, parts map[string][]*store.Part) {
		compactCount++
		fmt.Printf("\033[33m[compact #%d] %d messages summarised\033[0m\n", compactCount, len(head))
		builtinHook(head, parts)
	})

	// ── send real message ─────────────────────────────────────────────────────
	baseProv := openaiProv.New(
		envOr("LLM_API_KEY", ""),
		envOr("LLM_BASE_URL", ""),
		"openai", nil, 0, 0,
	)

	fmt.Println("Sending real message...")

	onEvent := func(ev llm.Event) {
		switch ev.Type {
		case llm.EventTextDelta:
			fmt.Print(ev.Text)
		case llm.EventRequestFinish:
			u := ev.Usage
			fmt.Printf("\n[tokens] in=%d out=%d\n", u.Input+u.CacheRead+u.CacheWrite, u.Output)
		case llm.EventError:
			fmt.Fprintf(os.Stderr, "\n[error] %v\n", ev.Err)
		}
	}

	hookedProv := &hookProvider{inner: baseProv, onEvent: onEvent}

	h := session.RunLoopAsync(ctx, st, session.RunInput{
		SessionID:   sessID,
		UserMsg:     "请简单总结一下我们刚才的对话内容。",
		Model:       model,
		Provider:    hookedProv,
		AgentPrompt: systemPrompt,
		MaxSteps:    *maxSteps,
		OnCompact:   onCompact,
	})
	<-h.Done

	fmt.Println()
	if h.Err != nil {
		fmt.Fprintf(os.Stderr, "[run error] %v\n", h.Err)
	}
	fmt.Printf("\nTotal compactions: %d\n", compactCount)
}
