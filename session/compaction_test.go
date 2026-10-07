package session

// compaction_test.go — white-box unit tests for the new compaction mechanisms:
//   - splitTurn(): intra-turn partial retention
//   - Select(): splitTurn path integration, recentHead correctness

import (
	"testing"

	"github.com/larryhou/llm-go/config"
	"github.com/larryhou/llm-go/llm"
	"github.com/larryhou/llm-go/store"
)

// helpers -------------------------------------------------------------------

func makeMsg(id, role string, tokens int) *store.Message {
	m := &store.Message{ID: id, Role: role}
	if tokens > 0 {
		m.Tokens = store.TokenSummary{Input: tokens}
	}
	return m
}

func makeUserMsg(id string, tokens int) *store.Message {
	return makeMsg(id, store.RoleUser, tokens)
}

func makeAssistantMsg(id string, tokens int) *store.Message {
	return makeMsg(id, store.RoleAssistant, tokens)
}

// noParts returns an empty allParts map (no compaction boundaries).
func noParts(msgs []*store.Message) map[string][]*store.Part {
	m := make(map[string][]*store.Part, len(msgs))
	for _, msg := range msgs {
		m[msg.ID] = nil
	}
	return m
}

// modelWithContext returns a model with a context window just large enough
// to make budgets predictable in tests.
func modelWithBudget(budget int) llm.Model {
	// PreserveRecentBudget = clamp(usable*0.25, 2000, 15000)
	// We want the budget to equal exactly `budget`, so set usable*0.25 = budget,
	// i.e. usable = budget*4, context = usable + maxOutput.
	// Keep output=1000 so reserved=min(20000,1000)=1000.
	// usable = context - 1000 = budget*4 → context = budget*4 + 1000.
	// BUT clamp kicks in at 2000 min. For budget<2000 we use cfg override instead.
	return llm.Model{Limit: llm.ModelLimit{Context: budget*4 + 1000, Output: 1000}}
}

func cfgWithBudget(budget int) *config.Info {
	return &config.Info{Compaction: &config.CompactionConfig{
		PreserveRecentTokens: &budget,
	}}
}

// ---- splitTurn ------------------------------------------------------------

func TestSplitTurn_zeroOrNegativeBudget(t *testing.T) {
	msgs := []*store.Message{
		makeUserMsg("u1", 500),
		makeAssistantMsg("a1", 500),
		makeAssistantMsg("a2", 500),
	}
	turn := Turn{StartIdx: 0, EndIdx: 3}
	if got := splitTurn(turn, msgs, 0); got != -1 {
		t.Errorf("budget=0: expected -1, got %d", got)
	}
	if got := splitTurn(turn, msgs, -10); got != -1 {
		t.Errorf("budget<0: expected -1, got %d", got)
	}
}

func TestSplitTurn_singleMessageTurn(t *testing.T) {
	// A turn with only 1 message cannot be split further.
	msgs := []*store.Message{makeUserMsg("u1", 500)}
	turn := Turn{StartIdx: 0, EndIdx: 1}
	if got := splitTurn(turn, msgs, 1000); got != -1 {
		t.Errorf("single-message turn: expected -1, got %d", got)
	}
}

func TestSplitTurn_nothingFits(t *testing.T) {
	// Budget too small for even the last message of the turn.
	msgs := []*store.Message{
		makeUserMsg("u1", 0),    // fallback 500
		makeAssistantMsg("a1", 0), // fallback 300
		makeAssistantMsg("a2", 0), // fallback 300
	}
	turn := Turn{StartIdx: 0, EndIdx: 3}
	// Smallest sub-slice: msgs[2:3] = 300 tokens. Budget = 299 < 300 → nothing fits.
	if got := splitTurn(turn, msgs, 299); got != -1 {
		t.Errorf("nothing fits: expected -1, got %d", got)
	}
}

func TestSplitTurn_lastMessageFits(t *testing.T) {
	// Only the last assistant message fits in the budget.
	msgs := []*store.Message{
		makeUserMsg("u1", 0),      // fallback 500
		makeAssistantMsg("a1", 0), // fallback 300
		makeAssistantMsg("a2", 0), // fallback 300
	}
	turn := Turn{StartIdx: 0, EndIdx: 3}
	// msgs[2:3] = 300 tokens fits in budget=300.
	// msgs[1:3] = 600 tokens does NOT fit.
	// Expected: split at index 2 (skip user + first assistant).
	got := splitTurn(turn, msgs, 300)
	if got != 2 {
		t.Errorf("last message fits: expected split at 2, got %d", got)
	}
}

