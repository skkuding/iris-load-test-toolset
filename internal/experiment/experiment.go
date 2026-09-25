// Package experiment runs one direct-suite measurement block inside its
// containment boundary.
//
// The coordinator is deliberately free of host-specific details. It depends on
// the containment and launcher boundaries so its sequencing, stop conditions,
// sample accounting, and cleanup can be unit tested without cgroup delegation.
//
// A block never reports success unless every worker cgroup was created and
// verified before the workers started, every worker signalled ready behind the
// local barrier, every observed process stayed in its assigned subtree, the
// expected number of samples was produced, and no process or cgroup remained.
package experiment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/skkuding/iris-load-test-toolset/internal/artifact"
	"github.com/skkuding/iris-load-test-toolset/internal/containment"
)

// Event status strings shared with the agent protocol.
const (
	StatusStarted     = "started"
	StatusPassed      = "passed"
	StatusFailed      = "failed"
	StatusUnsupported = "unsupported"
)

// EventSink receives structured progress without depending on the wire format.
type EventSink interface {
	Phase(phase, status string) error
	Check(name, status, message string) error
}

// Containment is the cgroup boundary used by the coordinator. It is satisfied
// by *containment.Manager and by fakes in tests.
type Containment interface {
	CheckDelegation(parent string) error
	Create(cfg containment.Config) (containment.Handle, error)
	CgroupOf(pid int) (string, error)
	Descendants(pid int) ([]int, error)
}

// WorkerSpec describes one worker process and its containment assignment.
type WorkerSpec struct {
	ID              string
	CPUList         string
	Mems            string
	CPUMax          string
	Command         string
	Args            []string
	Fixture         string
	ExpectedSamples int
	// OutputPath is where the worker writes its NDJSON samples.
	OutputPath string
}

// Worker is one started worker process.
type Worker interface {
	PID() int
	Wait() error
	Stop() error
}

// Launcher starts workers behind a local readiness barrier. Start begins a
// worker but must not let it execute measured work until Release is called.
// WaitReady blocks until every started worker signalled readiness.
type Launcher interface {
	Start(ctx context.Context, spec WorkerSpec, h containment.Handle) (Worker, error)
	WaitReady(ctx context.Context) error
	Release() error
	Close() error
}

// BlockRequest is one run-block operation.
type BlockRequest struct {
	RunID   string
	BlockID string
	// Parent is the absolute path of the explicitly delegated cgroup parent.
	Parent  string
	Workers []WorkerSpec
	Timeout time.Duration
	// MonitorInterval bounds how long an escaped process can go unnoticed.
	MonitorInterval time.Duration
	// RunDir is the evidence directory the samples and receipt are written to.
	RunDir string
	// SamplesName and ReceiptName are paths relative to RunDir.
	SamplesName string
	ReceiptName string
}

// WorkerReceipt records per-worker containment and sample evidence.
type WorkerReceipt struct {
	ID              string `json:"id"`
	CPUList         string `json:"cpuList"`
	Mems            string `json:"mems"`
	CPUMax          string `json:"cpuMax"`
	PID             int    `json:"pid"`
	Cgroup          string `json:"cgroup"`
	Samples         int    `json:"samples"`
	ExpectedSamples int    `json:"expectedSamples"`
	ExitError       string `json:"exitError,omitempty"`
}

// Receipt is the atomic outcome of a block.
type Receipt struct {
	SchemaVersion   int             `json:"schemaVersion"`
	RunID           string          `json:"runId"`
	BlockID         string          `json:"blockId"`
	Status          string          `json:"status"`
	DelegatedParent string          `json:"delegatedParent"`
	SampleCount     int             `json:"sampleCount"`
	SamplesSHA256   string          `json:"samplesSha256"`
	Workers         []WorkerReceipt `json:"workers"`
	Escape          string          `json:"escape,omitempty"`
	Failure         string          `json:"failure,omitempty"`
	StartedAt       time.Time       `json:"startedAt"`
	DurationMillis  float64         `json:"durationMillis"`
}

// Result reports the written artifacts of a completed block.
type Result struct {
	Receipt       Receipt
	ReceiptPath   string
	ReceiptSHA256 string
}

