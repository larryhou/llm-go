package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"

	"github.com/larryhou/llm-go/agent"
	"github.com/larryhou/llm-go/llm"
	"github.com/larryhou/llm-go/session"
	"github.com/larryhou/llm-go/store"
)

type webServer struct {
	client       *agent.Client
	mu           sync.Mutex
	activeHandle *session.RunHandle
}

func runWebServer(client *agent.Client) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d", ln.Addr().(*net.TCPAddr).Port)
	fmt.Printf("Web UI: %s\n\n", url)

	srv := &webServer{client: client}
	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleUI)
	mux.HandleFunc("/chat", srv.handleChat)
	mux.HandleFunc("/cancel", srv.handleCancel)
	mux.HandleFunc("/context", srv.handleContext)
	return http.Serve(ln, mux)
}

func (s *webServer) handleUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(uiHTML))
}

func (s *webServer) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Message == "" {
		http.Error(w, "message required", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)

	var sseMu sync.Mutex
	send := func(v any) {
		b, _ := json.Marshal(v)
		sseMu.Lock()
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
		sseMu.Unlock()
	}

	onEvent := func(ev llm.Event) {
		switch ev.Type {
		case llm.EventTextDelta:
			if ev.Text != "" {
				send(map[string]any{"type": "text", "delta": ev.Text})
			}
		case llm.EventToolCall:
			send(map[string]any{"type": "tool_call", "tool": ev.ToolName, "input": toolPath(ev.ToolName, ev.Input)})
		case llm.EventStepFinish:
			u := ev.Usage
			send(map[string]any{"type": "usage", "input": u.Input, "output": u.Output, "total": u.Effective()})
		case llm.EventError:
			send(map[string]any{"type": "error", "error": fmt.Sprintf("%v", ev.Err)})
		}
	}

	ctx := context.WithoutCancel(r.Context())

	s.mu.Lock()
	prev := s.activeHandle
	if prev != nil {
		prev.Cancel()
	}
	opts := s.client.DefaultRunOptions()
	if prev != nil {
		opts.WaitFor = prev.StoreDone
	}
	h := s.client.RunAsync(ctx, req.Message, opts, onEvent)
	s.activeHandle = h
	s.mu.Unlock()

	<-h.Done

	s.mu.Lock()
	if s.activeHandle == h {
		s.activeHandle = nil
	}
	s.mu.Unlock()

	if h.Err != nil && h.Err != context.Canceled {
		send(map[string]any{"type": "error", "error": h.Err.Error()})
	} else if h.Err == context.Canceled {
		send(map[string]any{"type": "cancelled"})
	}
	send(map[string]any{"type": "done"})
}

func (s *webServer) handleCancel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	s.mu.Lock()
	h := s.activeHandle
	s.mu.Unlock()
	if h != nil {
		h.Cancel()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *webServer) handleContext(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ctx := r.Context()
	msgs, err := s.client.Store.ListMessages(ctx, s.client.SessionID)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err), http.StatusInternalServerError)
		return
	}
	allParts := make(map[string][]*store.Part, len(msgs))
	for _, m := range msgs {
		if ps, err := s.client.Store.ListParts(ctx, m.ID); err == nil {
			allParts[m.ID] = ps
		}
	}
	filtered := session.FilterCompacted(msgs, allParts)
	modelMsgs, err := session.ToModelMessages(filtered, allParts)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err), http.StatusInternalServerError)
		return
	}

	type part struct {
		Type    string `json:"type"`
		Preview string `json:"preview,omitempty"`
		Chars   int    `json:"chars"`
	}
	type msg struct {
		Role    string `json:"role"`
		Content []part `json:"content"`
		Total   int    `json:"total_chars"`
	}

	const previewLen = 2000
	views := make([]msg, 0, len(modelMsgs))
	totalChars := 0
	for _, m := range modelMsgs {
		mv := msg{Role: m.Role}
		for _, p := range m.Content {
			var text string
			switch p.Type {
			case "text":
				text = p.Text
			case "tool-call":
				b, _ := json.Marshal(p.Input)
				text = fmt.Sprintf("[tool-call %s] %s", p.ToolName, b)
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
			ci := part{Type: p.Type, Chars: len(text)}
			if preview != "" {
				ci.Preview = preview
			}
			mv.Content = append(mv.Content, ci)
			mv.Total += len(text)
		}
		totalChars += mv.Total
		views = append(views, mv)
	}

	b, _ := json.MarshalIndent(map[string]any{
		"session_id":  s.client.SessionID,
		"messages":    len(modelMsgs),
		"total_chars": totalChars,
		"context":     views,
	}, "", "  ")
	_, _ = w.Write(b)
}
