package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/skkuding/iris-load-test-toolset/internal/artifact"
	"github.com/skkuding/iris-load-test-toolset/internal/containment"
	"github.com/skkuding/iris-load-test-toolset/internal/experiment"
	"github.com/skkuding/iris-load-test-toolset/internal/protocol"
	"github.com/skkuding/iris-load-test-toolset/internal/qualification"
	"github.com/skkuding/iris-load-test-toolset/internal/runplan"
	"github.com/skkuding/iris-load-test-toolset/internal/telemetry"
	"github.com/skkuding/iris-load-test-toolset/internal/transport"
)

// errTerminalFailed signals that a terminal result event was emitted with a
// non-completed status. The process must exit non-zero.
var errTerminalFailed = errors.New("agent: operation did not complete")

const ociJudgerPath = "/app/sandbox/libjudger.so"

var dockerNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

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
	benchmarkImage  string
	containerID     string
	workerTimeout   time.Duration
	monitorInterval time.Duration
	runTimeout      time.Duration

	// Test seams for the filesystem, process, and launcher boundaries.
	cgroupFS     containment.FS
	cgroupProc   containment.Proc
	newLauncher  func(workers int) (experiment.Launcher, error)
	telemetry    telemetry.Sampler
	dockerRunner runplan.CommandRunner
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

func (a *agent) telemetrySampler() telemetry.Sampler {
	if a.telemetry != nil {
		return a.telemetry
	}
	return telemetry.SysfsSampler{}
}