// Coordinator runs one block.
type Coordinator struct {
	Containment     Containment
	LauncherFactory func(workers int) Launcher
	Events          EventSink
	Now             func() time.Time
	// KillFunc, when set, is called for an escaped process and its
	// descendants so an escape is stopped, not merely reported.
	KillFunc func(pid int) error
}

type workerState struct {
	spec   WorkerSpec
	handle containment.Handle
	worker Worker
}

func (c *Coordinator) now() time.Time {
	if c.Now == nil {
		return time.Now().UTC()
	}
	return c.Now()
}

func (c *Coordinator) emitPhase(phase, status string) {
	if c.Events == nil {
		return
	}
	_ = c.Events.Phase(phase, status)
}

func (c *Coordinator) emitCheck(name, status, message string) {
	if c.Events == nil {
		return
	}
	_ = c.Events.Check(name, status, message)
}

// Run executes the block. It always attempts cleanup, and returns an error
// unless the block completed and every post-condition was verified.
func (c *Coordinator) Run(ctx context.Context, req BlockRequest) (Result, error) {
	if err := validateRequest(req); err != nil {
		return Result{}, err
	}
	if c.Containment == nil || c.LauncherFactory == nil {
		return Result{}, errors.New("experiment: containment and launcher are required")
	}
	start := c.now()
	c.emitPhase("run-block", StatusStarted)

	if err := c.Containment.CheckDelegation(req.Parent); err != nil {
		c.emitCheck("cgroup-delegation", statusFor(err), err.Error())
		return Result{}, err
	}
	c.emitCheck("cgroup-delegation", StatusPassed, req.Parent)

	runCtx := ctx
	var cancel context.CancelFunc
	if req.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
	} else {
		runCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	var (
		states []workerState
		launch Launcher
	)
	cleanup := func() {
		for _, st := range states {
			if st.worker != nil {
				_ = st.worker.Stop()
			}
		}
		if launch != nil {
			_ = launch.Close()
		}
		for _, st := range states {
			if st.handle != nil {
				_ = st.handle.Remove()
			}
		}
	}

	// Create and verify every subtree before any worker starts.
	for i := range req.Workers {
		spec := req.Workers[i]
		h, err := c.Containment.Create(containment.Config{
			Parent: req.Parent,
			Name:   subtreeName(req, spec),
			CPUSet: spec.CPUList,
			Mems:   spec.Mems,
			CPUMax: spec.CPUMax,
		})
		if err != nil {
			cleanup()
			c.emitCheck("cgroup-create-"+spec.ID, statusFor(err), err.Error())
			return Result{}, err
		}
		if err := h.Verify(); err != nil {
			cleanup()
			c.emitCheck("cgroup-verify-"+spec.ID, StatusFailed, err.Error())
			return Result{}, err
		}
		c.emitCheck("cgroup-verified-"+spec.ID, StatusPassed, h.RelPath())
		states = append(states, workerState{spec: spec, handle: h})
	}

	launch = c.LauncherFactory(len(req.Workers))
	for i := range states {
		w, err := launch.Start(runCtx, states[i].spec, states[i].handle)
		if err != nil {
			cleanup()
			c.emitCheck("worker-start-"+states[i].spec.ID, StatusFailed, err.Error())
			return Result{}, fmt.Errorf("experiment: start worker %s: %w", states[i].spec.ID, err)
		}
		states[i].worker = w
	}
	if err := launch.WaitReady(runCtx); err != nil {
		cleanup()
		c.emitCheck("readiness-barrier", StatusFailed, err.Error())
		return Result{}, fmt.Errorf("experiment: readiness barrier: %w", err)
	}
	c.emitCheck("readiness-barrier", StatusPassed, "")
	if err := launch.Release(); err != nil {
		cleanup()
		return Result{}, fmt.Errorf("experiment: release barrier: %w", err)
	}

	// Monitor membership for as long as any worker is alive.
	monCtx, monCancel := context.WithCancel(runCtx)
	escapeCh := make(chan error, 1)
	go c.monitor(monCtx, states, req.MonitorInterval, escapeCh)

	waitErr := c.waitWorkers(runCtx, states, escapeCh)
	monCancel()
	if waitErr != nil {
		cleanup()
		name := "worker-wait"
		if errors.Is(waitErr, containment.ErrEscape) {
			name = "cgroup-escape"
		}
		c.emitCheck(name, StatusFailed, waitErr.Error())
		c.writeFailureReceipt(req, states, waitErr, start)
		return Result{}, waitErr
	}

	// Stop any stragglers explicitly, then confirm no membership remains.
	for _, st := range states {
		_ = st.worker.Stop()
		members, err := st.handle.Members()
		if err != nil {
			cleanup()
			return Result{}, fmt.Errorf("experiment: read members of %s: %w", st.handle.RelPath(), err)
		}
		if len(members) > 0 {
			cleanup()
			err := fmt.Errorf("%w: worker %s left pids %v", containment.ErrEscape, st.spec.ID, members)
			c.emitCheck("cgroup-residual-"+st.spec.ID, StatusFailed, err.Error())
			c.writeFailureReceipt(req, states, err, start)
			return Result{}, err
		}
		c.emitCheck("cgroup-empty-"+st.spec.ID, StatusPassed, st.handle.RelPath())
	}

	// Collect and count samples before removing evidence-bearing cgroups. Each
	// sample must prove its own containment; a sample that reports escape or a
	// mismatched worker identity is a failed block, not merely a warning.
	samples, sampleCount, err := collectSamples(req, states)
	if err != nil {
		cleanup()
		c.emitCheck("sample-count", StatusFailed, err.Error())
		c.writeFailureReceipt(req, states, err, start)
		return Result{}, err
	}
	c.emitCheck("sample-count", StatusPassed, fmt.Sprintf("%d samples", sampleCount))

	// Remove subtrees and prove none remain.
	for _, st := range states {
		if err := st.handle.Remove(); err != nil {
			cleanup()
			return Result{}, fmt.Errorf("experiment: remove %s: %w", st.handle.RelPath(), err)
		}
		exists, err := st.handle.Exists()
		if err != nil {
			cleanup()
			return Result{}, err
		}
		if exists {
			cleanup()
			err := fmt.Errorf("experiment: cgroup %s still exists after removal", st.handle.RelPath())
			c.writeFailureReceipt(req, states, err, start)
			return Result{}, err
		}
		c.emitCheck("cgroup-removed-"+st.spec.ID, StatusPassed, st.handle.RelPath())
	}
	if launch != nil {
		_ = launch.Close()
	}

	samplesHash := artifact.HashBytes(samples)
	samplesPath := filepath.Join(req.RunDir, filepath.FromSlash(req.SamplesName))
	if err := artifact.WriteFileAtomic(samplesPath, samples, 0o644); err != nil {
		return Result{}, fmt.Errorf("experiment: write samples: %w", err)
	}
	c.emitPhase("samples", StatusPassed)

	receipt := buildReceipt(req, states, sampleCount, samplesHash, "", "", start, c.now())
	receiptPath := filepath.Join(req.RunDir, filepath.FromSlash(req.ReceiptName))
	if err := artifact.WriteJSONAtomic(receiptPath, receipt, 0o644); err != nil {
		return Result{}, fmt.Errorf("experiment: write receipt: %w", err)
	}
	receiptSHA, err := hashFile(receiptPath)
	if err != nil {
		return Result{}, err
	}
	return Result{Receipt: receipt, ReceiptPath: receiptPath, ReceiptSHA256: receiptSHA}, nil
}

