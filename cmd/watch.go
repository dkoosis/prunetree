package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/cobra"
)

var (
	watchDebounce int
	watchVerbose  bool
)

// WatchEvent represents a reindex event for JSON output.
type WatchEvent struct {
	Event     string   `json:"event"`
	Files     []string `json:"files,omitempty"`
	Ms        int64    `json:"ms,omitempty"`
	Error     string   `json:"error,omitempty"`
	Timestamp string   `json:"timestamp"`
}

var watchCmd = &cobra.Command{
	Use:    "watch",
	Short:  "Watch for file changes and reindex",
	Hidden: true,
	Long: `Watches the current directory for Go file changes and triggers reindexing.

V1 Implementation:
  - Uses fsnotify for file system events
  - Debounces rapid changes (default 500ms)
  - Triggers full 'snipe index' on changes
  - Emits JSON events for agent consumption

Events emitted:
  {"event": "started", "timestamp": "..."}
  {"event": "change_detected", "files": [...], "timestamp": "..."}
  {"event": "reindex_started", "timestamp": "..."}
  {"event": "reindexed", "files": [...], "ms": 123, "timestamp": "..."}
  {"event": "error", "error": "...", "timestamp": "..."}

Note: V2 will add incremental file-level reindexing.

Examples:
  snipe watch                    # Default 500ms debounce
  snipe watch --debounce 1000    # 1 second debounce
  snipe watch --verbose          # Include change details`,
	RunE: runWatch,
}

func init() {
	watchCmd.Flags().IntVar(&watchDebounce, "debounce", 500, "Debounce time in milliseconds")
	watchCmd.Flags().BoolVar(&watchVerbose, "verbose", false, "Verbose output")
	rootCmd.AddCommand(watchCmd)
}

func runWatch(cmd *cobra.Command, args []string) error {
	dir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create watcher: %w", err)
	}
	defer watcher.Close()

	// Add directories recursively
	if err := addWatchDirs(watcher, dir); err != nil {
		return fmt.Errorf("add watch directories: %w", err)
	}

	emitEvent(WatchEvent{Event: "started", Timestamp: time.Now().Format(time.RFC3339)})

	// Debounce timer
	debounceTimer := time.NewTimer(0)
	<-debounceTimer.C // Drain initial timer

	var pendingFiles []string
	debounceDuration := time.Duration(watchDebounce) * time.Millisecond

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}

			// Only watch .go files
			if !strings.HasSuffix(event.Name, ".go") {
				continue
			}

			// Skip temporary files
			if strings.Contains(event.Name, "~") || strings.HasPrefix(filepath.Base(event.Name), ".") {
				continue
			}

			// Track changed file
			relPath, _ := filepath.Rel(dir, event.Name)
			if relPath == "" {
				relPath = event.Name
			}

			// Avoid duplicates
			found := false
			for _, f := range pendingFiles {
				if f == relPath {
					found = true
					break
				}
			}
			if !found {
				pendingFiles = append(pendingFiles, relPath)
			}

			// Reset debounce timer (stop+drain before reset to avoid race)
			if !debounceTimer.Stop() {
				select {
				case <-debounceTimer.C:
				default:
				}
			}
			debounceTimer.Reset(debounceDuration)

			if watchVerbose {
				emitEvent(WatchEvent{
					Event:     "change_detected",
					Files:     []string{relPath},
					Timestamp: time.Now().Format(time.RFC3339),
				})
			}

		case <-debounceTimer.C:
			if len(pendingFiles) == 0 {
				continue
			}

			files := pendingFiles
			pendingFiles = nil

			emitEvent(WatchEvent{
				Event:     "reindex_started",
				Files:     files,
				Timestamp: time.Now().Format(time.RFC3339),
			})

			start := time.Now()
			err := runReindex(dir)
			elapsed := time.Since(start)

			if err != nil {
				emitEvent(WatchEvent{
					Event:     "error",
					Error:     err.Error(),
					Timestamp: time.Now().Format(time.RFC3339),
				})
			} else {
				emitEvent(WatchEvent{
					Event:     "reindexed",
					Files:     files,
					Ms:        elapsed.Milliseconds(),
					Timestamp: time.Now().Format(time.RFC3339),
				})
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			emitEvent(WatchEvent{
				Event:     "error",
				Error:     err.Error(),
				Timestamp: time.Now().Format(time.RFC3339),
			})

		case <-GetContext().Done():
			emitEvent(WatchEvent{
				Event:     "stopped",
				Timestamp: time.Now().Format(time.RFC3339),
			})
			return GetContext().Err()
		}
	}
}

// addWatchDirs recursively adds directories to the watcher.
func addWatchDirs(watcher *fsnotify.Watcher, root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil //nolint:nilerr // Skip inaccessible directories gracefully
		}

		// Skip hidden directories and common non-source directories
		base := filepath.Base(path)
		if strings.HasPrefix(base, ".") || base == "vendor" || base == "node_modules" || base == "testdata" {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if info.IsDir() {
			if err := watcher.Add(path); err != nil {
				// Don't fail on permission errors
				if watchVerbose {
					fmt.Fprintf(os.Stderr, "Warning: could not watch %s: %v\n", path, err)
				}
			}
		}

		return nil
	})
}

// runReindex runs 'snipe index' as a subprocess.
// V1: Full reindex. V2 will add incremental support.
func runReindex(dir string) error {
	// Find the snipe binary
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}

	cmd := exec.CommandContext(GetContext(), exe, "index") // #nosec G204 -- exe from os.Executable(), trusted self-invocation
	cmd.Dir = dir
	cmd.Stdout = os.Stderr // Redirect index output to stderr
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// emitEvent outputs a JSON event to stdout.
func emitEvent(event WatchEvent) {
	data, _ := json.Marshal(event)
	fmt.Println(string(data))
}
