//go:build mage

// Build and quality checks for snipe.
//
// Tiers:
//
//	mage     Build, lint, test (default)
//	mage qa  Full quality: race detection, all linters, blackbox tests
//
// Set CLI=1 for console output instead of fo dashboard.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/magefile/mage/sh"
)

var Default = All

// cli returns true if CLI=1 is set.
func cli() bool {
	return os.Getenv("CLI") != ""
}

// ----------------------------------------------------------------------------
// Tier 1: Standard (default)
// ----------------------------------------------------------------------------

// All runs build, lint, and test.
func All() error {
	if cli() {
		return allCLI()
	}
	return allDashboard()
}

func allDashboard() error {
	return runFoDashboard(
		// Build (go install puts binary on PATH)
		"Build/snipe:go install .",
		// Test
		"Test/unit:go test -json -cover ./...",
		// Lint - essential
		"Lint/vet:go vet ./...",
		"Lint/gofmt:gofmt -l cmd internal test main.go magefile.go",
		"Lint/staticcheck:golangci-lint run --allow-parallel-runners --enable-only staticcheck ./...",
	)
}

func allCLI() error {
	fmt.Println("═══ Build + Lint + Test ═══")
	return runSequential(
		step{"Build", "go", []string{"install", "."}},
		step{"Test", "go", []string{"test", "-cover", "./..."}},
		step{"Vet", "go", []string{"vet", "./..."}},
		step{"Gofmt", "gofmt", []string{"-l", "cmd", "internal", "test", "main.go", "magefile.go"}},
		step{"Staticcheck", "golangci-lint", []string{"run", "--enable-only", "staticcheck", "./..."}},
	)
}

// ----------------------------------------------------------------------------
// Tier 2: Full QA
// ----------------------------------------------------------------------------

// Qa runs comprehensive quality checks.
func Qa() error {
	if cli() {
		return qaCLI()
	}
	return qaDashboard()
}

func qaDashboard() error {
	return runFoDashboard(
		// Build (go install puts binary on PATH)
		"Build/snipe:go install .",
		// Test - comprehensive (note: -cover omitted to avoid "no such tool covdata" false failures)
		"Test/unit:go test -json ./...",
		"Test/race:go test -race -json -timeout=5m ./...",
		"Test/blackbox:go test -json -tags=blackbox ./test/blackbox/...",
		// Lint - full suite
		"Lint/golangci:golangci-lint run ./...",
		// Security
		"Security/govulncheck:govulncheck ./...",
		// Metrics - performance and quality baseline
		"Metrics/snipe:cat BASELINE.json",
	)
}

func qaCLI() error {
	fmt.Println("═══ Full QA ═══")
	start := time.Now()

	var failures []string
	checks := []step{
		{"Build", "go", []string{"install", "."}},
		{"Test", "go", []string{"test", "-cover", "./..."}},
		{"Race", "go", []string{"test", "-race", "-timeout=5m", "./..."}},
		{"Blackbox", "go", []string{"test", "-tags=blackbox", "-v", "./test/blackbox/..."}},
		{"Golangci-lint", "golangci-lint", []string{"run", "./..."}},
		{"Govulncheck", "govulncheck", []string{"./..."}},
	}

	for _, c := range checks {
		fmt.Printf("\n▶ %s...\n", c.name)
		if err := sh.Run(c.cmd, c.args...); err != nil {
			failures = append(failures, c.name)
			fmt.Printf("  ✗ %s failed\n", c.name)
		} else {
			fmt.Printf("  ✓ %s passed\n", c.name)
		}
	}

	elapsed := time.Since(start)
	fmt.Printf("\n═══ QA Complete - %s ═══\n", elapsed.Round(time.Second))
	if len(failures) > 0 {
		fmt.Printf("✗ FAILED: %s\n", strings.Join(failures, ", "))
		return fmt.Errorf("qa failed: %s", strings.Join(failures, ", "))
	}
	fmt.Println("✓ All checks passed")
	return nil
}

// ----------------------------------------------------------------------------
// Individual Targets
// ----------------------------------------------------------------------------

// Build installs the snipe binary to $GOPATH/bin.
func Build() error {
	fmt.Println("→ Installing snipe...")
	version := getVersion()
	commit := getCommit()
	ldflags := fmt.Sprintf("-X github.com/dkoosis/snipe/cmd.Version=%s -X github.com/dkoosis/snipe/cmd.GitCommit=%s",
		version, commit)

	return sh.Run("go", "install", "-ldflags", ldflags, ".")
}

// Install builds and installs snipe to $GOPATH/bin.
func Install() error {
	fmt.Println("→ Installing snipe...")
	version := getVersion()
	commit := getCommit()
	ldflags := fmt.Sprintf("-X github.com/dkoosis/snipe/cmd.Version=%s -X github.com/dkoosis/snipe/cmd.GitCommit=%s",
		version, commit)

	return sh.Run("go", "install", "-ldflags", ldflags, ".")
}

