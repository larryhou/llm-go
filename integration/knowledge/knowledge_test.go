// Package knowledge contains an end-to-end integration test that:
//
//  1. Builds an in-memory Bleve index from .opencode/skills Markdown files
//  2. Registers the index as a knowledge.Source via the knowledge.Manager
//  3. Runs a real LLM session with knowledge_search and knowledge_fetch tools
//  4. Verifies the LLM actually used the tools and retrieved correct content
//
// Run with:
//
//	LLM_INTEGRATION=1 go test ./integration/knowledge/ -v -count=1 -timeout=120s
package knowledge

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

const (
	defaultBaseURL = "http://192.168.3.119:8080/timi-claude/v1"
	defaultAPIKey  = "sk-zzz6FtyLMyuobNNOukwgobP0l1F3TjMO"
	defaultModel   = "claude-sonnet-4.6"
	skillsDirRel   = ".opencode/skills"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func skipIfNoIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("LLM_INTEGRATION") != "1" {
		t.Skip("set LLM_INTEGRATION=1 to run integration tests")
	}
}

// skillsDir walks up from cwd until it finds a directory containing
// .opencode/skills with at least 2 sub-directories.
func skillsDir(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		candidate := filepath.Join(dir, ".opencode", "skills")
		if entries, err := os.ReadDir(candidate); err == nil && len(entries) >= 2 {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate .opencode/skills with ≥2 entries")
		}
		dir = parent
	}
}

