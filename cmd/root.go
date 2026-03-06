package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/dkoosis/snipe/internal/config"
	ctxpkg "github.com/dkoosis/snipe/internal/context"
	"github.com/dkoosis/snipe/internal/output"
	"github.com/dkoosis/snipe/internal/store"
)

var (
	// Global flags
	limit         int
	offset        int
	contextLines  int
	noBody        bool
	noSiblings    bool
	signatureOnly bool
	maxTokens     int
	timeout       time.Duration

	// response_format mode: concise, detailed, or summary
	responseFormat string

	// KG integration
	withKGHints bool

	// Selection mode for multi-result commands
	selectMode string

	// Caller passthrough for correlation
	caller    string
	requestID string

	// Internal: auto-compact when piped
	autoCompact bool

	// loadedConfig holds the merged config (loaded lazily)
	loadedConfig *config.Config

	// cmdCtx is the context for the current command (with timeout and signal handling)
	cmdCtx    context.Context
	cmdCancel context.CancelFunc
)

var rootCmd = &cobra.Command{
	Use:   "snipe [symbol]",
	Short: "Code navigation CLI for LLMs",
	Long: `snipe: Go code navigation for LLMs.

  snipe index              Build index (run first)
  snipe def ProcessOrder   Jump to definition
  snipe refs ProcessOrder  Find all references
  snipe callers Handler    Who calls this?
  snipe pack ProcessOrder  Everything about a symbol
  snipe search "TODO"      Text search (no index needed)

  snipe doctor             Check index health
  snipe context --boot     LLM boot context`,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// Set up context with signal handling for graceful cancellation
		ctx := context.Background()
		var cancel context.CancelFunc
		if timeout > 0 {
			ctx, cancel = context.WithTimeout(ctx, timeout)
		} else {
			ctx, cancel = context.WithCancel(ctx)
		}
		cmdCancel = cancel

		// Handle Ctrl+C gracefully
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		go func() {
			select {
			case <-sigCh:
				fmt.Fprintln(os.Stderr, "\nInterrupted, cleaning up...")
				cmdCancel()
			case <-ctx.Done():
			}
			signal.Stop(sigCh)
		}()

		cmdCtx = ctx

		// Load config and apply defaults if flags weren't explicitly set
		cwd, err := os.Getwd()
		if err != nil {
			cwd = ""
		}
		cfg, err := config.Load(cwd)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		loadedConfig = cfg

		// Apply config defaults only if flags weren't explicitly set
		if !cmd.Flags().Changed("limit") && cfg.Limit > 0 {
			limit = cfg.Limit
		}
		if !cmd.Flags().Changed("context") && cfg.ContextLines > 0 {
			contextLines = cfg.ContextLines
		}

		// Auto-compact when output is piped (not a TTY)
		autoCompact = true

		return nil
	},
	PersistentPostRun: func(cmd *cobra.Command, args []string) {
		// Cancel context to release signal goroutine
		if cmdCancel != nil {
			cmdCancel()
		}
	},
	RunE: runStatus,
}

