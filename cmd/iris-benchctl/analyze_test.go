package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnalyzeReportsStatisticsAndAcceptance(t *testing.T) {
	dir := t.TempDir()
	plan, _ := controllerPlan(t)
	if err := plan.WriteStore(filepath.Join(dir, "plan.json")); err != nil {
		t.Fatal(err)
	}
	blockID := plan.Blocks[0].ID
	writeAnalyzeFile(t, filepath.Join(dir, "samples", blockID+".ndjson"), strings.Join([]string{
		`{"status":"success","cpuTimeMs":10,"realTimeMs":12}`,
		`{"status":"runtime_error","cpuTimeMs":11,"realTimeMs":13}`,
		`{"status":"success","cpuTimeMs":20,"realTimeMs":24}`,
	}, "\n")+"\n")
	writeAnalyzeFile(t, filepath.Join(dir, "qualification", "host-facts.json"), `{"kernel":"test"}`+"\n")

	var out bytes.Buffer
	if err := runAnalyze([]string{"--run", dir}, &out); err != nil {
		t.Fatal(err)
	}
	var report analysisReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Summary.Count != 3 || report.Summary.Failures != 1 || report.Summary.CPUTimeMs.Median != 15 {
		t.Fatalf("summary = %+v", report.Summary)
	}
	if fmt.Sprint(report.Samples) != fmt.Sprint([]string{"samples/" + blockID + ".ndjson"}) {
		t.Fatalf("sample files = %v", report.Samples)
	}
	if report.Comparable || len(report.Reasons) == 0 || !strings.Contains(strings.Join(report.Reasons, " "), "thermal") {
		t.Fatalf("acceptance = comparable %v reasons %v", report.Comparable, report.Reasons)
	}
}

func TestAnalyzeAssessesExactlyPlannedBlocks(t *testing.T) {
	dir := t.TempDir()
	plan, _ := controllerPlan(t)
	second := plan.Blocks[0]
	second.ID = "isolated-1s-02"
	plan.Blocks = append(plan.Blocks, second)
	plan.Expected.Blocks = 2
	if err := plan.WriteStore(filepath.Join(dir, "plan.json")); err != nil {
		t.Fatal(err)
	}
	for _, block := range plan.Blocks {
		writeAnalyzeFile(t, filepath.Join(dir, "samples", block.ID+".ndjson"), `{"status":"success","cpuTimeMs":10,"realTimeMs":12}`+"\n")
	}
	writeAnalyzeFile(t, filepath.Join(dir, "qualification", "host-facts.json"), `{"kernel":"test"}`+"\n")
	writeAnalyzeFile(t, filepath.Join(dir, "telemetry", "thermal-unplanned.ndjson"), strings.Join([]string{
		`{"schemaVersion":1,"blockId":"unplanned","phase":"before","monotonicNs":1,"temperaturesMilliC":{"zone0":42000}}`,
		`{"schemaVersion":1,"blockId":"unplanned","phase":"after","monotonicNs":2,"temperaturesMilliC":{"zone0":42000}}`,
	}, "\n")+"\n")

	var out bytes.Buffer
	if err := runAnalyze([]string{"--run", dir}, &out); err != nil {
		t.Fatal(err)
	}
	var report analysisReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	reasons := strings.Join(report.Telemetry.Reasons, " ")
	if report.Telemetry.Valid || !strings.Contains(reasons, "not associated with a planned block") || !strings.Contains(reasons, plan.Blocks[0].ID) || !strings.Contains(reasons, second.ID) {
		t.Fatalf("telemetry assessment = %+v", report.Telemetry)
	}
}

func TestAnalyzeRequiresRun(t *testing.T) {
	if err := runAnalyze(nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "--run") {
		t.Fatalf("err = %v", err)
	}
}

func writeAnalyzeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}
