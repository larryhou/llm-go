// Package agent provides a ready-to-run coding-assistant Client.
//
// New() wires everything a coding session needs by default:
//   - LLM provider (read from env / llm.json, or injected)
//   - Session store (memory by default, SQLite if Config.Store is set)
//   - Builtin file tools: glob, grep, read, write, edit, bash (scoped to WorkDir)
//   - SessionHistorySource wired as compaction hook
//   - Knowledge manager with the history source + optional Bleve skills index
//   - Session-reset tool
//   - Coding-assistant system prompt
//
// Every default can be replaced or extended through Config fields.
//
// Minimal usage:
//
//	client, err := agent.New(agent.Config{})   // reads LLM_* env vars
//	if err != nil { log.Fatal(err) }
//	err = client.Run(ctx, "Explain this codebase", func(ev llm.Event) {
//	    if ev.Type == llm.EventTextDelta { fmt.Print(ev.Text) }
//	})
//
// Extending with extra tools or knowledge sources:
//
//	client, err := agent.New(agent.Config{
//	    ExtraTools:   []tool.Tool{myCustomTool},
//	    ExtraSources: []knowledge.Source{myWebSource},
//	})
package agent

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	bleve "github.com/blevesearch/bleve/v2"

	"github.com/larryhou/llm-go/auth"
	llmconfig "github.com/larryhou/llm-go/config"
	"github.com/larryhou/llm-go/knowledge"
	blevesource "github.com/larryhou/llm-go/knowledge/source/bleve"
	"github.com/larryhou/llm-go/llm"
	providerPkg "github.com/larryhou/llm-go/provider"
	anthropicProv "github.com/larryhou/llm-go/provider/anthropic"
	openaiProv "github.com/larryhou/llm-go/provider/openai"
	"github.com/larryhou/llm-go/session"
	"github.com/larryhou/llm-go/store"
	"github.com/larryhou/llm-go/store/memory"
	sqlitestore "github.com/larryhou/llm-go/store/sqlite"
	"github.com/larryhou/llm-go/tool"
	"github.com/larryhou/llm-go/tool/builtin"
)

// Config holds all options for creating a Client.
// Every field is optional; zero values produce the opinionated defaults
// described in the field comments.
type Config struct {
	// ── provider ─────────────────────────────────────────────────────────────
	// Falls back to LLM_PROVIDER / LLM_BASE_URL / LLM_API_KEY / LLM_MODEL env
	// vars, then to llm.json config.
	ProviderID string // "openai" | "anthropic"; default: LLM_PROVIDER or "openai"
	BaseURL    string // LLM API base URL
	APIKey     string // LLM API key
	ModelID    string // model ID; default: LLM_MODEL or "claude-sonnet-4.6"

	// Provider injects a pre-built llm.Provider, skipping registry construction.
	Provider llm.Provider

	// ── model limits ──────────────────────────────────────────────────────────
	MaxSteps     int // default: 20
	ContextLimit int // default: 128000

	// ── session ───────────────────────────────────────────────────────────────
	// SessionID: default "agent-"+sha256(WorkDir)[:8]
	SessionID string
	// Store: default memory.New(). Use OpenSQLiteStore for persistence.
	Store store.Store

	// ── workspace ─────────────────────────────────────────────────────────────
	// WorkDir scopes builtin file tools and derives the default SessionID.
	// Default: os.Getwd()
	WorkDir string

	// ── skills knowledge source ───────────────────────────────────────────────
	// SkillsDir is walked for *.md files to build a Bleve skills index.
	// Default: "<WorkDir>/.opencode". Set to "-" to skip.
	SkillsDir string

	// ── extension points ──────────────────────────────────────────────────────
	// ExtraTools are appended after builtin + knowledge tools.
	ExtraTools []tool.Tool
	// ExtraSources are registered with the knowledge manager (after history src).
	ExtraSources []knowledge.Source
	// ExtraSystem is appended after the built-in coding-assistant prompt.
	ExtraSystem []string

	// ── opt-outs ──────────────────────────────────────────────────────────────
	// NoBuiltinTools disables the six builtin file tools.
	NoBuiltinTools bool
	// NoResetTool disables the session_reset tool.
	NoResetTool bool
	// NoSkillsIndex disables Bleve skills index even if SkillsDir exists.
	NoSkillsIndex bool
}

