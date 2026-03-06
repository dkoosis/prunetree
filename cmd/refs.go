package cmd

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dkoosis/snipe/internal/output"
	"github.com/dkoosis/snipe/internal/query"
)

var (
	refsAt   string
	refsKind string
	refsFile string
	refsPkg  string
)

var refsCmd = &cobra.Command{
	Use:     "refs [symbol]",
	Short:   "Find all references to a symbol",
	GroupID: "core",
	Long: `Finds all references to a symbol by name or position.

Scoped queries:
  snipe refs Open --file store.go     # Refs in matching file
  snipe refs Open --pkg internal/query # Refs in matching package

Examples:
  snipe refs ProcessOrder          # Find by name
  snipe refs --at main.go:42:12    # Find at position
  snipe refs Workspace --kind=method  # Only method references`,
	Args: cobra.MaximumNArgs(1),
	RunE: runRefs,
}

func init() {
	refsCmd.Flags().StringVar(&refsAt, "at", "", "Position to look up (file:line:col)")
	refsCmd.Flags().StringVar(&refsKind, "kind", "", "Filter by enclosing kind (func, method, etc.)")
	refsCmd.Flags().StringVar(&refsFile, "file", "", "Filter references to those in matching file")
	refsCmd.Flags().StringVar(&refsPkg, "pkg", "", "Filter references to those in matching package path")
	rootCmd.AddCommand(refsCmd)
}

