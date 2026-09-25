// Package orchestrator owns the persisted run state machine and operation
// idempotency. It is deliberately independent of SSH and experiment logic so
// the transitions can be unit tested with a fake invoker.
package orchestrator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/skkuding/iris-load-test-toolset/internal/artifact"
	"github.com/skkuding/iris-load-test-toolset/internal/protocol"
)

// SchemaVersion is the persisted state schema.
const SchemaVersion = 1

// State is a run state machine value.
type State string

// Run states.
const (
	StatePlanned     State = "planned"
	StateQualified   State = "qualified"
	StatePrepared    State = "prepared"
	StateRunning     State = "running"
	StateValidating  State = "validating"
	StateComplete    State = "complete"
	StateCollected   State = "collected"
	StateCleaned     State = "cleaned"
	StateFailed      State = "failed"
	StateInterrupted State = "interrupted"
)

// Operation statuses.
const (
	OpStarted     = "started"
	OpCompleted   = "completed"
	OpFailed      = "failed"
	OpInterrupted = "interrupted"
)

var transitions = map[State]map[State]bool{
	StatePlanned:     {StateQualified: true, StateFailed: true, StateInterrupted: true},
	StateQualified:   {StatePrepared: true, StateFailed: true, StateInterrupted: true},
	StatePrepared:    {StateRunning: true, StateFailed: true, StateInterrupted: true},
	StateRunning:     {StateValidating: true, StateFailed: true, StateInterrupted: true},
	StateValidating:  {StateComplete: true, StateFailed: true, StateInterrupted: true},
	StateComplete:    {StateCollected: true, StateCleaned: true},
	StateCollected:   {StateCleaned: true},
	StateFailed:      {StateCleaned: true},
	StateInterrupted: {StateCleaned: true},
	StateCleaned:     {},
}

// CanTransition reports whether from->to is an allowed state transition.
func CanTransition(from, to State) bool {
	if from == to {
		return false
	}
	return transitions[from][to]
}

// Active reports whether the state is mid-run.
func (s State) Active() bool {
	switch s {
	case StatePrepared, StateRunning, StateValidating:
		return true
	}
	return false
}

// Operation is one idempotent agent invocation record.
type Operation struct {
	OperationID   string          `json:"operationId"`
	Action        protocol.Action `json:"action"`
	InputSHA256   string          `json:"inputSha256"`
	PlanSHA256    string          `json:"planSha256"`
	BlockID       string          `json:"blockId,omitempty"`
	Status        string          `json:"status"`
	ReceiptSHA256 string          `json:"receiptSha256,omitempty"`
	Message       string          `json:"message,omitempty"`
	StartedAt     time.Time       `json:"startedAt"`
	EndedAt       time.Time       `json:"endedAt,omitempty"`
}

// Terminal reports whether the operation has a final status.
func (o Operation) Terminal() bool {
	return o.Status == OpCompleted || o.Status == OpFailed || o.Status == OpInterrupted
}

// RunState is the persisted state for one run.
type RunState struct {
	SchemaVersion int                  `json:"schemaVersion"`
	RunID         string               `json:"runId"`
	PlanSHA256    string               `json:"planSha256"`
	State         State                `json:"state"`
	BootID        string               `json:"bootId,omitempty"`
	Operations    map[string]Operation `json:"operations"`
	UpdatedAt     time.Time            `json:"updatedAt"`
}

// Errors surfaced by the orchestrator.
var (
	ErrRunNotFound         = errors.New("orchestrator: run not found")
	ErrOperationConflict   = errors.New("orchestrator: operation conflicts with existing input")
	ErrOperationInProgress = errors.New("orchestrator: operation already in progress")
	ErrInvalidTransition   = errors.New("orchestrator: invalid state transition")
	ErrPlanMismatch        = errors.New("orchestrator: plan digest mismatch")
	ErrBootIDMismatch      = errors.New("orchestrator: boot id mismatch")
)

// StateStore persists RunState.
type StateStore interface {
	Load(runID string) (RunState, error)
	Save(RunState) error
}

// FileStore stores state below root/<runID>/state.json.
type FileStore struct {
	Root string
}

// Load implements StateStore.
func (s FileStore) Load(runID string) (RunState, error) {
	if !protocol.ValidRunID(runID) {
		return RunState{}, fmt.Errorf("orchestrator: invalid run id %q", runID)
	}
	data, err := os.ReadFile(s.path(runID))
	if errors.Is(err, os.ErrNotExist) {
		return RunState{}, ErrRunNotFound
	}
	if err != nil {
		return RunState{}, err
	}
	var st RunState
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return RunState{}, err
	}
	if st.RunID != runID {
		return RunState{}, fmt.Errorf("orchestrator: state run id %q does not match %q", st.RunID, runID)
	}
	return st, nil
}

