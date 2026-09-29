package main

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

	"github.com/skkuding/iris-load-test-toolset/internal/orchestrator"
	"github.com/skkuding/iris-load-test-toolset/internal/protocol"
	"github.com/skkuding/iris-load-test-toolset/internal/runplan"
)

// fakeUploader records plan staging without touching a host.
type fakeUploader struct {
	mu           sync.Mutex
	uploads      [][2]string
	creates      []string
	removes      []string
	hashes       map[string]string
	failUploadAt int
	removeErr    error
}

func (u *fakeUploader) CreateRunDir(_ context.Context, runID string) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.creates = append(u.creates, runID)
	return "/tmp/iris-bench-" + runID, nil
}

func (u *fakeUploader) RemoveRunDir(_ context.Context, runID string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.removes = append(u.removes, runID)
	return u.removeErr
}

func (u *fakeUploader) Upload(_ context.Context, localPath, remotePath string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.failUploadAt > 0 && len(u.uploads) == u.failUploadAt {
		return errors.New("upload failed")
	}
	u.uploads = append(u.uploads, [2]string{localPath, remotePath})
	return nil
}

func (u *fakeUploader) SHA256(_ context.Context, remotePath string) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hashes[remotePath], nil
}

// fakeInvoker records the exact action sequence the controller drives and can
// fail one action to prove the run stops before later phases.
type fakeInvoker struct {
	mu      sync.Mutex
	actions []protocol.Action
	failOn  protocol.Action
}

func (f *fakeInvoker) Invoke(_ context.Context, req protocol.Request, _ func(protocol.Event) error) (protocol.Event, error) {
	f.mu.Lock()
	f.actions = append(f.actions, req.Action)
	fail := req.Action == f.failOn
	f.mu.Unlock()
	if fail {
		return protocol.Event{Kind: protocol.KindResult, Status: protocol.StatusFailed, Message: "simulated failure"}, nil
	}
	return protocol.Event{Kind: protocol.KindResult, Status: protocol.StatusCompleted, ReceiptSHA256: strings.Repeat("d", 64)}, nil
}

func (f *fakeInvoker) snapshot() []protocol.Action {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]protocol.Action(nil), f.actions...)
}

