package index

import (
	"go/ast"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Import represents a package import in a file
type Import struct {
	FilePath    string // Importing file
	PkgPath     string // Imported package path (e.g., "fmt", "github.com/foo/bar")
	Name        string // Local name if aliased, empty otherwise
	Line        int    // Line number of import statement
	Col         int    // Column of import statement
	ImporterPkg string // Package path of the importing file
}

// ExtractImports extracts all imports from loaded packages
func ExtractImports(result *LoadResult) ([]Import, error) {
	return ExtractImportsFiltered(result, nil)
}

// ExtractImportsFiltered extracts imports, optionally limited to specific files.
// When onlyFiles is non-nil, only imports from those files are extracted.
func ExtractImportsFiltered(result *LoadResult, onlyFiles map[string]bool) ([]Import, error) {
	var imports []Import

	for _, pkg := range result.Packages {
		for i, file := range pkg.Syntax {
			if i >= len(pkg.GoFiles) {
				continue
			}
			filePath := pkg.GoFiles[i]

			// Skip files not in the filter set (if filtering)
			if onlyFiles != nil && !onlyFiles[filePath] {
				continue
			}

			fileImports := extractFileImports(pkg, file, filePath, result)
			imports = append(imports, fileImports...)
		}
	}

	return imports, nil
}

func extractFileImports(pkg *packages.Package, file *ast.File, filePath string, result *LoadResult) []Import {
	var imports []Import

	for _, imp := range file.Imports {
		if imp.Path == nil {
			continue
		}

		// Get import path (strip quotes)
		pkgPath := strings.Trim(imp.Path.Value, "\"")

		// Get position
		pos := result.Fset.Position(imp.Pos())

		// Get alias name if present
		var name string
		if imp.Name != nil {
			name = imp.Name.Name
		}

		imports = append(imports, Import{
			FilePath:    filePath,
			PkgPath:     pkgPath,
			Name:        name,
			Line:        pos.Line,
			Col:         pos.Column,
			ImporterPkg: pkg.PkgPath,
		})
	}

	return imports
}

// FindImportedPackageFiles returns file paths for a given import path
// This is used to resolve "what files belong to this import"
func FindImportedPackageFiles(result *LoadResult, importPath string) []string {
	for _, pkg := range result.Packages {
		if pkg.PkgPath == importPath {
			return pkg.GoFiles
		}
	}
	return nil
}

// ResolveImportToRelative converts an import path to a relative path within the repo
// For local packages like "github.com/foo/bar/internal/pkg", returns "internal/pkg"
func ResolveImportToRelative(importPath, modulePath, repoRoot string) string {
	// If it's a local package, strip the module prefix
	if strings.HasPrefix(importPath, modulePath) {
		rel := strings.TrimPrefix(importPath, modulePath)
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			return "."
		}
		return rel
	}

	// External package - return as-is
	return importPath
}

// IsLocalImport checks if an import is from the same module
func IsLocalImport(importPath, modulePath string) bool {
	return strings.HasPrefix(importPath, modulePath)
}

// GetModulePath extracts the module path from a file path
// This is a heuristic based on the go.mod location
func GetModulePath(repoRoot string) string {
	// This would need to parse go.mod - for now return empty
	// The caller should get this from packages.Module.Path
	return ""
}

// GroupImportsByFile groups imports by their source file
func GroupImportsByFile(imports []Import) map[string][]Import {
	grouped := make(map[string][]Import)
	for _, imp := range imports {
		grouped[imp.FilePath] = append(grouped[imp.FilePath], imp)
	}
	return grouped
}

// GroupImportsByPackage groups imports by the imported package
func GroupImportsByPackage(imports []Import) map[string][]Import {
	grouped := make(map[string][]Import)
	for _, imp := range imports {
		grouped[imp.PkgPath] = append(grouped[imp.PkgPath], imp)
	}
	return grouped
}

// FilterLocalImports returns only imports from within the same module
func FilterLocalImports(imports []Import, modulePath string) []Import {
	var local []Import
	for _, imp := range imports {
		if IsLocalImport(imp.PkgPath, modulePath) {
			local = append(local, imp)
		}
	}
	return local
}

// NormalizeFilePath converts absolute paths to relative within repo
func NormalizeFilePath(absPath, repoRoot string) string {
	if rel, err := filepath.Rel(repoRoot, absPath); err == nil {
		return rel
	}
	return absPath
}
