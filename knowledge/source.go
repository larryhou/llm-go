package knowledge

import "context"

// Source is the core abstraction for a knowledge backend.
//
// Each implementation represents one information platform:
// a Bleve full-text index, a relational database, a vector store,
// a web search/fetch engine, a private API, etc.
//
// The Manager holds a priority-ordered list of Sources and routes every
// Query to those that declare they can handle it via Accepts().
// Sources never communicate with each other; the Manager is the only
// coordinator.
//
// Implementing a Source:
//
//  1. Return a stable, unique ID() — used in Result.RefID prefix and logs.
//  2. Return a static Priority() — lower value = higher priority.
//  3. Implement Accepts() to filter queries this source can serve.
//     Be conservative: a false return costs nothing; a wasted Peek/Fetch
//     call wastes latency and context budget.
//  4. Implement Peek() for lightweight snippet retrieval.
//  5. Implement Fetch() for full-content retrieval by RefID.
//
// RefID convention:
//
//	Result.RefID must be formatted as "{sourceID}:{internal-key}".
//	Example: "internal-wiki:doc-42", "web:https://example.com/page".
//	The Manager uses the prefix to route Fetch calls back to the correct
//	source without scanning all sources.
type Source interface {
	// ID returns the globally unique source identifier.
	// Used as the prefix in Result.RefID and in log/error messages.
	// Must be a stable, non-empty, URL-safe string (e.g. "internal-wiki").
	ID() string

	// Priority returns the static dispatch priority (lower = higher priority).
	// The Manager groups sources by priority and dispatches groups in order.
	// Sources in the same group are called concurrently.
	// A higher-priority group that returns enough results causes the Manager
	// to skip lower-priority groups entirely.
	Priority() int

	// Accepts reports whether this source can serve the given query.
	// The Manager calls this before every Peek or Fetch dispatch.
	// Implementations should check q.Type and optionally q.Filters / q.Input
	// format (e.g. URL prefix for a web source).
	// Returning false is cheap; returning true for a query the source cannot
	// handle well wastes latency and degrades result quality.
	Accepts(q Query) bool

	// Peek performs lightweight retrieval and returns compact result snippets.
	//
	// Contract:
	//   - Populate Result.RefID, Title, Source, Score, Snippet (and Metadata).
	//   - Leave Result.Content empty.
	//   - Return at most q.MaxResults items (or a source-defined default when
	//     q.MaxResults == 0).
	//   - Respect ctx cancellation; return promptly on ctx.Done().
	//   - Bleve implementation: Search() + Highlight → fragments as Snippet.
	//   - DB implementation: SELECT id, title, LEFT(body, N) LIMIT q.MaxResults.
	//
	// The Manager calls Peek for QueryTypeSearch.
	// Results are returned to the LLM as a compact list; the LLM decides
	// whether to call knowledge_fetch for any of them.
	Peek(ctx context.Context, q Query) ([]Result, error)

	// Fetch performs full-content retrieval for a single item.
	//
	// Contract:
	//   - q.Input is a RefID previously returned by Peek (format:
	//     "{sourceID}:{internal-key}"), or a raw URL / doc-ID supplied
	//     directly by the LLM.
	//   - Populate Result.RefID, Title, Source, Content (and Metadata).
	//   - Return exactly one Result in the slice (or an error).
	//   - Snippet may be left empty.
	//   - Respect ctx cancellation.
	//   - Bleve implementation: index.Document(internalKey) → stored fields.
	//   - Web implementation: HTTP GET + body extraction.
	//
	// The Manager calls Fetch for QueryTypeFetch.
	// Large Content values are truncated by the Manager before delivery to
	// the LLM tool result.
	Fetch(ctx context.Context, q Query) ([]Result, error)
}

// Knowledge source priority tiers.
// Lower value = higher priority. Sources in the same tier run concurrently.
//
// Each tier spans 1000 units, leaving room for intra-tier fine-tuning
// (e.g. TierKnowledge+1 for a higher-confidence remote source).
// All tiers start from 1000 so that the zero value of an int field is never
// silently interpreted as a valid (highest-priority) tier.
//
// Tier ordering rationale:
//
//	TierSOP (1000)       — team-wide authoritative playbooks; verified, always checked first.
//	TierKnowledge (2000) — externally-registered MCP sources; supplements SOP.
//	TierMemory (3000)    — personal notes; useful but unverified.
//	TierHistory (4000)   — session compaction summaries; unverified, already in context window.
const (
	TierSOP       = 1000
	TierKnowledge = 2000
	TierMemory    = 3000
	TierHistory   = 4000
)
