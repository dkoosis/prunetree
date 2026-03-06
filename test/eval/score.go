//go:build eval

package eval

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// BenchmarkFile is the top-level YAML structure.
type BenchmarkFile struct {
	Orca    []Task             `yaml:"orca"`
	Chi     []Task             `yaml:"chi"`
	Cobra   []Task             `yaml:"cobra"`
	Bbolt   []Task             `yaml:"bbolt"`
	Fzf     []Task             `yaml:"fzf"`
	Weights map[string]float64 `yaml:"weights"`
}

// Task is a single benchmark task parsed from YAML.
type Task struct {
	ID              string   `yaml:"id"`
	Category        string   `yaml:"category"`
	TaskDesc        string   `yaml:"task"`
	Commands        []string `yaml:"commands"`
	ExpectedSymbols []string `yaml:"expected_symbols"`
	ExpectedFiles   []string `yaml:"expected_files"`
	MaxCalls        int      `yaml:"max_calls"`
	Difficulty      string   `yaml:"difficulty"`
	Notes           string   `yaml:"notes"`
	KnownGap        bool     `yaml:"known_gap"`
}

// TaskResult is the scored outcome of running a single task.
type TaskResult struct {
	ID             string   `json:"id"`
	Category       string   `json:"category"`
	Difficulty     string   `json:"difficulty"`
	KnownGap       bool     `json:"known_gap,omitempty"`
	FileAccuracy   bool     `json:"file_accuracy"`
	SymbolAccuracy bool     `json:"symbol_accuracy"`
	CallEfficiency bool     `json:"call_efficiency"`
	MRR            float64  `json:"mrr"`
	CommandsRun    int      `json:"commands_run"`
	FoundSymbols   []string `json:"found_symbols"`
	FoundFiles     []string `json:"found_files"`
	MissingSymbols []string `json:"missing_symbols"`
	MissingFiles   []string `json:"missing_files"`
	Errors         []string `json:"errors,omitempty"`
}

// RepoResult groups task results for one repository.
type RepoResult struct {
	Name    string       `json:"name"`
	Tasks   []TaskResult `json:"tasks"`
	Skipped bool         `json:"skipped"`
}

// CategoryScore aggregates metrics for one task category.
type CategoryScore struct {
	Tasks      int     `json:"tasks"`
	FileAcc    float64 `json:"file_acc"`
	SymbolAcc  float64 `json:"symbol_acc"`
	Efficiency float64 `json:"efficiency"`
	Weight     float64 `json:"weight"`
}

// EvalReport is the full benchmark report.
type EvalReport struct {
	Timestamp  string                   `json:"timestamp"`
	GitCommit  string                   `json:"git_commit"`
	Repos      []RepoResult             `json:"repos"`
	FileAcc    float64                  `json:"file_acc"`
	SymbolAcc  float64                  `json:"symbol_acc"`
	Efficiency float64                  `json:"efficiency"`
	MeanMRR    float64                  `json:"mean_mrr"`
	KnownGaps  int                      `json:"known_gaps"`
	ByCategory map[string]CategoryScore `json:"by_category"`

	// Weighted aggregates (category importance × category score)
	WeightedFileAcc    float64 `json:"weighted_file_acc"`
	WeightedSymbolAcc  float64 `json:"weighted_symbol_acc"`
	WeightedEfficiency float64 `json:"weighted_efficiency"`
}

// snipeResponse is a minimal parse of snipe's JSON output.
// We only extract what we need for scoring.
type snipeResponse struct {
	Ok      bool          `json:"ok"`
	Results []snipeResult `json:"results"`
	Meta    snipeMeta     `json:"meta"`
	Error   *snipeError   `json:"error"`
}

type snipeResult struct {
	Name     string `json:"name"`
	Receiver string `json:"receiver"`
	File     string `json:"file"`
	Kind     string `json:"kind"`
	Match    string `json:"match"`
}

type snipeMeta struct {
	Command string `json:"command"`
	Total   int    `json:"total"`
}

type snipeError struct {
	Code       string           `json:"code"`
	Message    string           `json:"message"`
	Candidates []snipeCandidate `json:"candidates"`
}

type snipeCandidate struct {
	Name     string `json:"name"`
	Receiver string `json:"receiver"`
	File     string `json:"file"`
	Kind     string `json:"kind"`
}

