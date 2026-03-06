package query

import (
	"database/sql"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"github.com/dkoosis/snipe/internal/analyze"
	"github.com/dkoosis/snipe/internal/output"
)

// ExplainOptions controls explain analysis depth.
type ExplainOptions struct {
	Mode         output.ExplainMode
	WarningsMode output.WarningsMode
	MaxCallers   int // Cap on callers to analyze (0 = use mode default)
}

// DefaultExplainOptions returns sensible defaults.
func DefaultExplainOptions() ExplainOptions {
	return ExplainOptions{
		Mode:         output.ExplainNormal,
		WarningsMode: output.WarningsFast,
		MaxCallers:   10,
	}
}

// Explain generates a structured explanation for a symbol.
func Explain(db *sql.DB, symbolID string, opts ExplainOptions) (*output.ExplainResult, error) {
	// Look up the symbol
	sym, err := LookupByID(db, symbolID)
	if err != nil {
		return nil, fmt.Errorf("lookup symbol: %w", err)
	}
	if sym == nil {
		return nil, fmt.Errorf("symbol not found: %s", symbolID)
	}

	// Only explain funcs and methods for now
	if sym.Kind != "func" && sym.Kind != "method" {
		return nil, fmt.Errorf("explain only supports func/method, got %s", sym.Kind)
	}

	result := &output.ExplainResult{
		Symbol:    sym.Name,
		File:      fmt.Sprintf("%s:%d", sym.FilePathRel, sym.LineStart),
		Kind:      sym.Kind,
		Signature: sym.Signature.String,
	}

	// Parse the source file for AST analysis
	fset := token.NewFileSet()
	src, err := os.ReadFile(sym.FilePath)
	if err != nil {
		return nil, fmt.Errorf("read source: %w", err)
	}

	f, err := parser.ParseFile(fset, sym.FilePath, src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse source: %w", err)
	}

	// Find the function declaration
	var funcDecl *ast.FuncDecl
	ast.Inspect(f, func(n ast.Node) bool {
		if fn, ok := n.(*ast.FuncDecl); ok {
			pos := fset.Position(fn.Pos())
			if pos.Line == sym.LineStart && fn.Name.Name == sym.Name {
				funcDecl = fn
				return false
			}
		}
		return true
	})

	if funcDecl == nil {
		return nil, fmt.Errorf("function not found in AST at line %d", sym.LineStart)
	}

	// Extract purpose from doc or generate template
	result.Purpose, result.PurposeSource = analyze.ExtractPurpose(funcDecl, sym.Doc.String)

	// Check doc status
	result.DocStatus = analyze.CheckDocStatus(funcDecl, sym.Doc.String)

	// Run warning analysis based on mode
	if opts.WarningsMode != output.WarningsNone {
		analyzer := analyze.NewAnalyzer(fset, src, opts.WarningsMode)
		result.Warnings = analyzer.AnalyzeFunc(funcDecl)
	}

	// Extract mechanism (callees with action mapping)
	if opts.Mode != output.ExplainBrief {
		mechanism, keyDeps, err := extractMechanism(db, sym, funcDecl, fset, opts.Mode)
		if err == nil {
			result.Mechanism = mechanism
			result.KeyDeps = keyDeps
		}
	}

	// Get caller context
	callerLimit := opts.MaxCallers
	if callerLimit == 0 {
		switch opts.Mode {
		case output.ExplainBrief:
			callerLimit = 3
		case output.ExplainNormal:
			callerLimit = 10
		case output.ExplainDeep:
			callerLimit = 50
		}
	}

	callerCtx, err := buildCallerContext(db, symbolID, callerLimit)
	if err == nil && callerCtx != nil {
		result.CallerContext = callerCtx
	}

	return result, nil
}

// extractMechanism builds mechanism steps from callees.
func extractMechanism(db *sql.DB, sym *SymbolRow, _ *ast.FuncDecl, _ *token.FileSet, mode output.ExplainMode) ([]output.MechanismStep, []string, error) {
	// Get callees from the call graph
	limit := 20
	if mode == output.ExplainDeep {
		limit = 50
	}

	callees, err := FindCallees(db, sym.ID, limit, 0)
	if err != nil {
		return nil, nil, err
	}

	var steps []output.MechanismStep
	depSet := make(map[string]bool)

	for _, callee := range callees {
		action := inferAction(callee.CalleeName)
		step := output.MechanismStep{
			Action: action,
			Target: callee.CalleeName,
			Line:   callee.CallLine,
		}

		// Add note for important patterns
		if note := inferNote(callee.CalleeName, callee.CalleeKind); note != "" {
			step.Note = note
		}

		steps = append(steps, step)

		// Track key dependencies (types, not calls)
		if callee.CalleeSignature.Valid {
			extractDeps(callee.CalleeSignature.String, depSet)
		}
	}

	// Convert dep set to slice
	var keyDeps []string
	for dep := range depSet {
		keyDeps = append(keyDeps, dep)
	}

	return steps, keyDeps, nil
}

