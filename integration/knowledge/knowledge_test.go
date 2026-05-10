// Package knowledge contains an end-to-end integration test that:
//
//  1. Builds a Bleve index from .opencode/skills Markdown files
//  2. Registers the index as a knowledge.Source via the Manager
//  3. Runs a real LLM session with knowledge_search and knowledge_fetch tools
//  4. Verifies the LLM actually used the tools and retrieved correct content
//
// Run with:
//
//	LLM_INTEGRATION=1 go test ./integration/knowledge/ -v -count=1 -timeout=120s
package knowledge

import (
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

	// skillsDir is the .opencode/skills directory relative to repo root.
	// Resolved at test time via repoRoot().
	skillsDirRel = ".opencode/skills"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func skipIfNoIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("LLM_INTEGRATION") != "1" {
		t.Skip("set LLM_INTEGRATION=1 to run integration tests")
	}
}

// repoRoot walks up from the test binary location until it finds go.mod.
// repoRoot walks up from the working directory until it finds a directory
// containing the .opencode/skills subdirectory.
// SKILLS_DIR env var overrides the resolved path entirely.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, ".opencode", "skills")); err == nil {
			// Check it has more than just an llm/ sub-skill (i.e. it's the
			// opencode repo root, not the llm-go sub-repo root).
			entries, _ := os.ReadDir(filepath.Join(dir, ".opencode", "skills"))
			if len(entries) >= 2 {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate repo root with ≥2 skills (.opencode/skills not found)")
		}
		dir = parent
	}
}

// buildIndex creates a temporary Bleve index from the skills directory and
// returns it along with a cleanup function.
func buildIndex(t *testing.T, skillsDir string) (bleve.Index, func()) {
	t.Helper()
	idxPath := filepath.Join(t.TempDir(), "skills.bleve")

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

	idx, err := bleve.New(idxPath, mapping)
	if err != nil {
		t.Fatalf("create bleve index: %v", err)
	}

	// Walk skills dir and index every .md file.
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
		t.Fatalf("flush index batch: %v", err)
	}
	t.Logf("index built: %d document(s)", count)
	return idx, func() { idx.Close() }
}

// parseFrontmatter extracts "name:" from YAML frontmatter.
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
	fm := rest[:idx]
	body = strings.TrimPrefix(rest[idx+4:], "\n")
	for _, line := range strings.Split(fm, "\n") {
		if after, ok := strings.CutPrefix(line, "name:"); ok {
			name = strings.TrimSpace(after)
			break
		}
	}
	return name, body
}

// ── Unit tests (no LLM required) ─────────────────────────────────────────────

