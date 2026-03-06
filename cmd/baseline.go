package cmd

import (
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/dkoosis/snipe/internal/metrics"
	"github.com/dkoosis/snipe/internal/output"
)

var baselineCmd = &cobra.Command{
	Use:    "baseline",
	Short:  "Capture performance baseline for a codebase",
	Hidden: true,
	Long: `Captures performance and quality metrics as a baseline.

The baseline includes:
- Index time, size, symbol/ref counts
- Query latencies (def by name, def by position, refs)
- Search latencies (simple and regex patterns)
- Quality metrics (doc coverage, call graph coverage)

Examples:
  snipe baseline                      # Capture baseline for current directory
  snipe baseline --output base.json   # Save to specific file
  snipe baseline --name myproject     # Name the baseline`,
	RunE: runBaseline,
}

var (
	baselineOutput string
	baselineName   string
)

func init() {
	baselineCmd.Flags().StringVarP(&baselineOutput, "output", "o", "", "Output file (default: BASELINE.json)")
	baselineCmd.Flags().StringVar(&baselineName, "name", "", "Codebase name for the baseline")
	rootCmd.AddCommand(baselineCmd)
}

func runBaseline(cmd *cobra.Command, args []string) error {
	compact, _, _, _, _, _ := GetOutputConfig()
	w := output.NewWriter(os.Stdout, compact)

	dir, err := os.Getwd()
	if err != nil {
		return w.WriteError("baseline", &output.Error{
			Code:    output.ErrInternal,
			Message: "failed to get working directory: " + err.Error(),
		})
	}

	// Use directory name as default baseline name
	name := baselineName
	if name == "" {
		name = filepath.Base(dir)
	}

	baseline, err := metrics.Capture(metrics.CaptureConfig{
		Dir:  dir,
		Name: name,
	})
	if err != nil {
		return w.WriteError("baseline", &output.Error{
			Code:    output.ErrInternal,
			Message: "failed to capture baseline: " + err.Error(),
		})
	}

	// Output JSON
	jsonData, err := baseline.ToJSON()
	if err != nil {
		return w.WriteError("baseline", &output.Error{
			Code:    output.ErrInternal,
			Message: "failed to serialize baseline: " + err.Error(),
		})
	}

	// Write to output file if specified
	outputFile := baselineOutput
	if outputFile == "" {
		outputFile = filepath.Join(dir, "BASELINE.json")
	}

	if err := os.WriteFile(outputFile, jsonData, 0600); err != nil { // #nosec G306 -- baseline is project data, not secrets
		return w.WriteError("baseline", &output.Error{
			Code:    output.ErrInternal,
			Message: "failed to write baseline file: " + err.Error(),
		})
	}

	// Append to history
	historyDir := filepath.Join(dir, ".snipe")
	if err := os.MkdirAll(historyDir, 0750); err == nil {
		historyFile := filepath.Join(historyDir, "metrics.jsonl")
		jsonl, _ := baseline.ToJSONL()
		if f, err := os.OpenFile(historyFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600); err == nil { // #nosec G304 -- path derived from cwd
			_, _ = f.Write(jsonl)        // G104: best-effort append to history
			_, _ = f.Write([]byte("\n")) // G104: best-effort append
			_ = f.Close()                // G104: close in cleanup path
		}
	}

	_, _ = os.Stdout.Write(jsonData)     // G104: stdout write for output
	_, _ = os.Stdout.Write([]byte("\n")) // G104: stdout write for output

	return nil
}
