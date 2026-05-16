// Command control is an interactive REPL coding assistant that uses the
// llm-go session loop, builtin file tools, and an in-memory Bleve index of
// skill documentation for knowledge lookup.
//
// REPL mode (default):
//
//	go run ./cmd/control -provider openai -llm-key sk-...
//
// Web mode:
//
//	go run ./cmd/control -web
//	# opens browser automatically; port is chosen dynamically
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	bleve "github.com/blevesearch/bleve/v2"

	"github.com/larryhou/llm-go/auth"
	llmconfig "github.com/larryhou/llm-go/config"
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
	"github.com/larryhou/llm-go/tool/builtin"
)
// ── config ────────────────────────────────────────────────────────────────────

type appConfig struct {
	provider     string
	baseURL      string
	apiKey       string
	modelID      string
	maxSteps     int
	contextLimit int
	skillsDir    string
	web          bool
	debug        bool
}

// timeNano returns current unix nanoseconds; shared with web.go.
func timeNano() int64 { return time.Now().UnixNano() }

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ── provider wrapper — tees events to a local channel ────────────────────────

type replProvider struct {
	inner     llm.Provider
	out       chan llm.Event
	onRequest func(req llm.Request) // called once per Stream() invocation with the input request
}

func (p *replProvider) ID() string { return p.inner.ID() }

