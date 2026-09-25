package experiment

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
}

func newFakeContainment() *fakeContainment {
	return &fakeContainment{createErr: map[string]error{}, verifyErr: map[string]error{}, proc: newFakeProc()}
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

func sampleLine(worker, cgroupPath string, i int) string {
	return fmt.Sprintf(`{"worker":%q,"cgroupPath":%q,"cgroupContained":true,"iteration":%d}`, worker, cgroupPath, i)
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
		RunID:           "iris-20260925-abcdefgh",
		BlockID:         "isolated-01",
		Parent:          "/sys/fs/cgroup/iris-bench",
		RunDir:          dir,
		SamplesName:     "samples/judger.ndjson",
		ReceiptName:     "receipts/isolated-01.json",
		MonitorInterval: time.Millisecond,
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
	fc.createErr[req.RunID+"/"+req.BlockID+"/worker-02"] = errors.New("no space")
	_, err := coordinator(fc, fl, sink).Run(context.Background(), req)
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
			writeRawSample(spec, `{"worker":"worker-01","cgroupPath":"`+cg+`","cgroupContained":false}`)
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
			writeRawSample(spec, `{"worker":"worker-99","cgroupPath":"`+cg+`","cgroupContained":true}`)
			return
		}
		writeContained(spec, cg)
	}
	_, err := coordinator(fc, fl, sink).Run(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("err = %v, want worker identity mismatch", err)
	}
}

func TestRunBlockRejectsCgroupPathOutsideWorkerSubtree(t *testing.T) {
	req, fc, fl, sink := baseRequest(t)
	fl.onRun = func(spec WorkerSpec, cg string) {
		if spec.ID == "worker-01" {
			writeRawSample(spec, `{"worker":"worker-01","cgroupPath":"/sys/fs/cgroup/sandbox-escape/box","cgroupContained":true}`)
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