func (a *agent) docker() runplan.CommandRunner {
	if a.dockerRunner != nil {
		return a.dockerRunner
	}
	return transport.OSExecer{}
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
	if err := a.verifyPlanMatches(req.RunID, req.PlanSHA256); err != nil {
		return a.fail(enc, err.Error())
	}
	plan, err := a.loadPlanPath(a.planPath(req.RunID))
	if err != nil {
		return a.fail(enc, err.Error())
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
	if plan.Qualification.Report == nil {
		if err := enc.Emit(protocol.Event{Kind: protocol.KindCheck, Name: "ansible-qualification", Status: protocol.StatusUnsupported, Message: "no sealed report; run will be non-comparable"}); err != nil {
			return err
		}
	} else {
		if err := a.copyQualificationReport(req.RunID, plan); err != nil {
			_ = enc.Emit(protocol.Event{Kind: protocol.KindCheck, Name: "ansible-qualification", Status: protocol.StatusFailed, Message: sanitize(err.Error())})
			return a.fail(enc, err.Error())
		}
		if err := enc.Emit(protocol.Event{Kind: protocol.KindCheck, Name: "ansible-qualification", Status: protocol.StatusPassed}); err != nil {
			return err
		}
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
	plan, err := a.loadPlan(req.RunID)
	if err != nil {
		return a.fail(enc, err.Error())
	}
	if a.version != plan.ToolVersion {
		return a.fail(enc, fmt.Sprintf("agent version %q does not match plan tool version %q", a.version, plan.ToolVersion))
	}
	if plan.Qualification.Report != nil {
		if err := a.validateQualificationReport(filepath.Join(a.runDir(req.RunID), "qualification", "ansible-report.json"), plan); err != nil {
			return a.fail(enc, err.Error())
		}
	}
	if err := a.qualify(req.RunID, req.PlanSHA256, plan); err != nil {
		_ = enc.Emit(protocol.Event{Kind: protocol.KindCheck, Name: "qualification", Status: protocol.StatusFailed, Message: sanitize(err.Error())})
		return a.fail(enc, err.Error())
	}
	if err := enc.Emit(protocol.Event{Kind: protocol.KindCheck, Name: "qualification", Status: protocol.StatusPassed}); err != nil {
		return err
	}
	receipt, err := a.writeRecord(req.RunID, req.PlanSHA256, "prepared", "plan staged")
	if err != nil {
		return a.fail(enc, err.Error())
	}
	return a.complete(enc, receipt)
}

func (a *agent) qualify(runID, planSHA string, plan runplan.Plan) error {
	data, err := os.ReadFile(filepath.Join(a.runDir(runID), "qualification", "host-facts.json"))
	if err != nil {
		return fmt.Errorf("qualification evidence: %w", err)
	}
	var facts map[string]string
	if err := json.Unmarshal(data, &facts); err != nil {
		return fmt.Errorf("qualification evidence: %w", err)
	}
	q := plan.Qualification
	if q.RequireCgroupV2 && facts["cgroupVersion"] != "v2" {
		return errors.New("qualification requires cgroup v2")
	}
	for _, key := range q.RequiredFacts {
		if strings.TrimSpace(facts[key]) == "" {
			return fmt.Errorf("qualification required fact %q is missing", key)
		}
	}
	if q.MinPhysicalCores > 0 {
		cores, err := strconv.Atoi(facts["physicalCores"])
		if err != nil || cores < q.MinPhysicalCores {
			return fmt.Errorf("qualification physical cores %q is below %d", facts["physicalCores"], q.MinPhysicalCores)
		}
	}
	if q.MaxLoad1 > 0 {
		fields := strings.Fields(facts["load1"])
		if len(fields) == 0 {
			return errors.New("qualification load1 is missing")
		}
		load, err := strconv.ParseFloat(fields[0], 64)
		if err != nil || load > q.MaxLoad1 {
			return fmt.Errorf("qualification load1 %q exceeds %.2f", facts["load1"], q.MaxLoad1)
		}
	}
	_, err = a.writeRecord(runID, planSHA, "qualified", "host facts satisfy sealed qualification")
	return err
}

func (a *agent) copyQualificationReport(runID string, plan runplan.Plan) error {
	report := plan.Qualification.Report
	if report == nil {
		return nil
	}
	if err := a.validateQualificationReport(report.Path, plan); err != nil {
		return err
	}
	data, err := os.ReadFile(report.Path)
	if err != nil {
		return err
	}
	return artifact.WriteFileAtomic(filepath.Join(a.runDir(runID), "qualification", "ansible-report.json"), data, 0o644)
}

func (a *agent) validateQualificationReport(path string, plan runplan.Plan) error {
	return qualification.Validate(path, plan)
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
	oci := a.benchmarkImage != ""
	var containerNames []string
	if oci {
		containerNames = dockerContainerNames(plan, block)
		// Production-compat leaves a root-owned sandbox cgroup that an
		// unprivileged manager cannot remove. The OCI cleanup helper carries
		// the same run-scoped labels as the workers so residual cleanup
		// removes a helper left behind by an interrupted run.
		containerNames = append(containerNames, dockerCleanupContainerNames(plan, block, specs)...)
		if err := a.requireNoContainers(ctx, plan.RunID, block.ID, containerNames); err != nil {
			return a.fail(enc, err.Error())
		}
	}

	mgr := &containment.Manager{Mount: a.mount(), FS: a.fs(), Proc: a.proc()}
	coord := a.newBlockCoordinator(mgr, plan, block, a.benchmarkImage, enc, oci)
	timeout := a.runTimeout
	if timeout <= 0 && plan.Qualification.MaxRunSeconds > 0 {
		timeout = time.Duration(plan.Qualification.MaxRunSeconds) * time.Second
	}
	runDir := a.runDir(req.RunID)
	blockRequest := experiment.BlockRequest{
		RunID: req.RunID, BlockID: block.ID, Parent: a.cgroupParent, Workers: specs,
		Timeout: timeout, MonitorInterval: a.monitorInterval, RunDir: runDir,
		SamplesName:          filepath.ToSlash(filepath.Join("samples", block.ID+".ndjson")),
		ReceiptName:          filepath.ToSlash(filepath.Join("receipts", block.ID+".json")),
		WorkloadBinarySHA256: plan.WorkloadBinary.SHA256,
	}
	var result experiment.Result
	telemetryPath := filepath.Join(runDir, "telemetry", "thermal-"+block.ID+".ndjson")
	err = telemetry.Capture(telemetryPath, block.ID, a.telemetrySampler(), func() error {
		var runErr error
		result, runErr = coord.Run(ctx, blockRequest)
		return runErr
	})
	if oci {
		err = errors.Join(err, a.cleanupContainers(context.WithoutCancel(ctx), plan.RunID, block.ID, containerNames))
	}
	if err != nil {
		// A missing or invalid delegation is a containment boundary the host
		// does not provide, not a measurement failure. Surface it as
		// unsupported so the controller can refuse the run.
		if errors.Is(err, containment.ErrUnsupported) || errors.Is(err, containment.ErrNotDelegated) {
			return a.unsupported(enc, err.Error())
		}
		return a.fail(enc, err.Error())
	}

	relSamples := filepath.ToSlash(filepath.Join("samples", block.ID+".ndjson"))
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
	relTelemetry := filepath.ToSlash(filepath.Join("telemetry", "thermal-"+block.ID+".ndjson"))
	telemetrySHA, _, herr := artifact.HashFile(telemetryPath)
	if herr != nil {
		return a.fail(enc, "hash telemetry: "+herr.Error())
	}
	if err := enc.Emit(protocol.Event{Kind: protocol.KindArtifact, Path: relTelemetry, SHA256: telemetrySHA}); err != nil {
		return err
	}
	receipt, err := a.writeRecord(req.RunID, req.PlanSHA256, "run-block-"+block.ID, "block "+block.ID+" samples="+strconv.Itoa(result.Receipt.SampleCount))
	if err != nil {
		return a.fail(enc, err.Error())
	}
	return a.complete(enc, receipt)
}

// newBlockCoordinator wires the coordinator for one block. In OCI mode it
// injects the sandbox-root cleanup callback, whose root-owned production
// sandbox cgroups an unprivileged manager cannot remove directly. Host mode
// keeps the coordinator's built-in RemoveEmptySandboxRoot path.
func (a *agent) newBlockCoordinator(mgr *containment.Manager, plan runplan.Plan, block runplan.Block, image string, enc *protocol.EventEncoder, oci bool) *experiment.Coordinator {
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
	if oci {
		coord.CleanupSandboxRoot = func(ctx context.Context, root string) error {
			return a.cleanupSandboxRoot(ctx, mgr, plan, block, image, root)
		}
	}
	return coord
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
	data, err = os.ReadFile(filepath.Join(a.runDir(runID), "qualified.json"))
	if err != nil {
		return fmt.Errorf("run is not qualified for this plan: %w", err)
	}
	if err := json.Unmarshal(data, &rec); err != nil || rec.Action != "qualified" || rec.PlanSHA256 != planSHA {
		return errors.New("qualified record does not match the request plan digest")
	}
	return nil
}

func (a *agent) loadPlan(runID string) (runplan.Plan, error) {
	path := filepath.Join(a.runDir(runID), "plan.json")
	if _, err := os.Stat(path); err != nil {
		path = a.planPath(runID)
	}
	return a.loadPlanPath(path)
}

func (a *agent) loadPlanPath(path string) (runplan.Plan, error) {
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
	benchmarkImage, oci, err := a.planBenchmarkImage(plan)
	if err != nil {
		return nil, err
	}
	workloadPath, err := a.verifyWorkloadBinary(plan)
	if err != nil {
		return nil, err
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
	if len(plan.Fixtures) != 1 {
		return nil, errors.New("direct suite requires exactly one fixture pair")
	}
	fixture := plan.Fixtures[0]
	if err := verifyFixture(fixture); err != nil {
		return nil, err
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
		// from colliding. The coordinator derives the same flat, run-scoped
		// subtree name so the delegated boundary matches.
		subtree, err := experiment.SubtreeName(plan.RunID, block.ID, id)
		if err != nil {
			return nil, err
		}
		expectedParent := filepath.Join(a.cgroupParent, subtree)
		containerID := workerContainerID(a.containerID, plan, block, id)
		acceptedUncontained := ""
		if plan.ContainmentMode == runplan.ContainmentProductionCompat {
			acceptedUncontained = filepath.Join("/", "sandbox-"+containerID)
		}
		judgerPath := a.judgerPath
		if oci {
			judgerPath = ociJudgerPath
		}
		args, err := a.workerArguments(id, block, plan, cpusets[i], output, fixture, workloadPath, expectedParent, containerID, judgerSHA, judgerPath, workerDone)
		if err != nil {
			return nil, err
		}
		n, err := containment.CountCPUs(cpusets[i])
		if err != nil {
			return nil, err
		}
		command := bin
		if oci {
			command = "docker"
			args = dockerRunArguments(plan, block, id, benchmarkImage, cpusets[i], a.cpusetMems, a.mount(), fixture, workloadPath, filepath.Dir(output), args)
		}
		specs = append(specs, experiment.WorkerSpec{
			ID:                        id,
			CPUList:                   cpusets[i],
			Mems:                      a.cpusetMems,
			CPUMax:                    fmt.Sprintf("%d 100000", n*100000),
			Command:                   command,
			Args:                      args,
			Fixture:                   fixture.Name,
			ExpectedSamples:           block.Repetitions,
			AcceptedUncontainedCgroup: acceptedUncontained,
			OutputPath:                output,
		})
	}
	return specs, nil
}

// planBenchmarkImage selects OCI mode only when the controller-forwarded local
// image ID exactly matches the immutable image sealed into the plan.
func (a *agent) planBenchmarkImage(plan runplan.Plan) (string, bool, error) {
	img, sealed := plan.Images["judger-bench"]
	got := strings.TrimSpace(a.benchmarkImage)
	if !sealed {
		if got != "" {
			return "", false, errors.New("--benchmark-image is not sealed in the plan")
		}
		if plan.ContainmentMode == runplan.ContainmentProductionCompat {
			return "", false, errors.New("production-compat requires a sealed benchmark OCI image")
		}
		return "", false, nil
	}
	if !runplan.ValidImageDigest(img.Reference) || img.Digest != img.Reference {
		return "", false, errors.New("sealed judger-bench image is not an immutable local image ID")
	}
	if got != img.Reference {
		return "", false, fmt.Errorf("--benchmark-image %q does not match sealed judger-bench image %q", got, img.Reference)
	}
	if a.judgerPath != "" {
		return "", false, errors.New("OCI mode cannot use a host Judger path")
	}
	return got, true, nil
}

func (a *agent) verifyWorkloadBinary(plan runplan.Plan) (string, error) {
	path := plan.WorkloadBinary.Path
	if got := strings.TrimSpace(a.benchBinary); got != "" && got != path {
		return "", fmt.Errorf("--bench-binary %q does not match sealed workload path %q", got, path)
	}
	sum, _, err := artifact.HashFile(path)
	if err != nil {
		return "", fmt.Errorf("workload binary: %w", err)
	}
	if sum != plan.WorkloadBinary.SHA256 {
		return "", fmt.Errorf("workload binary sha256 %s does not match sealed %s", sum, plan.WorkloadBinary.SHA256)
	}
	return path, nil
}

func verifyFixture(f runplan.Fixture) error {
	for label, item := range map[string]struct{ path, digest string }{
		"input": {f.Path, f.SHA256}, "expected output": {f.ExpectedOutputPath, f.ExpectedOutputSHA256},
	} {
		sum, _, err := artifact.HashFile(item.path)
		if err != nil {
			return fmt.Errorf("fixture %q %s: %w", f.Name, label, err)
		}
		if sum != item.digest {
			return fmt.Errorf("fixture %q %s sha256 %s does not match sealed %s", f.Name, label, sum, item.digest)
		}
	}
	return nil
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
	return sanitizeContainerID(base, worker)
}

func sanitizeContainerID(base, worker string) string {
	clean := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
				b.WriteRune(r)
			default:
				b.WriteByte('-')
			}
		}
		return b.String()
	}
	base = clean(base)
	worker = clean(worker)
	if base == "" {
		base = "bench"
	}
	if worker == "" {
		worker = "worker"
	}
	const max = 128
	suffix := "-" + worker
	if len(suffix) >= max {
		sum := sha256.Sum256([]byte(worker))
		suffix = fmt.Sprintf("-worker-%x", sum[:8])
	}
	if len(base)+len(suffix) > max {
		base = base[:max-len(suffix)]
	}
	return base + suffix
}

