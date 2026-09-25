package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/skkuding/iris-load-test-toolset/internal/artifact"
	"github.com/skkuding/iris-load-test-toolset/internal/containment"
	"github.com/skkuding/iris-load-test-toolset/internal/experiment"
	"github.com/skkuding/iris-load-test-toolset/internal/protocol"
	"github.com/skkuding/iris-load-test-toolset/internal/runplan"
)

// errTerminalFailed signals that a terminal result event was emitted with a
// non-completed status. The process must exit non-zero.
var errTerminalFailed = errors.New("agent: operation did not complete")

type agent struct {
	varRoot  string
	runRoot  string
	planFile string
	version  string
	stderr   io.Writer
	now      func() time.Time

	cgroupParent    string
	cgroupMount     string
	cpusetMems      string
	benchBinary     string
	workerBin       string
	workerArgs      []string
	judgerPath      string
	judgerSHA256    string
	containerID     string
	workerTimeout   time.Duration
	monitorInterval time.Duration
	runTimeout      time.Duration

	// Test seams for the filesystem, process, and launcher boundaries.
	cgroupFS    containment.FS
	cgroupProc  containment.Proc
	newLauncher func(workers int) (experiment.Launcher, error)
}

func (a *agent) fs() containment.FS {
	if a.cgroupFS != nil {
		return a.cgroupFS
	}
	return containment.RealFS{}
}

func (a *agent) proc() containment.Proc {
	if a.cgroupProc != nil {
		return a.cgroupProc
	}
	return containment.RealProc{}
}

func (a *agent) launcherFactory(workers int) (experiment.Launcher, error) {
	if a.newLauncher != nil {
		return a.newLauncher(workers)
	}
	wrapper, err := os.Executable()
	if err != nil {
		return nil, err
	}
	l, err := experiment.NewExecLauncher(workers, wrapper)
	if err != nil {
		return nil, err
	}
	l.Mount = a.mount()
	l.Stderr = a.stderr
	return l, nil
}

func (a *agent) mount() string {
	if a.cgroupMount == "" {
		return containment.DefaultMount
	}
	return a.cgroupMount
}

func (a *agent) clock() time.Time {
	if a.now == nil {
		return time.Now().UTC()
	}
	return a.now()
}

// execute dispatches one fixed verb. It always emits exactly one terminal
// result event, including on internal failure.
func (a *agent) execute(ctx context.Context, req protocol.Request, enc *protocol.EventEncoder) (err error) {
	defer func() {
		if r := recover(); r != nil {
			_ = enc.Emit(protocol.Event{Kind: protocol.KindResult, Status: protocol.StatusFailed, Message: "internal panic"})
			err = fmt.Errorf("%w: internal panic: %v", errTerminalFailed, r)
		}
	}()
	if err := req.Validate(); err != nil {
		return a.fail(enc, err.Error())
	}
	if err := ctx.Err(); err != nil {
		return a.fail(enc, "cancelled before start")
	}

	switch req.Action {
	case protocol.ActionInspect:
		return a.inspect(ctx, req, enc)
	case protocol.ActionStatus:
		return a.status(req, enc)
	}

	release, err := a.lock(req.RunID)
	if err != nil {
		return a.fail(enc, err.Error())
	}
	defer release()

	switch req.Action {
	case protocol.ActionPrepare:
		return a.prepare(req, enc)
	case protocol.ActionRunBlock:
		return a.runBlock(ctx, req, enc)
	case protocol.ActionValidate:
		return a.validate(req, enc)
	case protocol.ActionBundle:
		return a.bundle(req, enc)
	case protocol.ActionCleanup:
		return a.cleanup(req, enc)
	default:
		return a.fail(enc, "unsupported action "+string(req.Action))
	}
}

func (a *agent) runDir(runID string) string { return filepath.Join(a.varRoot, runID) }
func (a *agent) rtDir(runID string) string  { return filepath.Join(a.runRoot, runID) }

func (a *agent) planPath(runID string) string {
	if a.planFile != "" {
		return a.planFile
	}
	return filepath.Join(a.runDir(runID), "plan.json")
}

// record is written for each mutating phase.
type record struct {
	RunID        string    `json:"runId"`
	PlanSHA256   string    `json:"planSha256,omitempty"`
	AgentVersion string    `json:"agentVersion"`
	Action       string    `json:"action"`
	At           time.Time `json:"at"`
	Detail       string    `json:"detail,omitempty"`
}

