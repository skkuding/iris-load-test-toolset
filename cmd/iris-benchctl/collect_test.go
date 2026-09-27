package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skkuding/iris-load-test-toolset/internal/artifact"
	"github.com/skkuding/iris-load-test-toolset/internal/experiment"
	"github.com/skkuding/iris-load-test-toolset/internal/manifest"
	"github.com/skkuding/iris-load-test-toolset/internal/runplan"
)

// fakeDownloader serves an in-memory remote bundle.
type fakeDownloader struct {
	files map[string][]byte
}

func (d *fakeDownloader) Download(_ context.Context, remotePath, localPath string) error {
	data, ok := d.files[remotePath]
	if !ok {
		return fmt.Errorf("no such remote file %s", remotePath)
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(localPath, data, 0o644)
}

// collectTestBundle builds a minimal remote run tree and returns its source
// directory and plan.
func collectTestBundle(t *testing.T) (string, runplan.Plan) {
	t.Helper()
	plan, sha := controllerPlan(t)
	src := t.TempDir()
	if err := plan.WriteStore(filepath.Join(src, "plan.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "samples"), 0o755); err != nil {
		t.Fatal(err)
	}
	subtree, err := experiment.SubtreeName(plan.RunID, "isolated-1s-01", "worker-01")
	if err != nil {
		t.Fatal(err)
	}
	sample := `{"runId":"` + plan.RunID + `","blockId":"isolated-1s-01","worker":"worker-01","status":"success","resultCode":0,"errorCode":0,"outputMatches":true,"containmentMode":"isolated","comparable":true,"cgroupPath":"/sys/fs/cgroup/iris-bench/` + subtree + `/sandbox-x","cgroupContained":true}` + "\n"
	samplesPath := filepath.Join(src, "samples", "isolated-1s-01.ndjson")
	if err := os.WriteFile(samplesPath, []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}
	samplesSHA, _, _ := artifact.HashFile(samplesPath)
	receipt := experiment.Receipt{SchemaVersion: 1, RunID: plan.RunID, BlockID: "isolated-1s-01", Status: experiment.StatusPassed, SampleCount: 5, SamplesSHA256: samplesSHA}
	if err := artifact.WriteJSONAtomic(filepath.Join(src, "receipts", "isolated-1s-01.json"), receipt, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := artifact.WriteJSONAtomic(filepath.Join(src, "qualification", "host-facts.json"), map[string]string{"cgroupVersion": "v2", "physicalCores": "4"}, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, block := range plan.Blocks {
		thermal := strings.Join([]string{
			fmt.Sprintf(`{"schemaVersion":1,"blockId":%q,"phase":"before","monotonicNs":1,"temperaturesMilliC":{"zone0":42000}}`, block.ID),
			fmt.Sprintf(`{"schemaVersion":1,"blockId":%q,"phase":"after","monotonicNs":2,"temperaturesMilliC":{"zone0":43000}}`, block.ID),
		}, "\n") + "\n"
		if err := artifact.WriteFileAtomic(filepath.Join(src, "telemetry", "thermal-"+block.ID+".ndjson"), []byte(thermal), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, action := range []string{"prepared", "qualified", "validated", "cleaned"} {
		rec := agentRecord{RunID: plan.RunID, PlanSHA256: sha, AgentVersion: "test-agent", Action: action}
		if err := artifact.WriteJSONAtomic(filepath.Join(src, action+".json"), rec, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return src, plan
}

// serveBundle builds the inventory for src and serves every listed file under
// remoteDir.
func serveBundle(t *testing.T, src, remoteDir string) *fakeDownloader {
	t.Helper()
	inv, err := artifact.BuildInventory(src)
	if err != nil {
		t.Fatal(err)
	}
	invData, err := inv.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	invData = append(invData, '\n')
	dl := &fakeDownloader{files: map[string][]byte{}}
	dl.files[filepath.Join(remoteDir, "bundle-inventory.json")] = invData
	for _, e := range inv.Entries {
		data, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(e.Path)))
		if err != nil {
			t.Fatal(err)
		}
		dl.files[filepath.Join(remoteDir, filepath.FromSlash(e.Path))] = data
	}
	return dl
}

func TestCollectRunVerifiesAndMarksComplete(t *testing.T) {
	src, plan := collectTestBundle(t)
	cfg := testConfig(t)
	cfg.Paths.ResultRoot = t.TempDir()
	remoteRoot := "/var/lib/iris-bench"
	remoteDir := filepath.Join(remoteRoot, plan.RunID)
	dl := serveBundle(t, src, remoteDir)

	if err := collectRun(context.Background(), cfg, "codedang8", remoteRoot, plan.RunID, dl); err != nil {
		t.Fatal(err)
	}
	finalDir := filepath.Join(cfg.Paths.ResultRoot, plan.RunID)
	for _, name := range []string{"manifest.json", "COMPLETE", "collection.json", "checksums.sha256", "plan.json"} {
		if _, err := os.Stat(filepath.Join(finalDir, name)); err != nil {
			t.Fatalf("%s missing from collected bundle: %v", name, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(finalDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m manifest.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	digest, err := plan.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if m.RunID != plan.RunID || m.PlanSHA256 != digest {
		t.Fatalf("manifest identity = %s/%s, want %s/%s", m.RunID, m.PlanSHA256, plan.RunID, digest)
	}
	if m.Comparable || len(m.Overrides) == 0 {
		t.Fatal("bundle without an Ansible qualification report was not explicitly non-comparable")
	}
	if m.AgentVersion != "test-agent" {
		t.Fatalf("agent version = %q", m.AgentVersion)
	}
	completeData, err := os.ReadFile(filepath.Join(finalDir, "COMPLETE"))
	if err != nil {
		t.Fatal(err)
	}
	var complete struct {
		Accepted   bool `json:"accepted"`
		Comparable bool `json:"comparable"`
		Qualified  bool `json:"qualified"`
	}
	if err := json.Unmarshal(completeData, &complete); err != nil {
		t.Fatal(err)
	}
	if !complete.Accepted || complete.Comparable || complete.Qualified {
		t.Fatalf("COMPLETE qualification semantics = %+v", complete)
	}
	checksums, err := os.ReadFile(filepath.Join(finalDir, "checksums.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(checksums), "  COMPLETE\n") || strings.Contains(string(checksums), "  checksums.sha256\n") {
		t.Fatalf("checksum coverage = %q", checksums)
	}
}

func TestCollectRunRejectsSecretBeforeComplete(t *testing.T) {
	src, plan := collectTestBundle(t)
	if err := os.MkdirAll(filepath.Join(src, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	leak := "postgres://benchuser:supersecret@db.example:5432/app\n"
	if err := os.WriteFile(filepath.Join(src, "logs", "leak.txt"), []byte(leak), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.Paths.ResultRoot = t.TempDir()
	remoteRoot := "/var/lib/iris-bench"
	remoteDir := filepath.Join(remoteRoot, plan.RunID)
	dl := serveBundle(t, src, remoteDir)

	err := collectRun(context.Background(), cfg, "codedang8", remoteRoot, plan.RunID, dl)
	if err == nil || !strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v, want secret-scan rejection", err)
	}
	if _, statErr := os.Stat(filepath.Join(cfg.Paths.ResultRoot, plan.RunID)); !os.IsNotExist(statErr) {
		t.Fatal("bundle was promoted despite a failed secret scan")
	}
}

func TestBuildCollectManifestRequiresPlan(t *testing.T) {
	if _, err := buildCollectManifest(t.TempDir()); err == nil {
		t.Fatal("buildCollectManifest accepted a bundle without plan.json")
	}
}

func TestCollectRejectsMissingAcceptanceRecord(t *testing.T) {
	src, plan := collectTestBundle(t)
	if err := os.Remove(filepath.Join(src, "cleaned.json")); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.Paths.ResultRoot = t.TempDir()
	remoteRoot := "/var/lib/iris-bench"
	err := collectRun(context.Background(), cfg, "codedang8", remoteRoot, plan.RunID, serveBundle(t, src, filepath.Join(remoteRoot, plan.RunID)))
	if err == nil || !strings.Contains(err.Error(), "cleaned") {
		t.Fatalf("err = %v, want missing cleanup evidence rejection", err)
	}
	if _, statErr := os.Stat(filepath.Join(cfg.Paths.ResultRoot, plan.RunID, "COMPLETE")); !os.IsNotExist(statErr) {
		t.Fatal("COMPLETE was written without cleanup evidence")
	}
}

func TestCollectRejectsMissingTelemetryBeforeComplete(t *testing.T) {
	src, plan := collectTestBundle(t)
	if err := os.Remove(filepath.Join(src, "telemetry", "thermal-"+plan.Blocks[0].ID+".ndjson")); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.Paths.ResultRoot = t.TempDir()
	remoteRoot := "/var/lib/iris-bench"
	err := collectRun(context.Background(), cfg, "codedang8", remoteRoot, plan.RunID, serveBundle(t, src, filepath.Join(remoteRoot, plan.RunID)))
	if err == nil || !strings.Contains(err.Error(), "telemetry") {
		t.Fatalf("err = %v, want telemetry rejection", err)
	}
	if _, statErr := os.Stat(filepath.Join(cfg.Paths.ResultRoot, plan.RunID, "COMPLETE")); !os.IsNotExist(statErr) {
		t.Fatal("COMPLETE was written without telemetry evidence")
	}
}

func TestCollectRevalidatesCopiedQualificationReport(t *testing.T) {
	src, plan := collectTestBundle(t)
	want := plan.Target.HostIdentity
	if want == "" {
		want = plan.Target.SSHAlias
	}
	report := []byte(fmt.Sprintf(`{"schema":"iris-benchmark-qualification/v1","inventory_hostname":%q,"compatible":true,"verification":{"failures":[]}}`, want))
	plan.Qualification.Report = &runplan.StagedFile{Path: stagedPath(plan.RunID, "qualification.json"), SHA256: artifact.HashBytes(report)}
	if err := plan.WriteStore(filepath.Join(src, "plan.json")); err != nil {
		t.Fatal(err)
	}
	sha, err := plan.Digest()
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"prepared", "qualified", "validated", "cleaned"} {
		rec := agentRecord{RunID: plan.RunID, PlanSHA256: sha, AgentVersion: "test-agent", Action: action}
		if err := artifact.WriteJSONAtomic(filepath.Join(src, action+".json"), rec, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := artifact.WriteFileAtomic(filepath.Join(src, "qualification", "ansible-report.json"), []byte(`{"tampered":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.Paths.ResultRoot = t.TempDir()
	remoteRoot := "/var/lib/iris-bench"
	err = collectRun(context.Background(), cfg, "codedang8", remoteRoot, plan.RunID, serveBundle(t, src, filepath.Join(remoteRoot, plan.RunID)))
	if err == nil || !strings.Contains(err.Error(), "qualification report sha256") {
		t.Fatalf("err = %v, want copied qualification rejection", err)
	}
	if _, statErr := os.Stat(filepath.Join(cfg.Paths.ResultRoot, plan.RunID, "COMPLETE")); !os.IsNotExist(statErr) {
		t.Fatal("COMPLETE was written for tampered qualification evidence")
	}
}
