package context

import (
	"bufio"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
)

// DetectBuildInfo detects the primary build system and CI configuration for a repo.
// db may be nil; only used to query mage targets from the index.
func DetectBuildInfo(repoRoot string, db *sql.DB) BuildInfo {
	ci := detectCI(repoRoot)
	goGen := detectGoGenerate(repoRoot)

	switch {
	case fileExists(filepath.Join(repoRoot, "magefile.go")):
		return BuildInfo{
			System:     "mage",
			Build:      "mage",
			Test:       "mage test",
			Targets:    mageTargetsFromIndex(db, repoRoot),
			CI:         ci,
			GoGenerate: goGen,
		}
	case fileExists(filepath.Join(repoRoot, "Makefile")):
		content, _ := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
		return BuildInfo{
			System:     "make",
			Build:      "make",
			Test:       "make test",
			Targets:    parseMakefileTargets(string(content)),
			CI:         ci,
			GoGenerate: goGen,
		}
	case fileExists(filepath.Join(repoRoot, "Taskfile.yml")) || fileExists(filepath.Join(repoRoot, "Taskfile.yaml")):
		taskfile := filepath.Join(repoRoot, "Taskfile.yml")
		if !fileExists(taskfile) {
			taskfile = filepath.Join(repoRoot, "Taskfile.yaml")
		}
		content, _ := os.ReadFile(taskfile)
		return BuildInfo{
			System:     "task",
			Build:      "task build",
			Test:       "task test",
			Targets:    parseTaskfileTargets(string(content)),
			CI:         ci,
			GoGenerate: goGen,
		}
	case fileExists(filepath.Join(repoRoot, "justfile")) || fileExists(filepath.Join(repoRoot, "Justfile")):
		justfile := filepath.Join(repoRoot, "justfile")
		if !fileExists(justfile) {
			justfile = filepath.Join(repoRoot, "Justfile")
		}
		content, _ := os.ReadFile(justfile)
		return BuildInfo{
			System:     "just",
			Build:      "just build",
			Test:       "just test",
			Targets:    parseJustfileRecipes(string(content)),
			CI:         ci,
			GoGenerate: goGen,
		}
	default:
		return BuildInfo{
			System:     "go",
			Build:      "go build ./...",
			Test:       "go test ./...",
			CI:         ci,
			GoGenerate: goGen,
		}
	}
}

// mageTargetsFromIndex queries the snipe index for exported functions in magefile.go.
func mageTargetsFromIndex(db *sql.DB, repoRoot string) []string {
	if db == nil {
		return nil
	}
	magefilePath := filepath.Join(repoRoot, "magefile.go")
	rows, err := db.Query(`
		SELECT name FROM symbols
		WHERE file_path = ?
		  AND kind = 'func'
		  AND name GLOB '[A-Z]*'
		ORDER BY line_start
	`, magefilePath)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var targets []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		targets = append(targets, name)
	}
	return targets
}

// parseMakefileTargets extracts target names from a Makefile.
// Collects .PHONY entries and targets with ## comments.
func parseMakefileTargets(content string) []string {
	var targets []string
	seen := make(map[string]bool)

	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, ".PHONY:") {
			rest := strings.TrimPrefix(line, ".PHONY:")
			for _, t := range strings.Fields(rest) {
				if !seen[t] {
					seen[t] = true
					targets = append(targets, t)
				}
			}
			continue
		}

		if strings.Contains(line, ":") && strings.Contains(line, "##") {
			name := strings.TrimSpace(strings.SplitN(line, ":", 2)[0])
			if name != "" && !strings.ContainsAny(name, " \t$()") && !seen[name] {
				seen[name] = true
				targets = append(targets, name)
			}
		}
	}
	return targets
}

// parseTaskfileTargets extracts task names from Taskfile.yml content.
func parseTaskfileTargets(content string) []string {
	var targets []string
	inTasks := false

	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if trimmed == "tasks:" {
			inTasks = true
			continue
		}
		if inTasks {
			if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "    ") {
				name := strings.TrimSuffix(strings.TrimSpace(line), ":")
				if name != "" && !strings.HasPrefix(name, "#") {
					targets = append(targets, name)
				}
			} else if !strings.HasPrefix(line, " ") && trimmed != "" {
				inTasks = false
			}
		}
	}
	return targets
}

// parseJustfileRecipes extracts recipe names from a justfile.
func parseJustfileRecipes(content string) []string {
	var recipes []string
	seen := make(map[string]bool)

	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		candidate := strings.TrimPrefix(line, "@")
		parts := strings.Fields(candidate)
		if len(parts) == 0 {
			continue
		}
		name := strings.TrimSuffix(parts[0], ":")
		if name != "" && !strings.ContainsAny(name, "=:") && !seen[name] {
			seen[name] = true
			recipes = append(recipes, name)
		}
	}
	return recipes
}

// detectCI checks for known CI configuration files.
func detectCI(repoRoot string) []CIInfo {
	var found []CIInfo

	workflowDir := filepath.Join(repoRoot, ".github", "workflows")
	if entries, err := os.ReadDir(workflowDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && (strings.HasSuffix(e.Name(), ".yml") || strings.HasSuffix(e.Name(), ".yaml")) {
				found = append(found, CIInfo{
					System: "github-actions",
					File:   filepath.Join(".github", "workflows", e.Name()),
				})
			}
		}
	}

	if fileExists(filepath.Join(repoRoot, ".gitlab-ci.yml")) {
		found = append(found, CIInfo{System: "gitlab-ci", File: ".gitlab-ci.yml"})
	}

	if fileExists(filepath.Join(repoRoot, ".circleci", "config.yml")) {
		found = append(found, CIInfo{System: "circleci", File: ".circleci/config.yml"})
	}

	if fileExists(filepath.Join(repoRoot, "Jenkinsfile")) {
		found = append(found, CIInfo{System: "jenkins", File: "Jenkinsfile"})
	}

	return found
}

// detectGoGenerate checks for //go:generate directives in Go source files.
// Scans root and one level of subdirectories.
func detectGoGenerate(repoRoot string) bool {
	entries, err := os.ReadDir(repoRoot)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			subEntries, err := os.ReadDir(filepath.Join(repoRoot, e.Name()))
			if err != nil {
				continue
			}
			for _, se := range subEntries {
				if !se.IsDir() && strings.HasSuffix(se.Name(), ".go") {
					if fileHasGoGenerate(filepath.Join(repoRoot, e.Name(), se.Name())) {
						return true
					}
				}
			}
		} else if strings.HasSuffix(e.Name(), ".go") {
			if fileHasGoGenerate(filepath.Join(repoRoot, e.Name())) {
				return true
			}
		}
	}
	return false
}

func fileHasGoGenerate(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), "//go:generate") {
			return true
		}
	}
	return false
}
