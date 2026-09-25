package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/skkuding/iris-load-test-toolset/internal/orchestrator"
	"github.com/skkuding/iris-load-test-toolset/internal/protocol"
	"github.com/skkuding/iris-load-test-toolset/internal/runplan"
	"github.com/skkuding/iris-load-test-toolset/internal/transport"
)

// directRunOptions are the target-side parameters a direct-suite run must
// forward to the agent. They are rendered into fixed agent flags, never into a
// remote shell string.
type directRunOptions struct {
	cgroupParent  string
	cgroupMount   string
	benchBinary   string
	judgerPath    string
	judgerSHA256  string
	containerID   string
	workerTimeout time.Duration
}

// agentArgs renders only non-empty flags so the agent's own defaults apply when
// the operator does not override a value.
func (o directRunOptions) agentArgs() []string {
	var args []string
	add := func(flag, val string) {
		if val != "" {
			args = append(args, flag, val)
		}
	}
	add("--cgroup-parent", o.cgroupParent)
	add("--cgroup-mount", o.cgroupMount)
	add("--bench-binary", o.benchBinary)
	add("--judger", o.judgerPath)
	add("--judger-sha256", o.judgerSHA256)
	add("--container-id", o.containerID)
	if o.workerTimeout > 0 {
		args = append(args, "--worker-timeout", o.workerTimeout.String())
	}
	return args
}

// sshUploader is the file-transfer boundary used to stage the plan.
type sshUploader interface {
	Upload(ctx context.Context, localPath, remotePath string) error
}

// agentInvoker is the remote agent boundary.
type agentInvoker interface {
	Invoke(ctx context.Context, req protocol.Request, sink func(protocol.Event) error) (protocol.Event, error)
}

// runConfig is the resolved input to one controller-driven run. It is a value
// so tests can drive the whole sequence with fake SSH and agent boundaries.
type runConfig struct {
	plan       runplan.Plan
	planSHA256 string
	resultRoot string
	ssh        sshUploader
	newInvoker func(remoteArgs []string) agentInvoker
}

func cmdRun(args []string) error {
	fs := newFlagSet("run")
	var o planOptions
	o.register(fs)
	var (
		agentOverride = fs.String("agent", "", "remote agent path override")
		dryRun        = fs.Bool("dry-run", false, "print the resolved plan and stop")
		yes           = fs.Bool("yes", false, "skip confirmation prompts")
		direct        directRunOptions
	)
	fs.StringVar(&direct.cgroupParent, "cgroup-parent", "", "delegated cgroup v2 parent forwarded to the agent run-block")
	fs.StringVar(&direct.cgroupMount, "cgroup-mount", "", "cgroup v2 unified mount forwarded to the agent")
	fs.StringVar(&direct.benchBinary, "bench-binary", "", "precompiled direct-suite binary forwarded to each worker")
	fs.StringVar(&direct.judgerPath, "judger", "", "external alpha.4 Judger path forwarded to each worker")
	fs.StringVar(&direct.judgerSHA256, "judger-sha256", "", "Judger SHA-256 forwarded to each worker; must match the plan")
	fs.StringVar(&direct.containerID, "container-id", "", "base CONTAINER_ID forwarded to the agent (per-worker suffix appended)")
	fs.DurationVar(&direct.workerTimeout, "worker-timeout", 0, "per-worker outer timeout forwarded to the agent")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	_ = yes
	cfg, err := loadConfig(o.configPath)
	if err != nil {
		return err
	}
	ctx := context.Background()
	plan, sha, err := o.buildPlan(ctx, cfg)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "run %s suite=%s profile=%s blocks=%d planSha256=%s cgroupParent=%s\n",
		plan.RunID, plan.Suite, plan.Profile, len(plan.Blocks), sha, direct.cgroupParent)
	if *dryRun {
		return nil
	}
	if plan.Suite != "judger" {
		return fmt.Errorf("suite %q is not supported in this build; only the direct judger suite is implemented", plan.Suite)
	}

	ssh := makeSSH(cfg, o.host, o.sshControlPath)
	baseArgs := direct.agentArgs()
	newInvoker := func(extra []string) agentInvoker {
		remoteArgs := append(append([]string(nil), baseArgs...), extra...)
		return transport.AgentClient{
			SSH:         ssh,
			RemoteAgent: agentPath(cfg, *agentOverride),
			RemoteArgs:  remoteArgs,
			Stderr:      os.Stderr,
		}
	}
	return executeRun(ctx, runConfig{
		plan:       plan,
		planSHA256: sha,
		resultRoot: cfg.Paths.ResultRoot,
		ssh:        ssh,
		newInvoker: newInvoker,
	})
}

