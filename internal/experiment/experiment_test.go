package experiment

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skkuding/iris-load-test-toolset/internal/containment"
)

// ---- fakes ----

type fakeHandle struct {
	name      string
	path      string
	cfg       containment.Config
	mu        sync.Mutex
	members   []int
	removed   bool
	verifyErr error
}

func (h *fakeHandle) Name() string               { return h.name }
func (h *fakeHandle) FSPath() string             { return h.path }
func (h *fakeHandle) RelPath() string            { return "/iris-bench/" + h.name }
func (h *fakeHandle) Config() containment.Config { return h.cfg }
func (h *fakeHandle) Verify() error              { return h.verifyErr }
func (h *fakeHandle) Attach(pid int) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.members = append(h.members, pid)
	return nil
}
func (h *fakeHandle) Members() ([]int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]int(nil), h.members...), nil
}
func (h *fakeHandle) Remove() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.members) > 0 {
		return errors.New("directory not empty")
	}
	h.removed = true
	return nil
}
func (h *fakeHandle) Exists() (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.removed, nil
}

type fakeProc struct {
	mu       sync.Mutex
	cgroups  map[int]string
	children map[int][]int
}

func newFakeProc() *fakeProc {
	return &fakeProc{cgroups: map[int]string{}, children: map[int][]int{}}
}
func (p *fakeProc) setCgroup(pid int, cg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cgroups[pid] = cg
}
func (p *fakeProc) setChildren(pid int, kids []int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.children[pid] = kids
}
func (p *fakeProc) Cgroup(pid int) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cg, ok := p.cgroups[pid]
	if !ok {
		return "", fmt.Errorf("pid %d gone", pid)
	}
	return cg, nil
}
func (p *fakeProc) Descendants(pid int) ([]int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.cgroups[pid]; !ok {
		return nil, fmt.Errorf("pid %d gone", pid)
	}
	seen := map[int]bool{}
	var walk func(int)
	walk = func(cur int) {
		if seen[cur] {
			return
		}
		seen[cur] = true
		for _, c := range p.children[cur] {
			walk(c)
		}
	}
	walk(pid)
	out := make([]int, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	return out, nil
}

type fakeContainment struct {
	mu            sync.Mutex
	delegationErr error
	createErr     map[string]error
	verifyErr     map[string]error
	created       []*fakeHandle
	proc          *fakeProc
	inspections   map[string]containment.Inspection
	inspectErr    error
	// childRoots models child cgroup directories under an inspected root.
	childRoots  map[string][]string
	removeErr   error
	removedRoot []string
}

func newFakeContainment() *fakeContainment {
	return &fakeContainment{createErr: map[string]error{}, verifyErr: map[string]error{}, proc: newFakeProc(), inspections: map[string]containment.Inspection{}, childRoots: map[string][]string{}}
}
func (c *fakeContainment) CheckDelegation(string) error { return c.delegationErr }
func (c *fakeContainment) Create(cfg containment.Config) (containment.Handle, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.createErr[cfg.Name]; err != nil {
		return nil, err
	}
	h := &fakeHandle{name: cfg.Name, path: filepath.Join(cfg.Parent, cfg.Name), cfg: cfg, verifyErr: c.verifyErr[cfg.Name]}
	c.created = append(c.created, h)
	return h, nil
}
func (c *fakeContainment) CgroupOf(pid int) (string, error) { return c.proc.Cgroup(pid) }
func (c *fakeContainment) Descendants(pid int) ([]int, error) {
	return c.proc.Descendants(pid)
}
func (c *fakeContainment) InspectCgroup(path string) (containment.Inspection, error) {
	if c.inspectErr != nil {
		return containment.Inspection{}, c.inspectErr
	}
	if got, ok := c.inspections[path]; ok {
		return got, nil
	}
	return containment.Inspection{Path: path}, nil
}
func (c *fakeContainment) RemoveEmptySandboxRoot(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.removeErr != nil {
		return c.removeErr
	}
	got, ok := c.inspections[path]
	if !ok || !got.Exists {
		return nil
	}
	if len(got.Members) > 0 {
		return fmt.Errorf("containment: sandbox cgroup %s still has member pids %v", path, got.Members)
	}
	// Model the manager's validated child-first, root-last removal order for
	// the empty box-<id> child cgroups live alpha.4 leaves behind.
	boxes := append([]string(nil), c.childRoots[path]...)
	sort.Strings(boxes)
	delete(c.inspections, path)
	for _, box := range boxes {
		c.removedRoot = append(c.removedRoot, filepath.Join(path, box))
	}
	c.removedRoot = append(c.removedRoot, path)
	return nil
}

type fakeWorker struct {
	pid         int
	done        chan struct{}
	release     chan struct{}
	finish      chan struct{}
	err         error
	releaseOnce sync.Once
	finishOnce  sync.Once
}

func (w *fakeWorker) PID() int { return w.pid }
func (w *fakeWorker) Wait() error {
	<-w.done
	return w.err
}
func (w *fakeWorker) releaseNow() { w.releaseOnce.Do(func() { close(w.release) }) }
func (w *fakeWorker) finishNow()  { w.finishOnce.Do(func() { close(w.finish) }) }
func (w *fakeWorker) Stop() error {
	w.releaseNow()
	w.finishNow()
	return nil
}

type fakeLauncher struct {
	mu         sync.Mutex
	count      int
	workers    []*fakeWorker
	startErr   error
	readyErr   error
	releaseErr error
	runErr     error
	hold       bool
	onRun      func(spec WorkerSpec, cgroupPath string)
	proc       *fakeProc
	closed     bool
}

func (l *fakeLauncher) Start(_ context.Context, spec WorkerSpec, h containment.Handle) (Worker, error) {
	if l.startErr != nil {
		return nil, l.startErr
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	pid := 1000 + len(l.workers)
	w := &fakeWorker{pid: pid, done: make(chan struct{}), release: make(chan struct{}), finish: make(chan struct{})}
	l.workers = append(l.workers, w)
	if l.proc != nil {
		l.proc.setCgroup(pid, h.RelPath())
	}
	go func() {
		<-w.release
		if l.onRun != nil {
			l.onRun(spec, h.FSPath())
		}
		if l.hold {
			<-w.finish
		}
		w.err = l.runErr
		close(w.done)
	}()
	return w, nil
}
func (l *fakeLauncher) WaitReady(context.Context) error { return l.readyErr }
func (l *fakeLauncher) Release() error {
	if l.releaseErr != nil {
		return l.releaseErr
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, w := range l.workers {
		w.releaseNow()
	}
	return nil
}
func (l *fakeLauncher) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	for _, w := range l.workers {
		_ = w.Stop()
	}
	return nil
}

type captureSink struct {
	mu     sync.Mutex
	checks []string
}

func (s *captureSink) Phase(phase, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks = append(s.checks, "phase:"+phase+":"+status)
	return nil
}
func (s *captureSink) Check(name, status, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks = append(s.checks, "check:"+name+":"+status)
	return nil
}
func (s *captureSink) has(sub string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.checks {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

// ---- helpers ----

// sampleBaseNs is an arbitrary positive wall-clock base. Every worker's
// synthetic samples share it so the all-workers-active window is well-formed.
const sampleBaseNs int64 = 1_700_000_000_000_000_000

func sampleLine(worker, cgroupPath string, i int) string {
	started := sampleBaseNs + int64(i)*1000
	ended := started + 500
	return fmt.Sprintf(`{"worker":%q,"cgroupPath":%q,"cgroupContained":true,"iteration":%d,"status":"success","outputMatches":true,"containmentMode":"isolated","comparable":true,"startedAtNs":%d,"endedAtNs":%d}`, worker, cgroupPath, i, started, ended)
}

// writeTimedSamples writes one successful sample per (start,end) pair so a test
// can shape the per-worker active intervals directly.
func writeTimedSamples(path, worker, cgroupPath string, spans [][2]int64) {
	var b strings.Builder
	for i, span := range spans {
		fmt.Fprintf(&b, `{"worker":%q,"cgroupPath":%q,"cgroupContained":true,"iteration":%d,"status":"success","outputMatches":true,"containmentMode":"isolated","comparable":true,"startedAtNs":%d,"endedAtNs":%d}`+"\n", worker, cgroupPath, i, span[0], span[1])
	}
	_ = os.WriteFile(path, []byte(b.String()), 0o644)
}

func writeSamples(path, worker, cgroupPath string, n int) {
	data := ""
	for i := 0; i < n; i++ {
		data += sampleLine(worker, cgroupPath, i) + "\n"
	}
	_ = os.WriteFile(path, []byte(data), 0o644)
}

func writeContained(spec WorkerSpec, cgroupPath string) {
	writeSamples(spec.OutputPath, spec.ID, cgroupPath, spec.ExpectedSamples)
}

func baseRequest(t *testing.T) (BlockRequest, *fakeContainment, *fakeLauncher, *captureSink) {
	t.Helper()
	dir := t.TempDir()
	fc := newFakeContainment()
	fl := &fakeLauncher{proc: fc.proc}
	sink := &captureSink{}
	req := BlockRequest{
		RunID:                "iris-20260925-abcdefgh",
		BlockID:              "isolated-01",
		Parent:               "/sys/fs/cgroup/iris-bench",
		RunDir:               dir,
		SamplesName:          "samples/judger.ndjson",
		ReceiptName:          "receipts/isolated-01.json",
		WorkloadBinarySHA256: strings.Repeat("a", 64),
		MonitorInterval:      time.Millisecond,
		Workers: []WorkerSpec{
			{ID: "worker-01", CPUList: "0", Mems: "0", CPUMax: "100000 100000", Command: "/bin/true", ExpectedSamples: 2, OutputPath: filepath.Join(dir, "w1.ndjson")},
			{ID: "worker-02", CPUList: "1", Mems: "0", CPUMax: "100000 100000", Command: "/bin/true", ExpectedSamples: 2, OutputPath: filepath.Join(dir, "w2.ndjson")},
		},
	}
	return req, fc, fl, sink
}

func coordinator(fc Containment, fl *fakeLauncher, sink *captureSink) *Coordinator {
	return &Coordinator{
		Containment:     fc,
		LauncherFactory: func(int) Launcher { return fl },
		Events:          sink,
	}
}

func TestRunBlockHappyPath(t *testing.T) {
	req, fc, fl, sink := baseRequest(t)
	fl.onRun = func(spec WorkerSpec, cg string) { writeContained(spec, cg) }

	res, err := coordinator(fc, fl, sink).Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Receipt.Status != StatusPassed || res.Receipt.SampleCount != 4 {
		t.Fatalf("receipt = %+v", res.Receipt)
	}
	if res.Receipt.WorkloadBinarySHA256 != req.WorkloadBinarySHA256 {
		t.Fatalf("receipt workload digest = %q", res.Receipt.WorkloadBinarySHA256)
	}
	if _, err := os.Stat(filepath.Join(req.RunDir, "samples/judger.ndjson")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(req.RunDir, "receipts/isolated-01.json")); err != nil {
		t.Fatal(err)
	}
	if len(fc.created) != 2 {
		t.Fatalf("created %d subtrees", len(fc.created))
	}
	for _, h := range fc.created {
		if exists, _ := h.Exists(); exists {
			t.Fatalf("subtree %s remained", h.RelPath())
		}
	}
	if !sink.has("readiness-barrier:passed") {
		t.Fatalf("checks = %v", sink.checks)
	}
}

func TestRunBlockRecordsValidSteadyWindow(t *testing.T) {
	req, fc, fl, sink := baseRequest(t)
	fl.onRun = func(spec WorkerSpec, cg string) { writeContained(spec, cg) }

	res, err := coordinator(fc, fl, sink).Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	// Two samples per worker: starts base+0/base+1000, ends base+500/base+1500.
	wantStart := sampleBaseNs
	wantEnd := sampleBaseNs + 1500
	if res.Receipt.SteadyWindowStartNs != wantStart || res.Receipt.SteadyWindowEndNs != wantEnd {
		t.Fatalf("window = [%d,%d], want [%d,%d]", res.Receipt.SteadyWindowStartNs, res.Receipt.SteadyWindowEndNs, wantStart, wantEnd)
	}
	if got, want := res.Receipt.SteadyWindowSeconds, 1500.0/1e9; math.Abs(got-want) > 1e-12 {
		t.Fatalf("steadyWindowSeconds = %v, want %v", got, want)
	}
	if !sink.has("steady-window:passed") {
		t.Fatalf("checks = %v", sink.checks)
	}
}

func TestRunBlockRejectsUnusableSteadyWindow(t *testing.T) {
	cases := []struct {
		name  string
		spans [][][2]int64 // per worker
	}{
		{
			name:  "missing timestamps",
			spans: [][][2]int64{{{0, 0}}, {{0, 0}}},
		},
		{
			name: "disjoint intervals leave an empty window",
			spans: [][][2]int64{
				{{sampleBaseNs, sampleBaseNs + 1000}},
				{{sampleBaseNs + 5000, sampleBaseNs + 6000}},
			},
		},
		{
			name: "negligible window over a long span",
			spans: [][][2]int64{
				{{sampleBaseNs, sampleBaseNs + 10}, {sampleBaseNs + 10_000, sampleBaseNs + 10_010}},
				{{sampleBaseNs + 5000, sampleBaseNs + 5001}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, fc, fl, sink := baseRequest(t)
			for i := range req.Workers {
				req.Workers[i].ExpectedSamples = len(tc.spans[i])
			}
			fl.onRun = func(spec WorkerSpec, cg string) {
				index := 0
				for i := range req.Workers {
					if req.Workers[i].ID == spec.ID {
						index = i
					}
				}
				writeTimedSamples(spec.OutputPath, spec.ID, cg, tc.spans[index])
			}

			_, err := coordinator(fc, fl, sink).Run(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), req.BlockID) || !strings.Contains(err.Error(), "measurement window") {
				t.Fatalf("err = %v, want steady-window rejection naming block %s", err, req.BlockID)
			}
			if !sink.has("sample-count:failed") {
				t.Fatalf("checks = %v", sink.checks)
			}
		})
	}
}

func TestRunBlockDetectsEscape(t *testing.T) {
	req, fc, fl, sink := baseRequest(t)
	fl.hold = true
	fl.onRun = func(spec WorkerSpec, cg string) {
		writeContained(spec, cg)
		if spec.ID == "worker-01" {
			// worker 1000 gains a child that escaped to a root-level cgroup
			fc.proc.setCgroup(2000, "/sandbox-xyz")
			fc.proc.setChildren(1000, []int{2000})
		}
	}
	var killedMu sync.Mutex
	killed := map[int]bool{}
	coord := coordinator(fc, fl, sink)
	coord.KillFunc = func(pid int) error {
		killedMu.Lock()
		defer killedMu.Unlock()
		killed[pid] = true
		return nil
	}
	_, err := coord.Run(context.Background(), req)
	if !errors.Is(err, containment.ErrEscape) {
		t.Fatalf("err = %v, want ErrEscape", err)
	}
	if !sink.has("cgroup-escape:failed") {
		t.Fatalf("checks = %v", sink.checks)
	}
	killedMu.Lock()
	gotKill := killed[2000]
	killedMu.Unlock()
	if !gotKill {
		t.Fatal("escaped process was not killed")
	}
	for _, h := range fc.created {
		if exists, _ := h.Exists(); exists {
			t.Fatalf("subtree %s remained after escape", h.RelPath())
		}
	}
}

func TestRunBlockSampleCountMismatch(t *testing.T) {
	req, fc, fl, sink := baseRequest(t)
	fl.onRun = func(spec WorkerSpec, cg string) { writeSamples(spec.OutputPath, spec.ID, cg, spec.ExpectedSamples-1) }
	_, err := coordinator(fc, fl, sink).Run(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "expected") {
		t.Fatalf("err = %v", err)
	}
	if !sink.has("sample-count:failed") {
		t.Fatalf("checks = %v", sink.checks)
	}
	for _, h := range fc.created {
		if exists, _ := h.Exists(); exists {
			t.Fatalf("subtree %s remained", h.RelPath())
		}
	}
}

func TestRunBlockRejectsUnsupportedDelegation(t *testing.T) {
	req, fc, fl, sink := baseRequest(t)
	fc.delegationErr = containment.ErrNotDelegated
	_, err := coordinator(fc, fl, sink).Run(context.Background(), req)
	if !errors.Is(err, containment.ErrNotDelegated) {
		t.Fatalf("err = %v", err)
	}
	if !sink.has("cgroup-delegation:unsupported") {
		t.Fatalf("checks = %v", sink.checks)
	}
	if len(fc.created) != 0 {
		t.Fatal("no subtree should be created when delegation fails")
	}
}

func TestRunBlockCreateFailureCleansUp(t *testing.T) {
	req, fc, fl, sink := baseRequest(t)
	name, err := SubtreeName(req.RunID, req.BlockID, "worker-02")
	if err != nil {
		t.Fatal(err)
	}
	fc.createErr[name] = errors.New("no space")
	_, err = coordinator(fc, fl, sink).Run(context.Background(), req)
	if err == nil {
		t.Fatal("expected create failure")
	}
	for _, h := range fc.created {
		if exists, _ := h.Exists(); exists {
			t.Fatalf("subtree %s remained", h.RelPath())
		}
	}
}

func TestRunBlockStartFailureCleansUp(t *testing.T) {
	req, fc, fl, sink := baseRequest(t)
	fl.startErr = errors.New("exec failed")
	_, err := coordinator(fc, fl, sink).Run(context.Background(), req)
	if err == nil {
		t.Fatal("expected start failure")
	}
	for _, h := range fc.created {
		if exists, _ := h.Exists(); exists {
			t.Fatalf("subtree %s remained", h.RelPath())
		}
	}
}

func TestRunBlockReadinessFailureCleansUp(t *testing.T) {
	req, fc, fl, sink := baseRequest(t)
	fl.readyErr = errors.New("worker never ready")
	_, err := coordinator(fc, fl, sink).Run(context.Background(), req)
	if err == nil {
		t.Fatal("expected readiness failure")
	}
	if !sink.has("readiness-barrier:failed") {
		t.Fatalf("checks = %v", sink.checks)
	}
	for _, h := range fc.created {
		if exists, _ := h.Exists(); exists {
			t.Fatalf("subtree %s remained", h.RelPath())
		}
	}
}

func TestRunBlockTimeoutStopsWorkers(t *testing.T) {
	req, fc, fl, sink := baseRequest(t)
	req.Timeout = 10 * time.Millisecond
	fl.hold = true
	fl.onRun = func(spec WorkerSpec, cg string) {}
	_, err := coordinator(fc, fl, sink).Run(context.Background(), req)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	for _, w := range fl.workers {
		select {
		case <-w.done:
		case <-time.After(time.Second):
			t.Fatal("worker was not stopped after timeout")
		}
	}
}

func TestRunBlockResidualProcessFails(t *testing.T) {
	req, fc, fl, sink := baseRequest(t)
	fl.onRun = func(spec WorkerSpec, cg string) { writeContained(spec, cg) }
	c := &lingeringContainment{fakeContainment: fc}
	_, err := coordinator(c, fl, sink).Run(context.Background(), req)
	if !errors.Is(err, containment.ErrEscape) {
		t.Fatalf("err = %v, want ErrEscape", err)
	}
	if !sink.has("cgroup-residual-") {
		t.Fatalf("checks = %v", sink.checks)
	}
}

func TestRunBlockMissingOutputFails(t *testing.T) {
	req, fc, fl, sink := baseRequest(t)
	fl.onRun = func(spec WorkerSpec, cg string) { /* writes nothing */ }
	_, err := coordinator(fc, fl, sink).Run(context.Background(), req)
	if err == nil {
		t.Fatal("expected missing sample failure")
	}
}

// writeRawSample writes a single caller-supplied NDJSON line as the worker's
// only sample so sample validation can be exercised directly.
func writeRawSample(spec WorkerSpec, line string) {
	_ = os.WriteFile(spec.OutputPath, []byte(line+"\n"), 0o644)
}

func TestRunBlockRejectsUncontainedSample(t *testing.T) {
	req, fc, fl, sink := baseRequest(t)
	fl.onRun = func(spec WorkerSpec, cg string) {
		if spec.ID == "worker-01" {
			writeRawSample(spec, `{"worker":"worker-01","cgroupPath":"`+cg+`","cgroupContained":false,"status":"success","outputMatches":true,"containmentMode":"isolated","comparable":true}`)
			return
		}
		writeContained(spec, cg)
	}
	_, err := coordinator(fc, fl, sink).Run(context.Background(), req)
	if !errors.Is(err, containment.ErrEscape) {
		t.Fatalf("err = %v, want ErrEscape", err)
	}
	if !sink.has("sample-count:failed") {
		t.Fatalf("checks = %v", sink.checks)
	}
}

func TestRunBlockRejectsWorkerIdentityMismatch(t *testing.T) {
	req, fc, fl, sink := baseRequest(t)
	fl.onRun = func(spec WorkerSpec, cg string) {
		if spec.ID == "worker-01" {
			writeRawSample(spec, `{"worker":"worker-99","cgroupPath":"`+cg+`","cgroupContained":true,"status":"success","outputMatches":true,"containmentMode":"isolated","comparable":true}`)
			return
		}
		writeContained(spec, cg)
	}
	_, err := coordinator(fc, fl, sink).Run(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("err = %v, want worker identity mismatch", err)
	}
}

func TestRunBlockRejectsNonSuccessSample(t *testing.T) {
	req, fc, fl, sink := baseRequest(t)
	fl.onRun = func(spec WorkerSpec, cg string) {
		if spec.ID == "worker-01" {
			writeRawSample(spec, `{"worker":"worker-01","cgroupPath":"`+cg+`","cgroupContained":true,"status":"runtime_error","outputMatches":true,"containmentMode":"isolated","comparable":true}`)
			return
		}
		writeContained(spec, cg)
	}
	_, err := coordinator(fc, fl, sink).Run(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "not successful") {
		t.Fatalf("err = %v, want sample result rejection", err)
	}
}

func TestProductionCompatSampleAcceptsRootSandboxAndDescendants(t *testing.T) {
	req, _, _, _ := baseRequest(t)
	state := workerState{spec: req.Workers[0]}
	state.spec.AcceptedUncontainedCgroup = "/sandbox-run-worker-01"
	newProbe := func(path string) sampleProbe {
		return sampleProbe{
			Worker: "worker-01", Status: "success", OutputMatches: true,
			ContainmentMode: "production-compat", CgroupPath: path,
		}
	}
	for _, path := range []string{
		"/sandbox-run-worker-01",
		"/sandbox-run-worker-01/child",
		"/sys/fs/cgroup/sandbox-run-worker-01",
		"/sys/fs/cgroup/sandbox-run-worker-01/box-123",
	} {
		if err := validateSample(req, state, newProbe(path)); err != nil {
			t.Fatalf("production compatibility cgroupPath %q rejected: %v", path, err)
		}
	}
	for _, path := range []string{
		"/sandbox-run-worker-01-evil/child",
		"/sys/fs/cgroup/sandbox-run-worker-01-evil/child",
		"/sandbox-run-worker-02",
		"sandbox-run-worker-01",
	} {
		if err := validateSample(req, state, newProbe(path)); err == nil {
			t.Fatalf("production compatibility accepted invalid cgroupPath %q", path)
		}
	}
}

func TestProductionCompatCleanupRemovesEmptyRootAndVerifiesAbsence(t *testing.T) {
	fc := newFakeContainment()
	sink := &captureSink{}
	root := "/sandbox-run-worker-01"
	fc.inspections[root] = containment.Inspection{Path: root, Exists: true}
	specs := []WorkerSpec{{ID: "worker-01", AcceptedUncontainedCgroup: root}}
	c := &Coordinator{Containment: fc, Events: sink}
	if err := c.verifyProductionCgroups(context.Background(), specs); err != nil {
		t.Fatalf("verifyProductionCgroups = %v", err)
	}
	if len(fc.removedRoot) != 1 || fc.removedRoot[0] != root {
		t.Fatalf("removed roots = %v, want [%s]", fc.removedRoot, root)
	}
	if got := fc.inspections[root]; got.Exists {
		t.Fatalf("root still present after cleanup: %+v", got)
	}
	if !sink.has("production-cgroup-removed-worker-01:passed") {
		t.Fatalf("checks = %v", sink.checks)
	}
}

func TestProductionCompatCleanupRemovesBoxesInOrderAndVerifiesAbsence(t *testing.T) {
	fc := newFakeContainment()
	sink := &captureSink{}
	root := "/sandbox-run-worker-01"
	fc.inspections[root] = containment.Inspection{Path: root, Exists: true}
	fc.childRoots[root] = []string{"box-2", "box-1", "box-3"}
	specs := []WorkerSpec{{ID: "worker-01", AcceptedUncontainedCgroup: root}}
	c := &Coordinator{Containment: fc, Events: sink}
	if err := c.verifyProductionCgroups(context.Background(), specs); err != nil {
		t.Fatalf("verifyProductionCgroups = %v", err)
	}
	want := []string{root + "/box-1", root + "/box-2", root + "/box-3", root}
	if fmt.Sprint(fc.removedRoot) != fmt.Sprint(want) {
		t.Fatalf("removed roots = %v, want %v", fc.removedRoot, want)
	}
	if fc.inspections[root].Exists {
		t.Fatalf("root still present after cleanup: %+v", fc.inspections[root])
	}
	if !sink.has("production-cgroup-removed-worker-01:passed") {
		t.Fatalf("checks = %v", sink.checks)
	}
}

func TestProductionCompatCleanupRefusesRootWithMembers(t *testing.T) {
	fc := newFakeContainment()
	sink := &captureSink{}
	root := "/sandbox-run-worker-01"
	fc.inspections[root] = containment.Inspection{Path: root, Exists: true, Members: []int{4242}}
	fc.childRoots[root] = []string{"box-123"}
	specs := []WorkerSpec{{ID: "worker-01", AcceptedUncontainedCgroup: root}}
	c := &Coordinator{Containment: fc, Events: sink}
	if err := c.verifyProductionCgroups(context.Background(), specs); err == nil {
		t.Fatal("expected residual evidence to fail cleanup")
	}
	if !sink.has("production-cgroup-removed-worker-01:failed") {
		t.Fatalf("checks = %v", sink.checks)
	}
	if !fc.inspections[root].Exists {
		t.Fatal("cleanup mutated a non-empty root")
	}
	if len(fc.removedRoot) != 0 {
		t.Fatalf("cleanup removed a non-empty root: %v", fc.removedRoot)
	}
}

func TestProductionCompatCleanupPrefersCallbackWhenSet(t *testing.T) {
	fc := newFakeContainment()
	sink := &captureSink{}
	root := "/sandbox-run-worker-01"
	fc.inspections[root] = containment.Inspection{Path: root, Exists: true}
	type ctxKey string
	const key ctxKey = "cleanup"
	var gotRoot string
	var gotCtx context.Context
	c := &Coordinator{
		Containment: fc,
		Events:      sink,
		CleanupSandboxRoot: func(ctx context.Context, path string) error {
			gotRoot, gotCtx = path, ctx
			delete(fc.inspections, path)
			return nil
		},
	}
	ctx := context.WithValue(context.Background(), key, "value")
	specs := []WorkerSpec{{ID: "worker-01", AcceptedUncontainedCgroup: root}}
	if err := c.verifyProductionCgroups(ctx, specs); err != nil {
		t.Fatalf("verifyProductionCgroups = %v", err)
	}
	if gotRoot != root {
		t.Fatalf("callback root = %q, want %q", gotRoot, root)
	}
	if gotCtx == nil || gotCtx.Value(key) != "value" {
		t.Fatal("callback did not receive the supplied context")
	}
	if len(fc.removedRoot) != 0 {
		t.Fatalf("host removal ran despite callback: %v", fc.removedRoot)
	}
	if !sink.has("production-cgroup-removed-worker-01:passed") {
		t.Fatalf("checks = %v", sink.checks)
	}
}

func TestProductionCompatCleanupCallbackErrorPreservesRoot(t *testing.T) {
	fc := newFakeContainment()
	sink := &captureSink{}
	root := "/sandbox-run-worker-01"
	fc.inspections[root] = containment.Inspection{Path: root, Exists: true}
	wantErr := fmt.Errorf("%w: sandbox root %s still has member pids [7]", containment.ErrNotEmpty, root)
	c := &Coordinator{
		Containment:        fc,
		Events:             sink,
		CleanupSandboxRoot: func(context.Context, string) error { return wantErr },
	}
	specs := []WorkerSpec{{ID: "worker-01", AcceptedUncontainedCgroup: root}}
	err := c.verifyProductionCgroups(context.Background(), specs)
	if !errors.Is(err, containment.ErrNotEmpty) {
		t.Fatalf("err = %v, want ErrNotEmpty", err)
	}
	if !fc.inspections[root].Exists {
		t.Fatal("callback error mutated the root")
	}
	if len(fc.removedRoot) != 0 {
		t.Fatalf("host removal ran despite callback: %v", fc.removedRoot)
	}
	if !sink.has("production-cgroup-removed-worker-01:failed") {
		t.Fatalf("checks = %v", sink.checks)
	}
}

func TestProductionCompatCleanupCallbackAbsenceIsReverified(t *testing.T) {
	fc := newFakeContainment()
	sink := &captureSink{}
	root := "/sandbox-run-worker-01"
	fc.inspections[root] = containment.Inspection{Path: root, Exists: true}
	c := &Coordinator{
		Containment:        fc,
		Events:             sink,
		CleanupSandboxRoot: func(context.Context, string) error { return nil },
	}
	specs := []WorkerSpec{{ID: "worker-01", AcceptedUncontainedCgroup: root}}
	err := c.verifyProductionCgroups(context.Background(), specs)
	if err == nil || !strings.Contains(err.Error(), "remains") {
		t.Fatalf("err = %v, want remaining-root failure", err)
	}
	if !sink.has("production-cgroup-removed-worker-01:failed") {
		t.Fatalf("checks = %v", sink.checks)
	}
}

func TestProductionCompatMonitorAcceptsRootSandboxDescendant(t *testing.T) {
	fc := newFakeContainment()
	worker := &fakeWorker{pid: 1000}
	handle := &fakeHandle{name: "iris-20260925-abcdefgh-run-block-worker-01", path: "/sys/fs/cgroup/iris-bench/iris-20260925-abcdefgh-run-block-worker-01"}
	fc.proc.setCgroup(1000, handle.RelPath())
	fc.proc.setChildren(1000, []int{2000})
	fc.proc.setCgroup(2000, "/sandbox-run-worker-01/box-123")
	state := workerState{
		spec:   WorkerSpec{ID: "worker-01", AcceptedUncontainedCgroup: "/sandbox-run-worker-01"},
		handle: handle,
		worker: worker,
	}
	ctx, cancel := context.WithCancel(context.Background())
	escape := make(chan error, 1)
	go (&Coordinator{Containment: fc}).monitor(ctx, []workerState{state}, time.Millisecond, escape)
	time.Sleep(5 * time.Millisecond)
	cancel()
	select {
	case err := <-escape:
		t.Fatalf("monitor rejected production sandbox descendant: %v", err)
	case <-time.After(5 * time.Millisecond):
	}
}

func TestProductionCompatFailsAndPreservesReceiptWhenRootRemains(t *testing.T) {
	req, fc, fl, sink := baseRequest(t)
	for i := range req.Workers {
		root := "/sandbox-run-" + req.Workers[i].ID
		req.Workers[i].AcceptedUncontainedCgroup = root
	}
	root := req.Workers[0].AcceptedUncontainedCgroup
	fc.inspections[root] = containment.Inspection{Path: root, Exists: true, Members: []int{4242}}
	fl.onRun = func(spec WorkerSpec, _ string) {
		line := fmt.Sprintf(`{"worker":%q,"cgroupPath":%q,"cgroupContained":false,"status":"success","outputMatches":true,"containmentMode":"production-compat","comparable":false}`, spec.ID, spec.AcceptedUncontainedCgroup)
		writeRawSample(spec, line)
	}

	_, err := coordinator(fc, fl, sink).Run(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), root) || !strings.Contains(err.Error(), "4242") {
		t.Fatalf("err = %v, want remaining production root evidence", err)
	}
	if !sink.has("production-cgroup-removed-worker-01:failed") {
		t.Fatalf("checks = %v", sink.checks)
	}
	data, readErr := os.ReadFile(filepath.Join(req.RunDir, req.ReceiptName))
	if readErr != nil || !strings.Contains(string(data), root) || !strings.Contains(string(data), "4242") {
		t.Fatalf("failure receipt = %q, %v", data, readErr)
	}
	if !fc.inspections[root].Exists {
		t.Fatal("coordinator mutated the unexpected production root")
	}
}

func TestRunBlocksKeepSeparateSampleFiles(t *testing.T) {
	req, fc, fl, sink := baseRequest(t)
	req.Workers = req.Workers[:1]
	fl.onRun = func(spec WorkerSpec, cg string) { writeContained(spec, cg) }
	if _, err := coordinator(fc, fl, sink).Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(req.RunDir, req.SamplesName)
	firstData, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	req.BlockID = "isolated-02"
	req.SamplesName = "samples/isolated-02.ndjson"
	req.ReceiptName = "receipts/isolated-02.json"
	fc2 := newFakeContainment()
	fl2 := &fakeLauncher{proc: fc2.proc, onRun: func(spec WorkerSpec, cg string) { writeContained(spec, cg) }}
	if _, err := coordinator(fc2, fl2, sink).Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(first)
	if err != nil || string(after) != string(firstData) {
		t.Fatalf("first block samples were overwritten: %v", err)
	}
	if _, err := os.Stat(filepath.Join(req.RunDir, req.SamplesName)); err != nil {
		t.Fatal("second block samples missing")
	}
}

func TestRunBlockRejectsCgroupPathOutsideWorkerSubtree(t *testing.T) {
	req, fc, fl, sink := baseRequest(t)
	fl.onRun = func(spec WorkerSpec, cg string) {
		if spec.ID == "worker-01" {
			writeRawSample(spec, `{"worker":"worker-01","cgroupPath":"/sys/fs/cgroup/sandbox-escape/box","cgroupContained":true,"status":"success","outputMatches":true,"containmentMode":"isolated","comparable":true}`)
			return
		}
		writeContained(spec, cg)
	}
	_, err := coordinator(fc, fl, sink).Run(context.Background(), req)
	if !errors.Is(err, containment.ErrEscape) {
		t.Fatalf("err = %v, want ErrEscape", err)
	}
}

func TestValidateRequestRejectsBadInput(t *testing.T) {
	bad := []BlockRequest{
		{},
		{RunID: "r", BlockID: "b", RunDir: "/tmp/x", SamplesName: "s", ReceiptName: "r", Workers: []WorkerSpec{{ID: "w", CPUList: "0", ExpectedSamples: 1, OutputPath: "/tmp/o"}}},
	}
	for i, req := range bad {
		if err := validateRequest(req); err == nil {
			t.Fatalf("case %d: expected error", i)
		}
	}
}

func TestSubtreeNameIsFlatRunScopedAndBounded(t *testing.T) {
	got, err := SubtreeName("iris-20260925-abcdefgh", "isolated-01", "worker-02")
	if err != nil {
		t.Fatal(err)
	}
	if want := "iris-20260925-abcdefgh-isolated-01-worker-02"; got != want {
		t.Fatalf("SubtreeName = %q, want %q", got, want)
	}
	if strings.ContainsAny(got, `/\`) {
		t.Fatalf("subtree name %q contains a path separator", got)
	}
	if got == "." || got == ".." {
		t.Fatalf("subtree name %q is not a safe directory entry", got)
	}
	if _, err := SubtreeName("iris-20260925-abcdefgh", "isolated-01", "worker-02"); err != nil {
		t.Fatalf("SubtreeName is not deterministic: %v", err)
	}
	long, err := SubtreeName(strings.Repeat("r", 128), strings.Repeat("b", 128), "worker-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(long) > maxSubtreeComponentLen || strings.ContainsAny(long, `/\`) {
		t.Fatalf("long subtree name %q is not a bounded single component", long)
	}
	other, err := SubtreeName(strings.Repeat("r", 128), strings.Repeat("b", 128), "worker-02")
	if err != nil {
		t.Fatal(err)
	}
	if long == other {
		t.Fatalf("truncated subtree names collided: %q", long)
	}
}

func TestSubtreeNameRejectsUnsafeIDs(t *testing.T) {
	cases := []struct {
		name    string
		runID   string
		blockID string
		worker  string
	}{
		{"empty run", "", "isolated-01", "worker-01"},
		{"run separator", "../run", "isolated-01", "worker-01"},
		{"run uppercase", "Run-1", "isolated-01", "worker-01"},
		{"block separator", "iris-20260925-abcdefgh", "a/b", "worker-01"},
		{"worker traversal", "iris-20260925-abcdefgh", "isolated-01", ".."},
	}
	for _, tc := range cases {
		if got, err := SubtreeName(tc.runID, tc.blockID, tc.worker); err == nil {
			t.Fatalf("%s: SubtreeName(%q, %q, %q) = %q, want error", tc.name, tc.runID, tc.blockID, tc.worker, got)
		}
	}
}

// lingeringContainment is intentionally unused in the happy path but keeps the
// compile-time coverage of a handle whose membership survives cleanup.
type lingeringContainment struct{ *fakeContainment }

func (l *lingeringContainment) Create(cfg containment.Config) (containment.Handle, error) {
	h, err := l.fakeContainment.Create(cfg)
	if err != nil {
		return nil, err
	}
	_ = h.Attach(9999)
	return h, nil
}
