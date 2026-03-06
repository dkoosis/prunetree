package cmd

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dkoosis/snipe/internal/embed"
	"github.com/dkoosis/snipe/internal/output"
	"github.com/dkoosis/snipe/internal/query"
	"github.com/dkoosis/snipe/internal/search"
	"github.com/dkoosis/snipe/internal/store"
)

var identifierRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.]*$`)

var searchFile string

var searchCmd = &cobra.Command{
	Use:     "search <pattern>",
	Short:   "Text search via ripgrep",
	GroupID: "core",
	Long: `Searches for a pattern using ripgrep. Works without an index.

If no text matches are found and embeddings are available, automatically
falls back to semantic similarity search. Use 'snipe sim' directly for
more control over semantic search parameters.

Examples:
  snipe search "func.*Error"              # Search all files
  snipe search "TODO" --file "*.go"       # Search only Go files
  snipe search "Handler" --file store.go  # Search in specific file`,
	Args: cobra.ExactArgs(1),
	RunE: runSearch,
}

func init() {
	searchCmd.Flags().StringVar(&searchFile, "file", "", "Glob pattern to filter files (e.g., \"*.go\", \"store.go\")")
	rootCmd.AddCommand(searchCmd)
}

func runSearch(cmd *cobra.Command, args []string) error {
	start := time.Now()
	pattern := args[0]

	compact, lim, _, ctx, _, _ := GetOutputConfig()
	format := GetResponseFormat()

	// Apply format overrides
	_, _, ctx = ApplyFormatOverrides(format, false, false, ctx)
	summary := format == FormatSummary

	w := output.NewWriter(os.Stdout, compact)

	// Get current directory
	dir, err := os.Getwd()
	if err != nil {
		return w.WriteError("search", &output.Error{
			Code:    output.ErrInternal,
			Message: "failed to get working directory: " + err.Error(),
		})
	}

	// Open store once — used for both enrichment and potential semantic fallback
	dbPath := store.DefaultIndexPath(dir)
	var s *store.Store
	if store.Exists(dbPath) && !store.IsIndexing(dbPath) {
		if opened, err := store.Open(dbPath); err == nil {
			s = opened
			defer s.Close()
		}
	}

	var globs []string
	if searchFile != "" {
		globs = append(globs, searchFile)
	} else if identifierRe.MatchString(pattern) {
		// Identifier-like queries: restrict to Go source to avoid docs/changelogs
		globs = append(globs, "*.go")
	}
	results, err := search.Search(dir, pattern, lim, ctx, globs...)
	if err != nil {
		code := output.ErrInternal
		if strings.Contains(err.Error(), "not found") {
			code = output.ErrRgNotFound
		}
		return w.WriteError("search", &output.Error{
			Code:    code,
			Message: err.Error(),
		})
	}

	// Semantic fallback — only on zero rg results, no --file filter.
	// Threshold 0.3 is intentionally lower than sim's default: fallback is a safety net
	// for "find the thing that does X" queries, not a precision tool.
	// Note: identifier-like patterns add a *.go glob to rg but the semantic search
	// operates over the full embedding space regardless of file type.
	usedFallback := false
	var decisionPath []string
	if len(results) == 0 && searchFile == "" && s != nil && embed.HasCredentials() {
		client, clientErr := embed.NewClient()
		if clientErr != nil {
			decisionPath = append(decisionPath, "rg:0_results", "sim:client_error")
		} else {
			simResults, simDur, simErr := embed.Search(pattern, s, client, lim, 0.3)
			if simErr == nil && len(simResults) > 0 {
				results = simResults
				decisionPath = []string{
					"rg:0_results",
					fmt.Sprintf("sim:%d_results:%dms", len(simResults), simDur.Milliseconds()),
				}
				usedFallback = true
			} else if simErr != nil {
				decisionPath = append(decisionPath, "rg:0_results", "sim:error")
			}
		}
	}

	// Enrich rg results with index metadata if available (skip for semantic results)
	var indexState output.IndexState
	enriched := false
	if s != nil && !usedFallback {
		for i := range results {
			if sym := query.FindSymbolAtPosition(s.DB(), results[i].File, results[i].Range.Start.Line); sym != nil {
				results[i].Name = sym.Name
				results[i].Kind = sym.Kind
				if sym.Receiver.Valid && sym.Receiver.String != "" {
					results[i].Receiver = sym.Receiver.String
				}
				enriched = true
			}
		}
		indexState = query.CheckIndexState(s.DB(), dir, Version)
	} else if usedFallback && s != nil {
		// TODO: semantic results reference indexed symbols — CheckFileStaleness
		// is relevant here but search has never included stale_files. Add when
		// search gets staleness support generally.
		indexState = query.CheckIndexState(s.DB(), dir, Version)
		enriched = true
	}

	// Score, sort, and apply selection (only for rg results)
	if !usedFallback {
		output.ScoreAndSort(results, pattern)
		results = ApplySelection(results)
	}

	// Apply token budget truncation if specified
	maxTok := GetMaxTokens()
	tokenTruncated := false
	if maxTok > 0 {
		results, tokenTruncated = output.TruncateToTokenBudget(results, maxTok)
	}

	// Determine index state and degraded flags for response metadata
	searchIndexState := output.IndexNotUsed
	var searchDegraded []string
	if enriched {
		searchIndexState = indexState
	} else {
		searchDegraded = []string{"no_index"}
	}

	// If summary mode, return condensed output
	if summary {
		summaryData := output.BuildSummary(results)
		summaryResp := output.Response[output.Summary]{
			Protocol: output.ProtocolVersion,
			Ok:       true,
			Results:  []output.Summary{summaryData},
			Meta: output.Meta{
				Command:      "search",
				Query:        searchQueryInfo(pattern),
				IndexState:   searchIndexState,
				Degraded:     searchDegraded,
				DecisionPath: decisionPath,
				Ms:           time.Since(start).Milliseconds(),
				Total:        summaryData.Total,
				Truncated:    len(results) >= lim,
			},
		}
		return w.WriteResponse(summaryResp)
	}

	// Estimate tokens
	tokenEstimate := 0
	for i := range results {
		tokenEstimate += output.EstimateResultTokens(&results[i])
	}

	resp := output.Response[output.Result]{
		Protocol:    output.ProtocolVersion,
		Ok:          true,
		Results:     results,
		Suggestions: output.SuggestionsForSearch(pattern, len(results), usedFallback),
		Meta: output.Meta{
			Command:       "search",
			Query:         searchQueryInfo(pattern),
			IndexState:    searchIndexState,
			Degraded:      searchDegraded,
			DecisionPath:  decisionPath,
			Ms:            time.Since(start).Milliseconds(),
			Total:         len(results),
			Truncated:     len(results) >= lim || tokenTruncated,
			TokenEstimate: tokenEstimate,
		},
	}

	return w.WriteResponse(resp)
}

func searchQueryInfo(pattern string) map[string]string {
	q := map[string]string{"pattern": pattern}
	if searchFile != "" {
		q["file"] = searchFile
	}
	return q
}
