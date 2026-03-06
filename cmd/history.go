package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/dkoosis/snipe/internal/metrics"
	"github.com/dkoosis/snipe/internal/output"
)

var historyCmd = &cobra.Command{
	Use:    "history",
	Short:  "Show performance history over time",
	Hidden: true,
	Long: `Shows performance trends from baseline history.

Reads from .snipe/metrics.jsonl to show how metrics have
changed across commits.

Examples:
  snipe history           # Show recent history
  snipe history --limit 5 # Show last 5 entries`,
	RunE: runHistory,
}

var (
	historyLimit int
)

func init() {
	historyCmd.Flags().IntVarP(&historyLimit, "limit", "n", 10, "Number of entries to show")
	rootCmd.AddCommand(historyCmd)
}

func runHistory(cmd *cobra.Command, args []string) error {
	compact, _, _, _, _, _ := GetOutputConfig()
	w := output.NewWriter(os.Stdout, compact)

	dir, err := os.Getwd()
	if err != nil {
		return w.WriteError("history", &output.Error{
			Code:    output.ErrInternal,
			Message: "failed to get working directory: " + err.Error(),
		})
	}

	historyFile := filepath.Join(dir, ".snipe", "metrics.jsonl")
	baselines, err := metrics.LoadHistory(historyFile)
	if err != nil {
		return w.WriteError("history", &output.Error{
			Code:    output.ErrNotFound,
			Message: "no history found at " + historyFile,
		})
	}

	if len(baselines) == 0 {
		return w.WriteError("history", &output.Error{
			Code:    output.ErrNotFound,
			Message: "history file is empty",
		})
	}

	// Limit entries
	if historyLimit > 0 && len(baselines) > historyLimit {
		baselines = baselines[len(baselines)-historyLimit:]
	}

	entries := metrics.ToHistoryEntries(baselines)

	jsonData, _ := json.MarshalIndent(entries, "", "  ")
	_, _ = os.Stdout.Write(jsonData)     // G104: stdout write for output
	_, _ = os.Stdout.Write([]byte("\n")) // G104: stdout write for output

	return nil
}
