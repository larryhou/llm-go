package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/larryhou/llm-go/config"
	"github.com/larryhou/llm-go/llm"
	"github.com/larryhou/llm-go/session"
	"github.com/larryhou/llm-go/store"
	"github.com/larryhou/llm-go/store/memory"
	"github.com/larryhou/llm-go/tool"
)

// ── web server ────────────────────────────────────────────────────────────────

type webServer struct {
	app       *appState
	mu        sync.Mutex // serialise concurrent /chat requests
	sessStore store.Store
	sessID    string
	debugDir  string     // non-empty when -debug is set
}

// appState holds shared state initialised once in main.
type appState struct {
	cwd         string
	tools       []tool.Tool
	extraSystem []string
	model       llm.Model
	prov        *replProvider
	cfg         *config.Info
	debug       bool
}

// ── web server ────────────────────────────────────────────────────────────────

func runWebServer(app *appState) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	url := fmt.Sprintf("http://127.0.0.1:%d", addr.Port)

	// Port is unique per process — use it as the session seed.
	sessID := fmt.Sprintf("web-%d", addr.Port)
	sessStore := memory.New()
	if err := sessStore.CreateSession(context.Background(), &store.Session{
		ID:    sessID,
		Model: app.model.ProviderID + "/" + app.model.ID,
	}); err != nil {
		return fmt.Errorf("create session: %w", err)
	}

	// Create debug directory if -debug is set.
	var debugDir string
	if app.debug {
		debugDir = sessID
		if err := os.MkdirAll(debugDir, 0755); err != nil {
			return fmt.Errorf("create debug dir: %w", err)
		}
		fmt.Printf("Debug recording: %s/\n", debugDir)
	}

	srv := &webServer{app: app, sessStore: sessStore, sessID: sessID, debugDir: debugDir}
	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleUI)
	mux.HandleFunc("/chat", srv.handleChat)
	mux.HandleFunc("/context", srv.handleContext)

	fmt.Printf("Web UI: %s\n\n", url)
	return http.Serve(ln, mux)
}

// ── / — serve embedded UI ─────────────────────────────────────────────────────

func (s *webServer) handleUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(uiHTML))
}

// ── /chat — SSE endpoint ──────────────────────────────────────────────────────

func (s *webServer) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Message == "" {
		http.Error(w, "message required", http.StatusBadRequest)
		return
	}

	// SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	sendEvent := func(payload any) {
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	// Per-request event channel.
	evCh := make(chan llm.Event, 128)
	prov := &replProvider{inner: s.app.prov.inner, out: evCh}

	// Debug recording: collect all raw LLM events for this turn.
	type eventRecord struct {
		TS           int64  `json:"ts_ms"`
		Type         string `json:"type"`
		Text         string `json:"text,omitempty"`
		ToolName     string `json:"tool_name,omitempty"`
		ToolID       string `json:"tool_id,omitempty"`
		Input        any    `json:"input,omitempty"`
		Usage        any    `json:"usage,omitempty"`
		FinishReason string `json:"finish_reason,omitempty"`
		Error        string `json:"error,omitempty"`
	}
	type turnRecord struct {
		SessionID string        `json:"session_id"`
		TS        string        `json:"ts"`
		UserMsg   string        `json:"user_message"`
		Events    []eventRecord `json:"events"`
		RunError  string        `json:"run_error,omitempty"`
	}
	rec := &turnRecord{
		SessionID: s.sessID,
		TS:        time.Now().Format(time.RFC3339),
		UserMsg:   req.Message,
	}

	// Drain events → SSE in a goroutine while RunLoop blocks.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range evCh {
			// Build event record for debug log.
			if s.debugDir != "" {
				er := eventRecord{TS: time.Now().UnixMilli(), Type: string(ev.Type)}
				switch ev.Type {
				case llm.EventTextDelta, llm.EventReasoningDelta:
					er.Text = ev.Text
				case llm.EventToolInputStart, llm.EventToolCall:
					er.ToolName = ev.ToolName
					er.ToolID = ev.ToolCallID
					er.Input = ev.Input
			case llm.EventStepFinish:
				er.Usage = map[string]int{
					"input":  ev.Usage.Input,
					"output": ev.Usage.Output,
					"total":  ev.Usage.Effective(),
				}
				er.FinishReason = string(ev.FinishReason)
				case llm.EventError:
					if ev.Err != nil {
						er.Error = ev.Err.Error()
					}
				}
				rec.Events = append(rec.Events, er)
			}

			switch ev.Type {
			case llm.EventTextDelta:
				if ev.Text != "" {
					sendEvent(map[string]any{"type": "text", "delta": ev.Text})
				}
			case llm.EventToolCall:
				sendEvent(map[string]any{
					"type":  "tool_call",
					"tool":  ev.ToolName,
					"input": toolPath(ev.ToolName, ev.Input),
				})
			case llm.EventStepFinish:
				sendEvent(map[string]any{
					"type":   "usage",
					"input":  ev.Usage.Input,
					"output": ev.Usage.Output,
					"total":  ev.Usage.Effective(),
				})
			case llm.EventError:
				sendEvent(map[string]any{"type": "error", "error": fmt.Sprintf("%v", ev.Err)})
			}
		}
	}()

	s.mu.Lock()
	_, runErr := session.RunLoop(r.Context(), s.sessStore, session.RunInput{
		SessionID:   s.sessID,
		UserMsg:     req.Message,
		Model:       s.app.model,
		Provider:    prov,
		Tools:       s.app.tools,
		ExtraSystem: s.app.extraSystem,
		MaxSteps:    20,
		Config:      s.app.cfg,
	})
	s.mu.Unlock()

	close(evCh)
	<-done

	if runErr != nil {
		rec.RunError = runErr.Error()
		sendEvent(map[string]any{"type": "error", "error": runErr.Error()})
	}
	sendEvent(map[string]any{"type": "done"})

	// Write debug file after the turn is fully done.
	if s.debugDir != "" {
		fname := fmt.Sprintf("%s/chat-%d.json", s.debugDir, time.Now().UnixMilli())
		if b, err := json.MarshalIndent(rec, "", "  "); err == nil {
			_ = os.WriteFile(fname, b, 0644)
		}
	}
}

