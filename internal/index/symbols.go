package index

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"
)

// SymbolKind represents the kind of symbol
type SymbolKind string

const (
	KindFunc      SymbolKind = "func"
	KindMethod    SymbolKind = "method"
	KindType      SymbolKind = "type"
	KindInterface SymbolKind = "interface"
	KindStruct    SymbolKind = "struct"
	KindVar       SymbolKind = "var"
	KindConst     SymbolKind = "const"
	KindField     SymbolKind = "field"
)

// Symbol represents a code symbol (function, type, etc.)
type Symbol struct {
	ID        string
	Name      string
	Kind      SymbolKind
	FilePath  string
	PkgPath   string // Go package path (e.g., "github.com/user/repo/internal/handler")
	LineStart int    // Display range start (includes 'func' keyword for functions)
	ColStart  int
	LineEnd   int
	ColEnd    int
	NameLine  int // Identifier position (used for ID generation and call graph linkage)
	NameCol   int
	Signature string
	Doc       string
	Receiver  string // For methods: "(*T)" or "(T)"
}

// ExtractSymbols extracts all symbols from loaded packages
func ExtractSymbols(result *LoadResult) ([]Symbol, error) {
	var symbols []Symbol

	for _, pkg := range result.Packages {
		pkgPath := pkg.PkgPath
		for i, file := range pkg.Syntax {
			if i >= len(pkg.GoFiles) {
				continue
			}
			filePath := pkg.GoFiles[i]

			fileSymbols := extractFileSymbols(pkg, file, filePath, pkgPath, result.Fset)
			symbols = append(symbols, fileSymbols...)
		}
	}

	return symbols, nil
}

func extractFileSymbols(pkg *packages.Package, file *ast.File, filePath, pkgPath string, fset *token.FileSet) []Symbol {
	var symbols []Symbol

	ast.Inspect(file, func(n ast.Node) bool {
		switch decl := n.(type) {
		case *ast.FuncDecl:
			sym := extractFuncSymbol(pkg, decl, filePath, pkgPath, fset)
			if sym != nil {
				symbols = append(symbols, *sym)
			}

		case *ast.GenDecl:
			genSymbols := extractGenDeclSymbols(pkg, decl, filePath, pkgPath, fset)
			symbols = append(symbols, genSymbols...)
		}
		return true
	})

	return symbols
}

func extractFuncSymbol(pkg *packages.Package, decl *ast.FuncDecl, filePath, pkgPath string, fset *token.FileSet) *Symbol {
	if decl.Name == nil {
		return nil
	}

	// Use decl.Name.Pos() for the identifier position - this matches what
	// types.Object.Pos() returns, enabling call graph linkage via posKey
	namePos := fset.Position(decl.Name.Pos())
	declPos := fset.Position(decl.Pos())
	endPos := fset.Position(decl.End())

	kind := KindFunc
	var receiver string

	if decl.Recv != nil && len(decl.Recv.List) > 0 {
		kind = KindMethod
		receiver = formatReceiver(decl.Recv.List[0].Type)
	}

	// Prefer type-based signature with qualified types when available
	sig := formatFuncSignatureTyped(pkg, decl)
	doc := extractDoc(decl.Doc)

	return &Symbol{
		// ID uses identifier position for posKey matching with call graph
		ID:       generateID(filePath, namePos.Line, namePos.Column, string(kind)),
		Name:     decl.Name.Name,
		Kind:     kind,
		FilePath: filePath,
		PkgPath:  pkgPath,
		// Range uses declaration start for user display (includes 'func' keyword)
		LineStart: declPos.Line,
		ColStart:  declPos.Column,
		LineEnd:   endPos.Line,
		ColEnd:    endPos.Column,
		// Identifier position for call graph linkage
		NameLine:  namePos.Line,
		NameCol:   namePos.Column,
		Signature: sig,
		Doc:       doc,
		Receiver:  receiver,
	}
}

func extractGenDeclSymbols(pkg *packages.Package, decl *ast.GenDecl, filePath, pkgPath string, fset *token.FileSet) []Symbol {
	var symbols []Symbol

	for _, spec := range decl.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			sym := extractTypeSymbol(pkg, s, decl, filePath, pkgPath, fset)
			if sym != nil {
				symbols = append(symbols, *sym)
			}

		case *ast.ValueSpec:
			valSymbols := extractValueSymbols(pkg, s, decl, filePath, pkgPath, fset)
			symbols = append(symbols, valSymbols...)
		}
	}

	return symbols
}

