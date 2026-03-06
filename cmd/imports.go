package cmd

import (
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/dkoosis/snipe/internal/output"
	"github.com/dkoosis/snipe/internal/query"
	"github.com/dkoosis/snipe/internal/store"
)

var importsCmd = &cobra.Command{
	Use:     "imports <file>",
	Short:   "Show packages imported by a file",
	GroupID: "advanced",
	Long: `Shows all packages imported by a given Go file.

Examples:
  snipe imports main.go
  snipe imports internal/handler/server.go`,
	Args: cobra.ExactArgs(1),
	RunE: runImports,
}

func init() {
	rootCmd.AddCommand(importsCmd)
}

func runImports(cmd *cobra.Command, args []string) error {
	start := time.Now()

	compact, lim, offset, _, _, _ := GetOutputConfig()
	w := output.NewWriter(os.Stdout, compact)

	filePath := args[0]

	dir, err := os.Getwd()
	if err != nil {
		return w.WriteError("imports", &output.Error{
			Code:    output.ErrInternal,
			Message: "failed to get working directory: " + err.Error(),
		})
	}

	// Make path absolute if relative
	if !filepath.IsAbs(filePath) {
		filePath = filepath.Join(dir, filePath)
	}

	dbPath := store.DefaultIndexPath(dir)
	if store.IsIndexing(dbPath) {
		return w.WriteError("imports", output.NewIndexInProgressError())
	}
	if !store.Exists(dbPath) {
		return w.WriteError("imports", output.NewMissingIndexError())
	}

	s, err := store.Open(dbPath)
	if err != nil {
		return w.WriteError("imports", &output.Error{
			Code:    output.ErrInternal,
			Message: "failed to open index: " + err.Error(),
		})
	}
	defer s.Close()

	imports, err := query.FindImports(s.DB(), filePath, lim, offset)
	if err != nil {
		return w.WriteError("imports", &output.Error{
			Code:    output.ErrInternal,
			Message: err.Error(),
		})
	}

	// Convert to results
	results := make([]output.Result, len(imports))
	tokenEstimate := 0

	for i, imp := range imports {
		name := imp.PkgPath
		if imp.Name.Valid && imp.Name.String != "" {
			name = imp.Name.String + " " + imp.PkgPath
		}

		impRange := output.Range{
			Start: output.Position{Line: imp.Line, Col: imp.Col},
			End:   output.Position{Line: imp.Line, Col: imp.Col + len(imp.PkgPath)},
		}
		// Compute relative path for output
		filePathRel, _ := filepath.Rel(dir, imp.FilePath)
		if filePathRel == "" {
			filePathRel = imp.FilePath
		}
		results[i] = output.Result{
			ID:         imp.PkgPath,
			File:       filePathRel,
			FileAbs:    imp.FilePath,
			Range:      impRange,
			Kind:       "import",
			Name:       name,
			Match:      imp.PkgPath,
			EditTarget: output.FormatEditTargetWithHash(filePathRel, imp.FilePath, impRange),
		}
		tokenEstimate += output.EstimateTokens(imp.PkgPath)
	}

	// Apply token budget truncation if specified
	maxTok := GetMaxTokens()
	tokenTruncated := false
	if maxTok > 0 {
		results, tokenTruncated = output.TruncateToTokenBudget(results, maxTok)
	}

	// Recalculate token estimate after truncation
	tokenEstimate = 0
	for i := range results {
		tokenEstimate += output.EstimateResultTokens(&results[i])
	}

	staleFiles := query.CheckFileStaleness(s.DB(), dir, results)

	resp := output.Response[output.Result]{
		Protocol: output.ProtocolVersion,
		Ok:       true,
		Results:  results,
		Meta: output.Meta{
			Command:       "imports",
			Query:         map[string]string{"file": args[0]},
			RepoRoot:      dir,
			IndexState:    query.CheckIndexState(s.DB(), dir, Version),
			Ms:            time.Since(start).Milliseconds(),
			Total:         len(results),
			Truncated:     len(results) >= lim || tokenTruncated,
			TokenEstimate: tokenEstimate,
			Offset:        offset,
			Limit:         lim,
			StaleFiles:    staleFiles,
		},
	}

	return w.WriteResponse(resp)
}
