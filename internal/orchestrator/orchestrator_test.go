package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/skkuding/iris-load-test-toolset/internal/protocol"
)

const planSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type fakeInvoker struct {
	calls    int
	terminal protocol.Event
	err      error
}

func (f *fakeInvoker) Invoke(_ context.Context, _ protocol.Request, sink func(protocol.Event) error) (protocol.Event, error) {
	f.calls++
	if sink != nil {
		_ = sink(protocol.Event{Seq: 1, Kind: protocol.KindPhase, Phase: "x", Status: protocol.StatusStarted})
	}
	if f.terminal.Status == "" {
		return protocol.Event{Kind: protocol.KindResult, Status: protocol.StatusCompleted}, f.err
	}
	return f.terminal, f.err
}

func newOrch(t *testing.T) (*Orchestrator, string) {
	t.Helper()
	o := New(FileStore{Root: t.TempDir()})
	o.Now = func() time.Time { return time.Unix(0, 0).UTC() }
	if _, err := o.InitState("iris-20260925-abcdefgh", planSHA, "boot-1"); err != nil {
		t.Fatal(err)
	}
	return o, "iris-20260925-abcdefgh"
}

func req(action protocol.Action, opID, blockID string) protocol.Request {
	return protocol.Request{
		ProtocolVersion: protocol.Version,
		OperationID:     opID,
		RunID:           "iris-20260925-abcdefgh",
		Action:          action,
		PlanSHA256:      planSHA,
		BlockID:         blockID,
	}
}

func TestTransitionRules(t *testing.T) {
	ok := [][2]State{
		{StatePlanned, StateQualified},
		{StateQualified, StatePrepared},
		{StatePrepared, StateRunning},
		{StateRunning, StateValidating},
		{StateValidating, StateComplete},
		{StateComplete, StateCollected},
		{StateCollected, StateCleaned},
		{StateFailed, StateCleaned},
		{StateInterrupted, StateCleaned},
	}
	for _, pair := range ok {
		if !CanTransition(pair[0], pair[1]) {
			t.Errorf("CanTransition(%s,%s) = false", pair[0], pair[1])
		}
	}
	bad := [][2]State{
		{StatePlanned, StateComplete},
		{StateComplete, StatePlanned},
		{StateCleaned, StateFailed},
		{StatePlanned, StatePlanned},
	}
	for _, pair := range bad {
		if CanTransition(pair[0], pair[1]) {
			t.Errorf("CanTransition(%s,%s) = true", pair[0], pair[1])
		}
	}
}

func TestRunOperationHappyAndReplay(t *testing.T) {
	o, runID := newOrch(t)
	inv := &fakeInvoker{}
	op, err := o.RunOperation(context.Background(), runID, req(protocol.ActionInspect, "op-inspect", ""), inv, nil)
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != OpCompleted {
		t.Fatalf("op status = %s", op.Status)
	}
	st, _ := o.Store.Load(runID)
	if st.State != StateQualified {
		t.Fatalf("state = %s, want qualified", st.State)
	}
	// Replay with identical input returns the stored record without invoking.
	replay, err := o.RunOperation(context.Background(), runID, req(protocol.ActionInspect, "op-inspect", ""), inv, nil)
	if err != nil {
		t.Fatal(err)
	}
	if replay.OperationID != op.OperationID {
		t.Fatal("replay returned a different operation")
	}
	if inv.calls != 1 {
		t.Fatalf("invoker called %d times, want 1", inv.calls)
	}
}

func TestRunOperationConflict(t *testing.T) {
	o, runID := newOrch(t)
	inv := &fakeInvoker{}
	if _, err := o.RunOperation(context.Background(), runID, req(protocol.ActionInspect, "op-1", ""), inv, nil); err != nil {
		t.Fatal(err)
	}
	// Same operation ID with a different action changes the input digest.
	_, err := o.RunOperation(context.Background(), runID, req(protocol.ActionStatus, "op-1", ""), inv, nil)
	if !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("err = %v, want ErrOperationConflict", err)
	}
}

