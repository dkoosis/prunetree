package cmd

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/dkoosis/snipe/internal/output"
	"github.com/dkoosis/snipe/internal/query"
)

var impactCmd = &cobra.Command{
	Use:     "impact [symbol|id]",
	Short:   "Show blast radius for changing a symbol",
	GroupID: "core",
	Long: `Analyzes what breaks if a symbol changes: transitive callers,
interface implementers, and test coverage in one call.

Returns a flat result list with hint-based classification:
  direct_caller, transitive_caller, implementer, direct_test, transitive_test

Accepts symbol name, 16-char hex ID (auto-detected), or --at position.

Examples:
  snipe impact ProcessOrder            # Full blast radius
  snipe impact --direct ProcessOrder   # Direct callers + direct tests only
  snipe impact --at order.go:42:1      # By position
  snipe impact a3f2c1de89ab0123        # By hex ID`,
	Args: cobra.MaximumNArgs(1),
	RunE: runImpact,
}

var (
	impactDirect bool
	impactAt     string
	impactID     string
)

func init() {
	impactCmd.Flags().BoolVar(&impactDirect, "direct", false, "1-hop only (skip transitive callers and tests)")
	impactCmd.Flags().StringVar(&impactAt, "at", "", "Position to look up (file:line:col)")
	impactCmd.Flags().StringVar(&impactID, "id", "", "Symbol ID to look up")
	rootCmd.AddCommand(impactCmd)
}