func (a *agent) writeRecord(runID, planSHA, action, detail string) (string, error) {
	rec := record{
		RunID:        runID,
		PlanSHA256:   planSHA,
		AgentVersion: a.version,
		Action:       action,
		At:           a.clock(),
		Detail:       detail,
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	path := filepath.Join(a.runDir(runID), action+".json")
	if err := artifact.WriteFileAtomic(path, data, 0o644); err != nil {
		return "", err
	}
	return artifact.HashBytes(data), nil
}

// verifyPlanMatches confirms the staged plan against the digest carried by the
// request. The request always carries the canonical plan digest computed over
// the compact plan JSON (runplan.Plan.Digest), never the SHA-256 of the
// indented staging file. The staged bytes are parsed and re-canonicalized, so
// formatting differences between the writer and this build cannot silently
// change meaning, and a file-bytes hash is rejected.
func (a *agent) verifyPlanMatches(runID, want string) error {
	if !artifact.ValidSHA256(want) {
		return errors.New("request plan digest is not a valid sha256")
	}
	plan := a.planPath(runID)
	data, err := os.ReadFile(plan)
	if err != nil {
		return fmt.Errorf("staged plan: %w", err)
	}
	var p runplan.Plan
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("staged plan: %w", err)
	}
	if err := p.Validate(); err != nil {
		return fmt.Errorf("staged plan: %w", err)
	}
	digest, err := p.Digest()
	if err != nil {
		return fmt.Errorf("staged plan: %w", err)
	}
	if digest != want {
		return fmt.Errorf("staged plan canonical digest %s does not match request %s", digest, want)
	}
	return nil
}

func (a *agent) inspect(ctx context.Context, req protocol.Request, enc *protocol.EventEncoder) error {
	if err := enc.Emit(protocol.Event{Kind: protocol.KindPhase, Phase: "inspect", Status: protocol.StatusStarted}); err != nil {
		return err
	}
	facts := collectFacts()
	facts["agentVersion"] = a.version
	dir := filepath.Join(a.runDir(req.RunID), "qualification")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return a.fail(enc, "create qualification dir: "+err.Error())
	}
	data, err := json.MarshalIndent(facts, "", "  ")
	if err != nil {
		return a.fail(enc, err.Error())
	}
	data = append(data, '\n')
	path := filepath.Join(dir, "host-facts.json")
	if err := artifact.WriteFileAtomic(path, data, 0o644); err != nil {
		return a.fail(enc, "write facts: "+err.Error())
	}
	if err := enc.Emit(protocol.Event{Kind: protocol.KindArtifact, Path: "qualification/host-facts.json", SHA256: artifact.HashBytes(data)}); err != nil {
		return err
	}
	if err := enc.Emit(protocol.Event{Kind: protocol.KindCheck, Name: "cgroup-v2", Status: statusIf(facts["cgroupVersion"] == "v2")}); err != nil {
		return err
	}
	if err := enc.Emit(protocol.Event{Kind: protocol.KindCheck, Name: "kernel", Status: protocol.StatusPassed}); err != nil {
		return err
	}
	return a.complete(enc, artifact.HashBytes(data))
}

func (a *agent) prepare(req protocol.Request, enc *protocol.EventEncoder) error {
	if err := enc.Emit(protocol.Event{Kind: protocol.KindPhase, Phase: "prepare", Status: protocol.StatusStarted}); err != nil {
		return err
	}
	if err := a.verifyPlanMatches(req.RunID, req.PlanSHA256); err != nil {
		_ = enc.Emit(protocol.Event{Kind: protocol.KindCheck, Name: "plan-digest", Status: protocol.StatusFailed, Message: sanitize(err.Error())})
		return a.fail(enc, err.Error())
	}
	if err := enc.Emit(protocol.Event{Kind: protocol.KindCheck, Name: "plan-digest", Status: protocol.StatusPassed}); err != nil {
		return err
	}
	for _, d := range []string{a.runDir(req.RunID), a.rtDir(req.RunID)} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return a.fail(enc, "create dir: "+err.Error())
		}
	}
	planBytes, err := os.ReadFile(a.planPath(req.RunID))
	if err != nil {
		return a.fail(enc, "read staged plan: "+err.Error())
	}
	if err := artifact.WriteFileAtomic(filepath.Join(a.runDir(req.RunID), "plan.json"), planBytes, 0o644); err != nil {
		return a.fail(enc, "store plan: "+err.Error())
	}
	receipt, err := a.writeRecord(req.RunID, req.PlanSHA256, "prepared", "plan staged")
	if err != nil {
		return a.fail(enc, err.Error())
	}
	return a.complete(enc, receipt)
}

