package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/dkoosis/snipe/internal/embed"
	"github.com/dkoosis/snipe/internal/output"
	"github.com/dkoosis/snipe/internal/store"
)

var embedCmd = &cobra.Command{
	Use:     "embed-status",
	Short:   "Check status of batch embedding job",
	GroupID: "advanced",
	Long: `Check the status of an async batch embedding job.

If the batch is complete, downloads results and saves embeddings to the index.
Use --wait to poll until completion.`,
	RunE: runEmbedStatus,
}

// Batch status constants.
const (
	batchStatusFailed    = "failed"
	batchStatusCancelled = "cancelled"
	batchStatusCompleted = "completed"
)

var (
	embedWait     bool
	embedPollSecs int
)

func init() {
	embedCmd.Flags().BoolVar(&embedWait, "wait", false, "Wait for batch to complete")
	embedCmd.Flags().IntVar(&embedPollSecs, "poll", 30, "Poll interval in seconds (with --wait)")
	rootCmd.AddCommand(embedCmd)
}

// EmbedStatusResult is the output for embed-status command.
type EmbedStatusResult struct {
	BatchID    string    `json:"batch_id,omitempty"`
	Status     string    `json:"status"`
	Total      int       `json:"total,omitempty"`
	Completed  int       `json:"completed,omitempty"`
	Failed     int       `json:"failed,omitempty"`
	Model      string    `json:"model,omitempty"`
	CreatedAt  time.Time `json:"created_at,omitempty"`
	Age        string    `json:"age,omitempty"`
	Stale      bool      `json:"stale,omitempty"`
	EmbedCount int       `json:"embed_count,omitempty"`
	Message    string    `json:"message,omitempty"`
}

func runEmbedStatus(cmd *cobra.Command, args []string) error {
	// Determine directory
	dir := "."
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	snipeDir := filepath.Join(absDir, ".snipe")
	dbPath := store.DefaultIndexPath(absDir)

	// Setup output writer
	compact, _, _, _, _, _ := GetOutputConfig()
	w := output.NewWriter(os.Stdout, compact)

	// Load batch client
	client, err := embed.NewBatchClient(snipeDir)
	if err != nil {
		return fmt.Errorf("create batch client: %w", err)
	}

	// Load state
	state, err := client.LoadState()
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	if state == nil {
		result := EmbedStatusResult{
			Status:  "no_batch",
			Message: "No batch embedding job found. Run 'snipe index' to start one.",
		}
		return w.WriteResponse(output.Response[EmbedStatusResult]{
			Protocol: output.ProtocolVersion,
			Ok:       true,
			Results:  []EmbedStatusResult{result},
			Meta:     output.Meta{Command: "embed-status"},
		})
	}

	// Poll loop (or single check)
	for {
		// Get current status from Voyage
		batchStatus, err := client.GetBatchStatus(state.BatchID)
		if err != nil {
			return fmt.Errorf("get batch status: %w", err)
		}

		// Update local state
		state.Status = batchStatus.Status
		state.Completed = batchStatus.RequestCounts.Completed
		state.Failed = batchStatus.RequestCounts.Failed
		state.OutputFileID = batchStatus.OutputFileID
		state.ErrorFileID = batchStatus.ErrorFileID
		state.UpdatedAt = time.Now()

		if err := client.SaveState(state); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to save state: %v\n", err)
		}

		fmt.Fprintf(os.Stderr, "Batch %s: status=%s, completed=%d/%d, failed=%d\n",
			state.BatchID, state.Status, state.Completed, state.Total, state.Failed)

		// Handle completion
		if state.Status == batchStatusCompleted {
			embedCount, err := downloadAndSaveEmbeddings(client, state, dbPath)
			if err != nil {
				return fmt.Errorf("download embeddings: %w", err)
			}

			// Clear batch state
			if err := client.ClearState(); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to clear state: %v\n", err)
			}

			result := EmbedStatusResult{
				BatchID:    state.BatchID,
				Status:     batchStatusCompleted,
				Total:      state.Total,
				Completed:  state.Completed,
				Failed:     state.Failed,
				Model:      state.Model,
				CreatedAt:  state.CreatedAt,
				EmbedCount: embedCount,
				Message:    fmt.Sprintf("Downloaded and saved %d embeddings", embedCount),
			}
			return w.WriteResponse(output.Response[EmbedStatusResult]{
				Protocol: output.ProtocolVersion,
				Ok:       true,
				Results:  []EmbedStatusResult{result},
				Meta:     output.Meta{Command: "embed-status"},
			})
		}

		// Handle failure
		if state.Status == batchStatusFailed || state.Status == batchStatusCancelled {
			if err := client.ClearState(); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to clear state: %v\n", err)
			}

			result := EmbedStatusResult{
				BatchID:   state.BatchID,
				Status:    state.Status,
				Total:     state.Total,
				Completed: state.Completed,
				Failed:    state.Failed,
				Model:     state.Model,
				CreatedAt: state.CreatedAt,
				Message:   "Batch job failed or was cancelled",
			}
			return w.WriteResponse(output.Response[EmbedStatusResult]{
				Protocol: output.ProtocolVersion,
				Ok:       true,
				Results:  []EmbedStatusResult{result},
				Meta:     output.Meta{Command: "embed-status"},
			})
		}

		// If not waiting, return current status
		if !embedWait {
			age := time.Since(state.CreatedAt)
			isStale := age > batchStaleThreshold
			msg := "Batch job in progress"
			if isStale {
				msg = fmt.Sprintf("Batch job appears stale (no progress in %v). Run 'snipe index' to auto-recover.", age.Round(time.Hour))
			}
			result := EmbedStatusResult{
				BatchID:   state.BatchID,
				Status:    state.Status,
				Total:     state.Total,
				Completed: state.Completed,
				Failed:    state.Failed,
				Model:     state.Model,
				CreatedAt: state.CreatedAt,
				Age:       age.Round(time.Minute).String(),
				Stale:     isStale,
				Message:   msg,
			}
			return w.WriteResponse(output.Response[EmbedStatusResult]{
				Protocol: output.ProtocolVersion,
				Ok:       true,
				Results:  []EmbedStatusResult{result},
				Meta:     output.Meta{Command: "embed-status"},
			})
		}

		// Wait and poll again
		time.Sleep(time.Duration(embedPollSecs) * time.Second)
	}
}

// downloadAndSaveEmbeddings streams batch results directly to the store.
// Downloads and parses line-by-line to avoid buffering the entire payload in RAM.
func downloadAndSaveEmbeddings(client *embed.BatchClient, state *embed.BatchState, dbPath string) (int, error) {
	if state.OutputFileID == "" {
		return 0, fmt.Errorf("no output file available")
	}

	fmt.Fprintf(os.Stderr, "Downloading results from file %s...\n", state.OutputFileID)

	body, err := client.DownloadFile(state.OutputFileID)
	if err != nil {
		return 0, fmt.Errorf("download output file: %w", err)
	}
	defer body.Close()

	// Open store before streaming so we can persist each embedding immediately.
	s, err := store.Open(dbPath)
	if err != nil {
		return 0, fmt.Errorf("open store: %w", err)
	}
	defer s.Close()

	count := 0
	if err := client.ParseBatchResults(body, func(symbolID string, embedding []float32) error {
		if err := s.SaveEmbedding(symbolID, embedding, state.Model); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to save embedding for %s: %v\n", symbolID, err)
			return nil // continue on save errors
		}
		count++
		return nil
	}); err != nil {
		return count, fmt.Errorf("parse results: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Saved %d embeddings to index\n", count)

	return count, nil
}
