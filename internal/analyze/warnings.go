// Package analyze provides static analysis for Go source code.
// Warning detectors are high-precision only: we prefer missing a warning over false alarms.
package analyze

import (
	"go/ast"
	"go/token"
	"strings"

	"github.com/dkoosis/snipe/internal/output"
)

// Analyzer performs static analysis on a function AST.
type Analyzer struct {
	fset    *token.FileSet
	src     []byte // Source for evidence extraction
	mode    output.WarningsMode
	funcPos token.Pos // Start of function being analyzed
}

// NewAnalyzer creates a new analyzer for warning detection.
func NewAnalyzer(fset *token.FileSet, src []byte, mode output.WarningsMode) *Analyzer {
	return &Analyzer{
		fset: fset,
		src:  src,
		mode: mode,
	}
}

// AnalyzeFunc runs all warning detectors on a function declaration.
// Returns only high-confidence warnings.
func (a *Analyzer) AnalyzeFunc(fn *ast.FuncDecl) []output.Warning {
	if a.mode == output.WarningsNone {
		return nil
	}

	if fn.Body == nil {
		return nil
	}

	a.funcPos = fn.Pos()

	var warnings []output.Warning

	// Run detectors based on mode
	warnings = append(warnings, a.detectDeferInLoop(fn.Body)...)
	warnings = append(warnings, a.detectIgnoredError(fn.Body)...)

	if a.mode == output.WarningsFull {
		warnings = append(warnings, a.detectLostCancel(fn.Body)...)
	}

	return warnings
}

// detectDeferInLoop finds defer statements inside for/range loops.
// High precision: defer in loop always accumulates resources until function returns.
func (a *Analyzer) detectDeferInLoop(body *ast.BlockStmt) []output.Warning {
	var warnings []output.Warning
	var loopDepth int

	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.ForStmt, *ast.RangeStmt:
			loopDepth++
		case *ast.DeferStmt:
			if loopDepth > 0 {
				pos := a.fset.Position(n.Pos())
				warnings = append(warnings, output.Warning{
					Code:     output.WarnDeferInLoop,
					Severity: "high",
					Line:     pos.Line,
					Message:  "defer inside loop accumulates resources until function returns",
					Evidence: a.extractLine(pos.Line),
				})
			}
		}
		return true
	})

	// Track loop exits
	ast.Inspect(body, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.ForStmt, *ast.RangeStmt:
			loopDepth--
		}
		return true
	})

	return warnings
}

// detectIgnoredError finds explicitly ignored error returns.
// High precision: only triggers on `_ = fn()` or `_, _ = fn()` patterns.
func (a *Analyzer) detectIgnoredError(body *ast.BlockStmt) []output.Warning {
	var warnings []output.Warning

	ast.Inspect(body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}

		// Look for blank identifier assignments
		if len(assign.Lhs) == 0 || len(assign.Rhs) != 1 {
			return true
		}

		// Check if last LHS is blank identifier (common error position)
		lastLhs := assign.Lhs[len(assign.Lhs)-1]
		ident, ok := lastLhs.(*ast.Ident)
		if !ok || ident.Name != "_" {
			return true
		}

		// Check if RHS is a function call
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}

		// Get function name for context
		funcName := extractCallName(call)
		if funcName == "" {
			return true
		}

		// Skip common non-error-returning patterns
		if isLikelyNonError(funcName) {
			return true
		}

		pos := a.fset.Position(assign.Pos())
		warnings = append(warnings, output.Warning{
			Code:     output.WarnIgnoredError,
			Severity: "medium",
			Line:     pos.Line,
			Message:  "error return from " + funcName + " explicitly ignored",
			Evidence: a.extractLine(pos.Line),
		})

		return true
	})

	return warnings
}