func Execute() {
	// Check if first non-flag arg looks like a symbol (not a known subcommand)
	// This allows "snipe Store" to work without "snipe sym Store"
	args := os.Args[1:]
	if len(args) > 0 && !isKnownSubcommandOrFlag(args[0]) {
		// Rewrite args to use sym subcommand: "snipe Store" -> "snipe sym Store"
		newArgs := append([]string{os.Args[0], "sym"}, args...)
		os.Args = newArgs
	}

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// knownSubcommands lists all registered snipe subcommands.
// Updated when new commands are added.
var knownSubcommands = map[string]bool{
	"help": true, "completion": true,
	// Core commands
	"index": true, "def": true, "refs": true, "callers": true, "callees": true,
	"search": true, "show": true, "sym": true, "status": true, "tests": true, "impact": true,
	// Analysis commands
	"impl": true, "types": true, "imports": true, "importers": true, "pkg": true, "deps": true,
	// Edit, explain, and pack
	"edit": true, "explain": true, "pack": true,
	// Maintenance commands
	"baseline": true, "context": true, "embed-status": true, "version": true,
	"doctor": true, "schema": true, "check": true, "history": true,
	// Semantic search
	"sim": true,
	// Watch mode
	"watch": true,
}

// isKnownSubcommandOrFlag checks if arg is a subcommand or flag.
func isKnownSubcommandOrFlag(arg string) bool {
	// Flags start with -
	if len(arg) > 0 && arg[0] == '-' {
		return true
	}
	return knownSubcommands[arg]
}

func init() {
	// Hide completion command from help
	rootCmd.CompletionOptions.HiddenDefaultCmd = true

	// Add command groups for 3-tier visibility
	rootCmd.AddGroup(
		&cobra.Group{ID: "core", Title: "Core Commands:"},
		&cobra.Group{ID: "index", Title: "Index Commands:"},
		&cobra.Group{ID: "advanced", Title: "Advanced Commands:"},
	)

	rootCmd.PersistentFlags().IntVar(&limit, "limit", 50, "Cap results")
	rootCmd.PersistentFlags().IntVar(&offset, "offset", 0, "Pagination offset")
	rootCmd.PersistentFlags().IntVar(&contextLines, "context", 3, "Context lines around match")
	rootCmd.PersistentFlags().BoolVar(&noBody, "no-body", false, "Exclude function body")
	rootCmd.PersistentFlags().BoolVar(&noSiblings, "no-siblings", false, "Exclude sibling declarations")
	rootCmd.PersistentFlags().BoolVar(&signatureOnly, "signature-only", false, "Return only signature (no body, no context)")
	rootCmd.PersistentFlags().IntVar(&maxTokens, "max-tokens", 0, "Token budget (0 = unlimited)")
	rootCmd.PersistentFlags().StringVar(&responseFormat, "format", "", "concise | detailed | summary")
	rootCmd.PersistentFlags().BoolVar(&withKGHints, "kg-hints", false, "Include Orca KG hints")
	rootCmd.PersistentFlags().DurationVar(&timeout, "timeout", 0, "Timeout for command (e.g., 30s, 5m)")
	rootCmd.PersistentFlags().StringVar(&selectMode, "select", "all", "Result selection: all, best, top3, top5")
	// Reserved for orca telemetry — hidden until persistToolCall is wired.
	rootCmd.PersistentFlags().StringVar(&caller, "caller", "", "Caller identifier (e.g., 'orca')")
	rootCmd.PersistentFlags().StringVar(&requestID, "request-id", "", "Request correlation ID")
	_ = rootCmd.PersistentFlags().MarkHidden("caller")
	_ = rootCmd.PersistentFlags().MarkHidden("request-id")
}

// GetContext returns the command context (with timeout and signal handling).
// Returns context.Background() if called before PersistentPreRunE.
func GetContext() context.Context {
	if cmdCtx == nil {
		return context.Background()
	}
	return cmdCtx
}

// ResponseFormat represents output format modes for go_symbol parity.
type ResponseFormat string

const (
	// FormatDefault uses command-specific defaults.
	FormatDefault ResponseFormat = ""
	// FormatConcise strips bodies, minimal metadata.
	FormatConcise ResponseFormat = "concise"
	// FormatDetailed includes full metadata and all hints.
	FormatDetailed ResponseFormat = "detailed"
	// FormatSummary aggregates results by file (counts only).
	FormatSummary ResponseFormat = "summary"
)

// GetOutputConfig returns the current output configuration.
// Returns: compact, limit, offset, contextLines, withBody, withSiblings
func GetOutputConfig() (compact bool, lim int, off int, ctx int, body bool, siblings bool) {
	// Apply signature-only override
	if signatureOnly {
		return autoCompact, limit, offset, 0, false, false
	}
	return autoCompact, limit, offset, contextLines, !noBody, !noSiblings
}

// GetResponseFormat returns the response format mode.
func GetResponseFormat() ResponseFormat {
	return ResponseFormat(responseFormat)
}

// ApplyFormatOverrides adjusts output config based on --format flag.
// Returns (withBody, withSiblings, contextLines) based on format mode.
func ApplyFormatOverrides(format ResponseFormat, baseBody, baseSiblings bool, baseContext int) (bool, bool, int) {
	switch format {
	case FormatConcise:
		// Concise: no body, no siblings, minimal context
		return false, false, 0
	case FormatDetailed:
		// Detailed: everything enabled, full context
		return true, true, baseContext
	case FormatSummary:
		// Summary: no body needed (just counts)
		return false, false, 0
	case FormatDefault:
		// Default: use base values
		return baseBody, baseSiblings, baseContext
	}
	// Unreachable, but satisfies exhaustive check
	return baseBody, baseSiblings, baseContext
}

// GetWithKGHints returns whether KG hints should be included.
func GetWithKGHints() bool {
	return withKGHints
}

// GetMaxTokens returns the max-tokens flag value (0 = unlimited)
func GetMaxTokens() int {
	return maxTokens
}

// uniqueStrings removes duplicates from a string slice, preserving order.
func uniqueStrings(ss []string) []string {
	if len(ss) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	result := make([]string, 0, len(ss))
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}

// GetConfig returns the loaded configuration.
func GetConfig() *config.Config {
	if loadedConfig == nil {
		return config.DefaultConfig()
	}
	return loadedConfig
}

// ApplySelection truncates results based on the --select flag.
// Should be called after ScoreAndSort.
func ApplySelection(results []output.Result) []output.Result {
	switch selectMode {
	case "best":
		if len(results) > 1 {
			return results[:1]
		}
	case "top3":
		if len(results) > 3 {
			return results[:3]
		}
	case "top5":
		if len(results) > 5 {
			return results[:5]
		}
	}
	// "all" or unrecognized: return everything
	return results
}

// GetCaller returns the --caller flag value.
func GetCaller() string { return caller }

// GetRequestID returns the --request-id flag value.
func GetRequestID() string { return requestID }

// OpenStore opens the index for query commands.
// Returns the store, working directory, and any error.
func OpenStore(w *output.Writer, cmdName string) (*store.Store, string, error) {
	dir, err := os.Getwd()
	if err != nil {
		if w != nil {
			_ = w.WriteError(cmdName, &output.Error{
				Code:    output.ErrInternal,
				Message: "failed to get working directory: " + err.Error(),
			})
		}
		return nil, "", err
	}

	dbPath := store.DefaultIndexPath(dir)

	// Check if indexing is in progress
	if store.IsIndexing(dbPath) {
		if w != nil {
			_ = w.WriteError(cmdName, output.NewIndexInProgressError())
		}
		return nil, dir, fmt.Errorf("indexing in progress")
	}

	// Check for missing index
	if !store.Exists(dbPath) {
		if w != nil {
			_ = w.WriteError(cmdName, output.NewMissingIndexError())
		}
		return nil, dir, fmt.Errorf("index missing")
	}

	s, err := store.Open(dbPath)
	if err != nil {
		if w != nil {
			_ = w.WriteError(cmdName, &output.Error{
				Code:    output.ErrInternal,
				Message: "failed to open index: " + err.Error(),
			})
		}
		return nil, dir, err
	}

	return s, dir, nil
}

// recordSessionQuery records a symbol query in the session for active work tracking.
// This is a best-effort operation - errors are silently ignored to not affect command execution.
func recordSessionQuery(projectRoot, symbol, file string, line int, kind, command string) {
	session, err := ctxpkg.LoadSession(projectRoot)
	if err != nil {
		return
	}
	session.RecordQuery(symbol, file, line, kind, command)
	_ = ctxpkg.SaveSession(session)
}
