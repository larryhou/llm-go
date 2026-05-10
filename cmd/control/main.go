// Command control is an interactive REPL coding assistant that uses the
// llm-go session loop, builtin file tools, and an in-memory Bleve index of
// skill documentation for knowledge lookup.
//
// Usage:
//
//	go run ./cmd/control \
//	    -provider openai \
//	    -llm-url  http://host/v1 \
//	    -llm-key  sk-... \
//	    -model    claude-sonnet-4.6 \
//	    -skills   .opencode
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	bleve "github.com/blevesearch/bleve/v2"

	"github.com/larryhou/llm-go/knowledge"
	blevesource "github.com/larryhou/llm-go/knowledge/source/bleve"
	"github.com/larryhou/llm-go/llm"
	anthropicProv "github.com/larryhou/llm-go/provider/anthropic"
	openaiProv "github.com/larryhou/llm-go/provider/openai"
	"github.com/larryhou/llm-go/session"
	"github.com/larryhou/llm-go/store"
	"github.com/larryhou/llm-go/store/memory"
	"github.com/larryhou/llm-go/tool"
	"github.com/larryhou/llm-go/tool/builtin"
)

// ── config ────────────────────────────────────────────────────────────────────

type config struct {
	provider     string
	baseURL      string
	apiKey       string
	modelID      string
	maxSteps     int
	contextLimit int
	skillsDir    string
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ── provider wrapper — tees events to a local channel ────────────────────────

type replProvider struct {
	inner llm.Provider
	out   chan llm.Event
}

func (p *replProvider) ID() string { return p.inner.ID() }

func (p *replProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	inner, err := p.inner.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	ch := make(chan llm.Event, 64)
	go func() {
		defer close(ch)
		for ev := range inner {
			// Forward to REPL output handler.
			select {
			case p.out <- ev:
			case <-ctx.Done():
			}
			// Pass through to session processor.
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// ── index helpers (ported from knowledge-api) ─────────────────────────────────

func newSkillsIndex() (bleve.Index, error) {
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
	return bleve.NewMemOnly(mapping)
}

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

func buildSkillsIndex(skillsDir string) (bleve.Index, int, error) {
	idx, err := newSkillsIndex()
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

		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", path, readErr)
		}
		title, body := parseFrontmatter(string(raw))
		if title == "" {
			title = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		}

		if batchErr := batch.Index(rel, map[string]any{
			"title":   title,
			"content": body,
			"skill":   skill,
			"path":    path,
		}); batchErr != nil {
			return batchErr
		}
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

// ── main ──────────────────────────────────────────────────────────────────────

func main() {
	cfg := config{}
	flag.StringVar(&cfg.provider, "provider", envOr("TIMI_PROVIDER", "openai"), "LLM provider: openai or anthropic")
	flag.StringVar(&cfg.baseURL, "llm-url", envOr("TIMI_BASE_URL", "http://192.168.3.119:8080/timi-claude/v1"), "LLM base URL")
	flag.StringVar(&cfg.apiKey, "llm-key", envOr("TIMI_API_KEY", "sk-zzz6FtyLMyuobNNOukwgobP0l1F3TjMO"), "LLM API key")
	flag.StringVar(&cfg.modelID, "model", envOr("TIMI_MODEL", "claude-sonnet-4.6"), "LLM model ID")
	flag.IntVar(&cfg.maxSteps, "max-steps", 20, "max agentic steps per turn")
	flag.IntVar(&cfg.contextLimit, "context-limit", 128000, "context window token limit")
	flag.StringVar(&cfg.skillsDir, "skills", ".opencode", "skills root directory to index")
	flag.Parse()

	// ── provider ──────────────────────────────────────────────────────────────

	var innerProv llm.Provider
	providerID := "timi"
	if cfg.provider == "openai" {
		innerProv = openaiProv.New(cfg.apiKey, cfg.baseURL, "timi", nil)
	} else {
		providerID = "anthropic"
		// anthropic-sdk-go auto-appends /v1/messages; base URL must NOT include /v1.
		baseURL := strings.TrimSuffix(cfg.baseURL, "/v1")
		innerProv = anthropicProv.New(cfg.apiKey, baseURL, map[string]string{
			"Authorization": "Bearer " + cfg.apiKey,
		})
	}

	// Event channel consumed by the REPL printer goroutine.
	evCh := make(chan llm.Event, 128)
	prov := &replProvider{inner: innerProv, out: evCh}

	// ── builtin tools ─────────────────────────────────────────────────────────

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "getwd: %v\n", err)
		os.Exit(1)
	}

	tools := []tool.Tool{
		&builtin.GlobTool{WorkDir: cwd},
		&builtin.GrepTool{WorkDir: cwd},
		&builtin.ReadTool{WorkDir: cwd},
		&builtin.WriteTool{WorkDir: cwd},
		&builtin.EditTool{WorkDir: cwd},
		&builtin.ShellTool{WorkDir: cwd},
	}

	// ── skills index + knowledge tools ───────────────────────────────────────

	skillsCount := 0
	skillsAbsDir := cfg.skillsDir
	if !filepath.IsAbs(skillsAbsDir) {
		skillsAbsDir = filepath.Join(cwd, skillsAbsDir)
	}

	if _, statErr := os.Stat(skillsAbsDir); os.IsNotExist(statErr) {
		fmt.Fprintf(os.Stderr, "[warn] skills directory not found: %s, skipping knowledge index\n", skillsAbsDir)
	} else {
		idx, n, idxErr := buildSkillsIndex(skillsAbsDir)
		if idxErr != nil {
			fmt.Fprintf(os.Stderr, "[warn] failed to build skills index: %v, skipping knowledge index\n", idxErr)
		} else {
			skillsCount = n
			km := knowledge.NewManager(knowledge.ManagerConfig{
				SourceTimeout:       10 * time.Second,
				MaxResults:          5,
				SnippetMaxChars:     400,
				ContentMaxChars:     8000,
				AllowPartialFailure: true,
			})
			km.Register(blevesource.New(idx, "skills", 0, &blevesource.Config{
				TitleField:   "title",
				ContentField: "content",
			}))
			tools = append(tools, km.Tools()...)
		}
	}

	// ── session store ─────────────────────────────────────────────────────────

	sessionStore := memory.New()
	sessionID := fmt.Sprintf("control-%d", time.Now().UnixNano())
	ctx := context.Background()
	if err := sessionStore.CreateSession(ctx, &store.Session{
		ID:    sessionID,
		Model: cfg.provider + "/" + cfg.modelID,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "create session: %v\n", err)
		os.Exit(1)
	}

	// ── system prompt ─────────────────────────────────────────────────────────

	extraSystem := []string{fmt.Sprintf(
		"You are an interactive coding assistant running in directory: %s\n"+
			"You have file tools (glob, grep, read, write, edit, bash) to explore and modify the codebase freely.\n"+
			"You also have knowledge_search and knowledge_fetch to look up skill documentation.\n"+
			"Always work within %s unless explicitly instructed otherwise.",
		cwd, cwd,
	)}

	model := llm.Model{
		ID:         cfg.modelID,
		ProviderID: providerID,
		APIID:      cfg.modelID,
		Limit: llm.ModelLimit{
			Context: cfg.contextLimit,
			Output:  8192,
		},
	}

	// ── startup banner ────────────────────────────────────────────────────────

	fmt.Println("Control — interactive coding assistant")
	fmt.Printf("Working directory: %s\n", cwd)
	if skillsCount > 0 {
		fmt.Printf("Skills indexed: %d documents from %s\n", skillsCount, skillsAbsDir)
	} else {
		fmt.Println("Skills indexed: none")
	}
	fmt.Println("Type 'exit' or Ctrl-D to quit.")
	fmt.Println()

	// ── REPL loop ─────────────────────────────────────────────────────────────

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			// EOF / Ctrl-D
			fmt.Println()
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			break
		}

		// Start printer goroutine before RunLoop so we consume evCh
		// while RunLoop (and its provider goroutines) are running.
		done := make(chan struct{})
		go func() {
			defer close(done)
			for ev := range evCh {
				switch ev.Type {
				case llm.EventTextDelta:
					fmt.Print(ev.Text)
				case llm.EventToolCall:
					fmt.Printf("\n[tool: %s]\n", ev.ToolName)
				case llm.EventStepFinish:
					// nothing — newline will come with EventRequestFinish
				case llm.EventRequestFinish:
					fmt.Println()
				case llm.EventError:
					fmt.Fprintf(os.Stderr, "\n[error] %v\n", ev.Err)
				}
			}
		}()

		_, runErr := session.RunLoop(ctx, sessionStore, session.RunInput{
			SessionID:   sessionID,
			UserMsg:     line,
			Model:       model,
			Provider:    prov,
			Tools:       tools,
			ExtraSystem: extraSystem,
			MaxSteps:    cfg.maxSteps,
		})

		// Signal printer to drain and exit.
		close(evCh)
		<-done
		// Reset channel for next turn.
		evCh = make(chan llm.Event, 128)
		prov.out = evCh

		if runErr != nil {
			fmt.Fprintf(os.Stderr, "[error] %v\n", runErr)
		}
	}
}