// Test runs unit tests.
func Test() error {
	return sh.RunV("go", "test", "-cover", "./...")
}

// Lint runs golangci-lint.
func Lint() error {
	return sh.RunV("golangci-lint", "run", "./...")
}

// Blackbox runs blackbox tests against ../orca.
func Blackbox() error {
	return sh.RunV("go", "test", "-tags=blackbox", "-v", "./test/blackbox/...")
}

// Cross cross-compiles snipe for Codex/Claude cloud sandboxes (linux/amd64 + linux/arm64).
func Cross() error {
	version := getVersion()
	commit := getCommit()
	ldflags := fmt.Sprintf("-s -w -X github.com/dkoosis/snipe/cmd.Version=%s -X github.com/dkoosis/snipe/cmd.GitCommit=%s",
		version, commit)

	targets := []struct{ goos, goarch, dir string }{
		{"linux", "amd64", ".bin/linux-amd64"},
		{"linux", "arm64", ".bin/linux-arm64"},
	}

	for _, t := range targets {
		outPath := filepath.Join(t.dir, "snipe")
		if err := os.MkdirAll(t.dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", t.dir, err)
		}
		env := map[string]string{
			"CGO_ENABLED": "0",
			"GOOS":        t.goos,
			"GOARCH":      t.goarch,
		}
		if err := sh.RunWith(env, "go", "build", "-trimpath", "-ldflags", ldflags, "-o", outPath, "."); err != nil {
			return fmt.Errorf("build %s/%s: %w", t.goos, t.goarch, err)
		}
		// Get file size for reporting
		if info, err := os.Stat(outPath); err == nil {
			fmt.Printf("  built %s (%dMB)\n", outPath, info.Size()/(1024*1024))
		}
	}
	return nil
}

// Clean removes build artifacts.
func Clean() error {
	fmt.Println("→ Cleaning...")
	_ = sh.Rm("./bin")
	_ = sh.Rm("./.snipe")
	return nil
}

// Baseline captures performance/quality metrics.
func Baseline() error {
	fmt.Println("→ Capturing baseline metrics...")
	return sh.RunV("go", "test", "-v", "-run", "TestCaptureBaseline", "./test/bench/")
}

// Bench runs Go benchmarks.
func Bench() error {
	fmt.Println("→ Running benchmarks...")
	return sh.RunV("go", "test", "-bench=.", "-benchmem", "./test/bench/")
}

// Trend shows performance metrics over time (last 30 entries).
func Trend() error {
	fmt.Println("→ Performance Trend (last 30 entries)")
	fmt.Println()

	data, err := os.ReadFile(".snipe/metrics.jsonl")
	if err != nil {
		return fmt.Errorf("no metrics history found - run 'mage baseline' first")
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	// Show last 30 entries
	start := 0
	if len(lines) > 30 {
		start = len(lines) - 30
	}

	fmt.Printf("%-12s %-8s %8s %8s %8s %8s %8s\n",
		"Date", "Commit", "Symbols", "Refs", "IdxMs", "DefMs", "CallCov")
	fmt.Println(strings.Repeat("-", 72))

	for _, line := range lines[start:] {
		var m struct {
			Timestamp string `json:"timestamp"`
			GitCommit string `json:"git_commit"`
			Codebase  struct {
				Symbols int `json:"symbols"`
				Refs    int `json:"refs"`
			} `json:"codebase"`
			Index struct {
				TotalMs int64 `json:"total_ms"`
			} `json:"index"`
			Query struct {
				DefByNameMs float64 `json:"def_by_name_ms"`
			} `json:"query"`
			Quality struct {
				CallGraphCoverage float64 `json:"callgraph_coverage_pct"`
			} `json:"quality"`
		}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}

		date := m.Timestamp[:10]
		commit := m.GitCommit
		if len(commit) > 7 {
			commit = commit[:7]
		}

		fmt.Printf("%-12s %-8s %8d %8d %8d %8.2f %7.1f%%\n",
			date, commit, m.Codebase.Symbols, m.Codebase.Refs,
			m.Index.TotalMs, m.Query.DefByNameMs, m.Quality.CallGraphCoverage)
	}

	return nil
}

