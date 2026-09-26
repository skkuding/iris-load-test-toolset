package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skkuding/iris-load-test-toolset/internal/artifact"
	"github.com/skkuding/iris-load-test-toolset/internal/containment"
	"github.com/skkuding/iris-load-test-toolset/internal/experiment"
	"github.com/skkuding/iris-load-test-toolset/internal/protocol"
	"github.com/skkuding/iris-load-test-toolset/internal/runplan"
	"github.com/skkuding/iris-load-test-toolset/internal/telemetry"
)

const testRunID = "iris-20260925-abcdefgh"

func newTestAgent(t *testing.T) *agent {
	t.Helper()
	return &agent{
		varRoot: t.TempDir(),
		runRoot: t.TempDir(),
		version: "test",
		stderr:  io.Discard,
	}
}

// buildTestPlan returns a structurally valid plan for the direct suite.
func buildTestPlan(t *testing.T, suite string, workers int) runplan.Plan {
	t.Helper()
	fixtureDir := t.TempDir()
	input := filepath.Join(fixtureDir, "input.txt")
	expected := filepath.Join(fixtureDir, "expected.txt")
	if err := os.WriteFile(input, []byte("1 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(expected, []byte("3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inputSHA, _, _ := artifact.HashFile(input)
	expectedSHA, _, _ := artifact.HashFile(expected)
	plan, err := runplan.Build(runplan.BuildInput{
		ToolVersion:    "test",
		RunID:          testRunID,
		Target:         runplan.Target{SSHAlias: "host"},
		Suite:          suite,
		Profile:        "isolated",
		Images:         map[string]runplan.Image{"iris": {Reference: runplan.DefaultIrisImage, Digest: "sha256:" + strings.Repeat("a", 64)}},
		JudgerDigest:   "sha256:" + strings.Repeat("b", 64),
		WorkloadBinary: runplan.StagedFile{Path: input, SHA256: inputSHA},
		Fixtures:       []runplan.Fixture{{Name: "fixture", Path: input, SHA256: inputSHA, ExpectedOutputPath: expected, ExpectedOutputSHA256: expectedSHA}},
		Blocks: []runplan.Block{{
			ID: "block-01", Suite: suite, Profile: "isolated",
			Workers: workers, CPUList: "0-3", Repetitions: 2,
		}},
		Qualification: runplan.Qualification{RequireCgroupV2: true, MinPhysicalCores: 1, MaxLoad1: 1e9, MaxRunSeconds: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// stageBuiltPlan writes a valid plan to the run dir and returns its canonical
// digest, which is what every request carries.
func stageBuiltPlan(t *testing.T, a *agent, suite string, workers int) string {
	t.Helper()
	plan := buildTestPlan(t, suite, workers)
	dir := a.runDir(testRunID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "qualification"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "qualification", "host-facts.json"), []byte(`{"cgroupVersion":"v2","physicalCores":"4","load1":"0.5"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := plan.WriteStore(filepath.Join(dir, "plan.json")); err != nil {
		t.Fatal(err)
	}
	digest, err := plan.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func execAgent(t *testing.T, a *agent, req protocol.Request) ([]protocol.Event, protocol.Event, error) {
	t.Helper()
	var buf bytes.Buffer
	enc := protocol.NewEventEncoder(&buf)
	err := a.execute(context.Background(), req, enc)
	r := protocol.NewEventReader(&buf)
	var events []protocol.Event
	var terminal protocol.Event
	for {
		ev, derr := r.Decode()
		if errors.Is(derr, io.EOF) {
			break
		}
		if derr != nil {
			t.Fatalf("decode events: %v (raw=%q)", derr, buf.String())
		}
		events = append(events, ev)
		if ev.Kind == protocol.KindResult {
			terminal = ev
		}
	}
	if err := r.Finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
	return events, terminal, err
}

func request(action protocol.Action, planSHA, blockID string) protocol.Request {
	return protocol.Request{
		ProtocolVersion: protocol.Version,
		OperationID:     "op-1",
		RunID:           testRunID,
		Action:          action,
		PlanSHA256:      planSHA,
		BlockID:         blockID,
	}
}

func TestInspectWritesFacts(t *testing.T) {
	a := newTestAgent(t)
	sha := stageBuiltPlan(t, a, "judger", 1)
	_, terminal, err := execAgent(t, a, request(protocol.ActionInspect, sha, ""))
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Status != protocol.StatusCompleted {
		t.Fatalf("terminal = %+v", terminal)
	}
	if _, err := os.Stat(filepath.Join(a.runDir(testRunID), "qualification", "host-facts.json")); err != nil {
		t.Fatalf("facts not written: %v", err)
	}
}

func TestInspectCopiesValidatedQualificationReport(t *testing.T) {
	a := newTestAgent(t)
	plan := buildTestPlan(t, "judger", 1)
	reportPath := filepath.Join(t.TempDir(), "report.json")
	report := []byte(`{"schema":"iris-benchmark-qualification/v1","inventory_hostname":"host","nodename":"node","compatible":true,"verification":{"failures":[]}}` + "\n")
	if err := os.WriteFile(reportPath, report, 0o644); err != nil {
		t.Fatal(err)
	}
	plan.Qualification.Report = &runplan.StagedFile{Path: reportPath, SHA256: artifact.HashBytes(report)}
	a.planFile = filepath.Join(t.TempDir(), "plan.json")
	if err := plan.WriteStore(a.planFile); err != nil {
		t.Fatal(err)
	}
	sha, _ := plan.Digest()
	_, terminal, err := execAgent(t, a, request(protocol.ActionInspect, sha, ""))
	if err != nil || terminal.Status != protocol.StatusCompleted {
		t.Fatalf("inspect err=%v terminal=%+v", err, terminal)
	}
	got, err := os.ReadFile(filepath.Join(a.runDir(testRunID), "qualification", "ansible-report.json"))
	if err != nil || !bytes.Equal(got, report) {
		t.Fatalf("qualification evidence = %q, %v", got, err)
	}
}

func TestQualificationReportFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"schema", `{"schema":"other","inventory_hostname":"host","compatible":true,"verification":{"failures":[]}}`},
		{"compatibility", `{"schema":"iris-benchmark-qualification/v1","inventory_hostname":"host","compatible":false,"verification":{"failures":[]}}`},
		{"verification", `{"schema":"iris-benchmark-qualification/v1","inventory_hostname":"host","compatible":true,"verification":{"failures":["bad"]}}`},
		{"missing-verification", `{"schema":"iris-benchmark-qualification/v1","inventory_hostname":"host","compatible":true}`},
		{"identity", `{"schema":"iris-benchmark-qualification/v1","inventory_hostname":"other","compatible":true,"verification":{"failures":[]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAgent(t)
			plan := buildTestPlan(t, "judger", 1)
			path := filepath.Join(t.TempDir(), "report.json")
			data := []byte(tc.body)
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
			plan.Qualification.Report = &runplan.StagedFile{Path: path, SHA256: artifact.HashBytes(data)}
			if err := a.validateQualificationReport(path, plan); err == nil {
				t.Fatal("invalid qualification report accepted")
			}
		})
	}
}

func TestPrepareRevalidatesQualificationEvidence(t *testing.T) {
	a := newTestAgent(t)
	plan := buildTestPlan(t, "judger", 1)
	reportPath := filepath.Join(t.TempDir(), "report.json")
	report := []byte(`{"schema":"iris-benchmark-qualification/v1","inventory_hostname":"host","compatible":true,"verification":{"failures":[]}}`)
	if err := os.WriteFile(reportPath, report, 0o644); err != nil {
		t.Fatal(err)
	}
	plan.Qualification.Report = &runplan.StagedFile{Path: reportPath, SHA256: artifact.HashBytes(report)}
	a.planFile = filepath.Join(t.TempDir(), "plan.json")
	if err := plan.WriteStore(a.planFile); err != nil {
		t.Fatal(err)
	}
	sha, _ := plan.Digest()
	if _, _, err := execAgent(t, a, request(protocol.ActionInspect, sha, "")); err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(a.runDir(testRunID), "qualification", "ansible-report.json")
	if err := os.WriteFile(evidence, []byte(`{"tampered":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, terminal, err := execAgent(t, a, request(protocol.ActionPrepare, sha, ""))
	if !errors.Is(err, errTerminalFailed) || terminal.Status != protocol.StatusFailed || !strings.Contains(terminal.Message, "sha256") {
		t.Fatalf("prepare err=%v terminal=%+v", err, terminal)
	}
}

func TestValidateRevalidatesQualificationEvidence(t *testing.T) {
	a := newTestAgent(t)
	plan := buildTestPlan(t, "judger", 1)
	reportPath := filepath.Join(t.TempDir(), "report.json")
	report := []byte(`{"schema":"iris-benchmark-qualification/v1","inventory_hostname":"host","compatible":true,"verification":{"failures":[]}}`)
	if err := os.WriteFile(reportPath, report, 0o644); err != nil {
		t.Fatal(err)
	}
	plan.Qualification.Report = &runplan.StagedFile{Path: reportPath, SHA256: artifact.HashBytes(report)}
	a.planFile = filepath.Join(t.TempDir(), "plan.json")
	if err := plan.WriteStore(a.planFile); err != nil {
		t.Fatal(err)
	}
	sha, _ := plan.Digest()
	if _, _, err := execAgent(t, a, request(protocol.ActionInspect, sha, "")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := execAgent(t, a, request(protocol.ActionPrepare, sha, "")); err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(a.runDir(testRunID), "qualification", "ansible-report.json")
	if err := os.WriteFile(evidence, []byte(`{"tampered":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, terminal, err := execAgent(t, a, request(protocol.ActionValidate, sha, ""))
	if !errors.Is(err, errTerminalFailed) || terminal.Status != protocol.StatusFailed || !strings.Contains(terminal.Message, "sha256") {
		t.Fatalf("validate err=%v terminal=%+v", err, terminal)
	}
	if _, statErr := os.Stat(filepath.Join(a.runDir(testRunID), "validated.json")); !os.IsNotExist(statErr) {
		t.Fatal("tampered qualification evidence produced validated acceptance")
	}
}

func TestPrepareVerifiesAndStoresPlan(t *testing.T) {
	a := newTestAgent(t)
	sha := stageBuiltPlan(t, a, "judger", 1)
	_, terminal, err := execAgent(t, a, request(protocol.ActionPrepare, sha, ""))
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Status != protocol.StatusCompleted {
		t.Fatalf("terminal = %+v", terminal)
	}
	if _, err := os.Stat(filepath.Join(a.runDir(testRunID), "prepared.json")); err != nil {
		t.Fatal("prepared record missing")
	}
}

func TestPrepareRejectsDigestMismatch(t *testing.T) {
	a := newTestAgent(t)
	stageBuiltPlan(t, a, "judger", 1)
	wrong := strings.Repeat("f", 64)
	_, terminal, err := execAgent(t, a, request(protocol.ActionPrepare, wrong, ""))
	if err == nil || !errors.Is(err, errTerminalFailed) {
		t.Fatalf("err = %v, want errTerminalFailed", err)
	}
	if terminal.Status != protocol.StatusFailed {
		t.Fatalf("terminal = %+v", terminal)
	}
}

func TestPrepareRejectsFailedQualification(t *testing.T) {
	a := newTestAgent(t)
	sha := stageBuiltPlan(t, a, "judger", 1)
	facts := filepath.Join(a.runDir(testRunID), "qualification", "host-facts.json")
	if err := os.WriteFile(facts, []byte(`{"cgroupVersion":"v1","physicalCores":"4"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, terminal, err := execAgent(t, a, request(protocol.ActionPrepare, sha, ""))
	if !errors.Is(err, errTerminalFailed) || terminal.Status != protocol.StatusFailed {
		t.Fatalf("prepare err=%v terminal=%+v", err, terminal)
	}
	if _, err := os.Stat(filepath.Join(a.runDir(testRunID), "qualified.json")); !os.IsNotExist(err) {
		t.Fatal("failed host qualification produced an acceptance record")
	}
}

func TestPrepareRejectsLoadAboveSealedMaximum(t *testing.T) {
	a := newTestAgent(t)
	plan := buildTestPlan(t, "judger", 1)
	plan.Qualification.MaxLoad1 = 1.0
	a.planFile = filepath.Join(t.TempDir(), "plan.json")
	if err := plan.WriteStore(a.planFile); err != nil {
		t.Fatal(err)
	}
	sha, _ := plan.Digest()
	if _, _, err := execAgent(t, a, request(protocol.ActionInspect, sha, "")); err != nil {
		t.Fatal(err)
	}
	facts := filepath.Join(a.runDir(testRunID), "qualification", "host-facts.json")
	if err := os.WriteFile(facts, []byte(`{"cgroupVersion":"v2","physicalCores":"4","load1":"1.01"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, terminal, err := execAgent(t, a, request(protocol.ActionPrepare, sha, ""))
	if !errors.Is(err, errTerminalFailed) || terminal.Status != protocol.StatusFailed || !strings.Contains(terminal.Message, "load1") {
		t.Fatalf("prepare err=%v terminal=%+v", err, terminal)
	}
}

func TestPrepareRejectsAgentVersionMismatch(t *testing.T) {
	a := newTestAgent(t)
	sha := stageBuiltPlan(t, a, "judger", 1)
	a.version = "different"
	_, terminal, err := execAgent(t, a, request(protocol.ActionPrepare, sha, ""))
	if !errors.Is(err, errTerminalFailed) || terminal.Status != protocol.StatusFailed || !strings.Contains(terminal.Message, "version") {
		t.Fatalf("prepare err=%v terminal=%+v", err, terminal)
	}
}

func TestRunBlockRejectsUnpreparedRun(t *testing.T) {
	a := newTestAgent(t)
	a.cgroupParent = "/sys/fs/cgroup/iris-bench"
	_, terminal, err := execAgent(t, a, request(protocol.ActionRunBlock, strings.Repeat("a", 64), "block-01"))
	if !errors.Is(err, errTerminalFailed) {
		t.Fatalf("err = %v, want errTerminalFailed", err)
	}
	if terminal.Status != protocol.StatusFailed {
		t.Fatalf("terminal = %+v", terminal)
	}
}

func TestRunBlockRequiresCgroupParent(t *testing.T) {
	a := newTestAgent(t)
	sha := stageReadyRun(t, a, "judger", 1)
	_, terminal, err := execAgent(t, a, request(protocol.ActionRunBlock, sha, "block-01"))
	if !errors.Is(err, errTerminalFailed) {
		t.Fatalf("err = %v, want errTerminalFailed", err)
	}
	if terminal.Status != protocol.StatusUnsupported {
		t.Fatalf("terminal = %+v", terminal)
	}
}

func TestBundleBuildsInventory(t *testing.T) {
	a := newTestAgent(t)
	sha := stageBuiltPlan(t, a, "judger", 1)
	if _, _, err := execAgent(t, a, request(protocol.ActionPrepare, sha, "")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.runDir(testRunID), "samples.ndjson"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"validated", "cleaned"} {
		if _, err := a.writeRecord(testRunID, sha, action, "test"); err != nil {
			t.Fatal(err)
		}
	}
	events, terminal, err := execAgent(t, a, request(protocol.ActionBundle, sha, ""))
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Status != protocol.StatusCompleted {
		t.Fatalf("terminal = %+v", terminal)
	}
	if _, err := os.Stat(filepath.Join(a.runDir(testRunID), "bundle-inventory.json")); err != nil {
		t.Fatal("inventory missing")
	}
	var artifactEvent bool
	for _, ev := range events {
		if ev.Kind == protocol.KindArtifact && ev.Path == "bundle-inventory.json" {
			artifactEvent = true
		}
	}
	if !artifactEvent {
		t.Fatal("no artifact event for inventory")
	}
	if _, _, err := execAgent(t, a, request(protocol.ActionBundle, sha, "")); err != nil {
		t.Fatalf("second bundle: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(a.runDir(testRunID), "bundle-inventory.json"))
	if err != nil {
		t.Fatal(err)
	}
	var inv artifact.Inventory
	if err := json.Unmarshal(data, &inv); err != nil {
		t.Fatal(err)
	}
	for _, entry := range inv.Entries {
		if entry.Path == "bundle-inventory.json" {
			t.Fatal("inventory included its previous version")
		}
	}
}

func TestCleanupOnlyRemovesRuntimeDir(t *testing.T) {
	a := newTestAgent(t)
	sha := stageBuiltPlan(t, a, "judger", 1)
	if _, _, err := execAgent(t, a, request(protocol.ActionPrepare, sha, "")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.rtDir(testRunID), "secret.env"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, terminal, err := execAgent(t, a, request(protocol.ActionCleanup, sha, ""))
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Status != protocol.StatusCompleted {
		t.Fatalf("terminal = %+v", terminal)
	}
	if _, err := os.Stat(a.rtDir(testRunID)); !os.IsNotExist(err) {
		t.Fatal("runtime dir still present after cleanup")
	}
	if _, err := os.Stat(a.runDir(testRunID)); err != nil {
		t.Fatal("cleanup removed preserved evidence")
	}
}

func TestInvalidRequestFailsWithoutMutation(t *testing.T) {
	a := newTestAgent(t)
	bad := request(protocol.ActionPrepare, "not-a-digest", "")
	_, terminal, err := execAgent(t, a, bad)
	if err == nil {
		t.Fatal("expected failure for invalid request")
	}
	if terminal.Status != protocol.StatusFailed {
		t.Fatalf("terminal = %+v", terminal)
	}
}

// ---- run-block integration with fake boundaries ----

// stageReadyRun writes a valid run plan plus the qualification and prepared
// records the precondition check requires, then returns the staged plan file
// hash (the digest the controller carries).
func stageReadyRun(t *testing.T, a *agent, suite string, workers int) string {
	t.Helper()
	sha := stageBuiltPlan(t, a, suite, workers)
	dir := a.runDir(testRunID)
	if err := os.MkdirAll(filepath.Join(dir, "qualification"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "qualification", "host-facts.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	qualified := record{RunID: testRunID, PlanSHA256: sha, AgentVersion: "test", Action: "qualified", At: time.Now().UTC()}
	qualifiedData, _ := json.MarshalIndent(qualified, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "qualified.json"), append(qualifiedData, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := record{RunID: testRunID, PlanSHA256: sha, AgentVersion: "test", Action: "prepared", At: time.Now().UTC()}
	data, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "prepared.json"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return sha
}

type agentTestFS struct {
	files map[string]string
	dirs  map[string]bool
}

func newAgentTestFS() *agentTestFS {
	return &agentTestFS{files: map[string]string{}, dirs: map[string]bool{"/": true}}
}
func (f *agentTestFS) put(p, v string) { f.files[path.Clean(p)] = v }
func (f *agentTestFS) mkdir(p string) {
	original := path.Clean(p)
	p = original
	f.dirs[p] = true
	for {
		parent := path.Dir(p)
		if parent == p {
			break
		}
		f.dirs[parent] = true
		p = parent
	}
	// Every cgroup directory exposes an (initially empty) cgroup.procs.
	if _, ok := f.files[original+"/cgroup.procs"]; !ok {
		f.files[original+"/cgroup.procs"] = ""
	}
}
func (f *agentTestFS) ReadFile(name string) ([]byte, error) {
	if v, ok := f.files[path.Clean(name)]; ok {
		return []byte(v), nil
	}
	return nil, fmt.Errorf("open %s: %w", name, fs.ErrNotExist)
}
func (f *agentTestFS) WriteFile(name string, data []byte, _ fs.FileMode) error {
	clean := path.Clean(name)
	if !f.dirs[path.Dir(clean)] {
		return fmt.Errorf("write %s: %w", name, fs.ErrNotExist)
	}
	f.files[clean] = string(data)
	if _, ok := f.files[clean+".effective"]; !ok {
		f.files[clean+".effective"] = string(data)
	}
	return nil
}
func (f *agentTestFS) MkdirAll(p string, _ fs.FileMode) error { f.mkdir(p); return nil }
func (f *agentTestFS) Remove(p string) error {
	clean := path.Clean(p)
	if _, ok := f.files[clean]; ok {
		delete(f.files, clean)
		return nil
	}
	if f.dirs[clean] {
		if procs, ok := f.files[clean+"/cgroup.procs"]; ok && len(strings.Fields(procs)) > 0 {
			return fmt.Errorf("remove %s: directory not empty", p)
		}
		prefix := clean + "/"
		for k := range f.files {
			if k == clean || strings.HasPrefix(k, prefix) {
				delete(f.files, k)
			}
		}
		for k := range f.dirs {
			if k == clean || strings.HasPrefix(k, prefix) {
				delete(f.dirs, k)
			}
		}
		return nil
	}
	return fmt.Errorf("remove %s: %w", p, fs.ErrNotExist)
}
func (f *agentTestFS) Stat(name string) (fs.FileInfo, error) {
	clean := path.Clean(name)
	if f.dirs[clean] {
		return agentTestInfo{name: path.Base(clean), dir: true}, nil
	}
	if v, ok := f.files[clean]; ok {
		return agentTestInfo{name: path.Base(clean), size: int64(len(v))}, nil
	}
	return nil, fmt.Errorf("stat %s: %w", name, fs.ErrNotExist)
}
func (f *agentTestFS) ReadDir(name string) ([]fs.DirEntry, error) {
	clean := path.Clean(name)
	if !f.dirs[clean] {
		return nil, fmt.Errorf("readdir %s: %w", name, fs.ErrNotExist)
	}
	seen := map[string]bool{}
	prefix := clean + "/"
	add := func(p string, isDir bool) {
		rest := strings.TrimPrefix(p, prefix)
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			rest = rest[:i]
		}
		seen[rest] = isDir
	}
	for k := range f.files {
		if strings.HasPrefix(k, prefix) {
			add(k, false)
		}
	}
	for k := range f.dirs {
		if k != clean && strings.HasPrefix(k, prefix) {
			add(k, true)
		}
	}
	out := make([]fs.DirEntry, 0, len(seen))
	for n, isDir := range seen {
		out = append(out, agentTestInfo{name: n, dir: isDir})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

type agentTestInfo struct {
	name string
	size int64
	dir  bool
}

func (i agentTestInfo) Name() string               { return i.name }
func (i agentTestInfo) Size() int64                { return i.size }
func (i agentTestInfo) Mode() fs.FileMode          { return 0o644 }
func (i agentTestInfo) ModTime() time.Time         { return time.Time{} }
func (i agentTestInfo) IsDir() bool                { return i.dir }
func (i agentTestInfo) Sys() any                   { return nil }
func (i agentTestInfo) Info() (fs.FileInfo, error) { return i, nil }
func (i agentTestInfo) Type() fs.FileMode          { return i.Mode() }

type agentTestProc struct {
	cgroups map[int]string
}

func newAgentTestProc() *agentTestProc { return &agentTestProc{cgroups: map[int]string{}} }
func (p *agentTestProc) Cgroup(pid int) (string, error) {
	if cg, ok := p.cgroups[pid]; ok {
		return cg, nil
	}
	return "", fmt.Errorf("pid %d gone", pid)
}
func (p *agentTestProc) Descendants(pid int) ([]int, error) {
	if _, ok := p.cgroups[pid]; !ok {
		return nil, fmt.Errorf("pid %d gone", pid)
	}
	return []int{pid}, nil
}
func (p *agentTestProc) Exists(pid int) (bool, error) {
	_, ok := p.cgroups[pid]
	return ok, nil
}

type agentTestWorker struct {
	pid     int
	done    chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *agentTestWorker) PID() int    { return w.pid }
func (w *agentTestWorker) Wait() error { <-w.done; return nil }
func (w *agentTestWorker) releaseNow() { w.once.Do(func() { close(w.release) }) }
func (w *agentTestWorker) Stop() error { w.releaseNow(); return nil }

type agentTestLauncher struct {
	proc    *agentTestProc
	mu      sync.Mutex
	workers []*agentTestWorker
	specs   []experiment.WorkerSpec
}

type staticTelemetry struct{}

func (staticTelemetry) Snapshot() (telemetry.Snapshot, error) {
	return telemetry.Snapshot{TemperaturesMilliC: map[string]int64{"test-zone": 42000}}, nil
}

func (l *agentTestLauncher) Start(_ context.Context, spec experiment.WorkerSpec, h containment.Handle) (experiment.Worker, error) {
	pid := 5000 + len(l.workers)
	w := &agentTestWorker{pid: pid, done: make(chan struct{}), release: make(chan struct{})}
	l.workers = append(l.workers, w)
	l.specs = append(l.specs, spec)
	if l.proc != nil {
		l.proc.cgroups[pid] = h.RelPath()
	}
	go func() {
		<-w.release
		var b strings.Builder
		for i := 0; i < spec.ExpectedSamples; i++ {
			fmt.Fprintf(&b, "{\"runId\":%q,\"blockId\":\"block-01\",\"worker\":%q,\"cgroupPath\":%q,\"cgroupContained\":true,\"iteration\":%d,\"status\":\"success\",\"outputMatches\":true,\"containmentMode\":\"isolated\",\"comparable\":true}\n",
				testRunID, spec.ID, filepath.Join(h.FSPath(), "sandbox-"+spec.ID), i)
		}
		_ = os.WriteFile(spec.OutputPath, []byte(b.String()), 0o644)
		close(w.done)
	}()
	return w, nil
}
func (l *agentTestLauncher) WaitReady(context.Context) error { return nil }
func (l *agentTestLauncher) Release() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, w := range l.workers {
		w.releaseNow()
	}
	return nil
}
func (l *agentTestLauncher) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, w := range l.workers {
		_ = w.Stop()
	}
	return nil
}

func newRunBlockAgent(t *testing.T) (*agent, *agentTestFS, *agentTestProc, *agentTestLauncher) {
	t.Helper()
	a := newTestAgent(t)
	a.cgroupParent = "/sys/fs/cgroup/iris-bench"
	a.cgroupMount = "/sys/fs/cgroup"
	a.cpusetMems = "0"
	a.workerBin = "/bin/true"
	a.monitorInterval = time.Millisecond
	fakeFS := newAgentTestFS()
	fakeFS.put("/sys/fs/cgroup/cgroup.controllers", "cpu cpuset memory\n")
	fakeFS.mkdir("/sys/fs/cgroup/iris-bench")
	fakeFS.put("/sys/fs/cgroup/iris-bench/cgroup.subtree_control", "cpu cpuset\n")
	fakeFS.put("/sys/fs/cgroup/iris-bench/cgroup.procs", "")
	proc := newAgentTestProc()
	launcher := &agentTestLauncher{proc: proc}
	a.cgroupFS = fakeFS
	a.cgroupProc = proc
	a.newLauncher = func(int) (experiment.Launcher, error) { return launcher, nil }
	a.telemetry = staticTelemetry{}
	return a, fakeFS, proc, launcher
}

func TestRunBlockHappyPath(t *testing.T) {
	a, _, _, launcher := newRunBlockAgent(t)
	sha := stageReadyRun(t, a, "judger", 2)
	events, terminal, err := execAgent(t, a, request(protocol.ActionRunBlock, sha, "block-01"))
	if err != nil {
		t.Fatalf("run-block: %v", err)
	}
	if terminal.Status != protocol.StatusCompleted {
		t.Fatalf("terminal = %+v", terminal)
	}
	if _, err := os.Stat(filepath.Join(a.runDir(testRunID), "samples", "block-01.ndjson")); err != nil {
		t.Fatal("samples missing")
	}
	if _, err := os.Stat(filepath.Join(a.runDir(testRunID), "receipts", "block-01.json")); err != nil {
		t.Fatal("receipt missing")
	}
	// Every worker received a unique disjoint cgroup and CPU set.
	seen := map[string]bool{}
	for _, spec := range launcher.specs {
		if seen[spec.CPUList] {
			t.Fatalf("duplicate cpu set %q", spec.CPUList)
		}
		seen[spec.CPUList] = true
		if spec.CPUMax != "200000 100000" {
			t.Fatalf("cpu.max = %q", spec.CPUMax)
		}
	}
	var artifacts int
	for _, ev := range events {
		if ev.Kind == protocol.KindArtifact {
			artifacts++
		}
	}
	if artifacts < 2 {
		t.Fatalf("artifact events = %d, want >= 2", artifacts)
	}
}

func TestValidateRejectsMissingBlockTelemetry(t *testing.T) {
	a, _, _, _ := newRunBlockAgent(t)
	sha := stageReadyRun(t, a, "judger", 1)
	if _, _, err := execAgent(t, a, request(protocol.ActionRunBlock, sha, "block-01")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(a.runDir(testRunID), "telemetry", "thermal-block-01.ndjson")); err != nil {
		t.Fatal(err)
	}
	_, terminal, err := execAgent(t, a, request(protocol.ActionValidate, sha, ""))
	if !errors.Is(err, errTerminalFailed) || terminal.Status != protocol.StatusFailed || !strings.Contains(terminal.Message, "telemetry") {
		t.Fatalf("validate err=%v terminal=%+v", err, terminal)
	}
}

func TestRunBlockRejectsIrisSuite(t *testing.T) {
	a, _, _, _ := newRunBlockAgent(t)
	sha := stageReadyRun(t, a, "iris", 1)
	_, terminal, err := execAgent(t, a, request(protocol.ActionRunBlock, sha, "block-01"))
	if !errors.Is(err, errTerminalFailed) {
		t.Fatalf("err = %v", err)
	}
	if terminal.Status != protocol.StatusUnsupported {
		t.Fatalf("terminal = %+v", terminal)
	}
}

func TestBuildWorkerSpecsRejectsMissingCPUs(t *testing.T) {
	a := newTestAgent(t)
	_, err := a.buildWorkerSpecs(request(protocol.ActionRunBlock, strings.Repeat("a", 64), "block-01"), runplan.Block{ID: "block-01", Suite: "judger", Workers: 2, Repetitions: 1}, runplan.Plan{})
	if err == nil {
		t.Fatal("expected missing cpu list error")
	}
}

func TestBuildWorkerSpecsRejectsFixtureMismatch(t *testing.T) {
	a := newTestAgent(t)
	a.cgroupParent = "/sys/fs/cgroup/iris-bench"
	a.workerBin = "/bin/true"
	plan := buildTestPlan(t, "judger", 1)
	if err := os.WriteFile(plan.Fixtures[0].Path, []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := a.buildWorkerSpecs(request(protocol.ActionRunBlock, strings.Repeat("a", 64), "block-01"), plan.Blocks[0], plan)
	if err == nil || !strings.Contains(err.Error(), "does not match sealed") {
		t.Fatalf("fixture mismatch error = %v", err)
	}
}

func TestBuildWorkerSpecsRejectsWorkloadMismatch(t *testing.T) {
	a := newTestAgent(t)
	a.cgroupParent = "/sys/fs/cgroup/iris-bench"
	a.workerBin = "/bin/true"
	plan := buildTestPlan(t, "judger", 1)
	plan.WorkloadBinary.SHA256 = strings.Repeat("f", 64)
	_, err := a.buildWorkerSpecs(request(protocol.ActionRunBlock, strings.Repeat("a", 64), "block-01"), plan.Blocks[0], plan)
	if err == nil || !strings.Contains(err.Error(), "workload binary sha256") {
		t.Fatalf("workload mismatch error = %v", err)
	}
}

func TestWorkerArgumentsTemplate(t *testing.T) {
	a := newTestAgent(t)
	a.workerArgs = []string{"--worker", "{worker}", "--out", "{output}"}
	got, err := a.workerArguments("worker-01", runplan.Block{ID: "b", Repetitions: 3}, runplan.Plan{RunID: testRunID}, "0", "/tmp/o", runplan.Fixture{Name: "fix"},
		"/bin/true", "/sys/fs/cgroup/iris-bench/run/b/worker-01", "run-b-worker-01", strings.Repeat("b", 64), "", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--worker", "worker-01", "--out", "/tmp/o"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestWorkerArgumentsAlwaysForwardProductionCompat(t *testing.T) {
	a := newTestAgent(t)
	a.workerArgs = []string{"--worker", "{worker}"}
	got, err := a.workerArguments("worker-01", runplan.Block{ID: "b", Repetitions: 1}, runplan.Plan{RunID: testRunID, ContainmentMode: runplan.ContainmentProductionCompat}, "0", "/tmp/o", runplan.Fixture{Name: "fix"},
		"/bin/true", "/sys/fs/cgroup/iris-bench/run/b/worker-01", "run-b-worker-01", strings.Repeat("b", 64), "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !containsArgument(got, "--production-compat") {
		t.Fatalf("custom args omitted production compatibility: %v", got)
	}
}

func TestRunWorkerExecRejectsBadInput(t *testing.T) {
	if err := runWorkerExec(nil); err == nil {
		t.Fatal("expected error for missing flags")
	}
	if err := runWorkerExec([]string{"--cgroup", "/sys/fs/cgroup/x"}); err == nil {
		t.Fatal("expected error for missing command")
	}
}

func TestVerifyPlanMatchesAcceptsCanonicalDigest(t *testing.T) {
	a := newTestAgent(t)
	dir := a.runDir(testRunID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	plan, err := runplan.Build(runplan.BuildInput{
		ToolVersion:    "test",
		RunID:          testRunID,
		Target:         runplan.Target{SSHAlias: "host"},
		Suite:          "judger",
		Profile:        "isolated",
		Images:         map[string]runplan.Image{"iris": {Reference: runplan.DefaultIrisImage, Digest: "sha256:" + strings.Repeat("a", 64)}},
		JudgerDigest:   "sha256:" + strings.Repeat("b", 64),
		WorkloadBinary: runplan.StagedFile{Path: "/tmp/workload", SHA256: strings.Repeat("c", 64)},
		Qualification:  runplan.Qualification{MaxLoad1: 1.0},
		Blocks:         []runplan.Block{{ID: "block-01", Suite: "judger", Profile: "isolated", Workers: 1, CPUList: "0", Repetitions: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(dir, "plan.json")
	if err := plan.WriteStore(planPath); err != nil {
		t.Fatal(err)
	}
	digest, err := plan.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.verifyPlanMatches(testRunID, digest); err != nil {
		t.Fatalf("canonical digest rejected: %v", err)
	}
	if err := a.verifyPlanMatches(testRunID, strings.Repeat("f", 64)); err == nil {
		t.Fatal("wrong digest accepted")
	}
}

func TestVerifyPlanMatchesRejectsStagedFileHash(t *testing.T) {
	a := newTestAgent(t)
	plan := buildTestPlan(t, "judger", 1)
	dir := a.runDir(testRunID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(dir, "plan.json")
	if err := plan.WriteStore(planPath); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	fileHash := hex.EncodeToString(sum[:])
	if err := a.verifyPlanMatches(testRunID, fileHash); err == nil {
		t.Fatal("indented staged file hash was accepted; only the canonical digest is valid")
	}
	digest, err := plan.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.verifyPlanMatches(testRunID, digest); err != nil {
		t.Fatalf("canonical digest rejected: %v", err)
	}
}

func TestBuildWorkerSpecsDefaultArgsCarryJudgerContext(t *testing.T) {
	a := newTestAgent(t)
	a.cgroupParent = "/sys/fs/cgroup/iris-bench"
	a.workerBin = "/bin/true"
	plan := buildTestPlan(t, "judger", 1)
	specs, err := a.buildWorkerSpecs(request(protocol.ActionRunBlock, strings.Repeat("a", 64), "block-01"), plan.Blocks[0], plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 {
		t.Fatalf("specs = %d", len(specs))
	}
	joined := strings.Join(specs[0].Args, " ")
	subtree, err := experiment.SubtreeName(testRunID, "block-01", "worker-01")
	if err != nil {
		t.Fatal(err)
	}
	expectedParent := filepath.Join(a.cgroupParent, subtree)
	for _, want := range []string{
		"--container-id " + testRunID + "-block-01-worker-01",
		"--expected-cgroup-parent " + expectedParent,
		"--judger-sha256 " + strings.Repeat("b", 64),
		"--uid 0",
		"--gid 0",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("default args missing %q in %v", want, specs[0].Args)
		}
	}
}

func TestWorkerContainerIDPreservesUniqueSuffixAfterTruncation(t *testing.T) {
	plan := runplan.Plan{RunID: strings.Repeat("r", 128)}
	block := runplan.Block{ID: strings.Repeat("b", 128)}
	one := workerContainerID(strings.Repeat("x", 256), plan, block, "worker-01")
	two := workerContainerID(strings.Repeat("x", 256), plan, block, "worker-02")
	if len(one) != 128 || len(two) != 128 {
		t.Fatalf("container ID lengths = %d/%d, want 128", len(one), len(two))
	}
	if one == two || !strings.HasSuffix(one, "-worker-01") || !strings.HasSuffix(two, "-worker-02") {
		t.Fatalf("worker suffixes collided or were truncated: %q / %q", one, two)
	}
}

func TestPlanJudgerSHAFromPlan(t *testing.T) {
	a := newTestAgent(t)
	plan := buildTestPlan(t, "judger", 1)
	a.judgerSHA256 = strings.Repeat("9", 64)
	if _, err := a.planJudgerSHA(plan); err == nil {
		t.Fatal("mismatched --judger-sha256 accepted")
	}
	a.judgerSHA256 = "sha256:" + strings.Repeat("b", 64)
	got, err := a.planJudgerSHA(plan)
	if err != nil || got != strings.Repeat("b", 64) {
		t.Fatalf("planJudgerSHA = %q, %v", got, err)
	}
}

func TestPlanBenchmarkImageMustMatchSealedPlan(t *testing.T) {
	a := newTestAgent(t)
	plan := buildTestPlan(t, "judger", 1)
	imageID := "sha256:" + strings.Repeat("d", 64)
	plan.Images["judger-bench"] = runplan.Image{Reference: imageID, Digest: imageID}
	if _, _, err := a.planBenchmarkImage(plan); err == nil {
		t.Fatal("missing forwarded benchmark image accepted")
	}
	a.benchmarkImage = "sha256:" + strings.Repeat("e", 64)
	if _, _, err := a.planBenchmarkImage(plan); err == nil {
		t.Fatal("mismatched forwarded benchmark image accepted")
	}
	a.benchmarkImage = imageID
	got, oci, err := a.planBenchmarkImage(plan)
	if err != nil || !oci || got != imageID {
		t.Fatalf("planBenchmarkImage = %q, %v, %v", got, oci, err)
	}
}

func TestBuildWorkerSpecsOCIUsesDockerArgvAndImageJudger(t *testing.T) {
	a := newTestAgent(t)
	a.cgroupParent = "/sys/fs/cgroup/iris-bench"
	a.cgroupMount = "/sys/fs/cgroup"
	a.cpusetMems = "1"
	a.workerBin = "/host/judger-bench"
	plan := buildTestPlan(t, "judger", 1)
	imageID := "sha256:" + strings.Repeat("d", 64)
	plan.Images["judger-bench"] = runplan.Image{Reference: imageID, Digest: imageID}
	plan.ContainmentMode = runplan.ContainmentProductionCompat
	a.benchmarkImage = imageID
	specs, err := a.buildWorkerSpecs(request(protocol.ActionRunBlock, strings.Repeat("a", 64), "block-01"), plan.Blocks[0], plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].Command != "docker" {
		t.Fatalf("OCI worker specs = %+v", specs)
	}
	joined := strings.Join(specs[0].Args, " ")
	outputDir := filepath.Dir(specs[0].OutputPath)
	for _, want := range []string{
		"run --rm",
		"--name iris-bench-" + testRunID + "-block-01-worker-01",
		"--privileged --cgroupns=host",
		"--cpuset-cpus 0-3 --cpuset-mems 1",
		"--mount type=bind,src=/sys/fs/cgroup,dst=/sys/fs/cgroup",
		"--mount type=bind,src=" + plan.WorkloadBinary.Path + ",dst=" + plan.WorkloadBinary.Path + ",readonly",
		"--mount type=bind,src=" + plan.Fixtures[0].ExpectedOutputPath + ",dst=" + plan.Fixtures[0].ExpectedOutputPath + ",readonly",
		"--mount type=bind,src=" + outputDir + ",dst=" + outputDir,
		imageID + " --mode execute",
		"--judger " + ociJudgerPath,
		"--judger-sha256 " + strings.Repeat("b", 64),
		"--production-compat",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("docker argv missing %q in %v", want, specs[0].Args)
		}
	}
	if strings.Contains(joined, "/host/judger-bench") {
		t.Fatalf("OCI argv uses host worker/Judger path: %v", specs[0].Args)
	}
}

type fakeDockerRunner struct {
	outputs []string
	errs    []error
	calls   []string
}

func (f *fakeDockerRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(append([]string{name}, args...), " "))
	i := len(f.calls) - 1
	var err error
	if i < len(f.errs) {
		err = f.errs[i]
	}
	if i < len(f.outputs) {
		return []byte(f.outputs[i]), err
	}
	return nil, err
}

func TestCleanupContainersRemovesOnlyExpectedRunScopedNames(t *testing.T) {
	plan := buildTestPlan(t, "judger", 1)
	name := dockerContainerName(plan, plan.Blocks[0], "worker-01")
	runner := &fakeDockerRunner{outputs: []string{name + "\n", "removed\n", ""}}
	a := newTestAgent(t)
	a.dockerRunner = runner
	if err := a.cleanupContainers(context.Background(), plan.RunID, plan.Blocks[0].ID, []string{name}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 3 || !strings.Contains(runner.calls[1], "docker container rm -f "+name) {
		t.Fatalf("Docker cleanup calls = %v", runner.calls)
	}
}

func TestCleanupContainersRejectsUnexpectedLabeledContainer(t *testing.T) {
	plan := buildTestPlan(t, "judger", 1)
	name := dockerContainerName(plan, plan.Blocks[0], "worker-01")
	a := newTestAgent(t)
	a.dockerRunner = &fakeDockerRunner{outputs: []string{"other-container\n"}}
	if err := a.cleanupContainers(context.Background(), plan.RunID, plan.Blocks[0].ID, []string{name}); err == nil || !strings.Contains(err.Error(), "unexpected container") {
		t.Fatalf("cleanup error = %v", err)
	}
}

func TestDockerContainerNameRemainsValidAndUniqueWhenTruncated(t *testing.T) {
	plan := runplan.Plan{RunID: strings.Repeat("r", 128)}
	block := runplan.Block{ID: strings.Repeat("b", 128)}
	one := dockerContainerName(plan, block, "worker-01")
	two := dockerContainerName(plan, block, "worker-02")
	if !dockerNamePattern.MatchString(one) || !dockerNamePattern.MatchString(two) || one == two {
		t.Fatalf("invalid or colliding Docker names %q / %q", one, two)
	}
}

func cleanupTestManager(a *agent) *containment.Manager {
	return &containment.Manager{Mount: a.mount(), FS: a.cgroupFS, Proc: a.cgroupProc}
}

func TestCleanupSandboxRootRunsArgvOnlySealedHelper(t *testing.T) {
	a, fakeFS, _, _ := newRunBlockAgent(t)
	plan := buildTestPlan(t, "judger", 1)
	imageID := "sha256:" + strings.Repeat("d", 64)
	plan.Images["judger-bench"] = runplan.Image{Reference: imageID, Digest: imageID}
	block := plan.Blocks[0]
	fakeFS.mkdir("/sys/fs/cgroup/sandbox-run-worker-01")
	fakeFS.put("/sys/fs/cgroup/sandbox-run-worker-01/cgroup.procs", "")
	for _, b := range []string{"box-5", "box-1", "box-3", "box-2", "box-4"} {
		fakeFS.mkdir("/sys/fs/cgroup/sandbox-run-worker-01/" + b)
		fakeFS.put("/sys/fs/cgroup/sandbox-run-worker-01/"+b+"/cgroup.procs", "")
	}
	runner := &fakeDockerRunner{outputs: []string{""}}
	a.dockerRunner = runner

	if err := a.cleanupSandboxRoot(context.Background(), cleanupTestManager(a), plan, block, imageID, "/sandbox-run-worker-01"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("docker calls = %v, want exactly one", runner.calls)
	}
	call := runner.calls[0]
	for _, want := range []string{
		"docker run --rm",
		"--name " + dockerCleanupContainerName(plan, block, "/sandbox-run-worker-01"),
		"--label iris-bench.run=" + plan.RunID,
		"--label iris-bench.block=" + block.ID,
		"--privileged --cgroupns=host",
		"--entrypoint /bin/rmdir",
		"--mount type=bind,src=/sys/fs/cgroup,dst=/sys/fs/cgroup",
	} {
		if !strings.Contains(call, want) {
			t.Fatalf("cleanup argv missing %q in %q", want, call)
		}
	}
	// rmdir gets every validated path in one argv, children first and root last.
	wantPaths := imageID +
		" /sys/fs/cgroup/sandbox-run-worker-01/box-1" +
		" /sys/fs/cgroup/sandbox-run-worker-01/box-2" +
		" /sys/fs/cgroup/sandbox-run-worker-01/box-3" +
		" /sys/fs/cgroup/sandbox-run-worker-01/box-4" +
		" /sys/fs/cgroup/sandbox-run-worker-01/box-5" +
		" /sys/fs/cgroup/sandbox-run-worker-01"
	if !strings.HasSuffix(call, wantPaths) {
		t.Fatalf("cleanup argv tail = %q, want suffix %q", call, wantPaths)
	}
	for _, forbidden := range []string{"sh -c", "/bin/sh", "bash"} {
		if strings.Contains(call, forbidden) {
			t.Fatalf("cleanup argv used a shell (%q): %q", forbidden, call)
		}
	}
}

func TestCleanupSandboxRootRefusesUnexpectedResidue(t *testing.T) {
	cases := []struct {
		name    string
		box     string
		boxProc string
		extra   string
		wantErr error
	}{
		{"box-member", "box-1", "7\n", "", containment.ErrNotEmpty},
		{"grandchild", "box-1", "", "box-1/grand-1", containment.ErrNotEmpty},
		{"unexpected-child", "other", "", "", containment.ErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, fakeFS, _, _ := newRunBlockAgent(t)
			plan := buildTestPlan(t, "judger", 1)
			imageID := "sha256:" + strings.Repeat("d", 64)
			fakeFS.mkdir("/sys/fs/cgroup/sandbox-run-worker-01")
			fakeFS.put("/sys/fs/cgroup/sandbox-run-worker-01/cgroup.procs", "")
			fakeFS.mkdir("/sys/fs/cgroup/sandbox-run-worker-01/" + tc.box)
			fakeFS.put("/sys/fs/cgroup/sandbox-run-worker-01/"+tc.box+"/cgroup.procs", tc.boxProc)
			if tc.extra != "" {
				fakeFS.mkdir("/sys/fs/cgroup/sandbox-run-worker-01/" + tc.extra)
			}
			runner := &fakeDockerRunner{}
			a.dockerRunner = runner

			err := a.cleanupSandboxRoot(context.Background(), cleanupTestManager(a), plan, plan.Blocks[0], imageID, "/sandbox-run-worker-01")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if len(runner.calls) != 0 {
				t.Fatalf("docker ran for residual evidence: %v", runner.calls)
			}
			if !fakeFS.dirs["/sys/fs/cgroup/sandbox-run-worker-01"] {
				t.Fatal("residual root was mutated")
			}
		})
	}
}

func TestCleanupSandboxRootRefusesNonEmptyRoot(t *testing.T) {
	a, fakeFS, _, _ := newRunBlockAgent(t)
	plan := buildTestPlan(t, "judger", 1)
	imageID := "sha256:" + strings.Repeat("d", 64)
	fakeFS.mkdir("/sys/fs/cgroup/sandbox-run-worker-01")
	fakeFS.put("/sys/fs/cgroup/sandbox-run-worker-01/cgroup.procs", "4242\n")
	runner := &fakeDockerRunner{}
	a.dockerRunner = runner

	err := a.cleanupSandboxRoot(context.Background(), cleanupTestManager(a), plan, plan.Blocks[0], imageID, "/sandbox-run-worker-01")
	if !errors.Is(err, containment.ErrNotEmpty) {
		t.Fatalf("err = %v, want ErrNotEmpty", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("docker ran for a non-empty root: %v", runner.calls)
	}
	if !fakeFS.dirs["/sys/fs/cgroup/sandbox-run-worker-01"] {
		t.Fatal("non-empty root was mutated")
	}
}

func TestCleanupSandboxRootSkipsAbsentRoot(t *testing.T) {
	a, _, _, _ := newRunBlockAgent(t)
	plan := buildTestPlan(t, "judger", 1)
	imageID := "sha256:" + strings.Repeat("d", 64)
	runner := &fakeDockerRunner{}
	a.dockerRunner = runner

	if err := a.cleanupSandboxRoot(context.Background(), cleanupTestManager(a), plan, plan.Blocks[0], imageID, "/sandbox-absent"); err != nil {
		t.Fatalf("absent root = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("docker ran for an absent root: %v", runner.calls)
	}
}

func TestCleanupSandboxRootRejectsUnsealedImage(t *testing.T) {
	a, fakeFS, _, _ := newRunBlockAgent(t)
	plan := buildTestPlan(t, "judger", 1)
	fakeFS.mkdir("/sys/fs/cgroup/sandbox-run-worker-01")
	fakeFS.put("/sys/fs/cgroup/sandbox-run-worker-01/cgroup.procs", "")
	runner := &fakeDockerRunner{}
	a.dockerRunner = runner

	err := a.cleanupSandboxRoot(context.Background(), cleanupTestManager(a), plan, plan.Blocks[0], "judger-bench:latest", "/sandbox-run-worker-01")
	if err == nil || !strings.Contains(err.Error(), "sealed immutable image id") {
		t.Fatalf("err = %v, want unsealed-image rejection", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("docker ran with an unsealed image: %v", runner.calls)
	}
}

func TestBlockCoordinatorInjectsCleanupOnlyForOCI(t *testing.T) {
	a, _, _, _ := newRunBlockAgent(t)
	plan := buildTestPlan(t, "judger", 1)
	block := plan.Blocks[0]
	enc := protocol.NewEventEncoder(io.Discard)
	host := a.newBlockCoordinator(cleanupTestManager(a), plan, block, "", enc, false)
	if host.CleanupSandboxRoot != nil {
		t.Fatal("host mode injected an OCI cleanup callback")
	}
	imageID := "sha256:" + strings.Repeat("d", 64)
	oci := a.newBlockCoordinator(cleanupTestManager(a), plan, block, imageID, enc, true)
	if oci.CleanupSandboxRoot == nil {
		t.Fatal("OCI mode did not inject a cleanup callback")
	}
}

func TestDockerCleanupContainerNameIsRunScopedAndValid(t *testing.T) {
	plan := runplan.Plan{RunID: testRunID}
	block := runplan.Block{ID: "block-01"}
	name := dockerCleanupContainerName(plan, block, "/sandbox-run-worker-01")
	if !dockerNamePattern.MatchString(name) || !strings.HasPrefix(name, "iris-bench-") {
		t.Fatalf("invalid helper name %q", name)
	}
	if !strings.Contains(name, "cleanup-sandbox-run-worker-01") || !strings.Contains(name, testRunID) {
		t.Fatalf("helper name %q is not run- and root-scoped", name)
	}
	if name == dockerContainerName(plan, block, "worker-01") {
		t.Fatalf("helper name collides with a worker name: %q", name)
	}
}

func TestDockerCleanupContainerNamesIncludesOnlyAcceptedRoots(t *testing.T) {
	plan := runplan.Plan{RunID: testRunID}
	block := runplan.Block{ID: "block-01"}
	specs := []experiment.WorkerSpec{
		{ID: "worker-01", AcceptedUncontainedCgroup: "/sandbox-a"},
		{ID: "worker-02"},
		{ID: "worker-03", AcceptedUncontainedCgroup: "/sandbox-a"},
	}
	names := dockerCleanupContainerNames(plan, block, specs)
	if len(names) != 1 || names[0] != dockerCleanupContainerName(plan, block, "/sandbox-a") {
		t.Fatalf("helper names = %v", names)
	}
}

func TestRunBlockRejectsRootCgroupParent(t *testing.T) {
	a, _, _, _ := newRunBlockAgent(t)
	a.cgroupParent = "/sys/fs/cgroup"
	sha := stageReadyRun(t, a, "judger", 1)
	_, terminal, err := execAgent(t, a, request(protocol.ActionRunBlock, sha, "block-01"))
	if !errors.Is(err, errTerminalFailed) {
		t.Fatalf("err = %v, want errTerminalFailed", err)
	}
	if terminal.Status != protocol.StatusUnsupported {
		t.Fatalf("terminal = %+v, want unsupported", terminal)
	}
}

func TestDeriveWorkerTimeoutIndependentFromBlock(t *testing.T) {
	if per := deriveWorkerTimeout(3600*time.Second, 5); per <= 0 || per >= 3600*time.Second {
		t.Fatalf("per-worker timeout = %v, want bounded below the block ceiling", per)
	}
	if per := deriveWorkerTimeout(10*time.Second, 1); per >= 10*time.Second || per <= 0 {
		t.Fatalf("single-repetition timeout = %v, want below the block ceiling", per)
	}
	if per := deriveWorkerTimeout(0, 0); per <= 0 {
		t.Fatalf("zero-input timeout = %v, want positive floor", per)
	}
}