// ── /context — inspect current session context window ────────────────────────

func (s *webServer) handleContext(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ctx := r.Context()

	msgs, err := s.sessStore.ListMessages(ctx, s.sessID)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}

	allParts := make(map[string][]*store.Part, len(msgs))
	for _, m := range msgs {
		ps, err := s.sessStore.ListParts(ctx, m.ID)
		if err != nil {
			continue
		}
		allParts[m.ID] = ps
	}

	filtered := session.FilterCompacted(msgs, allParts)
	modelMsgs, err := session.ToModelMessages(filtered, allParts)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}

	type contentItem struct {
		Type    string `json:"type"`
		Preview string `json:"preview,omitempty"`
		Chars   int    `json:"chars"`
	}
	type msgView struct {
		Role    string        `json:"role"`
		Content []contentItem `json:"content"`
		Total   int           `json:"total_chars"`
	}

	const previewLen = 200
	views := make([]msgView, 0, len(modelMsgs))
	totalChars := 0
	for _, m := range modelMsgs {
		mv := msgView{Role: m.Role}
		for _, p := range m.Content {
			var text string
			switch p.Type {
			case "text":
				text = p.Text
			case "tool-call":
				b, _ := json.Marshal(p.Input)
				text = fmt.Sprintf("[tool-call %s] %s", p.ToolName, string(b))
			case "tool-result":
				if p.Result != nil {
					text = fmt.Sprintf("[tool-result %s] %v", p.ToolName, p.Result.Value)
				}
			case "reasoning":
				text = "[reasoning] " + p.Text
			default:
				text = fmt.Sprintf("[%s]", p.Type)
			}
			preview := text
			if len(preview) > previewLen {
				preview = preview[:previewLen] + "…"
			}
			ci := contentItem{Type: p.Type, Chars: len(text)}
			if preview != "" {
				ci.Preview = preview
			}
			mv.Content = append(mv.Content, ci)
			mv.Total += len(text)
		}
		totalChars += mv.Total
		views = append(views, mv)
	}

	resp := map[string]any{
		"session_id":   s.sessID,
		"messages":     len(modelMsgs),
		"total_chars":  totalChars,
		"context":      views,
	}
	b, _ := json.MarshalIndent(resp, "", "  ")
	_, _ = w.Write(b)
}

