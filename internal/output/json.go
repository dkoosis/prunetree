package output

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/dkoosis/snipe/internal/util"
)

// globalFileCache is the default file cache for output operations.
var globalFileCache = util.NewFileCache(util.DefaultMaxCachedFiles)

// Writer handles JSON output formatting for LLM consumers.
type Writer struct {
	out     io.Writer
	compact bool
	start   time.Time
}

// NewWriter creates a new JSON output writer.
func NewWriter(out io.Writer, compact bool) *Writer {
	return &Writer{
		out:     out,
		compact: compact,
		start:   time.Now(),
	}
}

// WriteResponse writes a response as JSON.
func (w *Writer) WriteResponse(resp any) error {
	enc := json.NewEncoder(w.out)
	if !w.compact {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(resp)
}

// WriteError writes an error response
func (w *Writer) WriteError(command string, err *Error) error {
	resp := Response[any]{
		Protocol: ProtocolVersion,
		Ok:       false,
		Results:  nil,
		Meta: Meta{
			Command: command,
			Ms:      time.Since(w.start).Milliseconds(),
		},
		Error: err,
	}
	return w.WriteResponse(resp)
}

// Elapsed returns milliseconds since writer creation
func (w *Writer) Elapsed() int64 {
	return time.Since(w.start).Milliseconds()
}

// EstimateTokens estimates the token count for a string.
//
// This uses a rough heuristic of ~4 characters per token, which is
// reasonably accurate for code (keywords, identifiers, operators).
// For LLM models like GPT-4 and Claude, actual tokenization varies,
// but this provides a useful upper-bound estimate for budget planning.
//
// Note: This is an approximation. For precise token counts, use the
// specific tokenizer for your target model.
func EstimateTokens(s string) int {
	return (len(s) + 3) / 4
}

// EstimateResultTokens estimates token count for a single result.
func EstimateResultTokens(r *Result) int {
	tokens := EstimateTokens(r.Name)
	tokens += EstimateTokens(r.File)
	tokens += EstimateTokens(r.Match)
	if r.Body != "" {
		tokens += EstimateTokens(r.Body)
	}
	if r.Context != nil {
		for _, line := range r.Context.Before {
			tokens += EstimateTokens(line)
		}
		for _, line := range r.Context.After {
			tokens += EstimateTokens(line)
		}
	}
	if r.Enclosing != nil {
		tokens += EstimateTokens(r.Enclosing.Signature)
	}
	// Add overhead for JSON structure (~50 tokens per result)
	tokens += 50
	return tokens
}

// TruncateToTokenBudget truncates results to fit within a token budget.
// Returns the truncated slice and whether truncation occurred.
// If maxTokens is 0, returns the original slice unchanged.
func TruncateToTokenBudget(results []Result, maxTokens int) ([]Result, bool) {
	if maxTokens <= 0 {
		return results, false
	}

	// Reserve tokens for response wrapper (meta, error fields, etc.)
	const overhead = 200
	budget := maxTokens - overhead
	if budget <= 0 {
		return nil, len(results) > 0
	}

	var truncated []Result
	totalTokens := 0

	for i := range results {
		resultTokens := EstimateResultTokens(&results[i])
		if totalTokens+resultTokens > budget && len(truncated) > 0 {
			// Would exceed budget and we have at least one result
			return truncated, true
		}
		totalTokens += resultTokens
		truncated = append(truncated, results[i])
	}

	return truncated, false
}

// FormatEditTarget formats a range as an edit target string.
// If hash is non-empty, appends it for change detection: file:L:C-L:C@hash
func FormatEditTarget(file string, r Range, hash string) string {
	target := fmt.Sprintf("%s:%d:%d-%d:%d",
		file,
		r.Start.Line, r.Start.Col,
		r.End.Line, r.End.Col,
	)
	if hash != "" {
		target += "@" + hash
	}
	return target
}

// ComputeRangeHash computes a SHA256 hash of the content within a line range.
// Returns a truncated hash (16 hex chars) for embedding in edit_target.
// If the range cannot be read, returns an empty string.
func ComputeRangeHash(file string, r Range) string {
	lines, err := readFileLines(file)
	if err != nil {
		return ""
	}

	startLine := r.Start.Line
	endLine := r.End.Line

	// Validate range
	if startLine < 1 || endLine < startLine || startLine > len(lines) {
		return ""
	}
	if endLine > len(lines) {
		endLine = len(lines)
	}

	// Extract lines in range
	var content strings.Builder
	for i := startLine; i <= endLine; i++ {
		if i > startLine {
			content.WriteString("\n")
		}
		content.WriteString(lines[i-1])
	}

	// Compute SHA256 and truncate to 16 hex chars (8 bytes)
	h := sha256.Sum256([]byte(content.String()))
	return hex.EncodeToString(h[:8])
}

// FormatEditTargetWithHash is a convenience function that computes the range hash
// and formats the edit target in one call.
// fileRel is the relative path (for output), fileAbs is the absolute path (for reading file to compute hash).
func FormatEditTargetWithHash(fileRel, fileAbs string, r Range) string {
	hash := ComputeRangeHash(fileAbs, r)
	return FormatEditTarget(fileRel, r, hash)
}

// AddContext loads N lines of context before and after the result's range
func AddContext(result *Result, n int) error {
	if n <= 0 {
		return nil
	}

	// Use absolute path for file operations
	filePath := result.FileAbs
	if filePath == "" {
		filePath = result.File // Fallback if FileAbs not set
	}
	lines, err := readFileLines(filePath)
	if err != nil {
		return err
	}

	startLine := result.Range.Start.Line
	endLine := result.Range.End.Line

	// Get N lines before
	beforeStart := max(1, startLine-n)
	var before []string
	for i := beforeStart; i < startLine; i++ {
		if i <= len(lines) {
			before = append(before, lines[i-1])
		}
	}

	// Get N lines after
	afterEnd := min(len(lines), endLine+n)
	var after []string
	for i := endLine + 1; i <= afterEnd; i++ {
		if i <= len(lines) {
			after = append(after, lines[i-1])
		}
	}

	if len(before) > 0 || len(after) > 0 {
		result.Context = &Context{
			Before: before,
			After:  after,
		}
	}

	return nil
}

func readFileLines(path string) ([]string, error) {
	return globalFileCache.LoadLines(path)
}

// ScoreResult calculates a relevance score for a result based on match quality.
// Higher scores indicate better matches. Scoring factors:
// - Exact name match: +100
// - Prefix match: +50
// - Definition (vs reference): +30
// - Public symbol (uppercase): +20
// - Shorter file path: +10 (normalized)
func ScoreResult(result *Result, query string) float64 {
	var score float64

	name := result.Name
	queryLower := strings.ToLower(query)
	nameLower := strings.ToLower(name)

	// Match scoring (case-insensitive)
	switch {
	case nameLower == queryLower:
		score += 100 // Exact match
	case strings.HasPrefix(nameLower, queryLower):
		score += 50 // Prefix match
	case strings.Contains(nameLower, queryLower):
		score += 25 // Contains match
	}

	// Bonus for definitions over references
	switch result.Kind {
	case "func", "method", "type", "struct", "interface", "const", "var":
		score += 30
	}

	// Bonus for exported/public symbols
	if len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z' {
		score += 20
	}

	// Slight bonus for shorter paths (more likely to be core code)
	pathLen := len(result.File)
	if pathLen > 0 {
		score += 10.0 * (1.0 - float64(min(pathLen, 100))/100.0)
	}

	return score
}

