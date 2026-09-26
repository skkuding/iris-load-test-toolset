// Package analyze summarizes direct Judger NDJSON measurements.
package analyze

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
)

const maxLineBytes = 1 << 20

// Metric is the distribution summary for one timing field. Percentiles use
// linear interpolation between adjacent ordered observations.
type Metric struct {
	Median         float64 `json:"median"`
	MAD            float64 `json:"mad"`
	StdDev         float64 `json:"stddev"`
	CVPercent      float64 `json:"cvPercent"`
	P90            float64 `json:"p90"`
	P95            float64 `json:"p95"`
	P99            float64 `json:"p99"`
	Max            float64 `json:"max"`
	P99MedianRatio float64 `json:"p99MedianRatio"`
}

// Summary reports all records and failures while computing timing
// distributions from successful measurements only.
//
// When every successful sample carries valid host-wide timestamps, the timing
// distributions are computed only from samples inside the steady window (the
// interval in which every worker had a submission in flight simultaneously) and
// SteadyWindowApplied is true. Count, Failures, and Successful still describe
// the whole file. A file without timestamps keeps the pre-window behavior and
// reports a zero window.
type Summary struct {
	Count      int    `json:"count"`
	Failures   int    `json:"failures"`
	Successful int    `json:"successful"`
	CPUTimeMs  Metric `json:"cpuTimeMs"`
	RealTimeMs Metric `json:"realTimeMs"`
	// SteadyWindow* describe the all-workers-active interval, when present.
	SteadyWindowApplied bool    `json:"steadyWindowApplied"`
	SteadyWindowStartNs int64   `json:"steadyWindowStartNs"`
	SteadyWindowEndNs   int64   `json:"steadyWindowEndNs"`
	SteadyWindowSeconds float64 `json:"steadyWindowSeconds"`
}

type sample struct {
	Status      string   `json:"status"`
	CPUTimeMs   *float64 `json:"cpuTimeMs"`
	RealTimeMs  *float64 `json:"realTimeMs"`
	Worker      string   `json:"worker"`
	StartedAtNs *int64   `json:"startedAtNs"`
	EndedAtNs   *int64   `json:"endedAtNs"`
}

// successSample is a validated successful measurement plus the fields needed to
// place it inside the steady window.
type successSample struct {
	cpu, real   float64
	worker      string
	startedAtNs int64
	endedAtNs   int64
	timestamped bool
}

// NDJSON parses direct Judger samples and returns their timing summary.
func NDJSON(r io.Reader) (Summary, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	var summary Summary
	var successes []successSample
	for line := 1; scanner.Scan(); line++ {
		data := bytes.TrimSpace(scanner.Bytes())
		if len(data) == 0 {
			return Summary{}, fmt.Errorf("samples line %d is empty", line)
		}
		var s sample
		if err := json.Unmarshal(data, &s); err != nil {
			return Summary{}, fmt.Errorf("samples line %d: %w", line, err)
		}
		summary.Count++
		if s.Status != "success" {
			summary.Failures++
			continue
		}
		if s.CPUTimeMs == nil || s.RealTimeMs == nil {
			return Summary{}, fmt.Errorf("samples line %d: successful sample is missing cpuTimeMs or realTimeMs", line)
		}
		if !validMeasurement(*s.CPUTimeMs) || !validMeasurement(*s.RealTimeMs) {
			return Summary{}, fmt.Errorf("samples line %d: successful sample has an invalid timing value", line)
		}
		ss := successSample{cpu: *s.CPUTimeMs, real: *s.RealTimeMs, worker: s.Worker}
		if s.StartedAtNs != nil && s.EndedAtNs != nil &&
			*s.StartedAtNs > 0 && *s.EndedAtNs > 0 && *s.EndedAtNs >= *s.StartedAtNs {
			ss.startedAtNs = *s.StartedAtNs
			ss.endedAtNs = *s.EndedAtNs
			ss.timestamped = true
		}
		successes = append(successes, ss)
	}
	if err := scanner.Err(); err != nil {
		return Summary{}, fmt.Errorf("read samples: %w", err)
	}
	if summary.Count == 0 {
		return Summary{}, errors.New("samples file contains no records")
	}
	if len(successes) == 0 {
		return Summary{}, errors.New("samples file contains no successful measurements")
	}
	summary.Successful = len(successes)
	if start, end, ok := steadyWindow(successes); ok {
		filtered := make([]successSample, 0, len(successes))
		for _, ss := range successes {
			if ss.startedAtNs >= start && ss.endedAtNs <= end {
				filtered = append(filtered, ss)
			}
		}
		if len(filtered) == 0 {
			return Summary{}, errors.New("samples file contains no successful measurements inside the steady window")
		}
		summary.SteadyWindowApplied = true
		summary.SteadyWindowStartNs = start
		summary.SteadyWindowEndNs = end
		summary.SteadyWindowSeconds = float64(end-start) / 1e9
		successes = filtered
	}
	cpu := make([]float64, len(successes))
	real := make([]float64, len(successes))
	for i, ss := range successes {
		cpu[i] = ss.cpu
		real[i] = ss.real
	}
	summary.CPUTimeMs = summarize(cpu)
	summary.RealTimeMs = summarize(real)
	return summary, nil
}

