package index

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ChangeResult describes what changed since the last index.
type ChangeResult struct {
	HasChanges bool
	Modified   []string // files with changed content
	Added      []string // new files not in previous index
	Deleted    []string // files in previous index but no longer exist
	Unchanged  int      // count of unchanged files
}

// DetectChanges compares the current filesystem against stored file metadata.
// Uses mtime as a fast pre-filter, only computing SHA256 when mtime differs.
// Pass nil for stored on first index (all files will be reported as added).
func DetectChanges(dir string, stored map[string]FileInfo, exclude []string) (*ChangeResult, error) {
	if stored == nil {
		stored = make(map[string]FileInfo)
	}
	if exclude == nil {
		exclude = DefaultExclude()
	}

	result := &ChangeResult{}
	current := make(map[string]struct{})

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // Skip unreadable entries rather than aborting walk
		}

		if d.IsDir() {
			base := d.Name()
			// Skip hidden directories (.git, .snipe, etc.)
			if strings.HasPrefix(base, ".") && base != "." {
				return filepath.SkipDir
			}
			for _, pat := range exclude {
				if base == pat {
					return filepath.SkipDir
				}
			}
			return nil
		}

		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}

		current[path] = struct{}{}

		prev, exists := stored[path]
		if !exists {
			result.Added = append(result.Added, path)
			result.HasChanges = true
			return nil
		}

		// Get file info for mtime check
		info, infoErr := d.Info()
		if infoErr != nil {
			// Can't stat — conservatively treat as modified
			result.Modified = append(result.Modified, path)
			result.HasChanges = true
			return nil //nolint:nilerr // Stat failure treated as change, not walk abort
		}

		// Fast path: mtime unchanged means content unchanged
		if info.ModTime().Unix() == prev.Mtime {
			result.Unchanged++
			return nil
		}

		// Mtime changed — verify with content hash
		hash, hashErr := HashFileSHA256(path)
		if hashErr != nil {
			// Can't hash — conservatively treat as modified
			result.Modified = append(result.Modified, path)
			result.HasChanges = true
			return nil //nolint:nilerr // Hash failure treated as change, not walk abort
		}

		if hash == prev.Hash {
			// Content unchanged despite mtime change (e.g., touch, git checkout)
			result.Unchanged++
			return nil
		}

		result.Modified = append(result.Modified, path)
		result.HasChanges = true
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk directory: %w", err)
	}

	// Check for deleted files
	for path := range stored {
		if _, exists := current[path]; !exists {
			result.Deleted = append(result.Deleted, path)
			result.HasChanges = true
		}
	}

	return result, nil
}

// Summary returns a human-readable summary of changes.
func (cr *ChangeResult) Summary() string {
	if !cr.HasChanges {
		return fmt.Sprintf("no changes (%d files)", cr.Unchanged)
	}
	var parts []string
	if len(cr.Modified) > 0 {
		parts = append(parts, fmt.Sprintf("%d modified", len(cr.Modified)))
	}
	if len(cr.Added) > 0 {
		parts = append(parts, fmt.Sprintf("%d added", len(cr.Added)))
	}
	if len(cr.Deleted) > 0 {
		parts = append(parts, fmt.Sprintf("%d deleted", len(cr.Deleted)))
	}
	return fmt.Sprintf("%s, %d unchanged", strings.Join(parts, ", "), cr.Unchanged)
}

// TotalChanged returns the total number of changed files.
func (cr *ChangeResult) TotalChanged() int {
	return len(cr.Modified) + len(cr.Added) + len(cr.Deleted)
}
