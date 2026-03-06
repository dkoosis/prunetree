package index

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/packages"
)

// CallEdge represents a call from one function to another
type CallEdge struct {
	CallerID string
	CalleeID string
	FilePath string
	Line     int
	Col      int
}

// ExtractCallGraph extracts the static call graph from loaded packages.
// Uses an optimized approach: single AST pass with function stack tracking
// for O(1) enclosing function lookup per call.
func ExtractCallGraph(result *LoadResult, symbols []Symbol) ([]CallEdge, error) {
	return ExtractCallGraphFiltered(result, symbols, nil)
}

// ExtractCallGraphFiltered extracts the call graph, optionally limited to specific files.
// When onlyFiles is non-nil, only edges from those files are extracted.
func ExtractCallGraphFiltered(result *LoadResult, symbols []Symbol, onlyFiles map[string]bool) ([]CallEdge, error) {
	// Build symbol lookup by definition position (with fallback for chunked loading)
	symbolIndex := buildSymbolPosIndex(symbols)

	var edges []CallEdge

	for _, pkg := range result.Packages {
		if pkg.TypesInfo == nil {
			continue
		}

		for i, file := range pkg.Syntax {
			if i >= len(pkg.GoFiles) {
				continue
			}
			filePath := pkg.GoFiles[i]

			// Skip files not in the filter set (if filtering)
			if onlyFiles != nil && !onlyFiles[filePath] {
				continue
			}

			// Build import set for this package (for filtering interface dispatch targets).
			importedPkgs := make(map[string]bool)
			for impPath := range pkg.Imports {
				importedPkgs[impPath] = true
			}
			importedPkgs[pkg.PkgPath] = true // include own package

			// Extract call edges using AST walker with function stack tracking.
			// This avoids the separate buildEnclosingMap + findEnclosing passes.
			fileEdges := extractCallEdgesWithStack(pkg, file, filePath, result.Fset, symbolIndex, importedPkgs)
			edges = append(edges, fileEdges...)
		}
	}

	return edges, nil
}

// extractCallEdgesWithStack extracts all call edges from a file using a single AST pass.
// Maintains a function stack to track the current enclosing function for O(1) lookup.
func extractCallEdgesWithStack(pkg *packages.Package, file *ast.File, filePath string, fset *token.FileSet, symbolIndex *SymbolPosIndex, importedPkgs map[string]bool) []CallEdge {
	var edges []CallEdge
	var funcStack []string // Stack of enclosing function IDs

	var inspector func(n ast.Node) bool
	inspector = func(n ast.Node) bool {
		if n == nil {
			return true
		}

		switch node := n.(type) {
		case *ast.FuncDecl:
			if node.Body == nil || node.Name == nil {
				return true
			}
			// Compute function ID
			namePos := fset.Position(node.Name.Pos())
			kind := KindFunc
			if node.Recv != nil {
				kind = KindMethod
			}
			funcID := generateID(filePath, namePos.Line, namePos.Column, string(kind))

			// Push onto stack, traverse body, pop
			funcStack = append(funcStack, funcID)
			ast.Inspect(node.Body, inspector)
			funcStack = funcStack[:len(funcStack)-1]

			return false // Already handled children

		case *ast.CallExpr:
			// Extract call edges with current enclosing function
			newEdges := extractCallEdgeFromExpr(pkg, node, filePath, fset, symbolIndex, funcStack, importedPkgs)
			edges = append(edges, newEdges...)
		}

		return true
	}

	ast.Inspect(file, inspector)
	return edges
}

// extractCallEdgeFromExpr extracts call edges from a call expression.
// Returns multiple edges when an interface method call resolves to concrete implementations.
// Uses the function stack for O(1) enclosing function lookup.
func extractCallEdgeFromExpr(pkg *packages.Package, call *ast.CallExpr, filePath string, fset *token.FileSet, symbolIndex *SymbolPosIndex, funcStack []string, importedPkgs map[string]bool) []CallEdge {
	// Get current enclosing function (caller)
	if len(funcStack) == 0 {
		return nil // Call not inside a function (e.g., init expression)
	}
	callerID := funcStack[len(funcStack)-1]

	// Get the callee identifier
	var calleeIdent *ast.Ident

	switch fn := call.Fun.(type) {
	case *ast.Ident:
		// Direct function call: foo()
		calleeIdent = fn

	case *ast.SelectorExpr:
		// Method call or qualified call: x.Method() or pkg.Func()
		calleeIdent = fn.Sel

	default:
		// Function literal or other expression - can't track statically
		return nil
	}

	if calleeIdent == nil {
		return nil
	}

	// Look up the callee in TypesInfo.Uses
	obj := pkg.TypesInfo.Uses[calleeIdent]
	if obj == nil {
		return nil
	}

	// Get the definition position
	defPos := obj.Pos()
	if !defPos.IsValid() {
		return nil
	}

	defPosInfo := fset.Position(defPos)
	callPos := fset.Position(call.Pos())

	// Look up the callee symbol ID (with fallback for chunked loading)
	calleeID, ok := symbolIndex.Lookup(defPosInfo.Filename, defPosInfo.Line, defPosInfo.Column)
	if ok {
		return []CallEdge{{
			CallerID: callerID,
			CalleeID: calleeID,
			FilePath: filePath,
			Line:     callPos.Line,
			Col:      callPos.Column,
		}}
	}

	// Lookup failed — check if this is an interface method call.
	// When calling iface.Method(), TypesInfo.Uses resolves to the interface
	// definition, which has no symbol in our index. Fall back to matching
	// all concrete methods with the same name.
	fn, ok := obj.(*types.Func)
	if !ok {
		return nil
	}
	sig := fn.Type().(*types.Signature)
	recv := sig.Recv()
	if recv == nil {
		return nil
	}
	recvType := recv.Type()
	if ptr, ok := recvType.(*types.Pointer); ok {
		recvType = ptr.Elem()
	}
	named, ok := recvType.(*types.Named)
	if !ok {
		return nil
	}
	if _, isIface := named.Underlying().(*types.Interface); !isIface {
		return nil
	}

	// Interface method call — create edges to concrete methods with this name,
	// filtered to packages actually imported by the caller (eliminates false edges
	// from common names like Close, String, etc.).
	implIDs := symbolIndex.LookupMethodsByNameInPkgs(fn.Name(), importedPkgs)
	if len(implIDs) == 0 {
		return nil
	}

	edges := make([]CallEdge, 0, len(implIDs))
	for _, id := range implIDs {
		edges = append(edges, CallEdge{
			CallerID: callerID,
			CalleeID: id,
			FilePath: filePath,
			Line:     callPos.Line,
			Col:      callPos.Column,
		})
	}
	return edges
}
