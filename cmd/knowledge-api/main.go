// Command knowledge-api starts an HTTP server that:
//
//  1. Scans a skills directory and builds an in-memory Bleve index
//  2. Registers the index as a knowledge.Source via the knowledge.Manager
//  3. Exposes direct search/fetch endpoints for inspection
//  4. Exposes a /chat endpoint that accepts user input, runs a real LLM
//     session (with knowledge_search and knowledge_fetch tools), and
//     streams the assistant reply back to the caller
//
// # Usage
//
//	go run ./cmd/knowledge-api \
//	    -skills /path/to/.opencode/skills \
//	    -addr   127.0.0.1:7700 \
//	    -model  claude-sonnet-4.6
//
// # Endpoints
//
//	GET  /health
//	  Response: {"status":"ok","doc_count":2,"session_count":N}
//
//	POST /search
//	  Request:  {"query":"...", "max_results":5}
//	  Response: {"results":[{"ref_id":"...","title":"...","score":0.9,"snippet":"..."},...]}
//
//	GET  /search?q=...&max_results=3
//
//	POST /fetch
//	  Request:  {"ref_id":"skills:knowledge/SKILL.md"}
//	  Response: {"result":{"ref_id":"...","title":"...","content":"..."}}
//
//	GET  /fetch?ref_id=skills:knowledge/SKILL.md
//
//	POST /chat
//	  Request:  {"message":"...", "session_id":"optional-id"}
//	  Response: text/event-stream
//	    data: {"type":"text","delta":"..."}
//	    data: {"type":"tool_call","tool":"knowledge_search","input":{...}}
//	    data: {"type":"tool_result","tool":"knowledge_search","output":"..."}
//	    data: {"type":"done","session_id":"...","finish_reason":"stop"}
//	    data: {"type":"error","error":"..."}
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	bleve "github.com/blevesearch/bleve/v2"

	"github.com/larryhou/llm-go/auth"
	"github.com/larryhou/llm-go/config"
	"github.com/larryhou/llm-go/knowledge"
	blevesource "github.com/larryhou/llm-go/knowledge/source/bleve"
	"github.com/larryhou/llm-go/llm"
	anthropicProv "github.com/larryhou/llm-go/provider/anthropic"
	openaiProv "github.com/larryhou/llm-go/provider/openai"
	providerPkg "github.com/larryhou/llm-go/provider"
	"github.com/larryhou/llm-go/session"
	"github.com/larryhou/llm-go/store"
	"github.com/larryhou/llm-go/store/memory"
	"github.com/larryhou/llm-go/tool"
)

// ── config ────────────────────────────────────────────────────────────────────

type serverConfig struct {
	skillsDir  string
	addr       string
	sourceID   string
	snippetMax int
	contentMax int
	// LLM
	provider string
	baseURL  string
	apiKey   string
	modelID  string
	maxSteps int
}