// executeRun drives inspect -> prepare -> every block -> validate -> bundle ->
// cleanup. validate, bundle, and cleanup are reached only after every block
// completed. Cleanup removes only the agent runtime directory; evidence stays
// under the remote var root for `collect`.
func executeRun(ctx context.Context, rc runConfig) error {
	if rc.ssh == nil || rc.newInvoker == nil {
		return fmt.Errorf("run: ssh and agent invoker are required")
	}
	orch := orchestrator.New(orchestrator.FileStore{Root: filepath.Join(rc.resultRoot, ".state")})
	if _, err := orch.InitState(rc.plan.RunID, rc.planSHA256, ""); err != nil {
		return fmt.Errorf("init run state: %w", err)
	}
	// Inspect does not need the staged plan file.
	if err := invokeOp(ctx, orch, rc.newInvoker(nil), rc.plan.RunID, rc.planSHA256, protocol.ActionInspect, ""); err != nil {
		return fmt.Errorf("inspect: %w", err)
	}
	staged, err := stagePlan(rc.ssh, rc.plan)
	if err != nil {
		return fmt.Errorf("stage plan: %w", err)
	}
	invoker := rc.newInvoker([]string{"--plan-file", staged})
	if err := invokeOp(ctx, orch, invoker, rc.plan.RunID, rc.planSHA256, protocol.ActionPrepare, ""); err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	for _, block := range rc.plan.Blocks {
		if block.Suite != "judger" {
			return fmt.Errorf("run-block %s: suite %q is not supported in this build", block.ID, block.Suite)
		}
		if err := invokeOp(ctx, orch, invoker, rc.plan.RunID, rc.planSHA256, protocol.ActionRunBlock, block.ID); err != nil {
			return fmt.Errorf("run-block %s: %w", block.ID, err)
		}
	}
	if err := invokeOp(ctx, orch, invoker, rc.plan.RunID, rc.planSHA256, protocol.ActionValidate, ""); err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	if err := invokeOp(ctx, orch, invoker, rc.plan.RunID, rc.planSHA256, protocol.ActionBundle, ""); err != nil {
		return fmt.Errorf("bundle: %w", err)
	}
	if err := invokeOp(ctx, orch, invoker, rc.plan.RunID, rc.planSHA256, protocol.ActionCleanup, ""); err != nil {
		return fmt.Errorf("cleanup: %w", err)
	}
	return nil
}

// stagePlan writes the plan locally and uploads it to a staging path on the
// target. The agent verifies the canonical plan digest before copying it into
// the run dir.
func stagePlan(ssh sshUploader, plan runplan.Plan) (string, error) {
	tmp, err := os.CreateTemp("", "iris-bench-plan-*.json")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath)
	if err := plan.WriteStore(tmpPath); err != nil {
		return "", err
	}
	staged := "/tmp/iris-bench-" + plan.RunID + ".plan.json"
	if err := ssh.Upload(context.Background(), tmpPath, staged); err != nil {
		return "", err
	}
	return staged, nil
}

func invokeOp(ctx context.Context, orch *orchestrator.Orchestrator, invoker orchestrator.Invoker, runID, planSHA string, action protocol.Action, blockID string) error {
	opID, err := newOpID()
	if err != nil {
		return err
	}
	req := protocol.Request{
		ProtocolVersion: protocol.Version,
		OperationID:     opID,
		RunID:           runID,
		Action:          action,
		PlanSHA256:      planSHA,
		BlockID:         blockID,
	}
	_, err = orch.RunOperation(ctx, runID, req, invoker, func(ev protocol.Event) error {
		switch {
		case ev.Kind == protocol.KindLog:
			fmt.Fprintf(os.Stderr, "  log: %s\n", ev.Message)
		default:
			fmt.Fprintf(os.Stderr, "  %s %s%s %s %s\n", ev.Kind, ev.Phase, ev.Name, ev.Status, ev.Message)
		}
		return nil
	})
	return err
}