func (c *Coordinator) waitWorkers(ctx context.Context, states []workerState, escapeCh <-chan error) error {
	errCh := make(chan error, len(states))
	for _, st := range states {
		st := st
		go func() { errCh <- st.worker.Wait() }()
	}
	for range states {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-escapeCh:
			if err != nil {
				return err
			}
		case err := <-errCh:
			if err != nil {
				return fmt.Errorf("worker exited: %w", err)
			}
		}
	}
	return nil
}

// monitor polls process membership and reports the first escape.
func (c *Coordinator) monitor(ctx context.Context, states []workerState, interval time.Duration, escapeCh chan<- error) {
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, st := range states {
				pid := st.worker.PID()
				if pid <= 0 {
					continue
				}
				pids, err := c.Containment.Descendants(pid)
				if err != nil {
					// The worker may have exited; collectSamples and the
					// residual check are the authoritative post-conditions.
					continue
				}
				for _, p := range pids {
					cg, err := c.Containment.CgroupOf(p)
					if err != nil {
						continue
					}
					if !containment.IsWithin(st.handle.RelPath(), cg) {
						c.killEscape(p)
						select {
						case escapeCh <- fmt.Errorf("%w: pid %d in %s, want under %s", containment.ErrEscape, p, cg, st.handle.RelPath()):
						default:
						}
						return
					}
				}
			}
		}
	}
}

