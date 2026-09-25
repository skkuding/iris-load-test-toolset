// Package protocol defines the versioned controller/agent wire format.
//
// Requests are single JSON objects on stdin. Events are newline-delimited JSON
// on stdout. Human progress belongs on stderr. The format is deliberately
// small and strict: bounded fields, a fixed action verb set, a monotonic
// sequence, and exactly one terminal result event.
package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"

	"github.com/skkuding/iris-load-test-toolset/internal/artifact"
)

// Version is the protocol major version this build speaks.
const Version = 1

// Field size bounds.
const (
	MaxIDLen      = 128
	MaxPathLen    = artifact.MaxPathLen
	MaxMessageLen = 4096
	MaxLineBytes  = 1 << 20
)

// Action is one of the fixed agent verbs.
type Action string

// The fixed agent verb set. The agent accepts no other action and no arbitrary
// command string.
const (
	ActionInspect  Action = "inspect"
	ActionPrepare  Action = "prepare"
	ActionRunBlock Action = "run-block"
	ActionValidate Action = "validate"
	ActionBundle   Action = "bundle"
	ActionCleanup  Action = "cleanup"
	ActionStatus   Action = "status"
)

// Actions lists the supported verbs in a stable order.
var Actions = []Action{
	ActionInspect,
	ActionPrepare,
	ActionRunBlock,
	ActionValidate,
	ActionBundle,
	ActionCleanup,
	ActionStatus,
}

var actionSet = func() map[Action]struct{} {
	m := make(map[Action]struct{}, len(Actions))
	for _, a := range Actions {
		m[a] = struct{}{}
	}
	return m
}()

// ValidAction reports whether a is in the fixed verb set.
func ValidAction(a Action) bool {
	_, ok := actionSet[a]
	return ok
}

// Request is the controller-to-agent envelope carried on stdin.
type Request struct {
	ProtocolVersion int    `json:"protocolVersion"`
	OperationID     string `json:"operationId"`
	RunID           string `json:"runId"`
	Action          Action `json:"action"`
	PlanSHA256      string `json:"planSha256"`
	BlockID         string `json:"blockId,omitempty"`
}