// detectLostCancel finds context.WithCancel/Timeout without proper cancel handling.
// High precision: tracks cancel variable and checks for defer cancel().
func (a *Analyzer) detectLostCancel(body *ast.BlockStmt) []output.Warning {
	var warnings []output.Warning

	// Track context creation calls and their cancel variables
	type cancelInfo struct {
		varName string
		line    int
		pos     token.Pos
	}
	var cancels []cancelInfo

	// First pass: find all WithCancel/WithTimeout/WithDeadline calls
	ast.Inspect(body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 {
			return true
		}

		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}

		funcName := extractCallName(call)
		if !isContextCreator(funcName) {
			return true
		}

		// Find the cancel variable (second return value)
		if len(assign.Lhs) < 2 {
			return true
		}

		cancelIdent, ok := assign.Lhs[1].(*ast.Ident)
		if !ok || cancelIdent.Name == "_" {
			// Explicitly ignored - that's a different issue
			return true
		}

		pos := a.fset.Position(assign.Pos())
		cancels = append(cancels, cancelInfo{
			varName: cancelIdent.Name,
			line:    pos.Line,
			pos:     assign.Pos(),
		})

		return true
	})

	if len(cancels) == 0 {
		return nil
	}

	// Second pass: check for defer cancel() calls
	cancelCalled := make(map[string]bool)

	ast.Inspect(body, func(n ast.Node) bool {
		defer_, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}

		call, ok := defer_.Call.Fun.(*ast.Ident)
		if ok {
			cancelCalled[call.Name] = true
		}

		// Also check for defer func() { cancel() }() pattern
		if funcLit, ok := defer_.Call.Fun.(*ast.FuncLit); ok {
			ast.Inspect(funcLit.Body, func(inner ast.Node) bool {
				if innerCall, ok := inner.(*ast.CallExpr); ok {
					if ident, ok := innerCall.Fun.(*ast.Ident); ok {
						cancelCalled[ident.Name] = true
					}
				}
				return true
			})
		}

		return true
	})

	// Report uncalled cancels
	for _, c := range cancels {
		if !cancelCalled[c.varName] {
			warnings = append(warnings, output.Warning{
				Code:     output.WarnLostCancel,
				Severity: "high",
				Line:     c.line,
				Message:  "context cancel function '" + c.varName + "' not deferred (may leak goroutines)",
				Evidence: a.extractLine(c.line),
			})
		}
	}

	return warnings
}

// extractCallName returns the function name from a call expression.
func extractCallName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		if ident, ok := fn.X.(*ast.Ident); ok {
			return ident.Name + "." + fn.Sel.Name
		}
		return fn.Sel.Name
	}
	return ""
}

// isLikelyNonError returns true for functions unlikely to return meaningful errors.
func isLikelyNonError(name string) bool {
	// Common functions that don't return errors or where ignoring is acceptable
	nonErrorFuncs := map[string]bool{
		"fmt.Println":  true,
		"fmt.Printf":   true,
		"fmt.Print":    true,
		"fmt.Fprintln": true,
		"fmt.Fprintf":  true,
		"fmt.Fprint":   true,
		"copy":         true,
		"append":       true,
		"len":          true,
		"cap":          true,
	}
	return nonErrorFuncs[name]
}

// isContextCreator returns true for context creation functions that return cancel funcs.
func isContextCreator(name string) bool {
	return name == "context.WithCancel" ||
		name == "context.WithTimeout" ||
		name == "context.WithDeadline" ||
		name == "WithCancel" ||
		name == "WithTimeout" ||
		name == "WithDeadline"
}

// extractLine extracts a single line from source for evidence.
func (a *Analyzer) extractLine(lineNum int) string {
	if a.src == nil {
		return ""
	}

	lines := strings.Split(string(a.src), "\n")
	if lineNum < 1 || lineNum > len(lines) {
		return ""
	}

	line := lines[lineNum-1]
	// Trim and cap length for readability
	line = strings.TrimSpace(line)
	if len(line) > 80 {
		line = line[:77] + "..."
	}
	return line
}