// EvalSetup clones benchmark repos and indexes them for eval.
func EvalSetup() error {
	fmt.Println("→ Setting up eval repos...")

	// Install snipe to $GOPATH/bin
	if err := sh.Run("go", "install", "."); err != nil {
		return fmt.Errorf("install snipe: %w", err)
	}
	snipeBin, err := exec.LookPath("snipe")
	if err != nil {
		return fmt.Errorf("snipe not found on PATH after install: %w", err)
	}

	evalDir := ".eval-repos"
	if err := os.MkdirAll(evalDir, 0o755); err != nil {
		return err
	}

	repos := []struct {
		Name string
		URL  string
	}{
		{"chi", "https://github.com/go-chi/chi"},
		{"cobra", "https://github.com/spf13/cobra"},
		{"bbolt", "https://github.com/etcd-io/bbolt"},
		{"fzf", "https://github.com/junegunn/fzf"},
	}

	for _, r := range repos {
		dir := filepath.Join(evalDir, r.Name)
		if _, err := os.Stat(dir); err == nil {
			fmt.Printf("  %s: already cloned\n", r.Name)
		} else {
			fmt.Printf("  %s: cloning...\n", r.Name)
			if err := sh.Run("git", "clone", "--depth=1", r.URL, dir); err != nil {
				fmt.Printf("  %s: clone failed: %v\n", r.Name, err)
				continue
			}
		}

		// Index with snipe
		fmt.Printf("  %s: indexing...\n", r.Name)
		absDir, _ := filepath.Abs(dir)
		cmd := exec.Command(snipeBin, "index", absDir, "--enrich=false", "--embed-mode=off")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Printf("  %s: index failed: %v\n", r.Name, err)
			continue
		}
		fmt.Printf("  %s: ready\n", r.Name)
	}

	return nil
}

// Eval runs the localization benchmark.
func Eval() error {
	return sh.RunV("go", "test", "-v", "-tags=eval", "-run", "TestEval", "-timeout=10m", "./test/eval/")
}

