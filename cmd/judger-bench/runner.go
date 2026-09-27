package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/skkuding/iris-load-test-toolset/internal/artifact"
	"github.com/skkuding/iris-load-test-toolset/internal/containment"
)

type modeType string

const (
	modeCompile modeType = "compile"
	modeExecute modeType = "execute"
)

// defaultBinaryName matches the production C/C++ ExeName in
// apps/iris/src/service/sandbox/judger/lang_config.go.
const defaultBinaryName = "main"

// unifiedCgroupRoot is the conventional cgroup v2 mount root. It is never a
// valid expected sandbox parent because Judger would create root-level
// /sandbox-* cgroups outside the delegated worker subtree.
const unifiedCgroupRoot = "/sys/fs/cgroup"

// Judger alpha.4 defaults. These mirror the effective values produced by
// ToRunExecArgs and exec.go in the production Iris Judger sandbox. Production
// runs the sandbox privileged with uid/gid 0; any other identity starts a new
// comparison population.
const (
	defaultCompiler             = "g++"
	defaultSeccompRule          = "c_cpp"
	defaultUID                  = 0
	defaultGID                  = 0
	defaultMaxCPUTimeMs         = 2000
	defaultMaxRealTimeMs        = 6000
	defaultMaxMemoryBytes       = 256 * 1024 * 1024
	defaultMaxStackBytes        = 128 * 1024 * 1024
	defaultMaxOutputBytes       = 10 * 1024 * 1024
	defaultOuterTimeout         = 5 * time.Second
	defaultCompileRealTimeLimit = 20 * time.Second
)

// productionCPPCompileArgs is the exact cppConfig.CompileArgs string from
// apps/iris/src/service/sandbox/judger/lang_config.go split on spaces.
var productionCPPCompileArgs = []string{
	"-DONLINE_JUDGE",
	"-O2",
	"-Wall",
	"-std=c++14",
	"{srcPath}",
	"-lm",
	"-o",
	"{exePath}",
}

// errCgroupEscape reports that Judger executed user code outside the delegated
// cgroup parent. It is a failed run, never a warning.
var errCgroupEscape = errors.New("cgroup containment violation")

// options configures one direct benchmark invocation.
type options struct {
	Mode         modeType
	RunID        string
	BlockID      string
	Worker       string
	Fixture      string
	Source       string
	Binary       string
	Input        string
	WorkDir      string
	Iterations   int
	Compiler     string
	CompilerArgs []string
	Timeout      time.Duration

	// External alpha.4 Judger invocation.
	JudgerPath           string
	JudgerSHA256         string
	ContainerID          string
	ExpectedCgroupParent string
	ProductionCompat     bool
	ExpectedOutput       string
	UID                  int
	GID                  int
	SeccompRule          string
	MaxCPUTimeMs         int
	MaxRealTimeMs        int
	MaxMemoryBytes       int64
	MaxStackBytes        int64
	MaxOutputBytes       int64
	MaxProcessNumber     int
	Env                  []string
	RunArgs              []string
}

// sample is one NDJSON measurement record. It carries the raw Judger fields,
// the controller's monotonic elapsed time, and the containment verdict.
type sample struct {
	RunID               string  `json:"runId"`
	BlockID             string  `json:"blockId"`
	Worker              string  `json:"worker"`
	Fixture             string  `json:"fixture"`
	Iteration           int     `json:"iteration"`
	Phase               string  `json:"phase"`
	Status              string  `json:"status"`
	JudgerPID           int     `json:"judgerPid,omitempty"`
	CPUTimeMs           int     `json:"cpuTimeMs"`
	RealTimeMs          int     `json:"realTimeMs"`
	ControllerElapsedMs float64 `json:"controllerElapsedMs"`
	// StartedAtNs and EndedAtNs are host-wide wall-clock timestamps recorded
	// immediately before and after each execute-mode Judger invocation. They
	// let the coordinator and analyzers isolate the all-workers-active steady
	// window. Compile samples leave them zero.
	StartedAtNs     int64  `json:"startedAtNs"`
	EndedAtNs       int64  `json:"endedAtNs"`
	MemoryBytes     int64  `json:"memoryBytes"`
	Signal          int    `json:"signal"`
	ExitCode        int    `json:"exitCode"`
	ErrorCode       int    `json:"errorCode"`
	ResultCode      int    `json:"resultCode"`
	CgroupPath      string `json:"cgroupPath,omitempty"`
	CgroupContained bool   `json:"cgroupContained"`
	JudgerSHA256    string `json:"judgerSha256,omitempty"`
	OutputSHA256    string `json:"outputSha256,omitempty"`
	OutputBytes     int64  `json:"outputBytes,omitempty"`
	OutputMatches   bool   `json:"outputMatches"`
	ContainmentMode string `json:"containmentMode"`
	Comparable      bool   `json:"comparable"`
	Error           string `json:"error,omitempty"`
}

