package cmd

import (
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/dkoosis/snipe/internal/embed"
	"github.com/dkoosis/snipe/internal/output"
	"github.com/dkoosis/snipe/internal/query"
	"github.com/dkoosis/snipe/internal/store"
)

var simCmd = &cobra.Command{
	Use:     "sim <query>",
	Short:   "Semantic similarity search",
	GroupID: "advanced",
	Long: `Finds symbols semantically similar to the query using embeddings.

Requires embeddings to be generated first with 'snipe index --embed'.

Examples:
  snipe sim "handle HTTP request"
  snipe sim "database connection pool"
  snipe sim --threshold 0.5 "error handling"`,
	Args: cobra.ExactArgs(1),
	RunE: runSim,
}

var (
	simThreshold float64
)

func init() {
	simCmd.Flags().Float64Var(&simThreshold, "threshold", 0.3, "Minimum similarity threshold (0-1)")
	rootCmd.AddCommand(simCmd)
}

func runSim(cmd *cobra.Command, args []string) error {
	start := time.Now()
	queryText := args[0]

	compact, lim, off, contextLines, withBody, _ := GetOutputConfig()
	format := GetResponseFormat()
	withBody, _, contextLines = ApplyFormatOverrides(format, withBody, false, contextLines)
	summary := format == FormatSummary
	w := output.NewWriter(os.Stdout, compact)

	dir, err := os.Getwd()
	if err != nil {
		return w.WriteError("sim", &output.Error{
			Code:    output.ErrInternal,
			Message: "failed to get working directory: " + err.Error(),
		})
	}

	dbPath := store.DefaultIndexPath(dir)
	if store.IsIndexing(dbPath) {
		return w.WriteError("sim", output.NewIndexInProgressError())
	}
	if !store.Exists(dbPath) {
		return w.WriteError("sim", output.NewMissingIndexError())
	}

	s, err := store.Open(dbPath)
	if err != nil {
		return w.WriteError("sim", &output.Error{
			Code:    output.ErrInternal,
			Message: "failed to open index: " + err.Error(),
		})
	}
	defer s.Close()

	// Get embedding client
	client, err := embed.NewClient()
	if err != nil {
		return w.WriteError("sim", &output.Error{
			Code:    output.ErrInternal,
			Message: "embedding client: " + err.Error(),
		})
	}

	// Run semantic search (fetch off+lim to support offset)
	threshold := float32(simThreshold)
	searchLimit := off + lim
	results, _, simErr := embed.Search(queryText, s, client, searchLimit, threshold)
	if simErr != nil {
		return w.WriteError("sim", &output.Error{
			Code:    output.ErrInternal,
			Message: simErr.Error(),
		})
	}
	if results == nil {
		return w.WriteError("sim", &output.Error{
			Code:    output.ErrInternal,
			Message: "no embeddings found. Run 'snipe index --embed' first",
		})
	}

	// Apply offset
	totalBeforeOffset := len(results)
	if off > 0 && off < len(results) {
		results = results[off:]
	} else if off >= len(results) {
		results = nil
	}
	if len(results) > lim {
		results = results[:lim]
	}

	// Add body/context and track degraded
	var degraded []string
	for i := range results {
		if withBody {
			if err := output.AddBody(&results[i]); err != nil {
				degraded = append(degraded, "body_extraction_failed")
			}
		}
		if contextLines > 0 && !withBody {
			if err := output.AddContext(&results[i], contextLines); err != nil {
				degraded = append(degraded, "context_extraction_failed")
			}
		}
	}
	degraded = uniqueStrings(degraded)

	// Apply token budget truncation if specified
	maxTok := GetMaxTokens()
	tokenTruncated := false
	if maxTok > 0 {
		results, tokenTruncated = output.TruncateToTokenBudget(results, maxTok)
	}

	staleFiles := query.CheckFileStaleness(s.DB(), dir, results)

	if summary {
		summaryData := output.BuildSummary(results)
		summaryResp := output.Response[output.Summary]{
			Protocol: output.ProtocolVersion,
			Ok:       true,
			Results:  []output.Summary{summaryData},
			Meta: output.Meta{
				Command:    "sim",
				Query:      map[string]string{"query": queryText, "threshold": cmd.Flag("threshold").Value.String()},
				RepoRoot:   dir,
				IndexState: query.CheckIndexState(s.DB(), dir, Version),
				Degraded:   degraded,
				Ms:         time.Since(start).Milliseconds(),
				Total:      summaryData.Total,
				Offset:     off,
				Limit:      lim,
				Truncated:  totalBeforeOffset >= searchLimit,
				StaleFiles: staleFiles,
			},
		}
		return w.WriteResponse(summaryResp)
	}

	// Calculate token estimate after truncation
	tokenEstimate := 0
	for i := range results {
		tokenEstimate += output.EstimateResultTokens(&results[i])
	}

	resp := output.Response[output.Result]{
		Protocol: output.ProtocolVersion,
		Ok:       true,
		Results:  results,
		Meta: output.Meta{
			Command:       "sim",
			Query:         map[string]string{"query": queryText, "threshold": cmd.Flag("threshold").Value.String()},
			RepoRoot:      dir,
			IndexState:    query.CheckIndexState(s.DB(), dir, Version),
			Degraded:      degraded,
			Ms:            time.Since(start).Milliseconds(),
			Total:         len(results),
			Offset:        off,
			Limit:         lim,
			Truncated:     totalBeforeOffset >= searchLimit || tokenTruncated,
			TokenEstimate: tokenEstimate,
			StaleFiles:    staleFiles,
		},
	}

	return w.WriteResponse(resp)
}
