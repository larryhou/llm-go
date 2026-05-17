package session

// estimate_tokens_test.go — white-box tests for estimateTurnTokens.
// Lives in package session (not session_test) because estimateTurnTokens is unexported.

import (
	"testing"

	"github.com/larryhou/llm-go/store"
)

func TestEstimateTurnTokens(t *testing.T) {
	cases := []struct {
		name string
		msgs []*store.Message
		want int
	}{
		{
			name: "empty slice returns 0",
			msgs: nil,
			want: 0,
		},
		{
			name: "user message with no stored tokens uses fallback 500",
			msgs: []*store.Message{
				{Role: store.RoleUser},
			},
			want: 500,
		},
		{
			name: "assistant message with no stored tokens uses fallback 300",
			msgs: []*store.Message{
				{Role: store.RoleAssistant},
			},
			want: 300,
		},
		{
			name: "message with stored tokens ignores fallback",
			msgs: []*store.Message{
				{Role: store.RoleUser, Tokens: store.TokenSummary{Input: 123, Output: 77}},
			},
			want: 200, // 123 + 77
		},
		{
			name: "stored tokens: all four fields summed",
			msgs: []*store.Message{
				{Role: store.RoleAssistant, Tokens: store.TokenSummary{
					Input: 10, Output: 20, CacheRead: 5, CacheWrite: 3,
				}},
			},
			want: 38,
		},
		{
			name: "stored tokens zero-sum falls back to role default",
			// All four token fields are 0 → sum is 0 → use fallback.
			msgs: []*store.Message{
				{Role: store.RoleAssistant, Tokens: store.TokenSummary{Input: 0, Output: 0}},
			},
			want: 300,
		},
		{
			name: "mixed slice: stored + fallback user + fallback assistant",
			msgs: []*store.Message{
				// stored 1000
				{Role: store.RoleUser, Tokens: store.TokenSummary{Input: 600, Output: 400}},
				// fallback user 500
				{Role: store.RoleUser},
				// fallback assistant 300
				{Role: store.RoleAssistant},
				// stored 50
				{Role: store.RoleAssistant, Tokens: store.TokenSummary{Output: 50}},
			},
			want: 1000 + 500 + 300 + 50,
		},
		{
			name: "old fallback 100 is NOT the expected value for user",
			// Regression: before the fix the fallback was 100 for all roles.
			// This test documents that 100 is no longer returned for a user message.
			msgs: []*store.Message{
				{Role: store.RoleUser},
			},
			want: 500, // not 100
		},
		{
			name: "old fallback 100 is NOT the expected value for assistant",
			msgs: []*store.Message{
				{Role: store.RoleAssistant},
			},
			want: 300, // not 100
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := estimateTurnTokens(tc.msgs)
			if got != tc.want {
				t.Errorf("estimateTurnTokens() = %d, want %d", got, tc.want)
			}
		})
	}
}
