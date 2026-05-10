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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bleve "github.com/blevesearch/bleve/v2"

	"github.com/larryhou/llm-go/knowledge"
	blevesource "github.com/larryhou/llm-go/knowledge/source/bleve"
	"github.com/larryhou/llm-go/llm"
	openaiProv "github.com/larryhou/llm-go/provider/openai"
	"github.com/larryhou/llm-go/session"
	"github.com/larryhou/llm-go/store"
	"github.com/larryhou/llm-go/store/memory"
)

// ── config ────────────────────────────────────────────────────────────────────

type serverConfig struct {
	skillsDir  string
	addr       string
	sourceID   string
	snippetMax int
	contentMax int
	// LLM
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
	flag.StringVar(&cfg.baseURL, "llm-url", envOr("TIMI_BASE_URL", "http://192.168.3.119:8080/timi-claude/v1"), "LLM base URL")
	flag.StringVar(&cfg.apiKey, "llm-key", envOr("TIMI_API_KEY", "sk-zzz6FtyLMyuobNNOukwgobP0l1F3TjMO"), "LLM API key")
	flag.StringVar(&cfg.modelID, "model", envOr("TIMI_MODEL", "claude-sonnet-4.6"), "LLM model ID")
	flag.IntVar(&cfg.maxSteps, "max-steps", 10, "max LLM steps per turn")
	flag.Parse()

	if cfg.skillsDir == "" {
		flag.Usage()
		log.Fatal("flag -skills is required")
	}

	// Build in-memory Bleve index from skills directory.
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
	}

	tools := km.Tools()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", srv.handleHealth)
	mux.HandleFunc("/search", srv.handleSearch)
	mux.HandleFunc("/fetch", srv.handleFetch)
	mux.HandleFunc("/chat", srv.handleChat)
	mux.HandleFunc("/schema/search", handleSchema(tools[0]))
	mux.HandleFunc("/schema/fetch", handleSchema(tools[1]))

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

	// active sessions: sessionID -> *chatSession
	mu       sync.Mutex
	sessions map[string]*chatSession
}

type chatSession struct {
	id    string
	store store.Store // per-session isolated store
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
		Message   string `json:"message"`
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		writeErr(w, http.StatusBadRequest, "message is required")
		return
	}

	// Resolve or create session.
	sessID := req.SessionID
	if sessID == "" {
		sessID = fmt.Sprintf("sess-%d", time.Now().UnixNano())
	}
	ctx := r.Context()
	if _, err := s.sessionStore.GetSession(ctx, sessID); err != nil {
		// New session.
		_ = s.sessionStore.CreateSession(ctx, &store.Session{
			ID:    sessID,
			Model: "timi/" + s.cfg.modelID,
		})
		s.sessionCount.Add(1)
	}

	// Set SSE headers.
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

	// Wrap the provider to intercept streaming events and forward to SSE.
	prov := openaiProv.New(s.cfg.apiKey, s.cfg.baseURL, "timi", nil)
	wrappedProv := &sseProvider{inner: prov, send: sendEvent}

	model := llm.Model{
		ID:         s.cfg.modelID,
		ProviderID: "timi",
		APIID:      s.cfg.modelID,
		Limit:      llm.ModelLimit{Context: 200_000, Output: 4096},
	}

	_, err := session.RunLoop(ctx, s.sessionStore, session.RunInput{
		SessionID: sessID,
		UserMsg:   req.Message,
		Model:     model,
		Provider:  wrappedProv,
		Tools:     s.km.Tools(),
		ExtraSystem: []string{
			"You are a helpful assistant with access to a knowledge base of skill documentation.",
			"When asked about skills, architecture, or development guides, use knowledge_search to find relevant documents.",
			"Use knowledge_fetch to retrieve the full content of a document when you need details.",
		},
		DisableProviderPrompt: true,
		MaxSteps:              s.cfg.maxSteps,
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
			case llm.EventToolResult:
				p.send(map[string]any{
					"type":   "tool_result",
					"tool":   ev.ToolName,
					"output": ev.Output,
				})
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
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