// judgerResult is the authoritative alpha.4 JSON document. Every field the task
// requires is decoded explicitly; unknown fields are ignored.
type judgerResult struct {
	CPUTime    int    `json:"cpu_time"`
	RealTime   int    `json:"real_time"`
	Memory     int64  `json:"memory"`
	Signal     int    `json:"signal"`
	ExitCode   int    `json:"exit_code"`
	ErrorCode  int    `json:"error"`
	ResultCode int    `json:"result"`
	CgroupPath string `json:"cgroup_path"`
}

type procResult struct {
	real     time.Duration
	exitCode int
	signal   int
	stdout   []byte
	stderr   []byte
	timedOut bool
	startErr error
}

type judgerRun struct {
	res      procResult
	parsed   judgerResult
	parseErr error
	pid      int
	ok       bool
}

// runBench performs the requested phase and returns all samples. Execute may
// return samples together with a containment error: evidence is never dropped
// when a sample is rejected.
func runBench(ctx context.Context, opt options) ([]sample, error) {
	switch opt.Mode {
	case modeCompile:
		return runCompile(ctx, opt)
	case modeExecute:
		return runExecute(ctx, opt)
	default:
		return nil, fmt.Errorf("unsupported mode %q (want compile or execute)", opt.Mode)
	}
}

// runCompile runs the production compiler directly. Compilation is not a
// sandbox operation in Iris, so it is preserved as a separate, unsandboxed
// operation with the production cpp flags and a real-time bound.
func runCompile(ctx context.Context, opt options) ([]sample, error) {
	if opt.Source == "" {
		return nil, errors.New("compile mode requires --source")
	}
	if _, err := os.Stat(opt.Source); err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	workDir, err := ensureWorkDir(opt)
	if err != nil {
		return nil, err
	}
	out := opt.Binary
	if out == "" {
		out = filepath.Join(workDir, defaultBinaryName)
	}
	compiler := opt.Compiler
	if compiler == "" {
		compiler = defaultCompiler
	}
	args := opt.CompilerArgs
	if len(args) == 0 {
		args = productionCompileArgs(opt.Source, out)
	}
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = defaultCompileRealTimeLimit
	}

	// Production compileExec bounds only real time and passes PATH alone.
	res := runProcess(ctx, compiler, args, "", []string{"PATH=" + os.Getenv("PATH")}, timeout)
	s := opt.newCompileSample(res)
	if res.startErr == nil && res.exitCode == 0 && !res.timedOut {
		if sum, size, herr := artifact.HashFile(out); herr == nil {
			s.OutputSHA256 = sum
			s.OutputBytes = size
		}
	}
	return []sample{s}, nil
}