func extractTypeSymbol(pkg *packages.Package, spec *ast.TypeSpec, decl *ast.GenDecl, filePath, pkgPath string, fset *token.FileSet) *Symbol {
	if spec.Name == nil {
		return nil
	}

	pos := fset.Position(spec.Pos())
	endPos := fset.Position(spec.End())

	kind := KindType
	switch spec.Type.(type) {
	case *ast.InterfaceType:
		kind = KindInterface
	case *ast.StructType:
		kind = KindStruct
	}

	// Get type info if available
	var sig string
	if pkg.TypesInfo != nil {
		if obj := pkg.TypesInfo.Defs[spec.Name]; obj != nil {
			sig = obj.Type().String()
		}
	}

	doc := extractDoc(decl.Doc)
	if doc == "" {
		doc = extractDoc(spec.Doc)
	}

	return &Symbol{
		ID:        generateID(filePath, pos.Line, pos.Column, string(kind)),
		Name:      spec.Name.Name,
		Kind:      kind,
		FilePath:  filePath,
		PkgPath:   pkgPath,
		LineStart: pos.Line,
		ColStart:  pos.Column,
		LineEnd:   endPos.Line,
		ColEnd:    endPos.Column,
		NameLine:  pos.Line,
		NameCol:   pos.Column,
		Signature: sig,
		Doc:       doc,
	}
}

func extractValueSymbols(pkg *packages.Package, spec *ast.ValueSpec, decl *ast.GenDecl, filePath, pkgPath string, fset *token.FileSet) []Symbol {
	var symbols []Symbol

	kind := KindVar
	if decl.Tok == token.CONST {
		kind = KindConst
	}

	doc := extractDoc(decl.Doc)
	if doc == "" {
		doc = extractDoc(spec.Doc)
	}

	for _, name := range spec.Names {
		if name.Name == "_" {
			continue // Skip blank identifier
		}

		pos := fset.Position(name.Pos())
		endPos := fset.Position(name.End())

		var sig string
		if pkg.TypesInfo != nil {
			if obj := pkg.TypesInfo.Defs[name]; obj != nil {
				sig = obj.Type().String()
			}
		}

		symbols = append(symbols, Symbol{
			ID:        generateID(filePath, pos.Line, pos.Column, string(kind)),
			Name:      name.Name,
			Kind:      kind,
			FilePath:  filePath,
			PkgPath:   pkgPath,
			LineStart: pos.Line,
			ColStart:  pos.Column,
			LineEnd:   endPos.Line,
			ColEnd:    endPos.Column,
			NameLine:  pos.Line,
			NameCol:   pos.Column,
			Signature: sig,
			Doc:       doc,
		})
	}

	return symbols
}

func formatReceiver(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		if ident, ok := t.X.(*ast.Ident); ok {
			return "(*" + ident.Name + ")"
		}
	case *ast.Ident:
		return "(" + t.Name + ")"
	}
	return ""
}

// formatFuncSignatureTyped formats a function signature using type information
// when available, falling back to AST-based formatting otherwise.
// Type-based formatting provides qualified package names for external types.
func formatFuncSignatureTyped(pkg *packages.Package, decl *ast.FuncDecl) string {
	// Try to get type information for qualified signatures
	if pkg.TypesInfo != nil {
		if obj := pkg.TypesInfo.Defs[decl.Name]; obj != nil {
			if fn, ok := obj.(*types.Func); ok {
				return formatTypedSignature(fn, pkg.PkgPath)
			}
		}
	}
	// Fallback to AST-based formatting
	return formatFuncSignature(decl)
}

// formatTypedSignature formats a function signature using types.TypeString
// with a qualifier that shows short package names for external types.
func formatTypedSignature(fn *types.Func, currentPkg string) string {
	sig := fn.Type().(*types.Signature)
	var b strings.Builder
	b.WriteString("func ")

	// Format receiver if present
	if recv := sig.Recv(); recv != nil {
		b.WriteString("(")
		if recv.Name() != "" {
			b.WriteString(recv.Name())
			b.WriteString(" ")
		}
		b.WriteString(formatType(recv.Type(), currentPkg))
		b.WriteString(") ")
	}

	b.WriteString(fn.Name())

	// Format type parameters (generics) if present
	if tp := sig.TypeParams(); tp != nil && tp.Len() > 0 {
		b.WriteString("[")
		for i := 0; i < tp.Len(); i++ {
			if i > 0 {
				b.WriteString(", ")
			}
			tparam := tp.At(i)
			b.WriteString(tparam.Obj().Name())
			constraint := tparam.Constraint()
			// Only show constraint if not the default 'any'
			if constraint != nil {
				cstr := formatType(constraint, currentPkg)
				if cstr != "any" && cstr != "interface{}" {
					b.WriteString(" ")
					b.WriteString(cstr)
				}
			}
		}
		b.WriteString("]")
	}

	// Format parameters
	b.WriteString(formatTuple(sig.Params(), currentPkg, sig.Variadic()))

	// Format results
	results := sig.Results()
	if results.Len() > 0 {
		b.WriteString(" ")
		if results.Len() == 1 && results.At(0).Name() == "" {
			// Single unnamed result: no parens
			b.WriteString(formatType(results.At(0).Type(), currentPkg))
		} else {
			b.WriteString(formatTuple(results, currentPkg, false))
		}
	}

	return b.String()
}

