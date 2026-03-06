package store

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	_ "modernc.org/sqlite"
)

// Store manages the SQLite index database
type Store struct {
	db   *sql.DB
	path string
}

// DefaultIndexPath returns the default index path for a repo
func DefaultIndexPath(repoRoot string) string {
	return filepath.Join(repoRoot, ".snipe", "index.db")
}

// Open opens or creates an index database at the given path
func Open(path string) (*Store, error) {
	// Ensure directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, fmt.Errorf("create index directory: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Enable WAL mode for better concurrent read performance
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close() // G104: cleanup on error path
		return nil, fmt.Errorf("enable WAL mode: %w", err)
	}
	if err := verifyPragmaString(db, "journal_mode", "wal"); err != nil {
		_ = db.Close() // G104: cleanup on error path
		return nil, err
	}

	// Balance durability and latency for WAL workloads
	if _, err := db.Exec("PRAGMA synchronous=NORMAL"); err != nil {
		_ = db.Close() // G104: cleanup on error path
		return nil, fmt.Errorf("set synchronous: %w", err)
	}
	if err := verifyPragmaInt(db, "synchronous", 1); err != nil {
		_ = db.Close() // G104: cleanup on error path
		return nil, err
	}

	// Keep temporary data in memory for speed
	if _, err := db.Exec("PRAGMA temp_store=MEMORY"); err != nil {
		_ = db.Close() // G104: cleanup on error path
		return nil, fmt.Errorf("set temp_store: %w", err)
	}
	if err := verifyPragmaInt(db, "temp_store", 2); err != nil {
		_ = db.Close() // G104: cleanup on error path
		return nil, err
	}

	// Set busy timeout to avoid "database is locked" errors during concurrent access
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		_ = db.Close() // G104: cleanup on error path
		return nil, fmt.Errorf("set busy_timeout: %w", err)
	}
	if err := verifyPragmaInt(db, "busy_timeout", 5000); err != nil {
		_ = db.Close() // G104: cleanup on error path
		return nil, err
	}

	// Enable foreign keys
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		_ = db.Close() // G104: cleanup on error path
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}
	if err := verifyPragmaInt(db, "foreign_keys", 1); err != nil {
		_ = db.Close() // G104: cleanup on error path
		return nil, err
	}

	// Limit connections to avoid lock contention
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	s := &Store{db: db, path: path}

	// Initialize schema
	if err := s.initSchema(); err != nil {
		_ = db.Close() // G104: cleanup on error path
		return nil, fmt.Errorf("initialize schema: %w", err)
	}

	return s, nil
}

// Close closes the database connection
func (s *Store) Close() error {
	return s.db.Close()
}

// DB returns the underlying database connection for advanced queries
func (s *Store) DB() *sql.DB {
	return s.db
}

// Path returns the database file path
func (s *Store) Path() string {
	return s.path
}

// Exists checks if an index database exists at the given path
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// LockPath returns the lock file path for an index
func LockPath(dbPath string) string {
	return dbPath + ".lock"
}

// IsIndexing checks if indexing is in progress (lock file exists)
func IsIndexing(dbPath string) bool {
	_, err := os.Stat(LockPath(dbPath))
	return err == nil
}

// AcquireLock creates a lock file for indexing.
// The lock file contains the PID of the owning process. If a stale lock is
// detected (owning process no longer running), it is automatically removed.
func AcquireLock(dbPath string) error {
	lockPath := LockPath(dbPath)
	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(lockPath), 0750); err != nil {
		return fmt.Errorf("create lock directory: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600) // #nosec G304
	if err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("create lock file: %w", err)
		}
		// Lock exists — check if holder is still alive
		if !tryRemoveStaleLock(lockPath) {
			return fmt.Errorf("index is locked by another process (see %s)", lockPath)
		}
		// Stale lock removed, retry once
		f, err = os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600) // #nosec G304
		if err != nil {
			return fmt.Errorf("create lock file after stale removal: %w", err)
		}
	}
	fmt.Fprintf(f, "%d\n", os.Getpid())
	return f.Close()
}

// tryRemoveStaleLock checks whether the lock file is held by a dead process.
// Returns true if the lock was stale and removed.
func tryRemoveStaleLock(lockPath string) bool {
	data, err := os.ReadFile(lockPath) // #nosec G304
	if err != nil {
		return false
	}
	pidStr := strings.TrimSpace(string(data))
	if pidStr == "" {
		// Empty file (old format or crash mid-write) — treat as stale
		_ = os.Remove(lockPath) // G104: best-effort cleanup
		return true
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		// Unparseable — treat as stale
		_ = os.Remove(lockPath) // G104: best-effort cleanup
		return true
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		_ = os.Remove(lockPath) // G104: best-effort cleanup
		return true
	}
	// Signal 0 checks existence without killing
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		_ = os.Remove(lockPath) // G104: best-effort cleanup
		return true
	}
	return false
}

// ReleaseLock removes the lock file
func ReleaseLock(dbPath string) error {
	return os.Remove(LockPath(dbPath))
}

func verifyPragmaString(db *sql.DB, pragma, want string) error {
	var got string
	if err := db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil {
		return fmt.Errorf("verify %s pragma: %w", pragma, err)
	}
	if got != want {
		return fmt.Errorf("verify %s pragma: got %q, want %q", pragma, got, want)
	}
	return nil
}

func verifyPragmaInt(db *sql.DB, pragma string, want int) error {
	var got int
	if err := db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil {
		return fmt.Errorf("verify %s pragma: %w", pragma, err)
	}
	if got != want {
		return fmt.Errorf("verify %s pragma: got %d, want %d", pragma, got, want)
	}
	return nil
}