// RunOptions carries the per-turn parameters passed to RunAsync / Run / RunChan.
// WaitFor is the only field that changes between turns; the rest are normally
// fixed from Client.opts.
type RunOptions struct {
	Tools       []tool.Tool
	ExtraSystem []string
	OnCompact   store.CompactionHook
	// WaitFor is awaited before loading messages; pass prev.StoreDone to
	// pipeline turns without blocking the caller.
	WaitFor <-chan struct{}
}

// Client is a fully initialised, ready-to-run agent session.
type Client struct {
	// Store is the session store. Available for direct inspection or reset.
	Store store.Store
	// SessionID identifies this session in the store.
	SessionID string
	// Model is the LLM model used for all turns.
	Model llm.Model
	// Provider is the raw (un-hooked) LLM provider. May be replaced after New()
	// returns (e.g. wrapped with llm.NewRecordProvider) but MUST NOT be mutated
	// concurrently with or after the first RunAsync call.
	Provider llm.Provider
	// HistorySrc is the SessionHistorySource for this session.
	// It is already registered with the knowledge manager and wired as the
	// compaction hook; exposed here for callers that need direct access
	// (e.g. to call RollbackTo after a manual SoftReset).
	HistorySrc *store.SessionHistorySource

	opts     RunOptions // pre-built default run options
	cfg      *llmconfig.Info
	maxSteps int
}

