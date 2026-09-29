package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/skkuding/iris-load-test-toolset/internal/artifact"
	"github.com/skkuding/iris-load-test-toolset/internal/orchestrator"
	"github.com/skkuding/iris-load-test-toolset/internal/protocol"
	"github.com/skkuding/iris-load-test-toolset/internal/runplan"
	"github.com/skkuding/iris-load-test-toolset/internal/transport"
)

// directRunOptions are the target-side parameters a direct-suite run must
// forward to the agent. They are rendered into fixed agent flags, never into a
// remote shell string.
type directRunOptions struct {
	cgroupParent   string
	cgroupMount    string
	benchBinary    string
	judgerPath     string
	judgerSHA256   string
	benchmarkImage string
	containerID    string
	workerTimeout  time.Duration
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
	add("--benchmark-image", o.benchmarkImage)
	add("--container-id", o.containerID)
	if o.workerTimeout > 0 {
		args = append(args, "--worker-timeout", o.workerTimeout.String())
	}
	return args
}

// stagingTransport is the run-scoped remote staging boundary.
type stagingTransport interface {
	CreateRunDir(ctx context.Context, runID string) (string, error)
	Upload(ctx context.Context, localPath, remotePath string) error
	SHA256(ctx context.Context, remotePath string) (string, error)
	RemoveRunDir(ctx context.Context, runID string) error
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
	ssh        stagingTransport
	newInvoker func(remoteArgs []string) agentInvoker
	assets     []stagedAsset
}

type stagedAsset struct {
	localPath  string
	remotePath string
	verifySHA  string
}