func main() {
	cfg := serverConfig{}
	flag.StringVar(&cfg.skillsDir, "skills", "", "skills directory to index (required)")
	flag.StringVar(&cfg.addr, "addr", "127.0.0.1:7700", "listen address")
	flag.StringVar(&cfg.sourceID, "source", "skills", "source ID prefix for RefIDs")
	flag.IntVar(&cfg.snippetMax, "snippet-max", 400, "max chars per snippet")
	flag.IntVar(&cfg.contentMax, "content-max", 8000, "max chars for fetched content")
	flag.StringVar(&cfg.provider, "provider", envOr("TIMI_PROVIDER", "anthropic"), "LLM provider: anthropic or openai")
	flag.StringVar(&cfg.baseURL, "llm-url", envOr("TIMI_BASE_URL", ""), "LLM base URL (default depends on provider)")
	flag.StringVar(&cfg.apiKey, "llm-key", envOr("TIMI_API_KEY", "sk-zzz6FtyLMyuobNNOukwgobP0l1F3TjMO"), "LLM API key")
	flag.StringVar(&cfg.modelID, "model", envOr("TIMI_MODEL", "claude-sonnet-4.6"), "LLM model ID")
	flag.IntVar(&cfg.maxSteps, "max-steps", 10, "max LLM steps per turn")
	flag.Parse()

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if cfg.baseURL == "" {
		if cfg.provider == "openai" {
			cfg.baseURL = "http://192.168.3.119:8080/timi-claude/v1"
		} else {
			cfg.baseURL = "http://192.168.3.119:8080/claude"
		}
	}

	if cfg.skillsDir == "" {
		flag.Usage()
		log.Fatal("flag -skills is required")
	}

	// Build provider registry.
	registry := providerPkg.NewRegistry()
	registry.RegisterFactory(anthropicProv.ProviderID, anthropicProv.Factory)
	registry.RegisterFactory(openaiProv.ProviderID, openaiProv.Factory)
	registry.RegisterFactory("timi", func(provCfg *config.ProviderInfo, a *auth.Store) (llm.Provider, error) {
		return openaiProv.NewFromConfig("timi", nil, provCfg, a)
	})

	// Load file config + auth; CLI flags override.
	fileCfg, _ := config.Load()
	authStore, _ := auth.Load()

	provCfgMap := map[string]*config.ProviderInfo{}
	if fileCfg != nil {
		for k, v := range fileCfg.Provider {
			provCfgMap[k] = v
		}
	}
	if cfg.apiKey != "" || cfg.baseURL != "" {
		cliProvID := cfg.provider
		if cliProvID == "openai" {
			cliProvID = "timi"
		}
		existing := provCfgMap[cliProvID]
		override := &config.ProviderInfo{}
		if existing != nil {
			*override = *existing
		}
		if override.Options == nil {
			override.Options = &config.ProviderOptions{}
		}
		if cfg.apiKey != "" {
			override.Options.APIKey = cfg.apiKey
		}
		if cfg.baseURL != "" {
			if cfg.provider == "anthropic" {
				override.API = strings.TrimSuffix(cfg.baseURL, "/v1")
			} else {
				override.API = cfg.baseURL
			}
		}
		provCfgMap[cliProvID] = override
	}

	// Build in-memory Bleve index from skills directory.
	tool.StartCleanup(rootCtx)
	idx, count, err := buildMemoryIndex(cfg.skillsDir)
	if err != nil {
		log.Fatalf("build index: %v", err)
	}
	log.Printf("in-memory index ready: %d document(s) from %s", count, cfg.skillsDir)

	// Build knowledge manager.
	km := knowledge.NewManager(knowledge.ManagerConfig{
		SourceTimeout:       10 * time.Second,
		MaxResults:          10,
		SnippetMaxChars:     cfg.snippetMax,
		ContentMaxChars:     cfg.contentMax,
		AllowPartialFailure: false,
	})
	km.Register(blevesource.New(idx, cfg.sourceID, 0, &blevesource.Config{
		TitleField:   "title",
		ContentField: "content",
	}))

	// Session store: one in-memory store shared across all /chat sessions.
	sessionStore := memory.New()
	var sessionCount atomic.Int64

	srv := &server{
		cfg:          cfg,
		idx:          idx,
		km:           km,
		sessionStore: sessionStore,
		sessionCount: &sessionCount,
		registry:     registry,
		authStore:    authStore,
		provCfgMap:   provCfgMap,
		testTools:    buildTestTools(),
		sessions:     make(map[string]*chatSession),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", srv.handleHealth)
	mux.HandleFunc("/search", srv.handleSearch)
	mux.HandleFunc("/fetch", srv.handleFetch)
	mux.HandleFunc("/chat", srv.handleChat)
	mux.HandleFunc("/sessions/", srv.handleSession)
	kmTools := km.Tools()
	mux.HandleFunc("/schema/search", handleSchema(kmTools[0]))
	mux.HandleFunc("/schema/fetch", handleSchema(kmTools[1]))

	log.Printf("knowledge-api listening on %s", cfg.addr)
	if err := http.ListenAndServe(cfg.addr, logMiddleware(mux)); err != nil {
		log.Fatal(err)
	}
}

// ── server ────────────────────────────────────────────────────────────────────

type server struct {
	cfg          serverConfig
	idx          bleve.Index
	km           *knowledge.Manager
	sessionStore store.Store
	sessionCount *atomic.Int64

	// provider registry and resolved config/auth — used in handleChat.
	registry   *providerPkg.Registry
	authStore  *auth.Store
	provCfgMap map[string]*config.ProviderInfo

	// active sessions: sessionID -> *chatSession
	mu       sync.Mutex
	sessions map[string]*chatSession

	// test tools (registered once at startup)
	testTools []tool.Tool
}

type chatSession struct {
	id         string
	store      store.Store // per-session isolated store
	historySrc *knowledge.SessionHistorySource
	km         *knowledge.Manager // per-session manager: skills source + history source
	hook       knowledge.CompactionHook // cached hook from historySrc
	resetTool  tool.Tool                // cached session_reset tool
}

// ── /health ───────────────────────────────────────────────────────────────────

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	count, _ := s.idx.DocCount()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"doc_count":     count,
		"session_count": s.sessionCount.Load(),
	})
}

