package index

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Fingerprint represents the state of a Go project for cache invalidation
type Fingerprint struct {
	Version  string // snipe version
	GoMod    string // hash of go.mod
	GoSum    string // hash of go.sum
	GoWork   string // hash of go.work (if present)
	GoEnv    string // relevant go env values
	Combined string // combined hash for quick comparison
}

// ComputeFingerprint computes the fingerprint for a Go project
func ComputeFingerprint(dir, version string) (*Fingerprint, error) {
	fp := &Fingerprint{Version: version}

	// Hash go.mod
	if h, err := hashFile(filepath.Join(dir, "go.mod")); err == nil {
		fp.GoMod = h
	}

	// Hash go.sum
	if h, err := hashFile(filepath.Join(dir, "go.sum")); err == nil {
		fp.GoSum = h
	}

	// Hash go.work (optional)
	if h, err := hashFile(filepath.Join(dir, "go.work")); err == nil {
		fp.GoWork = h
	}

	// Get relevant go env values
	fp.GoEnv = getGoEnvHash(dir)

	// Compute combined hash
	fp.Combined = computeCombinedHash(fp)

	return fp, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- path is go.mod/go.sum/go.work in repo
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)[:8]), nil
}

func getGoEnvHash(dir string) string {
	// Get relevant environment values
	envVars := []string{"GOOS", "GOARCH", "GOMOD", "GOWORK"}
	var values []string

	cmd := exec.Command("go", "env")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	// Parse output
	envMap := make(map[string]string)
	for _, line := range strings.Split(string(output), "\n") {
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := parts[0]
			value := strings.Trim(parts[1], "\"'")
			envMap[key] = value
		}
	}

	for _, key := range envVars {
		if val, ok := envMap[key]; ok {
			values = append(values, key+"="+val)
		}
	}

	sort.Strings(values)
	h := sha256.Sum256([]byte(strings.Join(values, "\n")))
	return hex.EncodeToString(h[:8])
}

func computeCombinedHash(fp *Fingerprint) string {
	// Version intentionally excluded — a snipe rebuild should not
	// invalidate the index of every target repo.  Only dependency
	// and toolchain changes (go.mod, go.sum, go.work, go env) matter.
	data := strings.Join([]string{
		fp.GoMod,
		fp.GoSum,
		fp.GoWork,
		fp.GoEnv,
	}, "|")

	h := sha256.Sum256([]byte(data))
	return hex.EncodeToString(h[:8])
}

// String returns a string representation of the fingerprint
func (fp *Fingerprint) String() string {
	return fp.Combined
}

// IndexState represents the state of the index
type IndexState string

const (
	StateFresh   IndexState = "fresh"
	StateStale   IndexState = "stale"
	StateMissing IndexState = "missing"
)

// CheckIndexState compares fingerprints and returns the index state
func CheckIndexState(current, stored *Fingerprint) IndexState {
	if stored == nil {
		return StateMissing
	}
	if current.Combined != stored.Combined {
		return StateStale
	}
	return StateFresh
}
