package cmd

import (
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/dkoosis/snipe/internal/output"
	"github.com/dkoosis/snipe/internal/query"
	"github.com/dkoosis/snipe/internal/store"
)

var pkgCmd = &cobra.Command{
	Use:     "pkg <name>",
	Short:   "Show package overview with exported symbols",
	GroupID: "advanced",
	Long: `Shows an overview of a package including its exported symbols.

Displays all exported types, functions, constants, and variables in a package,
organized by kind for easy navigation.

Examples:
  snipe pkg store              # Show exported symbols in store package
  snipe pkg internal/query     # Show exported symbols in internal/query
  snipe pkg output             # Show exported symbols matching 'output'`,
	Args: cobra.ExactArgs(1),
	RunE: runPkg,
}

func init() {
	rootCmd.AddCommand(pkgCmd)
}

func runPkg(cmd *cobra.Command, args []string) error {
	start := time.Now()

	compact, lim, off, contextLines, withBody, _ := GetOutputConfig()
	format := GetResponseFormat()

	// pkg is an orientation command — show full surface by default
	if !cmd.Flags().Changed("limit") {
		lim = 200
	}
	withBody, _, contextLines = ApplyFormatOverrides(format, withBody, false, contextLines)
	summary := format == FormatSummary
	w := output.NewWriter(os.Stdout, compact)

	pkgPattern := args[0]

	dir, err := os.Getwd()
	if err != nil {
		return w.WriteError("pkg", &output.Error{
			Code:    output.ErrInternal,
			Message: "failed to get working directory: " + err.Error(),
		})
	}

	dbPath := store.DefaultIndexPath(dir)
	if store.IsIndexing(dbPath) {
		return w.WriteError("pkg", output.NewIndexInProgressError())
	}
	if !store.Exists(dbPath) {
		return w.WriteError("pkg", output.NewMissingIndexError())
	}

	s, err := store.Open(dbPath)
	if err != nil {
		return w.WriteError("pkg", &output.Error{
			Code:    output.ErrInternal,
			Message: "failed to open index: " + err.Error(),
		})
	}
	defer s.Close()

	repoRoot, _ := s.GetMeta("repo_root")
	pkgPattern = query.ResolvePkgPattern(s.DB(), pkgPattern, dir, repoRoot)

	queryInfo := map[string]string{"package": pkgPattern}

	// Find package symbols
	symbols, err := query.FindPackageSymbols(s.DB(), pkgPattern, lim, off)
	if err != nil {
		return w.WriteError("pkg", &output.Error{
			Code:    output.ErrInternal,
			Message: err.Error(),
		})
	}

	if len(symbols) == 0 {
		return w.WriteError("pkg", &output.Error{
			Code:    output.ErrNotFound,
			Message: "no exported symbols found in package matching: " + pkgPattern,
		})
	}

	// Convert to results
	results := make([]output.Result, len(symbols))
	tokenEstimate := 0
	var degraded []string

	for i, sym := range symbols {
		result := sym.ToResult()

		// Add body if requested
		if withBody {
			if err := output.AddBody(&result); err != nil {
				degraded = append(degraded, "body_extraction_failed")
			}
		}

		// Add context lines if requested (only if not showing full body)
		if contextLines > 0 && !withBody {
			if err := output.AddContext(&result, contextLines); err != nil {
				degraded = append(degraded, "context_extraction_failed")
			}
		}

		results[i] = result
		tokenEstimate += output.EstimateTokens(sym.Signature.String)
		if result.Body != "" {
			tokenEstimate = output.EstimateTokens(result.Body)
		}
	}

	// Deduplicate degraded messages
	degraded = uniqueStrings(degraded)

	// Apply token budget truncation if specified
	maxTok := GetMaxTokens()
	tokenTruncated := false
	if maxTok > 0 {
		results, tokenTruncated = output.TruncateToTokenBudget(results, maxTok)
	}

	staleFiles := query.CheckFileStaleness(s.DB(), dir, results)

	// If summary mode, return condensed output
	if summary {
		summaryData := output.BuildSummary(results)
		summaryResp := output.Response[output.Summary]{
			Protocol: output.ProtocolVersion,
			Ok:       true,
			Results:  []output.Summary{summaryData},
			Meta: output.Meta{
				Command:    "pkg",
				Query:      queryInfo,
				RepoRoot:   dir,
				IndexState: query.CheckIndexState(s.DB(), dir, Version),
				Degraded:   degraded,
				Ms:         time.Since(start).Milliseconds(),
				Total:      summaryData.Total,
				Offset:     off,
				Limit:      lim,
				Truncated:  len(results) >= lim,
				StaleFiles: staleFiles,
			},
		}
		return w.WriteResponse(summaryResp)
	}

	// Recalculate token estimate after truncation
	tokenEstimate = 0
	for i := range results {
		tokenEstimate += output.EstimateResultTokens(&results[i])
	}

	resp := output.Response[output.Result]{
		Protocol: output.ProtocolVersion,
		Ok:       true,
		Results:  results,
		Meta: output.Meta{
			Command:       "pkg",
			Query:         queryInfo,
			RepoRoot:      dir,
			IndexState:    query.CheckIndexState(s.DB(), dir, Version),
			Degraded:      degraded,
			Ms:            time.Since(start).Milliseconds(),
			Total:         len(results),
			Offset:        off,
			Limit:         lim,
			Truncated:     len(results) >= lim || tokenTruncated,
			TokenEstimate: tokenEstimate,
			StaleFiles:    staleFiles,
		},
	}

	return w.WriteResponse(resp)
}
