package query

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Note: strings is still used in ParsePosition for Join and Split

// PositionQuery represents a file:line:col query
type PositionQuery struct {
	File string
	Line int
	Col  int
}

// ParsePosition parses a position string like "file.go:42:12"
func ParsePosition(s string) (*PositionQuery, error) {
	parts := strings.Split(s, ":")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid position format: %s (expected file:line or file:line:col)", s)
	}

	line, err := strconv.Atoi(parts[len(parts)-2])
	if err != nil {
		// Might be file:line format
		if len(parts) == 2 {
			line, err = strconv.Atoi(parts[1])
			if err != nil {
				return nil, fmt.Errorf("invalid line number: %s", parts[1])
			}
			return &PositionQuery{
				File: parts[0],
				Line: line,
				Col:  1,
			}, nil
		}
		if len(parts) >= 2 {
			line, err = strconv.Atoi(parts[len(parts)-1])
			if err == nil {
				return &PositionQuery{
					File: strings.Join(parts[:len(parts)-1], ":"),
					Line: line,
					Col:  1,
				}, nil
			}
		}
		return nil, fmt.Errorf("invalid line number: %s", parts[len(parts)-2])
	}

	col := 1
	if len(parts) >= 3 {
		col, err = strconv.Atoi(parts[len(parts)-1])
		if err != nil {
			return nil, fmt.Errorf("invalid column number: %s", parts[len(parts)-1])
		}
		// Reconstruct file path (might contain colons on Windows)
		return &PositionQuery{
			File: strings.Join(parts[:len(parts)-2], ":"),
			Line: line,
			Col:  col,
		}, nil
	}

	return &PositionQuery{
		File: strings.Join(parts[:len(parts)-1], ":"),
		Line: line,
		Col:  col,
	}, nil
}

// ResolvePosition finds the symbol at or near the given position.
// Note: pos.File should be an absolute path for optimal index usage.
// Callers should resolve relative paths before calling this function.
func ResolvePosition(db *sql.DB, pos *PositionQuery) (symbolID string, err error) {
	// First, try to find a symbol definition at exactly this position
	// Check the identifier position (name_line/name_col) which is where users typically click
	// This handles clicking directly on the symbol name in a definition
	err = db.QueryRow(`
		SELECT id FROM symbols
		WHERE file_path = ? AND name_line = ? AND name_col = ?
		LIMIT 1
	`, pos.File, pos.Line, pos.Col).Scan(&symbolID)

	if err == nil {
		return symbolID, nil
	}

	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("query symbols by name position: %w", err)
	}

	// Try to find a reference at exactly this position
	// Uses idx_refs_position composite index for O(1) lookup
	err = db.QueryRow(`
		SELECT symbol_id FROM refs
		WHERE file_path = ? AND line = ? AND col = ?
		LIMIT 1
	`, pos.File, pos.Line, pos.Col).Scan(&symbolID)

	if err == nil {
		return symbolID, nil
	}

	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("query refs: %w", err)
	}

	// Try to find a symbol whose identifier is on this line, closest to the column
	err = db.QueryRow(`
		SELECT id FROM symbols
		WHERE file_path = ? AND name_line = ?
		ORDER BY ABS(name_col - ?)
		LIMIT 1
	`, pos.File, pos.Line, pos.Col).Scan(&symbolID)

	if err == nil {
		return symbolID, nil
	}

	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("query symbols by name line: %w", err)
	}

	// Try to find a reference on this line, closest to the column
	err = db.QueryRow(`
		SELECT symbol_id FROM refs
		WHERE file_path = ? AND line = ?
		ORDER BY ABS(col - ?)
		LIMIT 1
	`, pos.File, pos.Line, pos.Col).Scan(&symbolID)

	if err == nil {
		return symbolID, nil
	}

	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("query refs by line: %w", err)
	}

	// Try to find a symbol definition whose display range contains this position
	// Uses idx_symbols_position composite index
	err = db.QueryRow(`
		SELECT id FROM symbols
		WHERE file_path = ? AND line_start = ? AND col_start <= ? AND col_end >= ?
		LIMIT 1
	`, pos.File, pos.Line, pos.Col, pos.Col).Scan(&symbolID)

	if err == nil {
		return symbolID, nil
	}

	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("query symbols: %w", err)
	}

	// Try to find a symbol that spans this line (smallest enclosing)
	// Secondary sort by name, id ensures deterministic results for same-size spans
	err = db.QueryRow(`
		SELECT id FROM symbols
		WHERE file_path = ? AND line_start <= ? AND line_end >= ?
		ORDER BY (line_end - line_start), name, id
		LIMIT 1
	`, pos.File, pos.Line, pos.Line).Scan(&symbolID)

	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("no symbol found at %s:%d:%d", pos.File, pos.Line, pos.Col)
	}

	return symbolID, err
}
