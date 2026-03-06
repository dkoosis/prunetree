package metrics

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/dkoosis/snipe/internal/index"
	"github.com/dkoosis/snipe/internal/query"
	"github.com/dkoosis/snipe/internal/search"
	"github.com/dkoosis/snipe/internal/store"
)

// Baseline captures baseline measurements for snipe
type Baseline struct {
	Timestamp string         `json:"timestamp"`
	GitCommit string         `json:"git_commit,omitempty"`
	GoVersion string         `json:"go_version"`
	Codebase  CodebaseStats  `json:"codebase"`
	Index     IndexMetrics   `json:"index"`
	Query     QueryMetrics   `json:"query"`
	Search    SearchMetrics  `json:"search"`
	Quality   QualityMetrics `json:"quality"`
}

// CodebaseStats contains codebase statistics
type CodebaseStats struct {
	Name      string `json:"name,omitempty"`
	Path      string `json:"path"`
	GoFiles   int    `json:"go_files"`
	Symbols   int    `json:"symbols"`
	Refs      int    `json:"refs"`
	CallEdges int    `json:"call_edges"`
	DBSizeKB  int    `json:"db_size_kb"`
}

// IndexMetrics contains indexing performance metrics
type IndexMetrics struct {
	TotalMs   int64 `json:"total_ms"`
	LoadMs    int64 `json:"load_ms"`
	ExtractMs int64 `json:"extract_ms"`
	PersistMs int64 `json:"persist_ms"`
	PeakMemMB int   `json:"peak_mem_mb"`
}

// QueryMetrics contains query performance metrics
type QueryMetrics struct {
	DefByNameMs float64 `json:"def_by_name_ms"`
	DefByPosMs  float64 `json:"def_by_pos_ms"`
	RefsByIDMs  float64 `json:"refs_by_id_ms"`
}

// SearchMetrics contains search performance metrics
type SearchMetrics struct {
	SimplePatternMs float64 `json:"simple_pattern_ms"`
	RegexPatternMs  float64 `json:"regex_pattern_ms"`
}

// QualityMetrics contains quality metrics
type QualityMetrics struct {
	SymbolsWithDoc    int     `json:"symbols_with_doc"`
	SymbolsWithSig    int     `json:"symbols_with_sig"`
	DocCoverage       float64 `json:"doc_coverage_pct"`
	RefsPerSymbol     float64 `json:"refs_per_symbol"`
	CallGraphCoverage float64 `json:"callgraph_coverage_pct"`
}

// CaptureConfig configures baseline capture
type CaptureConfig struct {
	Dir       string // Directory to index
	Name      string // Codebase name (optional)
	DBPath    string // Path to store index (temp if empty)
	QueryRuns int    // Number of query iterations (default 100)
}