// inferAction maps callee names to action verbs.
func inferAction(name string) string {
	// Check exact matches first
	actionMap := map[string]string{
		"Open":      "opens",
		"Close":     "closes",
		"Read":      "reads",
		"Write":     "writes",
		"Get":       "retrieves",
		"Set":       "updates",
		"Save":      "persists",
		"Store":     "persists",
		"Load":      "loads",
		"Parse":     "parses",
		"Format":    "formats",
		"Validate":  "validates",
		"Check":     "validates",
		"Execute":   "executes",
		"Run":       "executes",
		"Start":     "starts",
		"Stop":      "stops",
		"Init":      "initializes",
		"Create":    "creates",
		"Delete":    "deletes",
		"Remove":    "removes",
		"Add":       "adds",
		"Append":    "appends",
		"Insert":    "inserts",
		"Update":    "updates",
		"Find":      "finds",
		"Search":    "searches",
		"Query":     "queries",
		"Fetch":     "fetches",
		"Send":      "sends",
		"Receive":   "receives",
		"Encode":    "encodes",
		"Decode":    "decodes",
		"Marshal":   "serializes",
		"Unmarshal": "deserializes",
	}

	if action, ok := actionMap[name]; ok {
		return action
	}

	// Check prefixes
	prefixes := map[string]string{
		"Is":     "validates",
		"Has":    "validates",
		"Check":  "validates",
		"New":    "creates",
		"Make":   "creates",
		"Get":    "retrieves",
		"Set":    "updates",
		"Save":   "persists",
		"Load":   "loads",
		"Parse":  "parses",
		"Format": "formats",
	}

	for prefix, action := range prefixes {
		if strings.HasPrefix(name, prefix) {
			return action
		}
	}

	return "calls"
}

// inferNote adds context notes for important patterns.
func inferNote(name, _ string) string {
	// Context-related
	if strings.Contains(strings.ToLower(name), "context") {
		return "context propagation"
	}

	// Error handling
	if strings.Contains(strings.ToLower(name), "error") {
		return "error handling"
	}

	// Concurrency
	if strings.HasPrefix(name, "Lock") || strings.HasPrefix(name, "Unlock") {
		return "synchronization"
	}

	// Defer/cleanup patterns
	if strings.HasPrefix(name, "Close") || strings.HasPrefix(name, "Release") {
		return "resource cleanup"
	}

	return ""
}

// extractDeps extracts type names from a signature for key_deps.
func extractDeps(sig string, deps map[string]bool) {
	// Simple extraction: find capitalized identifiers that look like types
	// This is a heuristic, not exhaustive
	words := strings.FieldsFunc(sig, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.') //nolint:staticcheck // readable as-is
	})

	for _, word := range words {
		// Skip keywords and common types
		if isKeyword(word) || isBuiltin(word) {
			continue
		}
		// Only include if starts with uppercase (exported type)
		if len(word) > 0 && word[0] >= 'A' && word[0] <= 'Z' {
			deps[word] = true
		}
	}
}

func isKeyword(s string) bool {
	keywords := map[string]bool{
		"func": true, "return": true, "if": true, "else": true, "for": true,
		"range": true, "switch": true, "case": true, "default": true,
		"type": true, "struct": true, "interface": true, "map": true,
		"chan": true, "go": true, "defer": true, "select": true,
	}
	return keywords[s]
}

func isBuiltin(s string) bool {
	builtins := map[string]bool{
		"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
		"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
		"float32": true, "float64": true, "complex64": true, "complex128": true,
		"string": true, "bool": true, "byte": true, "rune": true, "error": true,
		"any": true, "comparable": true,
	}
	return builtins[s]
}

// buildCallerContext summarizes caller patterns.
func buildCallerContext(db *sql.DB, symbolID string, limit int) (*output.CallerContext, error) {
	// Get total count
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM call_graph WHERE callee_id = ?`, symbolID).Scan(&count)
	if err != nil {
		return nil, err
	}

	if count == 0 {
		return nil, nil
	}

	// Get top callers
	callers, err := FindCallers(db, symbolID, limit, 0)
	if err != nil {
		return nil, err
	}

	ctx := &output.CallerContext{
		Count: count,
	}

	// Extract caller names
	for _, c := range callers {
		ctx.TopCallers = append(ctx.TopCallers, c.CallerName)
	}

	// Try to detect patterns
	ctx.Pattern = detectCallerPattern(callers)

	return ctx, nil
}

// detectCallerPattern tries to identify a pattern in callers.
func detectCallerPattern(callers []CallRow) string {
	if len(callers) == 0 {
		return ""
	}

	// Check if all from same directory
	dirs := make(map[string]int)
	for _, c := range callers {
		dir := filepath.Dir(c.CallerFileRel)
		dirs[dir]++
	}

	if len(dirs) == 1 {
		for dir, count := range dirs {
			return fmt.Sprintf("all from %s (%d callers)", dir, count)
		}
	}

	// Check for naming patterns
	prefixCounts := make(map[string]int)
	for _, c := range callers {
		// Extract prefix (e.g., "run" from "runDef", "runRefs")
		name := c.CallerName
		if len(name) > 3 {
			for i := 3; i <= len(name) && i <= 10; i++ {
				prefix := strings.ToLower(name[:i])
				prefixCounts[prefix]++
			}
		}
	}

	// Find dominant prefix
	for prefix, count := range prefixCounts {
		if count >= len(callers)/2 && count >= 2 {
			return fmt.Sprintf("mostly %s* functions", prefix)
		}
	}

	return ""
}
