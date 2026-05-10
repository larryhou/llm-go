// Command knowledge-api starts an HTTP server that exposes a Bleve-backed
// knowledge index via a simple JSON API.
//
// # Usage
//
//	go run ./cmd/knowledge-api \
//	    -index  /path/to/skills.bleve \
//	    -addr   127.0.0.1:7700
//
// # Endpoints
//
//	POST /search
//	  Request:  {"query":"...", "max_results":5, "filters":{...}}
//	  Response: {"results":[{"ref_id":"...","title":"...","source":"...","score":0.9,"snippet":"..."},...]}
//
//	POST /fetch
//	  Request:  {"ref_id":"skills:knowledge/SKILL.md"}
//	  Response: {"result":{"ref_id":"...","title":"...","source":"...","content":"..."}}
//
//	GET /health
//	  Response: {"status":"ok","doc_count":2}
//
// Both POST endpoints also accept GET with query parameters:
//
//	GET /search?q=knowledge+manager&max_results=3
//	GET /fetch?ref_id=skills:knowledge/SKILL.md
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	bleve "github.com/blevesearch/bleve/v2"

	"github.com/larryhou/llm-go/knowledge"
	blevesource "github.com/larryhou/llm-go/knowledge/source/bleve"
)

func main() {
	indexPath := flag.String("index", "", "path to the Bleve index (required)")
	addr := flag.String("addr", "127.0.0.1:7700", "listen address")
	sourceID := flag.String("source", "skills", "source ID prefix for RefIDs")
	snippetMax := flag.Int("snippet-max", 400, "max chars per snippet")
	contentMax := flag.Int("content-max", 8000, "max chars for fetched content")
	flag.Parse()

	if *indexPath == "" {
		flag.Usage()
		log.Fatal("flag -index is required")
	}

	idx, err := bleve.Open(*indexPath)
	if err != nil {
		log.Fatalf("open index %q: %v", *indexPath, err)
	}
	defer idx.Close()

	count, _ := idx.DocCount()
	log.Printf("opened index %s  (%d documents)", *indexPath, count)

	km := knowledge.NewManager(knowledge.ManagerConfig{
		SourceTimeout:       10 * time.Second,
		MaxResults:          20,
		SnippetMaxChars:     *snippetMax,
		ContentMaxChars:     *contentMax,
		AllowPartialFailure: false,
	})
	km.Register(blevesource.New(idx, *sourceID, 0, &blevesource.Config{
		TitleField:   "title",
		ContentField: "content",
	}))

	tools := km.Tools()
	// tools[0] = knowledge_search, tools[1] = knowledge_fetch

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth(idx))
	mux.HandleFunc("/search", handleSearch(km))
	mux.HandleFunc("/fetch", handleFetch(km))
	// Convenience: expose raw tool schemas
	mux.HandleFunc("/schema/search", handleSchema(tools[0]))
	mux.HandleFunc("/schema/fetch", handleSchema(tools[1]))

	log.Printf("knowledge-api listening on %s", *addr)
	if err := http.ListenAndServe(*addr, logMiddleware(mux)); err != nil {
		log.Fatal(err)
	}
}

// ── request / response types ──────────────────────────────────────────────────

type searchRequest struct {
	Query      string         `json:"query"`
	MaxResults int            `json:"max_results"`
	Filters    map[string]any `json:"filters"`
}

type searchResponse struct {
	Results []resultJSON `json:"results"`
}

type fetchRequest struct {
	RefID string `json:"ref_id"`
}

type fetchResponse struct {
	Result resultJSON `json:"result"`
}

type resultJSON struct {
	RefID    string         `json:"ref_id"`
	Title    string         `json:"title"`
	Source   string         `json:"source"`
	Score    float64        `json:"score"`
	Snippet  string         `json:"snippet,omitempty"`
	Content  string         `json:"content,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type healthResponse struct {
	Status   string `json:"status"`
	DocCount uint64 `json:"doc_count"`
}

func toResultJSON(r knowledge.Result) resultJSON {
	return resultJSON{
		RefID:    r.RefID,
		Title:    r.Title,
		Source:   r.Source,
		Score:    r.Score,
		Snippet:  r.Snippet,
		Content:  r.Content,
		Metadata: r.Metadata,
	}
}

// ── handlers ──────────────────────────────────────────────────────────────────

func handleHealth(idx bleve.Index) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		count, _ := idx.DocCount()
		writeJSON(w, http.StatusOK, healthResponse{Status: "ok", DocCount: count})
	}
}

func handleSearch(km *knowledge.Manager) http.HandlerFunc {
	tools := km.Tools()
	searchTool := tools[0]
	return func(w http.ResponseWriter, r *http.Request) {
		var req searchRequest
		if r.Method == http.MethodGet {
			req.Query = r.URL.Query().Get("q")
			if n, err := strconv.Atoi(r.URL.Query().Get("max_results")); err == nil {
				req.MaxResults = n
			}
		} else {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
		}
		if req.Query == "" {
			writeErr(w, http.StatusBadRequest, "query is required")
			return
		}
		if req.MaxResults <= 0 {
			req.MaxResults = 5
		}

		input := map[string]any{
			"query":       req.Query,
			"max_results": float64(req.MaxResults),
		}
		if len(req.Filters) > 0 {
			input["filters"] = req.Filters
		}

		toolResult, err := searchTool.Execute(r.Context(), input)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}

		// Re-execute via Manager directly to get structured results.
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		results, err := km.Search(ctx, knowledge.Query{
			Type:       knowledge.QueryTypeSearch,
			Input:      req.Query,
			Filters:    req.Filters,
			MaxResults: req.MaxResults,
		})
		if err != nil {
			// Fall back to raw tool output if direct call fails.
			writeJSON(w, http.StatusOK, map[string]any{"output": toolResult.Output})
			return
		}

		resp := searchResponse{Results: make([]resultJSON, len(results))}
		for i, r := range results {
			resp.Results[i] = toResultJSON(r)
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func handleFetch(km *knowledge.Manager) http.HandlerFunc {
	tools := km.Tools()
	fetchTool := tools[1]
	return func(w http.ResponseWriter, r *http.Request) {
		var req fetchRequest
		if r.Method == http.MethodGet {
			req.RefID = r.URL.Query().Get("ref_id")
		} else {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
		}
		if req.RefID == "" {
			writeErr(w, http.StatusBadRequest, "ref_id is required")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		results, err := km.Fetch(ctx, knowledge.Query{
			Type:  knowledge.QueryTypeFetch,
			Input: req.RefID,
		})
		if err != nil {
			// Try raw tool as fallback.
			toolResult, terr := fetchTool.Execute(r.Context(), map[string]any{"ref_id": req.RefID})
			if terr != nil {
				writeErr(w, http.StatusNotFound, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"output": toolResult.Output})
			return
		}
		if len(results) == 0 {
			writeErr(w, http.StatusNotFound, fmt.Sprintf("ref_id %q not found", req.RefID))
			return
		}
		writeJSON(w, http.StatusOK, fetchResponse{Result: toResultJSON(results[0])})
	}
}

func handleSchema(t interface{ InputSchema() map[string]any }) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, t.InputSchema())
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

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