// ScoreResults applies relevance scoring to all results.
func ScoreResults(results []Result, query string) {
	for i := range results {
		results[i].Score = ScoreResult(&results[i], query)
	}
}

// SortByScore sorts results by score in descending order (highest first).
// Uses stable sort with deterministic tie-breaking by File, then Name.
func SortByScore(results []Result) {
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		if results[i].File != results[j].File {
			return results[i].File < results[j].File
		}
		return results[i].Name < results[j].Name
	})
}

// ScoreAndSort scores results by relevance and sorts by score descending.
func ScoreAndSort(results []Result, query string) {
	ScoreResults(results, query)
	SortByScore(results)
}

// BuildSummary creates a summary from a slice of results
func BuildSummary(results []Result) Summary {
	fileCounts := make(map[string]int)
	kindCounts := make(map[string]int)

	for _, r := range results {
		fileCounts[r.File]++
		if r.Kind != "" {
			kindCounts[r.Kind]++
		}
	}

	var files []FileSummary
	for file, count := range fileCounts {
		files = append(files, FileSummary{File: file, Count: count})
	}

	return Summary{
		Total: len(results),
		Files: files,
		Kinds: kindCounts,
	}
}

// AddBody extracts the full source code for a result based on its range.
func AddBody(result *Result) error {
	// Use absolute path for file operations
	filePath := result.FileAbs
	if filePath == "" {
		filePath = result.File // Fallback if FileAbs not set
	}
	lines, err := readFileLines(filePath)
	if err != nil {
		return err
	}

	startLine := result.Range.Start.Line
	endLine := result.Range.End.Line

	if startLine < 1 || endLine > len(lines) {
		return nil // Invalid range, skip
	}

	// Extract lines from startLine to endLine (1-indexed)
	var body string
	for i := startLine; i <= endLine && i <= len(lines); i++ {
		if i > startLine {
			body += "\n"
		}
		body += lines[i-1]
	}

	result.Body = body
	return nil
}

