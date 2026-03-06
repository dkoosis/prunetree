package context

import (
	"database/sql"
	"strings"
)

// GenerateArchitectureSummary creates a high-level architecture overview from the snipe index.
// It analyzes call graph data to produce:
// - Spine: primary call flows from entry points
// - Components: packages with their inferred purposes
// - Edges: top cross-package call relationships
// - Description: placeholder for LLM-generated prose
//
// Performance: Uses batch SQL queries with no queries in loops. Total queries <= 5.
func GenerateArchitectureSummary(db *sql.DB, repoRoot string) (*ArchSummary, error) {
	// Query 1-3: Get primary flows (reuses batch queries from flows.go)
	spine, err := ExtractPrimaryFlows(db, repoRoot, 5)
	if err != nil {
		// Non-fatal: continue with empty spine
		spine = nil
	}

	// Query 4: Get package purposes in a single batch query
	components, err := getPackagePurposes(db, repoRoot)
	if err != nil {
		// Non-fatal: continue with empty components
		components = nil
	}

	// Query 5: Get cross-package call edges in a single batch query
	edges, err := getCrossPackageEdges(db, repoRoot)
	if err != nil {
		// Non-fatal: continue with empty edges
		edges = nil
	}

	// Build description placeholder
	description := "[Architecture description pending LLM enrichment]"

	return &ArchSummary{
		Spine:       spine,
		Components:  components,
		Edges:       edges,
		Description: description,
	}, nil
}