// packResponse mirrors the nested pack output structure for re-parsing.
type packResponse struct {
	Ok      bool         `json:"ok"`
	Results []packResult `json:"results"`
	Meta    snipeMeta    `json:"meta"`
	Error   *snipeError  `json:"error"`
}

type packResult struct {
	Definition *snipeResult  `json:"definition"`
	References []snipeResult `json:"references"`
	Callers    []snipeResult `json:"callers"`
	Callees    []snipeResult `json:"callees"`
}

// flattenPackResponse re-parses raw JSON as a pack response and flattens
// all nested results into a flat snipeResponse for uniform scoring.
func flattenPackResponse(raw []byte) (snipeResponse, bool) {
	var pr packResponse
	if err := json.Unmarshal(raw, &pr); err != nil {
		return snipeResponse{}, false
	}
	if pr.Meta.Command != "pack" {
		return snipeResponse{}, false
	}

	var flat snipeResponse
	flat.Ok = pr.Ok
	flat.Meta = pr.Meta
	flat.Error = pr.Error

	for _, r := range pr.Results {
		if r.Definition != nil {
			flat.Results = append(flat.Results, *r.Definition)
		}
		flat.Results = append(flat.Results, r.References...)
		flat.Results = append(flat.Results, r.Callers...)
		flat.Results = append(flat.Results, r.Callees...)
	}

	return flat, true
}

// promoteAmbiguousCandidates converts AMBIGUOUS_SYMBOL error candidates
// into synthetic results so all downstream scoring works uniformly.
func promoteAmbiguousCandidates(resp *snipeResponse) {
	if resp.Error == nil || resp.Error.Code != "AMBIGUOUS_SYMBOL" {
		return
	}
	for _, c := range resp.Error.Candidates {
		resp.Results = append(resp.Results, snipeResult{
			Name:     c.Name,
			Receiver: c.Receiver,
			File:     c.File,
			Kind:     c.Kind,
		})
	}
}

// methodDeclRe matches Go method declarations to extract Type.Method.
var methodDeclRe = regexp.MustCompile(`func\s+\(\w+\s+\*?(\w+)\)\s+(\w+)\(`)

// qualifiedName builds "Receiver.Name" from a result.
func qualifiedName(name, receiver string) string {
	if receiver == "" {
		return name
	}
	r := strings.TrimPrefix(receiver, "(*")
	r = strings.TrimPrefix(r, "(")
	r = strings.TrimSuffix(r, ")")
	return r + "." + name
}

// matchSymbol checks if a snipe result matches an expected symbol.
func matchSymbol(result snipeResult, expected string) bool {
	qn := qualifiedName(result.Name, result.Receiver)
	if qn == expected {
		return true
	}
	if result.Name == expected {
		return true
	}

	// Approach 1: strip qualifier from expected and compare bare name.
	// If expected is "Store.searchFTS", also try matching just "searchFTS".
	if idx := strings.LastIndex(expected, "."); idx >= 0 {
		bare := expected[idx+1:]
		if result.Name == bare {
			return true
		}
	}

	// Approach 2: extract qualified symbol from match text (method declarations).
	if result.Match != "" {
		// Direct substring check
		if strings.Contains(result.Match, expected) {
			return true
		}
		// Regex extraction for method declarations
		if m := methodDeclRe.FindStringSubmatch(result.Match); m != nil {
			extracted := m[1] + "." + m[2]
			if extracted == expected {
				return true
			}
		}
	}
	return false
}

// normalizePath strips absolute path prefixes to enable suffix matching.
func normalizePath(p string) string {
	// If it's an absolute path, take only the relative portion.
	// This handles cases where snipe returns absolute paths.
	if filepath.IsAbs(p) {
		// Try to find a common Go project structure marker
		for _, marker := range []string{"/internal/", "/cmd/", "/pkg/", "/src/"} {
			if idx := strings.Index(p, marker); idx >= 0 {
				return p[idx+1:] // strip leading slash
			}
		}
	}
	return p
}

// matchFile checks if a snipe result's file matches an expected file path.
func matchFile(resultFile, expected string) bool {
	if resultFile == "" || expected == "" {
		return false
	}
	// Normalize both paths for comparison
	rf := normalizePath(resultFile)
	ef := normalizePath(expected)

	// Directory pattern: "internal/kg/" matches any file under that directory
	if strings.HasSuffix(ef, "/") {
		dir := ef[:len(ef)-1] // strip trailing slash
		return strings.Contains(rf, dir+"/") || strings.HasSuffix(rf, dir)
	}

	return strings.HasSuffix(rf, ef) || strings.HasSuffix(ef, rf)
}

