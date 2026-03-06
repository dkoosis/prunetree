package query

import (
	"bufio"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/dkoosis/snipe/internal/output"
)

// TODO: Add context.Context to all query functions for cancellation support.
// This is a future improvement - see https://github.com/dkoosis/snipe/issues/XX

// SymbolRow represents a row from the symbols table
type SymbolRow struct {
	ID          string
	Name        string
	Kind        string
	FilePath    string // Absolute path (for file operations)
	FilePathRel string // Relative path (for output)
	PkgPath     string // Go package path (for qualified lookups)
	LineStart   int
	ColStart    int
	LineEnd     int
	ColEnd      int
	Signature   sql.NullString
	Doc         sql.NullString
	Receiver    sql.NullString
	FileHash    string // Content hash for change detection
}

// LookupByID looks up a symbol by its ID.
func LookupByID(db *sql.DB, id string) (*SymbolRow, error) {
	var s SymbolRow
	var fileHash, filePathRel, pkgPath sql.NullString
	err := db.QueryRow(`
		SELECT s.id, s.name, s.kind, s.file_path, s.file_path_rel, s.pkg_path, s.line_start, s.col_start, s.line_end, s.col_end,
		       s.signature, s.doc, s.receiver, f.hash
		FROM symbols s
		LEFT JOIN files f ON s.file_path = f.path
		WHERE s.id = ?
	`, id).Scan(&s.ID, &s.Name, &s.Kind, &s.FilePath, &filePathRel, &pkgPath, &s.LineStart, &s.ColStart, &s.LineEnd, &s.ColEnd,
		&s.Signature, &s.Doc, &s.Receiver, &fileHash)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query symbol by id: %w", err)
	}
	s.FileHash = fileHash.String
	s.FilePathRel = filePathRel.String
	s.PkgPath = pkgPath.String
	return &s, nil
}

// BatchLookupByID looks up multiple symbols by their IDs in a single query.
// Returns a map from ID to SymbolRow. Missing IDs are not included in the map.
func BatchLookupByID(db *sql.DB, ids []string) (map[string]*SymbolRow, error) {
	if len(ids) == 0 {
		return make(map[string]*SymbolRow), nil
	}

	// Deduplicate IDs to avoid redundant SQL parameters
	seen := make(map[string]struct{}, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			unique = append(unique, id)
		}
	}

	// Build placeholders for IN clause
	placeholders := make([]string, len(unique))
	args := make([]interface{}, len(unique))
	for i, id := range unique {
		placeholders[i] = "?"
		args[i] = id
	}

	// #nosec G201 -- placeholders[] contains only "?" literals, args[] holds actual values - parameterized query
	query := fmt.Sprintf(`
		SELECT s.id, s.name, s.kind, s.file_path, s.file_path_rel, s.pkg_path, s.line_start, s.col_start, s.line_end, s.col_end,
		       s.signature, s.doc, s.receiver, f.hash
		FROM symbols s
		LEFT JOIN files f ON s.file_path = f.path
		WHERE s.id IN (%s)
	`, strings.Join(placeholders, ", "))

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("batch query symbols: %w", err)
	}
	defer rows.Close()

	result := make(map[string]*SymbolRow, len(ids))
	for rows.Next() {
		var s SymbolRow
		var fileHash, filePathRel, pkgPath sql.NullString
		if err := rows.Scan(&s.ID, &s.Name, &s.Kind, &s.FilePath, &filePathRel, &pkgPath,
			&s.LineStart, &s.ColStart, &s.LineEnd, &s.ColEnd,
			&s.Signature, &s.Doc, &s.Receiver, &fileHash); err != nil {
			return nil, fmt.Errorf("scan symbol row: %w", err)
		}
		s.FileHash = fileHash.String
		s.FilePathRel = filePathRel.String
		s.PkgPath = pkgPath.String
		result[s.ID] = &s
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate symbol rows: %w", err)
	}

	return result, nil
}

// LookupByName looks up symbols by name
// Returns candidates if multiple matches
func LookupByName(db *sql.DB, name string) ([]SymbolRow, error) {
	// Check for method syntax ((*T).Method or T.Method)
	if strings.HasPrefix(name, "(") || strings.Contains(name, ").") {
		return lookupMethod(db, name)
	}

	// Check for qualified name (pkg/path.Symbol or Type.Method)
	if idx := strings.LastIndex(name, "."); idx >= 0 {
		prefix := name[:idx]
		suffix := name[idx+1:]

		// If prefix looks like a type name (starts with uppercase, no slashes),
		// try method lookup first
		if len(prefix) > 0 && prefix[0] >= 'A' && prefix[0] <= 'Z' && !strings.Contains(prefix, "/") {
			results, err := lookupMethod(db, name)
			if err != nil {
				return nil, err
			}
			if len(results) > 0 {
				return results, nil
			}
			// Fall through to qualified lookup
		}

		return lookupQualified(db, prefix, suffix)
	}

	// Simple name lookup
	return lookupSimple(db, name)
}

// LookupByNameInFile looks up a symbol by name within a specific file pattern.
// The filePattern can be a full path, relative path, or partial match.
func LookupByNameInFile(db *sql.DB, name, filePattern string) ([]SymbolRow, error) {
	pattern := "%" + filePattern + "%"
	rows, err := db.Query(`
		SELECT s.id, s.name, s.kind, s.file_path, s.file_path_rel, s.pkg_path, s.line_start, s.col_start, s.line_end, s.col_end,
		       s.signature, s.doc, s.receiver, f.hash
		FROM symbols s
		LEFT JOIN files f ON s.file_path = f.path
		WHERE s.name = ? AND (s.file_path LIKE ? OR s.file_path_rel LIKE ?)
		ORDER BY s.kind, s.file_path, s.line_start
	`, name, pattern, pattern)
	if err != nil {
		return nil, fmt.Errorf("query symbols by name in file: %w", err)
	}
	defer rows.Close()

	return scanSymbolRows(rows)
}