// Save implements StateStore.
func (s FileStore) Save(st RunState) error {
	if !protocol.ValidRunID(st.RunID) {
		return fmt.Errorf("orchestrator: invalid run id %q", st.RunID)
	}
	if st.Operations == nil {
		st.Operations = map[string]Operation{}
	}
	if st.SchemaVersion == 0 {
		st.SchemaVersion = SchemaVersion
	}
	return artifact.WriteJSONAtomic(s.path(st.RunID), st, 0o644)
}

func (s FileStore) path(runID string) string {
	return filepath.Join(s.Root, runID, "state.json")
}

// Orchestrator coordinates state and operation records.
type Orchestrator struct {
	Store StateStore
	Now   func() time.Time
}

// New returns an orchestrator with a wall clock.
func New(store StateStore) *Orchestrator {
	return &Orchestrator{Store: store, Now: time.Now}
}

// InitState creates a new planned state for runID.
func (o *Orchestrator) InitState(runID, planSHA256, bootID string) (RunState, error) {
	if !protocol.ValidRunID(runID) {
		return RunState{}, fmt.Errorf("orchestrator: invalid run id %q", runID)
	}
	if !artifact.ValidSHA256(planSHA256) {
		return RunState{}, errors.New("orchestrator: invalid plan digest")
	}
	st := RunState{
		SchemaVersion: SchemaVersion,
		RunID:         runID,
		PlanSHA256:    planSHA256,
		State:         StatePlanned,
		BootID:        bootID,
		Operations:    map[string]Operation{},
		UpdatedAt:     o.now(),
	}
	return st, o.Store.Save(st)
}

// InputDigest returns the SHA-256 over the canonical request JSON.
func InputDigest(req protocol.Request) (string, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// AllowedStates lists the states in which action may begin.
func AllowedStates(a protocol.Action) []State {
	switch a {
	case protocol.ActionInspect:
		return []State{StatePlanned}
	case protocol.ActionPrepare:
		return []State{StateQualified}
	case protocol.ActionRunBlock:
		return []State{StatePrepared}
	case protocol.ActionValidate:
		return []State{StateRunning, StateValidating}
	case protocol.ActionBundle:
		return []State{StateValidating, StateComplete}
	case protocol.ActionCleanup:
		return []State{StateComplete, StateCollected, StateFailed, StateInterrupted}
	case protocol.ActionStatus:
		return nil
	}
	return nil
}

func allowedIn(a protocol.Action, s State) bool {
	states := AllowedStates(a)
	if len(states) == 0 {
		return true
	}
	for _, want := range states {
		if want == s {
			return true
		}
	}
	return false
}

// Invoker runs one protocol request and returns the terminal event.
type Invoker interface {
	Invoke(ctx context.Context, req protocol.Request, sink func(protocol.Event) error) (protocol.Event, error)
}

// RunOperation validates, records, and invokes one agent operation.
//
// Replaying a completed operation with identical input returns the stored
// record without invoking the agent. Replaying with different input fails.
// run-block is moved to running before invocation and to validating after.
func (o *Orchestrator) RunOperation(ctx context.Context, runID string, req protocol.Request, invoker Invoker, sink func(protocol.Event) error) (Operation, error) {
	if err := req.Validate(); err != nil {
		return Operation{}, err
	}
	if runID != req.RunID {
		return Operation{}, fmt.Errorf("orchestrator: request run id %q does not match %q", req.RunID, runID)
	}
	st, err := o.Store.Load(runID)
	if err != nil {
		return Operation{}, err
	}
	if st.PlanSHA256 != req.PlanSHA256 {
		return Operation{}, ErrPlanMismatch
	}
	input, err := InputDigest(req)
	if err != nil {
		return Operation{}, err
	}
	if existing, ok := st.Operations[req.OperationID]; ok {
		if existing.InputSHA256 != input || existing.PlanSHA256 != req.PlanSHA256 {
			return Operation{}, ErrOperationConflict
		}
		switch existing.Status {
		case OpCompleted, OpFailed, OpInterrupted:
			return existing, nil
		default:
			return existing, ErrOperationInProgress
		}
	}
	if st.State == StateCleaned {
		return Operation{}, fmt.Errorf("%w: run is cleaned", ErrInvalidTransition)
	}
	if !allowedIn(req.Action, st.State) {
		return Operation{}, fmt.Errorf("%w: %s from %s", ErrInvalidTransition, req.Action, st.State)
	}

	op := Operation{
		OperationID: req.OperationID,
		Action:      req.Action,
		InputSHA256: input,
		PlanSHA256:  req.PlanSHA256,
		BlockID:     req.BlockID,
		Status:      OpStarted,
		StartedAt:   o.now(),
	}
	st.Operations[req.OperationID] = op
	st.UpdatedAt = o.now()
	if req.Action == protocol.ActionRunBlock {
		if err := o.transition(&st, StateRunning); err != nil {
			return Operation{}, err
		}
	}
	if err := o.Store.Save(st); err != nil {
		return Operation{}, err
	}

	terminal, invokeErr := invoker.Invoke(ctx, req, sink)
	op.EndedAt = o.now()
	switch terminal.Status {
	case protocol.StatusCompleted:
		op.Status = OpCompleted
		op.ReceiptSHA256 = terminal.ReceiptSHA256
	case protocol.StatusFailed, protocol.StatusUnsupported:
		op.Status = OpFailed
		op.Message = terminal.Message
	default:
		if invokeErr != nil {
			op.Status = OpFailed
			op.Message = invokeErr.Error()
		} else {
			op.Status = OpFailed
			op.Message = fmt.Sprintf("unexpected terminal status %q", terminal.Status)
		}
	}
	// Reload in case events were persisted out of band, then commit the record.
	if cur, lerr := o.Store.Load(runID); lerr == nil {
		st = cur
	}
	st.Operations[req.OperationID] = op
	st.UpdatedAt = o.now()
	o.applyOutcome(&st, req.Action, op.Status)
	if saveErr := o.Store.Save(st); saveErr != nil {
		return op, saveErr
	}
	if op.Status != OpCompleted {
		if invokeErr != nil {
			return op, invokeErr
		}
		return op, fmt.Errorf("orchestrator: operation %s %s: %s", req.OperationID, op.Status, op.Message)
	}
	return op, nil
}

func (o *Orchestrator) applyOutcome(st *RunState, action protocol.Action, status string) {
	if status != OpCompleted {
		switch st.State {
		case StateCleaned, StateFailed, StateInterrupted:
			return
		}
		_ = o.transition(st, StateFailed)
		return
	}
	switch action {
	case protocol.ActionInspect:
		_ = o.transition(st, StateQualified)
	case protocol.ActionPrepare:
		_ = o.transition(st, StatePrepared)
	case protocol.ActionRunBlock:
		_ = o.transition(st, StateValidating)
	case protocol.ActionValidate:
		_ = o.transition(st, StateComplete)
	case protocol.ActionBundle:
		_ = o.transition(st, StateCollected)
	case protocol.ActionCleanup:
		_ = o.transition(st, StateCleaned)
	}
}

func (o *Orchestrator) transition(st *RunState, to State) error {
	if !CanTransition(st.State, to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, st.State, to)
	}
	st.State = to
	st.UpdatedAt = o.now()
	return nil
}

