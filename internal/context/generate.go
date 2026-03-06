package context

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dkoosis/snipe/internal/query"
)

// Package directory constants.
const (
	pkgCmd      = "cmd"
	pkgQuery    = "query"
	pkgInternal = "internal"
)

// GenerateConfig configures context generation.
type GenerateConfig struct {
	// RepoRoot is the absolute path to the repository root
	RepoRoot string
	// DB is the snipe index database
	DB *sql.DB
	// Full includes all symbols, not just key ones
	Full bool
	// MaxSymbols is the maximum number of symbols to include per category (default: 20)
	MaxSymbols int
}

// Generate creates a ProjectContext from the snipe index.
func Generate(cfg GenerateConfig) (*ProjectContext, error) {
	if cfg.MaxSymbols == 0 {
		cfg.MaxSymbols = 20
	}

	bi := DetectBuildInfo(cfg.RepoRoot, cfg.DB)
	ctx := &ProjectContext{
		Project:      generateProject(cfg.RepoRoot, &bi),
		Architecture: generateArchitecture(cfg.DB, cfg.RepoRoot),
		Files:        generateFiles(cfg.DB, cfg.RepoRoot),
		Symbols:      generateSymbols(cfg.DB, cfg.RepoRoot, cfg.Full, cfg.MaxSymbols),
		Meta:         generateMeta(cfg.DB),
	}

	return ctx, nil
}

// GenerateBoot creates a minimal BootContext for LLM boot sequences (~2000 tokens).
func GenerateBoot(cfg GenerateConfig) (*BootContext, error) {
	buildInfo := DetectBuildInfo(cfg.RepoRoot, cfg.DB)
	proj := generateProject(cfg.RepoRoot, &buildInfo)
	meta := generateMeta(cfg.DB)

	// Get entry points (cmd/* main.go files) - backward compatible
	entryPoints := getEntryPoints(cfg.DB, cfg.RepoRoot)

	// Get top symbols by role-weighted ranking
	rankedSymbols, err := RankSymbols(cfg.DB, cfg.RepoRoot, 15)
	if err != nil {
		// Fall back to ref-count based ranking if ranking fails
		rankedSymbols = nil
	}

	// Convert ranked symbols to SymbolRef
	keySymbols := rankedToSymbolRefs(rankedSymbols, cfg.RepoRoot)
	if len(keySymbols) == 0 {
		// Fall back to ref-count based if ranking produced no results
		keySymbols = getKeySymbolsByRefCount(cfg.DB, cfg.RepoRoot, 10)
	}

	// Load session for active work context
	var activeWork *ActiveWork
	session, err := LoadSession(cfg.RepoRoot)
	if err == nil && session != nil {
		activeWork = session.GetActiveWork()
	}

	lang := "go"
	if len(proj.Lang) > 0 {
		lang = proj.Lang[0]
	}

	// Build boot views (Phase 2 enrichment)
	bootViews := generateBootViews(cfg.DB, cfg.RepoRoot)

	// Build package summaries
	var packages []PackageRef
	if purposes, err := getPackagePurposes(cfg.DB, cfg.RepoRoot); err == nil {
		for _, pp := range purposes {
			packages = append(packages, PackageRef{
				Name:    pp.Name,
				Purpose: pp.Purpose,
			})
		}
	}

	conventions := DetectConventions(cfg.DB, cfg.RepoRoot)

	return &BootContext{
		Project:     proj.Name,
		Lang:        lang,
		Build:       proj.Build,
		Test:        proj.Test,
		BuildInfo:   &buildInfo,
		EntryPoints: entryPoints,
		KeySymbols:  keySymbols,
		ActiveWork:  activeWork,
		Commit:      meta.GitCommit,
		BootViews:   bootViews,
		Packages:    packages,
		Conventions: conventions,
	}, nil
}