// New constructs a Client with sensible defaults. See Config for what each
// field controls. New() never returns a partially-initialised Client; any
// error means no session was created.
func New(cfg Config) (*Client, error) {
	// ── working directory ─────────────────────────────────────────────────────
	workDir := cfg.WorkDir
	if workDir == "" {
		var err error
		workDir, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("agent: getwd: %w", err)
		}
	}

	// ── provider config ───────────────────────────────────────────────────────
	providerID := cfg.ProviderID
	if providerID == "" {
		providerID = envOr("LLM_PROVIDER", "openai")
	}
	modelID := cfg.ModelID
	if modelID == "" {
		modelID = envOr("LLM_MODEL", "claude-sonnet-4.6")
	}
	maxSteps := cfg.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 20
	}
	contextLimit := cfg.ContextLimit
	if contextLimit <= 0 {
		contextLimit = 128_000
	}

	// ── provider ──────────────────────────────────────────────────────────────
	var prov llm.Provider
	if cfg.Provider != nil {
		prov = cfg.Provider
	} else {
		var err error
		prov, err = buildProvider(providerID, cfg.BaseURL, cfg.APIKey)
		if err != nil {
			return nil, fmt.Errorf("agent: build provider: %w", err)
		}
	}
	canonicalProvID := resolveProviderID(providerID)

	// ── store ─────────────────────────────────────────────────────────────────
	sessionStore := cfg.Store
	if sessionStore == nil {
		sessionStore = memory.New()
	}

	// ── session ID ────────────────────────────────────────────────────────────
	sessionID := cfg.SessionID
	if sessionID == "" {
		h := sha256.Sum256([]byte(workDir))
		sessionID = "agent-" + hex.EncodeToString(h[:8])
	}

	// ── session history source ────────────────────────────────────────────────
	var ps store.PersistStore
	if p, ok := sessionStore.(store.PersistStore); ok {
		ps = p
	}
	historySrc, err := store.NewSessionHistorySource(
		sessionID,
		store.DefaultMaxCompactions,
		store.DefaultMaxIndexedSeqs,
		ps,
	)
	if err != nil {
		return nil, fmt.Errorf("agent: session history source: %w", err)
	}

	// ── knowledge manager ─────────────────────────────────────────────────────
	km := knowledge.NewManager(knowledge.ManagerConfig{
		SourceTimeout:       10 * time.Second,
		MaxResults:          5,
		SnippetMaxChars:     400,
		ContentMaxChars:     8000,
		AllowPartialFailure: true,
	})
	km.Register(historySrc) // priority 0

	if !cfg.NoSkillsIndex {
		skillsDir := cfg.SkillsDir
		if skillsDir == "" {
			skillsDir = filepath.Join(workDir, ".opencode")
		} else if skillsDir != "-" && !filepath.IsAbs(skillsDir) {
			skillsDir = filepath.Join(workDir, skillsDir)
		}
		if skillsDir != "-" {
			if _, statErr := os.Stat(skillsDir); statErr == nil {
				idx, _, idxErr := buildSkillsIndex(skillsDir)
				if idxErr == nil {
					km.Register(blevesource.New(idx, "skills", 1, &blevesource.Config{
						TitleField:   "title",
						ContentField: "content",
					}))
				} else {
					fmt.Fprintf(os.Stderr, "[agent] warn: skills index %s: %v\n", skillsDir, idxErr)
				}
			}
		}
	}

	for _, s := range cfg.ExtraSources {
		km.Register(s)
	}

	// ── tools ─────────────────────────────────────────────────────────────────
	var tools []tool.Tool

	if !cfg.NoBuiltinTools {
		tools = append(tools,
			&builtin.GlobTool{WorkDir: workDir},
			&builtin.GrepTool{WorkDir: workDir},
			&builtin.ReadTool{WorkDir: workDir},
			&builtin.WriteTool{WorkDir: workDir},
			&builtin.EditTool{WorkDir: workDir},
			&builtin.ShellTool{WorkDir: workDir},
		)
	}

	tools = append(tools, km.Tools()...)
	tools = append(tools, cfg.ExtraTools...)

	// ── session reset tool ────────────────────────────────────────────────────
	if !cfg.NoResetTool {
		tools = append(tools, session.NewResetTool(
			func(resetCtx context.Context, fresh bool) error {
				deletedIDs, err := session.SoftReset(resetCtx, sessionID, sessionStore, fresh)
				if err != nil {
					return err
				}
				historySrc.RollbackTo(resetCtx, deletedIDs)
				return nil
			},
			func(resetCtx context.Context) error {
				if err := sessionStore.DeleteSession(resetCtx, sessionID); err != nil {
					return err
				}
				if err := sessionStore.CreateSession(resetCtx, &store.Session{
					ID:    sessionID,
					Model: canonicalProvID + "/" + modelID,
				}); err != nil {
					return err
				}
				return historySrc.Reset()
			},
		))
	}

	// ── model + compaction config ─────────────────────────────────────────────
	model := llm.Model{
		ID:         modelID,
		ProviderID: canonicalProvID,
		APIID:      modelID,
		Limit: llm.ModelLimit{
			Context: contextLimit,
			Output:  8192,
		},
	}
	pruneEnabled := true
	sessionCfg := &llmconfig.Info{
		Compaction: &llmconfig.CompactionConfig{
			Prune: &pruneEnabled,
		},
	}

	// ── system prompt ─────────────────────────────────────────────────────────
	systemPrompt := fmt.Sprintf(
		"You are an interactive coding assistant running in directory: %s\n"+
			"Tool usage priority:\n"+
			"1. Always call knowledge_search first to look up relevant documentation, architecture, and design guides.\n"+
			"2. If the search results are insufficient or no relevant knowledge is found, use file tools (glob, grep, read, write, edit, bash) to explore the codebase directly.\n"+
			"Never skip the knowledge lookup step when answering questions about the codebase.\n"+
			"Always work within %s unless explicitly instructed otherwise.",
		workDir, workDir,
	)
	extraSystem := append([]string{systemPrompt}, cfg.ExtraSystem...)

	// ── ensure session exists ─────────────────────────────────────────────────
	ensureCtx := context.Background()
	if _, getErr := sessionStore.GetSession(ensureCtx, sessionID); getErr != nil {
		if createErr := sessionStore.CreateSession(ensureCtx, &store.Session{
			ID:    sessionID,
			Model: canonicalProvID + "/" + modelID,
		}); createErr != nil {
			return nil, fmt.Errorf("agent: create session: %w", createErr)
		}
	}

	return &Client{
		Store:      sessionStore,
		SessionID:  sessionID,
		Model:      model,
		Provider:   prov,
		HistorySrc: historySrc,
		opts: RunOptions{
			Tools:       tools,
			ExtraSystem: extraSystem,
			OnCompact:   historySrc.Hook(),
		},
		cfg:      sessionCfg,
		maxSteps: maxSteps,
	}, nil
}

