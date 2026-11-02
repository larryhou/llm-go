// Package group provides semantic grouping of labeled items using dense
// vector embeddings.
//
// # Problem
//
// Given a list of items each identified by an opaque ID and a short name
// (e.g. QA bug tickets), find clusters of semantically similar items so that
// duplicate or near-duplicate entries can be reviewed together.
//
// Static rule-based deduplication (exact-title match, regex, edit distance)
// misses paraphrases and context-dependent synonyms.  Embedding-based cosine
// similarity catches those cases naturally.
//
// # Core abstractions
//
//	Item      — input: {ID, Name}
//	Group     — output: one cluster of Items with a representative label
//	Result    — full output: []Group (each group is sorted by score desc)
//	Grouper   — interface: Group(ctx, items) (Result, error)
//
// # EmbeddingGrouper
//
// The only built-in Grouper implementation uses an embed.Client to vectorise
// each item's Name field, then runs greedy agglomerative clustering:
//
//  1. Embed all names in one batch request.
//  2. Build an n×n cosine-similarity matrix.
//  3. Greedily assign items to the first existing group whose centroid is
//     within the configured Threshold; otherwise start a new group.
//  4. After all items are assigned, recompute each group's Label as the name
//     of the item whose vector is closest to the group centroid.
//
// Threshold tuning guide:
//
//	0.92–0.95  very strict — only near-identical phrasings merge
//	0.85–0.90  recommended for QA ticket deduplication
//	0.75–0.84  loose — broad topic clusters
//
// # HTTP interface (wired by llm-api)
//
//	POST /group
//	  Request:  {"items":[{"id":"BUG-1","desc":"Login button not working"},...],
//	             "threshold":0.88, "model":"nomic-embed-text",
//	             "ollama_url":"http://localhost:11434"}
//	  Response: {"groups":[{"label":"Login button not working",
//	                        "items":[{"id":"BUG-1","desc":"...","score":1.0},
//	                                 {"id":"BUG-7","desc":"...","score":0.91}]},
//	                       ...]}
package group

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/larryhou/llm-go/embed"
)

// ── public types ──────────────────────────────────────────────────────────────

// Item is the input unit: an opaque identifier plus a description string.
// The description is what gets vectorised; the ID is carried through to the
// output unchanged so callers can map results back to their own data model.
type Item struct {
	ID   string `json:"id"`
	Desc string `json:"desc"`
}

// ScoredItem is an Item annotated with its cosine similarity to the group
// centroid (1.0 for the seed item that started the group).
type ScoredItem struct {
	Item
	Score float64 `json:"score"`
}

// Group is one semantic cluster returned by a Grouper.
// Label is the name of the item whose vector is nearest the group centroid
// — a human-readable title for the cluster.
// Items are sorted by Score descending (highest similarity first).
type Group struct {
	Label string       `json:"label"`
	Items []ScoredItem `json:"items"`
}

// Result is the complete output of a Grouper call.
type Result struct {
	Groups []Group `json:"groups"`
}

// ── Grouper interface ─────────────────────────────────────────────────────────

// Grouper clusters a list of Items into semantic groups.
//
// Implementations are free to choose any algorithm (embedding cosine
// similarity, keyword hashing, LLM-based classification, etc.).
// The only contract is:
//
//   - Every input Item must appear in exactly one output Group.
//   - Groups are sorted by descending size (largest group first).
//   - Items within each Group are sorted by Score descending.
//   - An empty input produces an empty Result (no error).
type Grouper interface {
	Group(ctx context.Context, items []Item) (Result, error)
}

// ── EmbeddingGrouper ──────────────────────────────────────────────────────────

// EmbeddingGrouper implements Grouper using Ollama embeddings and greedy
// centroid-based agglomerative clustering.
//
// Zero value is not usable; construct with NewEmbeddingGrouper.
type EmbeddingGrouper struct {
	client      *embed.Client
	threshold   float64 // cosine similarity threshold in (0, 1]
	concurrency int     // number of concurrent embed requests (default: DefaultConcurrency)
}