// MarkInterrupted closes an in-progress operation as interrupted and moves the
// run to interrupted when the boot ID changed or the block cannot resume.
func (o *Orchestrator) MarkInterrupted(st *RunState, operationID, reason string) (RunState, error) {
	op, ok := st.Operations[operationID]
	if !ok {
		return *st, fmt.Errorf("orchestrator: unknown operation %q", operationID)
	}
	if !op.Terminal() {
		op.Status = OpInterrupted
		op.Message = reason
		op.EndedAt = o.now()
		st.Operations[operationID] = op
	}
	switch st.State {
	case StateCleaned, StateFailed, StateInterrupted:
	default:
		if err := o.transition(st, StateInterrupted); err != nil {
			return *st, err
		}
	}
	st.UpdatedAt = o.now()
	return *st, o.Store.Save(*st)
}

// ReconcileBoot invalidates an active run when the host boot ID changed. It
// returns the (possibly updated) state and ErrBootIDMismatch when a change was
// detected.
func (o *Orchestrator) ReconcileBoot(st RunState, bootID string) (RunState, error) {
	if bootID == "" || st.BootID == "" || st.BootID == bootID {
		return st, nil
	}
	if !st.State.Active() && st.State != StateQualified {
		return st, nil
	}
	for id, op := range st.Operations {
		if !op.Terminal() {
			op.Status = OpInterrupted
			op.Message = "host rebooted during operation"
			op.EndedAt = o.now()
			st.Operations[id] = op
		}
	}
	switch st.State {
	case StateCleaned, StateFailed, StateInterrupted:
	default:
		if err := o.transition(&st, StateInterrupted); err != nil {
			return st, err
		}
	}
	st.BootID = bootID
	st.UpdatedAt = o.now()
	if err := o.Store.Save(st); err != nil {
		return st, err
	}
	return st, ErrBootIDMismatch
}

func (o *Orchestrator) now() time.Time {
	if o.Now == nil {
		return time.Now()
	}
	return o.Now()
}
