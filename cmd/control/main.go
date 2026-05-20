// Command control is an interactive REPL coding assistant built on agent.Client.
//
// REPL mode (default):
//
//	go run ./cmd/control
//
// Web mode:
//
//	go run ./cmd/control -web
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/larryhou/llm-go/agent"
	"github.com/larryhou/llm-go/llm"
	"github.com/larryhou/llm-go/session"
	"github.com/larryhou/llm-go/tool"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// timeNano is used by web.go.
func timeNano() int64 { return time.Now().UnixNano() }

// toolPath extracts a display suffix from tool input for REPL/SSE output.
func toolPath(name string, input any) string {
	m, ok := input.(map[string]any)
	if !ok {
		return ""
	}
	switch name {
	case "glob", "grep":
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

func main() {
	var (
		providerID   = flag.String("provider", envOr("LLM_PROVIDER", "openai"), "LLM provider")
		baseURL      = flag.String("llm-url", envOr("LLM_BASE_URL", "http://192.168.3.119:8080/timi-claude/v1"), "LLM base URL")
		apiKey       = flag.String("llm-key", envOr("LLM_API_KEY", "sk-zzz6FtyLMyuobNNOukwgobP0l1F3TjMO"), "LLM API key")
		modelID      = flag.String("model", envOr("LLM_MODEL", "claude-sonnet-4.6"), "model ID")
		maxSteps     = flag.Int("max-steps", 20, "max agentic steps per turn")
		contextLimit = flag.Int("context-limit", 128000, "context window token limit")
		skillsDir    = flag.String("skills", ".opencode", "skills root directory")
		storeDSN     = flag.String("store", "sqlite:memory.db", "store DSN: \"memory\" or \"sqlite:<path>\"")
		web          = flag.Bool("web", false, "start web UI instead of REPL")
		debug        = flag.Bool("debug", false, "record turns to debug-<ts>.ndjson")
	)
	flag.Parse()

	// ── agent ─────────────────────────────────────────────────────────────────
	agentCfg := agent.Config{
		ProviderID:   *providerID,
		BaseURL:      *baseURL,
		APIKey:       *apiKey,
		ModelID:      *modelID,
		MaxSteps:     *maxSteps,
		ContextLimit: *contextLimit,
		SkillsDir:    *skillsDir,
	}

	switch {
	case *storeDSN == "" || *storeDSN == "memory":
		// leave nil → memory.New()
	default:
		path, ok := strings.CutPrefix(*storeDSN, "sqlite:")
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown store DSN %q\n", *storeDSN)
			os.Exit(1)
		}
		st, err := agent.OpenSQLiteStore(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open sqlite store: %v\n", err)
			os.Exit(1)
		}
		defer st.Close()
		agentCfg.Store = st
	}

	client, err := agent.New(agentCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent init: %v\n", err)
		os.Exit(1)
	}

	// ── optional debug recording ──────────────────────────────────────────────
	var debugFile string
	if *debug {
		debugFile = fmt.Sprintf("debug-%d.ndjson", time.Now().UnixMilli())
		rec, recErr := llm.NewRecordProvider(client.Provider, debugFile)
		if recErr != nil {
			fmt.Fprintf(os.Stderr, "debug recording: %v\n", recErr)
			os.Exit(1)
		}
		defer rec.Close()
		client.Provider = rec
	}

	// ── startup banner ────────────────────────────────────────────────────────
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "getwd: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Control — interactive coding assistant")
	fmt.Printf("Working directory: %s\n", cwd)
	if debugFile != "" {
		fmt.Printf("Debug recording: %s\n", debugFile)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	tool.StartCleanup(ctx)

	// ── web mode ──────────────────────────────────────────────────────────────
	if *web {
		if err := runWebServer(client); err != nil {
			fmt.Fprintf(os.Stderr, "web server: %v\n", err)
			os.Exit(1)
		}
		return
	}

	fmt.Println("Type 'exit' or Ctrl-D to quit.")
	fmt.Println()

	// ── REPL loop ─────────────────────────────────────────────────────────────
	intSig := make(chan os.Signal, 1)
	signal.Notify(intSig, syscall.SIGINT)
	defer signal.Stop(intSig)

	var activeHandle *session.RunHandle

	sc := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !sc.Scan() {
			fmt.Println()
			break
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			break
		}

		// Create a fresh channel and closure each turn so that a late-arriving
		// event from the completed provider goroutine never sends to a closed
		// channel (which would panic).
		evCh := make(chan llm.Event, 128)
		onEvent := func(ev llm.Event) { evCh <- ev }

		done := make(chan struct{})
		go func() {
			defer close(done)
			for ev := range evCh {
				switch ev.Type {
				case llm.EventTextDelta:
					fmt.Print(ev.Text)
				case llm.EventToolCall:
					fmt.Printf("\n[tool: %s%s]\n", ev.ToolName, toolPath(ev.ToolName, ev.Input))
				case llm.EventRequestFinish:
					u := ev.Usage
					fmt.Printf("\n[in:%d out:%d total:%d]\n", u.Input, u.Output, u.Effective())
				case llm.EventError:
					fmt.Fprintf(os.Stderr, "\n[error] %v\n", ev.Err)
				}
			}
		}()

		h := client.RunAsync(ctx, line, agent.RunOptions{}, onEvent)
		activeHandle = h

	wait:
		for {
			select {
			case <-h.Done:
				break wait
			case <-intSig:
				fmt.Fprintln(os.Stderr, "\n[cancelled]")
				h.Cancel()
				<-h.Done
				break wait
			}
		}
		activeHandle = nil

		close(evCh)
		<-done

		if h.Err != nil && h.Err != context.Canceled {
			fmt.Fprintf(os.Stderr, "[error] %v\n", h.Err)
		}
	}

	if activeHandle != nil {
		activeHandle.Cancel()
		<-activeHandle.Done
	}
}