// steadyWindow computes [max over workers of first start, min over workers of
// last end]. Timestamp presence is all-or-nothing: if any successful sample is
// missing valid positive timestamps, no window is reported and the caller keeps
// the pre-window behavior. A window that is empty is likewise not reported.
func steadyWindow(samples []successSample) (int64, int64, bool) {
	type interval struct {
		first int64
		last  int64
		seen  bool
	}
	perWorker := map[string]*interval{}
	for _, s := range samples {
		if !s.timestamped {
			return 0, 0, false
		}
		w := perWorker[s.worker]
		if w == nil {
			w = &interval{}
			perWorker[s.worker] = w
		}
		if !w.seen || s.startedAtNs < w.first {
			w.first = s.startedAtNs
		}
		if !w.seen || s.endedAtNs > w.last {
			w.last = s.endedAtNs
		}
		w.seen = true
	}
	if len(perWorker) == 0 {
		return 0, 0, false
	}
	var start, end int64
	first := true
	for _, w := range perWorker {
		if first || w.first > start {
			start = w.first
		}
		if first || w.last < end {
			end = w.last
		}
		first = false
	}
	if start >= end {
		return 0, 0, false
	}
	return start, end, true
}

// NDJSONFiles parses every named sample file in lexical path order as one
// aggregate population.
func NDJSONFiles(paths []string) (Summary, error) {
	paths = append([]string(nil), paths...)
	sort.Strings(paths)
	readers := make([]io.Reader, 0, len(paths))
	files := make([]*os.File, 0, len(paths))
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			for _, opened := range files {
				_ = opened.Close()
			}
			return Summary{}, fmt.Errorf("open samples %s: %w", path, err)
		}
		files = append(files, f)
		readers = append(readers, f)
	}
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	if len(readers) == 0 {
		return Summary{}, errors.New("no sample files")
	}
	return NDJSON(io.MultiReader(readers...))
}

func validMeasurement(v float64) bool {
	return v >= 0 && !math.IsNaN(v) && !math.IsInf(v, 0)
}

func summarize(values []float64) Metric {
	v := append([]float64(nil), values...)
	sort.Float64s(v)
	median := percentile(v, 0.5)
	deviations := make([]float64, len(v))
	var sum float64
	for i, n := range v {
		sum += n
		deviations[i] = math.Abs(n - median)
	}
	sort.Float64s(deviations)
	mean := sum / float64(len(v))
	var squared float64
	for _, n := range v {
		d := n - mean
		squared += d * d
	}
	var stddev float64
	if len(v) > 1 {
		stddev = math.Sqrt(squared / float64(len(v)-1))
	}
	p99 := percentile(v, 0.99)
	m := Metric{
		Median: median,
		MAD:    percentile(deviations, 0.5),
		StdDev: stddev,
		P90:    percentile(v, 0.90),
		P95:    percentile(v, 0.95),
		P99:    p99,
		Max:    v[len(v)-1],
	}
	if mean != 0 {
		m.CVPercent = stddev / mean * 100
	}
	if median != 0 {
		m.P99MedianRatio = p99 / median
	}
	return m
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 1 {
		return sorted[0]
	}
	position := p * float64(len(sorted)-1)
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	if lower == upper {
		return sorted[lower]
	}
	return sorted[lower] + (sorted[upper]-sorted[lower])*(position-float64(lower))
}
