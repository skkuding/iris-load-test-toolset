package main

import (
	"context"
	"fmt"
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
	mu      sync.Mutex
	uploads [][2]string
}

func (u *fakeUploader) Upload(_ context.Context, localPath, remotePath string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.uploads = append(u.uploads, [2]string{localPath, remotePath})
	return nil
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
		protocol.ActionValidate, protocol.ActionBundle, protocol.ActionCleanup,
	}
	if got := inv.snapshot(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("action order = %v, want %v", got, want)
	}
	if len(factoryArgs) != 2 {
		t.Fatalf("invoker factories = %d, want 2", len(factoryArgs))
	}
	if !containsArg(factoryArgs[1], "--plan-file") {
		t.Fatalf("post-inspect invoker missing --plan-file: %v", factoryArgs[1])
	}
	if len(up.uploads) != 1 {
		t.Fatalf("plan uploads = %d, want 1", len(up.uploads))
	}
	st, err := orchestrator.FileStore{Root: filepath.Join(rc.resultRoot, ".state")}.Load(plan.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != orchestrator.StateCleaned {
		t.Fatalf("final state = %s, want cleaned", st.State)
	}
}

func TestExecuteRunStopsBeforeValidateAfterBlockFailure(t *testing.T) {
	plan, sha := controllerPlan(t)
	inv := &fakeInvoker{failOn: protocol.ActionRunBlock}
	rc := runConfig{
		plan:       plan,
		planSHA256: sha,
		resultRoot: t.TempDir(),
		ssh:        &fakeUploader{},
		newInvoker: func([]string) agentInvoker { return inv },
	}
	err := executeRun(context.Background(), rc)
	if err == nil || !strings.Contains(err.Error(), "run-block") {
		t.Fatalf("err = %v, want run-block failure", err)
	}
	want := []protocol.Action{protocol.ActionInspect, protocol.ActionPrepare, protocol.ActionRunBlock}
	if got := inv.snapshot(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("action order = %v, want %v (no validate/bundle/cleanup)", got, want)
	}
}

func TestExecuteRunRejectsIrisSuiteBeforeBlocks(t *testing.T) {
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
	err := executeRun(context.Background(), rc)
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("err = %v, want unsupported suite", err)
	}
	for _, a := range inv.snapshot() {
		if a == protocol.ActionValidate || a == protocol.ActionBundle || a == protocol.ActionCleanup {
			t.Fatalf("post-success action %s ran for an unsupported suite", a)
		}
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
		cgroupParent:  "/sys/fs/cgroup/iris-bench",
		cgroupMount:   "/sys/fs/cgroup",
		benchBinary:   "/opt/iris-bench/judger-bench",
		judgerPath:    "/app/sandbox/libjudger.so",
		judgerSHA256:  strings.Repeat("a", 64),
		containerID:   "iris-20260925-abcdefgh",
		workerTimeout: 30 * time.Second,
	}
	joined := strings.Join(o.agentArgs(), " ")
	for _, want := range []string{
		"--cgroup-parent /sys/fs/cgroup/iris-bench",
		"--cgroup-mount /sys/fs/cgroup",
		"--bench-binary /opt/iris-bench/judger-bench",
		"--judger /app/sandbox/libjudger.so",
		"--judger-sha256 " + strings.Repeat("a", 64),
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

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