// runExecute invokes the external alpha.4 Judger once per iteration with fixed
// limits, identity, seccomp rule, environment, and I/O paths. It never moves or
// patches a running PID: containment is judged from the cgroup path Judger
// reports before a sample is accepted.
func runExecute(ctx context.Context, opt options) ([]sample, error) {
	workDir, err := ensureWorkDir(opt)
	if err != nil {
		return nil, err
	}
	bin := opt.Binary
	if bin == "" {
		bin = filepath.Join(workDir, defaultBinaryName)
	}
	if _, err := os.Stat(bin); err != nil {
		return nil, fmt.Errorf("binary: %w", err)
	}
	if opt.Iterations < 1 {
		return nil, errors.New("iterations must be positive")
	}
	if opt.JudgerPath == "" {
		return nil, errors.New("execute mode requires --judger")
	}
	if opt.ContainerID == "" {
		return nil, errors.New("execute mode requires --container-id (Judger derives sandbox-<id> from it)")
	}
	if opt.ExpectedOutput == "" {
		return nil, errors.New("execute mode requires --expected-output")
	}
	expectedOutputSum, _, err := artifact.HashFile(opt.ExpectedOutput)
	if err != nil {
		return nil, fmt.Errorf("expected output: %w", err)
	}
	parent := opt.ExpectedCgroupParent
	if parent == "" {
		return nil, errors.New("execute mode requires --expected-cgroup-parent")
	}
	if err := validateCgroupBeneath(parent, parent); err != nil {
		return nil, fmt.Errorf("expected cgroup parent: %w", err)
	}

	judgerSum := ""
	if opt.JudgerSHA256 != "" {
		sum, _, err := artifact.HashFile(opt.JudgerPath)
		if err != nil {
			return nil, fmt.Errorf("hash judger binary: %w", err)
		}
		if !strings.EqualFold(sum, opt.JudgerSHA256) {
			return nil, fmt.Errorf("judger binary sha256 %s does not match pinned %s", sum, opt.JudgerSHA256)
		}
		judgerSum = sum
	}

	uid, gid := opt.UID, opt.GID
	if uid < 0 {
		uid = defaultUID
	}
	if gid < 0 {
		gid = defaultGID
	}

	samples := make([]sample, 0, opt.Iterations)
	var runErr error
	for i := 0; i < opt.Iterations; i++ {
		if err := ctx.Err(); err != nil {
			return samples, err
		}

		inputPath := opt.Input
		if inputPath == "" {
			inputPath = os.DevNull
		}
		outPath := filepath.Join(workDir, fmt.Sprintf("iter-%06d.out", i))
		errPath := filepath.Join(workDir, fmt.Sprintf("iter-%06d.error", i))
		logPath := filepath.Join(workDir, fmt.Sprintf("iter-%06d.judger.log", i))

		args := buildJudgerArgs(opt, bin, inputPath, outPath, errPath, logPath, uid, gid)
		startedAtNs := time.Now().UnixNano()
		jr := invokeJudger(ctx, opt, args)
		endedAtNs := time.Now().UnixNano()

		s := opt.newExecuteSample(i, jr)
		s.StartedAtNs = startedAtNs
		s.EndedAtNs = endedAtNs
		s.JudgerSHA256 = judgerSum
		if jr.ok && jr.res.exitCode == 0 && !jr.res.timedOut {
			if sum, size, herr := artifact.HashFile(outPath); herr == nil {
				s.OutputSHA256 = sum
				s.OutputBytes = size
				s.OutputMatches = sum == expectedOutputSum
				if !s.OutputMatches && s.Status == "success" {
					s.Status = "wrong_answer"
					s.Error = "output sha256 does not match the sealed expected output"
				}
			} else {
				s.Status = "failed"
				s.Error = "read Judger output: " + herr.Error()
			}
		}
		if jr.ok {
			var verr error
			if opt.ProductionCompat {
				verr = validateProductionCgroup(opt.ContainerID, jr.parsed.CgroupPath)
				s.ContainmentMode = "production-compat"
				s.Comparable = false
			} else {
				verr = validateCgroupBeneath(parent, jr.parsed.CgroupPath)
				s.ContainmentMode = "isolated"
				s.Comparable = true
			}
			if verr != nil {
				s.CgroupContained = false
				s.Status = "cgroup_escape"
				s.Error = verr.Error()
				if runErr == nil {
					runErr = fmt.Errorf("iteration %d: %w", i, verr)
				}
			} else if !opt.ProductionCompat {
				s.CgroupContained = true
			}
		}
		if s.Status != "success" && runErr == nil {
			runErr = fmt.Errorf("iteration %d: sample status %s", i, s.Status)
		}
		samples = append(samples, s)
	}
	return samples, runErr
}

