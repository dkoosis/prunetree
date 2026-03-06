package cmd

import (
	"encoding/hex"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/dkoosis/snipe/internal/output"
	"github.com/dkoosis/snipe/internal/query"
)

var calleesCmd = &cobra.Command{
	Use:     "callees [symbol|id]",
	Short:   "Find functions that a symbol calls",
	GroupID: "core",
	Long: `Finds all functions called by a given symbol.

Accepts symbol name or 16-char hex ID (auto-detected).
Use --with-body to include callee function bodies.

Examples:
  snipe callees ProcessOrder      # Find callees by name
  snipe callees a3f2c1de89ab0123  # Find callees by hex ID (auto-detected)
  snipe callees --id abc123       # Explicit --id flag
  snipe callees ProcessOrder --with-body  # Include callee bodies`,
	Args: cobra.MaximumNArgs(1),
	RunE: runCallees,
}

var calleesID string

func init() {
	calleesCmd.Flags().StringVar(&calleesID, "id", "", "Symbol ID to look up")
	rootCmd.AddCommand(calleesCmd)
}

func runCallees(cmd *cobra.Command, args []string) error {
	start := time.Now()

	compact, lim, off, contextLines, withBody, _ := GetOutputConfig()
	format := GetResponseFormat()

	// Apply format overrides
	withBody, _, contextLines = ApplyFormatOverrides(format, withBody, false, contextLines)
	summary := format == FormatSummary

	w := output.NewWriter(os.Stdout, compact)

	if len(args) == 0 && calleesID == "" {
		return w.WriteError("callees", &output.Error{
			Code:    output.ErrInternal,
			Message: "provide a symbol name or --id",
		})
	}

	// Find repo root and open store (auto-indexes if needed)
	s, dir, err := OpenStore(w, "callees")
	if err != nil {
		return err
	}
	defer s.Close()

	var symbolID string
	var queryInfo map[string]string

	if calleesID != "" {
		symbolID = calleesID
		queryInfo = map[string]string{"id": calleesID}
	} else {
		name := args[0]

		// Check if input looks like a symbol ID (16-char hex string)
		if len(name) == 16 {
			if _, err := hex.DecodeString(name); err == nil {
				symbolID = name
				queryInfo = map[string]string{"id": name}
				goto findCallees
			}
		}

		symbols, err := query.LookupByName(s.DB(), name)
		if err != nil {
			return w.WriteError("callees", &output.Error{
				Code:    output.ErrInternal,
				Message: err.Error(),
			})
		}

		if len(symbols) == 0 {
			return w.WriteError("callees", output.NewNotFoundError(name))
		}

		if len(symbols) > 1 {
			candidates := make([]output.Candidate, len(symbols))
			for i, sym := range symbols {
				candidates[i] = sym.ToCandidate()
			}
			return w.WriteError("callees", output.NewAmbiguousError(name, candidates))
		}

		symbolID = symbols[0].ID
		queryInfo = map[string]string{"symbol": name}
	}

findCallees:

	// Record query in session for active work tracking
	var symName string
	if sym, err := query.LookupByID(s.DB(), symbolID); err == nil && sym != nil {
		symName = sym.Name
		recordSessionQuery(dir, sym.Name, sym.FilePathRel, sym.LineStart, sym.Kind, "callees")
	}

	// Find callees
	calls, err := query.FindCallees(s.DB(), symbolID, lim, off)
	if err != nil {
		return w.WriteError("callees", &output.Error{
			Code:    output.ErrInternal,
			Message: err.Error(),
		})
	}

	// Convert to results - callee function definitions
	results := make([]output.Result, 0, len(calls))
	calleeResults := make([]output.Result, len(calls))
	tokenEstimate := 0
	var degraded []string

	// Batch fetch callee symbols if bodies are requested (avoids N+1 queries)
	var calleeSymbols map[string]*query.SymbolRow
	if withBody && len(calls) > 0 {
		calleeIDs := make([]string, len(calls))
		for i, call := range calls {
			calleeIDs[i] = call.CalleeID
		}
		var batchErr error
		calleeSymbols, batchErr = query.BatchLookupByID(s.DB(), calleeIDs)
		if batchErr != nil {
			degraded = append(degraded, "batch_lookup_failed")
		}
	}

	for i, call := range calls {
		result := call.ToCalleeResult()

		// Add callee body if requested (from the callee's definition, not call site)
		if withBody {
			if calleeSym, ok := calleeSymbols[call.CalleeID]; ok && calleeSym != nil {
				calleeResult := calleeSym.ToResult()
				if err := output.AddBody(&calleeResult); err != nil {
					degraded = append(degraded, "body_extraction_failed")
				}
				result.Body = calleeResult.Body
			}
		}

		if contextLines > 0 && !withBody {
			if err := output.AddContext(&result, contextLines); err != nil {
				degraded = append(degraded, "context_extraction_failed")
			}
		}

		calleeResults[i] = result
		tokenEstimate += output.EstimateTokens(call.CalleeSignature.String)
		if result.Body != "" {
			tokenEstimate = output.EstimateTokens(result.Body)
		}
	}

	results = append(results, calleeResults...)

	// Deduplicate degraded messages
	degraded = uniqueStrings(degraded)

	// Score, sort, and apply selection
	output.ScoreAndSort(results, symName)
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
				Command:    "callees",
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
		Suggestions: output.SuggestionsForCallees(symName, len(results)),
		Meta: output.Meta{
			Command:       "callees",
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