// generateBootViews creates the three orientation views for boot context.
func generateBootViews(db *sql.DB, repoRoot string) *BootViews {
	// Get entry point details using batch queries
	entryPointDetails, _ := GetEntryPointDetails(db, repoRoot)

	// Get primary flows using batch queries
	primaryFlows, _ := ExtractPrimaryFlows(db, repoRoot, 4)

	// Get change boundaries using batch query
	changeBoundaries, _ := GetChangeBoundaries(db, repoRoot)

	// Get interface satisfaction map
	interfaceMap, _ := GetInterfaceMap(db, repoRoot)

	// Only return BootViews if we have meaningful content
	if len(entryPointDetails) == 0 && len(primaryFlows) == 0 && len(changeBoundaries) == 0 && len(interfaceMap) == 0 {
		return nil
	}

	return &BootViews{
		EntryPointDetails: entryPointDetails,
		PrimaryFlows:      primaryFlows,
		ChangeBoundaries:  changeBoundaries,
		InterfaceMap:      interfaceMap,
	}
}

// rankedToSymbolRefs converts ranked symbols to SymbolRef with role information.
func rankedToSymbolRefs(ranked []RankedSymbol, repoRoot string) []SymbolRef {
	refs := make([]SymbolRef, 0, len(ranked))
	for _, rs := range ranked {
		ref := SymbolRef{
			Name: rs.Name,
			File: strings.TrimPrefix(rs.File, repoRoot+"/"),
			Line: rs.Line,
			Role: rs.Role,
		}
		refs = append(refs, ref)
	}
	return refs
}

// getEntryPoints finds main.go files in cmd/ directory
func getEntryPoints(db *sql.DB, repoRoot string) []string {
	var entryPoints []string

	rows, err := db.Query(`
		SELECT DISTINCT file_path
		FROM symbols
		WHERE file_path LIKE ? || '/cmd/%/main.go'
		   OR file_path LIKE ? || '/main.go'
		ORDER BY file_path
		LIMIT 5
	`, repoRoot, repoRoot)
	if err != nil {
		return entryPoints
	}
	defer rows.Close()

	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			continue
		}
		relPath := strings.TrimPrefix(path, repoRoot+"/")
		entryPoints = append(entryPoints, relPath)
	}

	return entryPoints
}

// getKeySymbolsByRefCount returns symbols ordered by reference count (most important first)
func getKeySymbolsByRefCount(db *sql.DB, repoRoot string, limit int) []SymbolRef {
	var symbols []SymbolRef

	rows, err := db.Query(`
		SELECT s.name, s.file_path, s.line_start, COUNT(r.id) as ref_count
		FROM symbols s
		LEFT JOIN refs r ON s.id = r.symbol_id
		WHERE s.file_path LIKE ? || '/%'
		  AND s.kind IN ('func', 'method', 'type', 'interface', 'struct')
		  AND s.name GLOB '[A-Z]*'
		GROUP BY s.id
		ORDER BY ref_count DESC
		LIMIT ?
	`, repoRoot, limit)
	if err != nil {
		return symbols
	}
	defer rows.Close()

	for rows.Next() {
		var ref SymbolRef
		var fullPath string
		var refCount int
		if err := rows.Scan(&ref.Name, &fullPath, &ref.Line, &refCount); err != nil {
			continue
		}
		ref.File = strings.TrimPrefix(fullPath, repoRoot+"/")
		symbols = append(symbols, ref)
	}

	return symbols
}

func generateProject(repoRoot string, buildInfo *BuildInfo) Project {
	name := filepath.Base(repoRoot)
	proj := Project{
		Name: name,
		Root: repoRoot,
		Lang: []string{"go"}, // snipe currently only supports Go
	}
	if buildInfo != nil {
		proj.Build = buildInfo.Build
		proj.Test = buildInfo.Test
	}
	return proj
}

func generateArchitecture(db *sql.DB, repoRoot string) Architecture {
	arch := Architecture{
		Components: []Component{},
		DataFlows:  []DataFlow{},
		Boundaries: []Boundary{},
	}

	// Detect the Go module path from existing symbols
	modulePath := query.DetectModulePath(db)
	if modulePath == "" {
		modulePath = filepath.Base(repoRoot) // fallback
	}

	// Find top-level packages and their purposes
	packages := getPackageInfo(db, repoRoot)
	for _, pkg := range packages {
		comp := Component{
			Name:     pkg.name,
			Purpose:  pkg.purpose,
			Entry:    pkg.entry,
			KeyFiles: pkg.keyFiles,
		}
		arch.Components = append(arch.Components, comp)
	}

	// Infer data flows from import relationships
	arch.DataFlows = inferDataFlows(db, modulePath)

	// Build package boundaries
	arch.Boundaries = inferBoundaries(db, modulePath)

	return arch
}