func (p *replProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	if p.onRequest != nil {
		p.onRequest(req)
	}
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

// ── index helpers (ported from llm-api) ─────────────────────────────────

// toolPath extracts a display path suffix from tool input arguments.
// Returns a string like " path/to/file" for file tools, or "" for others.
func toolPath(name string, input any) string {
	m, ok := input.(map[string]any)
	if !ok {
		return ""
	}
	switch name {
	case "glob", "grep":
		// show "pattern  [in path]"
		pattern, _ := m["pattern"].(string)
		path, _ := m["path"].(string)
		if pattern == "" {
			return ""
		}
		if path != "" {
			return " " + pattern + " in " + path
		}
		return " " + pattern
	case "read", "write", "edit":
		if v, _ := m["filePath"].(string); v != "" {
			return " " + v
		}
	case "bash":
		if v, _ := m["command"].(string); v != "" {
			return " " + v
		}
	}
	return ""
}

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
	cfg := appConfig{}
	flag.StringVar(&cfg.provider, "provider", envOr("TIMI_PROVIDER", "openai"), "LLM provider: openai or anthropic")
	flag.StringVar(&cfg.baseURL, "llm-url", envOr("TIMI_BASE_URL", "http://192.168.3.119:8080/timi-claude/v1"), "LLM base URL")
	flag.StringVar(&cfg.apiKey, "llm-key", envOr("TIMI_API_KEY", "sk-zzz6FtyLMyuobNNOukwgobP0l1F3TjMO"), "LLM API key")
	flag.StringVar(&cfg.modelID, "model", envOr("TIMI_MODEL", "claude-sonnet-4.6"), "LLM model ID")
	flag.IntVar(&cfg.maxSteps, "max-steps", 20, "max agentic steps per turn")
	flag.IntVar(&cfg.contextLimit, "context-limit", 128000, "context window token limit")
	flag.StringVar(&cfg.skillsDir, "skills", ".opencode", "skills root directory to index")
	flag.BoolVar(&cfg.web, "web", false, "start web UI instead of REPL (port chosen automatically)")
	flag.BoolVar(&cfg.debug, "debug", false, "record each turn to <session-id>/chat-<ts>.json")
	flag.Parse()

	// ── provider ──────────────────────────────────────────────────────────────

	// Build provider registry with factories for all supported providers.
	registry := providerPkg.NewRegistry()
	registry.RegisterFactory(anthropicProv.ProviderID, anthropicProv.Factory)
	registry.RegisterFactory(openaiProv.ProviderID, openaiProv.Factory)

	// Also support the "timi" alias (OpenAI-compatible) used in legacy flags.
	registry.RegisterFactory("timi", func(provCfg *llmconfig.ProviderInfo, a *auth.Store) (llm.Provider, error) {
		return openaiProv.NewFromConfig("timi", nil, provCfg, a)
	})

	// Build per-provider config from CLI flags so legacy flags still work
	// when llm.json has no matching provider section.
	fileCfg, _ := llmconfig.Load()
	authStore, _ := auth.Load()

	// Merge: CLI flags override file config.
	provCfgMap := map[string]*llmconfig.ProviderInfo{}
	if fileCfg != nil {
		for k, v := range fileCfg.Provider {
			provCfgMap[k] = v
		}
	}
	// If CLI flags differ from file config, build an override entry.
	if cfg.apiKey != "" || cfg.baseURL != "" {
		cliProvID := cfg.provider
		if cliProvID == "openai" {
			cliProvID = "timi" // legacy: openai flag means timi proxy
		}
		existing := provCfgMap[cliProvID]
		override := &llmconfig.ProviderInfo{}
		if existing != nil {
			*override = *existing
		}
		if override.Options == nil {
			override.Options = &llmconfig.ProviderOptions{}
		}
		if cfg.apiKey != "" {
			override.Options.APIKey = cfg.apiKey
		}
		if cfg.baseURL != "" {
			if cfg.provider == "anthropic" {
				// anthropic-sdk-go auto-appends /v1/messages; strip /v1 suffix.
				override.API = strings.TrimSuffix(cfg.baseURL, "/v1")
			} else {
				override.API = cfg.baseURL
			}
		}
		provCfgMap[cliProvID] = override
	}

	providerID := cfg.provider
	if providerID == "openai" {
		providerID = "timi"
	} else if providerID == "anthropic" {
		providerID = anthropicProv.ProviderID
	}

	innerProv, err := registry.BuildProvider(providerID, provCfgMap[providerID], authStore)
	if err != nil {
		fmt.Fprintf(os.Stderr, "build provider: %v\n", err)
		os.Exit(1)
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

	// sessionID declared here so it can be used by SessionHistorySource below.
	sessionID := fmt.Sprintf("control-%d", time.Now().UnixNano())

	skillsCount := 0
	skillsAbsDir := cfg.skillsDir
	if !filepath.IsAbs(skillsAbsDir) {
		skillsAbsDir = filepath.Join(cwd, skillsAbsDir)
	}

	// compactionHook and historySrc are set when SessionHistorySource is created.
	var compactionHook knowledge.CompactionHook
	var historySrc *knowledge.SessionHistorySource

	if _, statErr := os.Stat(skillsAbsDir); os.IsNotExist(statErr) {
		fmt.Fprintf(os.Stderr, "[warn] skills directory not found: %s, skipping knowledge index\n", skillsAbsDir)
	} else {
		idx, n, idxErr := buildSkillsIndex(skillsAbsDir)
		if idxErr != nil {
			fmt.Fprintf(os.Stderr, "[warn] failed to build skills index: %v, skipping knowledge index\n", idxErr)
		} else {
			skillsCount = n

			// SessionHistorySource: indexes compacted messages for recall.
			// Priority 0 (highest) — queried before skills index (priority 1).
			var histErr error
			historySrc, histErr = knowledge.NewSessionHistorySource(sessionID, knowledge.DefaultMaxCompactions)
			if histErr != nil {
				fmt.Fprintf(os.Stderr, "[warn] failed to create session history source: %v\n", histErr)
			} else {
				compactionHook = historySrc.Hook()
			}

			km := knowledge.NewManager(knowledge.ManagerConfig{
				SourceTimeout:       10 * time.Second,
				MaxResults:          5,
				SnippetMaxChars:     400,
				ContentMaxChars:     8000,
				AllowPartialFailure: true,
			})
			km.Register(blevesource.New(idx, "skills", 1, &blevesource.Config{
				TitleField:   "title",
				ContentField: "content",
			}))
			if historySrc != nil {
				km.Register(historySrc)
			}
			tools = append(tools, km.Tools()...)
		}
	}

	// ── session store ─────────────────────────────────────────────────────────

	sessionStore := memory.New()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	tool.StartCleanup(ctx)
	if err := sessionStore.CreateSession(ctx, &store.Session{
		ID:    sessionID,
		Model: cfg.provider + "/" + cfg.modelID,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "create session: %v\n", err)
		os.Exit(1)
	}

	// session_reset: control is single-session, no server-level lock needed.
	// historySrc may be nil if the skills directory was not found.
	tools = append(tools, session.NewResetTool(func(resetCtx context.Context) error {
		if err := sessionStore.DeleteSession(resetCtx, sessionID); err != nil {
			return err
		}
		if err := sessionStore.CreateSession(resetCtx, &store.Session{
			ID:    sessionID,
			Model: cfg.provider + "/" + cfg.modelID,
		}); err != nil {
			return err
		}
		if historySrc != nil {
			if err := historySrc.Reset(); err != nil {
				fmt.Fprintf(os.Stderr, "[warn] session history index reset failed: %v\n", err)
			}
		}
		return nil
	}))

	// ── system prompt ─────────────────────────────────────────────────────────

	extraSystem := []string{fmt.Sprintf(
		"You are an interactive coding assistant running in directory: %s\n"+
			"Tool usage priority:\n"+
			"1. Always call knowledge_search first to look up relevant documentation, architecture, and design guides.\n"+
			"2. If the search results are insufficient or no relevant knowledge is found, then use file tools (glob, grep, read, write, edit, bash) to explore the codebase directly.\n"+
			"Never skip the knowledge lookup step when answering questions about the codebase.\n"+
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

	// ── default session config (prune enabled) ────────────────────────────────

	pruneEnabled := true
	sessionCfg := &llmconfig.Info{
		Compaction: &llmconfig.CompactionConfig{
			Prune: &pruneEnabled,
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

	// ── web mode ──────────────────────────────────────────────────────────────

	if cfg.web {
		app := &appState{
			cwd:         cwd,
			tools:       tools,
			extraSystem: extraSystem,
			model:       model,
			prov:        prov,
			cfg:         sessionCfg,
			debug:       cfg.debug,
		}
		if err := runWebServer(app); err != nil {
			fmt.Fprintf(os.Stderr, "web server: %v\n", err)
			os.Exit(1)
		}
		return
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
					fmt.Printf("\n[tool: %s%s]\n", ev.ToolName, toolPath(ev.ToolName, ev.Input))
				case llm.EventStepFinish:
					// nothing — newline will come with EventRequestFinish
				case llm.EventRequestFinish:
					u := ev.Usage
					fmt.Printf("\n[in:%d out:%d total:%d]\n", u.Input, u.Output, u.Effective())
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
			Config:      sessionCfg,
			OnCompact:   compactionHook,
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
