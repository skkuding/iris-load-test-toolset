package analyze

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNDJSONSummary(t *testing.T) {
	input := strings.Join([]string{
		`{"status":"success","cpuTimeMs":1,"realTimeMs":10}`,
		`{"status":"success","cpuTimeMs":2,"realTimeMs":20}`,
		`{"status":"runtime_error","cpuTimeMs":99,"realTimeMs":99}`,
		`{"status":"success","cpuTimeMs":3,"realTimeMs":30}`,
		`{"status":"success","cpuTimeMs":4,"realTimeMs":40}`,
	}, "\n") + "\n"

	got, err := NDJSON(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if got.Count != 5 || got.Failures != 1 || got.Successful != 4 {
		t.Fatalf("counts = %d/%d/%d", got.Count, got.Failures, got.Successful)
	}
	assertNear(t, got.CPUTimeMs.Median, 2.5)
	assertNear(t, got.CPUTimeMs.MAD, 1)
	assertNear(t, got.CPUTimeMs.StdDev, math.Sqrt(5.0/3.0))
	assertNear(t, got.CPUTimeMs.P90, 3.7)
	assertNear(t, got.CPUTimeMs.P95, 3.85)
	assertNear(t, got.CPUTimeMs.P99, 3.97)
	assertNear(t, got.CPUTimeMs.Max, 4)
	assertNear(t, got.RealTimeMs.Median, 25)
}

func TestNDJSONFiltersToSteadyWindow(t *testing.T) {
	// Per-worker intervals: w0=[100,2000], w1=[200,600]; the steady window is
	// [max first start, min last end] = [200,600]. Only the two w1 samples fall
	// strictly inside it.
	input := strings.Join([]string{
		`{"status":"success","worker":"w0","cpuTimeMs":1000,"realTimeMs":1000,"startedAtNs":100,"endedAtNs":500}`,
		`{"status":"success","worker":"w0","cpuTimeMs":9999,"realTimeMs":9999,"startedAtNs":600,"endedAtNs":2000}`,
		`{"status":"success","worker":"w1","cpuTimeMs":10,"realTimeMs":10,"startedAtNs":200,"endedAtNs":400}`,
		`{"status":"success","worker":"w1","cpuTimeMs":20,"realTimeMs":20,"startedAtNs":300,"endedAtNs":600}`,
	}, "\n") + "\n"

	got, err := NDJSON(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if got.Count != 4 || got.Failures != 0 || got.Successful != 4 {
		t.Fatalf("counts = %d/%d/%d", got.Count, got.Failures, got.Successful)
	}
	if !got.SteadyWindowApplied || got.SteadyWindowStartNs != 200 || got.SteadyWindowEndNs != 600 {
		t.Fatalf("window = %+v", got)
	}
	assertNear(t, got.SteadyWindowSeconds, 400e-9)
	assertNear(t, got.CPUTimeMs.Median, 15)
	assertNear(t, got.CPUTimeMs.Max, 20)
	assertNear(t, got.RealTimeMs.Median, 15)
}

func TestNDJSONKeepsPreWindowBehaviorWithoutTimestamps(t *testing.T) {
	got, err := NDJSON(strings.NewReader(`{"status":"success","cpuTimeMs":1,"realTimeMs":2}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.SteadyWindowApplied || got.SteadyWindowStartNs != 0 || got.SteadyWindowEndNs != 0 {
		t.Fatalf("unexpected window on untimestamped samples: %+v", got)
	}
}

func TestNDJSONFilesAggregatesInDeterministicOrder(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a.ndjson")
	second := filepath.Join(dir, "b.ndjson")
	if err := os.WriteFile(first, []byte(`{"status":"success","cpuTimeMs":1,"realTimeMs":2}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte(`{"status":"success","cpuTimeMs":3,"realTimeMs":4}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := NDJSONFiles([]string{second, first})
	if err != nil {
		t.Fatal(err)
	}
	if got.Count != 2 || got.CPUTimeMs.Median != 2 || got.RealTimeMs.Median != 3 {
		t.Fatalf("summary = %+v", got)
	}
}

func TestNDJSONRejectsMissingSuccessfulTiming(t *testing.T) {
	_, err := NDJSON(strings.NewReader(`{"status":"success","cpuTimeMs":1}` + "\n"))
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("err = %v", err)
	}
}

func TestNDJSONRequiresSuccessfulMeasurement(t *testing.T) {
	_, err := NDJSON(strings.NewReader(`{"status":"failed"}` + "\n"))
	if err == nil || !strings.Contains(err.Error(), "no successful") {
		t.Fatalf("err = %v", err)
	}
}

func assertNear(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("got %v, want %v", got, want)
	}
}