func lookupSimple(db *sql.DB, name string) ([]SymbolRow, error) {
	// Exact match first (uses idx_symbols_name index)
	rows, err := db.Query(`
		SELECT s.id, s.name, s.kind, s.file_path, s.file_path_rel, s.pkg_path, s.line_start, s.col_start, s.line_end, s.col_end,
		       s.signature, s.doc, s.receiver, f.hash
		FROM symbols s
		LEFT JOIN files f ON s.file_path = f.path
		WHERE s.name = ?
		ORDER BY s.kind, s.file_path, s.line_start
	`, name)
	if err != nil {
		return nil, fmt.Errorf("query symbols by name: %w", err)
	}
	defer rows.Close()

	results, err := scanSymbolRows(rows)
	if err != nil {
		return nil, err
	}
	if len(results) > 0 {
		return results, nil
	}

	// Case-insensitive fallback (table scan, but only on miss)
	rows2, err := db.Query(`
		SELECT s.id, s.name, s.kind, s.file_path, s.file_path_rel, s.pkg_path, s.line_start, s.col_start, s.line_end, s.col_end,
		       s.signature, s.doc, s.receiver, f.hash
		FROM symbols s
		LEFT JOIN files f ON s.file_path = f.path
		WHERE s.name = ? COLLATE NOCASE
		ORDER BY s.kind, s.file_path, s.line_start
	`, name)
	if err != nil {
		return nil, fmt.Errorf("query symbols by name (case-insensitive): %w", err)
	}
	defer rows2.Close()

	return scanSymbolRows(rows2)
}