// inferDataFlows analyzes imports to determine data flow between packages.
func inferDataFlows(db *sql.DB, modulePath string) []DataFlow {
	// Query inter-package imports within the repository
	rows, err := db.Query(`
		SELECT
			importer_pkg,
			pkg_path,
			COUNT(*) as weight
		FROM imports
		WHERE importer_pkg LIKE ? || '%'
		  AND pkg_path LIKE ? || '/%'
		GROUP BY importer_pkg, pkg_path
		HAVING weight >= 2
		ORDER BY weight DESC
		LIMIT 20
	`, modulePath, modulePath)
	if err != nil {
		return nil
	}
	defer rows.Close()

	// Map full package paths to short names
	var flows []DataFlow
	seen := make(map[string]bool)

	for rows.Next() {
		var importer, imported string
		var weight int
		if err := rows.Scan(&importer, &imported, &weight); err != nil {
			continue
		}

		// Extract short names from package paths
		fromPkg := extractPkgShortName(importer, modulePath)
		toPkg := extractPkgShortName(imported, modulePath)

		// Skip self-imports and internal details
		if fromPkg == toPkg || fromPkg == "" || toPkg == "" {
			continue
		}

		key := fromPkg + "->" + toPkg
		if seen[key] {
			continue
		}
		seen[key] = true

		flows = append(flows, DataFlow{
			From:   fromPkg,
			To:     toPkg,
			Via:    "import",
			Weight: weight,
		})
	}

	return flows
}

// extractPkgShortName converts a full package path to a short component name.
func extractPkgShortName(pkgPath, modulePath string) string {
	// Strip module path prefix
	rel := strings.TrimPrefix(pkgPath, modulePath)
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" {
		return "root"
	}

	// Extract first or second level component
	parts := strings.Split(rel, "/")
	if len(parts) == 0 {
		return ""
	}

	// For internal/*, use internal/subpkg
	if parts[0] == pkgInternal && len(parts) > 1 {
		return pkgInternal + "/" + parts[1]
	}

	return parts[0]
}

// inferBoundaries determines what each package owns/exports.
func inferBoundaries(db *sql.DB, modulePath string) []Boundary {
	// Get distinct internal packages
	rows, err := db.Query(`
		SELECT DISTINCT pkg_path
		FROM symbols
		WHERE pkg_path LIKE ? || '/internal/%'
		ORDER BY pkg_path
	`, modulePath)
	if err != nil {
		return nil
	}

	// Collect package paths first (avoid nested queries)
	type pkgInfo struct {
		fullPath  string
		shortName string
	}
	var packages []pkgInfo
	seen := make(map[string]bool)

	for rows.Next() {
		var pkgPath string
		if err := rows.Scan(&pkgPath); err != nil {
			continue
		}

		shortName := extractPkgShortName(pkgPath, modulePath)
		if shortName == "" || seen[shortName] {
			continue
		}
		seen[shortName] = true
		packages = append(packages, pkgInfo{fullPath: pkgPath, shortName: shortName})
	}
	_ = rows.Close() // G104: close after iteration (defer not used to avoid nested query issues)

	// Now build boundaries with exports (safe to query now)
	var boundaries []Boundary
	for _, pkg := range packages {
		exports := getExportedSymbols(db, pkg.fullPath, 5)
		owns := inferOwnership(pkg.shortName)

		boundaries = append(boundaries, Boundary{
			Package: pkg.shortName,
			Owns:    owns,
			Exports: exports,
		})
	}

	return boundaries
}