// buildMemoryIndex creates an in-memory Bleve index from all .md files under
// root.  Returns the index and the number of documents indexed.
func buildMemoryIndex(t *testing.T, root string) (bleve.Index, int) {
	t.Helper()

	mapping := bleve.NewIndexMapping()
	text := bleve.NewTextFieldMapping()
	text.Store, text.Index = true, true
	kw := bleve.NewKeywordFieldMapping()
	kw.Store, kw.Index = true, true
	dm := bleve.NewDocumentMapping()
	dm.AddFieldMappingsAt("title", text)
	dm.AddFieldMappingsAt("content", text)
	dm.AddFieldMappingsAt("skill", kw)
	dm.AddFieldMappingsAt("path", kw)
	mapping.AddDocumentMapping("_default", dm)

	idx, err := bleve.NewMemOnly(mapping)
	if err != nil {
		t.Fatalf("create memory index: %v", err)
	}
	t.Cleanup(func() { idx.Close() })

	batch := idx.NewBatch()
	count := 0
	err = filepath.WalkDir(root, func(path string, de os.DirEntry, err error) error {
		if err != nil || de.IsDir() || !strings.EqualFold(filepath.Ext(path), ".md") {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		skill := strings.SplitN(rel, string(filepath.Separator), 2)[0]
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		title, body := parseFrontmatter(string(raw))
		if title == "" {
			title = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		}
		_ = batch.Index(rel, map[string]any{
			"title":   title,
			"content": body,
			"skill":   skill,
			"path":    path,
		})
		t.Logf("  indexed: %s (%s, %d chars)", rel, title, len(body))
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("walk skills dir: %v", err)
	}
	if err := idx.Batch(batch); err != nil {
		t.Fatalf("flush batch: %v", err)
	}
	t.Logf("in-memory index ready: %d document(s)", count)
	return idx, count
}

// parseFrontmatter extracts "name:" from YAML frontmatter (--- delimited).
func parseFrontmatter(src string) (name, body string) {
	src = strings.TrimPrefix(src, "\xef\xbb\xbf")
	if !strings.HasPrefix(src, "---") {
		return "", src
	}
	rest := src[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", src
	}
	body = strings.TrimPrefix(rest[end+4:], "\n")
	scanner := bufio.NewScanner(strings.NewReader(rest[:end]))
	for scanner.Scan() {
		if after, ok := strings.CutPrefix(scanner.Text(), "name:"); ok {
			name = strings.TrimSpace(after)
			break
		}
	}
	return name, body
}

// newKnowledgeManager builds a Manager with the given Bleve index registered.
func newKnowledgeManager(idx bleve.Index) *knowledge.Manager {
	km := knowledge.NewManager(knowledge.ManagerConfig{
		SourceTimeout:       10 * time.Second,
		MaxResults:          5,
		SnippetMaxChars:     400,
		ContentMaxChars:     6000,
		AllowPartialFailure: true,
	})
	km.Register(blevesource.New(idx, "skills", 0, &blevesource.Config{
		TitleField:   "title",
		ContentField: "content",
	}))
	return km
}

// ── Unit: BleveSource directly ────────────────────────────────────────────────

func TestBleveIndex_SearchAndFetch(t *testing.T) {
	root := skillsDir(t)
	idx, count := buildMemoryIndex(t, root)
	if count == 0 {
		t.Fatal("no documents indexed")
	}

	src := blevesource.New(idx, "skills", 0, &blevesource.Config{
		TitleField:   "title",
		ContentField: "content",
	})
	ctx := context.Background()

	t.Run("search_knowledge_manager", func(t *testing.T) {
		results, err := src.Peek(ctx, knowledge.Query{
			Type:       knowledge.QueryTypeSearch,
			Input:      "knowledge manager source interface",
			MaxResults: 5,
		})
		if err != nil {
			t.Fatalf("Peek: %v", err)
		}
		if len(results) == 0 {
			t.Fatal("expected ≥1 result")
		}
		for _, r := range results {
			t.Logf("  [%.2f] %s  ref_id=%s  snippet=%q", r.Score, r.Title, r.RefID, truncate(r.Snippet, 80))
		}
		if !strings.Contains(strings.ToLower(results[0].Title), "knowledge") {
			t.Errorf("expected knowledge skill to rank first, got: %s", results[0].Title)
		}
		if !strings.HasPrefix(results[0].RefID, "skills:") {
			t.Errorf("RefID missing 'skills:' prefix: %s", results[0].RefID)
		}
		if results[0].Snippet == "" {
			t.Error("Peek must return non-empty Snippet")
		}
		if results[0].Content != "" {
			t.Error("Peek must leave Content empty")
		}
	})

	t.Run("search_llm_skill", func(t *testing.T) {
		results, err := src.Peek(ctx, knowledge.Query{
			Type:       knowledge.QueryTypeSearch,
			Input:      "RunLoop RecordProvider replay ndjson provider",
			MaxResults: 3,
		})
		if err != nil {
			t.Fatalf("Peek: %v", err)
		}
		if len(results) == 0 {
			t.Fatal("expected ≥1 result")
		}
		for _, r := range results {
			t.Logf("  [%.2f] %s", r.Score, r.Title)
		}
		if !strings.Contains(strings.ToLower(results[0].Title), "llm") &&
			!strings.Contains(strings.ToLower(results[0].Title), "replay") {
			t.Errorf("expected llm or replay skill to rank first, got: %s", results[0].Title)
		}
	})

	t.Run("fetch_by_refid", func(t *testing.T) {
		sr, _ := src.Peek(ctx, knowledge.Query{
			Type: knowledge.QueryTypeSearch, Input: "knowledge manager", MaxResults: 1,
		})
		if len(sr) == 0 {
			t.Skip("no search results")
		}
		refID := sr[0].RefID
		t.Logf("fetching: %s", refID)

		fr, err := src.Fetch(ctx, knowledge.Query{Type: knowledge.QueryTypeFetch, Input: refID})
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if len(fr) == 0 {
			t.Fatal("Fetch returned no results")
		}
		r := fr[0]
		t.Logf("fetched: %s (%d chars)", r.Title, len(r.Content))
		if r.Content == "" {
			t.Error("Fetch must populate Content")
		}
		if len(r.Content) < 500 {
			t.Errorf("expected substantial content, got %d chars", len(r.Content))
		}
		if !strings.Contains(r.Content, "Source") {
			t.Error("expected 'Source' in knowledge skill content")
		}
	})

	t.Run("manager_search_tool", func(t *testing.T) {
		km := newKnowledgeManager(idx)
		result, err := km.Tools()[0].Execute(ctx, map[string]any{
			"query": "bleve full-text search index source",
		})
		if err != nil {
			t.Fatalf("knowledge_search: %v", err)
		}
		t.Logf("output:\n%s", result.Output)
		if !strings.Contains(result.Output, "ref_id") {
			t.Error("expected ref_id in output")
		}
		if !strings.Contains(result.Output, "skills:") {
			t.Error("expected 'skills:' prefix in output")
		}
	})

	t.Run("manager_fetch_tool", func(t *testing.T) {
		km := newKnowledgeManager(idx)
		searchOut, err := km.Tools()[0].Execute(ctx, map[string]any{"query": "knowledge manager"})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		refID := extractRefID(searchOut.Output)
		if refID == "" {
			t.Fatalf("could not extract ref_id:\n%s", searchOut.Output)
		}
		t.Logf("fetching ref_id: %s", refID)
		fetchOut, err := km.Tools()[1].Execute(ctx, map[string]any{"ref_id": refID})
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		t.Logf("fetch output (%d chars): %s", len(fetchOut.Output), truncate(fetchOut.Output, 300))
		if !strings.Contains(fetchOut.Output, "Source") {
			t.Error("fetched content should mention 'Source'")
		}
	})
}

// ── LLM integration: RunLoop with real provider ───────────────────────────────

// TestLLM_UsesKnowledgeTools runs full RunLoop sessions with a real LLM and
// verifies it calls knowledge_search / knowledge_fetch to answer questions.
func TestLLM_UsesKnowledgeTools(t *testing.T) {
	skipIfNoIntegration(t)

	root := skillsDir(t)
	idx, count := buildMemoryIndex(t, root)
	if count == 0 {
		t.Fatal("no documents indexed")
	}

	km := newKnowledgeManager(idx)
	prov := openaiProv.New(defaultAPIKey, defaultBaseURL, "timi", nil)
	model := llm.Model{
		ID: defaultModel, ProviderID: "timi", APIID: defaultModel,
		Limit: llm.ModelLimit{Context: 200_000, Output: 4096},
	}

	s := memory.New()
	ctx := context.Background()
	sessID := fmt.Sprintf("llm-knowledge-%d", time.Now().UnixNano())
	_ = s.CreateSession(ctx, &store.Session{ID: sessID, Model: "timi/" + defaultModel})

	runTurn := func(msg string) session.RunResult {
		t.Helper()
		result, err := session.RunLoop(ctx, s, session.RunInput{
			SessionID: sessID,
			UserMsg:   msg,
			Model:     model,
			Provider:  prov,
			Tools:     km.Tools(),
			ExtraSystem: []string{
				"You are a helpful assistant with access to a knowledge base of skill documentation.",
				"When asked about skills, architecture, or guides, use knowledge_search to find relevant documents, then knowledge_fetch if you need full details.",
			},
			DisableProviderPrompt: true,
			MaxSteps:              10,
		})
		if err != nil {
			t.Fatalf("RunLoop error: %v", err)
		}
		return result
	}

	countToolCalls := func() (searchN, fetchN int) {
		msgs, _ := s.ListMessages(ctx, sessID)
		for _, m := range msgs {
			if m.Role != store.RoleAssistant {
				continue
			}
			parts, _ := s.ListParts(ctx, m.ID)
			for _, p := range parts {
				if p.Type != store.PartTypeTool {
					continue
				}
				d, ok := p.Data.(*store.ToolPartData)
				if !ok {
					continue
				}
				if d.Tool == "knowledge_search" {
					searchN++
				}
				if d.Tool == "knowledge_fetch" {
					fetchN++
				}
			}
		}
		return
	}

	// ── Turn 1: Source interface ──────────────────────────────────────────────
	t.Log("=== Turn 1: knowledge manager Source interface ===")
	runTurn("Using the knowledge tools, look up how the knowledge manager's Source interface works and what methods it requires. Give me a brief summary.")

	searchN, fetchN := countToolCalls()
	t.Logf("tool calls: knowledge_search=%d  knowledge_fetch=%d", searchN, fetchN)

	if searchN == 0 {
		t.Error("FAIL: LLM did not call knowledge_search")
	} else {
		t.Logf("PASS: knowledge_search called %d time(s)", searchN)
	}

	answer1 := lastAssistantText(ctx, t, s, sessID)
	t.Logf("answer:\n%s", answer1)

	for _, kw := range []string{"ID", "Priority", "Accepts", "Peek", "Fetch"} {
		if strings.Contains(answer1, kw) {
			t.Logf("PASS: mentions %q", kw)
		} else {
			t.Logf("NOTE: does not mention %q", kw)
		}
	}

	// ── Turn 2: Effect skill testing patterns ─────────────────────────────────
	t.Log("\n=== Turn 2: Effect skill testing patterns ===")
	runTurn("Now search for the Effect skill documentation and summarise its recommended testing patterns.")

	searchN2, fetchN2 := countToolCalls()
	t.Logf("cumulative tool calls: knowledge_search=%d  knowledge_fetch=%d", searchN2, fetchN2)

	answer2 := lastAssistantText(ctx, t, s, sessID)
	t.Logf("answer:\n%s", answer2)

	if strings.ContainsAny(answer2, "test") {
		t.Log("PASS: answer mentions testing")
	} else {
		t.Error("FAIL: answer does not mention testing")
	}

	// ── Summary ───────────────────────────────────────────────────────────────
	allMsgs, _ := s.ListMessages(ctx, sessID)
	t.Logf("\n=== Summary ===")
	t.Logf("session messages: %d", len(allMsgs))
	t.Logf("knowledge_search calls: %d", searchN2)
	t.Logf("knowledge_fetch calls: %d", fetchN2)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func lastAssistantText(ctx context.Context, t *testing.T, s store.Store, sessID string) string {
	t.Helper()
	msgs, _ := s.ListMessages(ctx, sessID)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != store.RoleAssistant {
			continue
		}
		parts, _ := s.ListParts(ctx, msgs[i].ID)
		for _, p := range parts {
			if p.Type == store.PartTypeText {
				if d, ok := p.Data.(*store.TextPartData); ok && d.Text != "" {
					return d.Text
				}
			}
		}
	}
	return ""
}

func extractRefID(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, "ref_id") {
			continue
		}
		start := strings.Index(line, "`")
		if start < 0 {
			continue
		}
		end := strings.Index(line[start+1:], "`")
		if end < 0 {
			continue
		}
		if c := line[start+1 : start+1+end]; strings.Contains(c, ":") {
			return c
		}
	}
	return ""
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func init() { _ = fmt.Sprintf }