// ── /search ───────────────────────────────────────────────────────────────────

func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	var query string
	var maxResults int

	if r.Method == http.MethodGet {
		query = r.URL.Query().Get("q")
		maxResults, _ = strconv.Atoi(r.URL.Query().Get("max_results"))
	} else {
		var req struct {
			Query      string         `json:"query"`
			MaxResults int            `json:"max_results"`
			Filters    map[string]any `json:"filters"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		query = req.Query
		maxResults = req.MaxResults
	}
	if query == "" {
		writeErr(w, http.StatusBadRequest, "query is required")
		return
	}
	if maxResults <= 0 {
		maxResults = 5
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	results, err := s.km.Search(ctx, knowledge.Query{
		Type:       knowledge.QueryTypeSearch,
		Input:      query,
		MaxResults: maxResults,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": toResultsJSON(results)})
}

// ── /fetch ────────────────────────────────────────────────────────────────────

func (s *server) handleFetch(w http.ResponseWriter, r *http.Request) {
	var refID string
	if r.Method == http.MethodGet {
		refID = r.URL.Query().Get("ref_id")
	} else {
		var req struct {
			RefID string `json:"ref_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		refID = req.RefID
	}
	if refID == "" {
		writeErr(w, http.StatusBadRequest, "ref_id is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	results, err := s.km.Fetch(ctx, knowledge.Query{Type: knowledge.QueryTypeFetch, Input: refID})
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if len(results) == 0 {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("ref_id %q not found", refID))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": toResultJSON(results[0])})
}

// ── /chat ─────────────────────────────────────────────────────────────────────

// handleChat accepts a user message and streams LLM events (SSE).
//
// Request body:
//
//	{"message": "...", "session_id": "optional-existing-id"}
//
// Response: text/event-stream, each line is:
//
//	data: <json>\n\n
//
// Event types: text | tool_call | tool_result | done | error
func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}

	var req struct {
		Message            string   `json:"message"`
		SessionID          string   `json:"session_id"`
		ContextLimit       int      `json:"context_limit"`        // override model context window (for compaction testing)
		CompactionReserved int      `json:"compaction_reserved"`  // override reserved tokens (default: min(20000, maxOutput))
		MaxSteps           int      `json:"max_steps"`            // override server default
		Tools              []string `json:"tools"`                // subset of tools to enable; empty = all
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		writeErr(w, http.StatusBadRequest, "message is required")
		return
	}

	// Resolve session ID early so it can be sent as a response header
	// before WriteHeader is called (headers are frozen after WriteHeader).
	sessID := req.SessionID
	if sessID == "" {
		sessID = fmt.Sprintf("sess-%d", time.Now().UnixNano())
	}
	// Detach from the HTTP request context so that RunLoop (and compaction) are
	// not cancelled when the SSE client disconnects mid-stream. This allows
	// multi-step agentic turns and compaction to complete even when curl or a
	// browser closes the connection after reading partial output.
	// context.WithoutCancel inherits request-scoped values but ignores cancellation.
	ctx := context.WithoutCancel(r.Context())

	// Set SSE headers first so all error responses are delivered as SSE events.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Session-ID", sessID)
	w.WriteHeader(http.StatusOK)
	flusher, canFlush := w.(http.Flusher)

	sendEvent := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if canFlush {
			flusher.Flush()
		}
	}

	// Get or create per-session state (history source + knowledge manager).
	// Created once per session; reused across turns.
	s.mu.Lock()
	sess, exists := s.sessions[sessID]
	if !exists {
		historySrc, histErr := knowledge.NewSessionHistorySource(sessID, knowledge.DefaultMaxCompactions)
		if histErr != nil {
			s.mu.Unlock()
			sendEvent(map[string]any{"type": "error", "error": "create history source: " + histErr.Error()})
			return
		}
		// Per-session knowledge manager: skills index (priority 1) + session
		// history (priority 0, higher). AllowPartialFailure matches s.km so that
		// /search, /fetch, and /chat all behave consistently on source errors.
		sessKM := knowledge.NewManager(knowledge.ManagerConfig{
			SourceTimeout:       10 * time.Second,
			MaxResults:          10,
			SnippetMaxChars:     s.cfg.snippetMax,
			ContentMaxChars:     s.cfg.contentMax,
			AllowPartialFailure: false,
		})
		sessKM.Register(blevesource.New(s.idx, s.cfg.sourceID, 1, &blevesource.Config{
			TitleField:   "title",
			ContentField: "content",
		}))
		sessKM.Register(historySrc)

		// resetFn is called under s.mu to prevent concurrent requests for the
		// same session from interleaving between DeleteSession and CreateSession.
		st := s.sessionStore // capture for closure
		resetFn := func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			if err := st.DeleteSession(ctx, sessID); err != nil {
				return fmt.Errorf("delete session: %w", err)
			}
			if err := st.CreateSession(ctx, &store.Session{ID: sessID}); err != nil {
				return fmt.Errorf("recreate session: %w", err)
			}
			if err := historySrc.Reset(); err != nil {
				// Non-fatal: store is clean; index failure leaves index nil
				// until next successful reset. Log but don't fail.
				log.Printf("[session_reset] history index reset failed: %v", err)
			}
			return nil
		}

		sess = &chatSession{
			id:         sessID,
			store:      s.sessionStore,
			historySrc: historySrc,
			km:         sessKM,
			hook:       historySrc.Hook(),
			resetTool:  session.NewResetTool(resetFn),
		}
		s.sessions[sessID] = sess
	}
	s.mu.Unlock()

	if _, err := s.sessionStore.GetSession(ctx, sessID); err != nil {
		modelPrefix := "anthropic"
		if s.cfg.provider == "openai" {
			modelPrefix = "timi"
		}
		_ = s.sessionStore.CreateSession(ctx, &store.Session{
			ID:    sessID,
			Model: modelPrefix + "/" + s.cfg.modelID,
		})
		s.sessionCount.Add(1)
	}

	// Build tool list: per-session knowledge tools (skills + history) + test tools
	// + session_reset. Allocate a fresh slice to avoid aliasing km.Tools()'s
	// backing array across requests.
	kmTools := sess.km.Tools()
	allTools := make([]tool.Tool, 0, len(kmTools)+len(s.testTools)+1)
	allTools = append(allTools, kmTools...)
	allTools = append(allTools, s.testTools...)
	allTools = append(allTools, sess.resetTool)
	activeTools := allTools
	if len(req.Tools) > 0 {
		allowed := make(map[string]bool, len(req.Tools))
		for _, name := range req.Tools {
			allowed[name] = true
		}
		activeTools = activeTools[:0:0]
		for _, t := range allTools {
			if allowed[t.Name()] {
				activeTools = append(activeTools, t)
			}
		}
	}

	// Wrap each tool to emit tool_result SSE events after execution.
	// (The provider stream only carries tool_call events; results are
	//  produced asynchronously by the processor, so we intercept here.)
	wrappedTools := make([]tool.Tool, len(activeTools))
	for i, t := range activeTools {
		wrappedTools[i] = &sseToolWrapper{inner: t, send: sendEvent}
	}
	activeTools = wrappedTools

	// Wrap the provider to intercept streaming events and forward to SSE.
	providerID := s.cfg.provider
	if providerID == "openai" {
		providerID = "timi"
	}
	innerProv, buildErr := s.registry.BuildProvider(providerID, s.provCfgMap[providerID], s.authStore)
	if buildErr != nil {
		sendEvent(map[string]any{"type": "error", "error": buildErr.Error()})
		return
	}
	summaryProv := innerProv
	wrappedProv := &sseProvider{inner: innerProv, send: sendEvent}

	contextLimit := 128_000
	if req.ContextLimit > 0 {
		contextLimit = req.ContextLimit
	}
	maxSteps := s.cfg.maxSteps
	if req.MaxSteps > 0 {
		maxSteps = req.MaxSteps
	}

	// Build per-request config — used only for compaction tuning.
	var sessionCfg *config.Info
	if req.CompactionReserved > 0 {
		reserved := req.CompactionReserved
		pruneEnabled := true
		sessionCfg = &config.Info{
			Compaction: &config.CompactionConfig{
				Reserved: &reserved,
				Prune:    &pruneEnabled,
			},
		}
	}

	model := llm.Model{
		ID:         s.cfg.modelID,
		ProviderID: providerID,
		APIID:      s.cfg.modelID,
		Limit:      llm.ModelLimit{Context: contextLimit, Output: 4096},
	}

	_, err := session.RunLoop(ctx, s.sessionStore, session.RunInput{
		SessionID: sessID,
		UserMsg:   req.Message,
		Model:     model,
		Provider:  wrappedProv,
		// Use a plain (unwrapped) provider for compaction summary so SSE
		// middleware does not forward internal summary events to the client.
		SummaryProvider: summaryProv,
		Tools:           activeTools,
		ExtraSystem: []string{
			"You are a helpful assistant with access to a knowledge base of skill documentation and various test tools.",
			"When asked about skills, architecture, or development guides, use knowledge_search to find relevant documents.",
			"Use knowledge_fetch to retrieve the full content of a document when you need details.",
		},
		DisableProviderPrompt: true,
		MaxSteps:              maxSteps,
		Config:                sessionCfg,
		OnCompact:             sess.hook,
	})
	if err != nil {
		sendEvent(map[string]any{"type": "error", "error": err.Error()})
		return
	}

	sendEvent(map[string]any{
		"type":       "done",
		"session_id": sessID,
	})
}