// Capture captures baseline metrics for a codebase
func Capture(cfg CaptureConfig) (*Baseline, error) {
	if cfg.QueryRuns == 0 {
		cfg.QueryRuns = 100
	}

	baseline := &Baseline{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		GoVersion: runtime.Version(),
		Codebase: CodebaseStats{
			Name: cfg.Name,
			Path: cfg.Dir,
		},
	}

	// Get git commit
	if commit, err := getGitCommit(cfg.Dir); err == nil {
		baseline.GitCommit = commit
	}

	// Determine DB path
	dbPath := cfg.DBPath
	if dbPath == "" {
		tmpDir, err := os.MkdirTemp("", "snipe-baseline-*")
		if err != nil {
			return nil, err
		}
		dbPath = tmpDir + "/snipe.db"
		defer os.RemoveAll(tmpDir)
	}

	// Capture memory baseline
	var memBefore, memAfter runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	// Index metrics
	indexStart := time.Now()

	s, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	defer s.Close()

	// Load
	loadStart := time.Now()
	result, err := index.Load(index.LoadConfig{Dir: cfg.Dir})
	if err != nil {
		return nil, err
	}
	loadEnd := time.Now()

	// Extract
	extractStart := time.Now()
	syms, err := index.ExtractSymbols(result)
	if err != nil {
		return nil, err
	}
	refs, err := index.ExtractRefs(result, syms)
	if err != nil {
		return nil, err
	}
	calls, err := index.ExtractCallGraph(result, syms)
	if err != nil {
		return nil, err
	}
	extractEnd := time.Now()

	// Persist
	persistStart := time.Now()
	if err := s.WriteIndex(syms, refs, calls); err != nil {
		return nil, err
	}
	persistEnd := time.Now()

	indexEnd := time.Now()
	runtime.ReadMemStats(&memAfter)

	baseline.Index.TotalMs = indexEnd.Sub(indexStart).Milliseconds()
	baseline.Index.LoadMs = loadEnd.Sub(loadStart).Milliseconds()
	baseline.Index.ExtractMs = extractEnd.Sub(extractStart).Milliseconds()
	baseline.Index.PersistMs = persistEnd.Sub(persistStart).Milliseconds()

	peakMemMB := (memAfter.TotalAlloc - memBefore.TotalAlloc) / 1024 / 1024
	if peakMemMB > math.MaxInt {
		return nil, fmt.Errorf("peak memory %d MB overflows int", peakMemMB)
	}
	baseline.Index.PeakMemMB = int(peakMemMB)

	// Codebase stats
	baseline.Codebase.Symbols = len(syms)
	baseline.Codebase.Refs = len(refs)
	baseline.Codebase.CallEdges = len(calls)

	// Count unique files
	files := make(map[string]bool)
	for _, sym := range syms {
		files[sym.FilePath] = true
	}
	baseline.Codebase.GoFiles = len(files)

	// DB size
	if fi, err := os.Stat(dbPath); err == nil {
		baseline.Codebase.DBSizeKB = int(fi.Size() / 1024)
	}

	// Query metrics
	runs := cfg.QueryRuns

	// def by name - find a symbol to test with
	testSymbol := "Symbol"
	if len(syms) > 0 {
		testSymbol = syms[0].Name
	}

	start := time.Now()
	for i := 0; i < runs; i++ {
		_, _ = query.LookupByName(s.DB(), testSymbol)
	}
	baseline.Query.DefByNameMs = float64(time.Since(start).Microseconds()) / float64(runs) / 1000.0

	// def by position - use first symbol's position
	if len(syms) > 0 {
		pos := &query.PositionQuery{
			File: syms[0].FilePath,
			Line: syms[0].LineStart,
			Col:  syms[0].ColStart,
		}
		start = time.Now()
		for i := 0; i < runs; i++ {
			_, _ = query.ResolvePosition(s.DB(), pos)
		}
		baseline.Query.DefByPosMs = float64(time.Since(start).Microseconds()) / float64(runs) / 1000.0
	}

	// refs by ID
	foundSyms, _ := query.LookupByName(s.DB(), testSymbol)
	if len(foundSyms) > 0 {
		symbolID := foundSyms[0].ID
		start = time.Now()
		for i := 0; i < runs; i++ {
			_, _ = query.FindRefs(s.DB(), symbolID, 100, 0)
		}
		baseline.Query.RefsByIDMs = float64(time.Since(start).Microseconds()) / float64(runs) / 1000.0
	}

	// Search metrics
	start = time.Now()
	for i := 0; i < runs; i++ {
		_, _ = search.Search(cfg.Dir, "func", 50, 0)
	}
	baseline.Search.SimplePatternMs = float64(time.Since(start).Microseconds()) / float64(runs) / 1000.0

	start = time.Now()
	for i := 0; i < runs; i++ {
		_, _ = search.Search(cfg.Dir, "func.*Error", 50, 0)
	}
	baseline.Search.RegexPatternMs = float64(time.Since(start).Microseconds()) / float64(runs) / 1000.0

	// Quality metrics
	withDoc := 0
	withSig := 0
	funcCount := 0
	for _, sym := range syms {
		if sym.Doc != "" {
			withDoc++
		}
		if sym.Signature != "" {
			withSig++
		}
		if sym.Kind == "function" || sym.Kind == "method" {
			funcCount++
		}
	}
	baseline.Quality.SymbolsWithDoc = withDoc
	baseline.Quality.SymbolsWithSig = withSig
	if len(syms) > 0 {
		baseline.Quality.DocCoverage = float64(withDoc) / float64(len(syms)) * 100
		baseline.Quality.RefsPerSymbol = float64(len(refs)) / float64(len(syms))
	}

	// Count funcs with outgoing call edges
	funcsWithCalls := make(map[string]bool)
	for _, c := range calls {
		funcsWithCalls[c.CallerID] = true
	}
	if funcCount > 0 {
		baseline.Quality.CallGraphCoverage = float64(len(funcsWithCalls)) / float64(funcCount) * 100
	}

	return baseline, nil
}

// ToJSON converts baseline to JSON
func (b *Baseline) ToJSON() ([]byte, error) {
	return json.MarshalIndent(b, "", "  ")
}

// ToJSONL converts baseline to single-line JSON (for appending to history)
func (b *Baseline) ToJSONL() ([]byte, error) {
	return json.Marshal(b)
}

func getGitCommit(dir string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--short", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