// TruncateBodySemantic truncates the Body field at a semantic boundary
// (complete statement or declaration) to fit within maxLines.
// Returns true if truncation occurred.
func TruncateBodySemantic(result *Result, maxLines int) bool {
	if result.Body == "" || maxLines <= 0 {
		return false
	}

	lines := strings.Split(result.Body, "\n")
	if len(lines) <= maxLines {
		return false
	}

	// Find the best truncation point at a statement boundary
	truncateAt := findSemanticBoundary(lines, maxLines)
	if truncateAt <= 0 {
		truncateAt = maxLines // Fallback to hard limit
	}

	result.Body = strings.Join(lines[:truncateAt], "\n") + "\n// ... truncated"
	return true
}

// findSemanticBoundary finds the best line to truncate at, looking for
// statement boundaries (lines ending with ; or } or { at appropriate nesting).
// Returns 0 if no good boundary found before maxLines.
func findSemanticBoundary(lines []string, maxLines int) int {
	bestLine := 0
	braceDepth := 0

	for i := 0; i < maxLines && i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}

		// Track brace depth
		braceDepth += strings.Count(line, "{") - strings.Count(line, "}")

		// Good truncation points: complete statements at depth 0 or 1
		if braceDepth <= 1 {
			// Line ends with semicolon (statement end)
			if strings.HasSuffix(line, ";") {
				bestLine = i + 1
			}
			// Line ends with closing brace (block end)
			if strings.HasSuffix(line, "}") {
				bestLine = i + 1
			}
			// Line ends with opening brace (start of block - include it)
			if strings.HasSuffix(line, "{") && i < maxLines-1 {
				bestLine = i + 1
			}
		}
	}

	return bestLine
}

// TruncateResultsSemantic truncates results to fit within a token budget,
// preferring to keep complete results and truncating bodies at semantic boundaries.
// Returns the truncated slice, whether truncation occurred, and the estimated token count.
func TruncateResultsSemantic(results []Result, maxTokens int, maxBodyLines int) ([]Result, bool, int) {
	if maxTokens <= 0 {
		total := 0
		for i := range results {
			total += EstimateResultTokens(&results[i])
		}
		return results, false, total
	}

	// Reserve tokens for response wrapper
	const overhead = 200
	budget := maxTokens - overhead
	if budget <= 0 {
		return nil, len(results) > 0, 0
	}

	var truncated []Result
	totalTokens := 0
	didTruncate := false

	for i := range results {
		result := results[i] // Copy to allow modification

		// First, try to fit the full result
		resultTokens := EstimateResultTokens(&result)

		if totalTokens+resultTokens <= budget {
			totalTokens += resultTokens
			truncated = append(truncated, result)
			continue
		}

		// Result doesn't fit - try truncating its body
		if result.Body != "" && maxBodyLines > 0 {
			if TruncateBodySemantic(&result, maxBodyLines) {
				didTruncate = true
				resultTokens = EstimateResultTokens(&result)
				if totalTokens+resultTokens <= budget {
					totalTokens += resultTokens
					truncated = append(truncated, result)
					continue
				}
			}
		}

		// Still doesn't fit - stop adding results
		if len(truncated) > 0 {
			didTruncate = true
			break
		}

		// First result must be included even if over budget
		totalTokens += resultTokens
		truncated = append(truncated, result)
		didTruncate = true
		break
	}

	return truncated, didTruncate, totalTokens
}
