package cmd

import (
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/dkoosis/snipe/internal/metrics"
	"github.com/dkoosis/snipe/internal/output"
)

var checkCmd = &cobra.Command{
	Use:    "check",
	Short:  "Check performance against baseline",
	Hidden: true,
	Long: `Compares current performance against a saved baseline.

Reports:
  ✅ PASS - Metric within acceptable range
  ⚠️  WARN - Metric changed but within threshold
  ❌ FAIL - Metric regressed beyond threshold

Examples:
  snipe check                             # Compare against BASELINE.json
  snipe check --baseline baselines/v1.json
  snipe check --fail-on-regression        # Exit 1 if any failures`,
	RunE: runCheck,
}

var (
	checkBaseline  string
	checkThreshold float64
	checkFailOnReg bool
)

func init() {
	checkCmd.Flags().StringVar(&checkBaseline, "baseline", "", "Reference baseline file (default: BASELINE.json)")
	checkCmd.Flags().Float64Var(&checkThreshold, "threshold", 15.0, "Regression threshold percentage")
	checkCmd.Flags().BoolVar(&checkFailOnReg, "fail-on-regression", false, "Exit with error code if regression detected")
	rootCmd.AddCommand(checkCmd)
}

func runCheck(cmd *cobra.Command, args []string) error {
	compact, _, _, _, _, _ := GetOutputConfig()
	w := output.NewWriter(os.Stdout, compact)

	dir, err := os.Getwd()
	if err != nil {
		return w.WriteError("check", &output.Error{
			Code:    output.ErrInternal,
			Message: "failed to get working directory: " + err.Error(),
		})
	}

	// Load reference baseline
	baselinePath := checkBaseline
	if baselinePath == "" {
		baselinePath = filepath.Join(dir, "BASELINE.json")
	}

	reference, err := metrics.LoadBaseline(baselinePath)
	if err != nil {
		return w.WriteError("check", &output.Error{
			Code:    output.ErrInternal,
			Message: "failed to load baseline: " + err.Error(),
		})
	}

	// Capture current metrics
	current, err := metrics.Capture(metrics.CaptureConfig{
		Dir:  dir,
		Name: reference.Codebase.Name,
	})
	if err != nil {
		return w.WriteError("check", &output.Error{
			Code:    output.ErrInternal,
			Message: "failed to capture current metrics: " + err.Error(),
		})
	}

	// Compare
	comparison := metrics.Compare(current, reference, metrics.CompareConfig{
		Threshold: checkThreshold,
	})

	jsonData, _ := comparison.ToJSON()
	_, _ = os.Stdout.Write(jsonData)     // G104: stdout write for output
	_, _ = os.Stdout.Write([]byte("\n")) // G104: stdout write for output

	if checkFailOnReg && comparison.HasFailure {
		os.Exit(1)
	}

	return nil
}
