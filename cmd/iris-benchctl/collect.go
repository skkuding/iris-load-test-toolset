package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/skkuding/iris-load-test-toolset/internal/artifact"
	"github.com/skkuding/iris-load-test-toolset/internal/config"
	"github.com/skkuding/iris-load-test-toolset/internal/manifest"
	"github.com/skkuding/iris-load-test-toolset/internal/runplan"
)

// sshDownloader is the file-transfer boundary used by collect.
type sshDownloader interface {
	Download(ctx context.Context, remotePath, localPath string) error
}

func cmdCollect(args []string) error {
	fs := newFlagSet("collect")
	var (
		configPath = fs.String("config", "", "configuration JSON file")
		host       = fs.String("host", "", "target SSH alias")
		runID      = fs.String("run", "", "run id to collect")
		remoteRoot = fs.String("remote-var-root", "/var/lib/iris-bench", "remote non-secret state root")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *runID == "" {
		return errors.New("--run is required")
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if *host == "" {
		return errors.New("--host is required")
	}
	if _, ok := cfg.AllowedHost(*host); !ok {
		return fmt.Errorf("host %q is not allowlisted", *host)
	}
	return collectRun(context.Background(), cfg, *host, *remoteRoot, *runID, makeSSH(cfg, *host))
}

// collectRun downloads, verifies, scans, and manifests a run bundle. It takes
// the transfer boundary as an argument so the whole flow can run against a
// fake SSH in tests.
func collectRun(ctx context.Context, cfg config.Config, host, remoteRoot, runID string, ssh sshDownloader) error {
	finalDir := filepath.Join(cfg.Paths.ResultRoot, runID)
	if _, err := os.Stat(finalDir); err == nil {
		return fmt.Errorf("result directory %s already exists", finalDir)
	}
	if err := os.MkdirAll(cfg.Paths.ResultRoot, 0o755); err != nil {
		return err
	}
	// The inventory file lives outside the bundle directory so VerifyComplete
	// sees only files the inventory lists.
	workDir, err := os.MkdirTemp(cfg.Paths.ResultRoot, ".collect-"+runID+"-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)
	bundleDir := filepath.Join(workDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		return err
	}

	remoteDir := filepath.Join(remoteRoot, runID)
	invLocal := filepath.Join(workDir, "bundle-inventory.json")
	if err := ssh.Download(ctx, filepath.Join(remoteDir, "bundle-inventory.json"), invLocal); err != nil {
		return fmt.Errorf("download inventory: %w", err)
	}
	inv, err := readInventory(invLocal)
	if err != nil {
		return err
	}
	for _, e := range inv.Entries {
		local := filepath.Join(bundleDir, filepath.FromSlash(e.Path))
		if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
			return err
		}
		if err := ssh.Download(ctx, filepath.Join(remoteDir, filepath.FromSlash(e.Path)), local); err != nil {
			return fmt.Errorf("download %q: %w", e.Path, err)
		}
	}
	// Every listed file must be present and no unlisted file may remain.
	if err := artifact.VerifyComplete(bundleDir, inv); err != nil {
		return fmt.Errorf("verify bundle: %w", err)
	}
	// The artifact validator scans text outputs for credential-shaped values
	// and private key material before a bundle may be marked complete.
	if err := artifact.ScanForSecrets(bundleDir, inv, nil); err != nil {
		return fmt.Errorf("secret scan: %w", err)
	}

	m, err := buildCollectManifest(bundleDir)
	if err != nil {
		return err
	}
	m.AddOutcome("artifact-completeness", "passed", fmt.Sprintf("%d listed file(s)", len(inv.Entries)))
	m.AddOutcome("secret-scan", "passed", "")
	if err := m.SecretsAbsent(nil); err != nil {
		return err
	}
	if err := m.WriteAtomic(filepath.Join(bundleDir, "manifest.json")); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	if err := writeChecksums(bundleDir, inv); err != nil {
		return err
	}
	marker := map[string]any{
		"runId":         runID,
		"entries":       len(inv.Entries),
		"schemaVersion": inv.SchemaVersion,
		"verified":      true,
	}
	if err := artifact.WriteJSONAtomic(filepath.Join(bundleDir, "collection.json"), marker, 0o644); err != nil {
		return err
	}
	complete := map[string]any{
		"runId":       runID,
		"planSha256":  m.PlanSHA256,
		"collectedAt": time.Now().UTC(),
		"entries":     len(inv.Entries),
	}
	if err := artifact.WriteJSONAtomic(filepath.Join(bundleDir, "COMPLETE"), complete, 0o644); err != nil {
		return err
	}
	if err := os.Rename(bundleDir, finalDir); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "collected %d verified artifact(s) into %s\n", len(inv.Entries), finalDir)
	return nil
}

// buildCollectManifest rebuilds the canonical run manifest from the collected
// plan and agent records. The plan's canonical digest is the identity every
// request and artifact carried.
func buildCollectManifest(bundleDir string) (manifest.Manifest, error) {
	data, err := os.ReadFile(filepath.Join(bundleDir, "plan.json"))
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("read collected plan: %w", err)
	}
	var plan runplan.Plan
	if err := json.Unmarshal(data, &plan); err != nil {
		return manifest.Manifest{}, fmt.Errorf("parse collected plan: %w", err)
	}
	if err := plan.Validate(); err != nil {
		return manifest.Manifest{}, err
	}
	digest, err := plan.Digest()
	if err != nil {
		return manifest.Manifest{}, err
	}
	if recorded := readRecordedPlanSHA(bundleDir); recorded != "" && recorded != digest {
		return manifest.Manifest{}, fmt.Errorf("collected plan digest %s does not match agent record %s", digest, recorded)
	}
	commit, dirty := buildGitInfo()
	return manifest.Build(plan, digest, readAgentVersion(bundleDir), commit, dirty, time.Now())
}