// scoreTask evaluates all command outputs against a task's expectations.
func scoreTask(task Task, responses []snipeResponse) TaskResult {
	tr := TaskResult{
		ID:          task.ID,
		Category:    task.Category,
		Difficulty:  task.Difficulty,
		KnownGap:    task.KnownGap,
		CommandsRun: len(responses),
	}

	// Promote ambiguous candidates into results before scoring
	for i := range responses {
		promoteAmbiguousCandidates(&responses[i])
	}

	// Collect all found symbols and files across all command responses
	foundSymbolSet := map[string]bool{}
	foundFileSet := map[string]bool{}

	for _, resp := range responses {
		if resp.Error != nil {
			tr.Errors = append(tr.Errors, resp.Error.Message)
		}
		for _, r := range resp.Results {
			qn := qualifiedName(r.Name, r.Receiver)
			foundSymbolSet[qn] = true
			if r.Name != "" {
				foundSymbolSet[r.Name] = true
			}
			if r.File != "" {
				foundFileSet[r.File] = true
			}
		}
	}

	// Convert sets to slices
	for s := range foundSymbolSet {
		tr.FoundSymbols = append(tr.FoundSymbols, s)
	}
	for f := range foundFileSet {
		tr.FoundFiles = append(tr.FoundFiles, f)
	}

	// File accuracy: any expected file found
	if len(task.ExpectedFiles) > 0 {
		for _, ef := range task.ExpectedFiles {
			for _, ff := range tr.FoundFiles {
				if matchFile(ff, ef) {
					tr.FileAccuracy = true
					break
				}
			}
			if tr.FileAccuracy {
				break
			}
		}
		// Track missing files
		for _, ef := range task.ExpectedFiles {
			found := false
			for _, ff := range tr.FoundFiles {
				if matchFile(ff, ef) {
					found = true
					break
				}
			}
			if !found {
				tr.MissingFiles = append(tr.MissingFiles, ef)
			}
		}
	} else {
		// No expected files specified — pass by default
		tr.FileAccuracy = true
	}

	// Symbol accuracy: ALL expected symbols found
	if len(task.ExpectedSymbols) > 0 {
		allFound := true
		for _, es := range task.ExpectedSymbols {
			found := false
			for _, resp := range responses {
				for _, r := range resp.Results {
					if matchSymbol(r, es) {
						found = true
						break
					}
				}
				if found {
					break
				}
			}
			if !found {
				allFound = false
				tr.MissingSymbols = append(tr.MissingSymbols, es)
			}
		}
		tr.SymbolAccuracy = allFound
	} else {
		tr.SymbolAccuracy = true
	}

	// Call efficiency: commands_run <= max_calls
	if task.MaxCalls > 0 {
		tr.CallEfficiency = tr.CommandsRun <= task.MaxCalls
	} else {
		tr.CallEfficiency = true
	}

	// MRR: 1/rank of first expected symbol across all results
	tr.MRR = computeMRR(task, responses)

	return tr
}

// computeMRR finds the reciprocal rank of the first expected symbol.
func computeMRR(task Task, responses []snipeResponse) float64 {
	if len(task.ExpectedSymbols) == 0 {
		return 1.0
	}

	// Flatten all results in order
	rank := 0
	for _, resp := range responses {
		for _, r := range resp.Results {
			rank++
			for _, es := range task.ExpectedSymbols {
				if matchSymbol(r, es) {
					return 1.0 / float64(rank)
				}
			}
		}
	}
	return 0.0
}