func TestRunOperationInvalidTransition(t *testing.T) {
	o, runID := newOrch(t)
	inv := &fakeInvoker{}
	_, err := o.RunOperation(context.Background(), runID, req(protocol.ActionPrepare, "op-prep", ""), inv, nil)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
}

func TestRunBlockUnsupportedFailsRun(t *testing.T) {
	o, runID := newOrch(t)
	done := &fakeInvoker{}
	if _, err := o.RunOperation(context.Background(), runID, req(protocol.ActionInspect, "op-i", ""), done, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := o.RunOperation(context.Background(), runID, req(protocol.ActionPrepare, "op-p", ""), done, nil); err != nil {
		t.Fatal(err)
	}
	unsupported := &fakeInvoker{
		terminal: protocol.Event{Kind: protocol.KindResult, Status: protocol.StatusUnsupported, Message: "not implemented"},
		err:      errors.New("unsupported"),
	}
	_, err := o.RunOperation(context.Background(), runID, req(protocol.ActionRunBlock, "op-rb", "isolated-1s-01"), unsupported, nil)
	if err == nil {
		t.Fatal("run-block unsupported did not return an error")
	}
	st, _ := o.Store.Load(runID)
	if st.State != StateFailed {
		t.Fatalf("state = %s, want failed", st.State)
	}
}

func TestReconcileBootInvalidatesActiveRun(t *testing.T) {
	o, runID := newOrch(t)
	done := &fakeInvoker{}
	_, _ = o.RunOperation(context.Background(), runID, req(protocol.ActionInspect, "op-i", ""), done, nil)
	_, _ = o.RunOperation(context.Background(), runID, req(protocol.ActionPrepare, "op-p", ""), done, nil)
	st, _ := o.Store.Load(runID)
	st.State = StateRunning
	st.Operations["op-live"] = Operation{OperationID: "op-live", Action: protocol.ActionRunBlock, Status: OpStarted}
	if err := o.Store.Save(st); err != nil {
		t.Fatal(err)
	}
	got, err := o.ReconcileBoot(st, "boot-2")
	if !errors.Is(err, ErrBootIDMismatch) {
		t.Fatalf("err = %v, want ErrBootIDMismatch", err)
	}
	if got.State != StateInterrupted {
		t.Fatalf("state = %s, want interrupted", got.State)
	}
	if got.Operations["op-live"].Status != OpInterrupted {
		t.Fatal("live operation was not marked interrupted")
	}
}

func TestMarkInterrupted(t *testing.T) {
	o, runID := newOrch(t)
	done := &fakeInvoker{}
	_, _ = o.RunOperation(context.Background(), runID, req(protocol.ActionInspect, "op-i", ""), done, nil)
	st, _ := o.Store.Load(runID)
	st.Operations["op-x"] = Operation{OperationID: "op-x", Status: OpStarted}
	got, err := o.MarkInterrupted(&st, "op-x", "ssh disconnect")
	if err != nil {
		t.Fatal(err)
	}
	if got.Operations["op-x"].Status != OpInterrupted {
		t.Fatal("operation not interrupted")
	}
}

func TestFileStoreMissingRun(t *testing.T) {
	store := FileStore{Root: t.TempDir()}
	_, err := store.Load("iris-20260925-zzzzzzzz")
	if !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("err = %v, want ErrRunNotFound", err)
	}
	if _, err := store.Load("../escape"); err == nil {
		t.Fatal("FileStore accepted a traversal run id")
	}
}

func TestInputDigestStable(t *testing.T) {
	a, _ := InputDigest(req(protocol.ActionInspect, "op-1", ""))
	b, _ := InputDigest(req(protocol.ActionInspect, "op-1", ""))
	if a != b || len(a) != 64 || !strings.HasPrefix(a, "") {
		t.Fatalf("digest unstable or wrong length: %s %s", a, b)
	}
}
