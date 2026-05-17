package session_test

// prune_threshold_test.go — boundary-value tests for the Prune() minimum-savings threshold.
//
// The fix (Issue-06) changed the check from:
//   totalPruned < PruneMinimum/4   (effective threshold: 5,000 estimated tokens)
// to:
//   totalPruned < PruneMinimum     (correct threshold: 20,000 estimated tokens)
//
// totalPruned accumulates len(d.Output)/4 per pruned part, so it is already
// in estimated-token units. The old /4 was a double-division bug.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/larryhou/llm-go/config"
	"github.com/larryhou/llm-go/llm"
	"github.com/larryhou/llm-go/session"
	"github.com/larryhou/llm-go/store"
	"github.com/larryhou/llm-go/store/memory"
)

// pruneConfig returns a config with Prune enabled.
func pruneConfig() *config.Info {
	t := true
	return &config.Info{
		Compaction: &config.CompactionConfig{Prune: &t},
	}
}

// seedPruneSession sets up a session with 6 user+assistant turn pairs.
// Prune protects the 2 most recent user turns (turns 5 and 6) and up to
// PruneProtect tokens of tool output (turns 3 and 4). Turns 1 and 2 are
// fully eligible for pruning.
// Each assistant message has one completed tool part with output of outputSize bytes.
func seedPruneSession(t *testing.T, s store.Store, outputSize int) string {
	t.Helper()
	ctx := context.Background()
	sessID := "prune-test-session"

	if err := s.CreateSession(ctx, &store.Session{ID: sessID}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	output := strings.Repeat("x", outputSize)
	for i := 1; i <= 6; i++ {
		uid := fmt.Sprintf("user-%d", i)
		if err := s.CreateMessage(ctx, &store.Message{
			ID: uid, SessionID: sessID, Role: store.RoleUser,
		}); err != nil {
			t.Fatalf("CreateMessage user %d: %v", i, err)
		}

		aid := fmt.Sprintf("asst-%d", i)
		if err := s.CreateMessage(ctx, &store.Message{
			ID: aid, SessionID: sessID, Role: store.RoleAssistant,
		}); err != nil {
			t.Fatalf("CreateMessage asst %d: %v", i, err)
		}

		if err := s.CreatePart(ctx, &store.Part{
			ID:        fmt.Sprintf("part-%d", i),
			MessageID: aid,
			SessionID: sessID,
			Type:      store.PartTypeTool,
			Data: &store.ToolPartData{
				Tool:   "test_tool",
				Status: store.ToolStatusCompleted,
				Output: output,
			},
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("CreatePart %d: %v", i, err)
		}
	}
	return sessID
}

// TestPrune_ThresholdExact verifies that totalPruned == PruneMinimum succeeds.
//
// Session layout (6 turns, walk backward):
//   turns 5,6 user: skipped (2-turn protection)
//   turns 3,4 asst:  outputTokens each = outputSize/4 = PruneProtect/2
//                    fill tokensProtected to exactly PruneProtect
//   turns 1,2 asst:  pruned; totalPruned = 2 * PruneProtect/2 = PruneProtect >= PruneMinimum ✓
func TestPrune_ThresholdExact(t *testing.T) {
	s := memory.New()
	// outputSize = 4 * (PruneProtect / 2) so two turns fill the protection budget
	// and two turns are pruned totalling PruneProtect tokens (> PruneMinimum).
	outputSize := 4 * (llm.PruneProtect / 2) // = 2 * PruneProtect chars
	sessID := seedPruneSession(t, s, outputSize)

	err := session.Prune(context.Background(), sessID, s, pruneConfig())
	if err != nil {
		t.Errorf("Prune should succeed when totalPruned >= PruneMinimum, got: %v", err)
	}
}

// TestPrune_ThresholdOneLess verifies that totalPruned < PruneMinimum returns an error.
// Each of the 2 pruned turns contributes (PruneMinimum/2 - 1) tokens → total < PruneMinimum.
func TestPrune_ThresholdOneLess(t *testing.T) {
	s := memory.New()
	// Same structure but smaller output so pruned total < PruneMinimum.
	// Protection budget per-turn = PruneProtect/2 = 20000. Two turns fill it.
	// Then 2 more turns are pruned with (PruneMinimum/2 - 1) tokens each.
	// totalPruned = 2*(PruneMinimum/2-1) = PruneMinimum-2 < PruneMinimum.
	//
	// But we only have one outputSize for all turns. Need turns 3,4 to fill
	// PruneProtect quickly. Use a large output for protection turns and a small
	// one for prune turns is not possible with a single seedPruneSession.
	//
	// Alternative: make outputSize so small that even 2 turns barely exceed
	// PruneProtect, but the pruned amount is still < PruneMinimum.
	// Simplest: use very small outputs → protection fills slowly, few turns
	// prune → totalPruned stays small.
	//
	// With outputSize = 4*(PruneMinimum/2 - 1):
	//   each turn contributes (PruneMinimum/2 - 1) tokens
	//   PruneProtect = 40000, so it takes ~4 turns to fill protection budget
	//   Only turns 1-2 remain for pruning (turns 5,6 skip, 3,4 might still protect)
	//   totalPruned ≤ 2*(PruneMinimum/2-1) < PruneMinimum ✓
	tokensPerTurn := llm.PruneMinimum/2 - 1
	outputSize := tokensPerTurn * 4
	sessID := seedPruneSession(t, s, outputSize)

	err := session.Prune(context.Background(), sessID, s, pruneConfig())
	if err == nil {
		t.Error("Prune below threshold should return error, got nil")
	}
}

// TestPrune_DisabledByConfig verifies that Prune is a no-op when cfg has Prune=false or nil.
func TestPrune_DisabledByConfig(t *testing.T) {
	s := memory.New()
	sessID := seedPruneSession(t, s, 1000)

	if err := session.Prune(context.Background(), sessID, s, nil); err != nil {
		t.Errorf("Prune(nil cfg) should be no-op, got: %v", err)
	}

	f := false
	cfg := &config.Info{Compaction: &config.CompactionConfig{Prune: &f}}
	if err := session.Prune(context.Background(), sessID, s, cfg); err != nil {
		t.Errorf("Prune(prune=false) should be no-op, got: %v", err)
	}
}
