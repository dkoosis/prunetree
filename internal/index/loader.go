package index

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"
)

// LoadConfig configures how packages are loaded
type LoadConfig struct {
	// Context for cancellation support (optional, defaults to context.Background)
	Context context.Context
	// Dir is the directory to load packages from
	Dir string
	// Patterns are the package patterns to load (e.g., "./...")
	Patterns []string
	// Exclude patterns to skip (e.g., "vendor", "testdata")
	Exclude []string
	// Tests includes test files
	Tests bool
	// ChunkSize is the number of packages to load at a time (0 = no chunking)
	// Default: 50 for large codebases to prevent OOM
	ChunkSize int
	// OnProgress is called after each chunk with (loaded, total) counts
	OnProgress func(loaded, total int)
}

// DefaultChunkSize is the default number of packages to load per batch.
const DefaultChunkSize = 50

// LoadResult contains the loaded packages and metadata
type LoadResult struct {
	Packages []*packages.Package
	Fset     *token.FileSet
	Errors   []error
}

// DefaultExclude returns the default exclude patterns
func DefaultExclude() []string {
	return []string{"vendor", "node_modules", "testdata", ".git"}
}

// Load loads Go packages from the specified directory.
// If ChunkSize > 0, packages are loaded in batches to prevent OOM on large repos.
// Supports cancellation via cfg.Context.
func Load(cfg LoadConfig) (*LoadResult, error) {
	ctx := cfg.Context
	if ctx == nil {
		ctx = context.Background()
	}

	if cfg.Dir == "" {
		cfg.Dir = "."
	}
	if len(cfg.Patterns) == 0 {
		cfg.Patterns = []string{"./..."}
	}
	if cfg.Exclude == nil {
		cfg.Exclude = DefaultExclude()
	}
	if cfg.ChunkSize == 0 {
		cfg.ChunkSize = DefaultChunkSize
	}

	// Check for cancellation before starting
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("cancelled: %w", err)
	}

	// Resolve absolute path
	absDir, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("resolve directory: %w", err)
	}

	fset := token.NewFileSet()

	// First pass: get package names only (lightweight)
	nameCfg := &packages.Config{
		Mode:    packages.NeedName,
		Dir:     absDir,
		Tests:   cfg.Tests,
		Context: ctx,
	}

	nameOnlyPkgs, err := packages.Load(nameCfg, cfg.Patterns...)
	if err != nil {
		return nil, fmt.Errorf("list packages: %w", err)
	}

	// Check for cancellation after listing
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("cancelled: %w", err)
	}

	// Filter and get unique import paths
	filteredPkgs := filterPackages(nameOnlyPkgs, cfg.Exclude)
	pkgPaths := make([]string, 0, len(filteredPkgs))
	seen := make(map[string]bool)
	for _, pkg := range filteredPkgs {
		if pkg.PkgPath != "" && !seen[pkg.PkgPath] {
			seen[pkg.PkgPath] = true
			pkgPaths = append(pkgPaths, pkg.PkgPath)
		}
	}

	// If small enough, load all at once
	if len(pkgPaths) <= cfg.ChunkSize {
		return loadPackagesFull(ctx, absDir, fset, pkgPaths, cfg.Tests)
	}

	// Chunked loading for large codebases
	return loadPackagesChunked(ctx, absDir, fset, pkgPaths, cfg.ChunkSize, cfg.Tests, cfg.OnProgress)
}

// loadPackagesFull loads all packages in a single call.
func loadPackagesFull(ctx context.Context, dir string, fset *token.FileSet, pkgPaths []string, tests bool) (*LoadResult, error) {
	loadCfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo |
			packages.NeedImports,
		Dir:     dir,
		Fset:    fset,
		Tests:   tests,
		Context: ctx,
	}

	pkgs, err := packages.Load(loadCfg, pkgPaths...)
	if err != nil {
		return nil, fmt.Errorf("load packages: %w", err)
	}

	var errs []error
	for _, pkg := range pkgs {
		for _, e := range pkg.Errors {
			errs = append(errs, e)
		}
	}

	return &LoadResult{
		Packages: pkgs,
		Fset:     fset,
		Errors:   errs,
	}, nil
}

// loadPackagesChunked loads packages in batches to control memory usage.
// Each batch is loaded independently, allowing GC to reclaim memory between batches.
// Checks for context cancellation between batches.
func loadPackagesChunked(ctx context.Context, dir string, fset *token.FileSet, pkgPaths []string, chunkSize int, tests bool, onProgress func(int, int)) (*LoadResult, error) {
	var allPkgs []*packages.Package
	var allErrs []error
	total := len(pkgPaths)

	for i := 0; i < len(pkgPaths); i += chunkSize {
		// Check for cancellation between batches
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("cancelled after %d/%d packages: %w", i, total, err)
		}

		end := i + chunkSize
		if end > len(pkgPaths) {
			end = len(pkgPaths)
		}
		chunk := pkgPaths[i:end]

		loadCfg := &packages.Config{
			Mode: packages.NeedName |
				packages.NeedFiles |
				packages.NeedSyntax |
				packages.NeedTypes |
				packages.NeedTypesInfo |
				packages.NeedImports,
			Dir:     dir,
			Fset:    fset,
			Tests:   tests,
			Context: ctx,
		}

		pkgs, err := packages.Load(loadCfg, chunk...)
		if err != nil {
			return nil, fmt.Errorf("load chunk %d-%d: %w", i, end, err)
		}

		for _, pkg := range pkgs {
			for _, e := range pkg.Errors {
				allErrs = append(allErrs, e)
			}
		}

		allPkgs = append(allPkgs, pkgs...)

		if onProgress != nil {
			onProgress(end, total)
		}
	}

	return &LoadResult{
		Packages: allPkgs,
		Fset:     fset,
		Errors:   allErrs,
	}, nil
}

// filterPackages removes packages matching exclude patterns
func filterPackages(pkgs []*packages.Package, exclude []string) []*packages.Package {
	if len(exclude) == 0 {
		return pkgs
	}

	var result []*packages.Package
	for _, pkg := range pkgs {
		excluded := false
		for _, pattern := range exclude {
			if matchesExclude(pkg.PkgPath, pattern) {
				excluded = true
				break
			}
		}
		if !excluded {
			result = append(result, pkg)
		}
	}
	return result
}

func matchesExclude(pkgPath, pattern string) bool {
	if pattern == "" {
		return false
	}

	// If pattern contains "/", it's a multi-component pattern
	// Match it as a contiguous path segment
	if strings.Contains(pattern, "/") {
		// Check if pattern appears as a complete segment (not partial match)
		// Pattern must be at start, end, or surrounded by "/"
		if pkgPath == pattern {
			return true
		}
		if strings.HasPrefix(pkgPath, pattern+"/") {
			return true
		}
		if strings.HasSuffix(pkgPath, "/"+pattern) {
			return true
		}
		if strings.Contains(pkgPath, "/"+pattern+"/") {
			return true
		}
		return false
	}

	// Single component pattern: split by "/" and match exactly
	for _, component := range strings.Split(pkgPath, "/") {
		if component == pattern {
			return true
		}
	}
	return false
}

// WalkFiles walks all Go files in the loaded packages
func WalkFiles(result *LoadResult, fn func(pkg *packages.Package, file *ast.File, path string) error) error {
	for _, pkg := range result.Packages {
		for i, file := range pkg.Syntax {
			if i >= len(pkg.GoFiles) {
				continue
			}
			path := pkg.GoFiles[i]
			if err := fn(pkg, file, path); err != nil {
				return err
			}
		}
	}
	return nil
}
