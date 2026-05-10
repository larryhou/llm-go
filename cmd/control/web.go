package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"

	"github.com/larryhou/llm-go/llm"
	"github.com/larryhou/llm-go/session"
	"github.com/larryhou/llm-go/store"
	"github.com/larryhou/llm-go/store/memory"
	"github.com/larryhou/llm-go/tool"
)

// ── free port ─────────────────────────────────────────────────────────────────

// ── web server ────────────────────────────────────────────────────────────────

type webServer struct {
	app *appState
	mu  sync.Mutex // serialise concurrent /chat requests
}

// appState holds shared state initialised once in main.
type appState struct {
	cwd         string
	tools       []tool.Tool
	extraSystem []string
	model       llm.Model
	sessionStore store.Store
	sessionID    string
	prov         *replProvider
}

// ── web server ────────────────────────────────────────────────────────────────

func runWebServer(app *appState) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	url := fmt.Sprintf("http://127.0.0.1:%d", addr.Port)

	srv := &webServer{app: app}
	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleUI)
	mux.HandleFunc("/chat", srv.handleChat)

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
		Message   string `json:"message"`
		SessionID string `json:"session_id"`
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

	// Each web request gets its own isolated session store + session.
	sessStore := memory.New()
	ctx := r.Context()
	sessID := req.SessionID
	if sessID == "" {
		sessID = fmt.Sprintf("web-%d", timeNano())
	}
	if err := sessStore.CreateSession(ctx, &store.Session{
		ID:    sessID,
		Model: s.app.model.ProviderID + "/" + s.app.model.ID,
	}); err != nil {
		sendEvent(map[string]any{"type": "error", "error": err.Error()})
		return
	}

	// Per-request event channel.
	evCh := make(chan llm.Event, 128)
	prov := &replProvider{inner: s.app.prov.inner, out: evCh}

	// Drain events → SSE in a goroutine while RunLoop blocks.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range evCh {
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
	_, runErr := session.RunLoop(ctx, sessStore, session.RunInput{
		SessionID:   sessID,
		UserMsg:     req.Message,
		Model:       s.app.model,
		Provider:    prov,
		Tools:       s.app.tools,
		ExtraSystem: s.app.extraSystem,
		MaxSteps:    20,
	})
	s.mu.Unlock()

	close(evCh)
	<-done

	if runErr != nil {
		sendEvent(map[string]any{"type": "error", "error": runErr.Error()})
	}
	sendEvent(map[string]any{"type": "done", "session_id": sessID})
}
