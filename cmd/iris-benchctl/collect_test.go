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
	sample := `{"runId":"` + plan.RunID + `","blockId":"isolated-1s-01","worker":"worker-01","cgroupPath":"/sys/fs/cgroup/iris-bench/run/block/worker-01/sandbox-x","cgroupContained":true}` + "\n"
	if err := os.WriteFile(filepath.Join(src, "samples", "judger.ndjson"), []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := fmt.Sprintf("{\"runId\":%q,\"planSha256\":%q,\"agentVersion\":\"test-agent\",\"action\":\"prepared\"}\n", plan.RunID, sha)
	if err := os.WriteFile(filepath.Join(src, "prepared.json"), []byte(rec), 0o644); err != nil {
		t.Fatal(err)
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
	if !m.Comparable {
		t.Fatal("bundle with passing checks marked non-comparable")
	}
	if m.AgentVersion != "test-agent" {
		t.Fatalf("agent version = %q", m.AgentVersion)
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