// ── /sessions/{id}/messages — session inspection endpoint ────────────────────

func (s *server) handleSession(w http.ResponseWriter, r *http.Request) {
	// Pattern: /sessions/{id}/messages
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/sessions/"), "/")
	if len(parts) < 2 || parts[1] != "messages" {
		writeErr(w, http.StatusNotFound, "use /sessions/{id}/messages")
		return
	}
	sessID := parts[0]
	ctx := r.Context()

	msgs, err := s.sessionStore.ListMessages(ctx, sessID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}

	type partSummary struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Summary string `json:"summary,omitempty"`
	}
	type msgSummary struct {
		ID      string        `json:"id"`
		Role    string        `json:"role"`
		Summary bool          `json:"summary,omitempty"`
		Parts   []partSummary `json:"parts"`
	}

	out := make([]msgSummary, 0, len(msgs))
	for _, m := range msgs {
		ps, _ := s.sessionStore.ListParts(ctx, m.ID)
		pss := make([]partSummary, 0, len(ps))
		for _, p := range ps {
			ps2 := partSummary{ID: p.ID, Type: p.Type}
			switch d := p.Data.(type) {
			case *store.TextPartData:
				if len(d.Text) > 120 {
					ps2.Summary = d.Text[:120] + "…"
				} else {
					ps2.Summary = d.Text
				}
			case *store.ToolPartData:
				ps2.Summary = fmt.Sprintf("tool=%s status=%s", d.Tool, d.Status)
			case *store.StepFinishData:
				ps2.Summary = fmt.Sprintf("finish=%s input=%d output=%d", d.FinishReason, d.Usage.Input, d.Usage.Output)
			case *store.CompactionPartData:
				ps2.Summary = "compaction boundary"
			case *store.RecentContextPartData:
				if len(d.Excerpt) > 120 {
					ps2.Summary = "recent-context: " + d.Excerpt[:120] + "…"
				} else {
					ps2.Summary = "recent-context: " + d.Excerpt
				}
			}
			pss = append(pss, ps2)
		}
		out = append(out, msgSummary{
			ID:      m.ID,
			Role:    m.Role,
			Summary: m.Summary,
			Parts:   pss,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"session_id":    sessID,
		"message_count": len(out),
		"messages":      out,
	})
}

