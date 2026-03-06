package output

import "fmt"

// ProtocolVersion is the snipe wire protocol version.
// Bump when the response envelope schema changes in a breaking way.
const ProtocolVersion = 1

// Response is the top-level response structure for all commands
type Response[T any] struct {
	Protocol    int          `json:"protocol"`
	Ok          bool         `json:"ok"`
	Results     []T          `json:"results"`
	Meta        Meta         `json:"meta"`
	Error       *Error       `json:"error"`
	Suggestions []Suggestion `json:"suggestions,omitempty"`
}

// Suggestion provides actionable next steps for LLM consumers
type Suggestion struct {
	Command     string `json:"command"`             // The suggested snipe command
	Description string `json:"description"`         // Why this command might be useful
	Priority    int    `json:"priority,omitempty"`  // 1=high, 2=medium, 3=low
	Condition   string `json:"condition,omitempty"` // When this suggestion applies
}

// Meta contains metadata about the query execution
type Meta struct {
	Command       string            `json:"command"`
	Query         map[string]string `json:"query,omitempty"`
	RepoRoot      string            `json:"repo_root,omitempty"`
	IndexState    IndexState        `json:"index_state"`
	Degraded      []string          `json:"degraded,omitempty"`
	Caller        string            `json:"caller,omitempty"`
	RequestID     string            `json:"request_id,omitempty"`
	Ms            int64             `json:"ms"`
	Total         int               `json:"total"`
	Offset        int               `json:"offset,omitempty"`
	Limit         int               `json:"limit,omitempty"`
	Truncated     bool              `json:"truncated"`
	TokenEstimate int               `json:"token_estimate,omitempty"`
	DecisionPath  []string          `json:"decision_path,omitempty"` // Resolution strategy trace
	StaleFiles    []string          `json:"stale_files,omitempty"`   // Files changed since last index
}

// IndexState represents the state of the index
type IndexState string

const (
	IndexFresh   IndexState = "fresh"
	IndexStale   IndexState = "stale"
	IndexMissing IndexState = "missing"
	IndexNotUsed IndexState = "not_used"
)

// Error represents an error response
type Error struct {
	Code       string      `json:"code"`
	Message    string      `json:"message"`
	Next       *NextAction `json:"next,omitempty"`
	Candidates []Candidate `json:"candidates,omitempty"`
}

// NextAction suggests the next command to run
type NextAction struct {
	Command     string `json:"command"`
	Description string `json:"description,omitempty"`
}

// Candidate represents an ambiguous symbol match
type Candidate struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	File     string `json:"file"`
	Kind     string `json:"kind"`
	Receiver string `json:"receiver,omitempty"`
	Doc      string `json:"doc,omitempty"`
}

// Result represents a single navigation result
type Result struct {
	ID             string          `json:"id"`
	File           string          `json:"file"` // Relative path (for output)
	FileAbs        string          `json:"-"`    // Absolute path (for file operations, not exported)
	Range          Range           `json:"range"`
	Kind           string          `json:"kind,omitempty"`
	Name           string          `json:"name,omitempty"`
	Receiver       string          `json:"receiver,omitempty"` // Method receiver type, e.g., "(*Server)" or "(Config)"
	Package        string          `json:"package,omitempty"`  // Go package path
	Match          string          `json:"match,omitempty"`
	Body           string          `json:"body,omitempty"`
	Score          float64         `json:"score,omitempty"`
	Role           string          `json:"role,omitempty"`
	RefCount       int             `json:"ref_count"`                 // Number of references to this symbol (-1 = unavailable)
	Hints          []string        `json:"hints,omitempty"`           // Static analysis hints: deprecated, unused, etc.
	CallersPreview []CallerPreview `json:"callers_preview,omitempty"` // Top callers for func/method
	Analysis       *FuncAnalysis   `json:"analysis,omitempty"`        // Function/method analysis (for func/method kinds)
	KGHints        []KGHint        `json:"kg_hints,omitempty"`        // Knowledge graph hints from Orca
	Enclosing      *Enclosing      `json:"enclosing,omitempty"`
	Context        *Context        `json:"context,omitempty"`
	Siblings       []Sibling       `json:"siblings,omitempty"`
	EditTarget     string          `json:"edit_target,omitempty"`
}

