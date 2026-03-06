package metrics

import (
	"bufio"
	"encoding/json"
	"os"
)

// LoadHistory loads baseline history from a JSONL file
func LoadHistory(path string) ([]Baseline, error) {
	f, err := os.Open(path) // #nosec G304 -- path from caller (metrics history file)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var history []Baseline
	scanner := bufio.NewScanner(f)
	// Increase buffer for large JSON lines
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		var b Baseline
		if err := json.Unmarshal(scanner.Bytes(), &b); err != nil {
			continue // Skip malformed lines
		}
		history = append(history, b)
	}

	return history, scanner.Err()
}

// HistoryEntry represents a single point in history for display
type HistoryEntry struct {
	Timestamp   string  `json:"timestamp"`
	GitCommit   string  `json:"git_commit"`
	IndexMs     int64   `json:"index_ms"`
	Symbols     int     `json:"symbols"`
	QueryP95Ms  float64 `json:"query_p95_ms"`
	DocCoverage float64 `json:"doc_coverage_pct"`
	IndexDelta  string  `json:"index_delta,omitempty"` // "↑", "↓", or ""
	QueryDelta  string  `json:"query_delta,omitempty"` // "↑", "↓", or ""
}

// ToHistoryEntries converts baselines to history entries with deltas
func ToHistoryEntries(baselines []Baseline) []HistoryEntry {
	entries := make([]HistoryEntry, len(baselines))

	for i, b := range baselines {
		entries[i] = HistoryEntry{
			Timestamp:   b.Timestamp,
			GitCommit:   b.GitCommit,
			IndexMs:     b.Index.TotalMs,
			Symbols:     b.Codebase.Symbols,
			QueryP95Ms:  b.Query.DefByNameMs,
			DocCoverage: b.Quality.DocCoverage,
		}

		// Calculate deltas from previous entry
		if i > 0 {
			prev := baselines[i-1]

			// Index time delta (lower is better)
			if b.Index.TotalMs < prev.Index.TotalMs {
				entries[i].IndexDelta = "↓"
			} else if b.Index.TotalMs > prev.Index.TotalMs*115/100 { // >15% increase
				entries[i].IndexDelta = "↑"
			}

			// Query time delta (lower is better)
			if b.Query.DefByNameMs < prev.Query.DefByNameMs {
				entries[i].QueryDelta = "↓"
			} else if b.Query.DefByNameMs > prev.Query.DefByNameMs*1.15 { // >15% increase
				entries[i].QueryDelta = "↑"
			}
		}
	}

	return entries
}