// aggregateReport computes aggregate scores from repo results.
// known_gap tasks are excluded from aggregate metrics.
func aggregateReport(repos []RepoResult) (fileAcc, symbolAcc, efficiency, meanMRR float64, knownGaps int, byCategory map[string]CategoryScore) {
	byCategory = map[string]CategoryScore{}

	var totalTasks int
	var totalFile, totalSym, totalEff int
	var totalMRR float64

	for _, repo := range repos {
		if repo.Skipped {
			continue
		}
		for _, tr := range repo.Tasks {
			if tr.KnownGap {
				knownGaps++
				continue
			}
			totalTasks++
			if tr.FileAccuracy {
				totalFile++
			}
			if tr.SymbolAccuracy {
				totalSym++
			}
			if tr.CallEfficiency {
				totalEff++
			}
			totalMRR += tr.MRR

			cs := byCategory[tr.Category]
			cs.Tasks++
			if tr.FileAccuracy {
				cs.FileAcc++
			}
			if tr.SymbolAccuracy {
				cs.SymbolAcc++
			}
			if tr.CallEfficiency {
				cs.Efficiency++
			}
			byCategory[tr.Category] = cs
		}
	}

	if totalTasks > 0 {
		fileAcc = float64(totalFile) / float64(totalTasks) * 100
		symbolAcc = float64(totalSym) / float64(totalTasks) * 100
		efficiency = float64(totalEff) / float64(totalTasks) * 100
		meanMRR = totalMRR / float64(totalTasks)
	}

	// Convert category counts to percentages
	for k, cs := range byCategory {
		if cs.Tasks > 0 {
			cs.FileAcc = cs.FileAcc / float64(cs.Tasks) * 100
			cs.SymbolAcc = cs.SymbolAcc / float64(cs.Tasks) * 100
			cs.Efficiency = cs.Efficiency / float64(cs.Tasks) * 100
		}
		byCategory[k] = cs
	}

	return
}

// computeWeightedScores computes weighted aggregates across categories.
// Each category's score is multiplied by its weight; the result is normalized
// by total weight so missing categories don't deflate the score.
// If weights is nil or empty, returns equal-weighted averages (same as unweighted).
func computeWeightedScores(byCategory map[string]CategoryScore, weights map[string]float64) (fileAcc, symbolAcc, efficiency float64) {
	if len(weights) == 0 {
		// Fallback: equal weight per category
		var n float64
		for _, cs := range byCategory {
			if cs.Tasks == 0 {
				continue
			}
			fileAcc += cs.FileAcc
			symbolAcc += cs.SymbolAcc
			efficiency += cs.Efficiency
			n++
		}
		if n > 0 {
			fileAcc /= n
			symbolAcc /= n
			efficiency /= n
		}
		return
	}

	var totalWeight float64
	for cat, cs := range byCategory {
		if cs.Tasks == 0 {
			continue
		}
		w := weights[cat]
		if w <= 0 {
			w = 0.01 // small default for unconfigured categories
		}
		totalWeight += w
		fileAcc += cs.FileAcc * w
		symbolAcc += cs.SymbolAcc * w
		efficiency += cs.Efficiency * w

		// Annotate the category with its weight for reporting
		cs.Weight = w
		byCategory[cat] = cs
	}

	if totalWeight > 0 {
		fileAcc /= totalWeight
		symbolAcc /= totalWeight
		efficiency /= totalWeight
	}
	return
}