// ── sseProvider — wraps a real Provider and forwards events to SSE ────────────

type sseProvider struct {
	inner llm.Provider
	send  func(any)
}

func (p *sseProvider) ID() string { return p.inner.ID() }

func (p *sseProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	inner, err := p.inner.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	out := make(chan llm.Event, 64)
	go func() {
		defer close(out)
		for ev := range inner {
			// Forward selected events to SSE client.
			switch ev.Type {
			case llm.EventTextDelta:
				if ev.Text != "" {
					p.send(map[string]any{"type": "text", "delta": ev.Text})
				}
			case llm.EventToolCall:
				p.send(map[string]any{
					"type":  "tool_call",
					"tool":  ev.ToolName,
					"input": ev.Input,
				})
			}
			out <- ev
		}
	}()
	return out, nil
}

// ── sseToolWrapper — wraps a Tool and emits tool_result SSE events ────────────

// sseToolWrapper wraps a tool.Tool and forwards execution results to the SSE
// client. This is necessary because tool results are produced asynchronously by
// the processor, not through the provider event stream, so they cannot be
// intercepted in sseProvider.
type sseToolWrapper struct {
	inner tool.Tool
	send  func(any)
}

func (w *sseToolWrapper) Name() string            { return w.inner.Name() }
func (w *sseToolWrapper) Description() string     { return w.inner.Description() }
func (w *sseToolWrapper) InputSchema() map[string]any { return w.inner.InputSchema() }

