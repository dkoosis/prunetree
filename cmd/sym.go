package cmd

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	ctxpkg "github.com/dkoosis/snipe/internal/context"
	"github.com/dkoosis/snipe/internal/output"
	"github.com/dkoosis/snipe/internal/query"
)

var (
	symAt           string
	symRefsLimit    int
	symCallersLimit int
	symCalleesLimit int
)

var symCmd = &cobra.Command{
	Use:     "sym [symbol]",
	Short:   "Combined symbol query (def + refs + callers + callees)",
	GroupID: "core",
	Long: `Returns definition, references, callers, and callees in a single query.
Matches go_symbol's single-call pattern for LLM integration.

Examples:
  snipe sym ProcessOrder              # Full symbol info by name
  snipe sym --at main.go:42:12        # Full symbol info at position
  snipe sym Handler --refs-limit 5    # Limit references returned
  snipe sym --no-body Handler         # Exclude body from output`,
	Args: cobra.MaximumNArgs(1),
	RunE: runSym,
}

func init() {
	symCmd.Flags().StringVar(&symAt, "at", "", "Position to look up (file:line:col)")
	symCmd.Flags().IntVar(&symRefsLimit, "refs-limit", 20, "Maximum references to return")
	symCmd.Flags().IntVar(&symCallersLimit, "callers-limit", 10, "Maximum callers to return")
	symCmd.Flags().IntVar(&symCalleesLimit, "callees-limit", 10, "Maximum callees to return")
	rootCmd.AddCommand(symCmd)
}