// validateProductionCgroup accepts the stock alpha.4 sandbox that Judger
// reports from its own cgroup view. A live alpha.4 reports the full filesystem
// path "/sys/fs/cgroup/sandbox-<id>/box-*" while the monitor compares against
// the mount-relative run-scoped root "/sandbox-<id>". Both forms are normalized
// and accepted as the exact run-scoped root or one of its descendants; sibling
// prefix confusion such as "/sandbox-<id>-evil" is still rejected. Isolated
// validation is separate and unchanged.
func validateProductionCgroup(containerID, reported string) error {
	want := "/sandbox-" + containerID
	if !safeContainerID(containerID) {
		return fmt.Errorf("%w: production compatibility container id %q is not a safe path component", errCgroupEscape, containerID)
	}
	if !containment.WithinMountRelative(containment.DefaultMount, want, reported) {
		return fmt.Errorf("%w: production compatibility expected root sandbox %q, got %q", errCgroupEscape, want, reported)
	}
	return nil
}

// safeContainerID reports whether id is a single safe path component matching
// the IDs produced by the agent's container-id sanitizer.
func safeContainerID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func (o options) newCompileSample(res procResult) sample {
	s := sample{
		RunID:               o.RunID,
		BlockID:             o.BlockID,
		Worker:              o.Worker,
		Fixture:             o.Fixture,
		Phase:               string(modeCompile),
		ControllerElapsedMs: ms(res.real),
	}
	switch {
	case res.startErr != nil:
		s.Status = "failed"
		s.Error = res.startErr.Error()
	case res.timedOut:
		s.Status = "timeout"
		s.Error = "compiler exceeded the real-time bound"
	case res.exitCode != 0 || res.signal != 0:
		s.Status = "failed"
		s.Error = fmt.Sprintf("compiler exited %d signal %d: %s", res.exitCode, res.signal, firstLine(res.stderr))
	default:
		s.Status = "success"
	}
	return s
}

func (o options) newExecuteSample(iter int, jr judgerRun) sample {
	s := sample{
		RunID:               o.RunID,
		BlockID:             o.BlockID,
		Worker:              o.Worker,
		Fixture:             o.Fixture,
		Iteration:           iter,
		Phase:               string(modeExecute),
		JudgerPID:           jr.pid,
		ControllerElapsedMs: ms(jr.res.real),
	}
	switch {
	case jr.res.startErr != nil:
		s.Status = "failed"
		s.Error = jr.res.startErr.Error()
	case jr.res.timedOut:
		s.Status = "judger_timeout"
		s.Error = "controller killed Judger after the outer timeout"
	case jr.res.exitCode != 0 || jr.res.signal != 0:
		s.Status = "failed"
		s.Error = fmt.Sprintf("judger exited %d signal %d: %s", jr.res.exitCode, jr.res.signal, firstLine(jr.res.stderr))
	case jr.parseErr != nil:
		s.Status = "failed"
		s.Error = jr.parseErr.Error()
	default:
		s.CPUTimeMs = jr.parsed.CPUTime
		s.RealTimeMs = jr.parsed.RealTime
		s.MemoryBytes = jr.parsed.Memory
		s.Signal = jr.parsed.Signal
		s.ExitCode = jr.parsed.ExitCode
		s.ErrorCode = jr.parsed.ErrorCode
		s.ResultCode = jr.parsed.ResultCode
		s.CgroupPath = jr.parsed.CgroupPath
		s.Status, s.Error = judgerStatus(jr.parsed)
	}
	return s
}

// judgerStatus maps the alpha.4 error/result code pair to a sample status.
func judgerStatus(jr judgerResult) (string, string) {
	if jr.ErrorCode != 0 {
		return "judger_error", fmt.Sprintf("judger error code %d", jr.ErrorCode)
	}
	if jr.CPUTime < 0 || jr.RealTime < 0 || jr.Memory < 0 {
		return "failed", "judger reported negative resource measurements"
	}
	switch jr.ResultCode {
	case 0:
		if jr.Signal != 0 || jr.ExitCode != 0 {
			return "failed", fmt.Sprintf("judger reported success with signal %d exit %d", jr.Signal, jr.ExitCode)
		}
		return "success", ""
	case 1:
		return "cpu_time_limit_exceeded", ""
	case 2:
		return "real_time_limit_exceeded", ""
	case 3:
		return "memory_limit_exceeded", ""
	case 4:
		return "runtime_error", ""
	case 5:
		return "system_error", ""
	default:
		return "failed", fmt.Sprintf("unknown judger result code %d", jr.ResultCode)
	}
}