// runBlock executes one direct-suite measurement block. It refuses to start
// unless the run is qualified and prepared, the staged plan digest matches, and
// an explicitly delegated cgroup parent was configured. It never silently
// claims containment: an unsupported or unverified boundary is a failed run.
func (a *agent) runBlock(ctx context.Context, req protocol.Request, enc *protocol.EventEncoder) error {
	if err := enc.Emit(protocol.Event{Kind: protocol.KindPhase, Phase: "run-block", Status: protocol.StatusStarted}); err != nil {
		return err
	}
	if err := a.verifyRunReady(req.RunID, req.PlanSHA256); err != nil {
		_ = enc.Emit(protocol.Event{Kind: protocol.KindCheck, Name: "plan-digest", Status: protocol.StatusFailed, Message: sanitize(err.Error())})
		return a.fail(enc, err.Error())
	}
	_ = enc.Emit(protocol.Event{Kind: protocol.KindCheck, Name: "plan-digest", Status: protocol.StatusPassed})

	if a.cgroupParent == "" {
		_ = enc.Emit(protocol.Event{Kind: protocol.KindCheck, Name: "cgroup-delegation", Status: protocol.StatusUnsupported, Message: "no delegated cgroup parent configured"})
		return a.unsupported(enc, "run-block requires --cgroup-parent naming an explicitly delegated cgroup v2 subtree")
	}

	plan, err := a.loadPlan(req.RunID)
	if err != nil {
		_ = enc.Emit(protocol.Event{Kind: protocol.KindCheck, Name: "plan", Status: protocol.StatusFailed, Message: sanitize(err.Error())})
		return a.fail(enc, err.Error())
	}
	block, ok := findBlock(plan, req.BlockID)
	if !ok {
		return a.fail(enc, "unknown block id "+req.BlockID)
	}
	if block.Suite != "judger" {
		_ = enc.Emit(protocol.Event{Kind: protocol.KindCheck, Name: "suite", Status: protocol.StatusUnsupported, Message: "suite=" + block.Suite})
		return a.unsupported(enc, "run-block supports only the direct judger suite in this build, got "+block.Suite)
	}
	specs, err := a.buildWorkerSpecs(req, block, plan)
	if err != nil {
		_ = enc.Emit(protocol.Event{Kind: protocol.KindCheck, Name: "worker-plan", Status: protocol.StatusFailed, Message: sanitize(err.Error())})
		return a.fail(enc, err.Error())
	}

	mgr := &containment.Manager{Mount: a.mount(), FS: a.fs(), Proc: a.proc()}
	coord := &experiment.Coordinator{
		Containment: mgr,
		LauncherFactory: func(workers int) experiment.Launcher {
			l, lerr := a.launcherFactory(workers)
			if lerr != nil {
				return errorLauncher{err: lerr}
			}
			return l
		},
		Events:   eventSink{enc: enc},
		KillFunc: func(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) },
	}
	timeout := a.runTimeout
	if timeout <= 0 && plan.Qualification.MaxRunSeconds > 0 {
		timeout = time.Duration(plan.Qualification.MaxRunSeconds) * time.Second
	}
	runDir := a.runDir(req.RunID)
	result, err := coord.Run(ctx, experiment.BlockRequest{
		RunID:           req.RunID,
		BlockID:         block.ID,
		Parent:          a.cgroupParent,
		Workers:         specs,
		Timeout:         timeout,
		MonitorInterval: a.monitorInterval,
		RunDir:          runDir,
		SamplesName:     "samples/judger.ndjson",
		ReceiptName:     filepath.ToSlash(filepath.Join("receipts", block.ID+".json")),
	})
	if err != nil {
		// A missing or invalid delegation is a containment boundary the host
		// does not provide, not a measurement failure. Surface it as
		// unsupported so the controller can refuse the run.
		if errors.Is(err, containment.ErrUnsupported) || errors.Is(err, containment.ErrNotDelegated) {
			return a.unsupported(enc, err.Error())
		}
		return a.fail(enc, err.Error())
	}

	relSamples := "samples/judger.ndjson"
	samplesSHA, _, herr := artifact.HashFile(filepath.Join(runDir, filepath.FromSlash(relSamples)))
	if herr != nil {
		return a.fail(enc, "hash samples: "+herr.Error())
	}
	if err := enc.Emit(protocol.Event{Kind: protocol.KindArtifact, Path: relSamples, SHA256: samplesSHA}); err != nil {
		return err
	}
	relReceipt := filepath.ToSlash(filepath.Join("receipts", block.ID+".json"))
	if err := enc.Emit(protocol.Event{Kind: protocol.KindArtifact, Path: relReceipt, SHA256: result.ReceiptSHA256}); err != nil {
		return err
	}
	receipt, err := a.writeRecord(req.RunID, req.PlanSHA256, "run-block", "block "+block.ID+" samples="+strconv.Itoa(result.Receipt.SampleCount))
	if err != nil {
		return a.fail(enc, err.Error())
	}
	return a.complete(enc, receipt)
}

