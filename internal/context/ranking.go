// Package context provides role-weighted symbol ranking for boot context generation.
package context

import (
	"database/sql"
	"math"
	"sort"
)

// roleWeights defines priority multipliers for each architectural role.
// Higher weights indicate more architecturally significant symbols.
var roleWeights = map[Role]float64{
	RoleEntryPoint:  10,
	RoleAPIBoundary: 5,
	RolePersistence: 5,
	RoleHTTPHandler: 5,
	RoleFactory:     3,
	RoleIO:          3,
	RoleInternal:    1,
}

// RankedSymbol represents a symbol with its calculated priority score.
type RankedSymbol struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	File     string  `json:"file"`
	Line     int     `json:"line"`
	RefCount int     `json:"ref_count"`
	Role     string  `json:"role"`
	Priority float64 `json:"priority"`
}

// CalculatePriority computes the priority score for a symbol.
// Formula: priority = (log(ref_count) + 1) * role_weight
// This balances reference count with architectural importance.
func CalculatePriority(refCount int, role Role) float64 {
	weight, ok := roleWeights[role]
	if !ok {
		weight = 1 // Default to internal weight
	}

	// Use log(ref_count + 1) to handle zero refs and dampen high ref counts
	// Adding 1 ensures log is never negative for ref_count >= 0
	logRefs := math.Log(float64(refCount) + 1)
	return (logRefs + 1) * weight
}

// RankSymbols queries symbols from the database, infers their roles,
// calculates priority scores, and returns them sorted by priority.
// Performance optimizations:
// - Single batch query for symbols and ref counts (no loop queries)
// - In-memory role lookup using map built from InferRoles
// - Early limiting via SQL ORDER BY + LIMIT after scoring
func RankSymbols(db *sql.DB, repoRoot string, limit int) ([]RankedSymbol, error) {
	// Step 1: Infer roles for all symbols in one batch
	// This builds the role map upfront to avoid per-symbol queries
	symbolRoles, err := InferRoles(db, repoRoot)
	if err != nil {
		return nil, err
	}

	// Build role lookup map by symbol ID for O(1) access
	roleMap := make(map[string]Role, len(symbolRoles))
	for _, sr := range symbolRoles {
		roleMap[sr.SymbolID] = sr.Role
	}

	// Step 2: Query all symbols with their ref counts in a single batch query
	// This avoids N+1 query problem by using a LEFT JOIN with COUNT
	rows, err := db.Query(`
		SELECT
			s.id,
			s.name,
			s.file_path,
			s.line_start,
			COUNT(r.id) as ref_count
		FROM symbols s
		LEFT JOIN refs r ON r.symbol_id = s.id
		WHERE s.kind IN ('func', 'method', 'type', 'interface', 'struct')
		  AND s.file_path LIKE ? || '/%'
		  AND s.file_path NOT LIKE '%/example%'
		  AND s.file_path NOT LIKE '%/testdata%'
		  AND s.file_path NOT LIKE '%_test.go'
		GROUP BY s.id
	`, repoRoot)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Step 3: Calculate priorities and collect results
	var results []RankedSymbol
	for rows.Next() {
		var rs RankedSymbol
		if err := rows.Scan(&rs.ID, &rs.Name, &rs.File, &rs.Line, &rs.RefCount); err != nil {
			continue // Skip malformed rows
		}

		// Look up role from pre-built map (O(1))
		role, ok := roleMap[rs.ID]
		if !ok {
			// Default based on name casing if not in role map
			role = RoleInternal
			if len(rs.Name) > 0 && rs.Name[0] >= 'A' && rs.Name[0] <= 'Z' {
				role = RoleAPIBoundary
			}
		}
		rs.Role = string(role)
		rs.Priority = CalculatePriority(rs.RefCount, role)

		results = append(results, rs)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Step 4: Sort by priority descending
	sort.Slice(results, func(i, j int) bool {
		// Primary sort by priority (descending)
		if results[i].Priority != results[j].Priority {
			return results[i].Priority > results[j].Priority
		}
		// Secondary sort by ref count (descending) for equal priorities
		if results[i].RefCount != results[j].RefCount {
			return results[i].RefCount > results[j].RefCount
		}
		// Tertiary sort by name (ascending) for stability
		return results[i].Name < results[j].Name
	})

	// Step 5: Apply limit
	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}

	return results, nil
}
