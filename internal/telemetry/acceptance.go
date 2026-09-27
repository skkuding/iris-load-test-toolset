// Package telemetry validates the minimum host and thermal evidence required
// to treat a direct-suite measurement as comparable.
package telemetry

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Assessment is the machine-readable telemetry acceptance result.
type Assessment struct {
	HostFactsPresent bool     `json:"hostFactsPresent"`
	ThermalFiles     []string `json:"thermalFiles,omitempty"`
	ThermalSamples   int      `json:"thermalSamples"`
	Valid            bool     `json:"valid"`
	Comparable       bool     `json:"comparable"`
	Reasons          []string `json:"reasons,omitempty"`
}

// Assess checks qualification/host-facts.json and every discovered per-block
// telemetry file.
// Missing evidence is an acceptance failure, not a parser error, so analysis
// can still report measurements while labeling them non-comparable.
func Assess(runDir string) Assessment {
	a := Assessment{Valid: true, Comparable: true}
	factsPath := filepath.Join(runDir, "qualification", "host-facts.json")
	if data, err := os.ReadFile(factsPath); err == nil {
		var facts map[string]any
		if json.Unmarshal(data, &facts) == nil && len(facts) > 0 {
			a.HostFactsPresent = true
		} else {
			a.reject("qualification/host-facts.json is empty or invalid")
		}
	} else {
		a.reject("qualification/host-facts.json is missing")
	}

	paths, _ := filepath.Glob(filepath.Join(runDir, "telemetry", "thermal-*.ndjson"))
	sort.Strings(paths)
	if len(paths) == 0 {
		a.reject("thermal telemetry is missing (want telemetry/thermal-<block>.ndjson)")
		return a
	}
	for _, path := range paths {
		rel, _ := filepath.Rel(runDir, path)
		a.ThermalFiles = append(a.ThermalFiles, filepath.ToSlash(rel))
		count, comparable, err := ValidateFile(path, "")
		if err != nil {
			a.reject(fmt.Sprintf("%s: %v", filepath.ToSlash(rel), err))
			continue
		}
		a.ThermalSamples += count
		if !comparable {
			a.nonComparable(filepath.ToSlash(rel) + ": CPU throttle counter increased during the block")
		}
	}
	return a
}

// AssessBlocks requires exactly one valid telemetry file for every planned
// block. It retains valid but throttled evidence as accepted non-comparable data.
func AssessBlocks(runDir string, blockIDs []string) Assessment {
	a := Assess(runDir)
	planned := make(map[string]bool, len(blockIDs))
	for _, blockID := range blockIDs {
		planned["thermal-"+blockID+".ndjson"] = true
	}
	seen := make(map[string]bool, len(a.ThermalFiles))
	for _, file := range a.ThermalFiles {
		name := filepath.Base(file)
		seen[name] = true
		if !planned[name] {
			a.reject("telemetry/" + name + " is not associated with a planned block")
		}
	}
	for _, blockID := range blockIDs {
		name := "thermal-" + blockID + ".ndjson"
		if !seen[name] {
			a.reject("telemetry/" + name + " is missing")
			continue
		}
		if _, _, err := ValidateFile(filepath.Join(runDir, "telemetry", name), blockID); err != nil {
			a.reject("telemetry/" + name + ": " + err.Error())
		}
	}
	return a
}

func (a *Assessment) reject(reason string) {
	a.Valid = false
	a.nonComparable(reason)
}

func (a *Assessment) nonComparable(reason string) {
	a.Comparable = false
	a.Reasons = append(a.Reasons, reason)
}

// ValidateFile validates one block's before/after telemetry and reports whether
// its CPU throttle counters remained stable.
func ValidateFile(path, blockID string) (int, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var records []Record
	for line := 1; scanner.Scan(); line++ {
		data := bytes.TrimSpace(scanner.Bytes())
		if len(data) == 0 {
			return 0, false, fmt.Errorf("line %d is empty", line)
		}
		var s Record
		if err := json.Unmarshal(data, &s); err != nil {
			return 0, false, fmt.Errorf("line %d: %w", line, err)
		}
		if s.SchemaVersion != 1 || s.MonotonicNS <= 0 || s.BlockID == "" {
			return 0, false, fmt.Errorf("line %d has invalid identity or monotonicNs", line)
		}
		if blockID != "" && s.BlockID != blockID {
			return 0, false, fmt.Errorf("line %d names block %q, want %q", line, s.BlockID, blockID)
		}
		if len(s.Temperatures) == 0 && len(s.Throttle) == 0 {
			return 0, false, fmt.Errorf("line %d has no thermal measurement", line)
		}
		records = append(records, s)
	}
	if err := scanner.Err(); err != nil {
		return 0, false, err
	}
	if len(records) != 2 || records[0].Phase != "before" || records[1].Phase != "after" {
		return 0, false, fmt.Errorf("must contain exactly before and after samples")
	}
	if records[0].BlockID != records[1].BlockID || records[1].MonotonicNS < records[0].MonotonicNS {
		return 0, false, fmt.Errorf("before/after identity or ordering mismatch")
	}
	if !sameKeys(records[0].Temperatures, records[1].Temperatures) || !sameKeys(records[0].Throttle, records[1].Throttle) {
		return 0, false, fmt.Errorf("before/after telemetry sources differ")
	}
	comparable := true
	for source, before := range records[0].Throttle {
		if after, ok := records[1].Throttle[source]; ok && after > before {
			comparable = false
		}
	}
	return len(records), comparable, nil
}

func sameKeys[V any](a, b map[string]V) bool {
	if len(a) != len(b) {
		return false
	}
	for key := range a {
		if _, ok := b[key]; !ok {
			return false
		}
	}
	return true
}