// formatReport prints the console report.
func formatReport(report EvalReport) string {
	var b strings.Builder

	b.WriteString("\nSNIPE LOCALIZATION BENCHMARK\n")
	b.WriteString(strings.Repeat("=", 56) + "\n")

	// Repo status line
	var repoStatus []string
	totalRun, totalSkipped := 0, 0
	for _, repo := range report.Repos {
		if repo.Skipped {
			repoStatus = append(repoStatus, repo.Name+" (skipped)")
			totalSkipped += len(repo.Tasks) // tasks from skipped repos
		} else {
			repoStatus = append(repoStatus, repo.Name)
			for _, tr := range repo.Tasks {
				if !tr.KnownGap {
					totalRun++
				}
			}
		}
	}
	b.WriteString("Repos: " + strings.Join(repoStatus, "  ") + "\n")
	if report.KnownGaps > 0 {
		b.WriteString(fmt.Sprintf("Tasks: %d scored, %d known gaps, %d skipped\n",
			totalRun, report.KnownGaps, totalSkipped))
	} else {
		b.WriteString(fmt.Sprintf("Tasks: %d run, %d skipped\n", totalRun, totalSkipped))
	}

	// By category
	b.WriteString("\nBY CATEGORY\n")
	b.WriteString(strings.Repeat("-", 68) + "\n")
	b.WriteString(fmt.Sprintf("%-16s %6s %7s %9s %12s %8s\n", "Category", "Tasks", "File%", "Symbol%", "Efficiency%", "Weight"))

	categories := []string{"search", "def", "refs", "callers", "pack", "cross-cutting", "impl", "pkg", "callees"}
	for _, cat := range categories {
		cs, ok := report.ByCategory[cat]
		if !ok {
			continue
		}
		weightStr := "  -"
		if cs.Weight > 0 {
			weightStr = fmt.Sprintf("  %.0f%%", cs.Weight*100)
		}
		b.WriteString(fmt.Sprintf("%-16s %6d %6.0f%% %8.0f%% %11.0f%% %7s\n",
			cat, cs.Tasks, cs.FileAcc, cs.SymbolAcc, cs.Efficiency, weightStr))
	}

	// Aggregate (unweighted — equal per-task contribution)
	b.WriteString("\nAGGREGATE (unweighted)\n")
	b.WriteString(strings.Repeat("-", 68) + "\n")
	b.WriteString(fmt.Sprintf("File accuracy:    %5.1f%%  (target: >90%%)  [%s]\n",
		report.FileAcc, passOrMiss(report.FileAcc, 90)))
	b.WriteString(fmt.Sprintf("Symbol accuracy:  %5.1f%%  (target: >75%%)  [%s]\n",
		report.SymbolAcc, passOrMiss(report.SymbolAcc, 75)))
	b.WriteString(fmt.Sprintf("Efficiency:       %5.1f%%  (target: >80%%)  [%s]\n",
		report.Efficiency, passOrMiss(report.Efficiency, 80)))
	b.WriteString(fmt.Sprintf("MRR (secondary):  %5.2f\n", report.MeanMRR))

	// Weighted aggregate (category importance × category score)
	if report.WeightedFileAcc > 0 || report.WeightedSymbolAcc > 0 {
		b.WriteString("\nAGGREGATE (weighted by category importance)\n")
		b.WriteString(strings.Repeat("-", 68) + "\n")
		b.WriteString(fmt.Sprintf("File accuracy:    %5.1f%%  (target: >90%%)  [%s]\n",
			report.WeightedFileAcc, passOrMiss(report.WeightedFileAcc, 90)))
		b.WriteString(fmt.Sprintf("Symbol accuracy:  %5.1f%%  (target: >75%%)  [%s]\n",
			report.WeightedSymbolAcc, passOrMiss(report.WeightedSymbolAcc, 75)))
		b.WriteString(fmt.Sprintf("Efficiency:       %5.1f%%  (target: >80%%)  [%s]\n",
			report.WeightedEfficiency, passOrMiss(report.WeightedEfficiency, 80)))
	}

	// Known gaps
	var gaps []string
	for _, repo := range report.Repos {
		if repo.Skipped {
			continue
		}
		for _, tr := range repo.Tasks {
			if tr.KnownGap {
				line := tr.ID
				if len(tr.MissingSymbols) > 0 {
					line += fmt.Sprintf("  missing:[%s]", strings.Join(tr.MissingSymbols, ", "))
				}
				gaps = append(gaps, line)
			}
		}
	}

	if len(gaps) > 0 {
		b.WriteString(fmt.Sprintf("\nKNOWN GAPS (%d)\n", len(gaps)))
		b.WriteString(strings.Repeat("-", 56) + "\n")
		for _, g := range gaps {
			b.WriteString(g + "\n")
		}
	}

	// Failures (non-gap)
	var failures []string
	for _, repo := range report.Repos {
		if repo.Skipped {
			continue
		}
		for _, tr := range repo.Tasks {
			if tr.KnownGap {
				continue
			}
			if !tr.SymbolAccuracy || !tr.FileAccuracy {
				line := tr.ID
				if !tr.FileAccuracy {
					line += "  file-miss"
				}
				if !tr.SymbolAccuracy && len(tr.MissingSymbols) > 0 {
					line += fmt.Sprintf("  missing:[%s]", strings.Join(tr.MissingSymbols, ", "))
				}
				failures = append(failures, line)
			}
		}
	}

	if len(failures) > 0 {
		b.WriteString("\nFAILURES\n")
		b.WriteString(strings.Repeat("-", 56) + "\n")
		for _, f := range failures {
			b.WriteString(f + "\n")
		}
	}

	return b.String()
}

func passOrMiss(value, target float64) string {
	if value >= target {
		return "PASS"
	}
	return "MISS"
}

// reportJSON marshals the report as indented JSON.
func reportJSON(report EvalReport) ([]byte, error) {
	return json.MarshalIndent(report, "", "  ")
}

// reportJSONL marshals the report as a single JSON line.
func reportJSONL(report EvalReport) ([]byte, error) {
	return json.Marshal(report)
}