func runRefs(cmd *cobra.Command, args []string) error {
	start := time.Now()

	compact, lim, off, contextLines, withBody, _ := GetOutputConfig()
	format := GetResponseFormat()

	// Apply format overrides
	withBody, _, contextLines = ApplyFormatOverrides(format, withBody, false, contextLines)
	summary := format == FormatSummary

	w := output.NewWriter(os.Stdout, compact)

	// Need either a symbol name or --at position
	if len(args) == 0 && refsAt == "" {
		return w.WriteError("refs", &output.Error{
			Code:    output.ErrInternal,
			Message: "provide a symbol name or --at position",
		})
	}

	// Find repo root and open store (auto-indexes if needed)
	s, dir, err := OpenStore(w, "refs")
	if err != nil {
		return err
	}
	defer s.Close()

	var symbolID string
	var queryInfo map[string]string

	if refsAt != "" {
		// Resolve position
		pos, err := query.ParsePosition(refsAt)
		if err != nil {
			return w.WriteError("refs", &output.Error{
				Code:    output.ErrInternal,
				Message: err.Error(),
			})
		}

		// Make path absolute if relative
		if !filepath.IsAbs(pos.File) {
			pos.File = filepath.Join(dir, pos.File)
		}

		symbolID, err = query.ResolvePosition(s.DB(), pos)
		if err != nil {
			return w.WriteError("refs", &output.Error{
				Code:    output.ErrNotFound,
				Message: err.Error(),
			})
		}
		queryInfo = map[string]string{"at": refsAt}
	} else {
		name := args[0]

		// Check if input looks like a symbol ID (16-char hex string)
		if len(name) == 16 {
			if _, err := hex.DecodeString(name); err == nil {
				symbolID = name
				queryInfo = map[string]string{"id": name}
				goto findRefs
			}
		}

		// Look up by name
		symbols, err := query.LookupByName(s.DB(), name)
		if err != nil {
			return w.WriteError("refs", &output.Error{
				Code:    output.ErrInternal,
				Message: err.Error(),
			})
		}

		if len(symbols) == 0 {
			return w.WriteError("refs", output.NewNotFoundError(name))
		}

		if len(symbols) > 1 {
			candidates := make([]output.Candidate, len(symbols))
			for i, s := range symbols {
				candidates[i] = s.ToCandidate()
			}
			return w.WriteError("refs", output.NewAmbiguousError(name, candidates))
		}

		symbolID = symbols[0].ID
		queryInfo = map[string]string{"symbol": name}
	}

findRefs:
	// Look up symbol to get name length for accurate range
	symbolName := ""
	if sym, err := query.LookupByID(s.DB(), symbolID); err == nil && sym != nil {
		symbolName = sym.Name
		// Record query in session for active work tracking
		recordSessionQuery(dir, sym.Name, sym.FilePathRel, sym.LineStart, sym.Kind, "refs")
	}
	nameLen := len(symbolName)
	if nameLen == 0 {
		nameLen = 1 // Fallback to minimal range
	}

	// Find all references
	refs, err := query.FindRefs(s.DB(), symbolID, lim, off)
	if err != nil {
		return w.WriteError("refs", &output.Error{
			Code:    output.ErrInternal,
			Message: err.Error(),
		})
	}

	// Filter by kind if specified
	if refsKind != "" {
		filtered := refs[:0]
		for _, ref := range refs {
			if ref.EnclosingKind == refsKind {
				filtered = append(filtered, ref)
			}
		}
		refs = filtered
	}

	// Filter by file if specified
	if refsFile != "" {
		filtered := refs[:0]
		for _, ref := range refs {
			if strings.Contains(ref.FilePathRel, refsFile) || strings.Contains(ref.FilePath, refsFile) {
				filtered = append(filtered, ref)
			}
		}
		refs = filtered
		queryInfo["file"] = refsFile
	}

	// Filter by package if specified
	if refsPkg != "" {
		filtered := refs[:0]
		for _, ref := range refs {
			// Match against directory components of the file path
			refDir := filepath.Dir(ref.FilePathRel)
			if refDir == "" {
				refDir = filepath.Dir(ref.FilePath)
			}
			if strings.Contains(refDir, refsPkg) {
				filtered = append(filtered, ref)
			}
		}
		refs = filtered
		queryInfo["pkg"] = refsPkg
	}

	// Convert to results
	results := make([]output.Result, len(refs))
	tokenEstimate := 0
	var degraded []string

	// Batch fetch enclosing symbols if bodies are requested (avoids N+1 queries)
	var enclosingSymbols map[string]*query.SymbolRow
	if withBody && len(refs) > 0 {
		enclosingIDs := make([]string, 0, len(refs))
		for _, ref := range refs {
			if ref.EnclosingID.Valid {
				enclosingIDs = append(enclosingIDs, ref.EnclosingID.String)
			}
		}
		if len(enclosingIDs) > 0 {
			var batchErr error
			enclosingSymbols, batchErr = query.BatchLookupByID(s.DB(), enclosingIDs)
			if batchErr != nil {
				degraded = append(degraded, "batch_lookup_failed")
			}
		}
	}

	for i, ref := range refs {
		refRange := output.Range{
			Start: output.Position{Line: ref.Line, Col: ref.Col},
			End:   output.Position{Line: ref.Line, Col: ref.Col + nameLen},
		}
		// Use relative path for output, absolute for file operations
		filePath := ref.FilePathRel
		if filePath == "" {
			filePath = ref.FilePath
		}
		result := output.Result{
			ID:         ref.ID,
			File:       filePath,
			FileAbs:    ref.FilePath,
			Range:      refRange,
			Kind:       "ref",
			Name:       symbolName,
			Match:      ref.Snippet,
			EditTarget: output.FormatEditTargetWithHash(filePath, ref.FilePath, refRange),
		}

		// Add enclosing info if available
		if ref.EnclosingID.Valid {
			result.Enclosing = &output.Enclosing{
				ID:        ref.EnclosingID.String,
				Kind:      ref.EnclosingKind,
				Name:      ref.EnclosingName,
				Signature: ref.EnclosingSignature,
			}

			// Add enclosing function body if requested
			if withBody {
				if encSym, ok := enclosingSymbols[ref.EnclosingID.String]; ok && encSym != nil {
					encResult := encSym.ToResult()
					if err := output.AddBody(&encResult); err != nil {
						degraded = append(degraded, "body_extraction_failed")
					}
					result.Body = encResult.Body
				}
			}
		}

		// Add context lines if requested (only if not showing full body)
		if contextLines > 0 && !withBody {
			if err := output.AddContext(&result, contextLines); err != nil {
				degraded = append(degraded, "context_extraction_failed")
			}
		}

		results[i] = result
		tokenEstimate += output.EstimateTokens(ref.Snippet)
		if result.Body != "" {
			tokenEstimate = output.EstimateTokens(result.Body)
		}
	}

	// Deduplicate degraded messages
	degraded = uniqueStrings(degraded)

	// Score, sort, and apply selection
	output.ScoreAndSort(results, symbolName)
	results = ApplySelection(results)

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
				Command:    "refs",
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
		Protocol:    output.ProtocolVersion,
		Ok:          true,
		Results:     results,
		Suggestions: output.SuggestionsForRefs(symbolName, len(results)),
		Meta: output.Meta{
			Command:       "refs",
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