// getExportedSymbols returns the top exported symbols for a package.
func getExportedSymbols(db *sql.DB, pkgPath string, limit int) []string {
	rows, err := db.Query(`
		SELECT s.name
		FROM symbols s
		LEFT JOIN refs r ON s.id = r.symbol_id
		WHERE s.pkg_path = ?
		  AND s.name GLOB '[A-Z]*'
		  AND s.kind IN ('func', 'type', 'interface', 'struct')
		GROUP BY s.id
		ORDER BY COUNT(r.id) DESC
		LIMIT ?
	`, pkgPath, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var exports []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		exports = append(exports, name)
	}
	return exports
}

// inferOwnership determines what a package is responsible for based on its name.
func inferOwnership(pkgName string) []string {
	ownership := map[string][]string{
		"internal/store":   {"SQLite database", "persistence", "transactions"},
		"internal/query":   {"symbol lookup", "reference lookup", "call graph queries"},
		"internal/index":   {"Go package loading", "symbol extraction", "call graph building"},
		"internal/output":  {"JSON formatting", "response structures", "suggestions"},
		"internal/config":  {"configuration loading", "defaults", "validation"},
		"internal/search":  {"ripgrep integration", "regex patterns", "result parsing"},
		"internal/embed":   {"vector embeddings", "similarity search", "embedding API"},
		"internal/context": {"boot context", "project analysis", "LLM summaries"},
		"internal/analyze": {"function analysis", "warning detection", "doc status"},
		"cmd":              {"CLI commands", "flags", "output formatting"},
	}

	if owns, ok := ownership[pkgName]; ok {
		return owns
	}
	return []string{"implementation details"}
}

type packageInfo struct {
	name     string
	purpose  string
	entry    string
	keyFiles []string
}

func getPackageInfo(db *sql.DB, repoRoot string) []packageInfo {
	// Query for distinct directories containing Go files
	rows, err := db.Query(`
		SELECT DISTINCT
			SUBSTR(file_path, LENGTH(?) + 2,
				INSTR(SUBSTR(file_path, LENGTH(?) + 2), '/') - 1
			) as pkg_dir
		FROM symbols
		WHERE file_path LIKE ? || '/%'
		ORDER BY pkg_dir
	`, repoRoot, repoRoot, repoRoot)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var packages []packageInfo
	seenDirs := make(map[string]bool)

	for rows.Next() {
		var dir sql.NullString
		if err := rows.Scan(&dir); err != nil || !dir.Valid || dir.String == "" {
			continue
		}
		pkgDir := dir.String
		if seenDirs[pkgDir] {
			continue
		}
		seenDirs[pkgDir] = true

		// Determine purpose based on directory name
		purpose := inferPurpose(pkgDir)

		// Find entry point
		entry := ""
		keyFiles := []string{}
		switch pkgDir {
		case pkgCmd:
			entry = "cmd/root.go"
			keyFiles = append(keyFiles, "cmd/*.go")
		case pkgInternal:
			// Get subdirectories
			keyFiles = append(keyFiles, "internal/*/*.go")
		}

		packages = append(packages, packageInfo{
			name:     pkgDir,
			purpose:  purpose,
			entry:    entry,
			keyFiles: keyFiles,
		})
	}

	return packages
}

func inferPurpose(dir string) string {
	purposes := map[string]string{
		"cmd":      "Command-line interface",
		"internal": "Internal implementation packages",
		"pkg":      "Public packages",
		"api":      "API definitions",
		"store":    "Data storage and persistence",
		"query":    "Query execution",
		"index":    "Code indexing",
		"output":   "Output formatting",
		"config":   "Configuration management",
		"search":   "Search functionality",
		"embed":    "Embedding/vector operations",
		"util":     "Utility functions",
		"test":     "Test utilities",
	}
	if purpose, ok := purposes[dir]; ok {
		return purpose
	}
	return "Application logic"
}

