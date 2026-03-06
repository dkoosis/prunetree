package query

import (
	"database/sql"
	"fmt"

	"github.com/dkoosis/snipe/internal/output"
)

// TestRow represents a test function that exercises a target symbol.
type TestRow struct {
	ID          string
	Name        string
	Kind        string
	FilePath    string // Absolute path
	FilePathRel string // Relative path
	PkgPath     string
	LineStart   int
	ColStart    int
	LineEnd     int
	ColEnd      int
	Signature   sql.NullString
	Doc         sql.NullString
	Receiver    sql.NullString
	FileHash    string
	Hop         int // 1=direct, 2=transitive
}

// testFuncFilterFor returns the SQL predicate for Go test function names,
// qualified with the given table alias (e.g. "s" or "ts").
func testFuncFilterFor(alias string) string {
	return fmt.Sprintf(
		"(%s.name GLOB 'Test*' OR %s.name GLOB 'Benchmark*' OR %s.name GLOB 'Fuzz*' OR %s.name GLOB 'Example*')",
		alias, alias, alias, alias,
	)
}

// FindTests returns test functions that exercise the given symbol.
// When direct=true, only 1-hop callers are returned.
// When direct=false (default), also returns 2-hop transitive callers (Test* -> helper -> symbol).
func FindTests(db *sql.DB, symbolID string, direct bool, limit, offset int) ([]TestRow, error) {
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
			  AND s.file_path GLOB '*_test.go'
			  AND ` + testFuncFilterFor("s") + `
			ORDER BY s.file_path, s.name
			LIMIT ? OFFSET ?`
	} else {
		q = `
			WITH direct_tests AS (
				SELECT s.id, s.name, s.kind, s.file_path, s.file_path_rel,
				       s.pkg_path, s.line_start, s.col_start, s.line_end, s.col_end,
				       s.signature, s.doc, s.receiver, f.hash,
				       1 AS hop
				FROM call_graph cg
				JOIN symbols s ON s.id = cg.caller_id
				LEFT JOIN files f ON s.file_path = f.path
				WHERE cg.callee_id = ?
				  AND s.file_path GLOB '*_test.go'
				  AND ` + testFuncFilterFor("s") + `
			),
			transitive_tests AS (
				SELECT ts.id, ts.name, ts.kind, ts.file_path, ts.file_path_rel,
				       ts.pkg_path, ts.line_start, ts.col_start, ts.line_end, ts.col_end,
				       ts.signature, ts.doc, ts.receiver, f.hash,
				       2 AS hop
				FROM call_graph cg1
				JOIN call_graph cg2 ON cg2.callee_id = cg1.caller_id
				JOIN symbols ts ON ts.id = cg2.caller_id
				LEFT JOIN files f ON ts.file_path = f.path
				WHERE cg1.callee_id = ?
				  AND ts.file_path GLOB '*_test.go'
				  AND ` + testFuncFilterFor("ts") + `
				  AND ts.id NOT IN (SELECT id FROM direct_tests)
			)
			SELECT * FROM direct_tests
			UNION ALL
			SELECT * FROM transitive_tests
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

// scanTestRows scans rows into TestRow slices.
func scanTestRows(rows *sql.Rows) ([]TestRow, error) {
	var results []TestRow
	for rows.Next() {
		var r TestRow
		var filePathRel, pkgPath, fileHash sql.NullString
		err := rows.Scan(
			&r.ID, &r.Name, &r.Kind, &r.FilePath, &filePathRel,
			&pkgPath, &r.LineStart, &r.ColStart, &r.LineEnd, &r.ColEnd,
			&r.Signature, &r.Doc, &r.Receiver, &fileHash,
			&r.Hop,
		)
		if err != nil {
			return nil, err
		}
		r.FilePathRel = filePathRel.String
		r.PkgPath = pkgPath.String
		r.FileHash = fileHash.String
		results = append(results, r)
	}
	return results, rows.Err()
}

// ToResult converts a TestRow to an output.Result.
func (r *TestRow) ToResult() output.Result {
	filePath := r.FilePathRel
	if filePath == "" {
		filePath = r.FilePath
	}
	defRange := output.Range{
		Start: output.Position{Line: r.LineStart, Col: r.ColStart},
		End:   output.Position{Line: r.LineEnd, Col: r.ColEnd},
	}
	return output.Result{
		ID:         r.ID,
		File:       filePath,
		FileAbs:    r.FilePath,
		Range:      defRange,
		Kind:       r.Kind,
		Name:       r.Name,
		Receiver:   r.Receiver.String,
		Package:    r.PkgPath,
		Match:      r.Signature.String,
		EditTarget: output.FormatEditTargetWithHash(filePath, r.FilePath, defRange),
	}
}