func runImpact(cmd *cobra.Command, args []string) error {
	start := time.Now()

	compact, lim, off, contextLines, withBody, _ := GetOutputConfig()
	format := GetResponseFormat()
	withBody, _, contextLines = ApplyFormatOverrides(format, withBody, false, contextLines)
	summary := format == FormatSummary

	w := output.NewWriter(os.Stdout, compact)

	if len(args) == 0 && impactAt == "" && impactID == "" {
		return w.WriteError("impact", &output.Error{
			Code:    output.ErrInternal,
			Message: "provide a symbol name, --at position, or --id",
		})
	}

	s, dir, err := OpenStore(w, "impact")
	if err != nil {
		return err
	}
	defer s.Close()

	// --- Symbol resolution (same pattern as tests/callers) ---
	var symbolID string
	var queryInfo map[string]string

	switch {
	case impactID != "":
		symbolID = impactID
		queryInfo = map[string]string{"id": impactID}

	case impactAt != "":
		pos, err := query.ParsePosition(impactAt)
		if err != nil {
			return w.WriteError("impact", &output.Error{
				Code:    output.ErrInternal,
				Message: err.Error(),
			})
		}
		filePath := pos.File
		if filepath.IsAbs(filePath) {
			if rel, err := filepath.Rel(dir, filePath); err == nil {
				filePath = rel
			}
		}
		sym := query.FindSymbolAtPosition(s.DB(), filePath, pos.Line)
		if sym == nil {
			return w.WriteError("impact", &output.Error{
				Code:    output.ErrNotFound,
				Message: "no symbol found at " + impactAt,
			})
		}
		symbolID = sym.ID
		queryInfo = map[string]string{"at": impactAt, "resolved": sym.Name}

	default:
		name := args[0]
		if len(name) == 16 {
			if _, err := hex.DecodeString(name); err == nil {
				symbolID = name
				queryInfo = map[string]string{"id": name}
				break
			}
		}
		symbols, err := query.LookupByName(s.DB(), name)
		if err != nil {
			return w.WriteError("impact", &output.Error{
				Code:    output.ErrInternal,
				Message: err.Error(),
			})
		}
		if len(symbols) == 0 {
			return w.WriteError("impact", output.NewNotFoundError(name))
		}
		if len(symbols) > 1 {
			candidates := make([]output.Candidate, len(symbols))
			for i, sym := range symbols {
				candidates[i] = sym.ToCandidate()
			}
			return w.WriteError("impact", output.NewAmbiguousError(name, candidates))
		}
		symbolID = symbols[0].ID
		queryInfo = map[string]string{"symbol": name}
	}

	// Look up symbol metadata for hints and session tracking
	var symName, symKind string
	if sym, err := query.LookupByID(s.DB(), symbolID); err == nil && sym != nil {
		symName = sym.Name
		symKind = sym.Kind
		recordSessionQuery(dir, sym.Name, sym.FilePathRel, sym.LineStart, sym.Kind, "impact")
	}

	var degraded []string
	var bodyFailed, contextFailed bool

	// --- Phase 1: Transitive callers (non-test files) ---
	// Use internal limit 3x to avoid phase 1 starving phases 2-3
	internalLim := lim * 3
	callerRows, err := query.FindImpactCallers(s.DB(), symbolID, impactDirect, internalLim, 0)
	if err != nil {
		degraded = append(degraded, "callers_failed")
		callerRows = nil
	}

	// --- Phase 2: Interface implementers ---
	var implRows []query.SymbolRow
	const kindInterface = "interface"
	if symKind == kindInterface {
		implRows, err = query.FindImplementers(s.DB(), symbolID, internalLim, 0)
		if err != nil {
			degraded = append(degraded, "implementers_failed")
			implRows = nil
		}
	}

	// --- Phase 3: Test coverage ---
	testRows, err := query.FindTests(s.DB(), symbolID, impactDirect, internalLim, 0)
	if err != nil {
		degraded = append(degraded, "tests_failed")
		testRows = nil
	}

	// --- Merge phases with cross-phase dedup ---
	type merged struct {
		result output.Result
		hints  []string
		order  int
	}
	seen := map[string]*merged{}
	orderCounter := 0

	addOrMerge := func(id string, r output.Result, hint string) {
		if m, ok := seen[id]; ok {
			m.hints = append(m.hints, hint)
		} else {
			seen[id] = &merged{result: r, hints: []string{hint}, order: orderCounter}
			orderCounter++
		}
	}

	// Phase 1 results
	directCallerCount := 0
	transitiveCallerCount := 0
	for _, cr := range callerRows {
		hint := output.HintDirectCaller
		if cr.Hop == 2 {
			hint = output.HintTransitiveCaller
			transitiveCallerCount++
		} else {
			directCallerCount++
		}
		addOrMerge(cr.ID, cr.ToResult(), hint)
	}

	// Phase 2 results
	implementerCount := 0
	for _, ir := range implRows {
		addOrMerge(ir.ID, ir.ToResult(), output.HintImplementer)
		implementerCount++
	}

	// Phase 3 results
	testCount := 0
	for _, tr := range testRows {
		hint := output.HintDirectTest
		if tr.Hop == 2 {
			hint = output.HintTransitiveTest
		}
		addOrMerge(tr.ID, tr.ToResult(), hint)
		testCount++
	}

	// Flatten: sort by insertion order (preserves phase grouping)
	sortable := make([]struct {
		m     *merged
		order int
	}, 0, len(seen))
	for _, m := range seen {
		sortable = append(sortable, struct {
			m     *merged
			order int
		}{m: m, order: m.order})
	}
	sort.Slice(sortable, func(i, j int) bool {
		return sortable[i].order < sortable[j].order
	})

	results := make([]output.Result, 0, len(sortable))
	for _, s := range sortable {
		s.m.result.Hints = s.m.hints
		if s.m.result.Name != "" && s.m.result.Name[0] >= 'A' && s.m.result.Name[0] <= 'Z' {
			s.m.result.Hints = append(s.m.result.Hints, output.HintExported)
		}
		results = append(results, s.m.result)
	}

	// Batch fetch bodies if requested
	if withBody && len(results) > 0 {
		ids := make([]string, len(results))
		for i, r := range results {
			ids[i] = r.ID
		}
		bodySymbols, batchErr := query.BatchLookupByID(s.DB(), ids)
		if batchErr != nil {
			degraded = append(degraded, "batch_lookup_failed")
		} else {
			for i, r := range results {
				if sym, ok := bodySymbols[r.ID]; ok && sym != nil {
					symResult := sym.ToResult()
					if err := output.AddBody(&symResult); err != nil && !bodyFailed {
						degraded = append(degraded, "body_extraction_failed")
						bodyFailed = true
					}
					results[i].Body = symResult.Body
				}
			}
		}
	}

	// Context lines
	if contextLines > 0 && !withBody {
		for i := range results {
			if err := output.AddContext(&results[i], contextLines); err != nil && !contextFailed {
				degraded = append(degraded, "context_extraction_failed")
				contextFailed = true
			}
		}
	}

	// NOTE: ScoreAndSort intentionally skipped for impact.
	// Phase grouping (callers -> implementers -> tests) IS the meaningful
	// ordering. ScoreAndSort ranks by name-similarity which destroys it.
	results = ApplySelection(results)

	maxTok := GetMaxTokens()
	tokenTruncated := false
	if maxTok > 0 {
		results, tokenTruncated = output.TruncateToTokenBudget(results, maxTok)
	}

	// Apply user-requested offset/limit AFTER merge (phases use internal limits)
	if off > 0 && off < len(results) {
		results = results[off:]
	} else if off >= len(results) {
		results = nil
	}
	if len(results) > lim {
		results = results[:lim]
	}

	staleFiles := query.CheckFileStaleness(s.DB(), dir, results)

	// Count packages for summary
	pkgs := map[string]bool{}
	for _, r := range results {
		if r.Package != "" {
			pkgs[r.Package] = true
		}
	}

	suggestions := output.SuggestionsForImpact(
		symName, directCallerCount, transitiveCallerCount,
		implementerCount, testCount, len(pkgs),
	)

	if summary {
		summaryData := output.BuildSummary(results)
		return w.WriteResponse(output.Response[output.Summary]{
			Protocol:    output.ProtocolVersion,
			Ok:          true,
			Results:     []output.Summary{summaryData},
			Suggestions: suggestions,
			Meta: output.Meta{
				Command:    "impact",
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
		})
	}

	tokenEstimate := 0
	for i := range results {
		tokenEstimate += output.EstimateResultTokens(&results[i])
	}

	resp := output.Response[output.Result]{
		Protocol:    output.ProtocolVersion,
		Ok:          true,
		Results:     results,
		Suggestions: suggestions,
		Meta: output.Meta{
			Command:       "impact",
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
