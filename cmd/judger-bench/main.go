// Command judger-bench runs the direct compile/execute loop inside the
// digest-pinned benchmark image and writes NDJSON samples.
//
// Compile is a separate, unsandboxed operation that reproduces the production
// C++ compiler flags. Execute invokes the external alpha.4 Judger exactly as
// the production Iris sandbox does: explicit fixed limits, uid/gid, seccomp
// rule, environment, and input/output paths. It records the Judger JSON fields
// and refuses to accept a sample whose reported cgroup is not beneath the
// configured delegated parent. It never patches a running PID.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// judgerAlpha4AMD64SHA256 is the authoritative digest of libjudger-amd64.so
// from https://github.com/skkuding/Judger/releases/download/v1.0.0-alpha.4/.
// It is enforced by images/judger-bench.Dockerfile and re-checked at run time.
const judgerAlpha4AMD64SHA256 = "2c9a4da817e06f49daabe6b437f81d10f78b42be517d2e345ab4f868a4189103"

const (
	defaultJudgerPath = "/app/sandbox/libjudger.so"
	// defaultExpectedCgroupParent is intentionally empty. The cgroup mount root
	// is rejected as an expected parent, so an explicit delegated subtree must
	// always be supplied by the caller.
	defaultExpectedCgroupParent = ""
)

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "judger-bench:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("judger-bench", flag.ContinueOnError)
	var (
		mode       = fs.String("mode", "", "compile or execute")
		runID      = fs.String("run-id", "", "run identifier")
		blockID    = fs.String("block-id", "", "block identifier")
		worker     = fs.String("worker", "", "worker identifier")
		fixture    = fs.String("fixture", "", "fixture name")
		source     = fs.String("source", "", "source file for compile mode")
		binaryPath = fs.String("binary", "", "compiled binary for execute mode (default work-dir/main)")
		input      = fs.String("input", "", "stdin fixture for execute mode")
		output     = fs.String("output", "", "NDJSON output path")
		workDir    = fs.String("work-dir", "", "scratch directory")
		iterations = fs.Int("iterations", 1, "execute iterations")
		compiler   = fs.String("compiler", defaultCompiler, "compiler binary for compile mode")
		timeout    = fs.Duration("timeout", 0, "outer per-command timeout (0 derives from the phase limits)")

		judgerPath     = fs.String("judger", defaultJudgerPath, "external alpha.4 Judger binary")
		judgerSHA      = fs.String("judger-sha256", judgerAlpha4AMD64SHA256, "required Judger binary SHA-256 (empty disables)")
		containerID    = fs.String("container-id", "", "CONTAINER_ID exported to Judger; selects sandbox-<id>")
		expectedParent = fs.String("expected-cgroup-parent", defaultExpectedCgroupParent, "delegated cgroup parent every sample must be beneath")
		uid            = fs.Int("uid", -1, "sandbox uid (-1 uses the Judger default 65534)")
		gid            = fs.Int("gid", -1, "sandbox gid (-1 uses the Judger default 65534)")
		seccompRule    = fs.String("seccomp-rule", defaultSeccompRule, "seccomp rule name")
		maxCPUTime     = fs.Int("max-cpu-time", defaultMaxCPUTimeMs, "max CPU time in milliseconds")
		maxRealTime    = fs.Int("max-real-time", defaultMaxRealTimeMs, "max real time in milliseconds")
		maxMemory      = fs.Int64("max-memory", defaultMaxMemoryBytes, "max memory in bytes")
		maxStack       = fs.Int64("max-stack", defaultMaxStackBytes, "max stack in bytes")
		maxOutput      = fs.Int64("max-output-size", defaultMaxOutputBytes, "max output in bytes")
		maxProcesses   = fs.Int("max-process-number", 0, "max process number (0 leaves Judger unlimited)")
	)
	var envList, runArgs stringList
	fs.Var(&envList, "env", "sandbox child environment KEY=VALUE (repeatable; default PATH)")
	fs.Var(&runArgs, "run-args", "extra argument appended after exe_path (repeatable)")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if *output == "" {
		return fmt.Errorf("--output is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opt := options{
		Mode:                 modeType(*mode),
		RunID:                *runID,
		BlockID:              *blockID,
		Worker:               *worker,
		Fixture:              *fixture,
		Source:               *source,
		Binary:               *binaryPath,
		Input:                *input,
		WorkDir:              *workDir,
		Iterations:           *iterations,
		Compiler:             *compiler,
		Timeout:              *timeout,
		JudgerPath:           *judgerPath,
		JudgerSHA256:         *judgerSHA,
		ContainerID:          *containerID,
		ExpectedCgroupParent: *expectedParent,
		UID:                  *uid,
		GID:                  *gid,
		SeccompRule:          *seccompRule,
		MaxCPUTimeMs:         *maxCPUTime,
		MaxRealTimeMs:        *maxRealTime,
		MaxMemoryBytes:       *maxMemory,
		MaxStackBytes:        *maxStack,
		MaxOutputBytes:       *maxOutput,
		MaxProcessNumber:     *maxProcesses,
		Env:                  envList,
		RunArgs:              runArgs,
	}

	samples, runErr := runBench(ctx, opt)
	// Evidence is written even when a sample is rejected for containment so a
	// failed run remains diagnosable.
	if runErr == nil || len(samples) > 0 {
		if err := writeSamples(*output, samples); err != nil {
			return err
		}
	}
	if runErr != nil {
		return runErr
	}
	fmt.Fprintf(os.Stderr, "judger-bench: wrote %d sample(s) to %s\n", len(samples), *output)
	return nil
}