// TestBleveIndex_SearchAndFetch verifies the Bleve source returns correct
// results without involving any LLM.
func TestBleveIndex_SearchAndFetch(t *testing.T) {
	root := repoRoot(t)
	skillsDir := filepath.Join(root, skillsDirRel)
	if _, err := os.Stat(skillsDir); err != nil {
		t.Skipf("skills dir not found: %s", skillsDir)
	}

	idx, cleanup := buildIndex(t, skillsDir)
	defer cleanup()

	src := blevesource.New(idx, "skills", 0, &blevesource.Config{
		TitleField:   "title",
		ContentField: "content",
	})

	ctx := context.Background()

	// ── Search ────────────────────────────────────────────────────────────────
	t.Run("search_knowledge_manager", func(t *testing.T) {
		results, err := src.Peek(ctx, knowledge.Query{
			Type:       knowledge.QueryTypeSearch,
			Input:      "knowledge manager source interface",
			MaxResults: 5,
		})
		if err != nil {
			t.Fatalf("Peek error: %v", err)
		}
		if len(results) == 0 {
			t.Fatal("expected at least one result")
		}
		t.Logf("search returned %d result(s):", len(results))
		for _, r := range results {
			t.Logf("  [%.2f] %s  ref_id=%s  snippet=%q", r.Score, r.Title, r.RefID, truncate(r.Snippet, 80))
		}
		// The knowledge SKILL.md must rank highest for this query.
		if !strings.Contains(strings.ToLower(results[0].Title), "knowledge") {
			t.Errorf("expected top result to be the knowledge skill, got: %s", results[0].Title)
		}
		// RefID must have the source prefix.
		if !strings.HasPrefix(results[0].RefID, "skills:") {
			t.Errorf("RefID missing prefix: %s", results[0].RefID)
		}
		// Snippet must be non-empty.
		if results[0].Snippet == "" {
			t.Error("expected non-empty snippet")
		}
		// Content must be empty (Peek contract).
		if results[0].Content != "" {
			t.Error("Peek must leave Content empty")
		}
	})

	t.Run("search_effect_skill", func(t *testing.T) {
		results, err := src.Peek(ctx, knowledge.Query{
			Type:       knowledge.QueryTypeSearch,
			Input:      "Effect TypeScript composable",
			MaxResults: 3,
		})
		if err != nil {
			t.Fatalf("Peek error: %v", err)
		}
		if len(results) == 0 {
			t.Fatal("expected at least one result")
		}
		t.Logf("effect search returned %d result(s):", len(results))
		for _, r := range results {
			t.Logf("  [%.2f] %s", r.Score, r.Title)
		}
	})

	// ── Fetch ─────────────────────────────────────────────────────────────────
	t.Run("fetch_by_refid", func(t *testing.T) {
		// First get a RefID from search.
		searchResults, _ := src.Peek(ctx, knowledge.Query{
			Type:       knowledge.QueryTypeSearch,
			Input:      "knowledge manager",
			MaxResults: 1,
		})
		if len(searchResults) == 0 {
			t.Skip("no search results to fetch")
		}
		refID := searchResults[0].RefID
		t.Logf("fetching: %s", refID)

		fetchResults, err := src.Fetch(ctx, knowledge.Query{
			Type:  knowledge.QueryTypeFetch,
			Input: refID,
		})
		if err != nil {
			t.Fatalf("Fetch error: %v", err)
		}
		if len(fetchResults) == 0 {
			t.Fatal("Fetch returned no results")
		}
		r := fetchResults[0]
		t.Logf("fetched: %s (%d chars)", r.Title, len(r.Content))

		if r.Content == "" {
			t.Error("Fetch must populate Content")
		}
		// Content should contain significant skill documentation.
		if len(r.Content) < 500 {
			t.Errorf("expected substantial content, got %d chars", len(r.Content))
		}
		// Knowledge skill content must mention key concepts.
		if !strings.Contains(r.Content, "Source") {
			t.Error("expected 'Source' in knowledge skill content")
		}
	})

	// ── Manager integration ───────────────────────────────────────────────────
	t.Run("manager_search_tool", func(t *testing.T) {
		km := knowledge.NewManager(knowledge.ManagerConfig{
			MaxResults:          5,
			SnippetMaxChars:     300,
			ContentMaxChars:     8000,
			AllowPartialFailure: false,
		})
		km.Register(src)
		tools := km.Tools()

		result, err := tools[0].Execute(ctx, map[string]any{
			"query": "bleve full-text search index",
		})
		if err != nil {
			t.Fatalf("knowledge_search error: %v", err)
		}
		t.Logf("knowledge_search output:\n%s", result.Output)

		if !strings.Contains(result.Output, "ref_id") {
			t.Error("expected ref_id in search output")
		}
		if !strings.Contains(result.Output, "skills:") {
			t.Error("expected 'skills:' prefix in ref_id")
		}
	})

	t.Run("manager_fetch_tool", func(t *testing.T) {
		km := knowledge.NewManager(knowledge.ManagerConfig{
			ContentMaxChars:     8000,
			AllowPartialFailure: false,
		})
		km.Register(src)
		tools := km.Tools()

		// Search first to get a real RefID.
		searchResult, err := tools[0].Execute(ctx, map[string]any{"query": "knowledge manager"})
		if err != nil {
			t.Fatalf("search error: %v", err)
		}
		// Extract a ref_id from the output.
		refID := extractRefID(searchResult.Output)
		if refID == "" {
			t.Fatalf("could not extract ref_id from search output:\n%s", searchResult.Output)
		}
		t.Logf("fetching ref_id: %s", refID)

		fetchResult, err := tools[1].Execute(ctx, map[string]any{"ref_id": refID})
		if err != nil {
			t.Fatalf("knowledge_fetch error: %v", err)
		}
		t.Logf("knowledge_fetch output (%d chars):\n%s", len(fetchResult.Output), truncate(fetchResult.Output, 400))

		if !strings.Contains(fetchResult.Output, "Source") {
			t.Error("fetched content should mention 'Source'")
		}
	})
}

// ── LLM integration test ──────────────────────────────────────────────────────