func TestSplitTurn_multipleMessagesFit(t *testing.T) {
	msgs := []*store.Message{
		makeUserMsg("u1", 0),      // 500
		makeAssistantMsg("a1", 0), // 300
		makeAssistantMsg("a2", 0), // 300
		makeAssistantMsg("a3", 0), // 300
	}
	turn := Turn{StartIdx: 0, EndIdx: 4}
	// msgs[1:4] = 900. msgs[2:4] = 600. msgs[3:4] = 300.
	// Budget = 650: msgs[2:4]=600 fits first.
	got := splitTurn(turn, msgs, 650)
	if got != 2 {
		t.Errorf("multiple fit: expected split at 2, got %d", got)
	}
}

func TestSplitTurn_allFit(t *testing.T) {
	// Even the full turn (minus user) fits. splitTurn starts at start+1.
	msgs := []*store.Message{
		makeUserMsg("u1", 0),      // 500
		makeAssistantMsg("a1", 0), // 300
	}
	turn := Turn{StartIdx: 0, EndIdx: 2}
	// msgs[1:2] = 300 ≤ 1000.
	got := splitTurn(turn, msgs, 1000)
	if got != 1 {
		t.Errorf("all fit: expected split at 1, got %d", got)
	}
}

// ---- Select: splitTurn path -----------------------------------------------

func TestSelect_splitTurnActivated(t *testing.T) {
	// Session: 4 turns, tailTurns=2, budget set so that the last full turn
	// doesn't fit but part of it does.
	//
	//   turn0: u0(500) + a0(300) = 800 tokens
	//   turn1: u1(500) + a1(300) = 800 tokens
	//   turn2: u2(500) + a2(300) = 800 tokens   ← would be in tail
	//   turn3: u3(500) + a3(300) = 800 tokens   ← would be in tail
	//
	// tailTurns=2 → tail candidates = turn2, turn3
	// budget=900: turn3(800) fits, turn2(800) doesn't (total would be 1600>900).
	// remaining after turn3 = 900-800 = 100.
	// splitTurn(turn2, msgs, 100):
	//   msgs[u2..a2] indices:
	//     start at turn2.StartIdx+1 = 5, end = turn2.EndIdx = 8
	//     msgs[5:8] = a2(300)+u3(500)+... wait, need to be careful with layout.

	// Layout: u0,a0, u1,a1, u2,a2, u3,a3  (indices 0..7)
	msgs := []*store.Message{
		makeUserMsg("u0", 500),    // 0
		makeAssistantMsg("a0", 300), // 1
		makeUserMsg("u1", 500),    // 2
		makeAssistantMsg("a1", 300), // 3
		makeUserMsg("u2", 500),    // 4  ← turn2 start
		makeAssistantMsg("a2", 300), // 5  ← turn2 end = 6 (exclusive, before u3)
		makeUserMsg("u3", 500),    // 6  ← turn3 start
		makeAssistantMsg("a3", 300), // 7
	}
	allParts := noParts(msgs)
	// turn2 = [4,6), turn3 = [6,8)
	// budget=900: turn3(800) fits. remaining=100. splitTurn(turn2,[4,6),100):
	//   candidate start=5: msgs[5:6]=300 > 100 → doesn't fit → return -1
	// So split fails → tailStartTurnIdx advances to turn3 → tail = turn3.

	// To make splitTurn succeed: budget=900 for turn3(800), remaining=100.
	// msgs[5:6] = a2(300) > 100. Doesn't work.
	// Let's use budget=1100: turn3(800) fits. remaining=300. msgs[5:6]=300 ≤ 300 → split at 5.

	cfg := cfgWithBudget(1100)
	// Set tailTurns=2 via cfg so that only last 2 turns are candidates.
	two := 2
	cfg.Compaction.TailTurns = &two
	model := llm.Model{Limit: llm.ModelLimit{Context: 1, Output: 0}} // won't use model budget (cfg overrides)

	sel := Select(msgs, allParts, model, cfg)

	// splitTurn succeeds at index 5 (a2).
	// Head = msgs[:5] = [u0,a0,u1,a1,u2]
	// Tail starts at msgs[5] = "a2"
	if sel.TailStartID != "a2" {
		t.Errorf("TailStartID = %q, want %q", sel.TailStartID, "a2")
	}
	wantHeadLen := 5
	if len(sel.Head) != wantHeadLen {
		t.Errorf("Head len = %d, want %d", len(sel.Head), wantHeadLen)
	}
}

