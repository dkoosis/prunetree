// Package util provides shared utilities for snipe operations.
package util

import (
	"bufio"
	"os"
	"sync"
	"time"
)

// FileCache provides LRU-cached file line reading.
type FileCache struct {
	mu       sync.RWMutex
	cache    map[string]*cachedFile
	maxFiles int
}

type cachedFile struct {
	lines      []string
	accessTime time.Time
}

// DefaultMaxCachedFiles is the default maximum number of files to cache.
const DefaultMaxCachedFiles = 100

// NewFileCache creates a new file cache with the specified maximum size.
func NewFileCache(maxFiles int) *FileCache {
	if maxFiles <= 0 {
		maxFiles = DefaultMaxCachedFiles
	}
	return &FileCache{
		cache:    make(map[string]*cachedFile),
		maxFiles: maxFiles,
	}
}

// LoadLines reads a file and returns its lines, using cache if available.
func (fc *FileCache) LoadLines(path string) ([]string, error) {
	// Check cache first (write lock needed to update accessTime)
	fc.mu.Lock()
	if cached, ok := fc.cache[path]; ok {
		cached.accessTime = time.Now()
		lines := cached.lines
		fc.mu.Unlock()
		return lines, nil
	}
	fc.mu.Unlock()

	// Read file
	lines, err := LoadFileLines(path)
	if err != nil {
		return nil, err
	}

	// Cache the result with LRU eviction
	fc.mu.Lock()
	defer fc.mu.Unlock()

	// Evict oldest entries if at capacity
	for len(fc.cache) >= fc.maxFiles {
		fc.evictOldest()
	}

	fc.cache[path] = &cachedFile{
		lines:      lines,
		accessTime: time.Now(),
	}

	return lines, nil
}

// evictOldest removes the least recently accessed entry.
// Must be called with mu held.
func (fc *FileCache) evictOldest() {
	var oldestPath string
	var oldestTime time.Time

	for path, cached := range fc.cache {
		if oldestPath == "" || cached.accessTime.Before(oldestTime) {
			oldestPath = path
			oldestTime = cached.accessTime
		}
	}

	if oldestPath != "" {
		delete(fc.cache, oldestPath)
	}
}

// Clear removes all entries from the cache.
func (fc *FileCache) Clear() {
	fc.mu.Lock()
	fc.cache = make(map[string]*cachedFile)
	fc.mu.Unlock()
}

// Size returns the number of cached files.
func (fc *FileCache) Size() int {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	return len(fc.cache)
}

// LoadFileLines reads a file and returns its lines without caching.
// This is useful for one-off reads during indexing.
func LoadFileLines(path string) ([]string, error) {
	f, err := os.Open(path) // #nosec G304 -- path from caller (file cache, indexing)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	// Increase buffer for long lines (minified code, long strings)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}
