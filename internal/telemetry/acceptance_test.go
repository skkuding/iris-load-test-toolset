package telemetry

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAssessAcceptsHostAndThermalEvidence(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "qualification", "host-facts.json"), `{"kernel":"test"}`+"\n")
	writeTestFile(t, filepath.Join(dir, "telemetry", "thermal-block-01.ndjson"), strings.Join([]string{
		`{"schemaVersion":1,"blockId":"block-01","phase":"before","monotonicNs":1,"temperaturesMilliC":{"zone0":42000},"throttleCounters":{"cpu0":3}}`,
		`{"schemaVersion":1,"blockId":"block-01","phase":"after","monotonicNs":2,"temperaturesMilliC":{"zone0":43000},"throttleCounters":{"cpu0":3}}`,
	}, "\n")+"\n")

	got := AssessBlocks(dir, []string{"block-01"})
	if !got.Comparable || !got.HostFactsPresent || got.ThermalSamples != 2 {
		t.Fatalf("assessment = %+v", got)
	}
}

func TestAssessMakesMissingTelemetryNonComparable(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "qualification", "host-facts.json"), `{"kernel":"test"}`+"\n")

	got := Assess(dir)
	if got.Comparable || len(got.Reasons) != 1 || !strings.Contains(got.Reasons[0], "missing") {
		t.Fatalf("assessment = %+v", got)
	}
}

func TestAssessRejectsUnlinkedThermalSample(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "qualification", "host-facts.json"), `{"kernel":"test"}`+"\n")
	writeTestFile(t, filepath.Join(dir, "telemetry", "thermal-block-01.ndjson"), `{"temperatureMilliC":42000}`+"\n")

	got := Assess(dir)
	if got.Comparable || len(got.Reasons) == 0 || !strings.Contains(got.Reasons[0], "monotonicNs") {
		t.Fatalf("assessment = %+v", got)
	}
}

func TestAssessMarksThrottleIncreaseNonComparable(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "qualification", "host-facts.json"), `{"kernel":"test"}`+"\n")
	writeTestFile(t, filepath.Join(dir, "telemetry", "thermal-block-01.ndjson"), strings.Join([]string{
		`{"schemaVersion":1,"blockId":"block-01","phase":"before","monotonicNs":1,"throttleCounters":{"cpu0":3}}`,
		`{"schemaVersion":1,"blockId":"block-01","phase":"after","monotonicNs":2,"throttleCounters":{"cpu0":4}}`,
	}, "\n")+"\n")
	got := AssessBlocks(dir, []string{"block-01"})
	if got.Comparable || !strings.Contains(strings.Join(got.Reasons, " "), "increased") {
		t.Fatalf("assessment = %+v", got)
	}
}

type sequenceSampler struct {
	values []Snapshot
	err    error
}

func (s *sequenceSampler) Snapshot() (Snapshot, error) {
	if s.err != nil {
		return Snapshot{}, s.err
	}
	v := s.values[0]
	s.values = s.values[1:]
	return v, nil
}

func TestCaptureWritesBeforeAndAfter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry", "thermal-block-01.ndjson")
	s := &sequenceSampler{values: []Snapshot{
		{TemperaturesMilliC: map[string]int64{"zone0": 41000}},
		{TemperaturesMilliC: map[string]int64{"zone0": 42000}},
	}}
	called := false
	if err := Capture(path, "block-01", s, func() error { called = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("capture did not run the block")
	}
	if count, comparable, err := ValidateFile(path, "block-01"); err != nil || count != 2 || !comparable {
		t.Fatalf("validation = %d/%v/%v", count, comparable, err)
	}
}

func TestCaptureFailsClosedWithoutTelemetry(t *testing.T) {
	called := false
	err := Capture(filepath.Join(t.TempDir(), "thermal.ndjson"), "block-01", &sequenceSampler{err: errors.New("unavailable")}, func() error {
		called = true
		return nil
	})
	if err == nil || called {
		t.Fatalf("capture err=%v called=%v", err, called)
	}
}

func TestSysfsSamplerReadsAvailableSources(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "sys/class/thermal/thermal_zone0/temp"), "42000\n")
	writeTestFile(t, filepath.Join(root, "sys/devices/system/cpu/cpu0/thermal_throttle/core_throttle_count"), "7\n")
	got, err := (SysfsSampler{Root: root}).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.TemperaturesMilliC) != 1 || len(got.ThrottleCounters) != 1 {
		t.Fatalf("snapshot = %+v", got)
	}
}

func TestSysfsSamplerRejectsUnavailableSources(t *testing.T) {
	if _, err := (SysfsSampler{Root: t.TempDir()}).Snapshot(); err == nil {
		t.Fatal("sampler accepted a host with no supported telemetry")
	}
}

func writeTestFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}