func controllerPlan(t *testing.T) (runplan.Plan, string) {
	t.Helper()
	o := baseOptions()
	plan, sha, err := o.buildPlan(context.Background(), testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	return plan, sha
}

func TestExecuteRunSequencesInspectThroughCleanup(t *testing.T) {
	plan, sha := controllerPlan(t)
	up := &fakeUploader{}
	inv := &fakeInvoker{}
	var factoryArgs [][]string
	rc := runConfig{
		plan:       plan,
		planSHA256: sha,
		resultRoot: t.TempDir(),
		ssh:        up,
		newInvoker: func(extra []string) agentInvoker {
			factoryArgs = append(factoryArgs, append([]string(nil), extra...))
			return inv
		},
	}
	if err := executeRun(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	want := []protocol.Action{
		protocol.ActionInspect, protocol.ActionPrepare, protocol.ActionRunBlock,
		protocol.ActionValidate, protocol.ActionCleanup, protocol.ActionBundle,
	}
	if got := inv.snapshot(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("action order = %v, want %v", got, want)
	}
	if len(factoryArgs) != 1 {
		t.Fatalf("invoker factories = %d, want 1", len(factoryArgs))
	}
	if !containsArg(factoryArgs[0], "--plan-file") {
		t.Fatalf("invoker missing --plan-file: %v", factoryArgs[0])
	}
	if len(up.creates) != 1 || len(up.uploads) != 1 || len(up.removes) != 1 {
		t.Fatalf("staging creates/uploads/removes = %d/%d/%d, want 1/1/1", len(up.creates), len(up.uploads), len(up.removes))
	}
	st, err := orchestrator.FileStore{Root: filepath.Join(rc.resultRoot, ".state")}.Load(plan.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != orchestrator.StateCollected {
		t.Fatalf("final state = %s, want collected after cleanup and bundle", st.State)
	}
}

func TestExecuteRunStopsBeforeValidateAfterBlockFailure(t *testing.T) {
	plan, sha := controllerPlan(t)
	inv := &fakeInvoker{failOn: protocol.ActionRunBlock}
	up := &fakeUploader{}
	rc := runConfig{
		plan:       plan,
		planSHA256: sha,
		resultRoot: t.TempDir(),
		ssh:        up,
		newInvoker: func([]string) agentInvoker { return inv },
	}
	err := executeRun(context.Background(), rc)
	if err == nil || !strings.Contains(err.Error(), "run-block") {
		t.Fatalf("err = %v, want run-block failure", err)
	}
	// A failure stops the forward sequence but still triggers a best-effort
	// cleanup of the remote runtime directory; validate and bundle never run.
	want := []protocol.Action{
		protocol.ActionInspect, protocol.ActionPrepare, protocol.ActionRunBlock, protocol.ActionCleanup,
	}
	if got := inv.snapshot(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("action order = %v, want %v (cleanup after failure, no validate/bundle)", got, want)
	}
	if len(up.removes) != 1 {
		t.Fatalf("staged cleanup removes = %v, want plan removed after failure", up.removes)
	}
	st, err := orchestrator.FileStore{Root: filepath.Join(rc.resultRoot, ".state")}.Load(plan.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != orchestrator.StateCleaned {
		t.Fatalf("final state = %s, want cleaned after post-failure cleanup", st.State)
	}
}

func TestStageRunCleansPartialUploadsAndJoinsCleanupError(t *testing.T) {
	plan, _ := controllerPlan(t)
	up := &fakeUploader{failUploadAt: 1, removeErr: errors.New("remove failed")}
	_, err := stageRun(context.Background(), up, plan, []stagedAsset{{localPath: "/local/workload", remotePath: stagedPath(plan.RunID, "workload")}})
	if err == nil || !strings.Contains(err.Error(), "upload failed") || !strings.Contains(err.Error(), "remove failed") {
		t.Fatalf("err = %v, want upload and cleanup errors", err)
	}
	if len(up.uploads) != 1 || fmt.Sprint(up.removes) != fmt.Sprint([]string{plan.RunID}) {
		t.Fatalf("uploads/removes = %v/%v", up.uploads, up.removes)
	}
}

func TestExecuteRunJoinsPrimaryAndStagedCleanupErrors(t *testing.T) {
	plan, sha := controllerPlan(t)
	up := &fakeUploader{removeErr: errors.New("remove failed")}
	inv := &fakeInvoker{failOn: protocol.ActionInspect}
	err := executeRun(context.Background(), runConfig{
		plan: plan, planSHA256: sha, resultRoot: t.TempDir(), ssh: up,
		newInvoker: func([]string) agentInvoker { return inv },
	})
	if err == nil || !strings.Contains(err.Error(), "inspect") || !strings.Contains(err.Error(), "remove failed") {
		t.Fatalf("err = %v, want primary and cleanup errors", err)
	}
}

func TestExecuteRunRunsEveryBlockBeforeValidation(t *testing.T) {
	plan, _ := controllerPlan(t)
	second := plan.Blocks[0]
	second.ID = "isolated-1s-02"
	plan.Blocks = append(plan.Blocks, second)
	plan.Expected.Blocks = len(plan.Blocks)
	sha, err := plan.Digest()
	if err != nil {
		t.Fatal(err)
	}
	inv := &fakeInvoker{}
	rc := runConfig{plan: plan, planSHA256: sha, resultRoot: t.TempDir(), ssh: &fakeUploader{}, newInvoker: func([]string) agentInvoker { return inv }}
	if err := executeRun(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	want := []protocol.Action{protocol.ActionInspect, protocol.ActionPrepare, protocol.ActionRunBlock, protocol.ActionRunBlock, protocol.ActionValidate, protocol.ActionCleanup, protocol.ActionBundle}
	if got := inv.snapshot(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("action order = %v, want %v", got, want)
	}
}

func TestExecuteRunDispatchesIrisSuite(t *testing.T) {
	plan, sha := controllerPlan(t)
	plan.Blocks[0].Suite = "iris"
	inv := &fakeInvoker{}
	rc := runConfig{
		plan:       plan,
		planSHA256: sha,
		resultRoot: t.TempDir(),
		ssh:        &fakeUploader{},
		newInvoker: func([]string) agentInvoker { return inv },
	}
	if err := executeRun(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	want := []protocol.Action{protocol.ActionInspect, protocol.ActionPrepare, protocol.ActionRunBlock, protocol.ActionValidate, protocol.ActionCleanup, protocol.ActionBundle}
	if got := inv.snapshot(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("action order = %v, want %v", got, want)
	}
}

func TestExecuteRunRequiresBoundaries(t *testing.T) {
	plan, sha := controllerPlan(t)
	if err := executeRun(context.Background(), runConfig{plan: plan, planSHA256: sha}); err == nil {
		t.Fatal("executeRun accepted nil ssh/newInvoker")
	}
}

func TestDirectRunOptionsAgentArgs(t *testing.T) {
	o := directRunOptions{
		cgroupParent:   "/sys/fs/cgroup/iris-bench",
		cgroupMount:    "/sys/fs/cgroup",
		benchBinary:    "/opt/iris-bench/judger-bench",
		judgerPath:     "/app/sandbox/libjudger.so",
		judgerSHA256:   strings.Repeat("a", 64),
		benchmarkImage: "sha256:" + strings.Repeat("b", 64),
		containerID:    "iris-20260925-abcdefgh",
		workerTimeout:  30 * time.Second,
	}
	joined := strings.Join(o.agentArgs(), " ")
	for _, want := range []string{
		"--cgroup-parent /sys/fs/cgroup/iris-bench",
		"--cgroup-mount /sys/fs/cgroup",
		"--bench-binary /opt/iris-bench/judger-bench",
		"--judger /app/sandbox/libjudger.so",
		"--judger-sha256 " + strings.Repeat("a", 64),
		"--benchmark-image sha256:" + strings.Repeat("b", 64),
		"--container-id iris-20260925-abcdefgh",
		"--worker-timeout 30s",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("forwarded args missing %q in %q", want, joined)
		}
	}
	if got := (directRunOptions{}).agentArgs(); len(got) != 0 {
		t.Fatalf("empty options rendered %v, want no flags", got)
	}
}

func TestRunAssetsUseSealedDeterministicPaths(t *testing.T) {
	dir := t.TempDir()
	workload := filepath.Join(dir, "workload")
	report := filepath.Join(dir, "report.json")
	if err := os.WriteFile(workload, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(report, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	agentPath := filepath.Join(dir, "agent")
	if err := os.WriteFile(agentPath, []byte("agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	o := baseOptions()
	o.localBenchBinary = workload
	o.benchBinary = ""
	o.benchBinarySHA = ""
	o.qualificationReport = report
	plan, _, err := o.buildPlan(context.Background(), testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	assets, err := runAssets(o, plan, agentPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 5 {
		t.Fatalf("assets = %d, want fixture pair, workload, report, and agent", len(assets))
	}
	for _, asset := range assets {
		if filepath.Dir(asset.remotePath) != "/tmp/iris-bench-"+plan.RunID {
			t.Fatalf("non-deterministic staged path %q", asset.remotePath)
		}
	}
	if plan.WorkloadBinary.Path != stagedPath(plan.RunID, "workload") || plan.Qualification.Report == nil {
		t.Fatalf("sealed staged files = %+v / %+v", plan.WorkloadBinary, plan.Qualification.Report)
	}
	agentAsset := assets[len(assets)-1]
	if agentAsset.verifySHA == "" {
		t.Fatal("local agent was not sealed for remote hash verification")
	}
}

func TestStageRunRejectsRemoteAgentHashMismatchAndCleansDirectory(t *testing.T) {
	plan, _ := controllerPlan(t)
	remote := stagedPath(plan.RunID, "agent")
	up := &fakeUploader{hashes: map[string]string{remote: strings.Repeat("f", 64)}}
	_, err := stageRun(context.Background(), up, plan, []stagedAsset{
		{localPath: "/local/agent", remotePath: remote, verifySHA: strings.Repeat("a", 64)},
	})
	if err == nil || !strings.Contains(err.Error(), "remote sha256") {
		t.Fatalf("stageRun error = %v, want hash mismatch", err)
	}
	if fmt.Sprint(up.removes) != fmt.Sprint([]string{plan.RunID}) {
		t.Fatalf("cleanup = %v, want run directory removal", up.removes)
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