func (w *sseToolWrapper) Execute(ctx context.Context, input map[string]any) (tool.Result, error) {
	result, err := w.inner.Execute(ctx, input)
	if err != nil {
		if tf, ok := tool.IsToolFailure(err); ok {
			w.send(map[string]any{
				"type":   "tool_result",
				"tool":   w.inner.Name(),
				"output": "[tool failure] " + tf.Message,
				"error":  true,
			})
		}
		return result, err
	}
	// Truncate long outputs in the SSE event for readability.
	out := result.Output
	if len(out) > 500 {
		out = out[:500] + "…"
	}
	w.send(map[string]any{
		"type":   "tool_result",
		"tool":   w.inner.Name(),
		"output": out,
	})
	return result, nil
}

// ── test tools ────────────────────────────────────────────────────────────────

// buildTestTools creates a set of purpose-built tools for live feature testing:
//
//   - calc:          arithmetic (exercises normal tool execution + multi-turn)
//   - slow_calc:     same but sleeps 2s (exercises async tool + concurrent dispatch)
//   - counter:       stateful incrementing counter (multi-turn state accumulation)
//   - tool_failure:  always returns a ToolFailure (exercises recoverable error path)
//   - doom_bait:     echoes its input unchanged (LLM tends to call it repeatedly → doom-loop)
func buildTestTools() []tool.Tool {
	var mu sync.Mutex
	counters := map[string]int{}

	return []tool.Tool{
		&simpleTool{
			name:        "calc",
			description: "Evaluate a simple arithmetic expression. Supported operators: + - * /. Example: {\"expr\": \"123 * 456\"}",
			schema: map[string]any{
				"type":     "object",
				"required": []string{"expr"},
				"properties": map[string]any{
					"expr": map[string]any{"type": "string", "description": "arithmetic expression like '2 + 3 * 4'"},
				},
			},
			fn: func(_ context.Context, input map[string]any) (tool.Result, error) {
				expr, _ := input["expr"].(string)
				result, err := evalExpr(expr)
				if err != nil {
					return tool.Result{}, tool.Fail("invalid expression: " + err.Error())
				}
				return tool.Result{Output: fmt.Sprintf("%g", result), Title: "calc"}, nil
			},
		},
		&simpleTool{
			name:        "slow_calc",
			description: "Like calc but deliberately slow (2-second delay). Use to test concurrent/async tool execution.",
			schema: map[string]any{
				"type":     "object",
				"required": []string{"expr"},
				"properties": map[string]any{
					"expr": map[string]any{"type": "string"},
				},
			},
			fn: func(ctx context.Context, input map[string]any) (tool.Result, error) {
				select {
				case <-time.After(2 * time.Second):
				case <-ctx.Done():
					return tool.Result{}, ctx.Err()
				}
				expr, _ := input["expr"].(string)
				result, err := evalExpr(expr)
				if err != nil {
					return tool.Result{}, tool.Fail("invalid expression: " + err.Error())
				}
				return tool.Result{Output: fmt.Sprintf("%g (slow)", result), Title: "slow_calc"}, nil
			},
		},
		&simpleTool{
			name:        "counter",
			description: "Increment a named counter by a given amount and return the new value. Use key 'default' when no specific key needed.",
			schema: map[string]any{
				"type":     "object",
				"required": []string{"key", "delta"},
				"properties": map[string]any{
					"key":   map[string]any{"type": "string", "description": "counter name"},
					"delta": map[string]any{"type": "integer", "description": "amount to add (can be negative)"},
				},
			},
			fn: func(_ context.Context, input map[string]any) (tool.Result, error) {
				key, _ := input["key"].(string)
				if key == "" {
					key = "default"
				}
				delta := 0
				switch v := input["delta"].(type) {
				case float64:
					delta = int(v)
				case int:
					delta = v
				}
				mu.Lock()
				counters[key] += delta
				val := counters[key]
				mu.Unlock()
				return tool.Result{
					Output:   fmt.Sprintf("counter[%s] = %d (delta %+d)", key, val, delta),
					Title:    "counter",
					Metadata: map[string]any{"key": key, "value": val},
				}, nil
			},
		},
		&simpleTool{
			name:        "tool_failure",
			description: "Always returns a recoverable ToolFailure error. Use to test how the session handles tool errors without crashing.",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"message": map[string]any{"type": "string", "description": "custom failure message"},
				},
			},
			fn: func(_ context.Context, input map[string]any) (tool.Result, error) {
				msg, _ := input["message"].(string)
				if msg == "" {
					msg = "intentional tool failure for testing"
				}
				return tool.Result{}, tool.Fail(msg)
			},
		},
		&simpleTool{
			name:        "doom_bait",
			description: "Echoes the input value back unchanged. Do NOT call this tool more than once with the same input.",
			schema: map[string]any{
				"type":     "object",
				"required": []string{"value"},
				"properties": map[string]any{
					"value": map[string]any{"type": "string", "description": "any string value"},
				},
			},
			fn: func(_ context.Context, input map[string]any) (tool.Result, error) {
				v, _ := input["value"].(string)
				return tool.Result{Output: v, Title: "doom_bait"}, nil
			},
		},
	}
}

