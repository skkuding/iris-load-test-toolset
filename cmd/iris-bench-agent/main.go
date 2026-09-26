// Command iris-bench-agent runs on the benchmark host. The controller invokes
// it over SSH with one JSON request on stdin; the agent emits validated NDJSON
// events on stdout and writes samples atomically on the host. It is not a
// network daemon.
//
// The same binary also implements the internal "worker-exec" subcommand used to
// place a worker in its cgroup behind the readiness barrier before it runs
// measured code.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/skkuding/iris-load-test-toolset/internal/config"
	"github.com/skkuding/iris-load-test-toolset/internal/containment"
	"github.com/skkuding/iris-load-test-toolset/internal/protocol"
)

// version is the agent version compiled into the binary.
var version = "0.1.0"

func main() {
	if isWorkerExec(os.Args) {
		if err := runWorkerExec(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "iris-bench-agent:", err)
			os.Exit(1)
		}
		return
	}

	fs := flag.NewFlagSet("iris-bench-agent", flag.ContinueOnError)
	var (
		varRoot         = fs.String("var-root", config.DefaultVarRoot, "non-secret state root")
		runRoot         = fs.String("run-root", config.DefaultRunRoot, "tmpfs runtime root")
		planFile        = fs.String("plan-file", "", "explicit staged plan path override")
		cgroupParent    = fs.String("cgroup-parent", "", "explicitly delegated cgroup v2 parent (required for run-block)")
		cgroupMount     = fs.String("cgroup-mount", containment.DefaultMount, "cgroup v2 unified mount")
		cgroupMems      = fs.String("cpuset-mems", "0", "cpuset.mems assigned to each worker")
		benchBinary     = fs.String("bench-binary", "", "precompiled direct-suite binary executed by each worker")
		workerBin       = fs.String("worker-bin", "judger-bench", "worker command executed inside each cgroup")
		judgerPath      = fs.String("judger", "", "external alpha.4 Judger binary path (empty uses the worker default)")
		judgerSHA       = fs.String("judger-sha256", "", "Judger SHA-256; must agree with the sealed plan digest")
		benchmarkImage  = fs.String("benchmark-image", "", "immutable local judger-bench Docker image ID; must agree with the sealed plan")
		containerID     = fs.String("container-id", "", "base CONTAINER_ID; a per-worker suffix is appended (empty uses the run id)")
		workerTimeout   = fs.Duration("worker-timeout", 0, "per-worker outer timeout; defaults to the block ceiling divided across repetitions")
		monitorInterval = fs.Duration("monitor-interval", 100*time.Millisecond, "process cgroup membership polling interval")
		runTimeout      = fs.Duration("run-timeout", 0, "hard block timeout; defaults to the plan qualification limit")
	)
	var workerArgs repeated
	fs.Var(&workerArgs, "worker-arg", "worker argument template with {run},{block},{worker},{fixture},{input},{expected-output},{output},{iterations},{cpuset},{bench-binary},{run-dir},{runtime-dir},{judger},{judger-sha256},{container-id},{expected-cgroup-parent},{timeout} (repeatable; overrides defaults; production-compat is still appended when sealed)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}

	// The agent is normally launched from an SSH session cgroup that is a
	// sibling of the delegated user service. Worker attach into the delegated
	// parent would then fail the kernel's common-ancestor permission check.
	// Before any protocol bytes are read, replace this process with a fresh
	// agent running inside a systemd-run user scope, or verify that the scoped
	// child landed beneath the delegated parent and fail closed otherwise.
	// The internal worker-exec path is dispatched above and never reaches here.
	scoped := scopedExec{
		mount:  *cgroupMount,
		parent: *cgroupParent,
		args:   os.Args[1:],
	}
	if err := scoped.apply(); err != nil {
		fmt.Fprintln(os.Stderr, "iris-bench-agent:", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	req, err := protocol.DecodeRequest(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "iris-bench-agent:", err)
		os.Exit(2)
	}
	a := &agent{
		varRoot:         *varRoot,
		runRoot:         *runRoot,
		planFile:        *planFile,
		version:         version,
		stderr:          os.Stderr,
		cgroupParent:    *cgroupParent,
		cgroupMount:     *cgroupMount,
		cpusetMems:      *cgroupMems,
		benchBinary:     *benchBinary,
		workerBin:       *workerBin,
		workerArgs:      append([]string(nil), workerArgs...),
		judgerPath:      *judgerPath,
		judgerSHA256:    *judgerSHA,
		benchmarkImage:  *benchmarkImage,
		containerID:     *containerID,
		workerTimeout:   *workerTimeout,
		monitorInterval: *monitorInterval,
		runTimeout:      *runTimeout,
	}
	enc := protocol.NewEventEncoder(os.Stdout)
	if err := a.execute(ctx, req, enc); err != nil {
		fmt.Fprintln(os.Stderr, "iris-bench-agent:", err)
		os.Exit(1)
	}
}

// repeated is a repeatable string flag.
type repeated []string

func (r *repeated) String() string { return fmt.Sprint([]string(*r)) }

func (r *repeated) Set(v string) error {
	*r = append(*r, v)
	return nil
}