func cmdRun(args []string) error {
	fs := newFlagSet("run")
	var o planOptions
	o.register(fs)
	var (
		agentOverride       = fs.String("agent", "", "REMOTE agent path override")
		localAgent          = fs.String("local-agent", "", "LOCAL version-matched iris-bench-agent binary to upload for this run")
		localJudger         = fs.String("local-judger", "", "LOCAL alpha.4 Judger executable to upload and seal")
		localWorkload       = fs.String("local-bench-binary", "", "LOCAL precompiled direct-suite workload binary to upload and seal")
		qualificationReport = fs.String("qualification-report", "", "LOCAL sanitized Ansible qualification report to upload and seal")
		dryRun              = fs.Bool("dry-run", false, "print the resolved plan and stop")
		yes                 = fs.Bool("yes", false, "skip confirmation prompts")
		direct              directRunOptions
	)
	fs.StringVar(&direct.cgroupParent, "cgroup-parent", "", "delegated cgroup v2 parent forwarded to the agent run-block")
	fs.StringVar(&direct.cgroupMount, "cgroup-mount", "", "cgroup v2 unified mount forwarded to the agent")
	fs.StringVar(&direct.judgerPath, "judger", "", "REMOTE external alpha.4 Judger path forwarded to each worker")
	fs.StringVar(&direct.judgerSHA256, "judger-sha256", "", "Judger SHA-256 forwarded to each worker; must match the plan")
	fs.StringVar(&direct.containerID, "container-id", "", "base CONTAINER_ID forwarded to the agent (per-worker suffix appended)")
	fs.DurationVar(&direct.workerTimeout, "worker-timeout", 0, "per-worker outer timeout forwarded to the agent")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if *localAgent != "" && *agentOverride != "" {
		return errors.New("--local-agent cannot be combined with remote --agent")
	}
	if *localJudger != "" && direct.judgerPath != "" {
		return errors.New("--local-judger cannot be combined with remote --judger")
	}
	if o.benchmarkImage != "" && (*localJudger != "" || direct.judgerPath != "") {
		return errors.New("--benchmark-image cannot be combined with host Judger flags")
	}
	o.localBenchBinary = *localWorkload
	o.qualificationReport = *qualificationReport
	if *localJudger != "" {
		if o.judgerDigestFile != "" && o.judgerDigestFile != *localJudger {
			return errors.New("--local-judger conflicts with --judger-digest-file")
		}
		o.judgerDigestFile = *localJudger
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
	if plan.Suite == "judger" {
		direct.benchBinary = plan.WorkloadBinary.Path
		direct.benchmarkImage = o.benchmarkImage
		if *localJudger != "" {
			direct.judgerPath = stagedPath(plan.RunID, "judger")
			direct.judgerSHA256 = strings.TrimPrefix(plan.JudgerDigest, "sha256:")
		}
	}

	ssh := makeSSH(cfg, o.host, o.sshControlPath)
	remoteAgent := agentPath(cfg, *agentOverride)
	if *localAgent != "" {
		remoteAgent = stagedPath(plan.RunID, "agent")
	}
	baseArgs := direct.agentArgs()
	newInvoker := func(extra []string) agentInvoker {
		remoteArgs := append(append([]string(nil), baseArgs...), extra...)
		return transport.AgentClient{
			SSH:         ssh,
			RemoteAgent: remoteAgent,
			RemoteArgs:  remoteArgs,
			Stderr:      os.Stderr,
		}
	}
	assets, err := runAssets(o, plan, *localAgent, *localJudger)
	if err != nil {
		return err
	}
	return executeRun(ctx, runConfig{
		plan:       plan,
		planSHA256: sha,
		resultRoot: cfg.Paths.ResultRoot,
		ssh:        ssh,
		newInvoker: newInvoker,
		assets:     assets,
	})
}

// executeRun drives inspect -> prepare -> every block -> validate -> cleanup ->
// bundle. validate, cleanup, and bundle are reached only after every block
// completed. Cleanup removes only the agent runtime directory; evidence stays
// under the remote var root for `collect`.
func executeRun(ctx context.Context, rc runConfig) (retErr error) {
	if rc.ssh == nil || rc.newInvoker == nil {
		return fmt.Errorf("run: ssh and agent invoker are required")
	}
	orch := orchestrator.New(orchestrator.FileStore{Root: filepath.Join(rc.resultRoot, ".state")})
	if _, err := orch.InitState(rc.plan.RunID, rc.planSHA256, ""); err != nil {
		return fmt.Errorf("init run state: %w", err)
	}
	staged, err := stageRun(ctx, rc.ssh, rc.plan, rc.assets)
	if err != nil {
		return fmt.Errorf("stage run: %w", err)
	}
	defer func() {
		if err := rc.ssh.RemoveRunDir(context.WithoutCancel(ctx), rc.plan.RunID); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("remove staged run directory: %w", err))
		}
	}()
	invoker := rc.newInvoker([]string{"--plan-file", staged})
	if err := runOperations(ctx, orch, invoker, rc); err != nil {
		// Any failure can leave the tmpfs runtime directory behind. Remove it
		// best-effort while the staged agent still exists (RemoveRunDir runs
		// only after this function returns). Evidence under the var root is
		// intentionally preserved for collection and diagnosis.
		cleanupErr := invokeOp(context.WithoutCancel(ctx), orch, invoker, rc.plan.RunID, rc.planSHA256, protocol.ActionCleanup, "")
		if cleanupErr != nil {
			return errors.Join(err, fmt.Errorf("post-failure cleanup: %w", cleanupErr))
		}
		return err
	}
	return nil
}

// runOperations runs the ordered agent operations for a staged run. The normal
// path performs cleanup before bundle so post-cleanup phases cannot recreate
// run state.
func runOperations(ctx context.Context, orch *orchestrator.Orchestrator, invoker orchestrator.Invoker, rc runConfig) error {
	if err := invokeOp(ctx, orch, invoker, rc.plan.RunID, rc.planSHA256, protocol.ActionInspect, ""); err != nil {
		return fmt.Errorf("inspect: %w", err)
	}
	if err := invokeOp(ctx, orch, invoker, rc.plan.RunID, rc.planSHA256, protocol.ActionPrepare, ""); err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	for _, block := range rc.plan.Blocks {
		if err := invokeOp(ctx, orch, invoker, rc.plan.RunID, rc.planSHA256, protocol.ActionRunBlock, block.ID); err != nil {
			return fmt.Errorf("run-block %s: %w", block.ID, err)
		}
	}
	if err := invokeOp(ctx, orch, invoker, rc.plan.RunID, rc.planSHA256, protocol.ActionValidate, ""); err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	if err := invokeOp(ctx, orch, invoker, rc.plan.RunID, rc.planSHA256, protocol.ActionCleanup, ""); err != nil {
		return fmt.Errorf("cleanup: %w", err)
	}
	if err := invokeOp(ctx, orch, invoker, rc.plan.RunID, rc.planSHA256, protocol.ActionBundle, ""); err != nil {
		return fmt.Errorf("bundle: %w", err)
	}
	return nil
}