// ── hookProvider — per-turn event tee ────────────────────────────────────────

// hookProvider wraps an inner Provider and fans every streamed event to both
// the session loop (ch) and an observer function (onEvent).
//
// Design invariants:
//   - ch delivery is never dropped; the session loop depends on it.
//   - onEvent delivery is best-effort: events are dropped when obsCh is full
//     rather than stalling the session loop.
//   - ch is closed only after the observer goroutine has fully exited, i.e.
//     after the last possible call to onEvent. Because the session loop's
//     h.Done fires after ch is closed, callers can safely close any channel
//     passed to onEvent once h.Done is received.
type hookProvider struct {
	inner   llm.Provider
	onEvent func(llm.Event)
}

func newHookProvider(inner llm.Provider, on func(llm.Event)) *hookProvider {
	return &hookProvider{inner: inner, onEvent: on}
}

func (p *hookProvider) ID() string { return p.inner.ID() }

func (p *hookProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	inner, err := p.inner.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	ch := make(chan llm.Event, 64)
	// observerDone is allocated per Stream call so that multi-step turns
	// (where Stream is called once per agentic step) each get an independent
	// lifecycle. A shared struct field would be closed by the first step and
	// panic on close in subsequent steps.
	observerDone := make(chan struct{})
	// obsCh decouples the observer from the session-loop delivery path.
	// The fan goroutine non-blocking-writes into obsCh so a slow observer
	// never stalls the session loop. Excess events are dropped silently.
	obsCh := make(chan llm.Event, 128)
	go func() {
		defer close(observerDone) // signal: no more onEvent calls after this
		for ev := range obsCh {
			p.onEvent(ev)
		}
	}()
	go func() {
		// Drain inner and forward to session loop + observer.
		for ev := range inner {
			ch <- ev // must not be dropped; session loop depends on this
			select {
			case obsCh <- ev: // non-blocking: drop if observer is busy
			default:
			}
		}
		// Close obsCh first so the observer goroutine drains and exits,
		// then close ch. This guarantees that when ch is closed (and thus
		// when h.Done fires), onEvent will never be called again — making it
		// safe for callers to close the channel they passed to onEvent.
		close(obsCh)
		<-observerDone
		close(ch)
	}()
	return ch, nil
}

// ── public API ────────────────────────────────────────────────────────────────

// RunAsync starts the agentic loop in a background goroutine and returns a
// *session.RunHandle immediately.
//
// on, if non-nil, is called for each llm.Event in delivery order. It runs in
// a dedicated goroutine. Events may be dropped if on cannot keep up (the
// session loop is never stalled). When h.Done is closed, on will not be called
// again, so it is safe to close any channel passed to on at that point.
//
// opts overrides the default RunOptions built during New(); pass a zero value
// to use all defaults.
func (c *Client) RunAsync(ctx context.Context, userMsg string, opts RunOptions, on func(llm.Event)) *session.RunHandle {
	merged := c.mergeOpts(opts)
	prov := llm.Provider(c.Provider)
	if on != nil {
		prov = newHookProvider(c.Provider, on)
	}
	return session.RunLoopAsync(ctx, c.Store, session.RunInput{
		SessionID:   c.SessionID,
		UserMsg:     userMsg,
		Model:       c.Model,
		Provider:    prov,
		Tools:       merged.Tools,
		ExtraSystem: merged.ExtraSystem,
		MaxSteps:    c.maxSteps,
		Config:      c.cfg,
		OnCompact:   merged.OnCompact,
		WaitFor:     merged.WaitFor,
	})
}

// Run is the blocking variant of RunAsync.
// Returns nil on success, context.Canceled if cancelled.
func (c *Client) Run(ctx context.Context, userMsg string, opts RunOptions, on func(llm.Event)) error {
	h := c.RunAsync(ctx, userMsg, opts, on)
	<-h.Done
	return h.Err
}