// productionCompileArgs expands the production cpp compile template.
func productionCompileArgs(src, out string) []string {
	args := make([]string, 0, len(productionCPPCompileArgs))
	for _, a := range productionCPPCompileArgs {
		a = strings.ReplaceAll(a, "{srcPath}", src)
		a = strings.ReplaceAll(a, "{exePath}", out)
		args = append(args, a)
	}
	return args
}

// buildJudgerArgs renders the explicit alpha.4 CLI. Flag order mirrors the
// production makeExecArgs plus the run-level PATH environment argument.
func buildJudgerArgs(opt options, exe, input, output, errPath, logPath string, uid, gid int) []string {
	maxCPU := opt.MaxCPUTimeMs
	if maxCPU <= 0 {
		maxCPU = defaultMaxCPUTimeMs
	}
	maxReal := opt.MaxRealTimeMs
	if maxReal <= 0 {
		maxReal = defaultMaxRealTimeMs
	}
	maxMem := opt.MaxMemoryBytes
	if maxMem <= 0 {
		maxMem = defaultMaxMemoryBytes
	}
	maxStack := opt.MaxStackBytes
	if maxStack <= 0 {
		maxStack = defaultMaxStackBytes
	}
	maxOut := opt.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = defaultMaxOutputBytes
	}
	rule := opt.SeccompRule
	if rule == "" {
		rule = defaultSeccompRule
	}

	args := []string{
		"--max_cpu_time=" + strconv.Itoa(maxCPU),
		"--max_real_time=" + strconv.Itoa(maxReal),
		"--max_memory=" + strconv.FormatInt(maxMem, 10),
		"--max_stack=" + strconv.FormatInt(maxStack, 10),
		"--max_output_size=" + strconv.FormatInt(maxOut, 10),
	}
	if opt.MaxProcessNumber > 0 {
		args = append(args, "--max_process_number="+strconv.Itoa(opt.MaxProcessNumber))
	}
	args = append(args,
		"--uid="+strconv.Itoa(uid),
		"--gid="+strconv.Itoa(gid),
		"--exe_path="+exe,
		"--input_path="+input,
		"--output_path="+output,
		"--error_path="+errPath,
		"--log_path="+logPath,
		"--seccomp_rule_name="+rule,
		"--memory_limit_check_only=0",
	)
	for _, a := range opt.RunArgs {
		args = append(args, "--args="+a)
	}
	env := opt.Env
	if len(env) == 0 {
		env = []string{"PATH=" + os.Getenv("PATH")}
	}
	for _, e := range env {
		args = append(args, "--env="+e)
	}
	return args
}

// invokeJudger runs the external alpha.4 shared object and parses its JSON. It
// never inspects or mutates the sandbox process afterwards.
func invokeJudger(parent context.Context, opt options, args []string) judgerRun {
	var jr judgerRun

	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = time.Duration(defaultMaxRealTime(opt))*time.Millisecond + defaultOuterTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, opt.JudgerPath, args...)
	// Judger reads CONTAINER_ID from its own environment and derives
	// /sys/fs/cgroup/sandbox-<id> from it. The sandbox child environment is
	// controlled separately by the --env arguments.
	cmd.Env = []string{
		"CONTAINER_ID=" + opt.ContainerID,
		"PATH=" + os.Getenv("PATH"),
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	if err := cmd.Start(); err != nil {
		jr.res.startErr = fmt.Errorf("start judger: %w", err)
		jr.res.real = time.Since(start)
		return jr
	}
	jr.pid = cmd.Process.Pid
	runErr := cmd.Wait()
	jr.res.real = time.Since(start)
	jr.res.stdout = stdout.Bytes()
	jr.res.stderr = stderr.Bytes()
	jr.res.timedOut = ctx.Err() == context.DeadlineExceeded
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			jr.res.exitCode = exitErr.ExitCode()
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				jr.res.signal = int(ws.Signal())
			}
		} else if !jr.res.timedOut {
			jr.res.startErr = runErr
		}
	}
	if jr.res.startErr == nil && jr.res.exitCode == 0 && !jr.res.timedOut {
		if err := json.Unmarshal(stdout.Bytes(), &jr.parsed); err != nil {
			jr.parseErr = fmt.Errorf("parse judger json: %w", err)
		} else {
			jr.ok = true
		}
	}
	return jr
}

