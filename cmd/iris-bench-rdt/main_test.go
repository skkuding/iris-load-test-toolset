package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadRecordAndDefaultGroup(t *testing.T) {
	root := t.TempDir()
	features := filepath.Join(root, "info", "L3_MON", "mon_features")
	if err := os.MkdirAll(filepath.Dir(features), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(features, []byte("llc_occupancy\nmbm_total_bytes\nmbm_local_bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "mon_groups"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "mon_data", "mon_L3_00")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"llc_occupancy": "1048576\n", "mbm_total_bytes": "12345\n", "mbm_local_bytes": "6789\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := checkDefaultGroup(root); err != nil {
		t.Fatal(err)
	}
	r, err := readRecord(root, 100)
	if err != nil {
		t.Fatal(err)
	}
	if r.Scope != "resctrl-default-host" || r.ObservedAtNs != 100 || r.Domains["mon_L3_00"].LLCOccupancyBytes != 1048576 || r.Domains["mon_L3_00"].MBMTotalBytes != 12345 {
		t.Fatalf("record = %+v", r)
	}
	if err := os.Mkdir(filepath.Join(root, "other-class"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkDefaultGroup(root); err == nil {
		t.Fatal("accepted a non-default RDT control group")
	}
	if err := os.Remove(filepath.Join(root, "other-class")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "mon_groups", "other-monitor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkDefaultGroup(root); err == nil {
		t.Fatal("accepted a non-default RDT monitoring group")
	}
}
