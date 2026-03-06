package embed

import (
	"fmt"
	"sort"
	"time"

	"github.com/dkoosis/snipe/internal/output"
	"github.com/dkoosis/snipe/internal/query"
	"github.com/dkoosis/snipe/internal/store"
	"github.com/dkoosis/snipe/internal/vector"
)

// searchResult pairs a symbol ID with its similarity score for sorting.
type searchResult struct {
	symbolID   string
	similarity float32
}

// Search embeds the query, compares against all stored embeddings,
// and returns results above the threshold sorted by similarity descending.
// Returns (nil, 0, nil) if no embeddings exist — caller decides how to handle.
//
// Scaling boundary: GetAllEmbeddings() loads the full embedding table into memory.
// At 1024 dims × 4 bytes per float32, that's ~4KB per symbol. For 5,000 symbols
// this is ~20MB — acceptable for current use. If this becomes a bottleneck,
// the first optimization is an ANN index (HNSW or IVF) to avoid the full scan.
func Search(queryText string, s *store.Store, client *Client, limit int, threshold float32) ([]output.Result, time.Duration, error) {
	start := time.Now()

	count, err := s.CountEmbeddings()
	if err != nil { //nolint:nilerr // treat count failure as "no embeddings" — not caller's problem
		return nil, 0, nil
	}
	if count == 0 {
		return nil, 0, nil
	}

	queryEmbed, err := client.EmbedOne(queryText, "query")
	if err != nil {
		return nil, 0, fmt.Errorf("embed query: %w", err)
	}

	embeddings, err := s.GetAllEmbeddings()
	if err != nil {
		return nil, 0, fmt.Errorf("load embeddings: %w", err)
	}

	var matches []searchResult
	for _, e := range embeddings {
		sim := vector.CosineSimilarity(queryEmbed, e.Embedding)
		if sim >= threshold {
			matches = append(matches, searchResult{
				symbolID:   e.SymbolID,
				similarity: sim,
			})
		}
	}

	sort.Slice(matches, func(i, j int) bool {
		return matches[i].similarity > matches[j].similarity
	})

	if len(matches) > limit {
		matches = matches[:limit]
	}

	// Batch-hydrate results with full symbol info
	ids := make([]string, len(matches))
	for i, m := range matches {
		ids[i] = m.symbolID
	}

	symMap, err := query.BatchLookupByID(s.DB(), ids)
	if err != nil {
		return nil, 0, fmt.Errorf("batch lookup: %w", err)
	}

	results := make([]output.Result, 0, len(matches))
	for _, m := range matches {
		sym := symMap[m.symbolID]
		if sym == nil {
			continue // skip unresolvable symbols
		}
		r := sym.ToResult()
		r.Score = float64(m.similarity)
		results = append(results, r)
	}

	return results, time.Since(start), nil
}