// killEscape stops an escaped process and its descendants when a kill boundary
// is configured. Errors are ignored: the block is failing regardless, and the
// post-conditions cannot be strengthened by a kill failure here.
func (c *Coordinator) killEscape(pid int) {
	if c.KillFunc == nil {
		return
	}
	targets := []int{pid}
	if desc, err := c.Containment.Descendants(pid); err == nil {
		targets = append(targets, desc...)
	}
	seen := map[int]bool{}
	for _, p := range targets {
		if p <= 0 || seen[p] {
			continue
		}
		seen[p] = true
		_ = c.KillFunc(p)
	}
}

func validateRequest(req BlockRequest) error {
	if req.RunID == "" || req.BlockID == "" {
		return errors.New("experiment: run and block ids are required")
	}
	if req.Parent == "" || !filepath.IsAbs(req.Parent) {
		return errors.New("experiment: delegated cgroup parent must be absolute")
	}
	if len(req.Workers) == 0 {
		return errors.New("experiment: at least one worker is required")
	}
	if req.RunDir == "" {
		return errors.New("experiment: run dir is required")
	}
	if req.SamplesName == "" || req.ReceiptName == "" {
		return errors.New("experiment: samples and receipt names are required")
	}
	if err := artifact.ValidateRelativePath(req.SamplesName); err != nil {
		return fmt.Errorf("experiment: samples name: %w", err)
	}
	if err := artifact.ValidateRelativePath(req.ReceiptName); err != nil {
		return fmt.Errorf("experiment: receipt name: %w", err)
	}
	seen := map[string]bool{}
	for i, w := range req.Workers {
		if w.ID == "" {
			return fmt.Errorf("experiment: worker %d has no id", i)
		}
		if seen[w.ID] {
			return fmt.Errorf("experiment: duplicate worker id %q", w.ID)
		}
		seen[w.ID] = true
		if w.CPUList == "" {
			return fmt.Errorf("experiment: worker %q has no cpu list", w.ID)
		}
		if w.ExpectedSamples < 1 {
			return fmt.Errorf("experiment: worker %q expected samples must be positive", w.ID)
		}
		if w.Command == "" && w.OutputPath == "" {
			return fmt.Errorf("experiment: worker %q has no command or output path", w.ID)
		}
		if w.OutputPath == "" {
			return fmt.Errorf("experiment: worker %q has no output path", w.ID)
		}
	}
	return nil
}

func subtreeName(req BlockRequest, spec WorkerSpec) string {
	return req.RunID + "/" + req.BlockID + "/" + spec.ID
}

// sampleProbe is the subset of a worker NDJSON sample whose containment and
// identity the coordinator must independently confirm before a block passes.
type sampleProbe struct {
	RunID           string `json:"runId"`
	BlockID         string `json:"blockId"`
	Worker          string `json:"worker"`
	CgroupContained bool   `json:"cgroupContained"`
	CgroupPath      string `json:"cgroupPath"`
}

func collectSamples(req BlockRequest, states []workerState) ([]byte, int, error) {
	var buf bytes.Buffer
	total := 0
	for _, st := range states {
		data, err := os.ReadFile(st.spec.OutputPath)
		if err != nil {
			return nil, 0, fmt.Errorf("experiment: worker %s samples: %w", st.spec.ID, err)
		}
		count := 0
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			var probe sampleProbe
			if err := json.Unmarshal(line, &probe); err != nil {
				return nil, 0, fmt.Errorf("experiment: worker %s emitted invalid NDJSON: %w", st.spec.ID, err)
			}
			if err := validateSample(req, st, probe); err != nil {
				return nil, 0, fmt.Errorf("experiment: worker %s sample %d: %w", st.spec.ID, count, err)
			}
			buf.Write(line)
			buf.WriteByte('\n')
			count++
			total++
		}
		if count != st.spec.ExpectedSamples {
			return nil, 0, fmt.Errorf("experiment: worker %s produced %d samples, expected %d", st.spec.ID, count, st.spec.ExpectedSamples)
		}
	}
	return buf.Bytes(), total, nil
}