func TestSelect_splitTurnFails_fallsBackToNextTurn(t *testing.T) {
	// Same layout but budget too tight to split even the last assistant msg.
	msgs := []*store.Message{
		makeUserMsg("u0", 500),
		makeAssistantMsg("a0", 300),
		makeUserMsg("u1", 500),
		makeAssistantMsg("a1", 300),
		makeUserMsg("u2", 500),    // turn2 start=4, end=6
		makeAssistantMsg("a2", 300),
		makeUserMsg("u3", 500),    // turn3 start=6, end=8
		makeAssistantMsg("a3", 300),
	}
	allParts := noParts(msgs)
	// budget=900: turn3(800) fits. remaining=100. splitTurn(turn2, budget=100):
	//   msgs[5:6]=300 > 100 → -1. Fall back: tailStartTurnIdx = turn3 (idx=2 in turns[]).
	cfg := cfgWithBudget(900)
	two := 2
	cfg.Compaction.TailTurns = &two
	model := llm.Model{}

	sel := Select(msgs, allParts, model, cfg)

	// tail = turn3 only (starting at u3, index 6)
	if sel.TailStartID != "u3" {
		t.Errorf("TailStartID = %q, want %q", sel.TailStartID, "u3")
	}
}

func TestSelect_splitTurnRecentHead_correctTurns(t *testing.T) {
	// Verify that recentHead is computed relative to the split turn index,
	// not the stale tailStartTurnIdx (regression for the bug fixed in review).
	//
	// 5 turns, tailTurns=2, splitTurn activates on turn3 (the older of the 2 tail candidates).
	// turns: t0[0,2), t1[2,4), t2[4,6), t3[6,8), t4[8,10)
	msgs := []*store.Message{
		makeUserMsg("u0", 500), makeAssistantMsg("a0", 300), // turn0
		makeUserMsg("u1", 500), makeAssistantMsg("a1", 300), // turn1
		makeUserMsg("u2", 500), makeAssistantMsg("a2", 300), // turn2
		makeUserMsg("u3", 500), makeAssistantMsg("a3", 300), // turn3 (split target)
		makeUserMsg("u4", 500), makeAssistantMsg("a4", 300), // turn4 (fits whole)
	}
	allParts := noParts(msgs)
	// budget=1100: turn4(800) fits. remaining=300. splitTurn(turn3,[6,8),300):
	//   msgs[7:8]=a3(300) ≤ 300 → split at 7.
	// splitTurnAbsIdx = 5-2+0 = 3 (turn3 in turns[]).
	// recentHead: recentTurnIdx = 3-2 = 1 → turns[1].StartIdx = 2.
	// recentHead = msgs[2:7] = [u1,a1, u2,a2, u3]
	cfg := cfgWithBudget(1100)
	two := 2
	cfg.Compaction.TailTurns = &two
	model := llm.Model{}

	sel := Select(msgs, allParts, model, cfg)

	if sel.TailStartID != "a3" {
		t.Errorf("TailStartID = %q, want %q", sel.TailStartID, "a3")
	}
	// recentHead should start at u1 (index 2) and end before split point (index 7)
	if len(sel.RecentHead) == 0 {
		t.Fatal("RecentHead is nil/empty, want non-empty")
	}
	if sel.RecentHead[0].ID != "u1" {
		t.Errorf("RecentHead[0].ID = %q, want %q", sel.RecentHead[0].ID, "u1")
	}
	if sel.RecentHead[len(sel.RecentHead)-1].ID != "u3" {
		t.Errorf("RecentHead last ID = %q, want %q", sel.RecentHead[len(sel.RecentHead)-1].ID, "u3")
	}
}
