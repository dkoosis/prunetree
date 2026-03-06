package query

import (
	"database/sql"
)

// ImpactRow represents a symbol in the impact blast radius.
// Reuses the same fields as TestRow — same scan pattern (15 columns including hop).
type ImpactRow = TestRow

// FindImpactCallers returns non-test callers of a symbol with hop distance.
// When direct=true, only 1-hop callers. Otherwise includes 2-hop transitive.
// Test files (*_test.go) are excluded — use FindTests() for test coverage.
func FindImpactCallers(db *sql.DB, symbolID string, direct bool, limit, offset int) ([]ImpactRow, error) {
	var q string
	if direct {
		q = `
			SELECT s.id, s.name, s.kind, s.file_path, s.file_path_rel,
			       s.pkg_path, s.line_start, s.col_start, s.line_end, s.col_end,
			       s.signature, s.doc, s.receiver, f.hash,
			       1 AS hop
			FROM call_graph cg
			JOIN symbols s ON s.id = cg.caller_id
			LEFT JOIN files f ON s.file_path = f.path
			WHERE cg.callee_id = ?
			  AND s.file_path NOT GLOB '*_test.go'
			ORDER BY s.file_path, s.name
			LIMIT ? OFFSET ?`
	} else {
		q = `
			WITH direct_callers AS (
				SELECT s.id, s.name, s.kind, s.file_path, s.file_path_rel,
				       s.pkg_path, s.line_start, s.col_start, s.line_end, s.col_end,
				       s.signature, s.doc, s.receiver, f.hash,
				       1 AS hop
				FROM call_graph cg
				JOIN symbols s ON s.id = cg.caller_id
				LEFT JOIN files f ON s.file_path = f.path
				WHERE cg.callee_id = ?
				  AND s.file_path NOT GLOB '*_test.go'
			),
			transitive_callers AS (
				SELECT ts.id, ts.name, ts.kind, ts.file_path, ts.file_path_rel,
				       ts.pkg_path, ts.line_start, ts.col_start, ts.line_end, ts.col_end,
				       ts.signature, ts.doc, ts.receiver, f.hash,
				       2 AS hop
				FROM call_graph cg1
				JOIN call_graph cg2 ON cg2.callee_id = cg1.caller_id
				JOIN symbols ts ON ts.id = cg2.caller_id
				LEFT JOIN files f ON ts.file_path = f.path
				WHERE cg1.callee_id = ?
				  AND ts.file_path NOT GLOB '*_test.go'
				  AND ts.id NOT IN (SELECT id FROM direct_callers)
			)
			SELECT * FROM direct_callers
			UNION ALL
			SELECT * FROM transitive_callers
			ORDER BY hop, file_path, name
			LIMIT ? OFFSET ?`
	}

	var rows *sql.Rows
	var err error
	if direct {
		rows, err = db.Query(q, symbolID, limit, offset)
	} else {
		rows, err = db.Query(q, symbolID, symbolID, limit, offset)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanTestRows(rows)
}