// validateSample rejects a sample that does not prove containment in the
// worker's own subtree or that carries another worker's identity.
func validateSample(req BlockRequest, st workerState, s sampleProbe) error {
	if !s.CgroupContained {
		return fmt.Errorf("%w: sample is not cgroupContained", containment.ErrEscape)
	}
	if s.Worker != st.spec.ID {
		return fmt.Errorf("worker identity mismatch: sample names %q, want %q", s.Worker, st.spec.ID)
	}
	if s.RunID != "" && s.RunID != req.RunID {
		return fmt.Errorf("run identity mismatch: sample names %q, want %q", s.RunID, req.RunID)
	}
	if s.BlockID != "" && s.BlockID != req.BlockID {
		return fmt.Errorf("block identity mismatch: sample names %q, want %q", s.BlockID, req.BlockID)
	}
	if s.CgroupPath == "" {
		return errors.New("sample has no cgroupPath")
	}
	if !filepath.IsAbs(s.CgroupPath) {
		return fmt.Errorf("sample cgroupPath %q is not absolute", s.CgroupPath)
	}
	if st.handle == nil || !pathWithin(st.handle.FSPath(), s.CgroupPath) {
		want := ""
		if st.handle != nil {
			want = st.handle.FSPath()
		}
		return fmt.Errorf("%w: sample cgroupPath %q is not beneath worker subtree %q", containment.ErrEscape, s.CgroupPath, want)
	}
	return nil
}

// pathWithin reports whether child is parent or a descendant of it, using
// cleaned absolute cgroup filesystem paths. The cgroup root is never accepted.
func pathWithin(parent, child string) bool {
	p := filepath.Clean(parent)
	c := filepath.Clean(child)
	if p == "" || p == "." || p == "/" || c == "" || c == "." {
		return false
	}
	return c == p || strings.HasPrefix(c, p+string(os.PathSeparator))
}

func (c *Coordinator) writeFailureReceipt(req BlockRequest, states []workerState, cause error, start time.Time) {
	receipt := buildReceipt(req, states, 0, "", cause.Error(), "", start, c.now())
	receipt.Status = StatusFailed
	path := filepath.Join(req.RunDir, filepath.FromSlash(req.ReceiptName))
	_ = artifact.WriteJSONAtomic(path, receipt, 0o644)
}

func buildReceipt(req BlockRequest, states []workerState, sampleCount int, samplesHash, failure, escape string, start, end time.Time) Receipt {
	r := Receipt{
		SchemaVersion:   1,
		RunID:           req.RunID,
		BlockID:         req.BlockID,
		Status:          StatusPassed,
		DelegatedParent: req.Parent,
		SampleCount:     sampleCount,
		SamplesSHA256:   samplesHash,
		Escape:          escape,
		Failure:         failure,
		StartedAt:       start.UTC(),
		DurationMillis:  float64(end.Sub(start).Milliseconds()),
	}
	for _, st := range states {
		wr := WorkerReceipt{
			ID:              st.spec.ID,
			CPUList:         st.spec.CPUList,
			Mems:            st.spec.Mems,
			CPUMax:          st.spec.CPUMax,
			ExpectedSamples: st.spec.ExpectedSamples,
		}
		if st.worker != nil {
			wr.PID = st.worker.PID()
		}
		if st.handle != nil {
			wr.Cgroup = st.handle.RelPath()
		}
		r.Workers = append(r.Workers, wr)
	}
	sort.Slice(r.Workers, func(i, j int) bool { return r.Workers[i].ID < r.Workers[j].ID })
	return r
}

func hashFile(path string) (string, error) {
	sum, _, err := artifact.HashFile(path)
	return sum, err
}

func statusFor(err error) string {
	if errors.Is(err, containment.ErrUnsupported) || errors.Is(err, containment.ErrNotDelegated) {
		return StatusUnsupported
	}
	return StatusFailed
}
