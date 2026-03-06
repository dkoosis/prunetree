package index

import (
	"fmt"
	"go/ast"
	"go/token"
	"sort"
	"strings"

	"github.com/dkoosis/snipe/internal/util"
)

// Ref represents a reference to a symbol
type Ref struct {
	ID          string
	SymbolID    string // ID of the referenced symbol
	FilePath    string
	Line        int
	Col         int
	EnclosingID string // ID of the enclosing function/method
	Snippet     string // The line of code
}

// ExtractRefs extracts all references from loaded packages.
// For better performance during indexing, use ExtractRefsWithCache.
func ExtractRefs(result *LoadResult, symbols []Symbol) ([]Ref, error) {
	return ExtractRefsFiltered(result, symbols, nil, nil)
}

// ExtractRefsWithCache extracts all references from loaded packages.
// If cache is provided, file contents are cached to avoid repeated disk reads.
// The cache should be cleared after indexing completes to free memory.
func ExtractRefsWithCache(result *LoadResult, symbols []Symbol, cache *util.FileCache) ([]Ref, error) {
	return ExtractRefsFiltered(result, symbols, cache, nil)
}

// ExtractRefsFiltered extracts references, optionally limited to specific files.
// When onlyFiles is non-nil, only refs in those files are extracted.
// Symbols are still indexed from all files for cross-file ref resolution.
func ExtractRefsFiltered(result *LoadResult, symbols []Symbol, cache *util.FileCache, onlyFiles map[string]bool) ([]Ref, error) {
	// Build symbol lookup by definition position
	symbolByPos := buildSymbolPosIndex(symbols)

	var refs []Ref

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

			// Load file content for snippets (with optional caching)
			var lines []string
			var err error
			if cache != nil {
				lines, err = cache.LoadLines(filePath)
			} else {
				lines, err = util.LoadFileLines(filePath)
			}
			if err != nil {
				continue // Skip files we can't read
			}

			// Build enclosing function map for this file
			enclosingMap := buildEnclosingMap(file, filePath, result.Fset)

			// Extract references from Uses map
			for ident, obj := range pkg.TypesInfo.Uses {
				if obj == nil {
					continue
				}

				// Get the definition position
				defPos := obj.Pos()
				if !defPos.IsValid() {
					continue
				}

				defPosInfo := result.Fset.Position(defPos)

				// Look up the symbol ID (with fallback for chunked loading)
				symbolID, ok := symbolByPos.Lookup(defPosInfo.Filename, defPosInfo.Line, defPosInfo.Column)
				if !ok {
					continue // Reference to symbol not in our index (e.g., stdlib)
				}

				// Get reference position
				refPos := result.Fset.Position(ident.Pos())
				if refPos.Filename != filePath {
					continue // Skip if not in current file
				}

				// Get enclosing function
				enclosingID := findEnclosing(ident.Pos(), enclosingMap)

				// Get snippet
				snippet := ""
				if refPos.Line > 0 && refPos.Line <= len(lines) {
					snippet = strings.TrimSpace(lines[refPos.Line-1])
				}

				ref := Ref{
					ID:          generateID(filePath, refPos.Line, refPos.Column, "ref"),
					SymbolID:    symbolID,
					FilePath:    filePath,
					Line:        refPos.Line,
					Col:         refPos.Column,
					EnclosingID: enclosingID,
					Snippet:     snippet,
				}
				refs = append(refs, ref)
			}
		}
	}

	return refs, nil
}

// SymbolPosIndex provides position-based symbol lookup with fallback for chunked loading.
// When packages are loaded in chunks, obj.Pos() may return declaration start (col 1)
// instead of identifier position. The fallback index handles this case.
type SymbolPosIndex struct {
	exact         map[string]string   // file:line:col -> symbol ID
	fallback      map[string]string   // file:line -> symbol ID (for col 1 lookups)
	methodsByName map[string][]string // method name -> [symbol IDs] for interface dispatch
	idToPkg       map[string]string   // symbol ID -> package path (for import-set filtering)
}