func generateFiles(db *sql.DB, repoRoot string) Files {
	files := Files{
		ByConcern: make(map[string][]FileInfo),
	}

	// Query files with exported symbols (key files only)
	rows, err := db.Query(`
		SELECT
			file_path,
			GROUP_CONCAT(DISTINCT CASE WHEN name GLOB '[A-Z]*' THEN name END) as exports,
			MIN(CASE WHEN doc != '' AND name GLOB '[A-Z]*' THEN doc END) as doc
		FROM symbols
		WHERE file_path LIKE ? || '/%'
		  AND kind IN ('func', 'method', 'type', 'interface', 'struct')
		GROUP BY file_path
		HAVING exports IS NOT NULL
		ORDER BY file_path
	`, repoRoot)
	if err != nil {
		return files
	}
	defer rows.Close()

	for rows.Next() {
		var filePath string
		var exportsStr, doc sql.NullString
		if err := rows.Scan(&filePath, &exportsStr, &doc); err != nil {
			continue
		}

		relPath := strings.TrimPrefix(filePath, repoRoot+"/")
		concern := categorizeByConcern(relPath)

		// Build file info
		info := FileInfo{
			Path: relPath,
		}

		// Use doc comment if available, otherwise infer from file name
		if doc.Valid && doc.String != "" {
			info.Description = ExtractFirstSentence(doc.String)
			info.Source = "doc"
		} else {
			info.Description = describeFile(relPath)
			info.Source = "inferred"
		}

		// Extract top exports (limit to 5)
		if exportsStr.Valid && exportsStr.String != "" {
			exports := strings.Split(exportsStr.String, ",")
			if len(exports) > 5 {
				exports = exports[:5]
			}
			info.Exports = exports
		}

		files.ByConcern[concern] = append(files.ByConcern[concern], info)
	}

	return files
}

// ExtractFirstSentence returns the first sentence of a doc comment.
func ExtractFirstSentence(doc string) string {
	doc = strings.TrimSpace(doc)
	// Find first period followed by space or end of string
	for i, r := range doc {
		if r == '.' {
			if i+1 >= len(doc) || doc[i+1] == ' ' || doc[i+1] == '\n' {
				return doc[:i+1]
			}
		}
		// Stop at newline too
		if r == '\n' {
			return strings.TrimSpace(doc[:i])
		}
	}
	// No period found, return first 100 chars
	if len(doc) > 100 {
		return doc[:100] + "..."
	}
	return doc
}

func categorizeByConcern(relPath string) string {
	parts := strings.Split(relPath, "/")
	if len(parts) == 0 {
		return "other"
	}

	// Check for internal packages
	if len(parts) >= 2 && parts[0] == pkgInternal {
		switch parts[1] {
		case "store":
			return "storage"
		case "query":
			return "query"
		case "index":
			return "indexing"
		case "output":
			return "output"
		case "config":
			return "configuration"
		case "search":
			return "search"
		case "embed":
			return "embeddings"
		case "context":
			return "context"
		}
	}

	// Top-level directories
	switch parts[0] {
	case pkgCmd:
		return "cli"
	case pkgInternal:
		return pkgInternal
	case "test":
		return "testing"
	}

	return "other"
}

func describeFile(relPath string) string {
	base := filepath.Base(relPath)
	name := strings.TrimSuffix(base, ".go")

	descriptions := map[string]string{
		"main":        "Application entry point",
		"root":        "CLI root command",
		"store":       "Database operations",
		"schema":      "Database schema",
		"types":       "Type definitions",
		"config":      "Configuration handling",
		"loader":      "Package loading",
		"refs":        "Reference extraction",
		"symbols":     "Symbol extraction",
		"callgraph":   "Call graph analysis",
		"lookup":      "Symbol lookup",
		"position":    "Position-based queries",
		"rg":          "Ripgrep integration",
		"json":        "JSON output formatting",
		"fingerprint": "Index fingerprinting",
		"generate":    "Code generation",
		"imports":     "Import analysis",
		"doctor":      "Health checks",
		"search":      "Search command",
		"def":         "Definition lookup",
		"show":        "Symbol display",
		"index":       "Index command",
		"callers":     "Caller analysis",
		"callees":     "Callee analysis",
		"version":     "Version information",
		"baseline":    "Performance baseline",
		"sim":         "Similarity search",
		"write":       "Write operations",
		"client":      "Client implementation",
		"vector":      "Vector operations",
		"state":       "State management",
		"history":     "History tracking",
		"metrics":     "Metrics collection",
		"degradation": "Graceful degradation",
	}

	if desc, ok := descriptions[name]; ok {
		return desc
	}
	return "Implementation"
}