var (
	idPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	runIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,126}[a-z0-9]$|^[a-z0-9]$`)
)

// ValidRunID reports whether s is a safe run identifier: lowercase
// alphanumerics and hyphens, bounded length, no leading or trailing hyphen.
func ValidRunID(s string) bool {
	return runIDPattern.MatchString(s)
}

// ValidOperationID reports whether s is a safe opaque operation identifier.
func ValidOperationID(s string) bool {
	return idPattern.MatchString(s)
}

// Validate checks envelope version, identifiers, action, plan digest, and the
// block ID rule for run-block.
func (r Request) Validate() error {
	if r.ProtocolVersion != Version {
		return fmt.Errorf("protocol: unsupported version %d", r.ProtocolVersion)
	}
	if !ValidOperationID(r.OperationID) {
		return fmt.Errorf("protocol: invalid operationId")
	}
	if !ValidRunID(r.RunID) {
		return fmt.Errorf("protocol: invalid runId")
	}
	if !ValidAction(r.Action) {
		return fmt.Errorf("protocol: unsupported action %q", r.Action)
	}
	if r.Action == ActionRunBlock {
		if !ValidOperationID(r.BlockID) {
			return fmt.Errorf("protocol: run-block requires a valid blockId")
		}
	} else if r.BlockID != "" && !ValidOperationID(r.BlockID) {
		return fmt.Errorf("protocol: invalid blockId")
	}
	if r.PlanSHA256 != "" && !artifact.ValidSHA256(r.PlanSHA256) {
		return fmt.Errorf("protocol: invalid planSha256")
	}
	// Read-only verbs may run before a plan is sealed.
	if r.Action != ActionInspect && r.Action != ActionStatus && r.PlanSHA256 == "" {
		return fmt.Errorf("protocol: planSha256 is required for %s", r.Action)
	}
	return nil
}

// DecodeRequest strictly decodes a single JSON request object, rejecting
// unknown fields, trailing data, and oversized input.
func DecodeRequest(r io.Reader) (Request, error) {
	var req Request
	data, err := io.ReadAll(io.LimitReader(r, MaxLineBytes))
	if err != nil {
		return Request{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return Request{}, fmt.Errorf("protocol: decode request: %w", err)
	}
	if dec.More() {
		return Request{}, errors.New("protocol: trailing data after request")
	}
	if err := req.Validate(); err != nil {
		return Request{}, err
	}
	return req, nil
}

// EncodeRequest writes req as JSON followed by a newline.
func EncodeRequest(w io.Writer, req Request) error {
	if err := req.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}

// EventKind classifies an agent event.
type EventKind string

// Event kinds.
const (
	KindPhase    EventKind = "phase"
	KindCheck    EventKind = "check"
	KindArtifact EventKind = "artifact"
	KindResult   EventKind = "result"
	KindLog      EventKind = "log"
)

// Event statuses.
const (
	StatusStarted     = "started"
	StatusPassed      = "passed"
	StatusFailed      = "failed"
	StatusCompleted   = "completed"
	StatusInterrupted = "interrupted"
	StatusUnsupported = "unsupported"
)

// Event is one agent-to-controller NDJSON record. All fields are bounded.
type Event struct {
	Seq           int64     `json:"seq"`
	Kind          EventKind `json:"kind"`
	Phase         string    `json:"phase,omitempty"`
	Name          string    `json:"name,omitempty"`
	Status        string    `json:"status,omitempty"`
	Path          string    `json:"path,omitempty"`
	SHA256        string    `json:"sha256,omitempty"`
	ReceiptSHA256 string    `json:"receiptSha256,omitempty"`
	Message       string    `json:"message,omitempty"`
}

// Validate checks kind-specific required fields, size bounds, and safe paths.
func (e Event) Validate() error {
	if e.Seq <= 0 {
		return fmt.Errorf("protocol: event seq must be positive")
	}
	switch e.Kind {
	case KindPhase:
		if e.Phase == "" || !ValidOperationID(e.Phase) {
			return fmt.Errorf("protocol: phase event requires a valid phase")
		}
		if !validShort(e.Status) {
			return fmt.Errorf("protocol: phase event requires a valid status")
		}
	case KindCheck:
		if e.Name == "" || !ValidOperationID(e.Name) {
			return fmt.Errorf("protocol: check event requires a valid name")
		}
		if !validShort(e.Status) {
			return fmt.Errorf("protocol: check event requires a valid status")
		}
	case KindArtifact:
		if err := artifact.ValidateRelativePath(e.Path); err != nil {
			return fmt.Errorf("protocol: artifact event: %w", err)
		}
		if !artifact.ValidSHA256(e.SHA256) {
			return fmt.Errorf("protocol: artifact event requires a valid sha256")
		}
	case KindResult:
		if !validShort(e.Status) {
			return fmt.Errorf("protocol: result event requires a valid status")
		}
		if e.ReceiptSHA256 != "" && !artifact.ValidSHA256(e.ReceiptSHA256) {
			return fmt.Errorf("protocol: result event has invalid receiptSha256")
		}
	case KindLog:
	default:
		return fmt.Errorf("protocol: unknown event kind %q", e.Kind)
	}
	if len(e.Message) > MaxMessageLen {
		return fmt.Errorf("protocol: event message exceeds %d bytes", MaxMessageLen)
	}
	return nil
}

func validShort(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// EventEncoder writes validated NDJSON events.
type EventEncoder struct {
	w   io.Writer
	seq int64
}

// NewEventEncoder returns an encoder that assigns sequence numbers.
func NewEventEncoder(w io.Writer) *EventEncoder {
	return &EventEncoder{w: w}
}

// Emit validates ev, assigns the next sequence number, and writes one line.
func (enc *EventEncoder) Emit(ev Event) error {
	enc.seq++
	ev.Seq = enc.seq
	if err := ev.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = enc.w.Write(data)
	return err
}

// Seq returns the last sequence number emitted.
func (enc *EventEncoder) Seq() int64 { return enc.seq }

// EventReader validates the NDJSON event stream from an agent: consecutive
// sequence numbers starting at one, exactly one terminal result, and no events
// after the terminal result.
type EventReader struct {
	scanner  *bufio.Scanner
	last     int64
	terminal bool
}

// NewEventReader wraps r with a bounded line scanner.
func NewEventReader(r io.Reader) *EventReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), MaxLineBytes)
	return &EventReader{scanner: sc}
}

// Decode reads and validates the next event. io.EOF signals a clean end of
// stream; call Finish afterward to require a terminal result.
func (r *EventReader) Decode() (Event, error) {
	if r.terminal {
		// Anything after a terminal result, including EOF, is rejected.
		if r.scanner.Scan() {
			return Event{}, errors.New("protocol: event after terminal result")
		}
		if err := r.scanner.Err(); err != nil {
			return Event{}, err
		}
		return Event{}, io.EOF
	}
	if !r.scanner.Scan() {
		if err := r.scanner.Err(); err != nil {
			return Event{}, err
		}
		return Event{}, io.EOF
	}
	line := bytes.TrimSpace(r.scanner.Bytes())
	if len(line) == 0 {
		return Event{}, errors.New("protocol: empty event line")
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	var ev Event
	if err := dec.Decode(&ev); err != nil {
		return Event{}, fmt.Errorf("protocol: decode event: %w", err)
	}
	if err := ev.Validate(); err != nil {
		return Event{}, err
	}
	if ev.Seq != r.last+1 {
		return Event{}, fmt.Errorf("protocol: event seq %d, want %d", ev.Seq, r.last+1)
	}
	r.last = ev.Seq
	if ev.Kind == KindResult {
		r.terminal = true
	}
	return ev, nil
}

// Finish requires that a terminal result was seen and there is no trailing
// non-empty input.
func (r *EventReader) Finish() error {
	for r.scanner.Scan() {
		if len(bytes.TrimSpace(r.scanner.Bytes())) != 0 {
			return errors.New("protocol: trailing event data after terminal result")
		}
	}
	if err := r.scanner.Err(); err != nil {
		return err
	}
	if !r.terminal {
		return errors.New("protocol: stream ended without a terminal result")
	}
	return nil
}

// LastSeq returns the last accepted sequence number.
func (r *EventReader) LastSeq() int64 { return r.last }