// DefaultThreshold is the cosine-similarity cutoff used when
// NewEmbeddingGrouper is called with threshold == 0.
const DefaultThreshold = 0.88

// DefaultConcurrency is the number of concurrent /api/embed requests used
// when splitting a large input batch. Conservatively set to 4 to match a
// typical OLLAMA_NUM_PARALLEL=4 deployment.
const DefaultConcurrency = 4

// NewEmbeddingGrouper creates an EmbeddingGrouper.
//
//   - client: Ollama embed client; must not be nil.
//   - threshold: cosine-similarity cutoff in (0,1]; 0 → DefaultThreshold.
//
// Items with similarity ≥ threshold to any group centroid are merged into
// that group; otherwise a new group is started.
func NewEmbeddingGrouper(client *embed.Client, threshold float64) *EmbeddingGrouper {
	if threshold <= 0 || threshold > 1 {
		threshold = DefaultThreshold
	}
	return &EmbeddingGrouper{client: client, threshold: threshold, concurrency: DefaultConcurrency}
}

// Group clusters items by semantic similarity of their Name fields.
//
// Algorithm:
//  1. Batch-embed all names via the Ollama /api/embed endpoint.
//  2. For each item (in input order), compare its vector against each
//     existing group centroid.  If the best matching centroid is ≥ threshold,
//     assign the item to that group and update the centroid.  Otherwise open
//     a new single-item group.
//  3. After all assignments, recompute each group's Label as the item whose
//     vector is closest to the final centroid.
//  4. Sort groups by descending size; sort items within each group by
//     descending Score.
func (g *EmbeddingGrouper) Group(ctx context.Context, items []Item) (Result, error) {
	if len(items) == 0 {
		return Result{}, nil
	}

	// Step 1: embed all descs, splitting into `concurrency` chunks sent concurrently.
	descs := make([]string, len(items))
	for i, it := range items {
		descs[i] = it.Desc
	}
	vecs, err := g.embedConcurrent(ctx, descs)
	if err != nil {
		return Result{}, fmt.Errorf("group: embed: %w", err)
	}

	// Step 2: greedy centroid clustering.
	type cluster struct {
		centroid []float64 // running mean of member vectors
		count    int        // number of members
		members  []ScoredItem
	}

	var clusters []*cluster

	for i, vec := range vecs {
		bestIdx := -1
		bestSim := -1.0
		for ci, cl := range clusters {
			sim := embed.CosineSimilarity(vec, cl.centroid)
			if sim > bestSim {
				bestSim = sim
				bestIdx = ci
			}
		}

		if bestIdx >= 0 && bestSim >= g.threshold {
			// Merge into existing cluster; update centroid incrementally.
			cl := clusters[bestIdx]
			cl.members = append(cl.members, ScoredItem{Item: items[i], Score: bestSim})
			cl.centroid = updateCentroid(cl.centroid, vec, cl.count)
			cl.count++
		} else {
			// Seed a new cluster; score of the seed item is 1.0.
			clusters = append(clusters, &cluster{
				centroid: vec,
				count:    1,
				members:  []ScoredItem{{Item: items[i], Score: 1.0}},
			})
		}
	}

	// Step 3: recompute label = item closest to final centroid.
	groups := make([]Group, len(clusters))
	for gi, cl := range clusters {
		// Sort items by Score descending.
		sort.Slice(cl.members, func(a, b int) bool {
			return cl.members[a].Score > cl.members[b].Score
		})

		// Label = name of the item with score closest to 1.0
		// (i.e. most central to the cluster).
		// After sort, that is cl.members[0].
		label := cl.members[0].Desc
		// Refine: find the item whose original vector is geometrically
		// closest to the final centroid (in case incremental centroid
		// drift changed the ordering).
		bestSim := -1.0
		for _, m := range cl.members {
			// Find the original vector for this member.
			idx := indexOfID(items, m.ID)
			if idx < 0 {
				continue
			}
			sim := embed.CosineSimilarity(vecs[idx], cl.centroid)
			if sim > bestSim {
				bestSim = sim
				label = m.Desc
			}
		}

		groups[gi] = Group{Label: label, Items: cl.members}
	}

	// Step 4: sort groups by descending size, then by label lexicographically
	// for stability.
	sort.Slice(groups, func(i, j int) bool {
		if len(groups[i].Items) != len(groups[j].Items) {
			return len(groups[i].Items) > len(groups[j].Items)
		}
		return groups[i].Label < groups[j].Label
	})

	return Result{Groups: groups}, nil
}