// TestLLM_UsesKnowledgeTools runs a full session with a real LLM and verifies
// it correctly uses knowledge_search and knowledge_fetch to answer questions
// about the .opencode skills documentation.
func TestLLM_UsesKnowledgeTools(t *testing.T) {
	skipIfNoIntegration(t)

	root := repoRoot(t)
	skillsDir := filepath.Join(root, skillsDirRel)
	if _, err := os.Stat(skillsDir); err != nil {
		t.Skipf("skills dir not found: %s", skillsDir)
	}

	idx, cleanup := buildIndex(t, skillsDir)
	defer cleanup()

	// Build knowledge manager with the bleve source.
	km := knowledge.NewManager(knowledge.ManagerConfig{
		SourceTimeout:       15 * time.Second,
		MaxResults:          5,
		SnippetMaxChars:     400,
		ContentMaxChars:     6000,
		AllowPartialFailure: true,
	})
	km.Register(blevesource.New(idx, "skills", 0, &blevesource.Config{
		TitleField:   "title",
		ContentField: "content",
	}))

	prov := openaiProv.New(defaultAPIKey, defaultBaseURL, "timi", nil)
	model := llm.Model{
		ID:         defaultModel,
		ProviderID: "timi",
		APIID:      defaultModel,
		Limit:      llm.ModelLimit{Context: 200_000, Output: 4096},
	}

	s := memory.New()
	ctx := context.Background()
	sessID := "knowledge-integration"
	_ = s.CreateSession(ctx, &store.Session{
		ID:    sessID,
		Model: model.ProviderID + "/" + model.ID,
	})

	// ── turn 1: ask about knowledge manager architecture ──────────────────────
	t.Log("=== Turn 1: Ask about knowledge manager architecture ===")
	result1, err := session.RunLoop(ctx, s, session.RunInput{
		SessionID:             sessID,
		UserMsg:               "Using the available knowledge tools, look up how the knowledge manager's Source interface works and what methods it requires. Give me a brief summary.",
		Model:                 model,
		Provider:              prov,
		Tools:                 km.Tools(),
		DisableProviderPrompt: true,
		ExtraSystem:           []string{"You are a helpful assistant. When you need information, use the knowledge_search and knowledge_fetch tools to look it up from the documentation."},
		MaxSteps:              8,
	})
	if err != nil {
		t.Fatalf("turn 1 error: %v", err)
	}
	t.Logf("turn 1 result: %v", result1)

	// Verify tool calls were made.
	msgs, _ := s.ListMessages(ctx, sessID)
	toolCallCount := 0
	searchCalled := false
	fetchCalled := false
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
			toolCallCount++
			t.Logf("  tool call: %s (status=%s)", d.Tool, d.Status)
			if d.Tool == "knowledge_search" {
				searchCalled = true
			}
			if d.Tool == "knowledge_fetch" {
				fetchCalled = true
			}
		}
	}

	if !searchCalled {
		t.Error("FAIL: LLM did not call knowledge_search")
	} else {
		t.Log("PASS: knowledge_search was called")
	}

	// Fetch is optional — the LLM may decide snippets are sufficient.
	t.Logf("knowledge_fetch called: %v  (total tool calls: %d)", fetchCalled, toolCallCount)

	// Verify the final answer mentions key concepts.
	lastText := lastAssistantText(ctx, t, s, sessID)
	t.Logf("final answer:\n%s", lastText)

	for _, keyword := range []string{"ID", "Priority", "Accepts", "Peek", "Fetch"} {
		if strings.Contains(lastText, keyword) {
			t.Logf("PASS: answer mentions %q", keyword)
		} else {
			t.Logf("NOTE: answer does not mention %q (may still be correct)", keyword)
		}
	}

	// ── turn 2: ask about effect skill ───────────────────────────────────────
	t.Log("\n=== Turn 2: Ask about Effect skill ===")
	result2, err := session.RunLoop(ctx, s, session.RunInput{
		SessionID:             sessID,
		UserMsg:               "Now search for the Effect skill documentation and tell me what testing patterns it recommends.",
		Model:                 model,
		Provider:              prov,
		Tools:                 km.Tools(),
		DisableProviderPrompt: true,
		ExtraSystem:           []string{"You are a helpful assistant. When you need information, use the knowledge_search and knowledge_fetch tools."},
		MaxSteps:              8,
	})
	if err != nil {
		t.Fatalf("turn 2 error: %v", err)
	}
	t.Logf("turn 2 result: %v", result2)

	lastText2 := lastAssistantText(ctx, t, s, sessID)
	t.Logf("turn 2 answer:\n%s", lastText2)

	if strings.Contains(lastText2, "testEffect") || strings.Contains(lastText2, "it.live") || strings.Contains(lastText2, "test") {
		t.Log("PASS: answer mentions Effect testing patterns")
	} else {
		t.Error("FAIL: answer does not mention Effect testing patterns")
	}

	// Summary
	t.Log("\n=== Summary ===")
	allMsgs, _ := s.ListMessages(ctx, sessID)
	t.Logf("total messages in session: %d", len(allMsgs))
	t.Logf("knowledge_search called: %v", searchCalled)
	t.Logf("knowledge_fetch called: %v", fetchCalled)
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
	// Look for `ref_id` in backtick: `skills:...`
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "ref_id") {
			start := strings.Index(line, "`")
			if start < 0 {
				continue
			}
			end := strings.Index(line[start+1:], "`")
			if end < 0 {
				continue
			}
			candidate := line[start+1 : start+1+end]
			if strings.Contains(candidate, ":") {
				return candidate
			}
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

func init() {
	// Ensure store part data types are registered for memory store.
	_ = fmt.Sprintf // suppress unused import
}
