// Package context provides session tracking for active work context.
package context

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Session tracks recently queried symbols for active work context.
type Session struct {
	Project   string        `json:"project"`           // Project root path
	Branch    string        `json:"branch"`            // Git branch when session started
	Queries   []QueryRecord `json:"queries,omitempty"` // Recent queries (newest first)
	UpdatedAt string        `json:"updated_at"`        // Last update timestamp
}

// QueryRecord tracks a single symbol query.
type QueryRecord struct {
	Symbol    string `json:"symbol"`         // Symbol name
	File      string `json:"file"`           // File path (relative)
	Line      int    `json:"line,omitempty"` // Line number
	Kind      string `json:"kind,omitempty"` // Symbol kind (func, type, etc.)
	Command   string `json:"command"`        // Command used (def, refs, callers)
	Timestamp string `json:"timestamp"`      // When queried
}

// ActiveWork represents the active work section for boot context.
type ActiveWork struct {
	RecentSymbols []SymbolRef `json:"recent_symbols,omitempty" yaml:"recent_symbols,omitempty"`
	Branch        string      `json:"branch,omitempty" yaml:"branch,omitempty"`
}

const (
	maxQueries  = 20 // Keep last N queries
	sessionDir  = ".snipe"
	sessionFile = "session.json"
)

// sessionPath returns the path to the session file for a project.
func sessionPath(projectRoot string) string {
	return filepath.Join(projectRoot, sessionDir, sessionFile)
}

// LoadSession loads the session for a project, returning empty session if none exists.
func LoadSession(projectRoot string) (*Session, error) {
	path := sessionPath(projectRoot)

	data, err := os.ReadFile(path) // #nosec G304 -- path derived from projectRoot (session file)
	if err != nil {
		if os.IsNotExist(err) {
			return &Session{Project: projectRoot}, nil
		}
		return nil, err
	}

	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		// Corrupted session file, start fresh (intentionally ignoring parse error)
		return &Session{Project: projectRoot}, nil //nolint:nilerr // Intentional: recover from corrupt file
	}

	// Check if branch changed - if so, clear session
	currentBranch := getCurrentBranch(projectRoot)
	if session.Branch != "" && session.Branch != currentBranch {
		// Branch changed, start fresh session
		return &Session{
			Project: projectRoot,
			Branch:  currentBranch,
		}, nil
	}

	session.Branch = currentBranch
	return &session, nil
}

// SaveSession saves the session to disk.
func SaveSession(session *Session) error {
	if session.Project == "" {
		return nil // No project, nothing to save
	}

	session.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	// Ensure directory exists
	dir := filepath.Join(session.Project, sessionDir)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}

	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(sessionPath(session.Project), data, 0600)
}

// RecordQuery adds a query to the session.
func (s *Session) RecordQuery(symbol, file string, line int, kind, command string) {
	// Ensure branch is current
	if s.Project != "" {
		s.Branch = getCurrentBranch(s.Project)
	}

	// Convert absolute path to relative if needed
	relFile := file
	if s.Project != "" && strings.HasPrefix(file, s.Project) {
		relFile = strings.TrimPrefix(file, s.Project+"/")
	}

	record := QueryRecord{
		Symbol:    symbol,
		File:      relFile,
		Line:      line,
		Kind:      kind,
		Command:   command,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	// Remove any existing query for the same symbol (dedup using relative path)
	filtered := make([]QueryRecord, 0, len(s.Queries))
	for _, q := range s.Queries {
		if q.Symbol != symbol || q.File != relFile {
			filtered = append(filtered, q)
		}
	}

	// Add new query at the front
	s.Queries = append([]QueryRecord{record}, filtered...)

	// Trim to max size
	if len(s.Queries) > maxQueries {
		s.Queries = s.Queries[:maxQueries]
	}
}

// GetActiveWork returns the ActiveWork structure for boot context.
func (s *Session) GetActiveWork() *ActiveWork {
	if len(s.Queries) == 0 {
		return nil
	}

	// Convert recent queries to SymbolRefs (top 10)
	limit := 10
	if len(s.Queries) < limit {
		limit = len(s.Queries)
	}

	symbols := make([]SymbolRef, limit)
	for i := 0; i < limit; i++ {
		q := s.Queries[i]
		symbols[i] = SymbolRef{
			Name: q.Symbol,
			File: q.File,
			Line: q.Line,
		}
	}

	return &ActiveWork{
		RecentSymbols: symbols,
		Branch:        s.Branch,
	}
}

// getCurrentBranch returns the current git branch for a directory.
func getCurrentBranch(dir string) string {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