func runSym(cmd *cobra.Command, args []string) error {
	start := time.Now()

	compact, _, _, contextLines, withBody, withSiblings := GetOutputConfig()
	w := output.NewWriter(os.Stdout, compact)

	// Need either a symbol name or --at position
	if len(args) == 0 && symAt == "" {
		return w.WriteError("sym", &output.Error{
			Code:    output.ErrInternal,
			Message: "provide a symbol name or --at position",
		})
	}

	// Find repo root and open store (auto-indexes if needed)
	s, dir, err := OpenStore(w, "sym")
	if err != nil {
		return err
	}
	defer s.Close()

	var symbolID string
	var queryInfo map[string]string

	if symAt != "" {
		// Resolve position
		pos, err := query.ParsePosition(symAt)
		if err != nil {
			return w.WriteError("sym", &output.Error{
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
			return w.WriteError("sym", &output.Error{
				Code:    output.ErrNotFound,
				Message: err.Error(),
			})
		}
		queryInfo = map[string]string{"at": symAt}
	} else {
		// Check if input looks like a symbol ID (16-char hex string)
		name := args[0]
		if len(name) == 16 {
			if _, err := hex.DecodeString(name); err == nil {
				symbolID = name
				queryInfo = map[string]string{"id": name}
				goto lookup
			}
		}

		// If arg looks like a filename, list symbols in that file
		if strings.HasSuffix(name, ".go") {
			_, lim, off, _, _, _ := GetOutputConfig()
			symbols, err := query.FindSymbolsInFile(s.DB(), name, lim, off)
			if err != nil {
				return w.WriteError("sym", &output.Error{
					Code:    output.ErrInternal,
					Message: err.Error(),
				})
			}
			if len(symbols) > 0 {
				candidates := make([]output.Candidate, len(symbols))
				for i, sym := range symbols {
					candidates[i] = sym.ToCandidate()
				}
				return w.WriteError("sym", &output.Error{
					Code:       output.ErrFileListing,
					Message:    fmt.Sprintf("%s (%d symbols)", name, len(candidates)),
					Candidates: candidates,
				})
			}
			return w.WriteError("sym", &output.Error{
				Code:    output.ErrNotFound,
				Message: "no symbols found in " + name,
			})
		}

		// Check for file-qualified syntax: file.go:SymbolName
		if idx := strings.LastIndex(name, ":"); idx > 0 && !strings.Contains(name[idx:], "/") {
			filePart := name[:idx]
			symbolPart := name[idx+1:]
			if symbolPart != "" && !strings.Contains(symbolPart, ":") {
				symbols, err := query.LookupByNameInFile(s.DB(), symbolPart, filePart)
				if err != nil {
					return w.WriteError("sym", &output.Error{
						Code:    output.ErrInternal,
						Message: err.Error(),
					})
				}
				if len(symbols) == 1 {
					symbolID = symbols[0].ID
					queryInfo = map[string]string{"symbol": symbolPart, "file": filePart}
					goto lookup
				} else if len(symbols) > 1 {
					candidates := make([]output.Candidate, len(symbols))
					for i, s := range symbols {
						candidates[i] = s.ToCandidate()
					}
					return w.WriteError("sym", output.NewAmbiguousError(name, candidates))
				}
			}
		}

		// Look up by name
		symbols, err := query.LookupByName(s.DB(), name)
		if err != nil {
			return w.WriteError("sym", &output.Error{
				Code:    output.ErrInternal,
				Message: err.Error(),
			})
		}

		if len(symbols) == 0 {
			maxDist := query.DefaultMaxDistance(name)
			suggestions, err := query.FindSimilarSymbols(s.DB(), name, maxDist, 3)
			if err != nil {
				return w.WriteError("sym", output.NewNotFoundError(name))
			}
			return w.WriteError("sym", output.NewNotFoundError(name, suggestions...))
		}

		if len(symbols) > 1 {
			candidates := make([]output.Candidate, len(symbols))
			for i, s := range symbols {
				candidates[i] = s.ToCandidate()
			}
			return w.WriteError("sym", output.NewAmbiguousError(name, candidates))
		}

		symbolID = symbols[0].ID
		queryInfo = map[string]string{"symbol": name}
	}

lookup:
	// Get the symbol details
	sym, err := query.LookupByID(s.DB(), symbolID)
	if err != nil {
		return w.WriteError("sym", &output.Error{
			Code:    output.ErrInternal,
			Message: err.Error(),
		})
	}

	if sym == nil {
		return w.WriteError("sym", &output.Error{
			Code:    output.ErrNotFound,
			Message: fmt.Sprintf("symbol %s not found", symbolID),
		})
	}

	var degraded []string

	// Record query in session for active work tracking
	recordSessionQuery(dir, sym.Name, sym.FilePathRel, sym.LineStart, sym.Kind, "sym")

	// Build definition result
	defResult := sym.ToResultWithHints(s.DB())

	if withBody {
		if err := output.AddBody(&defResult); err != nil {
			degraded = append(degraded, "body_extraction_failed")
		}
	}

	if contextLines > 0 && !withBody {
		if err := output.AddContext(&defResult, contextLines); err != nil {
			degraded = append(degraded, "context_extraction_failed")
		}
	}

	// Add role classification in detailed format
	if GetResponseFormat() == FormatDetailed {
		role := ctxpkg.InferRoleForSymbol(s.DB(), sym.ID, sym.Name, sym.Kind,
			sym.Signature.String, sym.PkgPath, sym.FilePath)
		defResult.Role = string(role)
	}

	if withSiblings {
		siblings, err := query.FindSiblings(s.DB(), sym.FilePath, sym.Kind, sym.ID, 20)
		if err != nil {
			degraded = append(degraded, "siblings_query_failed")
		} else if len(siblings) > 0 {
			defResult.Siblings = siblings
		}
	}

	// Get references
	refs, err := query.FindRefs(s.DB(), symbolID, symRefsLimit, 0)
	if err != nil {
		degraded = append(degraded, "refs_query_failed")
	}

	refResults := make([]output.Result, 0, len(refs))
	nameLen := len(sym.Name)
	if nameLen == 0 {
		nameLen = 1
	}

	for _, ref := range refs {
		refRange := output.Range{
			Start: output.Position{Line: ref.Line, Col: ref.Col},
			End:   output.Position{Line: ref.Line, Col: ref.Col + nameLen},
		}
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
			Name:       sym.Name,
			Match:      ref.Snippet,
			EditTarget: output.FormatEditTargetWithHash(filePath, ref.FilePath, refRange),
		}

		if ref.EnclosingID.Valid {
			result.Enclosing = &output.Enclosing{
				ID:        ref.EnclosingID.String,
				Kind:      ref.EnclosingKind,
				Name:      ref.EnclosingName,
				Signature: ref.EnclosingSignature,
			}
		}

		refResults = append(refResults, result)
	}

	// Get total ref count
	refCount, err := query.GetRefCount(s.DB(), symbolID)
	if err != nil {
		degraded = append(degraded, "ref_count_query_failed")
		refCount = -1
	}

	// Get callers
	callerRows, err := query.FindCallers(s.DB(), symbolID, symCallersLimit, 0)
	if err != nil {
		degraded = append(degraded, "callers_query_failed")
	}

	callerResults := make([]output.Result, 0, len(callerRows))
	for _, call := range callerRows {
		callerResults = append(callerResults, call.ToCallerResult())
	}

	// Get caller count
	var callerCount int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM call_graph WHERE callee_id = ?`, symbolID).Scan(&callerCount); err != nil {
		degraded = append(degraded, "caller_count_query_failed")
		callerCount = -1
	}

	// Get callees
	calleeRows, err := query.FindCallees(s.DB(), symbolID, symCalleesLimit, 0)
	if err != nil {
		degraded = append(degraded, "callees_query_failed")
	}

	calleeResults := make([]output.Result, 0, len(calleeRows))
	for _, call := range calleeRows {
		calleeResults = append(calleeResults, call.ToCalleeResult())
	}

	// Get callee count
	var calleeCount int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM call_graph WHERE caller_id = ?`, symbolID).Scan(&calleeCount); err != nil {
		degraded = append(degraded, "callee_count_query_failed")
		calleeCount = -1
	}

	// Deduplicate degraded messages
	degraded = uniqueStrings(degraded)

	// Build combined response
	symResp := output.SymResult{
		Definition:  &defResult,
		References:  refResults,
		Callers:     callerResults,
		Callees:     calleeResults,
		RefCount:    refCount,
		CallerCount: callerCount,
		CalleeCount: calleeCount,
	}

	// Estimate tokens
	tokenEstimate := output.EstimateResultTokens(&defResult)
	for i := range refResults {
		tokenEstimate += output.EstimateResultTokens(&refResults[i])
	}
	for i := range callerResults {
		tokenEstimate += output.EstimateResultTokens(&callerResults[i])
	}
	for i := range calleeResults {
		tokenEstimate += output.EstimateResultTokens(&calleeResults[i])
	}

	// Collect all results for staleness check
	var allResults []output.Result
	allResults = append(allResults, defResult)
	allResults = append(allResults, refResults...)
	allResults = append(allResults, callerResults...)
	allResults = append(allResults, calleeResults...)
	staleFiles := query.CheckFileStaleness(s.DB(), dir, allResults)

	resp := output.Response[output.SymResult]{
		Protocol: output.ProtocolVersion,
		Ok:       true,
		Results:  []output.SymResult{symResp},
		Meta: output.Meta{
			Command:       "sym",
			Query:         queryInfo,
			RepoRoot:      dir,
			IndexState:    query.CheckIndexState(s.DB(), dir, Version),
			Degraded:      degraded,
			Ms:            time.Since(start).Milliseconds(),
			Total:         1,
			TokenEstimate: tokenEstimate,
			StaleFiles:    staleFiles,
		},
	}

	return w.WriteResponse(resp)
}
