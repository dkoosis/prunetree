package metrics

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
)

// Comparison contains the result of comparing two baselines
type Comparison struct {
	Current    *Baseline `json:"current"`
	Reference  *Baseline `json:"reference"`
	Checks     []Check   `json:"checks"`
	HasFailure bool      `json:"has_failure"`
}

// Check represents a single metric comparison
type Check struct {
	Name      string  `json:"name"`
	Current   float64 `json:"current"`
	Reference float64 `json:"reference"`
	Threshold float64 `json:"threshold_pct"`
	ChangePct float64 `json:"change_pct"`
	Status    string  `json:"status"` // "pass", "warn", "fail"
	Message   string  `json:"message,omitempty"`
}

// CompareConfig configures baseline comparison
type CompareConfig struct {
	Threshold float64 // Default regression threshold (e.g., 15 = 15%)
}

// LoadBaseline loads a baseline from a JSON file
func LoadBaseline(path string) (*Baseline, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path from caller (baseline JSON file)
	if err != nil {
		return nil, err
	}
	var b Baseline
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// Compare compares current baseline against a reference
func Compare(current, reference *Baseline, cfg CompareConfig) *Comparison {
	if cfg.Threshold == 0 {
		cfg.Threshold = 15.0 // Default 15% regression threshold
	}

	comp := &Comparison{
		Current:   current,
		Reference: reference,
	}

	// Performance checks (lower is better)
	comp.addCheck("index_time", float64(current.Index.TotalMs), float64(reference.Index.TotalMs), cfg.Threshold, true)
	comp.addCheck("query_def_by_name", current.Query.DefByNameMs, reference.Query.DefByNameMs, cfg.Threshold, true)
	comp.addCheck("query_def_by_pos", current.Query.DefByPosMs, reference.Query.DefByPosMs, cfg.Threshold, true)
	comp.addCheck("query_refs", current.Query.RefsByIDMs, reference.Query.RefsByIDMs, cfg.Threshold, true)
	comp.addCheck("search_simple", current.Search.SimplePatternMs, reference.Search.SimplePatternMs, cfg.Threshold, true)
	comp.addCheck("search_regex", current.Search.RegexPatternMs, reference.Search.RegexPatternMs, cfg.Threshold, true)

	// Size checks (lower is better)
	comp.addCheck("db_size", float64(current.Codebase.DBSizeKB), float64(reference.Codebase.DBSizeKB), cfg.Threshold, true)

	// Quality checks (higher is better)
	comp.addCheck("doc_coverage", current.Quality.DocCoverage, reference.Quality.DocCoverage, cfg.Threshold, false)
	comp.addCheck("callgraph_coverage", current.Quality.CallGraphCoverage, reference.Quality.CallGraphCoverage, cfg.Threshold, false)

	return comp
}

// addCheck adds a metric comparison check
func (c *Comparison) addCheck(name string, current, reference, threshold float64, lowerIsBetter bool) {
	check := Check{
		Name:      name,
		Current:   current,
		Reference: reference,
		Threshold: threshold,
	}

	if reference == 0 {
		check.Status = "pass"
		check.Message = "no reference value"
		c.Checks = append(c.Checks, check)
		return
	}

	// Calculate change percentage
	check.ChangePct = ((current - reference) / reference) * 100

	// Determine status based on direction
	var regression float64
	if lowerIsBetter {
		regression = check.ChangePct // Positive change = regression
	} else {
		regression = -check.ChangePct // Negative change = regression
	}

	switch {
	case regression > threshold:
		check.Status = "fail"
		c.HasFailure = true
		if lowerIsBetter {
			check.Message = fmt.Sprintf("increased %.1f%% (was %.2f, now %.2f)", check.ChangePct, reference, current)
		} else {
			check.Message = fmt.Sprintf("decreased %.1f%% (was %.2f, now %.2f)", -check.ChangePct, reference, current)
		}
	case math.Abs(regression) > threshold/2:
		check.Status = "warn"
		check.Message = fmt.Sprintf("changed %.1f%%", check.ChangePct)
	default:
		check.Status = "pass"
		if check.ChangePct < 0 == lowerIsBetter {
			check.Message = fmt.Sprintf("improved %.1f%%", math.Abs(check.ChangePct))
		}
	}

	c.Checks = append(c.Checks, check)
}

// ToJSON converts comparison to JSON
func (c *Comparison) ToJSON() ([]byte, error) {
	return json.MarshalIndent(c, "", "  ")
}