// Metrics shows 30-day performance and quality metrics with sparklines.
func Metrics() error {
	data, err := os.ReadFile(".snipe/metrics.jsonl")
	if err != nil {
		return fmt.Errorf("no metrics history - run 'mage baseline' first")
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 {
		return fmt.Errorf("no metrics data")
	}

	// Parse entries
	type entry struct {
		Timestamp string
		Commit    string
		Symbols   int
		Refs      int
		CallEdges int
		IndexMs   int64
		DefMs     float64
		RefsMs    float64
		DocCov    float64
		CallCov   float64
	}

	var entries []entry
	for _, line := range lines {
		var m struct {
			Timestamp string `json:"timestamp"`
			GitCommit string `json:"git_commit"`
			Codebase  struct {
				Symbols   int `json:"symbols"`
				Refs      int `json:"refs"`
				CallEdges int `json:"call_edges"`
			} `json:"codebase"`
			Index struct {
				TotalMs int64 `json:"total_ms"`
			} `json:"index"`
			Query struct {
				DefByNameMs float64 `json:"def_by_name_ms"`
				RefsByIDMs  float64 `json:"refs_by_id_ms"`
			} `json:"query"`
			Quality struct {
				DocCoverage       float64 `json:"doc_coverage_pct"`
				CallGraphCoverage float64 `json:"callgraph_coverage_pct"`
			} `json:"quality"`
		}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}

		commit := m.GitCommit
		if len(commit) > 7 {
			commit = commit[:7]
		}

		entries = append(entries, entry{
			Timestamp: m.Timestamp,
			Commit:    commit,
			Symbols:   m.Codebase.Symbols,
			Refs:      m.Codebase.Refs,
			CallEdges: m.Codebase.CallEdges,
			IndexMs:   m.Index.TotalMs,
			DefMs:     m.Query.DefByNameMs,
			RefsMs:    m.Query.RefsByIDMs,
			DocCov:    m.Quality.DocCoverage,
			CallCov:   m.Quality.CallGraphCoverage,
		})
	}

	// Last 30 entries
	start := 0
	if len(entries) > 30 {
		start = len(entries) - 30
	}
	entries = entries[start:]

	if len(entries) == 0 {
		return fmt.Errorf("no metrics data")
	}

	// Sparkline helper: ▁▂▃▄▅▆▇█
	sparkline := func(values []float64) string {
		if len(values) == 0 {
			return ""
		}
		bars := []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}
		minV, maxV := values[0], values[0]
		for _, v := range values {
			if v < minV {
				minV = v
			}
			if v > maxV {
				maxV = v
			}
		}
		rangeV := maxV - minV
		if rangeV == 0 {
			rangeV = 1
		}
		var result strings.Builder
		for _, v := range values {
			idx := int((v - minV) / rangeV * 7)
			if idx > 7 {
				idx = 7
			}
			result.WriteRune(bars[idx])
		}
		return result.String()
	}

	// Collect series
	var indexMs, defMs, refsMs []float64
	var symbols, refs, calls []float64
	var docCov, callCov []float64

	for _, e := range entries {
		indexMs = append(indexMs, float64(e.IndexMs))
		defMs = append(defMs, e.DefMs)
		refsMs = append(refsMs, e.RefsMs)
		symbols = append(symbols, float64(e.Symbols))
		refs = append(refs, float64(e.Refs))
		calls = append(calls, float64(e.CallEdges))
		docCov = append(docCov, e.DocCov)
		callCov = append(callCov, e.CallCov)
	}

	curr := entries[len(entries)-1]
	first := entries[0]

	// Delta helpers
	pctChange := func(curr, prev float64) string {
		if prev == 0 {
			return ""
		}
		pct := (curr - prev) / prev * 100
		if pct > 0 {
			return fmt.Sprintf("+%.1f%%", pct)
		}
		return fmt.Sprintf("%.1f%%", pct)
	}

	intChange := func(curr, prev int) string {
		diff := curr - prev
		if diff > 0 {
			return fmt.Sprintf("+%d", diff)
		} else if diff < 0 {
			return fmt.Sprintf("%d", diff)
		}
		return "="
	}

	fmt.Println()
	fmt.Println("SNIPE METRICS (30 measurements)")
	fmt.Println(strings.Repeat("─", 70))
	fmt.Println()

	// Performance metrics (lower is better)
	fmt.Println("PERFORMANCE                   Trend (30)                    Now      Δ")
	fmt.Println(strings.Repeat("─", 70))
	fmt.Printf("Index Time      %s  %8dms  %s\n",
		sparkline(indexMs), curr.IndexMs, pctChange(float64(curr.IndexMs), float64(first.IndexMs)))
	fmt.Printf("Def Query       %s  %8.3fms  %s\n",
		sparkline(defMs), curr.DefMs, pctChange(curr.DefMs, first.DefMs))
	fmt.Printf("Refs Query      %s  %8.3fms  %s\n",
		sparkline(refsMs), curr.RefsMs, pctChange(curr.RefsMs, first.RefsMs))
	fmt.Println()

	// Codebase metrics (growth)
	fmt.Println("CODEBASE                      Trend (30)                    Now      Δ")
	fmt.Println(strings.Repeat("─", 70))
	fmt.Printf("Symbols         %s  %8d  %s\n",
		sparkline(symbols), curr.Symbols, intChange(curr.Symbols, first.Symbols))
	fmt.Printf("References      %s  %8d  %s\n",
		sparkline(refs), curr.Refs, intChange(curr.Refs, first.Refs))
	fmt.Printf("Call Edges      %s  %8d  %s\n",
		sparkline(calls), curr.CallEdges, intChange(curr.CallEdges, first.CallEdges))
	fmt.Println()

	// Quality metrics (higher is better)
	fmt.Println("QUALITY                       Trend (30)                    Now      Δ")
	fmt.Println(strings.Repeat("─", 70))
	fmt.Printf("Doc Coverage    %s  %7.1f%%  %s\n",
		sparkline(docCov), curr.DocCov, pctChange(curr.DocCov, first.DocCov))
	fmt.Printf("Call Coverage   %s  %7.1f%%  %s\n",
		sparkline(callCov), curr.CallCov, pctChange(curr.CallCov, first.CallCov))
	fmt.Println()

	fmt.Printf("Period: %s → %s (%d measurements)\n",
		first.Timestamp[:10], curr.Timestamp[:10], len(entries))
	fmt.Println()

	return nil
}

// ----------------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------------

type step struct {
	name string
	cmd  string
	args []string
}

func runSequential(steps ...step) error {
	for _, s := range steps {
		fmt.Printf("→ %s\n", s.name)
		cmd := exec.Command(s.cmd, s.args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s failed: %w", s.name, err)
		}
	}
	return nil
}

func runFoDashboard(tasks ...string) error {
	// Find fo binary
	foBin := os.Getenv("HOME") + "/Projects/fo/bin/fo"
	if _, err := os.Stat(foBin); err != nil {
		var lookupErr error
		foBin, lookupErr = exec.LookPath("fo")
		if lookupErr != nil {
			// Fall back to CLI mode if fo not available
			fmt.Println("fo not found, falling back to CLI mode")
			return allCLI()
		}
	}

	// Build task args
	args := []string{"--dashboard"}
	for _, t := range tasks {
		args = append(args, "--task", t)
	}

	// Run dashboard with TTY attached
	cmd := exec.Command(foBin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "PATH="+os.Getenv("PATH")+":"+os.Getenv("HOME")+"/go/bin")
	return cmd.Run()
}

func getVersion() string {
	out, err := sh.Output("git", "describe", "--tags", "--always", "--dirty")
	if err != nil || strings.TrimSpace(out) == "" {
		return "0.1.0-dev"
	}
	return strings.TrimSpace(out)
}

func getCommit() string {
	out, err := sh.Output("git", "rev-parse", "--short", "HEAD")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(out)
}