func lookupQualified(db *sql.DB, pkgPath, name string) ([]SymbolRow, error) {
	// Try exact pkg_path match first — uses idx_symbols_name_pkg composite index.
	rows, err := db.Query(`
		SELECT s.id, s.name, s.kind, s.file_path, s.file_path_rel, s.pkg_path, s.line_start, s.col_start, s.line_end, s.col_end,
		       s.signature, s.doc, s.receiver, f.hash
		FROM symbols s
		LEFT JOIN files f ON s.file_path = f.path
		WHERE s.name = ? AND s.pkg_path = ?
		ORDER BY s.kind, s.file_path, s.line_start
	`, name, pkgPath)
	if err != nil {
		return nil, fmt.Errorf("query symbols qualified exact: %w", err)
	}
	results, err := scanSymbolRows(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(results) > 0 {
		return results, nil
	}

	// Fallback: suffix match for partial package paths (e.g., "internal/handler").
	// Uses leading-wildcard LIKE which can't use index, but only runs when exact fails.
	suffixPattern := "%/" + pkgPath
	rows, err = db.Query(`
		SELECT s.id, s.name, s.kind, s.file_path, s.file_path_rel, s.pkg_path, s.line_start, s.col_start, s.line_end, s.col_end,
		       s.signature, s.doc, s.receiver, f.hash
		FROM symbols s
		LEFT JOIN files f ON s.file_path = f.path
		WHERE s.name = ? AND s.pkg_path LIKE ?
		ORDER BY s.kind, s.file_path, s.line_start
	`, name, suffixPattern)
	if err != nil {
		return nil, fmt.Errorf("query symbols qualified suffix: %w", err)
	}
	defer rows.Close()

	return scanSymbolRows(rows)
}

func lookupMethod(db *sql.DB, name string) ([]SymbolRow, error) {
	// Parse method syntax: (*T).Method or (T).Method
	var receiver, method string

	if idx := strings.Index(name, ")."); idx >= 0 {
		receiver = name[:idx+1]
		method = name[idx+2:]
	} else {
		// Try T.Method format
		parts := strings.SplitN(name, ".", 2)
		if len(parts) == 2 {
			receiver = "(" + parts[0] + ")"
			method = parts[1]
		} else {
			return nil, nil
		}
	}

	rows, err := db.Query(`
		SELECT s.id, s.name, s.kind, s.file_path, s.file_path_rel, s.pkg_path, s.line_start, s.col_start, s.line_end, s.col_end,
		       s.signature, s.doc, s.receiver, f.hash
		FROM symbols s
		LEFT JOIN files f ON s.file_path = f.path
		WHERE s.name = ? AND (s.receiver = ? OR s.receiver = ?)
		ORDER BY s.file_path, s.line_start
	`, method, receiver, "(*"+strings.Trim(receiver, "()")+")")
	if err != nil {
		return nil, fmt.Errorf("query method: %w", err)
	}
	defer rows.Close()

	return scanSymbolRows(rows)
}

// FindSymbolsInFile finds all symbols in files matching a file pattern.
// Returns symbols ordered by line position.
func FindSymbolsInFile(db *sql.DB, filePattern string, limit, offset int) ([]SymbolRow, error) {
	pattern := "%" + filePattern + "%"
	rows, err := db.Query(`
		SELECT s.id, s.name, s.kind, s.file_path, s.file_path_rel, s.pkg_path, s.line_start, s.col_start, s.line_end, s.col_end,
		       s.signature, s.doc, s.receiver, f.hash
		FROM symbols s
		LEFT JOIN files f ON s.file_path = f.path
		WHERE (s.file_path LIKE ? OR s.file_path_rel LIKE ?)
		  AND s.kind NOT IN ('field')
		ORDER BY s.file_path, s.line_start
		LIMIT ? OFFSET ?
	`, pattern, pattern, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("query symbols in file: %w", err)
	}
	defer rows.Close()

	return scanSymbolRows(rows)
}

func scanSymbolRows(rows *sql.Rows) ([]SymbolRow, error) {
	var symbols []SymbolRow
	for rows.Next() {
		var s SymbolRow
		var fileHash, filePathRel, pkgPath sql.NullString
		err := rows.Scan(&s.ID, &s.Name, &s.Kind, &s.FilePath, &filePathRel, &pkgPath, &s.LineStart, &s.ColStart, &s.LineEnd, &s.ColEnd,
			&s.Signature, &s.Doc, &s.Receiver, &fileHash)
		if err != nil {
			return nil, fmt.Errorf("scan symbol row: %w", err)
		}
		s.FileHash = fileHash.String
		s.FilePathRel = filePathRel.String
		s.PkgPath = pkgPath.String
		symbols = append(symbols, s)
	}
	return symbols, rows.Err()
}

// FindRefs finds all references to a symbol
func FindRefs(db *sql.DB, symbolID string, limit, offset int) ([]RefRow, error) {
	rows, err := db.Query(`
		SELECT r.id, r.symbol_id, r.file_path, r.file_path_rel, r.line, r.col, r.enclosing_id, r.snippet,
		       s.name, s.kind, s.signature, f.hash
		FROM refs r
		LEFT JOIN symbols s ON r.enclosing_id = s.id
		LEFT JOIN files f ON r.file_path = f.path
		WHERE r.symbol_id = ?
		ORDER BY r.file_path, r.line
		LIMIT ? OFFSET ?
	`, symbolID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("query refs: %w", err)
	}
	defer rows.Close()

	var refs []RefRow
	for rows.Next() {
		var r RefRow
		var encName, encKind, encSig, fileHash, filePathRel sql.NullString
		err := rows.Scan(&r.ID, &r.SymbolID, &r.FilePath, &filePathRel, &r.Line, &r.Col, &r.EnclosingID, &r.Snippet,
			&encName, &encKind, &encSig, &fileHash)
		if err != nil {
			return nil, fmt.Errorf("scan ref row: %w", err)
		}
		r.FilePathRel = filePathRel.String
		r.EnclosingName = encName.String
		r.EnclosingKind = encKind.String
		r.EnclosingSignature = encSig.String
		r.FileHash = fileHash.String
		refs = append(refs, r)
	}

	return refs, rows.Err()
}

// RefRow represents a reference with enclosing context
type RefRow struct {
	ID                 string
	SymbolID           string
	FilePath           string // Absolute path (for file operations)
	FilePathRel        string // Relative path (for output)
	Line               int
	Col                int
	EnclosingID        sql.NullString
	Snippet            string
	EnclosingName      string
	EnclosingKind      string
	EnclosingSignature string
	FileHash           string // Content hash for change detection
}

// ToResult converts a SymbolRow to an output.Result
func (s *SymbolRow) ToResult() output.Result {
	r := output.Range{
		Start: output.Position{Line: s.LineStart, Col: s.ColStart},
		End:   output.Position{Line: s.LineEnd, Col: s.ColEnd},
	}
	// Use relative path for output, absolute path for file operations
	filePath := s.FilePathRel
	if filePath == "" {
		filePath = s.FilePath // Fallback to absolute if relative not available
	}

	// Compute static analysis hints
	hints := s.computeHints()

	result := output.Result{
		ID:         s.ID,
		File:       filePath,
		FileAbs:    s.FilePath,
		Range:      r,
		Kind:       s.Kind,
		Name:       s.Name,
		Match:      s.Signature.String,
		Hints:      hints,
		EditTarget: output.FormatEditTargetWithHash(filePath, s.FilePath, r),
	}

	// Add receiver for methods
	if s.Receiver.Valid && s.Receiver.String != "" {
		result.Receiver = s.Receiver.String
	}

	// Add package path
	if s.PkgPath != "" {
		result.Package = s.PkgPath
	}

	return result
}

// computeHints detects static analysis hints for a symbol.
func (s *SymbolRow) computeHints() []string {
	var hints []string

	// Check for deprecated in doc string
	if s.Doc.Valid && s.Doc.String != "" {
		docLower := strings.ToLower(s.Doc.String)
		if strings.Contains(docLower, "deprecated") {
			hints = append(hints, output.HintDeprecated)
		}
	}

	// Check for pointer receiver (method can be called on nil)
	if s.Receiver.Valid && strings.HasPrefix(s.Receiver.String, "(*") {
		hints = append(hints, output.HintPointerRecv)
	}

	return hints
}

// ToResultWithHints converts a SymbolRow to an output.Result with ref count and unused detection.
// If db is provided, includes reference count and checks if exported symbols are unused.
// RefCount is set to -1 if the query fails.
func (s *SymbolRow) ToResultWithHints(db *sql.DB) output.Result {
	result := s.ToResult()

	// Get reference count (always included when db is available)
	if db != nil {
		refCount, err := GetRefCount(db, s.ID)
		if err != nil {
			result.RefCount = -1 // Indicate unavailable due to error
		} else {
			result.RefCount = refCount
			// Check for unused exported symbols
			if isExported(s.Name) && refCount == 0 {
				result.Hints = append(result.Hints, output.HintUnused)
			}
		}
	}

	// Add function analysis for func/method kinds
	if s.Kind == "func" || s.Kind == "method" {
		result.Analysis = s.ComputeFuncAnalysis()
	}

	return result
}

// ComputeFuncAnalysis computes function metrics from symbol data.
func (s *SymbolRow) ComputeFuncAnalysis() *output.FuncAnalysis {
	analysis := &output.FuncAnalysis{
		LineCount:  s.LineEnd - s.LineStart + 1,
		IsExported: isExported(s.Name),
	}

	// Extract receiver type for methods
	if s.Receiver.Valid && s.Receiver.String != "" {
		recv := s.Receiver.String
		// Strip parentheses: "(*Server)" -> "*Server"
		recv = strings.TrimPrefix(recv, "(")
		recv = strings.TrimSuffix(recv, ")")
		analysis.ReceiverType = recv
	}

	// Parse signature for param/result counts
	if s.Signature.Valid && s.Signature.String != "" {
		sig := s.Signature.String
		analysis.ParamCount, analysis.ResultCount, analysis.IsVariadic = parseSignatureCounts(sig)
	}

	return analysis
}

// parseSignatureCounts extracts param count, result count, and variadic flag from a signature.
// Example: "func Load(cfg LoadConfig) (*LoadResult, error)" -> (1, 2, false)
// Example: "func (*Writer) WriteError(cmd string, err *Error) error" -> (2, 1, false)
func parseSignatureCounts(sig string) (params, results int, variadic bool) {
	// Skip "func" prefix if present
	sig = strings.TrimPrefix(sig, "func")
	sig = strings.TrimSpace(sig)

	// Skip receiver if present (e.g., "(*Writer)" or "(T)")
	if len(sig) > 0 && sig[0] == '(' {
		recvEnd := findMatchingParen(sig, 0)
		if recvEnd > 0 {
			sig = strings.TrimSpace(sig[recvEnd+1:])
		}
	}

	// Skip function name (until next paren)
	parenStart := strings.Index(sig, "(")
	if parenStart < 0 {
		return 0, 0, false
	}

	// Find the matching closing paren
	paramEnd := findMatchingParen(sig, parenStart)
	if paramEnd < 0 {
		return 0, 0, false
	}

	paramStr := sig[parenStart+1 : paramEnd]
	params, variadic = countParams(paramStr)

	// Check for results after params
	rest := strings.TrimSpace(sig[paramEnd+1:])
	if rest == "" {
		return params, 0, variadic
	}

	// Results can be: "(Type, error)", "Type", or nothing
	if len(rest) > 0 && rest[0] == '(' {
		resultEnd := findMatchingParen(rest, 0)
		if resultEnd > 0 {
			resultStr := rest[1:resultEnd]
			results, _ = countParams(resultStr)
		}
	} else if len(rest) > 0 {
		// Single result (e.g., "error" or "*Type")
		results = 1
	}

	return params, results, variadic
}

// findMatchingParen finds the index of the closing paren matching the opening at start.
func findMatchingParen(s string, start int) int {
	if start >= len(s) || s[start] != '(' {
		return -1
	}
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// countParams counts parameters in a param list string.
// Handles cases like "a int, b int" (2 params) and "opts ...T" (variadic).
func countParams(paramStr string) (count int, variadic bool) {
	paramStr = strings.TrimSpace(paramStr)
	if paramStr == "" {
		return 0, false
	}

	// Check for variadic
	variadic = strings.Contains(paramStr, "...")

	// Split by comma, but be careful of generics and nested types
	// Simple heuristic: count commas at depth 0, add 1
	depth := 0
	commas := 0
	for _, ch := range paramStr {
		switch ch {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				commas++
			}
		}
	}

	return commas + 1, variadic
}

// GetRefCount returns the number of references to a symbol.
func GetRefCount(db *sql.DB, symbolID string) (int, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM refs WHERE symbol_id = ?`, symbolID).Scan(&count)
	return count, err
}

// isExported returns true if a Go identifier is exported (starts with uppercase).
func isExported(name string) bool {
	if name == "" {
		return false
	}
	r := rune(name[0])
	return r >= 'A' && r <= 'Z'
}

// ToCandidate converts a SymbolRow to an output.Candidate
func (s *SymbolRow) ToCandidate() output.Candidate {
	// Use relative path for output
	filePath := s.FilePathRel
	if filePath == "" {
		filePath = s.FilePath // Fallback to absolute if relative not available
	}
	// Extract a short doc snippet (first line, truncated to 80 chars)
	docSnippet := ""
	if s.Doc.Valid && s.Doc.String != "" {
		docSnippet = s.Doc.String
		if idx := strings.Index(docSnippet, "\n"); idx != -1 {
			docSnippet = docSnippet[:idx]
		}
		if len(docSnippet) > 80 {
			docSnippet = docSnippet[:77] + "..."
		}
	}
	receiver := ""
	if s.Receiver.Valid {
		receiver = s.Receiver.String
	}
	return output.Candidate{
		ID:       s.ID,
		Name:     s.Name,
		File:     filePath,
		Kind:     s.Kind,
		Receiver: receiver,
		Doc:      docSnippet,
	}
}

// FindSiblings finds other symbols of the same kind in the same file
func FindSiblings(db *sql.DB, filePath, kind, excludeID string, limit int) ([]output.Sibling, error) {
	rows, err := db.Query(`
		SELECT id, name, kind, line_start
		FROM symbols
		WHERE file_path = ? AND kind = ? AND id != ?
		ORDER BY line_start
		LIMIT ?
	`, filePath, kind, excludeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var siblings []output.Sibling
	for rows.Next() {
		var s output.Sibling
		if err := rows.Scan(&s.ID, &s.Name, &s.Kind, &s.Line); err != nil {
			return nil, err
		}
		siblings = append(siblings, s)
	}
	return siblings, rows.Err()
}

// CallRow represents a call graph edge with caller/callee details
type CallRow struct {
	CallerID        string
	CallerName      string
	CallerKind      string
	CallerFile      string // Absolute path
	CallerFileRel   string // Relative path
	CallerSignature sql.NullString
	CallerReceiver  string // Receiver type for caller (e.g., "(*Server)")
	CallerFileHash  string // Content hash for caller file
	CalleeID        string
	CalleeName      string
	CalleeKind      string
	CalleeFile      string // Absolute path
	CalleeFileRel   string // Relative path
	CalleeSignature sql.NullString
	CalleeReceiver  string // Receiver type for callee (e.g., "(*Store)")
	CalleeFileHash  string // Content hash for callee file
	// Callee definition coordinates (from symbols table)
	CalleeLineStart int
	CalleeColStart  int
	CalleeLineEnd   int
	CalleeColEnd    int
	// Call site coordinates (where the call expression appears in the caller)
	CallLine int
	CallCol  int
}

// FindCallers returns all functions that call the given symbol
func FindCallers(db *sql.DB, symbolID string, limit, offset int) ([]CallRow, error) {
	rows, err := db.Query(`
		SELECT
			cg.caller_id, caller.name, caller.kind, caller.file_path, caller.file_path_rel, caller.signature, caller.receiver, fc.hash,
			cg.callee_id, callee.name, callee.kind, callee.file_path, callee.file_path_rel, callee.signature, callee.receiver, fe.hash,
			callee.line_start, callee.col_start, callee.line_end, callee.col_end,
			cg.line, cg.col
		FROM call_graph cg
		JOIN symbols caller ON cg.caller_id = caller.id
		JOIN symbols callee ON cg.callee_id = callee.id
		LEFT JOIN files fc ON caller.file_path = fc.path
		LEFT JOIN files fe ON callee.file_path = fe.path
		WHERE cg.callee_id = ?
		ORDER BY caller.file_path, cg.line
		LIMIT ? OFFSET ?
	`, symbolID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanCallRows(rows)
}

// FindCallees returns all functions that the given symbol calls
func FindCallees(db *sql.DB, symbolID string, limit, offset int) ([]CallRow, error) {
	rows, err := db.Query(`
		SELECT
			cg.caller_id, caller.name, caller.kind, caller.file_path, caller.file_path_rel, caller.signature, caller.receiver, fc.hash,
			cg.callee_id, callee.name, callee.kind, callee.file_path, callee.file_path_rel, callee.signature, callee.receiver, fe.hash,
			callee.line_start, callee.col_start, callee.line_end, callee.col_end,
			cg.line, cg.col
		FROM call_graph cg
		JOIN symbols caller ON cg.caller_id = caller.id
		JOIN symbols callee ON cg.callee_id = callee.id
		LEFT JOIN files fc ON caller.file_path = fc.path
		LEFT JOIN files fe ON callee.file_path = fe.path
		WHERE cg.caller_id = ?
		ORDER BY cg.line, cg.col
		LIMIT ? OFFSET ?
	`, symbolID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanCallRows(rows)
}

// FindCallersForType aggregates callers across all methods of a type.
// Uses a subquery to find method IDs by receiver, avoiding a round-trip.
func FindCallersForType(db *sql.DB, typeName string, limit, offset int) ([]CallRow, error) {
	valueRecv := "(" + typeName + ")"
	ptrRecv := "(*" + typeName + ")"

	rows, err := db.Query(`
		SELECT
			cg.caller_id, caller.name, caller.kind, caller.file_path, caller.file_path_rel, caller.signature, caller.receiver, fc.hash,
			cg.callee_id, callee.name, callee.kind, callee.file_path, callee.file_path_rel, callee.signature, callee.receiver, fe.hash,
			callee.line_start, callee.col_start, callee.line_end, callee.col_end,
			cg.line, cg.col
		FROM call_graph cg
		JOIN symbols caller ON cg.caller_id = caller.id
		JOIN symbols callee ON cg.callee_id = callee.id
		LEFT JOIN files fc ON caller.file_path = fc.path
		LEFT JOIN files fe ON callee.file_path = fe.path
		WHERE cg.callee_id IN (
			SELECT id FROM symbols WHERE kind = 'method'
			AND (receiver = ? OR receiver = ?)
		)
		ORDER BY caller.file_path, cg.line
		LIMIT ? OFFSET ?
	`, valueRecv, ptrRecv, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanCallRows(rows)
}

// FindCalleesForType aggregates callees across all methods of a type.
func FindCalleesForType(db *sql.DB, typeName string, limit, offset int) ([]CallRow, error) {
	valueRecv := "(" + typeName + ")"
	ptrRecv := "(*" + typeName + ")"

	rows, err := db.Query(`
		SELECT
			cg.caller_id, caller.name, caller.kind, caller.file_path, caller.file_path_rel, caller.signature, caller.receiver, fc.hash,
			cg.callee_id, callee.name, callee.kind, callee.file_path, callee.file_path_rel, callee.signature, callee.receiver, fe.hash,
			callee.line_start, callee.col_start, callee.line_end, callee.col_end,
			cg.line, cg.col
		FROM call_graph cg
		JOIN symbols caller ON cg.caller_id = caller.id
		JOIN symbols callee ON cg.callee_id = callee.id
		LEFT JOIN files fc ON caller.file_path = fc.path
		LEFT JOIN files fe ON callee.file_path = fe.path
		WHERE cg.caller_id IN (
			SELECT id FROM symbols WHERE kind = 'method'
			AND (receiver = ? OR receiver = ?)
		)
		ORDER BY cg.line, cg.col
		LIMIT ? OFFSET ?
	`, valueRecv, ptrRecv, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanCallRows(rows)
}

// CountCallersForType returns total caller count across all methods of a type.
func CountCallersForType(db *sql.DB, typeName string) (int, error) {
	valueRecv := "(" + typeName + ")"
	ptrRecv := "(*" + typeName + ")"

	var count int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM call_graph
		WHERE callee_id IN (
			SELECT id FROM symbols WHERE kind = 'method'
			AND (receiver = ? OR receiver = ?)
		)
	`, valueRecv, ptrRecv).Scan(&count)
	return count, err
}