// getPackagePurposes returns all packages with their inferred purposes.
// Queries distinct pkg_paths and shortens them in Go (SQLite lacks REVERSE).
// Uses real doc comments from package_docs table when available, falling back
// to hardcoded inference.
func getPackagePurposes(db *sql.DB, repoRoot string) ([]PackagePurpose, error) {
	rows, err := db.Query(`
		SELECT DISTINCT pkg_path
		FROM symbols
		WHERE file_path LIKE ? || '/%'
		  AND pkg_path IS NOT NULL
		  AND pkg_path != ''
		ORDER BY pkg_path
	`, repoRoot)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pkgPaths []string
	for rows.Next() {
		var pkgPath string
		if err := rows.Scan(&pkgPath); err != nil {
			continue
		}
		pkgPaths = append(pkgPaths, pkgPath)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Batch load package docs
	docMap := loadPackageDocs(db, pkgPaths)

	seen := make(map[string]bool)
	var components []PackagePurpose
	for _, pkgPath := range pkgPaths {
		name := shortenPackagePath(pkgPath)
		name = normalizePackageName(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true

		purpose := docMap[pkgPath]
		if purpose == "" {
			purpose = inferPackagePurpose(name)
		}
		components = append(components, PackagePurpose{
			Name:    name,
			Purpose: purpose,
		})
	}

	return components, nil
}

// loadPackageDocs returns a map of pkg_path -> first sentence of doc comment.
// Non-fatal: returns empty map if package_docs table doesn't exist yet.
func loadPackageDocs(db *sql.DB, pkgPaths []string) map[string]string {
	if len(pkgPaths) == 0 {
		return nil
	}

	placeholders := make([]string, len(pkgPaths))
	args := make([]interface{}, len(pkgPaths))
	for i, p := range pkgPaths {
		placeholders[i] = "?"
		args[i] = p
	}

	// #nosec G201 -- placeholders are positional parameters
	query := "SELECT pkg_path, doc FROM package_docs WHERE pkg_path IN (" +
		strings.Join(placeholders, ",") + ")"

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	result := make(map[string]string, len(pkgPaths))
	for rows.Next() {
		var pkgPath, doc string
		if err := rows.Scan(&pkgPath, &doc); err != nil {
			continue
		}
		result[pkgPath] = ExtractFirstSentence(doc)
	}
	return result
}

// normalizePackageName cleans up package names for display.
func normalizePackageName(name string) string {
	// Remove leading/trailing slashes
	name = strings.Trim(name, "/")

	// Skip empty or test-only packages
	if name == "" || name == "_test" {
		return ""
	}

	return name
}

// inferPackagePurpose determines a package's purpose from its name.
// This reuses the same logic as inferPurpose in generate.go but with full paths.
func inferPackagePurpose(pkg string) string {
	// Map of package patterns to purposes
	purposes := map[string]string{
		"cmd":              "CLI commands and entry points",
		"internal/store":   "SQLite persistence and database operations",
		"internal/query":   "Symbol lookup and reference queries",
		"internal/index":   "Go package loading and symbol extraction",
		"internal/output":  "JSON/human output formatting",
		"internal/config":  "Configuration management",
		"internal/search":  "Ripgrep integration and search",
		"internal/embed":   "Vector embeddings and similarity",
		"internal/context": "Boot context and LLM summaries",
		"internal/analyze": "Function analysis and diagnostics",
		"internal/edit":    "AST-safe code editing operations",
		"internal/kg":      "Knowledge graph integration (orca)",
		"internal/metrics": "Index and query metrics collection",
		"pkg":              "Public library packages",
		"api":              "API definitions and handlers",
		"test":             "Test utilities and fixtures",
	}

	// Check for exact match first
	if purpose, ok := purposes[pkg]; ok {
		return purpose
	}

	// Check for prefix matches
	for pattern, purpose := range purposes {
		if strings.HasPrefix(pkg, pattern) {
			return purpose
		}
	}

	// Infer from last segment of package path
	parts := strings.Split(pkg, "/")
	lastPart := parts[len(parts)-1]

	segmentPurposes := map[string]string{
		"store":    "Data storage and persistence",
		"query":    "Query execution",
		"index":    "Indexing operations",
		"output":   "Output formatting",
		"config":   "Configuration",
		"search":   "Search functionality",
		"embed":    "Embeddings",
		"context":  "Context generation",
		"analyze":  "Analysis",
		"util":     "Utility functions",
		"utils":    "Utility functions",
		"internal": "Internal implementation",
	}

	if purpose, ok := segmentPurposes[lastPart]; ok {
		return purpose
	}

	return "Application logic"
}

// getCrossPackageEdges returns the top cross-package call relationships.
// Uses a single batch query with aggregation.
func getCrossPackageEdges(db *sql.DB, repoRoot string) ([]CrossPackageEdge, error) {
	// Query cross-package calls grouped by caller/callee package
	rows, err := db.Query(`
		WITH PackageCalls AS (
			SELECT
				caller.pkg_path as caller_pkg,
				callee.pkg_path as callee_pkg,
				COUNT(*) as call_count
			FROM call_graph cg
			JOIN symbols caller ON cg.caller_id = caller.id
			JOIN symbols callee ON cg.callee_id = callee.id
			WHERE caller.file_path LIKE ? || '/%'
			  AND callee.file_path LIKE ? || '/%'
			  AND caller.pkg_path IS NOT NULL
			  AND callee.pkg_path IS NOT NULL
			  AND caller.pkg_path != callee.pkg_path
			GROUP BY caller.pkg_path, callee.pkg_path
			HAVING call_count >= 2
		)
		SELECT caller_pkg, callee_pkg, call_count
		FROM PackageCalls
		ORDER BY call_count DESC
		LIMIT 15
	`, repoRoot, repoRoot)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var edges []CrossPackageEdge
	for rows.Next() {
		var callerPkg, calleePkg string
		var count int
		if err := rows.Scan(&callerPkg, &calleePkg, &count); err != nil {
			continue
		}

		// Shorten package paths for display
		fromShort := shortenPackagePath(callerPkg)
		toShort := shortenPackagePath(calleePkg)

		edges = append(edges, CrossPackageEdge{
			From:  fromShort,
			To:    toShort,
			Count: count,
		})
	}

	return edges, rows.Err()
}

// shortenPackagePath extracts the short form of a package path.
// Example: "github.com/user/snipe/internal/store" -> "internal/store"
func shortenPackagePath(pkgPath string) string {
	// Find /internal/ or /cmd/ and take from there
	if idx := strings.Index(pkgPath, "/internal/"); idx != -1 {
		return pkgPath[idx+1:]
	}
	if idx := strings.Index(pkgPath, "/cmd/"); idx != -1 {
		return pkgPath[idx+1:]
	}
	if strings.HasSuffix(pkgPath, "/cmd") {
		return "cmd"
	}

	// Root module package — use last segment (project name)
	parts := strings.Split(pkgPath, "/")
	if len(parts) >= 1 {
		return parts[len(parts)-1]
	}

	return pkgPath
}
