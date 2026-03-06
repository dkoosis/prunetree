package query

import (
	"database/sql"
	"strings"
)

// ImportRow represents an import record from the database
type ImportRow struct {
	FilePath    string
	PkgPath     string
	Name        sql.NullString
	Line        int
	Col         int
	ImporterPkg sql.NullString
}

// FindImports returns all imports for a given file
func FindImports(db *sql.DB, filePath string, limit, offset int) ([]ImportRow, error) {
	rows, err := db.Query(`
		SELECT file_path, pkg_path, name, line, col, importer_pkg
		FROM imports
		WHERE file_path = ?
		ORDER BY line
		LIMIT ? OFFSET ?
	`, filePath, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanImportRows(rows)
}

// FindImportsByPackage returns all imports of a given package path
// This is used for the "importers" command - who imports this package
func FindImportsByPackage(db *sql.DB, pkgPath string, limit, offset int) ([]ImportRow, error) {
	rows, err := db.Query(`
		SELECT file_path, pkg_path, name, line, col, importer_pkg
		FROM imports
		WHERE pkg_path = ? OR pkg_path LIKE ?
		ORDER BY file_path, line
		LIMIT ? OFFSET ?
	`, pkgPath, "%/"+pkgPath, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanImportRows(rows)
}

// FindImportersByDirectory returns files that import any package within a directory
// For "snipe importers internal/handler" - find who imports anything in that dir
func FindImportersByDirectory(db *sql.DB, dirPath string, limit, offset int) ([]ImportRow, error) {
	// Boundary-aware matching: match dirPath as a complete path segment
	// "internal/handler" matches ".../internal/handler" and ".../internal/handler/sub"
	// but NOT ".../internal/handler2"
	dirPath = strings.TrimPrefix(dirPath, "/")
	suffixMatch := "%/" + dirPath         // ends with /dirPath (exact package)
	subpkgSuffix := "%/" + dirPath + "/%" // subpackages: .../dirPath/...
	subpkgExact := dirPath + "/%"         // root-relative subpackages

	rows, err := db.Query(`
		SELECT file_path, pkg_path, name, line, col, importer_pkg
		FROM imports
		WHERE pkg_path = ? OR pkg_path LIKE ?
		   OR pkg_path LIKE ? OR pkg_path LIKE ?
		ORDER BY file_path, line
		LIMIT ? OFFSET ?
	`, dirPath, suffixMatch, subpkgSuffix, subpkgExact, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanImportRows(rows)
}

func scanImportRows(rows *sql.Rows) ([]ImportRow, error) {
	var imports []ImportRow
	for rows.Next() {
		var imp ImportRow
		err := rows.Scan(&imp.FilePath, &imp.PkgPath, &imp.Name, &imp.Line, &imp.Col, &imp.ImporterPkg)
		if err != nil {
			return nil, err
		}
		imports = append(imports, imp)
	}
	return imports, rows.Err()
}