// buildSymbolPosIndex creates a position index with exact and fallback lookups.
func buildSymbolPosIndex(symbols []Symbol) *SymbolPosIndex {
	idx := &SymbolPosIndex{
		exact:         make(map[string]string),
		fallback:      make(map[string]string),
		methodsByName: make(map[string][]string),
		idToPkg:       make(map[string]string),
	}
	for _, sym := range symbols {
		// Exact key using identifier position
		exactKey := posKey(sym.FilePath, sym.NameLine, sym.NameCol)
		idx.exact[exactKey] = sym.ID

		// Track symbol ID → package path for import-set filtering
		idx.idToPkg[sym.ID] = sym.PkgPath

		// Fallback key for declaration start (col 1) - only for funcs/methods
		// which are the primary callees we care about
		if sym.Kind == KindFunc || sym.Kind == KindMethod {
			fallbackKey := posKey(sym.FilePath, sym.NameLine, 1)
			// Don't overwrite if multiple symbols on same line
			if _, exists := idx.fallback[fallbackKey]; !exists {
				idx.fallback[fallbackKey] = sym.ID
			}
		}

		// Index methods by name for interface dispatch resolution
		if sym.Kind == KindMethod {
			idx.methodsByName[sym.Name] = append(idx.methodsByName[sym.Name], sym.ID)
		}
	}
	return idx
}

// Lookup finds a symbol ID by position, trying exact match first then fallback.
func (idx *SymbolPosIndex) Lookup(file string, line, col int) (string, bool) {
	key := posKey(file, line, col)
	if id, ok := idx.exact[key]; ok {
		return id, true
	}
	// Fallback for chunked loading where col may be 1 (declaration start)
	if col == 1 {
		fallbackKey := posKey(file, line, 1)
		if id, ok := idx.fallback[fallbackKey]; ok {
			return id, true
		}
	}
	return "", false
}

// LookupMethodsByNameInPkgs returns method symbol IDs filtered to only those
// whose package is in the provided import set. Eliminates false-positive
// cross-package edges from common method names (e.g., Close, String).
func (idx *SymbolPosIndex) LookupMethodsByNameInPkgs(name string, importedPkgs map[string]bool) []string {
	all := idx.methodsByName[name]
	if len(importedPkgs) == 0 {
		return all
	}
	var filtered []string
	for _, id := range all {
		if pkg, ok := idx.idToPkg[id]; ok && importedPkgs[pkg] {
			filtered = append(filtered, id)
		}
	}
	return filtered
}

func posKey(file string, line, col int) string {
	return fmt.Sprintf("%s:%d:%d", file, line, col)
}

// enclosingFunc tracks function/method ranges for finding enclosing scope
type enclosingFunc struct {
	id    string
	start token.Pos
	end   token.Pos
}

func buildEnclosingMap(file *ast.File, filePath string, fset *token.FileSet) []enclosingFunc {
	var funcs []enclosingFunc

	ast.Inspect(file, func(n ast.Node) bool {
		if fn, ok := n.(*ast.FuncDecl); ok && fn.Body != nil && fn.Name != nil {
			// Use fn.Name.Pos() to match symbol ID generation in symbols.go
			namePos := fset.Position(fn.Name.Pos())
			kind := KindFunc
			if fn.Recv != nil {
				kind = KindMethod
			}
			funcs = append(funcs, enclosingFunc{
				id:    generateID(filePath, namePos.Line, namePos.Column, string(kind)),
				start: fn.Body.Lbrace,
				end:   fn.Body.Rbrace,
			})
		}
		return true
	})

	// Sort by start position for binary search
	sort.Slice(funcs, func(i, j int) bool {
		return funcs[i].start < funcs[j].start
	})

	return funcs
}

// findEnclosing uses binary search to find the enclosing function for a position.
// O(log N) instead of O(N) linear scan.
func findEnclosing(pos token.Pos, funcs []enclosingFunc) string {
	if len(funcs) == 0 {
		return ""
	}

	// Binary search for rightmost function where start <= pos
	idx := sort.Search(len(funcs), func(i int) bool {
		return funcs[i].start > pos
	})

	// Check functions from idx-1 down to 0 (nested functions possible)
	for i := idx - 1; i >= 0; i-- {
		if pos >= funcs[i].start && pos <= funcs[i].end {
			return funcs[i].id
		}
		// If this function ends before pos, earlier functions won't contain pos either
		// (unless they're nested, but Go doesn't have nested named functions)
		if funcs[i].end < pos {
			break
		}
	}

	return ""
}
