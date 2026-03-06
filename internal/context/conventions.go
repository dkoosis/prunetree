package context

import (
	"database/sql"
	"path/filepath"
	"strings"
)

// DetectConventions analyzes the snipe index to detect coding conventions.
// Returns nil fields for any category with zero samples.
func DetectConventions(db *sql.DB, repoRoot string) *Conventions {
	_ = repoRoot // reserved for future filesystem-based detection
	return &Conventions{
		Constructors:  detectConstructors(db),
		Receivers:     detectReceivers(db),
		Testing:       detectTesting(db),
		Interfaces:    detectInterfaces(db),
		ErrorHandling: detectErrors(db),
		FileOrg:       detectFileOrg(db),
	}
}

const (
	confidenceHigh   = "high"
	confidenceMedium = "medium"
	confidenceLow    = "low"
)

// confidence returns "high", "medium", or "low" based on consistency ratio and sample size.
func confidence(ratio float64, sampleSize int) string {
	switch {
	case ratio >= 0.8 && sampleSize >= 3:
		return confidenceHigh
	case ratio >= 0.6 && sampleSize >= 3:
		return confidenceMedium
	default:
		return confidenceLow
	}
}

// detectConstructors finds New* functions and classifies their return style.
func detectConstructors(db *sql.DB) *ConstructorConvention {
	rows, err := db.Query(`
		SELECT signature FROM symbols
		WHERE kind = 'func' AND name LIKE 'New%'
	`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var total, withErr, withoutErr int
	for rows.Next() {
		var sig string
		if err := rows.Scan(&sig); err != nil {
			continue
		}
		total++
		if strings.HasSuffix(sig, "error)") {
			withErr++
		} else {
			withoutErr++
		}
	}
	if err := rows.Err(); err != nil {
		return nil
	}

	if total == 0 {
		return nil
	}

	ratio := float64(max(withErr, withoutErr)) / float64(total)
	pattern := "New* constructors return (T, error)"
	if withoutErr > withErr {
		pattern = "New* constructors return T (no error)"
	}

	return &ConstructorConvention{
		Pattern:    pattern,
		Confidence: confidence(ratio, total),
		Total:      total,
		WithError:  withErr,
		WithoutErr: withoutErr,
	}
}

// detectReceivers analyzes method receiver naming patterns.
func detectReceivers(db *sql.DB) *ReceiverConvention {
	rows, err := db.Query(`
		SELECT receiver FROM symbols
		WHERE kind = 'method' AND receiver != ''
	`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var total, singleLetter, descriptive, pointer int
	for rows.Next() {
		var recv string
		if err := rows.Scan(&recv); err != nil {
			continue
		}
		total++

		if strings.Contains(recv, "*") {
			pointer++
		}

		// Strip ( ) and *
		name := strings.Trim(recv, "()")
		name = strings.TrimPrefix(name, "*")

		if len(name) == 1 {
			singleLetter++
		} else {
			descriptive++
		}
	}
	if err := rows.Err(); err != nil {
		return nil
	}

	if total == 0 {
		return nil
	}

	ratio := float64(max(singleLetter, descriptive)) / float64(total)
	pattern := "single-letter receivers"
	if descriptive > singleLetter {
		pattern = "descriptive receivers"
	}

	return &ReceiverConvention{
		Pattern:      pattern,
		Confidence:   confidence(ratio, total),
		Total:        total,
		SingleLetter: singleLetter,
		Descriptive:  descriptive,
		PointerPct:   float64(pointer) / float64(total) * 100,
	}
}

// detectTesting analyzes test file organization patterns.
func detectTesting(db *sql.DB) *TestConvention {
	// Get distinct test files
	rows, err := db.Query(`
		SELECT DISTINCT file_path FROM symbols
		WHERE file_path LIKE '%_test.go'
	`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var testFiles []string
	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err != nil {
			continue
		}
		testFiles = append(testFiles, fp)
	}
	if err := rows.Err(); err != nil {
		return nil
	}

	if len(testFiles) == 0 {
		return nil
	}

	// Collect directories containing non-test .go source files (single query).
	srcDirs := make(map[string]bool)
	srcRows, err := db.Query(`
		SELECT DISTINCT file_path FROM symbols
		WHERE file_path LIKE '%.go'
		  AND file_path NOT LIKE '%_test.go'
	`)
	if err == nil {
		defer srcRows.Close()
		for srcRows.Next() {
			var fp string
			if err := srcRows.Scan(&fp); err != nil {
				continue
			}
			srcDirs[filepath.Dir(fp)] = true
		}
	}

	// Check colocation: does a non-test .go file exist in the same directory?
	var colocated, separate int
	for _, tf := range testFiles {
		if srcDirs[filepath.Dir(tf)] {
			colocated++
		} else {
			separate++
		}
	}

	// Count test helpers: unexported funcs in test files
	var helpers int
	err = db.QueryRow(`
		SELECT COUNT(*) FROM symbols
		WHERE file_path LIKE '%_test.go'
		  AND kind = 'func'
		  AND name GLOB '[a-z]*'
	`).Scan(&helpers)
	if err != nil {
		helpers = 0
	}

	totalFiles := len(testFiles)
	ratio := float64(max(colocated, separate)) / float64(totalFiles)
	pattern := "colocated test files"
	if separate > colocated {
		pattern = "separate test directory"
	}

	return &TestConvention{
		Pattern:    pattern,
		Confidence: confidence(ratio, totalFiles),
		TestFiles:  totalFiles,
		Colocated:  colocated,
		Separate:   separate,
		Helpers:    helpers,
	}
}

// detectInterfaces analyzes interface naming patterns.
func detectInterfaces(db *sql.DB) *InterfaceConvention {
	rows, err := db.Query(`
		SELECT name FROM symbols
		WHERE kind = 'interface' AND name GLOB '[A-Z]*'
	`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var total, erSuffix int
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		total++
		lower := strings.ToLower(name)
		if strings.HasSuffix(lower, "er") || strings.HasSuffix(lower, "or") {
			erSuffix++
		}
	}
	if err := rows.Err(); err != nil {
		return nil
	}

	if total == 0 {
		return nil
	}

	ratio := float64(max(erSuffix, total-erSuffix)) / float64(total)
	pattern := "-er/-or suffix naming"
	if total-erSuffix > erSuffix {
		pattern = "noun-based naming"
	}

	return &InterfaceConvention{
		Pattern:    pattern,
		Confidence: confidence(ratio, total),
		Total:      total,
		ErSuffix:   erSuffix,
	}
}

// detectErrors analyzes error handling patterns (sentinels vs inline).
func detectErrors(db *sql.DB) *ErrorConvention {
	var sentinels int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM symbols
		WHERE kind = 'var' AND name LIKE 'Err%'
	`).Scan(&sentinels)
	if err != nil {
		sentinels = 0
	}

	var errorFuncs int
	err = db.QueryRow(`
		SELECT COUNT(*) FROM symbols
		WHERE kind = 'func'
		  AND (signature LIKE '%error)' OR signature LIKE '% error')
		  AND file_path NOT LIKE '%_test.go'
	`).Scan(&errorFuncs)
	if err != nil {
		errorFuncs = 0
	}

	total := sentinels + errorFuncs
	if total == 0 {
		return nil
	}

	pattern := "inline error returns"
	if sentinels >= 1 {
		pattern = "sentinel errors"
	}

	// Use sentinel ratio as the consistency signal
	ratio := float64(sentinels) / float64(total)
	if sentinels == 0 {
		ratio = 1.0 // fully consistent: no sentinels at all
	}

	return &ErrorConvention{
		Pattern:    pattern,
		Confidence: confidence(max(ratio, 1-ratio), total),
		Sentinels:  sentinels,
		ErrorFuncs: errorFuncs,
	}
}

// detectFileOrg analyzes whether types are organized one-per-file or multiple-per-file.
func detectFileOrg(db *sql.DB) *FileOrgConvention {
	rows, err := db.Query(`
		SELECT file_path, COUNT(*) as cnt FROM symbols
		WHERE kind IN ('struct', 'interface', 'type')
		  AND file_path NOT LIKE '%_test.go'
		GROUP BY file_path
	`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var singleType, multiType, totalTypes int
	var fileCount int
	for rows.Next() {
		var fp string
		var cnt int
		if err := rows.Scan(&fp, &cnt); err != nil {
			continue
		}
		fileCount++
		totalTypes += cnt
		if cnt == 1 {
			singleType++
		} else {
			multiType++
		}
	}
	if err := rows.Err(); err != nil {
		return nil
	}

	if fileCount == 0 {
		return nil
	}

	ratio := float64(max(singleType, multiType)) / float64(fileCount)
	pattern := "one type per file"
	if multiType > singleType {
		pattern = "multiple types per file"
	}

	return &FileOrgConvention{
		Pattern:      pattern,
		Confidence:   confidence(ratio, fileCount),
		AvgTypesFile: float64(totalTypes) / float64(fileCount),
		SingleType:   singleType,
		MultiType:    multiType,
	}
}