// agentRecord is the subset of an agent phase record collect needs.
type agentRecord struct {
	AgentVersion string `json:"agentVersion"`
	PlanSHA256   string `json:"planSha256"`
}

// readAgentVersion reads the agent version from the prepared record. It is
// advisory: a missing or unreadable record leaves the field empty.
func readAgentVersion(bundleDir string) string {
	rec, ok := readAgentRecord(bundleDir)
	if !ok {
		return ""
	}
	return rec.AgentVersion
}

// readRecordedPlanSHA returns the canonical plan digest the agent recorded in
// its validated (preferred) or prepared record, or "" when neither is present.
func readRecordedPlanSHA(bundleDir string) string {
	rec, ok := readAgentRecord(bundleDir)
	if !ok {
		return ""
	}
	return rec.PlanSHA256
}

func readAgentRecord(bundleDir string) (agentRecord, bool) {
	for _, name := range []string{"validated.json", "prepared.json"} {
		data, err := os.ReadFile(filepath.Join(bundleDir, name))
		if err != nil {
			continue
		}
		var rec agentRecord
		if err := json.Unmarshal(data, &rec); err == nil {
			return rec, true
		}
	}
	return agentRecord{}, false
}

// buildGitInfo reports the VCS revision and dirty state of the controller
// build, when the Go toolchain recorded it.
func buildGitInfo() (string, bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	var commit string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			commit = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	return commit, dirty
}

// writeChecksums writes the standard sha256 manifest for the listed files.
func writeChecksums(bundleDir string, inv artifact.Inventory) error {
	var buf bytes.Buffer
	for _, e := range inv.Entries {
		fmt.Fprintf(&buf, "%s  %s\n", e.SHA256, e.Path)
	}
	return artifact.WriteFileAtomic(filepath.Join(bundleDir, "checksums.sha256"), buf.Bytes(), 0o644)
}

func readInventory(path string) (artifact.Inventory, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return artifact.Inventory{}, err
	}
	var inv artifact.Inventory
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&inv); err != nil {
		return artifact.Inventory{}, fmt.Errorf("parse inventory: %w", err)
	}
	if err := inv.Validate(); err != nil {
		return artifact.Inventory{}, err
	}
	return inv, nil
}