func generateSymbols(db *sql.DB, repoRoot string, full bool, maxSymbols int) Symbols {
	syms := Symbols{}

	limit := maxSymbols
	if full {
		limit = 1000
	}

	syms.Types = querySymbolRefsByKind(db, repoRoot, "('type', 'interface', 'struct')", limit)
	syms.Functions = querySymbolRefsByKind(db, repoRoot, "('func', 'method')", limit)

	// Get extension points: high-centrality symbols suitable for adding new functionality
	syms.ExtensionPoints = getExtensionPoints(db, repoRoot)

	return syms
}

// querySymbolRefsByKind returns exported symbols of given kinds, ranked by reference count.
// The kindClause is a SQL IN expression like "('func', 'method')".
func querySymbolRefsByKind(db *sql.DB, repoRoot, kindClause string, limit int) []SymbolRef {
	// #nosec G201 -- kindClause is a hardcoded literal from caller
	rows, err := db.Query(`
		SELECT s.name, s.file_path, s.line_start, COUNT(r.id) as ref_count
		FROM symbols s
		LEFT JOIN refs r ON s.id = r.symbol_id
		WHERE s.kind IN `+kindClause+`
		  AND s.file_path LIKE ? || '/%'
		  AND s.name GLOB '[A-Z]*'
		GROUP BY s.id
		ORDER BY ref_count DESC
		LIMIT ?
	`, repoRoot, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var refs []SymbolRef
	for rows.Next() {
		var ref SymbolRef
		var fullPath string
		var refCount int
		if err := rows.Scan(&ref.Name, &fullPath, &ref.Line, &refCount); err != nil {
			continue
		}
		ref.File = strings.TrimPrefix(fullPath, repoRoot+"/")
		refs = append(refs, ref)
	}
	return refs
}

// getExtensionPoints finds symbols that are good extension points:
// - Interfaces (can add new implementations)
// - High-ref-count funcs (central to codebase)
// - Types with many callers (frequently used)
func getExtensionPoints(db *sql.DB, repoRoot string) []ExtensionPoint {
	var points []ExtensionPoint

	// Query for interfaces and high-centrality symbols
	rows, err := db.Query(`
		SELECT
			s.name,
			s.kind,
			s.file_path,
			s.line_start,
			s.doc,
			COUNT(DISTINCT r.id) as ref_count,
			(SELECT COUNT(*) FROM call_graph c WHERE c.callee_id = s.id) as caller_count
		FROM symbols s
		LEFT JOIN refs r ON s.id = r.symbol_id
		WHERE s.file_path LIKE ? || '/%'
		  AND s.name GLOB '[A-Z]*'
		  AND (
		      s.kind = 'interface'
		      OR (s.kind IN ('func', 'method') AND s.name NOT LIKE 'New%')
		  )
		GROUP BY s.id
		HAVING ref_count >= 3 OR s.kind = 'interface'
		ORDER BY (ref_count + caller_count * 2) DESC
		LIMIT 10
	`, repoRoot)
	if err != nil {
		return nil
	}
	defer rows.Close()

	for rows.Next() {
		var ep ExtensionPoint
		var fullPath string
		var doc sql.NullString
		if err := rows.Scan(&ep.Name, &ep.Kind, &fullPath, &ep.Line, &doc, &ep.RefCount, &ep.CallerCount); err != nil {
			continue
		}
		ep.File = strings.TrimPrefix(fullPath, repoRoot+"/")
		if doc.Valid && doc.String != "" {
			// Extract first sentence for purpose
			ep.Purpose = ExtractFirstSentence(doc.String)
		}
		points = append(points, ep)
	}

	return points
}

func generateMeta(db *sql.DB) Meta {
	meta := Meta{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}

	// Get git commit
	if out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output(); err == nil {
		meta.GitCommit = strings.TrimSpace(string(out))
	}

	// Get index fingerprint
	var fp sql.NullString
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'fingerprint'`).Scan(&fp); err == nil && fp.Valid {
		meta.IndexFingerprint = fp.String
	}

	return meta
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