// stageRun writes and uploads the plan and every sealed local dependency.
func stageRun(ctx context.Context, ssh stagingTransport, plan runplan.Plan, assets []stagedAsset) (staged string, retErr error) {
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
	dir, err := ssh.CreateRunDir(ctx, plan.RunID)
	if err != nil {
		return "", err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, ssh.RemoveRunDir(context.WithoutCancel(ctx), plan.RunID))
		}
	}()
	staged = stagedPath(plan.RunID, "plan.json")
	all := append([]stagedAsset{{localPath: tmpPath, remotePath: staged}}, assets...)
	for _, asset := range all {
		if asset.localPath == "" || asset.remotePath == "" || filepath.Dir(asset.remotePath) != dir {
			return "", fmt.Errorf("unsafe staged asset mapping %q -> %q", asset.localPath, asset.remotePath)
		}
		if err := ssh.Upload(ctx, asset.localPath, asset.remotePath); err != nil {
			return "", err
		}
		if asset.verifySHA != "" {
			got, err := ssh.SHA256(ctx, asset.remotePath)
			if err != nil {
				return "", fmt.Errorf("verify staged %s: %w", filepath.Base(asset.remotePath), err)
			}
			if got != asset.verifySHA {
				return "", fmt.Errorf("verify staged %s: remote sha256 %s does not match local %s", filepath.Base(asset.remotePath), got, asset.verifySHA)
			}
		}
	}
	return staged, nil
}

func runAssets(o planOptions, plan runplan.Plan, localAgent, localJudger string) ([]stagedAsset, error) {
	assets := make([]stagedAsset, 0, len(plan.Fixtures)*2+4)
	if len(o.fixtures) != len(plan.Fixtures) || len(o.expectedOutputs) != len(plan.Fixtures) {
		return nil, errors.New("fixture staging does not match the sealed plan")
	}
	expected := make(map[string]string, len(o.expectedOutputs))
	for _, spec := range o.expectedOutputs {
		name, local, _ := strings.Cut(spec, "=")
		expected[name] = local
	}
	for i, spec := range o.fixtures {
		name, local, _ := strings.Cut(spec, "=")
		assets = append(assets,
			stagedAsset{localPath: local, remotePath: plan.Fixtures[i].Path},
			stagedAsset{localPath: expected[name], remotePath: plan.Fixtures[i].ExpectedOutputPath},
		)
	}
	if o.localBenchBinary != "" {
		assets = append(assets, stagedAsset{localPath: o.localBenchBinary, remotePath: plan.WorkloadBinary.Path})
	}
	if plan.IrisSuite != nil {
		assets = append(assets,
			stagedAsset{localPath: o.irisSource, remotePath: plan.IrisSuite.Source.Path, verifySHA: plan.IrisSuite.Source.SHA256},
			stagedAsset{localPath: o.irisEnvFile, remotePath: plan.IrisSuite.SecretEnvironment.Path, verifySHA: plan.IrisSuite.SecretEnvironment.SHA256},
		)
	}
	if o.qualificationReport != "" {
		assets = append(assets, stagedAsset{localPath: o.qualificationReport, remotePath: plan.Qualification.Report.Path})
	}
	if localAgent != "" {
		info, err := os.Stat(localAgent)
		if err != nil {
			return nil, fmt.Errorf("local agent: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return nil, errors.New("--local-agent must be an executable regular file")
		}
		sum, _, err := artifact.HashFile(localAgent)
		if err != nil {
			return nil, fmt.Errorf("hash local agent: %w", err)
		}
		assets = append(assets, stagedAsset{localPath: localAgent, remotePath: stagedPath(plan.RunID, "agent"), verifySHA: sum})
	}
	if localJudger != "" {
		info, err := os.Stat(localJudger)
		if err != nil {
			return nil, fmt.Errorf("local Judger: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return nil, errors.New("--local-judger must be an executable regular file")
		}
		sum, _, err := artifact.HashFile(localJudger)
		if err != nil {
			return nil, fmt.Errorf("hash local Judger: %w", err)
		}
		if sum != strings.TrimPrefix(plan.JudgerDigest, "sha256:") {
			return nil, errors.New("local Judger digest does not match the sealed plan")
		}
		assets = append(assets, stagedAsset{localPath: localJudger, remotePath: stagedPath(plan.RunID, "judger"), verifySHA: sum})
	}
	return assets, nil
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