// formatTuple formats a parameter or result tuple with qualified types.
func formatTuple(tuple *types.Tuple, currentPkg string, variadic bool) string {
	var parts []string
	for i := 0; i < tuple.Len(); i++ {
		v := tuple.At(i)
		var part string
		if v.Name() != "" {
			part = v.Name() + " "
		}
		// Handle variadic parameter (last param with ...)
		if variadic && i == tuple.Len()-1 {
			if slice, ok := v.Type().(*types.Slice); ok {
				part += "..." + formatType(slice.Elem(), currentPkg)
			} else {
				part += formatType(v.Type(), currentPkg)
			}
		} else {
			part += formatType(v.Type(), currentPkg)
		}
		parts = append(parts, part)
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// formatType formats a type with a qualifier that uses short package names.
// Types from the current package are unqualified, external types show "pkg.Type".
func formatType(t types.Type, currentPkg string) string {
	qualifier := func(pkg *types.Package) string {
		if pkg == nil || pkg.Path() == currentPkg {
			return "" // No qualifier for current package
		}
		return pkg.Name() // Use short name, e.g., "http" not "net/http"
	}
	return types.TypeString(t, qualifier)
}

func formatFuncSignature(decl *ast.FuncDecl) string {
	var b strings.Builder
	b.WriteString("func ")

	if decl.Recv != nil && len(decl.Recv.List) > 0 {
		b.WriteString(formatReceiver(decl.Recv.List[0].Type))
		b.WriteString(" ")
	}

	b.WriteString(decl.Name.Name)
	b.WriteString(formatFieldList(decl.Type.Params))

	if decl.Type.Results != nil {
		results := formatFieldList(decl.Type.Results)
		if strings.Contains(results, ",") || strings.Contains(results, " ") {
			b.WriteString(" ")
			b.WriteString(results)
		} else if results != "()" {
			b.WriteString(" ")
			b.WriteString(strings.Trim(results, "()"))
		}
	}

	return b.String()
}

func formatFieldList(fl *ast.FieldList) string {
	if fl == nil {
		return "()"
	}

	var parts []string
	for _, field := range fl.List {
		typeStr := types.ExprString(field.Type)
		if len(field.Names) == 0 {
			parts = append(parts, typeStr)
		} else {
			for _, name := range field.Names {
				parts = append(parts, name.Name+" "+typeStr)
			}
		}
	}

	return "(" + strings.Join(parts, ", ") + ")"
}

func extractDoc(doc *ast.CommentGroup) string {
	if doc == nil {
		return ""
	}
	return strings.TrimSpace(doc.Text())
}

// PackageDoc holds the extracted package-level doc comment for a Go package.
type PackageDoc struct {
	PkgPath string
	Doc     string
}

// ExtractPackageDocs extracts package-level doc comments from loaded packages.
// For each package, it prioritizes doc.go files, then falls back to the first
// file with a non-empty package doc comment.
func ExtractPackageDocs(result *LoadResult) []PackageDoc {
	seen := make(map[string]bool)
	var docs []PackageDoc

	for _, pkg := range result.Packages {
		if pkg.PkgPath == "" || seen[pkg.PkgPath] {
			continue
		}

		doc := extractPackageDoc(pkg)
		if doc == "" {
			continue
		}
		seen[pkg.PkgPath] = true
		docs = append(docs, PackageDoc{
			PkgPath: pkg.PkgPath,
			Doc:     doc,
		})
	}

	return docs
}

// extractPackageDoc returns the best package-level doc comment for a package.
// Prioritizes doc.go, then falls back to first file with a non-empty File.Doc.
func extractPackageDoc(pkg *packages.Package) string {
	var fallback string

	for i, file := range pkg.Syntax {
		if i >= len(pkg.GoFiles) {
			continue
		}
		if file.Doc == nil {
			continue
		}
		doc := strings.TrimSpace(file.Doc.Text())
		if doc == "" {
			continue
		}

		filePath := pkg.GoFiles[i]
		if filepath.Base(filePath) == "doc.go" {
			return doc
		}
		if fallback == "" {
			fallback = doc
		}
	}

	return fallback
}

func generateID(filePath string, line, col int, kind string) string {
	data := fmt.Sprintf("%s:%d:%d:%s", filePath, line, col, kind)
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:8]) // Use first 8 bytes = 16 hex chars
}