// ── internal helpers ──────────────────────────────────────────────────────────

// embedConcurrent splits texts into at most g.concurrency chunks and embeds
// them in parallel, then reassembles the result slices in original order.
//
// If len(texts) ≤ g.concurrency each text goes in its own request; if there
// are more texts than workers each worker gets ⌈len/concurrency⌉ texts.
// Any single worker error cancels the remaining work and is returned immediately.
func (g *EmbeddingGrouper) embedConcurrent(ctx context.Context, texts []string) ([][]float64, error) {
	n := len(texts)
	workers := g.concurrency
	if workers <= 0 {
		workers = DefaultConcurrency
	}
	if workers > n {
		workers = n
	}

	// Divide texts into `workers` contiguous chunks.
	type chunk struct {
		start int
		texts []string
	}
	chunks := make([]chunk, 0, workers)
	size := (n + workers - 1) / workers // ceil division
	for i := 0; i < n; i += size {
		end := i + size
		if end > n {
			end = n
		}
		chunks = append(chunks, chunk{start: i, texts: texts[i:end]})
	}

	// Fan-out: one goroutine per chunk.
	type result struct {
		start int
		vecs  [][]float64
		err   error
	}
	ch := make(chan result, len(chunks))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	for _, c := range chunks {
		c := c
		go func() {
			vecs, err := g.client.Embed(ctx, c.texts)
			ch <- result{start: c.start, vecs: vecs, err: err}
		}()
	}

	// Collect and reassemble in original order.
	out := make([][]float64, n)
	for range chunks {
		r := <-ch
		if r.err != nil {
			cancel()
			// drain remaining goroutines to avoid leaks
			for i := 1; i < len(chunks); i++ {
				<-ch
			}
			return nil, r.err
		}
		for j, vec := range r.vecs {
			out[r.start+j] = vec
		}
	}
	return out, nil
}

// updateCentroid computes the new running mean after adding vec to a cluster
// that currently has `count` members with centroid `old`.
// Does not normalise — raw arithmetic mean preserves direction well enough
// for cosine comparison with unit vectors from Ollama.
func updateCentroid(old, vec []float64, count int) []float64 {
	next := make([]float64, len(old))
	n := float64(count)
	for i := range old {
		next[i] = (old[i]*n + vec[i]) / (n + 1)
	}
	return next
}

// normVec normalises a vector to unit length.  Returns a zero vector if the
// input has zero magnitude.
func normVec(v []float64) []float64 {
	var sum float64
	for _, x := range v {
		sum += x * x
	}
	mag := math.Sqrt(sum)
	if mag == 0 {
		return v
	}
	out := make([]float64, len(v))
	for i, x := range v {
		out[i] = x / mag
	}
	return out
}

// indexOfID returns the index in items where items[i].ID == id, or -1.
func indexOfID(items []Item, id string) int {
	for i, it := range items {
		if it.ID == id {
			return i
		}
	}
	return -1
}

// Ensure normVec is referenced (it is used by callers that may unit-test
// centroid quality; keep it exported-visible via the unexported helper).
var _ = normVec
