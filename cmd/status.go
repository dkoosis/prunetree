package cmd

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/dkoosis/snipe/internal/output"
	"github.com/dkoosis/snipe/internal/query"
	"github.com/dkoosis/snipe/internal/store"
)

var statusCmd = &cobra.Command{
	Use:     "status",
	Short:   "Show index status and statistics",
	GroupID: "index",
	Long: `Shows the current state of the snipe index including:
- Whether the index is fresh, stale, or missing
- Git commit at time of indexing
- Symbol, reference, and call edge counts

Examples:
  snipe status           # Show index status
  snipe status --json    # Output as JSON`,
	RunE: runStatus,
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

// StatusResponse is the JSON response for status command.
type StatusResponse struct {
	State       output.IndexState `json:"state"`
	Commit      string            `json:"commit,omitempty"`
	IndexedAt   string            `json:"indexed_at,omitempty"`
	Symbols     int               `json:"symbols"`
	Refs        int               `json:"refs"`
	Calls       int               `json:"calls"`
	Fingerprint string            `json:"fingerprint,omitempty"`
}

func runStatus(cmd *cobra.Command, args []string) error {
	compact, _, _, _, _, _ := GetOutputConfig()
	w := output.NewWriter(os.Stdout, compact)

	// Find repo root
	dir := findProjectRoot(".")
	if dir == "" {
		return w.WriteError("status", &output.Error{
			Code:    output.ErrInternal,
			Message: "not in a git repository",
		})
	}

	// Check if index exists
	dbPath := store.DefaultIndexPath(dir)
	if !store.Exists(dbPath) {
		resp := output.Response[StatusResponse]{
			Protocol: output.ProtocolVersion,
			Ok:       true,
			Results: []StatusResponse{{
				State: output.IndexMissing,
			}},
			Meta: output.Meta{
				Command:    "status",
				RepoRoot:   dir,
				IndexState: output.IndexMissing,
				Ms:         w.Elapsed(),
				Total:      1,
			},
		}
		return w.WriteResponse(resp)
	}

	// Open store (read-only mode)
	s, err := store.Open(dbPath)
	if err != nil {
		return w.WriteError("status", &output.Error{
			Code:    output.ErrInternal,
			Message: err.Error(),
		})
	}
	defer s.Close()

	// Get stats
	symbols, refs, calls, err := s.GetStats()
	if err != nil {
		return w.WriteError("status", &output.Error{
			Code:    output.ErrInternal,
			Message: "failed to get stats: " + err.Error(),
		})
	}

	// Get meta values
	indexedAt, _ := s.GetMeta("indexed_at")
	commit, _ := s.GetMeta("git_commit")
	fingerprint, _ := s.GetMeta("fingerprint")

	// Check index state
	state := query.CheckIndexState(s.DB(), dir, Version)

	// JSON response
	resp := output.Response[StatusResponse]{
		Protocol: output.ProtocolVersion,
		Ok:       true,
		Results: []StatusResponse{{
			State:       state,
			Commit:      commit,
			IndexedAt:   indexedAt,
			Symbols:     symbols,
			Refs:        refs,
			Calls:       calls,
			Fingerprint: fingerprint,
		}},
		Meta: output.Meta{
			Command:    "status",
			RepoRoot:   dir,
			IndexState: state,
			Ms:         w.Elapsed(),
			Total:      1,
		},
	}

	return w.WriteResponse(resp)
}
