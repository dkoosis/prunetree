package index

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

// FileInfo represents file metadata for change detection
type FileInfo struct {
	Path  string
	Mtime int64
	Hash  string
}

// ExtractFileInfo collects file hashes from loaded packages.
// Returns a map of file path to FileInfo for efficient lookup.
func ExtractFileInfo(result *LoadResult) ([]FileInfo, error) {
	// Collect unique files
	seen := make(map[string]struct{})
	var files []FileInfo

	for _, pkg := range result.Packages {
		for _, path := range pkg.GoFiles {
			if _, ok := seen[path]; ok {
				continue
			}
			seen[path] = struct{}{}

			info, err := computeFileInfo(path)
			if err != nil {
				// Skip files that can't be hashed (may have been deleted)
				continue
			}
			files = append(files, info)
		}
	}

	return files, nil
}

// computeFileInfo computes the hash and mtime for a file
func computeFileInfo(path string) (FileInfo, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return FileInfo{}, err
	}

	hash, err := HashFileSHA256(path)
	if err != nil {
		return FileInfo{}, err
	}

	return FileInfo{
		Path:  path,
		Mtime: stat.ModTime().Unix(),
		Hash:  hash,
	}, nil
}

// HashFileSHA256 computes a truncated SHA256 hash of a file (16 hex chars).
// Used during indexing and for staleness checks at query time.
func HashFileSHA256(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- path from go/packages load result
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}

	// Return first 8 bytes as hex (16 characters)
	return hex.EncodeToString(h.Sum(nil)[:8]), nil
}
