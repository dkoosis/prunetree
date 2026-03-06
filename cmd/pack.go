package cmd

import (
	"database/sql"
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
	"github.com/dkoosis/snipe/internal/store"
)

var (
	packAt           string
	packRefsLimit    int
	packCallersLimit int
	packCalleesLimit int
)

var packCmd = &cobra.Command{
	Use:     "pack [symbol...]",
	Short:   "Full symbol profile: def + refs + callers + callees + role + purpose",
	GroupID: "core",
	Long: `Returns everything an LLM needs about a symbol in a single query.
Combines definition, references, callers, callees, architectural role,
relevance score, and purpose summary.

Multi-ID mode bundles multiple symbols in one response:
  snipe pack abc123def456 789012345678  # Multiple hex IDs

Examples:
  snipe pack ProcessOrder              # Full profile by name
  snipe pack --at main.go:42:12        # Full profile at position
  snipe pack Handler --refs-limit 5    # Limit references returned`,
	RunE: runPack,
}

func init() {
	packCmd.Flags().StringVar(&packAt, "at", "", "Position to look up (file:line:col)")
	packCmd.Flags().IntVar(&packRefsLimit, "refs-limit", 20, "Maximum references to return")
	packCmd.Flags().IntVar(&packCallersLimit, "callers-limit", 10, "Maximum callers to return")
	packCmd.Flags().IntVar(&packCalleesLimit, "callees-limit", 10, "Maximum callees to return")
	rootCmd.AddCommand(packCmd)
}