// FuncAnalysis provides metrics about a function or method.
// Populated for func/method kinds in detailed format.
type FuncAnalysis struct {
	LineCount    int    `json:"line_count"`              // Number of lines in function body
	ParamCount   int    `json:"param_count"`             // Number of parameters
	ResultCount  int    `json:"result_count"`            // Number of return values
	ReceiverType string `json:"receiver_type,omitempty"` // Receiver type for methods (e.g., "*Server")
	IsExported   bool   `json:"is_exported"`             // Whether the function is exported
	IsVariadic   bool   `json:"is_variadic,omitempty"`   // Whether the function has variadic params
}

// CallerPreview represents a preview of a function caller
type CallerPreview struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	File string `json:"file"`
	Line int    `json:"line"`
}

// KGHint represents a hint from the Orca knowledge graph
type KGHint struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`               // trap, pattern, etc.
	Severity string `json:"severity,omitempty"` // h, m, l for traps
	Summary  string `json:"summary"`
}

// Hint constants for static analysis
const (
	HintDeprecated  = "deprecated"   // Symbol is marked as deprecated
	HintUnused      = "unused"       // Exported symbol with no references
	HintPointerRecv = "pointer_recv" // Method has pointer receiver (nil-callable)
)

// Impact hint constants
const (
	HintDirectCaller     = "direct_caller"
	HintTransitiveCaller = "transitive_caller"
	HintImplementer      = "implementer"
	HintDirectTest       = "direct_test"
	HintTransitiveTest   = "transitive_test"
	HintExported         = "exported" // Symbol is part of the public API surface
)

// SuggestionsForImpact generates suggestions after an impact command.
func SuggestionsForImpact(symbol string, directCallers, transitiveCallers, implementers, tests, pkgCount int) []Suggestion {
	var suggestions []Suggestion

	summary := fmt.Sprintf("Impact: %d direct callers, %d transitive, %d implementers, %d tests across %d packages",
		directCallers, transitiveCallers, implementers, tests, pkgCount)
	suggestions = append(suggestions, Suggestion{
		Description: summary,
		Priority:    1,
	})

	if tests == 0 {
		suggestions = append(suggestions, Suggestion{
			Command:     "snipe tests " + symbol,
			Description: "No test coverage found — check with transitive search",
			Priority:    1,
			Condition:   "no_tests",
		})
	}

	if transitiveCallers > 10 {
		suggestions = append(suggestions, Suggestion{
			Command:     "snipe callers --direct " + symbol,
			Description: "Many transitive callers — drill into direct callers",
			Priority:    2,
		})
	}

	return suggestions
}

// Sibling represents another declaration of the same kind in the same file
type Sibling struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	Line int    `json:"line"`
}

// Range represents a source code range
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Position represents a line:col position
type Position struct {
	Line int `json:"line"`
	Col  int `json:"col"`
}

// Enclosing represents the enclosing scope of a result
type Enclosing struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Signature string `json:"signature,omitempty"`
	Range     *Range `json:"range,omitempty"`
}

// Context provides surrounding lines for a result
type Context struct {
	Before []string `json:"before,omitempty"`
	After  []string `json:"after,omitempty"`
}

// Summary provides a condensed view of results grouped by file
type Summary struct {
	Total int            `json:"total"`
	Files []FileSummary  `json:"files"`
	Kinds map[string]int `json:"kinds,omitempty"`
}

// FileSummary shows the count of results in a single file
type FileSummary struct {
	File  string `json:"file"`
	Count int    `json:"count"`
}

// Error codes
const (
	ErrNotFound        = "NOT_FOUND"
	ErrAmbiguousSymbol = "AMBIGUOUS_SYMBOL"
	ErrFileListing     = "FILE_LISTING"
	ErrMissingIndex    = "MISSING_INDEX"
	ErrIndexInProgress = "INDEX_IN_PROGRESS"
	ErrStaleIndex      = "STALE_INDEX"
	ErrRgNotFound      = "RG_NOT_FOUND"
	ErrInternal        = "INTERNAL_ERROR"
)

// VersionInfo is the JSON output for the version command with --json.
type VersionInfo struct {
	Version  string   `json:"version"`
	Protocol int      `json:"protocol"`
	Features []string `json:"features"`
	Commit   string   `json:"commit,omitempty"`
}

// NewNotFoundError creates a NOT_FOUND error with optional similar symbol suggestions.
func NewNotFoundError(symbol string, suggestions ...string) *Error {
	msg := "Symbol not found: " + symbol
	if len(suggestions) > 0 {
		msg += ". Did you mean: " + joinSuggestions(suggestions)
	}
	return &Error{
		Code:    ErrNotFound,
		Message: msg,
	}
}

// joinSuggestions formats a list of suggestions for display.
func joinSuggestions(suggestions []string) string {
	if len(suggestions) == 0 {
		return ""
	}
	if len(suggestions) == 1 {
		return suggestions[0] + "?"
	}
	// Join all but the last with commas, then add "or" before the last
	result := ""
	for i, s := range suggestions[:len(suggestions)-1] {
		if i > 0 {
			result += ", "
		}
		result += s
	}
	result += ", or " + suggestions[len(suggestions)-1] + "?"
	return result
}

// NewAmbiguousError creates an AMBIGUOUS_SYMBOL error
func NewAmbiguousError(symbol string, candidates []Candidate) *Error {
	return &Error{
		Code:       ErrAmbiguousSymbol,
		Message:    "Multiple definitions found for '" + symbol + "'",
		Candidates: candidates,
	}
}

// NewMissingIndexError creates a MISSING_INDEX error
func NewMissingIndexError() *Error {
	return &Error{
		Code:    ErrMissingIndex,
		Message: "No index found. Run: snipe index (~5s for most projects). This creates .snipe/ -- add it to .gitignore.",
		Next: &NextAction{
			Command:     "snipe index",
			Description: "Build the symbol index for this repository",
		},
	}
}

// NewIndexInProgressError creates an INDEX_IN_PROGRESS error
func NewIndexInProgressError() *Error {
	return &Error{
		Code:    ErrIndexInProgress,
		Message: "Indexing in progress. Wait for 'snipe index' to complete, then retry.",
		Next: &NextAction{
			Command:     "snipe status",
			Description: "Check indexing progress",
		},
	}
}

// SuggestionsForDef generates suggestions after a def command
func SuggestionsForDef(result *Result) []Suggestion {
	if result == nil {
		return nil
	}

	suggestions := []Suggestion{
		{
			Command:     "snipe refs " + result.Name,
			Description: "Find all usages of this symbol",
			Priority:    1,
		},
	}

	// If it's a function/method, suggest callers
	if result.Kind == "func" || result.Kind == "method" {
		suggestions = append(suggestions, Suggestion{
			Command:     "snipe callers " + result.Name,
			Description: "Find functions that call this",
			Priority:    2,
		})
		suggestions = append(suggestions, Suggestion{
			Command:     "snipe callees " + result.Name,
			Description: "Find functions called by this",
			Priority:    2,
		})
	}

	return suggestions
}

// SuggestionsForRefs generates suggestions after a refs command
func SuggestionsForRefs(symbol string, resultCount int) []Suggestion {
	suggestions := []Suggestion{
		{
			Command:     "snipe def " + symbol,
			Description: "Jump to the definition",
			Priority:    1,
		},
	}

	if resultCount > 10 {
		suggestions = append(suggestions, Suggestion{
			Command:     "snipe refs " + symbol + " --format=summary",
			Description: "Get summary grouped by file",
			Priority:    2,
			Condition:   "many results",
		})
	}

	return suggestions
}

// SuggestionsForSearch generates suggestions after a search command.
// usedFallback indicates whether semantic similarity was used as fallback.
func SuggestionsForSearch(pattern string, resultCount int, usedFallback bool) []Suggestion {
	var suggestions []Suggestion

	if usedFallback && resultCount > 0 {
		suggestions = append(suggestions, Suggestion{
			Command:     "snipe sim \"" + pattern + "\"",
			Description: "Results from semantic similarity — use 'snipe sim' for more control",
			Priority:    2,
			Condition:   "semantic_fallback",
		})
		return suggestions
	}

	if resultCount == 0 {
		suggestions = append(suggestions, Suggestion{
			Command:     "snipe sim \"" + pattern + "\"",
			Description: "Try semantic search if you're looking for concepts, not exact text",
			Priority:    2,
		})
	}

	if resultCount > 20 {
		suggestions = append(suggestions, Suggestion{
			Command:     "snipe search \"" + pattern + "\" --format=summary",
			Description: "Get summary grouped by file",
			Priority:    2,
			Condition:   "many results",
		})
	}

	return suggestions
}

// SuggestionsForCallers generates suggestions after a callers command
func SuggestionsForCallers(symbol string, resultCount int) []Suggestion {
	suggestions := []Suggestion{
		{
			Command:     "snipe def " + symbol,
			Description: "View the function definition",
			Priority:    1,
		},
		{
			Command:     "snipe callees " + symbol,
			Description: "See what this function calls",
			Priority:    2,
		},
	}

	return suggestions
}

// SuggestionsForCallees generates suggestions after a callees command
func SuggestionsForCallees(symbol string, resultCount int) []Suggestion {
	return []Suggestion{
		{
			Command:     "snipe def " + symbol,
			Description: "View the function definition",
			Priority:    1,
		},
		{
			Command:     "snipe callers " + symbol,
			Description: "See what calls this function",
			Priority:    2,
		},
	}
}

// SuggestionsForTests generates suggestions after a tests command.
func SuggestionsForTests(symbol string, resultCount int, suggestedFile string) []Suggestion {
	if resultCount == 0 {
		suggestions := []Suggestion{
			{
				Command:     "snipe refs " + symbol,
				Description: "Check if the symbol is referenced anywhere",
				Priority:    1,
			},
		}
		if suggestedFile != "" {
			suggestions = append(suggestions, Suggestion{
				Description: "No tests found for " + symbol + ". Consider adding tests in " + suggestedFile,
				Priority:    2,
			})
		}
		return suggestions
	}
	return []Suggestion{
		{
			Command:     "snipe def " + symbol,
			Description: "View the function definition",
			Priority:    1,
		},
		{
			Command:     "snipe callers " + symbol,
			Description: "See all callers, not just tests",
			Priority:    2,
		},
	}
}

// SuggestionsForAmbiguous generates suggestions when symbol is ambiguous
func SuggestionsForAmbiguous(candidates []Candidate) []Suggestion {
	if len(candidates) == 0 {
		return nil
	}

	suggestions := make([]Suggestion, 0, len(candidates))
	for i, c := range candidates {
		if i >= 3 {
			break // Limit to 3 suggestions
		}
		desc := c.Name
		if c.Receiver != "" {
			desc = c.Receiver + "." + c.Name
		}
		suggestions = append(suggestions, Suggestion{
			Command:     "snipe show " + c.ID,
			Description: desc + " in " + c.File,
			Priority:    1,
		})
	}

	return suggestions
}

// ============================================================================
// Explain types - structured function explanations for LLM consumption
// ============================================================================

// SymResult is the combined response for the sym command.
// Contains definition, references, callers, and callees in a single result.
type SymResult struct {
	Definition  *Result  `json:"definition"`
	References  []Result `json:"references,omitempty"`
	Callers     []Result `json:"callers,omitempty"`
	Callees     []Result `json:"callees,omitempty"`
	RefCount    int      `json:"ref_count"`
	CallerCount int      `json:"caller_count"`
	CalleeCount int      `json:"callee_count"`
}

// MethodSummary describes a method aggregated in a type's pack result.
type MethodSummary struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Signature string `json:"signature,omitempty"`
}

// PackResult is the combined response for the pack command.
// Contains everything an LLM needs: definition, graph, role, score, purpose.
type PackResult struct {
	Definition   *Result         `json:"definition"`
	References   []Result        `json:"references,omitempty"`
	Callers      []Result        `json:"callers,omitempty"`
	Callees      []Result        `json:"callees,omitempty"`
	Methods      []MethodSummary `json:"methods,omitempty"` // populated for struct/interface types
	RefCount     int             `json:"ref_count"`
	CallerCount  int             `json:"caller_count"`
	CalleeCount  int             `json:"callee_count"`
	Role         string          `json:"role,omitempty"`
	Purpose      string          `json:"purpose,omitempty"`
	RelatedTypes []string        `json:"related_types,omitempty"`
}

// DepsResult is the response for single-package dependency queries.
type DepsResult struct {
	Package      string     `json:"package"`
	Dependencies []DepRef   `json:"dependencies"`
	Dependents   []DepRef   `json:"dependents"`
	Cycles       [][]string `json:"cycles,omitempty"`
}

// DepRef references a package with import weight.
type DepRef struct {
	Package   string `json:"package"`
	FileCount int    `json:"file_count"`
}

// DepTreeResult is the response for full dependency graph queries.
type DepTreeResult struct {
	Packages []string      `json:"packages"`
	Edges    []DepTreeEdge `json:"edges"`
	Cycles   [][]string    `json:"cycles,omitempty"`
}

// DepTreeEdge is a directed edge in the dependency tree.
type DepTreeEdge struct {
	From      string `json:"from"`
	To        string `json:"to"`
	FileCount int    `json:"file_count"`
}

// SuggestionsForPack generates suggestions after a pack command.
func SuggestionsForPack(result *Result) []Suggestion {
	if result == nil {
		return nil
	}
	return []Suggestion{
		{
			Command:     "snipe explain " + result.Name,
			Description: "Get detailed explanation with mechanism steps",
			Priority:    1,
		},
		{
			Command:     "snipe context --boot",
			Description: "Get project-level boot context",
			Priority:    2,
		},
	}
}

// ExplainResult contains structured explanation of a symbol.
// Designed for LLM consumption with explicit confidence and source tracking.
type ExplainResult struct {
	Symbol        string          `json:"symbol"`
	File          string          `json:"file"`           // file:line format
	Kind          string          `json:"kind"`           // func, method, type, etc.
	Signature     string          `json:"signature"`      // Always included for context
	Purpose       string          `json:"purpose"`        // Best-effort summary
	PurposeSource PurposeSource   `json:"purpose_source"` // How purpose was derived
	Mechanism     []MechanismStep `json:"mechanism,omitempty"`
	CallerContext *CallerContext  `json:"caller_context,omitempty"`
	Warnings      []Warning       `json:"warnings,omitempty"`
	DocStatus     DocStatus       `json:"doc_status"`
	KeyDeps       []string        `json:"key_deps,omitempty"` // Key dependencies
}

// PurposeSource indicates how the purpose field was derived.
type PurposeSource string

const (
	// PurposeFromDoc means purpose came from doc comment first sentence.
	PurposeFromDoc PurposeSource = "doc"
	// PurposeFromTemplate means purpose was generated from signature patterns.
	PurposeFromTemplate PurposeSource = "template"
	// PurposeMissing means no purpose could be determined.
	PurposeMissing PurposeSource = "missing"
)

// MechanismStep describes one observable execution step.
// Only includes what can be determined from static analysis.
type MechanismStep struct {
	Action string `json:"action"`         // validates, opens, creates, etc.
	Target string `json:"target"`         // What is being acted on
	Note   string `json:"note,omitempty"` // Brief clarification
	Line   int    `json:"line,omitempty"` // Source anchor for navigation
}

// CallerContext summarizes who calls this symbol.
type CallerContext struct {
	Count      int      `json:"count"`                 // Total caller count
	Pattern    string   `json:"pattern,omitempty"`     // Observed pattern, e.g., "all cmd/*.go RunE"
	TopCallers []string `json:"top_callers,omitempty"` // Names of top callers (capped)
}

// Warning represents a static analysis finding.
// High-precision only: we prefer missing a warning over false alarms.
type Warning struct {
	Code     WarningCode `json:"code"`
	Severity string      `json:"severity"` // high, medium, low
	Line     int         `json:"line"`
	Message  string      `json:"message"`
	Evidence string      `json:"evidence,omitempty"` // Source snippet showing the issue
}

// WarningCode identifies the type of warning.
type WarningCode string

const (
	// WarnDeferInLoop - defer statement inside a loop (resource accumulation).
	WarnDeferInLoop WarningCode = "defer_in_loop"
	// WarnIgnoredError - error return value explicitly ignored.
	WarnIgnoredError WarningCode = "ignored_error"
	// WarnLostCancel - context.WithCancel/Timeout without defer cancel().
	WarnLostCancel WarningCode = "lost_cancel"
)

// DocStatus indicates documentation freshness.
type DocStatus struct {
	Status  DocStatusCode `json:"status"`
	Reasons []string      `json:"reasons,omitempty"` // Why status was determined
}

// DocStatusCode represents documentation state.
type DocStatusCode string

const (
	// DocFresh - doc present and references match signature.
	DocFresh DocStatusCode = "fresh"
	// DocStale - doc references params/returns that no longer exist.
	DocStale DocStatusCode = "stale"
	// DocMissing - no doc comment present.
	DocMissing DocStatusCode = "missing"
)

// ExplainMode controls depth of explain analysis.
type ExplainMode string

const (
	// ExplainBrief - minimal analysis, fastest.
	ExplainBrief ExplainMode = "brief"
	// ExplainNormal - standard analysis (default).
	ExplainNormal ExplainMode = "normal"
	// ExplainDeep - full analysis including all callers.
	ExplainDeep ExplainMode = "deep"
)

// WarningsMode controls warning analysis depth.
type WarningsMode string

const (
	// WarningsNone - skip warning analysis.
	WarningsNone WarningsMode = "none"
	// WarningsFast - quick AST-only checks.
	WarningsFast WarningsMode = "fast"
	// WarningsFull - comprehensive analysis.
	WarningsFull WarningsMode = "full"
)