func (a *agent) workerArguments(worker string, block runplan.Block, plan runplan.Plan, cpuset, output string, fixture runplan.Fixture, workloadPath, expectedParent, containerID, judgerSHA, judgerPath string, workerTimeout time.Duration) ([]string, error) {
	vars := map[string]string{
		"run":                    plan.RunID,
		"block":                  block.ID,
		"worker":                 worker,
		"fixture":                fixture.Name,
		"input":                  fixture.Path,
		"expected-output":        fixture.ExpectedOutputPath,
		"output":                 output,
		"iterations":             strconv.Itoa(block.Repetitions),
		"cpuset":                 cpuset,
		"bench-binary":           workloadPath,
		"run-dir":                a.runDir(plan.RunID),
		"runtime-dir":            a.rtDir(plan.RunID),
		"timeout":                workerTimeout.String(),
		"judger":                 judgerPath,
		"judger-sha256":          judgerSHA,
		"container-id":           containerID,
		"expected-cgroup-parent": expectedParent,
	}
	templates := a.workerArgs
	if len(templates) == 0 {
		if workloadPath == "" {
			return nil, errors.New("run-block requires --bench-binary when no --worker-arg template is given")
		}
		templates = []string{
			"--mode", "execute",
			"--run-id", "{run}",
			"--block-id", "{block}",
			"--worker", "{worker}",
			"--fixture", "{fixture}",
			"--input", "{input}",
			"--expected-output", "{expected-output}",
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
		if judgerPath != "" {
			templates = append(templates, "--judger", "{judger}")
		}
	}
	if plan.ContainmentMode == runplan.ContainmentProductionCompat && !containsArgument(templates, "--production-compat") {
		templates = append(templates, "--production-compat")
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

func dockerRunArguments(plan runplan.Plan, block runplan.Block, worker, image, cpus, mems, cgroupMount string, fixture runplan.Fixture, workloadPath, outputDir string, workerArgs []string) []string {
	args := []string{
		"run", "--rm",
		"--name", dockerContainerName(plan, block, worker),
		"--label", "iris-bench.run=" + plan.RunID,
		"--label", "iris-bench.block=" + block.ID,
		"--privileged", "--cgroupns=host",
		"--cpuset-cpus", cpus,
		"--cpuset-mems", mems,
		"--mount", dockerBind(cgroupMount, false),
	}
	seen := map[string]bool{cgroupMount: true}
	for _, path := range []string{workloadPath, fixture.Path, fixture.ExpectedOutputPath} {
		if !seen[path] {
			args = append(args, "--mount", dockerBind(path, true))
			seen[path] = true
		}
	}
	if !seen[outputDir] {
		args = append(args, "--mount", dockerBind(outputDir, false))
	}
	args = append(args, image)
	return append(args, workerArgs...)
}

func dockerBind(path string, readOnly bool) string {
	s := "type=bind,src=" + path + ",dst=" + path
	if readOnly {
		s += ",readonly"
	}
	return s
}

// cleanupSandboxRoot removes one leftover production-compat sandbox root and
// the empty "box-<safe-id>" children live alpha.4 leaves under it. It first
// proves through the read-only containment validator that every cgroup is empty
// of member processes, has no grandchildren, and has no unexpected directory
// names, obtaining the canonical full filesystem paths in child-first,
// root-last removal order. It then runs only the sealed OCI image with a single
// argv-only privileged helper container whose rmdir entrypoint receives every
// path at once; rmdir accepts multiple operands, no shell is involved, and the
// children are removed before the root. The helper's run-scoped, validated name
// and labels let the coordinator's residual container cleanup remove it if the
// run is interrupted. The caller verifies root absence afterwards.
func (a *agent) cleanupSandboxRoot(ctx context.Context, mgr *containment.Manager, plan runplan.Plan, block runplan.Block, image, root string) error {
	paths, err := mgr.ValidateSandboxRemovalPaths(root)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return nil
	}
	if !runplan.ValidImageDigest(image) {
		return fmt.Errorf("oci sandbox root cleanup %s: image %q is not a sealed immutable image id", root, image)
	}
	args := []string{
		"run", "--rm",
		"--name", dockerCleanupContainerName(plan, block, root),
		"--label", "iris-bench.run=" + plan.RunID,
		"--label", "iris-bench.block=" + block.ID,
		"--privileged", "--cgroupns=host",
		"--entrypoint", "/bin/rmdir",
		"--mount", dockerBind(a.mount(), false),
		image,
	}
	args = append(args, paths...)
	if _, err := a.docker().Output(ctx, "docker", args...); err != nil {
		return fmt.Errorf("oci sandbox root cleanup %s: %w", root, err)
	}
	return nil
}

func dockerContainerNames(plan runplan.Plan, block runplan.Block) []string {
	names := make([]string, block.Workers)
	for i := range names {
		names[i] = dockerContainerName(plan, block, fmt.Sprintf("worker-%02d", i+1))
	}
	return names
}

func dockerContainerName(plan runplan.Plan, block runplan.Block, worker string) string {
	return dockerScopedName(plan, block, worker)
}

// dockerCleanupContainerName is the run-scoped, validated name of the helper
// container that removes one production-compat sandbox root. Deriving it from
// the root's sandbox-<id> basename keeps helper names distinct per worker while
// staying inside the Docker name grammar.
func dockerCleanupContainerName(plan runplan.Plan, block runplan.Block, root string) string {
	return dockerScopedName(plan, block, "cleanup-"+filepath.Base(root))
}

// dockerCleanupContainerNames returns the helper names for every accepted
// production-compat root, de-duplicated. They are added to the expected worker
// names so residual cleanup can remove an interrupted helper.
func dockerCleanupContainerNames(plan runplan.Plan, block runplan.Block, specs []experiment.WorkerSpec) []string {
	var names []string
	seen := map[string]bool{}
	for _, spec := range specs {
		if spec.AcceptedUncontainedCgroup == "" {
			continue
		}
		name := dockerCleanupContainerName(plan, block, spec.AcceptedUncontainedCgroup)
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

func dockerScopedName(plan runplan.Plan, block runplan.Block, leaf string) string {
	name := "iris-bench-" + plan.RunID + "-" + block.ID + "-" + leaf
	if len(name) > 128 {
		sum := sha256.Sum256([]byte(name))
		run := plan.RunID
		if len(run) > 80 {
			run = run[:80]
		}
		name = "iris-bench-" + run + fmt.Sprintf("-%x", sum[:8])
	}
	if !dockerNamePattern.MatchString(name) {
		panic("invalid run-scoped Docker container name")
	}
	return name
}

func (a *agent) listedContainers(ctx context.Context, runID, blockID string) ([]string, error) {
	out, err := a.docker().Output(ctx, "docker", "container", "ls", "-a",
		"--filter", "label=iris-bench.run="+runID,
		"--filter", "label=iris-bench.block="+blockID,
		"--format", "{{.Names}}")
	if err != nil {
		return nil, fmt.Errorf("list OCI workers: %w", err)
	}
	return strings.Fields(string(out)), nil
}

func (a *agent) requireNoContainers(ctx context.Context, runID, blockID string, expected []string) error {
	names, err := a.listedContainers(ctx, runID, blockID)
	if err != nil {
		return err
	}
	if len(names) != 0 {
		return fmt.Errorf("run-scoped OCI workers already exist: %s", strings.Join(names, ","))
	}
	for _, name := range expected {
		if !dockerNamePattern.MatchString(name) || !strings.HasPrefix(name, "iris-bench-") {
			return fmt.Errorf("invalid run-scoped OCI worker name %q", name)
		}
	}
	return nil
}

func (a *agent) cleanupContainers(ctx context.Context, runID, blockID string, expected []string) error {
	names, err := a.listedContainers(ctx, runID, blockID)
	if err != nil {
		return err
	}
	allowed := make(map[string]bool, len(expected))
	for _, name := range expected {
		allowed[name] = true
	}
	for _, name := range names {
		if !allowed[name] {
			return fmt.Errorf("unexpected container %q has run-scoped OCI labels", name)
		}
	}
	if len(names) > 0 {
		args := append([]string{"container", "rm", "-f"}, names...)
		if _, err := a.docker().Output(ctx, "docker", args...); err != nil {
			return fmt.Errorf("remove OCI workers: %w", err)
		}
	}
	remaining, err := a.listedContainers(ctx, runID, blockID)
	if err != nil {
		return err
	}
	if len(remaining) != 0 {
		return fmt.Errorf("OCI workers remain after cleanup: %s", strings.Join(remaining, ","))
	}
	return nil
}

func containsArgument(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
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
	if err := a.verifyRunReady(req.RunID, req.PlanSHA256); err != nil {
		return a.fail(enc, err.Error())
	}
	plan, err := a.loadPlan(req.RunID)
	if err != nil {
		return a.fail(enc, err.Error())
	}
	if plan.Qualification.Report != nil {
		path := filepath.Join(a.runDir(req.RunID), "qualification", "ansible-report.json")
		if err := a.validateQualificationReport(path, plan); err != nil {
			_ = enc.Emit(protocol.Event{Kind: protocol.KindCheck, Name: "ansible-qualification", Status: protocol.StatusFailed, Message: sanitize(err.Error())})
			return a.fail(enc, err.Error())
		}
		if err := enc.Emit(protocol.Event{Kind: protocol.KindCheck, Name: "ansible-qualification", Status: protocol.StatusPassed}); err != nil {
			return err
		}
	}
	for _, block := range plan.Blocks {
		if err := a.validateBlock(req.RunID, block, plan.WorkloadBinary.SHA256); err != nil {
			_ = enc.Emit(protocol.Event{Kind: protocol.KindCheck, Name: "block-" + block.ID, Status: protocol.StatusFailed, Message: sanitize(err.Error())})
			return a.fail(enc, err.Error())
		}
		if err := enc.Emit(protocol.Event{Kind: protocol.KindCheck, Name: "block-" + block.ID, Status: protocol.StatusPassed}); err != nil {
			return err
		}
	}
	receipt, err := a.writeRecord(req.RunID, req.PlanSHA256, "validated", "qualification, fixtures, blocks, samples, and results verified")
	if err != nil {
		return a.fail(enc, err.Error())
	}
	return a.complete(enc, receipt)
}

func (a *agent) validateBlock(runID string, block runplan.Block, workloadSHA string) error {
	data, err := os.ReadFile(filepath.Join(a.runDir(runID), "receipts", block.ID+".json"))
	if err != nil {
		return fmt.Errorf("block %s receipt: %w", block.ID, err)
	}
	var receipt experiment.Receipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return fmt.Errorf("block %s receipt: %w", block.ID, err)
	}
	want := block.Workers * block.Repetitions
	if receipt.RunID != runID || receipt.BlockID != block.ID || receipt.Status != experiment.StatusPassed || receipt.SampleCount != want {
		return fmt.Errorf("block %s receipt is not a passing %d-sample result", block.ID, want)
	}
	if receipt.WorkloadBinarySHA256 != workloadSHA {
		return fmt.Errorf("block %s receipt workload digest does not match the plan", block.ID)
	}
	samples := filepath.Join(a.runDir(runID), "samples", block.ID+".ndjson")
	sum, _, err := artifact.HashFile(samples)
	if err != nil {
		return fmt.Errorf("block %s samples: %w", block.ID, err)
	}
	if sum != receipt.SamplesSHA256 {
		return fmt.Errorf("block %s samples do not match receipt", block.ID)
	}
	telemetryPath := filepath.Join(a.runDir(runID), "telemetry", "thermal-"+block.ID+".ndjson")
	if _, _, err := telemetry.ValidateFile(telemetryPath, block.ID); err != nil {
		return fmt.Errorf("block %s telemetry: %w", block.ID, err)
	}
	return nil
}

func (a *agent) bundle(req protocol.Request, enc *protocol.EventEncoder) error {
	if err := enc.Emit(protocol.Event{Kind: protocol.KindPhase, Phase: "bundle", Status: protocol.StatusStarted}); err != nil {
		return err
	}
	for _, name := range []string{"validated.json", "cleaned.json"} {
		if _, err := os.Stat(filepath.Join(a.runDir(req.RunID), name)); err != nil {
			return a.fail(enc, "bundle requires "+name+": "+err.Error())
		}
	}
	path := filepath.Join(a.runDir(req.RunID), "bundle-inventory.json")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return a.fail(enc, "remove old inventory: "+err.Error())
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