// simpleTool is a lightweight tool.Tool implementation backed by a closure.
type simpleTool struct {
	name        string
	description string
	schema      map[string]any
	fn          func(context.Context, map[string]any) (tool.Result, error)
}

func (t *simpleTool) Name() string                    { return t.name }
func (t *simpleTool) Description() string             { return t.description }
func (t *simpleTool) InputSchema() map[string]any     { return t.schema }
func (t *simpleTool) Execute(ctx context.Context, input map[string]any) (tool.Result, error) {
	return t.fn(ctx, input)
}

// evalExpr evaluates a simple two-operand arithmetic expression.
// Supports: +, -, *, /
// Also accepts multi-operand left-to-right evaluation.
func evalExpr(expr string) (float64, error) {
	expr = strings.TrimSpace(expr)
	// Simple recursive descent for + - * /
	return parseAddSub(expr)
}

func parseAddSub(expr string) (float64, error) {
	left, rest, err := parseMulDiv(expr)
	if err != nil {
		return 0, err
	}
	for {
		rest = strings.TrimSpace(rest)
		if len(rest) == 0 {
			return left, nil
		}
		op := rest[0]
		if op != '+' && op != '-' {
			return left, nil
		}
		right, tail, err := parseMulDiv(strings.TrimSpace(rest[1:]))
		if err != nil {
			return 0, err
		}
		if op == '+' {
			left += right
		} else {
			left -= right
		}
		rest = tail
	}
}

func parseMulDiv(expr string) (float64, string, error) {
	left, rest, err := parseNumber(expr)
	if err != nil {
		return 0, rest, err
	}
	for {
		rest = strings.TrimSpace(rest)
		if len(rest) == 0 {
			return left, rest, nil
		}
		op := rest[0]
		if op != '*' && op != '/' {
			return left, rest, nil
		}
		right, tail, err := parseNumber(strings.TrimSpace(rest[1:]))
		if err != nil {
			return 0, tail, err
		}
		if op == '*' {
			left *= right
		} else {
			if right == 0 {
				return 0, tail, fmt.Errorf("division by zero")
			}
			left /= right
		}
		rest = tail
	}
}

