// Package knowledge provides a pluggable knowledge manager that exposes
// external information sources to an LLM as tool calls.
//
// Design goals:
//   - Results are injected into context ONLY when the LLM explicitly calls a
//     knowledge tool — never proactively.
//   - Two-level retrieval: Peek returns compact snippets (minimal context
//     growth); Fetch returns full content (on-demand, LLM decides).
//   - Sources are routed automatically via Accepts(); the LLM has no
//     awareness of which backend is used.
//   - Priority-grouped concurrent dispatch: higher-priority sources are
//     queried first; lower-priority groups are skipped when results are
//     sufficient.
package knowledge

// QueryType classifies a knowledge request from the LLM's intent perspective,
// not from a technical-operation perspective.
type QueryType string

const (
	// QueryTypeSearch is a broad exploratory request: "find information about X".
	// The LLM does not know exactly where the answer lives.
	// Manager dispatches to Source.Peek(); results are snippets + RefIDs.
	// Typical backends: full-text index (Bleve), vector DB, web search.
	QueryTypeSearch QueryType = "search"

	// QueryTypeFetch is a precise retrieval request: "get the full content of X".
	// The LLM already has a RefID (from a prior Search) or an explicit URL/ID.
	// Manager dispatches to Source.Fetch(); result is the complete document.
	// Typical backends: Bleve by doc ID, HTTP GET, DB row by PK.
	QueryTypeFetch QueryType = "fetch"
)

// Query is the normalised request the Manager sends to each Source.
// It is constructed from the LLM tool-call input and passed unchanged to
// every Source that Accepts it.
type Query struct {
	// Type classifies the retrieval intent (see QueryType constants).
	Type QueryType

	// Input is the free-form request body:
	//   Search → search terms or query expression
	//   Fetch  → RefID returned by a prior Peek, or a raw URL / doc ID
	Input string

	// Filters holds optional structured constraints (field=value pairs,
	// date ranges, tag lists, etc.).  Sources may ignore fields they do not
	// understand.
	Filters map[string]any

	// MaxResults caps the number of results a Source should return.
	// 0 means "use the source's own default".
	// Only meaningful for Peek; Fetch always returns a single item.
	MaxResults int

	// CutScore is the minimum relevance score threshold for Peek results.
	// Results with Score < CutScore are discarded after each priority group.
	// The Manager also forwards this value to remote sources (e.g. via the
	// ks_search "score" parameter) so they can apply the same filter server-side.
	// 0 means "use the default" (DefaultCutScore = 0.75).
	// Set to a negative value to disable filtering entirely.
	CutScore float64

	// HitScore is the "good enough" threshold.
	// When at least one result with Score >= HitScore has been accumulated
	// and the total count has reached MaxResults, the Manager stops descending
	// into lower-priority groups.
	// 0 means "use the default" (DefaultHitScore = 0.95).
	// Set to a negative value to disable early stopping entirely.
	HitScore float64
}

const (
	// DefaultCutScore is the default minimum score threshold applied when
	// Query.CutScore is zero (unset). Results below this score are discarded.
	DefaultCutScore = 0.5

	// DefaultHitScore is the default "good enough" threshold applied when
	// Query.HitScore is zero (unset). When accumulated results include at
	// least one result at or above this score and MaxResults is reached,
	// lower-priority groups are skipped.
	DefaultHitScore = 0.95
)

// Result is a single knowledge item.  Peek and Fetch share this struct;
// the active fields differ by query type.
type Result struct {
	// RefID is the globally unique reference identifier for this item.
	// Format: "{sourceID}:{internal-id-or-path}"
	// The LLM passes this verbatim to knowledge_fetch to retrieve full content.
	RefID string

	// Title is the human-readable name of the item (document title, page
	// heading, table row summary, etc.).
	Title string

	// Source is the originating source ID (matches Source.ID()).
	Source string

	// Score is the relevance score in [0, 1].
	// -1 means the source cannot compute a score (e.g. exact-match lookup).
	Score float64

	// Metadata holds source-specific extra fields (timestamps, authors,
	// tags, HTTP status codes, SQL column values, etc.).
	Metadata map[string]any

	// Snippet is populated by Peek: a short excerpt or highlighted fragment
	// of the document, suitable for inclusion in an LLM context summary.
	// Content is left empty by Peek.
	Snippet string

	// Content is populated by Fetch: the complete document body.
	// Snippet may be left empty by Fetch.
	// Large values are truncated by the Manager before returning to the LLM.
	Content string
}
