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
	explainMode     string
	explainWarnings string
	explainAt       string
)

var explainCmd = &cobra.Command{
	Use:     "explain [symbol]",
	Short:   "Structured function explanation for LLMs",
	GroupID: "advanced",
	Long: `Generates a structured explanation of a function or method.

Output includes:
  - purpose: Best-effort summary with explicit source tracking
  - mechanism: Observable execution steps (callees with action verbs)
  - caller_context: Who calls this and patterns detected
  - warnings: High-precision static analysis findings
  - doc_status: Documentation freshness assessment

Modes:
  brief   - Minimal analysis, fastest (<20ms)
  normal  - Standard analysis (default, <50ms)
  deep    - Full analysis including all callers

Warning levels:
  none    - Skip warning analysis
  fast    - Quick AST-only checks (default)
  full    - Comprehensive analysis

Examples:
  snipe explain OpenStore              # Explain by name
  snipe explain --at cmd/root.go:185   # Explain at position
  snipe explain OpenStore --mode=deep  # Full analysis
  snipe explain OpenStore --warnings=none  # Skip warnings`,
	Args: cobra.MaximumNArgs(1),
	RunE: runExplain,
}

func init() {
	explainCmd.Flags().StringVar(&explainMode, "mode", "normal", "Analysis depth: brief, normal, deep")
	explainCmd.Flags().StringVar(&explainWarnings, "warnings", "fast", "Warning level: none, fast, full")
	explainCmd.Flags().StringVar(&explainAt, "at", "", "Position to explain (file:line:col)")
	rootCmd.AddCommand(explainCmd)
}

func runExplain(cmd *cobra.Command, args []string) error {
	start := time.Now()

	compact, _, _, _, _, _ := GetOutputConfig()
	w := output.NewWriter(os.Stdout, compact)

	// Need either a symbol name or --at position
	if len(args) == 0 && explainAt == "" {
		return w.WriteError("explain", &output.Error{
			Code:    output.ErrInternal,
			Message: "provide a symbol name or --at position",
		})
	}

	// Open store
	s, dir, err := OpenStore(w, "explain")
	if err != nil {
		return err
	}
	defer s.Close()

	var symbolID string
	var queryInfo map[string]string

	if explainAt != "" {
		// Resolve position
		pos, err := query.ParsePosition(explainAt)
		if err != nil {
			return w.WriteError("explain", &output.Error{
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
			return w.WriteError("explain", &output.Error{
				Code:    output.ErrNotFound,
				Message: err.Error(),
			})
		}
		queryInfo = map[string]string{"at": explainAt}
	} else {
		name := args[0]

		// Check if input looks like a symbol ID (16-char hex string)
		if len(name) == 16 {
			if _, err := hex.DecodeString(name); err == nil {
				symbolID = name
				queryInfo = map[string]string{"id": name}
				goto explain
			}
		}

		// Check for file:Symbol syntax
		if idx := strings.LastIndex(name, ":"); idx > 0 && !strings.Contains(name[idx:], "/") {
			filePart := name[:idx]
			symbolPart := name[idx+1:]
			if symbolPart != "" && !strings.Contains(symbolPart, ":") {
				symbols, err := query.LookupByNameInFile(s.DB(), symbolPart, filePart)
				if err != nil {
					return w.WriteError("explain", &output.Error{
						Code:    output.ErrInternal,
						Message: err.Error(),
					})
				}
				if len(symbols) == 1 {
					symbolID = symbols[0].ID
					queryInfo = map[string]string{"symbol": symbolPart, "file": filePart}
					goto explain
				} else if len(symbols) > 1 {
					candidates := make([]output.Candidate, len(symbols))
					for i, sym := range symbols {
						candidates[i] = sym.ToCandidate()
					}
					return w.WriteError("explain", output.NewAmbiguousError(name, candidates))
				}
			}
		}

		// Look up by name
		symbols, err := query.LookupByName(s.DB(), name)
		if err != nil {
			return w.WriteError("explain", &output.Error{
				Code:    output.ErrInternal,
				Message: err.Error(),
			})
		}

		if len(symbols) == 0 {
			maxDist := query.DefaultMaxDistance(name)
			suggestions, err := query.FindSimilarSymbols(s.DB(), name, maxDist, 3)
			if err != nil {
				return w.WriteError("explain", output.NewNotFoundError(name))
			}
			return w.WriteError("explain", output.NewNotFoundError(name, suggestions...))
		}

		if len(symbols) > 1 {
			candidates := make([]output.Candidate, len(symbols))
			for i, sym := range symbols {
				candidates[i] = sym.ToCandidate()
			}
			return w.WriteError("explain", output.NewAmbiguousError(name, candidates))
		}

		symbolID = symbols[0].ID
		queryInfo = map[string]string{"symbol": name}
	}

explain:
	// Parse options
	opts := query.DefaultExplainOptions()

	switch explainMode {
	case "brief":
		opts.Mode = output.ExplainBrief
	case "normal":
		opts.Mode = output.ExplainNormal
	case "deep":
		opts.Mode = output.ExplainDeep
	default:
		return w.WriteError("explain", &output.Error{
			Code:    output.ErrInternal,
			Message: "invalid --mode: use brief, normal, or deep",
		})
	}

	switch explainWarnings {
	case "none":
		opts.WarningsMode = output.WarningsNone
	case "fast":
		opts.WarningsMode = output.WarningsFast
	case "full":
		opts.WarningsMode = output.WarningsFull
	default:
		return w.WriteError("explain", &output.Error{
			Code:    output.ErrInternal,
			Message: "invalid --warnings: use none, fast, or full",
		})
	}

	// Look up symbol for staleness check (Explain does this internally too)
	var staleFiles []string
	if sym, lookupErr := query.LookupByID(s.DB(), symbolID); lookupErr == nil && sym != nil {
		staleFiles = query.CheckPathStaleness(s.DB(), dir, []string{sym.FilePath})
	}

	// Run explain
	result, err := query.Explain(s.DB(), symbolID, opts)
	if err != nil {
		return w.WriteError("explain", &output.Error{
			Code:    output.ErrInternal,
			Message: err.Error(),
		})
	}

	resp := output.Response[output.ExplainResult]{
		Protocol: output.ProtocolVersion,
		Ok:       true,
		Results:  []output.ExplainResult{*result},
		Meta: output.Meta{
			Command:    "explain",
			Query:      queryInfo,
			RepoRoot:   dir,
			IndexState: query.CheckIndexState(s.DB(), dir, Version),
			Ms:         time.Since(start).Milliseconds(),
			Total:      1,
			StaleFiles: staleFiles,
		},
	}

	return w.WriteResponse(resp)
}