// verifyRunReady enforces the qualified/prepared precondition and confirms the
// staged plan digest. It reads only records this agent wrote.
func (a *agent) verifyRunReady(runID, planSHA string) error {
	if err := a.verifyPlanMatches(runID, planSHA); err != nil {
		return err
	}
	qualified := filepath.Join(a.runDir(runID), "qualification", "host-facts.json")
	if _, err := os.Stat(qualified); err != nil {
		return fmt.Errorf("run is not qualified: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(a.runDir(runID), "prepared.json"))
	if err != nil {
		return fmt.Errorf("run is not prepared: %w", err)
	}
	var rec record
	if err := json.Unmarshal(data, &rec); err != nil {
		return fmt.Errorf("prepared record: %w", err)
	}
	if rec.Action != "prepared" || rec.PlanSHA256 != planSHA {
		return errors.New("prepared record does not match the request plan digest")
	}
	return nil
}

func (a *agent) loadPlan(runID string) (runplan.Plan, error) {
	path := filepath.Join(a.runDir(runID), "plan.json")
	if _, err := os.Stat(path); err != nil {
		path = a.planPath(runID)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return runplan.Plan{}, fmt.Errorf("read plan: %w", err)
	}
	var p runplan.Plan
	if err := json.Unmarshal(data, &p); err != nil {
		return runplan.Plan{}, fmt.Errorf("parse plan: %w", err)
	}
	if err := p.Validate(); err != nil {
		return runplan.Plan{}, err
	}
	return p, nil
}

func findBlock(plan runplan.Plan, blockID string) (runplan.Block, bool) {
	for _, b := range plan.Blocks {
		if b.ID == blockID {
			return b, true
		}
	}
	return runplan.Block{}, false
}

// buildWorkerSpecs assigns a disjoint CPU set to every worker, derives a CPU
// quota, and constructs the worker command. It refuses to guess: a missing CPU
// list, missing binary, or oversubscribed assignment fails before any subtree
// is created.
func (a *agent) buildWorkerSpecs(req protocol.Request, block runplan.Block, plan runplan.Plan) ([]experiment.WorkerSpec, error) {
	if block.Workers < 1 {
		return nil, errors.New("block has no workers")
	}
	if block.CPUList == "" {
		return nil, errors.New("block cpuList is required for the direct suite")
	}
	cpusets, err := containment.SplitCPUList(block.CPUList, block.Workers)
	if err != nil {
		return nil, err
	}
	bin := strings.TrimSpace(a.workerBin)
	if bin == "" {
		return nil, errors.New("worker-bin is required")
	}
	// The Judger digest always comes from the sealed plan. An explicit
	// --judger-sha256 is accepted only when it agrees with the plan.
	judgerSHA, err := a.planJudgerSHA(plan)
	if err != nil {
		return nil, err
	}
	blockMax := blockTimeout(plan)
	if a.runTimeout > 0 {
		blockMax = a.runTimeout
	}
	workerDone := a.workerTimeout
	if workerDone <= 0 {
		workerDone = deriveWorkerTimeout(blockMax, block.Repetitions)
	}
	fixture := ""
	if len(plan.Fixtures) > 0 {
		fixture = plan.Fixtures[0].Name
	}
	runtimeDir := a.rtDir(req.RunID)
	if err := os.MkdirAll(runtimeDir, 0o750); err != nil {
		return nil, err
	}
	outDir := filepath.Join(runtimeDir, block.ID)
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return nil, err
	}
	specs := make([]experiment.WorkerSpec, 0, block.Workers)
	for i := 0; i < block.Workers; i++ {
		id := fmt.Sprintf("worker-%02d", i+1)
		output := filepath.Join(outDir, id+".ndjson")
		// Judger must create its sandbox beneath the exact subtree this worker
		// runs in, and a distinct CONTAINER_ID keeps per-worker sandbox names
		// from colliding.
		expectedParent := filepath.Join(a.cgroupParent, plan.RunID, block.ID, id)
		containerID := workerContainerID(a.containerID, plan, block, id)
		args, err := a.workerArguments(id, block, plan, cpusets[i], output, fixture, expectedParent, containerID, judgerSHA, workerDone)
		if err != nil {
			return nil, err
		}
		n, err := containment.CountCPUs(cpusets[i])
		if err != nil {
			return nil, err
		}
		specs = append(specs, experiment.WorkerSpec{
			ID:              id,
			CPUList:         cpusets[i],
			Mems:            a.cpusetMems,
			CPUMax:          fmt.Sprintf("%d 100000", n*100000),
			Command:         bin,
			Args:            args,
			Fixture:         fixture,
			ExpectedSamples: block.Repetitions,
			OutputPath:      output,
		})
	}
	return specs, nil
}

// planJudgerSHA returns the raw hex Judger digest carried by the plan and
// rejects an explicit --judger-sha256 that disagrees with it.
func (a *agent) planJudgerSHA(plan runplan.Plan) (string, error) {
	want := strings.TrimPrefix(strings.TrimSpace(plan.JudgerDigest), "sha256:")
	if !artifact.ValidSHA256(want) {
		return "", fmt.Errorf("plan judger digest %q is not a valid sha256", plan.JudgerDigest)
	}
	if got := strings.TrimSpace(a.judgerSHA256); got != "" {
		got = strings.TrimPrefix(got, "sha256:")
		if !strings.EqualFold(got, want) {
			return "", fmt.Errorf("--judger-sha256 %s does not match plan judger digest %s", got, want)
		}
	}
	return want, nil
}

// workerContainerID derives a stable, safe-per-worker CONTAINER_ID. Judger
// derives sandbox-<id> from it, so it must not contain path separators.
func workerContainerID(base string, plan runplan.Plan, block runplan.Block, worker string) string {
	if strings.TrimSpace(base) == "" {
		base = plan.RunID
	}
	if block.ID != "" {
		base = base + "-" + block.ID
	}
	return sanitizeContainerID(base + "-" + worker)
}

func sanitizeContainerID(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if out == "" {
		return "bench"
	}
	if len(out) > 128 {
		out = out[:128]
	}
	return out
}

func (a *agent) workerArguments(worker string, block runplan.Block, plan runplan.Plan, cpuset, output, fixture, expectedParent, containerID, judgerSHA string, workerTimeout time.Duration) ([]string, error) {
	vars := map[string]string{
		"run":                    plan.RunID,
		"block":                  block.ID,
		"worker":                 worker,
		"fixture":                fixture,
		"output":                 output,
		"iterations":             strconv.Itoa(block.Repetitions),
		"cpuset":                 cpuset,
		"bench-binary":           a.benchBinary,
		"run-dir":                a.runDir(plan.RunID),
		"runtime-dir":            a.rtDir(plan.RunID),
		"timeout":                workerTimeout.String(),
		"judger":                 a.judgerPath,
		"judger-sha256":          judgerSHA,
		"container-id":           containerID,
		"expected-cgroup-parent": expectedParent,
	}
	templates := a.workerArgs
	if len(templates) == 0 {
		if a.benchBinary == "" {
			return nil, errors.New("run-block requires --bench-binary when no --worker-arg template is given")
		}
		templates = []string{
			"--mode", "execute",
			"--run-id", "{run}",
			"--block-id", "{block}",
			"--worker", "{worker}",
			"--fixture", "{fixture}",
			"--binary", "{bench-binary}",
			"--output", "{output}",
			"--iterations", "{iterations}",
			"--timeout", "{timeout}",
			"--judger-sha256", "{judger-sha256}",
			"--container-id", "{container-id}",
			"--expected-cgroup-parent", "{expected-cgroup-parent}",
			// Production runs Judger privileged; a different sandbox identity
			// is a separate comparison population.
			"--uid", "0",
			"--gid", "0",
		}
		if a.judgerPath != "" {
			templates = append(templates, "--judger", "{judger}")
		}
	}
	out := make([]string, 0, len(templates))
	for _, t := range templates {
		for k, v := range vars {
			t = strings.ReplaceAll(t, "{"+k+"}", v)
		}
		out = append(out, t)
	}
	return out, nil
}

// blockTimeout is the whole-block ceiling from the plan.
func blockTimeout(plan runplan.Plan) time.Duration {
	if plan.Qualification.MaxRunSeconds > 0 {
		return time.Duration(plan.Qualification.MaxRunSeconds) * time.Second
	}
	return 10 * time.Second
}

// deriveWorkerTimeout bounds one worker's outer command timeout independently
// from the whole-block ceiling. It reserves one repetition's worth of budget
// so a single stuck worker cannot consume the entire block, and never returns
// the full block ceiling.
func deriveWorkerTimeout(blockMax time.Duration, repetitions int) time.Duration {
	if blockMax <= 0 {
		blockMax = 10 * time.Second
	}
	if repetitions < 1 {
		repetitions = 1
	}
	per := blockMax / time.Duration(repetitions+1)
	const floor = time.Second
	if per < floor {
		per = floor
	}
	if per >= blockMax {
		per = blockMax / 2
	}
	return per
}

// eventSink adapts the experiment event boundary to protocol events.
type eventSink struct{ enc *protocol.EventEncoder }

func (s eventSink) Phase(phase, status string) error {
	return s.enc.Emit(protocol.Event{Kind: protocol.KindPhase, Phase: phase, Status: status})
}

func (s eventSink) Check(name, status, message string) error {
	return s.enc.Emit(protocol.Event{Kind: protocol.KindCheck, Name: name, Status: status, Message: sanitize(message)})
}

// errorLauncher surfaces a launcher construction failure at Start time.
type errorLauncher struct{ err error }

func (l errorLauncher) Start(context.Context, experiment.WorkerSpec, containment.Handle) (experiment.Worker, error) {
	return nil, l.err
}
func (l errorLauncher) WaitReady(context.Context) error { return l.err }
func (l errorLauncher) Release() error                  { return l.err }
func (l errorLauncher) Close() error                    { return nil }

func (a *agent) validate(req protocol.Request, enc *protocol.EventEncoder) error {
	if err := enc.Emit(protocol.Event{Kind: protocol.KindPhase, Phase: "validate", Status: protocol.StatusStarted}); err != nil {
		return err
	}
	if err := a.verifyPlanMatches(req.RunID, req.PlanSHA256); err != nil {
		_ = enc.Emit(protocol.Event{Kind: protocol.KindCheck, Name: "plan-digest", Status: protocol.StatusFailed, Message: sanitize(err.Error())})
		return a.fail(enc, err.Error())
	}
	invPath := filepath.Join(a.runDir(req.RunID), "bundle-inventory.json")
	if _, err := os.Stat(invPath); err == nil {
		data, rerr := os.ReadFile(invPath)
		if rerr != nil {
			return a.fail(enc, "read inventory: "+rerr.Error())
		}
		var inv artifact.Inventory
		if uerr := json.Unmarshal(data, &inv); uerr != nil {
			return a.fail(enc, "parse inventory: "+uerr.Error())
		}
		if verr := artifact.VerifyInventory(a.runDir(req.RunID), inv); verr != nil {
			_ = enc.Emit(protocol.Event{Kind: protocol.KindCheck, Name: "artifact-integrity", Status: protocol.StatusFailed, Message: sanitize(verr.Error())})
			return a.fail(enc, verr.Error())
		}
		if err := enc.Emit(protocol.Event{Kind: protocol.KindCheck, Name: "artifact-integrity", Status: protocol.StatusPassed}); err != nil {
			return err
		}
	}
	receipt, err := a.writeRecord(req.RunID, req.PlanSHA256, "validated", "plan and artifacts verified")
	if err != nil {
		return a.fail(enc, err.Error())
	}
	return a.complete(enc, receipt)
}

func (a *agent) bundle(req protocol.Request, enc *protocol.EventEncoder) error {
	if err := enc.Emit(protocol.Event{Kind: protocol.KindPhase, Phase: "bundle", Status: protocol.StatusStarted}); err != nil {
		return err
	}
	inv, err := artifact.BuildInventory(a.runDir(req.RunID))
	if err != nil {
		return a.fail(enc, "build inventory: "+err.Error())
	}
	data, err := inv.Marshal()
	if err != nil {
		return a.fail(enc, "marshal inventory: "+err.Error())
	}
	data = append(data, '\n')
	path := filepath.Join(a.runDir(req.RunID), "bundle-inventory.json")
	if err := artifact.WriteFileAtomic(path, data, 0o644); err != nil {
		return a.fail(enc, "write inventory: "+err.Error())
	}
	if err := enc.Emit(protocol.Event{Kind: protocol.KindArtifact, Path: "bundle-inventory.json", SHA256: artifact.HashBytes(data)}); err != nil {
		return err
	}
	return a.complete(enc, artifact.HashBytes(data))
}

func (a *agent) cleanup(req protocol.Request, enc *protocol.EventEncoder) error {
	if err := enc.Emit(protocol.Event{Kind: protocol.KindPhase, Phase: "cleanup", Status: protocol.StatusStarted}); err != nil {
		return err
	}
	rt := a.rtDir(req.RunID)
	if filepath.Base(rt) != req.RunID {
		return a.fail(enc, "refusing to remove unexpected path")
	}
	if err := os.RemoveAll(rt); err != nil {
		return a.fail(enc, "remove runtime dir: "+err.Error())
	}
	if err := enc.Emit(protocol.Event{Kind: protocol.KindCheck, Name: "runtime-cleaned", Status: protocol.StatusPassed}); err != nil {
		return err
	}
	receipt, err := a.writeRecord(req.RunID, req.PlanSHA256, "cleaned", "runtime dir removed; evidence preserved")
	if err != nil {
		return a.fail(enc, err.Error())
	}
	return a.complete(enc, receipt)
}

func (a *agent) status(req protocol.Request, enc *protocol.EventEncoder) error {
	if err := enc.Emit(protocol.Event{Kind: protocol.KindPhase, Phase: "status", Status: protocol.StatusStarted}); err != nil {
		return err
	}
	prepared := filepath.Join(a.runDir(req.RunID), "prepared.json")
	st := protocol.StatusFailed
	if _, err := os.Stat(prepared); err == nil {
		st = protocol.StatusPassed
	}
	if err := enc.Emit(protocol.Event{Kind: protocol.KindCheck, Name: "prepared", Status: st}); err != nil {
		return err
	}
	return a.complete(enc, "")
}

func (a *agent) complete(enc *protocol.EventEncoder, receipt string) error {
	return enc.Emit(protocol.Event{Kind: protocol.KindResult, Status: protocol.StatusCompleted, ReceiptSHA256: receipt})
}

func (a *agent) fail(enc *protocol.EventEncoder, msg string) error {
	_ = enc.Emit(protocol.Event{Kind: protocol.KindResult, Status: protocol.StatusFailed, Message: sanitize(msg)})
	return fmt.Errorf("%w: %s", errTerminalFailed, msg)
}

func (a *agent) unsupported(enc *protocol.EventEncoder, msg string) error {
	_ = enc.Emit(protocol.Event{Kind: protocol.KindResult, Status: protocol.StatusUnsupported, Message: sanitize(msg)})
	return fmt.Errorf("%w: %s", errTerminalFailed, msg)
}

func statusIf(ok bool) string {
	if ok {
		return protocol.StatusPassed
	}
	return protocol.StatusFailed
}

// sanitize bounds and strips control characters from event messages.
func sanitize(s string) string {
	const max = protocol.MaxMessageLen
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			out = append(out, ' ')
			continue
		}
		if r < 0x20 || r == 0x7f {
			continue
		}
		out = append(out, r)
	}
	if len(out) > max {
		out = out[:max]
	}
	return string(out)
}