// CountCalleesForType returns total callee count across all methods of a type.
func CountCalleesForType(db *sql.DB, typeName string) (int, error) {
	valueRecv := "(" + typeName + ")"
	ptrRecv := "(*" + typeName + ")"

	var count int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM call_graph
		WHERE caller_id IN (
			SELECT id FROM symbols WHERE kind = 'method'
			AND (receiver = ? OR receiver = ?)
		)
	`, valueRecv, ptrRecv).Scan(&count)
	return count, err
}

// ToCalleeResult converts a CallRow to an output.Result describing the callee's definition.
// ID, file, range, and edit_target all point to where the callee is defined.
func (c *CallRow) ToCalleeResult() output.Result {
	calleeRange := output.Range{
		Start: output.Position{Line: c.CalleeLineStart, Col: c.CalleeColStart},
		End:   output.Position{Line: c.CalleeLineEnd, Col: c.CalleeColEnd},
	}
	filePath := c.CalleeFileRel
	if filePath == "" {
		filePath = c.CalleeFile
	}
	return output.Result{
		ID:         c.CalleeID,
		File:       filePath,
		FileAbs:    c.CalleeFile,
		Range:      calleeRange,
		Kind:       c.CalleeKind,
		Name:       c.CalleeName,
		Receiver:   c.CalleeReceiver,
		Match:      c.CalleeSignature.String,
		EditTarget: output.FormatEditTargetWithHash(filePath, c.CalleeFile, calleeRange),
	}
}

// ToCallerResult converts a CallRow to an output.Result describing the caller at the call site.
// ID, name, kind describe the caller; range points to the call expression location.
func (c *CallRow) ToCallerResult() output.Result {
	nameLen := len(c.CalleeName)
	if nameLen == 0 {
		nameLen = 1
	}
	callRange := output.Range{
		Start: output.Position{Line: c.CallLine, Col: c.CallCol},
		End:   output.Position{Line: c.CallLine, Col: c.CallCol + nameLen},
	}
	filePath := c.CallerFileRel
	if filePath == "" {
		filePath = c.CallerFile
	}
	return output.Result{
		ID:         c.CallerID,
		File:       filePath,
		FileAbs:    c.CallerFile,
		Range:      callRange,
		Kind:       c.CallerKind,
		Name:       c.CallerName,
		Receiver:   c.CallerReceiver,
		Match:      c.CallerSignature.String,
		EditTarget: output.FormatEditTargetWithHash(filePath, c.CallerFile, callRange),
	}
}

// scanCallRows scans rows into CallRow slices (shared by FindCallers/FindCallees/ForType variants).
func scanCallRows(rows *sql.Rows) ([]CallRow, error) {
	var results []CallRow
	for rows.Next() {
		var r CallRow
		var callerHash, calleeHash, callerFileRel, calleeFileRel, callerReceiver, calleeReceiver sql.NullString
		err := rows.Scan(
			&r.CallerID, &r.CallerName, &r.CallerKind, &r.CallerFile, &callerFileRel, &r.CallerSignature, &callerReceiver, &callerHash,
			&r.CalleeID, &r.CalleeName, &r.CalleeKind, &r.CalleeFile, &calleeFileRel, &r.CalleeSignature, &calleeReceiver, &calleeHash,
			&r.CalleeLineStart, &r.CalleeColStart, &r.CalleeLineEnd, &r.CalleeColEnd,
			&r.CallLine, &r.CallCol,
		)
		if err != nil {
			return nil, err
		}
		r.CallerFileRel = callerFileRel.String
		r.CalleeFileRel = calleeFileRel.String
		r.CallerReceiver = callerReceiver.String
		r.CalleeReceiver = calleeReceiver.String
		r.CallerFileHash = callerHash.String
		r.CalleeFileHash = calleeHash.String
		results = append(results, r)
	}
	return results, rows.Err()
}

// FindImplementers finds types that potentially implement an interface.
// Uses method-set matching: extracts the interface's method names from source,
// then finds types that have methods matching ALL of them (Go structural typing).
// Falls back to file co-occurrence heuristic if no interface methods are found.
func FindImplementers(db *sql.DB, interfaceID string, limit, offset int) ([]SymbolRow, error) {
	// Step 1: Get the interface symbol info
	var ifaceName, ifaceFile, ifacePkg string
	var ifaceLineStart, ifaceLineEnd int
	err := db.QueryRow(`SELECT name, file_path, pkg_path, line_start, line_end FROM symbols WHERE id = ?`,
		interfaceID).Scan(&ifaceName, &ifaceFile, &ifacePkg, &ifaceLineStart, &ifaceLineEnd)
	if err != nil {
		return nil, fmt.Errorf("lookup interface: %w", err)
	}

	// Step 2: Extract method names from the interface source body.
	// Go interface methods aren't stored as symbols with the interface as receiver,
	// so we read the source and parse method declarations.
	methodNames := ExtractInterfaceMethodNames(ifaceFile, ifaceLineStart, ifaceLineEnd)

	// If no methods found, fall back to file co-occurrence heuristic
	if len(methodNames) == 0 {
		return findImplementersByCooccurrence(db, interfaceID, limit, offset)
	}

	// Step 3: Find types that have methods matching ALL interface method names.
	// We look for types (struct/type) whose name appears as a receiver on methods
	// that match every interface method name.
	// Build placeholders for the IN clause
	placeholders := make([]string, len(methodNames))
	args := make([]interface{}, 0, len(methodNames)+3)
	for i, name := range methodNames {
		placeholders[i] = "?"
		args = append(args, name)
	}
	inClause := strings.Join(placeholders, ", ")
	methodCount := len(methodNames)
	args = append(args, methodCount, ifaceName, ifacePkg)

	// Find candidate types that have ALL required methods.
	// Group by (type_name, pkg_path) to avoid merging same-named types from different packages.
	// Exclude the interface itself by matching BOTH name and package (not just name,
	// since a struct in another package may share the interface's name).
	candidateQuery := fmt.Sprintf(`
		SELECT
		  CASE
		    WHEN m.receiver LIKE '(*%%' THEN SUBSTR(m.receiver, 3, LENGTH(m.receiver) - 3)
		    ELSE m.receiver
		  END AS type_name,
		  m.pkg_path
		FROM symbols m
		WHERE m.kind = 'method'
		  AND m.name IN (%s)
		GROUP BY type_name, m.pkg_path
		HAVING COUNT(DISTINCT m.name) >= ?
		  AND NOT (type_name = ? AND m.pkg_path = ?)
	`, inClause)

	candRows, err := db.Query(candidateQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("query candidate implementers: %w", err)
	}
	type candidate struct {
		name    string
		pkgPath string
	}
	var candidates []candidate
	for candRows.Next() {
		var c candidate
		if err := candRows.Scan(&c.name, &c.pkgPath); err != nil {
			candRows.Close()
			return nil, fmt.Errorf("scan candidate type: %w", err)
		}
		candidates = append(candidates, c)
	}
	candRows.Close()

	if err := candRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate candidate rows: %w", err)
	}

	if len(candidates) == 0 {
		return nil, nil
	}

	// Step 4: Fetch the full SymbolRow for each implementing type, matching by name AND pkg_path
	var conditions []string
	typeArgs := make([]interface{}, 0, len(candidates)*2+2)
	for _, c := range candidates {
		conditions = append(conditions, "(s.name = ? AND s.pkg_path = ?)")
		typeArgs = append(typeArgs, c.name, c.pkgPath)
	}
	whereClause := strings.Join(conditions, " OR ")
	typeArgs = append(typeArgs, limit, offset)

	rows, err := db.Query(fmt.Sprintf(`
		SELECT DISTINCT s.id, s.name, s.kind, s.file_path, s.file_path_rel, s.pkg_path, s.line_start, s.col_start, s.line_end, s.col_end,
		       s.signature, s.doc, s.receiver, f.hash
		FROM symbols s
		LEFT JOIN files f ON s.file_path = f.path
		WHERE (%s)
		  AND s.kind IN ('struct', 'type')
		ORDER BY s.file_path, s.name
		LIMIT ? OFFSET ?
	`, whereClause), typeArgs...)
	if err != nil {
		return nil, fmt.Errorf("query implementer symbols: %w", err)
	}
	defer rows.Close()

	return scanSymbolRows(rows)
}

// findImplementersByCooccurrence is the fallback heuristic: finds struct/type
// symbols in files that reference the interface.
func findImplementersByCooccurrence(db *sql.DB, interfaceID string, limit, offset int) ([]SymbolRow, error) {
	rows, err := db.Query(`
		SELECT DISTINCT s.id, s.name, s.kind, s.file_path, s.file_path_rel, s.pkg_path, s.line_start, s.col_start, s.line_end, s.col_end,
		       s.signature, s.doc, s.receiver, f.hash
		FROM symbols s
		LEFT JOIN files f ON s.file_path = f.path
		WHERE s.kind IN ('struct', 'type')
		  AND EXISTS (
		    SELECT 1 FROM refs r
		    WHERE r.symbol_id = ?
		      AND r.file_path = s.file_path
		  )
		ORDER BY s.file_path, s.name
		LIMIT ? OFFSET ?
	`, interfaceID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("query implementers by cooccurrence: %w", err)
	}
	defer rows.Close()

	return scanSymbolRows(rows)
}

// interfaceMethodRe matches Go interface method declarations.
// Matches lines like: "  Name() string" or "  Complete(ctx context.Context, req Request) (*Response, error)"
var interfaceMethodRe = regexp.MustCompile(`^\s+([A-Z]\w*)\s*\(`)

// ExtractInterfaceMethodNames reads the interface body from source and extracts
// exported method names. Returns nil if file can't be read or has no methods.
func ExtractInterfaceMethodNames(filePath string, lineStart, lineEnd int) []string {
	f, err := os.Open(filePath)
	if err != nil {
		return nil
	}
	defer f.Close()

	var methods []string
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if lineNum <= lineStart || lineNum >= lineEnd {
			continue
		}
		line := scanner.Text()
		// Skip comments and embedded interfaces
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") {
			continue
		}
		// Match method declaration: starts with uppercase letter followed by '('
		if m := interfaceMethodRe.FindStringSubmatch(line); m != nil {
			methods = append(methods, m[1])
		}
	}
	return methods
}

// FindPackageSymbols finds all exported symbols in files matching a package path pattern.
// It filters to exported symbols only (those starting with uppercase).
// Cascades: exact → suffix → substring, returning on first match to avoid leading-wildcard LIKE.
func FindPackageSymbols(db *sql.DB, pkgPattern string, limit, offset int) ([]SymbolRow, error) {
	const selectCols = `
		SELECT s.id, s.name, s.kind, s.file_path, s.file_path_rel, s.pkg_path, s.line_start, s.col_start, s.line_end, s.col_end,
		       s.signature, s.doc, s.receiver, f.hash
		FROM symbols s
		LEFT JOIN files f ON s.file_path = f.path`
	const filterAndOrder = `
		  AND s.name GLOB '[A-Z]*'
		  AND s.kind NOT IN ('field')
		ORDER BY
		  CASE s.kind
		    WHEN 'interface' THEN 1
		    WHEN 'struct'    THEN 2
		    WHEN 'type'      THEN 3
		    WHEN 'method'    THEN 4
		    WHEN 'func'      THEN 5
		    WHEN 'const'     THEN 6
		    WHEN 'var'       THEN 7
		    ELSE 8
		  END,
		  s.name
		LIMIT ? OFFSET ?`

	// Try exact match first (index-friendly).
	rows, err := db.Query(selectCols+`
		WHERE s.pkg_path = ?`+filterAndOrder, pkgPattern, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("query package symbols: %w", err)
	}
	results, err := scanSymbolRows(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(results) > 0 {
		return results, nil
	}

	// Suffix match (e.g., "internal/handler" matching "github.com/.../internal/handler").
	suffixPattern := "%/" + pkgPattern
	rows, err = db.Query(selectCols+`
		WHERE s.pkg_path LIKE ?`+filterAndOrder, suffixPattern, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("query package symbols suffix: %w", err)
	}
	results, err = scanSymbolRows(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(results) > 0 {
		return results, nil
	}

	// Substring match as last resort.
	substringPattern := "%" + pkgPattern + "%"
	rows, err = db.Query(selectCols+`
		WHERE s.pkg_path LIKE ?`+filterAndOrder, substringPattern, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("query package symbols substring: %w", err)
	}
	defer rows.Close()

	return scanSymbolRows(rows)
}

// FindSymbolAtPosition looks up a symbol by relative file path and line number.
// Uses range containment (line_start <= line <= line_end) so that rg hits inside
// a function body resolve to the enclosing symbol. Prefers the narrowest match.
// Returns nil if no match. Used to enrich search results with index metadata.
func FindSymbolAtPosition(db *sql.DB, filePathRel string, line int) *SymbolRow {
	var s SymbolRow
	var fileHash, relPath, pkgPath sql.NullString
	err := db.QueryRow(`
		SELECT s.id, s.name, s.kind, s.file_path, s.file_path_rel, s.pkg_path,
		       s.line_start, s.col_start, s.line_end, s.col_end,
		       s.signature, s.doc, s.receiver, f.hash
		FROM symbols s
		LEFT JOIN files f ON s.file_path = f.path
		WHERE s.file_path_rel = ? AND s.line_start <= ? AND s.line_end >= ?
		ORDER BY (s.line_end - s.line_start) ASC
		LIMIT 1
	`, filePathRel, line, line).Scan(&s.ID, &s.Name, &s.Kind, &s.FilePath, &relPath, &pkgPath,
		&s.LineStart, &s.ColStart, &s.LineEnd, &s.ColEnd,
		&s.Signature, &s.Doc, &s.Receiver, &fileHash)
	if err != nil {
		return nil
	}
	s.FileHash = fileHash.String
	s.FilePathRel = relPath.String
	s.PkgPath = pkgPath.String
	return &s
}

// GetCallersPreview returns a preview of top N callers for a symbol.
// Used for quick caller context without full call graph traversal.
func GetCallersPreview(db *sql.DB, symbolID string, limit int) ([]output.CallerPreview, error) {
	rows, err := db.Query(`
		SELECT caller.id, caller.name, caller.file_path_rel, cg.line
		FROM call_graph cg
		JOIN symbols caller ON cg.caller_id = caller.id
		WHERE cg.callee_id = ?
		ORDER BY caller.file_path_rel, cg.line
		LIMIT ?
	`, symbolID, limit)
	if err != nil {
		return nil, fmt.Errorf("query callers preview: %w", err)
	}
	defer rows.Close()

	var callers []output.CallerPreview
	for rows.Next() {
		var c output.CallerPreview
		var fileRel sql.NullString
		if err := rows.Scan(&c.ID, &c.Name, &fileRel, &c.Line); err != nil {
			return nil, fmt.Errorf("scan caller preview: %w", err)
		}
		c.File = fileRel.String
		callers = append(callers, c)
	}
	return callers, rows.Err()
}
