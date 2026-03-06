package context

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/dkoosis/snipe/internal/query"
)

// GetInterfaceMap finds non-trivial interfaces in the repo and their implementors.
// Non-trivial means 2+ methods OR 2+ implementors.
func GetInterfaceMap(db *sql.DB, repoRoot string) ([]InterfaceEntry, error) {
	ifaces, err := queryRepoInterfaces(db, repoRoot)
	if err != nil {
		return nil, fmt.Errorf("query interfaces: %w", err)
	}
	if len(ifaces) == 0 {
		return nil, nil
	}

	// Read method names from source for each interface
	for i := range ifaces {
		ifaces[i].methods = query.ExtractInterfaceMethodNames(
			ifaces[i].filePath, ifaces[i].lineStart, ifaces[i].lineEnd,
		)
	}

	// Find implementors for each interface
	implMap := queryImplementorsBatch(db, repoRoot, ifaces)

	// Build final list with non-trivial threshold
	var entries []InterfaceEntry
	for _, iface := range ifaces {
		impls := implMap[iface.id]
		methodCount := len(iface.methods)
		implCount := len(impls)

		// Non-trivial: 2+ methods OR 2+ implementors
		if methodCount < 2 && implCount < 2 {
			continue
		}
		if implCount == 0 {
			continue
		}

		entry := InterfaceEntry{
			Interface:    iface.name,
			File:         strings.TrimPrefix(iface.filePath, repoRoot+"/"),
			Line:         iface.lineStart,
			Methods:      iface.methods,
			Implementors: impls,
		}
		entries = append(entries, entry)
	}

	if len(entries) > 15 {
		entries = entries[:15]
	}

	return entries, nil
}

type ifaceInfo struct {
	id        string
	name      string
	filePath  string
	lineStart int
	lineEnd   int
	methods   []string
}

func queryRepoInterfaces(db *sql.DB, repoRoot string) ([]ifaceInfo, error) {
	rows, err := db.Query(`
		SELECT id, name, file_path, line_start, line_end
		FROM symbols
		WHERE kind = 'interface'
		  AND file_path LIKE ? || '/%'
		  AND name GLOB '[A-Z]*'
		  AND file_path NOT LIKE '%_test.go'
		ORDER BY name
		LIMIT 50
	`, repoRoot)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ifaces []ifaceInfo
	for rows.Next() {
		var iface ifaceInfo
		if err := rows.Scan(&iface.id, &iface.name, &iface.filePath, &iface.lineStart, &iface.lineEnd); err != nil {
			continue
		}
		ifaces = append(ifaces, iface)
	}
	return ifaces, rows.Err()
}

func queryImplementorsBatch(db *sql.DB, repoRoot string, ifaces []ifaceInfo) map[string][]ImplementorRef {
	result := make(map[string][]ImplementorRef, len(ifaces))

	for _, iface := range ifaces {
		if len(iface.methods) == 0 {
			impls, err := queryImplementorsByCooccurrence(db, repoRoot, iface.id)
			if err == nil {
				result[iface.id] = impls
			}
			continue
		}

		impls, err := queryImplementorsForInterface(db, repoRoot, iface)
		if err != nil {
			continue
		}
		result[iface.id] = impls
	}

	return result
}

func queryImplementorsForInterface(db *sql.DB, repoRoot string, iface ifaceInfo) ([]ImplementorRef, error) {
	placeholders := make([]string, len(iface.methods))
	args := make([]interface{}, 0, len(iface.methods)+3)
	for i, name := range iface.methods {
		placeholders[i] = "?"
		args = append(args, name)
	}
	args = append(args, repoRoot, len(iface.methods), iface.name)

	// #nosec G201 -- placeholders are positional parameters
	candidateQuery := fmt.Sprintf(`
		SELECT
		  CASE
		    WHEN m.receiver LIKE '(*%%' THEN SUBSTR(m.receiver, 3, LENGTH(m.receiver) - 3)
		    ELSE TRIM(m.receiver, '()')
		  END AS type_name,
		  m.pkg_path
		FROM symbols m
		WHERE m.kind = 'method'
		  AND m.name IN (%s)
		  AND m.file_path LIKE ? || '/%%'
		GROUP BY type_name, m.pkg_path
		HAVING COUNT(DISTINCT m.name) >= ?
		  AND type_name != ?
	`, strings.Join(placeholders, ", "))

	candRows, err := db.Query(candidateQuery, args...)
	if err != nil {
		return nil, err
	}
	defer candRows.Close()

	type candidate struct {
		name    string
		pkgPath string
	}
	var candidates []candidate
	for candRows.Next() {
		var c candidate
		if err := candRows.Scan(&c.name, &c.pkgPath); err != nil {
			continue
		}
		candidates = append(candidates, c)
	}

	if len(candidates) == 0 {
		return nil, nil
	}

	var conditions []string
	typeArgs := make([]interface{}, 0, len(candidates)*2+1)
	for _, c := range candidates {
		conditions = append(conditions, "(s.name = ? AND s.pkg_path = ?)")
		typeArgs = append(typeArgs, c.name, c.pkgPath)
	}
	typeArgs = append(typeArgs, repoRoot)

	// #nosec G201 -- conditions built from literal templates
	rows, err := db.Query(fmt.Sprintf(`
		SELECT DISTINCT s.name, s.file_path, s.line_start
		FROM symbols s
		WHERE (%s)
		  AND s.kind IN ('struct', 'type')
		  AND s.file_path LIKE ? || '/%%'
		ORDER BY s.name
		LIMIT 20
	`, strings.Join(conditions, " OR ")), typeArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanImplementorRefs(rows, repoRoot)
}

func queryImplementorsByCooccurrence(db *sql.DB, repoRoot, interfaceID string) ([]ImplementorRef, error) {
	rows, err := db.Query(`
		SELECT DISTINCT s.name, s.file_path, s.line_start
		FROM symbols s
		WHERE s.kind IN ('struct', 'type')
		  AND s.file_path LIKE ? || '/%'
		  AND EXISTS (
		    SELECT 1 FROM refs r
		    WHERE r.symbol_id = ?
		      AND r.file_path = s.file_path
		  )
		ORDER BY s.name
		LIMIT 10
	`, repoRoot, interfaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanImplementorRefs(rows, repoRoot)
}

// scanImplementorRefs scans rows of (name, file_path, line_start) into ImplementorRef slices.
func scanImplementorRefs(rows *sql.Rows, repoRoot string) ([]ImplementorRef, error) {
	var refs []ImplementorRef
	for rows.Next() {
		var ref ImplementorRef
		var filePath string
		if err := rows.Scan(&ref.Name, &filePath, &ref.Line); err != nil {
			continue
		}
		ref.File = strings.TrimPrefix(filePath, repoRoot+"/")
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}
