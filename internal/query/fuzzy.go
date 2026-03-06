package query

import (
	"database/sql"
	"sort"
	"strings"
)

// FuzzyMatch represents a fuzzy match result with its distance score.
type FuzzyMatch struct {
	Name     string
	Distance int
}

// LevenshteinDistance computes the edit distance between two strings.
// This is the minimum number of single-character edits (insertions,
// deletions, or substitutions) required to transform s1 into s2.
func LevenshteinDistance(s1, s2 string) int {
	// Convert to lowercase for case-insensitive comparison
	s1 = strings.ToLower(s1)
	s2 = strings.ToLower(s2)

	if len(s1) == 0 {
		return len(s2)
	}
	if len(s2) == 0 {
		return len(s1)
	}

	// Create two rows for the DP matrix (space optimization)
	prev := make([]int, len(s2)+1)
	curr := make([]int, len(s2)+1)

	// Initialize first row
	for j := range prev {
		prev[j] = j
	}

	// Fill in the matrix
	for i := 1; i <= len(s1); i++ {
		curr[0] = i
		for j := 1; j <= len(s2); j++ {
			cost := 1
			if s1[i-1] == s2[j-1] {
				cost = 0
			}
			curr[j] = min(
				prev[j]+1,      // deletion
				curr[j-1]+1,    // insertion
				prev[j-1]+cost, // substitution
			)
		}
		prev, curr = curr, prev
	}

	return prev[len(s2)]
}

// FindSimilarSymbols queries the database for symbols similar to the given name.
// It returns up to maxResults symbols with Levenshtein distance <= maxDistance.
func FindSimilarSymbols(db *sql.DB, name string, maxDistance, maxResults int) ([]string, error) {
	// Query all symbol names from the database
	// We limit to a reasonable set by first checking prefix matches and substring matches
	rows, err := db.Query(`
		SELECT DISTINCT name FROM symbols
		WHERE name LIKE ? OR name LIKE ? OR name LIKE ?
		LIMIT 1000
	`, name+"%", "%"+name+"%", strings.ToLower(name)+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var matches []FuzzyMatch
	for rows.Next() {
		var symName string
		if err := rows.Scan(&symName); err != nil {
			return nil, err
		}

		dist := LevenshteinDistance(name, symName)
		if dist <= maxDistance && dist > 0 { // Exclude exact matches (dist == 0)
			matches = append(matches, FuzzyMatch{Name: symName, Distance: dist})
		}
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	// If we didn't find enough matches with the prefix/substring approach,
	// do a broader search
	if len(matches) < maxResults {
		rows2, err := db.Query(`
			SELECT DISTINCT name FROM symbols
			LIMIT 5000
		`)
		if err != nil {
			return nil, err
		}
		defer rows2.Close()

		seen := make(map[string]bool)
		for _, m := range matches {
			seen[m.Name] = true
		}

		for rows2.Next() {
			var symName string
			if err := rows2.Scan(&symName); err != nil {
				return nil, err
			}

			if seen[symName] {
				continue
			}

			dist := LevenshteinDistance(name, symName)
			if dist <= maxDistance && dist > 0 {
				matches = append(matches, FuzzyMatch{Name: symName, Distance: dist})
				seen[symName] = true
			}
		}

		if err := rows2.Err(); err != nil {
			return nil, err
		}
	}

	// Sort by distance (closest matches first)
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Distance != matches[j].Distance {
			return matches[i].Distance < matches[j].Distance
		}
		// Secondary sort by name length (prefer shorter names)
		return len(matches[i].Name) < len(matches[j].Name)
	})

	// Return top results
	result := make([]string, 0, maxResults)
	for i := 0; i < len(matches) && i < maxResults; i++ {
		result = append(result, matches[i].Name)
	}

	return result, nil
}

// DefaultMaxDistance returns a reasonable max distance based on the query length.
// Shorter queries allow fewer edits, longer queries allow more.
func DefaultMaxDistance(query string) int {
	length := len(query)
	switch {
	case length <= 3:
		return 1
	case length <= 6:
		return 2
	case length <= 12:
		return 3
	default:
		return 4
	}
}