// RunChan returns a channel that receives every llm.Event in order, closed
// when the turn finishes. The caller MUST drain it (or select on ctx.Done).
// Run errors appear as EventError events on the channel, not as a return value.
func (c *Client) RunChan(ctx context.Context, userMsg string, opts RunOptions) <-chan llm.Event {
	ch := make(chan llm.Event, 128)
	on := func(ev llm.Event) {
		select {
		case ch <- ev:
		case <-ctx.Done():
		}
	}
	h := c.RunAsync(ctx, userMsg, opts, on)
	go func() {
		<-h.Done
		close(ch)
	}()
	return ch
}

// DefaultRunOptions returns a copy of the RunOptions built by New().
// The Tools and ExtraSystem slices are shallow-copied so that the caller can
// safely append to them without affecting the Client's internal defaults.
func (c *Client) DefaultRunOptions() RunOptions {
	opts := c.opts
	opts.Tools = append([]tool.Tool(nil), c.opts.Tools...)
	opts.ExtraSystem = append([]string(nil), c.opts.ExtraSystem...)
	return opts
}

// mergeOpts fills zero fields from opts with the Client's defaults.
func (c *Client) mergeOpts(opts RunOptions) RunOptions {
	if opts.Tools == nil {
		opts.Tools = c.opts.Tools
	}
	if opts.ExtraSystem == nil {
		opts.ExtraSystem = c.opts.ExtraSystem
	}
	if opts.OnCompact == nil {
		opts.OnCompact = c.opts.OnCompact
	}
	return opts
}

// ── helpers ───────────────────────────────────────────────────────────────────

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func resolveProviderID(id string) string {
	switch id {
	case "openai":
		return "timi"
	case "anthropic":
		return anthropicProv.ProviderID
	default:
		return id
	}
}

func buildProvider(providerID, baseURL, apiKey string) (llm.Provider, error) {
	registry := providerPkg.NewRegistry()
	registry.RegisterFactory(anthropicProv.ProviderID, anthropicProv.Factory)
	registry.RegisterFactory(openaiProv.ProviderID, openaiProv.Factory)
	registry.RegisterFactory("timi", func(provCfg *llmconfig.ProviderInfo, a *auth.Store) (llm.Provider, error) {
		return openaiProv.NewFromConfig("timi", nil, provCfg, a)
	})

	fileCfg, _ := llmconfig.Load()
	authStore, _ := auth.Load()

	provCfgMap := map[string]*llmconfig.ProviderInfo{}
	if fileCfg != nil {
		for k, v := range fileCfg.Provider {
			provCfgMap[k] = v
		}
	}

	canonicalID := resolveProviderID(providerID)
	if apiKey != "" || baseURL != "" {
		existing := provCfgMap[canonicalID]
		override := &llmconfig.ProviderInfo{}
		if existing != nil {
			*override = *existing
		}
		if override.Options == nil {
			override.Options = &llmconfig.ProviderOptions{}
		}
		if apiKey != "" {
			override.Options.APIKey = apiKey
		}
		if baseURL != "" {
			if providerID == "anthropic" {
				override.API = strings.TrimSuffix(baseURL, "/v1")
			} else {
				override.API = baseURL
			}
		}
		provCfgMap[canonicalID] = override
	}

	return registry.BuildProvider(canonicalID, provCfgMap[canonicalID], authStore)
}

// buildSkillsIndex walks skillsDir and indexes all *.md files into Bleve.
func buildSkillsIndex(skillsDir string) (bleve.Index, int, error) {
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
		return nil, 0, fmt.Errorf("create skills index: %w", err)
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
			"title": title, "content": body, "skill": skill, "path": path,
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
		return nil, 0, fmt.Errorf("flush skills batch: %w", err)
	}
	return idx, count, nil
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
	sc := bufio.NewScanner(strings.NewReader(rest[:idx]))
	for sc.Scan() {
		if after, ok := strings.CutPrefix(sc.Text(), "name:"); ok {
			name = strings.TrimSpace(after)
			break
		}
	}
	return name, body
}

// Store is a session store that can also be closed. Returned by OpenSQLiteStore.
type Store interface {
	store.Store
	Close() error
}

// OpenSQLiteStore opens (or creates) a SQLite-backed session store at path.
// The returned Store must be closed when no longer needed (defer st.Close()).
func OpenSQLiteStore(path string) (Store, error) {
	return sqlitestore.Open(path)
}