func defaultMaxRealTime(opt options) int {
	if opt.MaxRealTimeMs > 0 {
		return opt.MaxRealTimeMs
	}
	return defaultMaxRealTimeMs
}

// validateCgroupBeneath accepts only a clean absolute path that is the expected
// delegated parent or a descendant of it. Prefix-confusion siblings such as
// /sys/fs/cgroup/sandbox-evil against /sys/fs/cgroup/sandbox are rejected.
// Containment is judged lexically before any sample is accepted; no PID is
// moved after execution starts.
func validateCgroupBeneath(parent, reported string) error {
	if parent == "" {
		return fmt.Errorf("%w: empty expected parent", errCgroupEscape)
	}
	if !filepath.IsAbs(parent) {
		return fmt.Errorf("%w: expected parent %q is not absolute", errCgroupEscape, parent)
	}
	p := filepath.Clean(parent)
	if p == "/" || p == unifiedCgroupRoot {
		return fmt.Errorf("%w: expected parent %q must not be the cgroup root", errCgroupEscape, p)
	}
	if reported == "" {
		return fmt.Errorf("%w: judger reported an empty cgroup path", errCgroupEscape)
	}
	if !filepath.IsAbs(reported) {
		return fmt.Errorf("%w: reported cgroup %q is not absolute", errCgroupEscape, reported)
	}
	r := filepath.Clean(reported)
	if r == p || strings.HasPrefix(r, p+string(os.PathSeparator)) {
		return nil
	}
	return fmt.Errorf("%w: reported cgroup %q is not beneath delegated parent %q", errCgroupEscape, reported, parent)
}

func ms(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1e6 }

// firstLine bounds an error string for the NDJSON record.
func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const max = 512
	if len(s) > max {
		s = s[:max]
	}
	return s
}

// runProcess executes one child process in its own process group, enforces a
// timeout, and returns accounting data plus captured output. It is used only
// for the unsandboxed compile operation.
func runProcess(parent context.Context, name string, args []string, inputPath string, env []string, timeout time.Duration) procResult {
	var res procResult
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
	if env != nil {
		cmd.Env = env
	}

	if inputPath != "" {
		f, err := os.Open(inputPath)
		if err != nil {
			res.startErr = err
			return res
		}
		defer f.Close()
		cmd.Stdin = f
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	runErr := cmd.Run()
	res.real = time.Since(start)
	res.stdout = stdout.Bytes()
	res.stderr = stderr.Bytes()
	res.timedOut = ctx.Err() == context.DeadlineExceeded
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			res.exitCode = exitErr.ExitCode()
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				res.signal = int(ws.Signal())
			}
		} else {
			res.startErr = runErr
		}
	}
	return res
}

func ensureWorkDir(opt options) (string, error) {
	if opt.WorkDir != "" {
		return opt.WorkDir, os.MkdirAll(opt.WorkDir, 0o755)
	}
	return os.MkdirTemp("", "judger-bench-*")
}

// writeSamples writes the sample slice atomically as NDJSON.
func writeSamples(path string, samples []sample) error {
	sort.SliceStable(samples, func(i, j int) bool {
		if samples[i].Phase != samples[j].Phase {
			return samples[i].Phase < samples[j].Phase
		}
		return samples[i].Iteration < samples[j].Iteration
	})
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, s := range samples {
		if err := enc.Encode(s); err != nil {
			return err
		}
	}
	return artifact.WriteFileAtomic(path, buf.Bytes(), 0o644)
}