func runPack(cmd *cobra.Command, args []string) error {
	start := time.Now()

	compact, _, _, contextLines, withBody, withSiblings := GetOutputConfig()
	w := output.NewWriter(os.Stdout, compact)

	if len(args) == 0 && packAt == "" {
		return w.WriteError("pack", &output.Error{
			Code:    output.ErrInternal,
			Message: "provide a symbol name or --at position",
		})
	}

	s, dir, err := OpenStore(w, "pack")
	if err != nil {
		return err
	}
	defer s.Close()

	opts := packOpts{
		withBody:     withBody,
		withSiblings: withSiblings,
		contextLines: contextLines,
	}

	// Multi-ID mode: multiple args that are all hex IDs
	if len(args) > 1 {
		return runPackMulti(w, s, dir, args, opts, start)
	}

	// Single-symbol mode
	symbolID, queryInfo, err := resolvePackSymbol(w, s, dir, args, packAt)
	if err != nil {
		return err
	}

	packResult, degraded, allResults, err := buildPackForSymbol(s, dir, symbolID, opts)
	if err != nil {
		return w.WriteError("pack", &output.Error{
			Code:    output.ErrInternal,
			Message: err.Error(),
		})
	}

	tokenEstimate := estimatePackTokens(packResult)
	staleFiles := query.CheckFileStaleness(s.DB(), dir, allResults)

	resp := output.Response[output.PackResult]{
		Protocol:    output.ProtocolVersion,
		Ok:          true,
		Results:     []output.PackResult{packResult},
		Suggestions: output.SuggestionsForPack(packResult.Definition),
		Meta: output.Meta{
			Command:       "pack",
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

// runPackMulti handles multi-ID mode: snipe pack id1 id2 id3
func runPackMulti(w *output.Writer, s *store.Store, dir string, args []string, opts packOpts, start time.Time) error {
	// Validate all args are hex IDs
	ids := make([]string, 0, len(args))
	for _, arg := range args {
		if len(arg) != 16 {
			return w.WriteError("pack", &output.Error{
				Code:    output.ErrInternal,
				Message: fmt.Sprintf("multi-ID mode requires 16-char hex IDs, got %q", arg),
			})
		}
		if _, err := hex.DecodeString(arg); err != nil {
			return w.WriteError("pack", &output.Error{
				Code:    output.ErrInternal,
				Message: fmt.Sprintf("invalid hex ID %q", arg),
			})
		}
		ids = append(ids, arg)
	}

	var allPackResults []output.PackResult
	var allDegraded []string
	var allResultsForStale []output.Result
	tokenEstimate := 0

	for _, id := range ids {
		packResult, degraded, results, err := buildPackForSymbol(s, dir, id, opts)
		if err != nil {
			allDegraded = append(allDegraded, fmt.Sprintf("pack_%s_failed", id[:8]))
			continue
		}
		allPackResults = append(allPackResults, packResult)
		allDegraded = append(allDegraded, degraded...)
		allResultsForStale = append(allResultsForStale, results...)
		tokenEstimate += estimatePackTokens(packResult)
	}

	allDegraded = uniqueStrings(allDegraded)
	staleFiles := query.CheckFileStaleness(s.DB(), dir, allResultsForStale)

	queryInfo := map[string]string{"ids": strings.Join(ids, ",")}

	resp := output.Response[output.PackResult]{
		Protocol: output.ProtocolVersion,
		Ok:       true,
		Results:  allPackResults,
		Meta: output.Meta{
			Command:       "pack",
			Query:         queryInfo,
			RepoRoot:      dir,
			IndexState:    query.CheckIndexState(s.DB(), dir, Version),
			Degraded:      allDegraded,
			Ms:            time.Since(start).Milliseconds(),
			Total:         len(allPackResults),
			TokenEstimate: tokenEstimate,
			StaleFiles:    staleFiles,
		},
	}

	return w.WriteResponse(resp)
}

// packOpts holds output configuration for pack building.
type packOpts struct {
	withBody     bool
	withSiblings bool
	contextLines int
}

// resolvePackSymbol resolves args/--at into a symbol ID.
func resolvePackSymbol(w *output.Writer, s *store.Store, dir string, args []string, at string) (string, map[string]string, error) {
	if at != "" {
		pos, err := query.ParsePosition(at)
		if err != nil {
			return "", nil, w.WriteError("pack", &output.Error{
				Code:    output.ErrInternal,
				Message: err.Error(),
			})
		}
		if !filepath.IsAbs(pos.File) {
			pos.File = filepath.Join(dir, pos.File)
		}
		symbolID, err := query.ResolvePosition(s.DB(), pos)
		if err != nil {
			return "", nil, w.WriteError("pack", &output.Error{
				Code:    output.ErrNotFound,
				Message: err.Error(),
			})
		}
		return symbolID, map[string]string{"at": at}, nil
	}

	name := args[0]

	// Check if input is a hex ID
	if len(name) == 16 {
		if _, err := hex.DecodeString(name); err == nil {
			return name, map[string]string{"id": name}, nil
		}
	}

	// Check for file-qualified syntax: file.go:SymbolName
	if idx := strings.LastIndex(name, ":"); idx > 0 && !strings.Contains(name[idx:], "/") {
		filePart := name[:idx]
		symbolPart := name[idx+1:]
		if symbolPart != "" && !strings.Contains(symbolPart, ":") {
			symbols, err := query.LookupByNameInFile(s.DB(), symbolPart, filePart)
			if err != nil {
				return "", nil, w.WriteError("pack", &output.Error{
					Code:    output.ErrInternal,
					Message: err.Error(),
				})
			}
			if len(symbols) == 1 {
				return symbols[0].ID, map[string]string{"symbol": symbolPart, "file": filePart}, nil
			}
			if len(symbols) > 1 {
				candidates := make([]output.Candidate, len(symbols))
				for i, sym := range symbols {
					candidates[i] = sym.ToCandidate()
				}
				return "", nil, w.WriteError("pack", output.NewAmbiguousError(name, candidates))
			}
		}
	}

	// Look up by name
	symbols, err := query.LookupByName(s.DB(), name)
	if err != nil {
		return "", nil, w.WriteError("pack", &output.Error{
			Code:    output.ErrInternal,
			Message: err.Error(),
		})
	}

	if len(symbols) == 0 {
		maxDist := query.DefaultMaxDistance(name)
		suggestions, sErr := query.FindSimilarSymbols(s.DB(), name, maxDist, 3)
		if sErr != nil {
			return "", nil, w.WriteError("pack", output.NewNotFoundError(name))
		}
		return "", nil, w.WriteError("pack", output.NewNotFoundError(name, suggestions...))
	}

	if len(symbols) > 1 {
		candidates := make([]output.Candidate, len(symbols))
		for i, sym := range symbols {
			candidates[i] = sym.ToCandidate()
		}
		return "", nil, w.WriteError("pack", output.NewAmbiguousError(name, candidates))
	}

	return symbols[0].ID, map[string]string{"symbol": name}, nil
}

// buildPackForSymbol builds a full PackResult for a single symbol ID.
// Returns the pack result, degraded warnings, all inner results (for staleness), and any error.
func buildPackForSymbol(s *store.Store, dir, symbolID string, opts packOpts) (output.PackResult, []string, []output.Result, error) {
	db := s.DB()
	var degraded []string

	sym, err := query.LookupByID(db, symbolID)
	if err != nil {
		return output.PackResult{}, nil, nil, err
	}
	if sym == nil {
		return output.PackResult{}, nil, nil, fmt.Errorf("symbol %s not found", symbolID)
	}

	recordSessionQuery(dir, sym.Name, sym.FilePathRel, sym.LineStart, sym.Kind, "pack")

	// Build definition result
	defResult := sym.ToResultWithHints(db)

	if opts.withBody {
		if err := output.AddBody(&defResult); err != nil {
			degraded = append(degraded, "body_extraction_failed")
		}
	}
	if opts.contextLines > 0 && !opts.withBody {
		if err := output.AddContext(&defResult, opts.contextLines); err != nil {
			degraded = append(degraded, "context_extraction_failed")
		}
	}
	if opts.withSiblings {
		siblings, err := query.FindSiblings(db, sym.FilePath, sym.Kind, sym.ID, 20)
		if err != nil {
			degraded = append(degraded, "siblings_query_failed")
		} else if len(siblings) > 0 {
			defResult.Siblings = siblings
		}
	}

	defResult.Score = output.ScoreResult(&defResult, sym.Name)

	// Infer role
	role := ctxpkg.InferRoleForSymbol(db, sym.ID, sym.Name, sym.Kind,
		sym.Signature.String, sym.PkgPath, sym.FilePath)

	// Get purpose
	purpose, _ := s.GetPurpose(sym.ID)
	if purpose == "" && sym.Doc.Valid && sym.Doc.String != "" {
		purpose = ctxpkg.ExtractFirstSentence(sym.Doc.String)
	}

	// Build refs
	refResults, refDegraded := buildRefResults(db, symbolID, sym.Name)
	degraded = append(degraded, refDegraded...)

	refCount, err := query.GetRefCount(db, symbolID)
	if err != nil {
		degraded = append(degraded, "ref_count_query_failed")
		refCount = -1
	}

	// Build callers/callees — branch on symbol kind
	var callerResults, calleeResults []output.Result
	var callerDegraded, calleeDegraded []string
	var callerCount, calleeCount int
	var methods []output.MethodSummary

	isType := isTypeKind(sym.Kind)
	if isType {
		// Aggregate callers/callees across all methods of this type
		methodInfos, mErr := query.GetMethodsForType(db, sym.Name, "")
		if mErr != nil {
			degraded = append(degraded, "methods_query_failed")
		}
		for _, m := range methodInfos {
			methods = append(methods, output.MethodSummary{
				ID:        m.ID,
				Name:      m.Name,
				Signature: m.Signature,
			})
		}

		callerResults, callerDegraded = buildCallRowResults(
			func() ([]query.CallRow, error) { return query.FindCallersForType(db, sym.Name, packCallersLimit, 0) },
			(*query.CallRow).ToCallerResult, "callers_query_failed")
		calleeResults, calleeDegraded = buildCallRowResults(
			func() ([]query.CallRow, error) { return query.FindCalleesForType(db, sym.Name, packCalleesLimit, 0) },
			(*query.CallRow).ToCalleeResult, "callees_query_failed")

		var cErr error
		callerCount, cErr = query.CountCallersForType(db, sym.Name)
		if cErr != nil {
			degraded = append(degraded, "caller_count_query_failed")
			callerCount = -1
		}
		calleeCount, cErr = query.CountCalleesForType(db, sym.Name)
		if cErr != nil {
			degraded = append(degraded, "callee_count_query_failed")
			calleeCount = -1
		}
	} else {
		callerResults, callerDegraded = buildCallRowResults(
			func() ([]query.CallRow, error) { return query.FindCallers(db, symbolID, packCallersLimit, 0) },
			(*query.CallRow).ToCallerResult, "callers_query_failed")

		if err := db.QueryRow(`SELECT COUNT(*) FROM call_graph WHERE callee_id = ?`, symbolID).Scan(&callerCount); err != nil {
			degraded = append(degraded, "caller_count_query_failed")
			callerCount = -1
		}

		calleeResults, calleeDegraded = buildCallRowResults(
			func() ([]query.CallRow, error) { return query.FindCallees(db, symbolID, packCalleesLimit, 0) },
			(*query.CallRow).ToCalleeResult, "callees_query_failed")

		if err := db.QueryRow(`SELECT COUNT(*) FROM call_graph WHERE caller_id = ?`, symbolID).Scan(&calleeCount); err != nil {
			degraded = append(degraded, "callee_count_query_failed")
			calleeCount = -1
		}
	}
	degraded = append(degraded, callerDegraded...)
	degraded = append(degraded, calleeDegraded...)

	relatedTypes := extractRelatedTypes(sym.Signature.String)
	degraded = uniqueStrings(degraded)

	packResult := output.PackResult{
		Definition:   &defResult,
		References:   refResults,
		Callers:      callerResults,
		Callees:      calleeResults,
		Methods:      methods,
		RefCount:     refCount,
		CallerCount:  callerCount,
		CalleeCount:  calleeCount,
		Role:         string(role),
		Purpose:      purpose,
		RelatedTypes: relatedTypes,
	}

	// Collect all results for staleness check
	var allResults []output.Result
	allResults = append(allResults, defResult)
	allResults = append(allResults, refResults...)
	allResults = append(allResults, callerResults...)
	allResults = append(allResults, calleeResults...)

	return packResult, degraded, allResults, nil
}

func buildRefResults(db *sql.DB, symbolID, symName string) ([]output.Result, []string) {
	var degraded []string
	refs, err := query.FindRefs(db, symbolID, packRefsLimit, 0)
	if err != nil {
		degraded = append(degraded, "refs_query_failed")
	}

	nameLen := len(symName)
	if nameLen == 0 {
		nameLen = 1
	}

	results := make([]output.Result, 0, len(refs))
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
			Name:       symName,
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
		results = append(results, result)
	}

	return results, degraded
}

// buildCallRowResults queries call graph rows and converts them to output results.
// queryFn fetches the rows; toResult converts each row to an output.Result.
func buildCallRowResults(
	queryFn func() ([]query.CallRow, error),
	toResult func(*query.CallRow) output.Result,
	degradedLabel string,
) ([]output.Result, []string) {
	var degraded []string
	rows, err := queryFn()
	if err != nil {
		degraded = append(degraded, degradedLabel)
	}

	results := make([]output.Result, 0, len(rows))
	for i := range rows {
		results = append(results, toResult(&rows[i]))
	}

	return results, degraded
}

func estimatePackTokens(pr output.PackResult) int {
	estimate := 0
	if pr.Definition != nil {
		estimate += output.EstimateResultTokens(pr.Definition)
	}
	for i := range pr.References {
		estimate += output.EstimateResultTokens(&pr.References[i])
	}
	for i := range pr.Callers {
		estimate += output.EstimateResultTokens(&pr.Callers[i])
	}
	for i := range pr.Callees {
		estimate += output.EstimateResultTokens(&pr.Callees[i])
	}
	return estimate
}

// extractRelatedTypes extracts type names from a Go function signature.
// Returns unique type names referenced as parameters or return values.
func extractRelatedTypes(signature string) []string {
	if signature == "" {
		return nil
	}

	// Simple heuristic: find capitalized identifiers that look like types
	// Skip common Go builtins
	builtins := map[string]bool{
		"string": true, "int": true, "int64": true, "int32": true,
		"float64": true, "float32": true, "bool": true, "byte": true,
		"error": true, "any": true, "rune": true, "uint": true,
		"uint64": true, "uint32": true, "nil": true, "func": true,
	}

	seen := make(map[string]bool)
	var types []string

	// Tokenize by non-alphanumeric characters
	words := strings.FieldsFunc(signature, func(r rune) bool {
		isAlphaNum := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_'
		return !isAlphaNum
	})

	for _, word := range words {
		if len(word) == 0 || builtins[word] {
			continue
		}
		// Only include capitalized words (exported types)
		if word[0] >= 'A' && word[0] <= 'Z' {
			if !seen[word] {
				seen[word] = true
				types = append(types, word)
			}
		}
	}

	return types
}

// isTypeKind returns true for symbol kinds that represent Go types.
func isTypeKind(kind string) bool {
	return kind == "struct" || kind == "interface" || kind == "type"
}
