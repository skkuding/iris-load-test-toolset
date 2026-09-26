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
type Summary struct {
	Count      int    `json:"count"`
	Failures   int    `json:"failures"`
	Successful int    `json:"successful"`
	CPUTimeMs  Metric `json:"cpuTimeMs"`
	RealTimeMs Metric `json:"realTimeMs"`
}

type sample struct {
	Status     string   `json:"status"`
	CPUTimeMs  *float64 `json:"cpuTimeMs"`
	RealTimeMs *float64 `json:"realTimeMs"`
}

// NDJSON parses direct Judger samples and returns their timing summary.
func NDJSON(r io.Reader) (Summary, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	var summary Summary
	var cpu, real []float64
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
		cpu = append(cpu, *s.CPUTimeMs)
		real = append(real, *s.RealTimeMs)
	}
	if err := scanner.Err(); err != nil {
		return Summary{}, fmt.Errorf("read samples: %w", err)
	}
	if summary.Count == 0 {
		return Summary{}, errors.New("samples file contains no records")
	}
	if len(cpu) == 0 {
		return Summary{}, errors.New("samples file contains no successful measurements")
	}
	summary.Successful = len(cpu)
	summary.CPUTimeMs = summarize(cpu)
	summary.RealTimeMs = summarize(real)
	return summary, nil
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
