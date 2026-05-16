package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"

	"github.com/larryhou/llm-go/config"
	"github.com/larryhou/llm-go/llm"
	"github.com/larryhou/llm-go/session"
	"github.com/larryhou/llm-go/store"
	"github.com/larryhou/llm-go/store/memory"
	"github.com/larryhou/llm-go/tool"
)

// ── web server ────────────────────────────────────────────────────────────────

type webServer struct {
	app          *appState
	mu           sync.Mutex // guards activeHandle
	activeHandle *session.RunHandle
	sessStore    store.Store
	sessID       string
}

// appState holds shared state initialised once in main.
type appState struct {
	cwd         string
	tools       []tool.Tool
	extraSystem []string
	model       llm.Model
	prov        llm.Provider // RecordProvider(real) in -debug, real otherwise
	cfg         *config.Info
}

// ── web server ────────────────────────────────────────────────────────────────

func runWebServer(app *appState) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	url := fmt.Sprintf("http://127.0.0.1:%d", addr.Port)

	sessID := fmt.Sprintf("web-%d", addr.Port)
	sessStore := memory.New()
	if err := sessStore.CreateSession(context.Background(), &store.Session{
		ID:    sessID,
		Model: app.model.ProviderID + "/" + app.model.ID,
	}); err != nil {
		return fmt.Errorf("create session: %w", err)
	}

	srv := &webServer{app: app, sessStore: sessStore, sessID: sessID}
	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleUI)
	mux.HandleFunc("/chat", srv.handleChat)
	mux.HandleFunc("/cancel", srv.handleCancel)
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
	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)

	// sseMu serialises all writes to w. http.ResponseWriter is not
	// goroutine-safe; concurrent tool-execution goroutines can call sendEvent
	// simultaneously, which corrupts chunked-encoding framing.
	var sseMu sync.Mutex
	sendEvent := func(payload any) {
		b, _ := json.Marshal(payload)
		sseMu.Lock()
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
		sseMu.Unlock()
	}

	// Wrap provider to forward events to SSE.
	turnProv := &hookProvider{
		inner: s.app.prov,
		onEvent: func(ev llm.Event) {
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
				u := ev.Usage
				sendEvent(map[string]any{
					"type":   "usage",
					"input":  u.Input,
					"output": u.Output,
					"total":  u.Effective(),
				})
			case llm.EventError:
				sendEvent(map[string]any{"type": "error", "error": fmt.Sprintf("%v", ev.Err)})
			}
		},
	}

	// Detach from the HTTP request context so that RunLoop is not cancelled
	// when the SSE client disconnects mid-stream.
	ctx := context.WithoutCancel(r.Context())

	// Cancel the previous turn and register the new handle atomically under
	// s.mu. Holding the lock across Cancel+RunLoopAsync+activeHandle assignment
	// ensures a third concurrent request cannot observe a stale prev and race
	// to also register itself against the same old StoreDone.
	s.mu.Lock()
	prev := s.activeHandle
	if prev != nil {
		prev.Cancel()
	}
	var waitFor <-chan struct{}
	if prev != nil {
		waitFor = prev.StoreDone
	}
	h := session.RunLoopAsync(ctx, s.sessStore, session.RunInput{
		SessionID:   s.sessID,
		UserMsg:     req.Message,
		Model:       s.app.model,
		Provider:    turnProv,
		Tools:       s.app.tools,
		ExtraSystem: s.app.extraSystem,
		MaxSteps:    20,
		Config:      s.app.cfg,
		WaitFor:     waitFor,
	})
	s.activeHandle = h
	s.mu.Unlock()

	// Wait for the async turn to finish.
	<-h.Done

	s.mu.Lock()
	if s.activeHandle == h {
		s.activeHandle = nil
	}
	s.mu.Unlock()

	runErrStr := ""
	if h.Err != nil && h.Err != context.Canceled {
		runErrStr = h.Err.Error()
		sendEvent(map[string]any{"type": "error", "error": runErrStr})
	} else if h.Err == context.Canceled {
		sendEvent(map[string]any{"type": "cancelled"})
	}
	_ = runErrStr
	sendEvent(map[string]any{"type": "done"})
}

// ── /cancel — cancel the in-flight turn ──────────────────────────────────────

func (s *webServer) handleCancel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	s.mu.Lock()
	h := s.activeHandle
	s.mu.Unlock()
	if h == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.Cancel()
	w.WriteHeader(http.StatusNoContent)
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

	const previewLen = 2000
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