func parseNumber(expr string) (float64, string, error) {
	expr = strings.TrimSpace(expr)
	i := 0
	if i < len(expr) && (expr[i] == '-' || expr[i] == '+') {
		i++
	}
	for i < len(expr) && (expr[i] >= '0' && expr[i] <= '9' || expr[i] == '.') {
		i++
	}
	if i == 0 {
		return 0, expr, fmt.Errorf("expected number, got %q", expr)
	}
	f, err := strconv.ParseFloat(expr[:i], 64)
	return f, expr[i:], err
}

// ── index building ────────────────────────────────────────────────────────────

// buildMemoryIndex creates an in-memory Bleve index from all .md files under
// skillsDir.  Returns the index, the number of documents indexed, and any error.
func buildMemoryIndex(skillsDir string) (bleve.Index, int, error) {
	mapping := bleve.NewIndexMapping()

	text := bleve.NewTextFieldMapping()
	text.Store = true
	text.Index = true

	kw := bleve.NewKeywordFieldMapping()
	kw.Store = true
	kw.Index = true

	dm := bleve.NewDocumentMapping()
	dm.AddFieldMappingsAt("title", text)
	dm.AddFieldMappingsAt("content", text)
	dm.AddFieldMappingsAt("skill", kw)
	dm.AddFieldMappingsAt("path", kw)
	mapping.AddDocumentMapping("_default", dm)

	idx, err := bleve.NewMemOnly(mapping)
	if err != nil {
		return nil, 0, fmt.Errorf("create memory index: %w", err)
	}

	batch := idx.NewBatch()
	count := 0
	err = filepath.WalkDir(skillsDir, func(path string, de os.DirEntry, err error) error {
		if err != nil || de.IsDir() || !strings.EqualFold(filepath.Ext(path), ".md") {
			return err
		}
		rel, _ := filepath.Rel(skillsDir, path)
		skill := strings.SplitN(rel, string(filepath.Separator), 2)[0]

		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		title, body := parseFrontmatter(string(raw))
		if title == "" {
			title = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		}

		if err := batch.Index(rel, map[string]any{
			"title":   title,
			"content": body,
			"skill":   skill,
			"path":    path,
		}); err != nil {
			return err
		}
		log.Printf("  indexed: %s (%s, %d chars)", rel, title, len(body))
		count++
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	if err := idx.Batch(batch); err != nil {
		return nil, 0, fmt.Errorf("flush batch: %w", err)
	}
	return idx, count, nil
}

// parseFrontmatter extracts the "name:" field from YAML frontmatter.
func parseFrontmatter(src string) (name, body string) {
	src = strings.TrimPrefix(src, "\xef\xbb\xbf")
	if !strings.HasPrefix(src, "---") {
		return "", src
	}
	rest := src[3:]
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return "", src
	}
	body = strings.TrimPrefix(rest[idx+4:], "\n")
	scanner := bufio.NewScanner(strings.NewReader(rest[:idx]))
	for scanner.Scan() {
		if after, ok := strings.CutPrefix(scanner.Text(), "name:"); ok {
			name = strings.TrimSpace(after)
			break
		}
	}
	return name, body
}

// ── JSON helpers ──────────────────────────────────────────────────────────────

type resultJSON struct {
	RefID    string         `json:"ref_id"`
	Title    string         `json:"title"`
	Source   string         `json:"source"`
	Score    float64        `json:"score"`
	Snippet  string         `json:"snippet,omitempty"`
	Content  string         `json:"content,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

func toResultJSON(r knowledge.Result) resultJSON {
	return resultJSON{
		RefID: r.RefID, Title: r.Title, Source: r.Source,
		Score: r.Score, Snippet: r.Snippet, Content: r.Content, Metadata: r.Metadata,
	}
}

func toResultsJSON(rs []knowledge.Result) []resultJSON {
	out := make([]resultJSON, len(rs))
	for i, r := range rs {
		out[i] = toResultJSON(r)
	}
	return out
}

func handleSchema(t interface{ InputSchema() map[string]any }) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, t.InputSchema())
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s  %v", r.Method, r.URL.Path, time.Since(start))
	})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
