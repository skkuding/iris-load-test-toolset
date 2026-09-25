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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skkuding/iris-load-test-toolset/internal/containment"
	"github.com/skkuding/iris-load-test-toolset/internal/experiment"
	"github.com/skkuding/iris-load-test-toolset/internal/protocol"
	"github.com/skkuding/iris-load-test-toolset/internal/runplan"
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
	plan, err := runplan.Build(runplan.BuildInput{
		ToolVersion:  "test",
		RunID:        testRunID,
		Target:       runplan.Target{SSHAlias: "host"},
		Suite:        suite,
		Profile:      "isolated",
		Images:       map[string]runplan.Image{"iris": {Reference: runplan.DefaultIrisImage, Digest: "sha256:" + strings.Repeat("a", 64)}},
		JudgerDigest: "sha256:" + strings.Repeat("b", 64),
		Blocks: []runplan.Block{{
			ID: "block-01", Suite: suite, Profile: "isolated",
			Workers: workers, CPUList: "0-3", Repetitions: 2,
		}},
		Qualification: runplan.Qualification{MaxRunSeconds: 5},
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
	_, terminal, err := execAgent(t, a, request(protocol.ActionInspect, "", ""))
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
	return nil, fmt.Errorf("readdir %s: %w", name, fs.ErrNotExist)
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
			fmt.Fprintf(&b, "{\"runId\":%q,\"blockId\":\"block-01\",\"worker\":%q,\"cgroupPath\":%q,\"cgroupContained\":true,\"iteration\":%d}\n",
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
	a.benchBinary = "/bin/true"
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
	if _, err := os.Stat(filepath.Join(a.runDir(testRunID), "samples", "judger.ndjson")); err != nil {
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
	a.benchBinary = "/bin/true"
	_, err := a.buildWorkerSpecs(request(protocol.ActionRunBlock, strings.Repeat("a", 64), "block-01"), runplan.Block{ID: "block-01", Suite: "judger", Workers: 2, Repetitions: 1}, runplan.Plan{})
	if err == nil {
		t.Fatal("expected missing cpu list error")
	}
}

func TestWorkerArgumentsTemplate(t *testing.T) {
	a := newTestAgent(t)
	a.workerArgs = []string{"--worker", "{worker}", "--out", "{output}"}
	got, err := a.workerArguments("worker-01", runplan.Block{ID: "b", Repetitions: 3}, runplan.Plan{RunID: testRunID}, "0", "/tmp/o", "fix",
		"/sys/fs/cgroup/iris-bench/run/b/worker-01", "run-b-worker-01", strings.Repeat("b", 64), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--worker", "worker-01", "--out", "/tmp/o"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v want %v", got, want)
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
		ToolVersion:  "test",
		RunID:        testRunID,
		Target:       runplan.Target{SSHAlias: "host"},
		Suite:        "judger",
		Profile:      "isolated",
		Images:       map[string]runplan.Image{"iris": {Reference: runplan.DefaultIrisImage, Digest: "sha256:" + strings.Repeat("a", 64)}},
		JudgerDigest: "sha256:" + strings.Repeat("b", 64),
		Blocks:       []runplan.Block{{ID: "block-01", Suite: "judger", Profile: "isolated", Workers: 1, CPUList: "0", Repetitions: 1}},
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
	a.benchBinary = "/bin/true"
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
	expectedParent := filepath.Join(a.cgroupParent, testRunID, "block-01", "worker-01")
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
