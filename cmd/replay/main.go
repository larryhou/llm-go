// Command replay replays a session recording produced by cmd/control -debug.
//
// A recording is a single ndjson file (debug-<ts>.ndjson) where each line is
// one Record — the full LLM request plus the significant response events
// including real token usage. The replay tool drives session.RunLoop with a
// DebugProvider that replays each line in order, no real LLM calls needed.
//
// User messages are extracted from the recorded requests: the last user message
// in each request that is different from the previous step's last user message
// marks the start of a new turn.
//
// Usage:
//
//	go run ./cmd/replay -recording debug-<ts>.ndjson
//	go run ./cmd/replay -recording debug-<ts>.ndjson -verbose
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	llmconfig "github.com/larryhou/llm-go/config"
	"github.com/larryhou/llm-go/llm"
	"github.com/larryhou/llm-go/session"
	"github.com/larryhou/llm-go/store"
	"github.com/larryhou/llm-go/store/memory"
)

func main() {
	recFlag := flag.String("recording", "", "ndjson recording file (required)")
	verbose := flag.Bool("verbose", false, "print post-compaction context after each turn")
	flag.Parse()

	if *recFlag == "" {
		fmt.Fprintln(os.Stderr, "usage: replay -recording <file.ndjson> [-verbose]")
		os.Exit(1)
	}

	// Load all steps from the recording.
	steps, err := loadSteps(*recFlag)
	if err != nil {
		fatalf("load recording: %v", err)
	}
	fmt.Printf("[recording] %s  total_steps=%d\n\n", *recFlag, len(steps))

	// Extract the sequence of turns: group consecutive steps that share the
	// same last-user-message into one turn.
	turns := groupTurns(steps)
	fmt.Printf("[turns] detected %d turns\n", len(turns))
	for i, t := range turns {
		fmt.Printf("  turn %d: %q  (%d steps)\n", i+1, truncate(t.userMsg, 60), len(t.steps))
	}
	fmt.Println()

	// Reconstruct model and config from the first step's request.
	model := steps[0].Request.Model
	if model.Limit.Output == 0 {
		model.Limit.Output = 8192
	}
	if model.Limit.Context == 0 {
		model.Limit.Context = 128000
	}

	// ExtraSystem: take System[1:] from first request (System[0] is provider prompt).
	var extraSystem []string
	if sys := steps[0].Request.System; len(sys) > 1 {
		extraSystem = sys[1:]
	}

	// Session config matching cmd/control: prune enabled.
	pruneEnabled := true
	sessionCfg := &llmconfig.Info{
		Compaction: &llmconfig.CompactionConfig{
			Prune: &pruneEnabled,
		},
	}

	fmt.Printf("Model    : %s  context=%d  output=%d\n", model.ID, model.Limit.Context, model.Limit.Output)
	fmt.Printf("Config   : prune=%v\n\n", pruneEnabled)

	sessID := "replay"
	sessStore := memory.New()
	ctx := context.Background()
	if err := sessStore.CreateSession(ctx, &store.Session{ID: sessID}); err != nil {
		fatalf("create session: %v", err)
	}

	compactionCount := 0
	onCompact := store.CompactionHook(func(head []*store.Message, parts map[string][]*store.Part) {
		compactionCount++
		fmt.Printf("\n╔══ COMPACTION #%d ══════════════════════════════════════\n", compactionCount)
		fmt.Printf("║  head: %d messages\n", len(head))
		for _, m := range head {
			if m.Role != store.RoleUser {
				continue
			}
			ps := parts[m.ID]
			if hasPartType(ps, store.PartTypeCompaction) {
				continue
			}
			txt := firstTextPart(ps)
			fmt.Printf("║    user: %s\n", truncate(txt, 80))
		}
		fmt.Printf("╚═══════════════════════════════════════════════════════\n")
	})

	// Replay each turn using a ReplayProvider scoped to that turn's steps.
	for i, turn := range turns {
		fmt.Printf("┌─ Turn %d: %q\n", i+1, truncate(turn.userMsg, 70))

		prov := llm.NewReplayProviderFromRecords(turn.steps)

		_, runErr := session.RunLoop(ctx, sessStore, session.RunInput{
			SessionID:   sessID,
			UserMsg:     turn.userMsg,
			Model:       model,
			Provider:    prov,
			ExtraSystem: extraSystem,
			MaxSteps:    20,
			Config:      sessionCfg,
			OnCompact:   onCompact,
		})

		msgs, _ := sessStore.ListMessages(ctx, sessID)
		allParts := loadAllParts(ctx, sessStore, msgs)
		filtered := session.FilterCompacted(msgs, allParts)

		cb, rc := countBoundaryParts(msgs, allParts)
		fmt.Printf("│  store: total=%d filtered=%d boundaries=%d rc_parts=%d\n",
			len(msgs), len(filtered), cb, rc)

		if *verbose {
			for j, m := range filtered {
				ps := allParts[m.ID]
				txt := firstTextPart(ps)
				fmt.Printf("│  [%2d] %-9s %s\n", j, m.Role, truncate(txt, 70))
			}
		}

		if runErr != nil {
			fmt.Printf("│  error: %v\n", runErr)
		}
		fmt.Printf("└─ turn %d done  (consumed %d/%d steps)\n\n", i+1, int(prov.Len()), len(turn.steps))
	}

	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Printf("Total turns      : %d\n", len(turns))
	fmt.Printf("Total compactions: %d\n", compactionCount)

	// Print RC excerpts.
	msgs, _ := sessStore.ListMessages(ctx, sessID)
	allParts := loadAllParts(ctx, sessStore, msgs)
	fmt.Println("\nRecent-context excerpts:")
	for _, m := range msgs {
		for _, p := range allParts[m.ID] {
			if p.Type == store.PartTypeRecentContext {
				if d, ok := store.DataAs[*store.RecentContextPartData](p); ok {
					fmt.Printf("  boundary %s:\n%s\n\n", m.ID[:8], indent(d.Excerpt, "    "))
				}
			}
		}
	}
}

