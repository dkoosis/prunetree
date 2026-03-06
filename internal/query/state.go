package query

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dkoosis/snipe/internal/index"
	"github.com/dkoosis/snipe/internal/output"
)

// CheckIndexState computes current fingerprint and compares with stored
func CheckIndexState(db *sql.DB, repoRoot, version string) output.IndexState {
	// Compute current fingerprint
	current, err := index.ComputeFingerprint(repoRoot, version)
	if err != nil {
		return output.IndexMissing
	}

	// Get stored fingerprint
	var stored string
	err = db.QueryRow(`SELECT value FROM meta WHERE key = 'fingerprint'`).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) || stored == "" {
		return output.IndexMissing
	}
	if err != nil {
		return output.IndexMissing
	}

	// Compare
	if current.Combined == stored {
		return output.IndexFresh
	}
	return output.IndexStale
}

// CheckFileStaleness compares on-disk mtimes against stored mtimes for result files.
// Returns relative paths of files that changed since indexing (sorted, deterministic).
func CheckFileStaleness(db *sql.DB, repoRoot string, results []output.Result) []string {
	if len(results) == 0 {
		return nil
	}

	// Collect unique absolute file paths from results
	seen := make(map[string]struct{})
	var paths []string
	for i := range results {
		abs := results[i].FileAbs
		if abs == "" {
			continue
		}
		if _, ok := seen[abs]; ok {
			continue
		}
		seen[abs] = struct{}{}
		paths = append(paths, abs)
	}

	return checkPathStaleness(db, repoRoot, paths)
}

// CheckPathStaleness compares on-disk mtimes against stored mtimes for absolute file paths.
// Use this when results aren't output.Result (e.g., explain, types commands).
func CheckPathStaleness(db *sql.DB, repoRoot string, absPaths []string) []string {
	return checkPathStaleness(db, repoRoot, absPaths)
}

func checkPathStaleness(db *sql.DB, repoRoot string, paths []string) []string {
	if len(paths) == 0 {
		return nil
	}

	// Batch-query stored mtimes and hashes
	storedMeta, err := queryFileMetadata(db, paths)
	if err != nil {
		return nil // Don't block query on staleness check failure
	}

	// Compare with disk
	var stale []string
	for _, absPath := range paths {
		meta, ok := storedMeta[absPath]
		if !ok {
			// File not in index — treat as stale
			stale = append(stale, toRelPath(absPath, repoRoot))
			continue
		}

		info, err := os.Stat(absPath)
		if err != nil {
			// File deleted or inaccessible — treat as stale
			stale = append(stale, toRelPath(absPath, repoRoot))
			continue
		}

		if info.ModTime().Unix() > meta.mtime {
			// Mtime changed — check hash if available
			if meta.hash != "" {
				currentHash, hashErr := index.HashFileSHA256(absPath)
				if hashErr == nil && currentHash == meta.hash {
					// Content unchanged despite mtime bump — treat as fresh
					continue
				}
			}
			stale = append(stale, toRelPath(absPath, repoRoot))
		}
	}

	sort.Strings(stale)
	if len(stale) == 0 {
		return nil
	}
	return stale
}

// fileMeta holds stored file metadata for staleness comparison.
type fileMeta struct {
	mtime int64
	hash  string
}

// queryFileMetadata queries the files table for stored mtimes and hashes.
func queryFileMetadata(db *sql.DB, paths []string) (map[string]fileMeta, error) {
	if len(paths) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(paths))
	args := make([]interface{}, len(paths))
	for i, p := range paths {
		placeholders[i] = "?"
		args[i] = p
	}

	q := "SELECT path, mtime, COALESCE(hash, '') FROM files WHERE path IN (" + strings.Join(placeholders, ",") + ")"
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]fileMeta, len(paths))
	for rows.Next() {
		var path string
		var m fileMeta
		if err := rows.Scan(&path, &m.mtime, &m.hash); err != nil {
			continue
		}
		result[path] = m
	}
	return result, rows.Err()
}

// toRelPath converts an absolute file path to a repo-relative path.
func toRelPath(absPath, repoRoot string) string {
	if repoRoot == "" {
		return absPath
	}
	rel, err := filepath.Rel(repoRoot, absPath)
	if err != nil {
		return absPath
	}
	return strings.ReplaceAll(rel, "\\", "/")
}
