package query

import (
	"database/sql"
	"path/filepath"
	"strings"
)

const pkgMain = "main"

// ResolvePkgPattern translates special package patterns ("main", ".") into
// actual pkg_path values before they hit SQL. Without this, "main" matches
// nothing (the indexed pkg_path is the full module path) and "." matches
// everything (every pkg_path contains a dot).
func ResolvePkgPattern(db *sql.DB, pattern string, cwd string, repoRoot string) string {
	switch pattern {
	case pkgMain:
		return resolveMain(db)
	case ".":
		return resolveDot(db, cwd, repoRoot)
	default:
		// Try resolving short package names (no "/" or ".") via suffix matching
		if !strings.Contains(pattern, "/") && !strings.Contains(pattern, ".") {
			if resolved := resolveShortName(db, pattern); resolved != "" {
				return resolved
			}
		}
		return pattern
	}
}

// resolveMain returns the module root package path (shortest pkg_path in the index).
func resolveMain(db *sql.DB) string {
	var pkgPath string
	err := db.QueryRow(`
		SELECT DISTINCT pkg_path FROM symbols
		ORDER BY LENGTH(pkg_path)
		LIMIT 1
	`).Scan(&pkgPath)
	if err != nil {
		return pkgMain // fallback to original if DB query fails
	}
	return pkgPath
}

// resolveShortName resolves a short package name (e.g., "store") to a full
// pkg_path by suffix-matching against indexed symbols. Returns empty string
// if no unique match found.
func resolveShortName(db *sql.DB, name string) string {
	var pkgPath string
	// Find shortest pkg_path ending with /<name>. Using shortest ensures
	// we pick the most local match (e.g., "internal/store" over "vendor/x/store").
	err := db.QueryRow(`
		SELECT DISTINCT pkg_path FROM symbols
		WHERE pkg_path LIKE '%/' || ?
		ORDER BY LENGTH(pkg_path)
		LIMIT 1
	`, name).Scan(&pkgPath)
	if err != nil {
		return ""
	}
	return pkgPath
}

// resolveDot computes the package path for the current working directory.
func resolveDot(db *sql.DB, cwd string, repoRoot string) string {
	modulePath := resolveMain(db)
	if modulePath == pkgMain {
		return "." // couldn't resolve, return unchanged
	}

	// Resolve symlinks to handle macOS /var -> /private/var and similar
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	if resolved, err := filepath.EvalSymlinks(repoRoot); err == nil {
		repoRoot = resolved
	}

	rel, err := filepath.Rel(repoRoot, cwd)
	if err != nil || rel == "." {
		return modulePath // cwd is repo root
	}

	// Convert OS path separators to forward slashes for Go package paths
	rel = strings.ReplaceAll(rel, string(filepath.Separator), "/")
	return modulePath + "/" + rel
}