// ── turn grouping ─────────────────────────────────────────────────────────────

type turnGroup struct {
	userMsg string
	steps   []llm.Record
}

// groupTurns groups steps by last-user-message: a new turn begins whenever the
// last user message in a step's request changes from the previous step.
//
// Two special cases both merge into the current turn rather than starting a new one:
//  1. No user message at all in the request.
//  2. The last user message is a compaction summary prompt (starts with
//     session.SummaryTemplate prefix) — this step is the summary-generation
//     LLM call issued internally by Compact() and must be fed to the same
//     ReplayProvider as the turn that triggered compaction.
func groupTurns(steps []llm.Record) []turnGroup {
	var turns []turnGroup
	var cur *turnGroup
	prevKey := ""

	for _, s := range steps {
		key := lastUserMsg(s.Request.Messages)
		if key == "" || isSummaryStep(key) {
			// No user message or compaction summary call — belongs to current turn.
			if cur != nil {
				cur.steps = append(cur.steps, s)
			}
			continue
		}
		if key != prevKey {
			turns = append(turns, turnGroup{userMsg: key})
			cur = &turns[len(turns)-1]
			prevKey = key
		}
		cur.steps = append(cur.steps, s)
	}
	return turns
}

// isSummaryStep reports whether the last-user-message key is a compaction
// summary prompt generated internally by session.Compact(). These steps must
// stay in the same turn as the one that triggered compaction.
func isSummaryStep(key string) bool {
	return strings.HasPrefix(key, session.SummaryTemplate[:60])
}

// lastUserMsg returns the text of the last user message in msgs.
func lastUserMsg(msgs []llm.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != llm.RoleUser {
			continue
		}
		for _, cp := range msgs[i].Content {
			if cp.Type == llm.PartTypeText && cp.Text != "" {
				return cp.Text
			}
		}
	}
	return ""
}

// ── helpers ───────────────────────────────────────────────────────────────────

func loadSteps(path string) ([]llm.Record, error) {
	p, err := llm.NewReplayProvider(path)
	if err != nil {
		return nil, err
	}
	return p.Records(), nil
}

func loadAllParts(ctx context.Context, s store.Store, msgs []*store.Message) map[string][]*store.Part {
	ap := make(map[string][]*store.Part, len(msgs))
	for _, m := range msgs {
		ps, _ := s.ListParts(ctx, m.ID)
		ap[m.ID] = ps
	}
	return ap
}

func countBoundaryParts(msgs []*store.Message, allParts map[string][]*store.Part) (cb, rc int) {
	for _, m := range msgs {
		for _, p := range allParts[m.ID] {
			if p.Type == store.PartTypeCompaction {
				cb++
			}
			if p.Type == store.PartTypeRecentContext {
				rc++
			}
		}
	}
	return
}

func hasPartType(ps []*store.Part, t string) bool {
	for _, p := range ps {
		if p.Type == t {
			return true
		}
	}
	return false
}

func firstTextPart(ps []*store.Part) string {
	for _, p := range ps {
		if p.Type == store.PartTypeText {
			if d, ok := store.DataAs[*store.TextPartData](p); ok && d.Text != "" {
				return d.Text
			}
		}
		if p.Type == store.PartTypeCompaction {
			return "[compaction boundary]"
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func indent(s, prefix string) string {
	scanner := bufio.NewScanner(strings.NewReader(s))
	var b strings.Builder
	for scanner.Scan() {
		b.WriteString(prefix + scanner.Text() + "\n")
	}
	return b.String()
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "replay: "+format+"\n", args...)
	os.Exit(1)
}
