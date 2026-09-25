package protocol

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func sampleRequest() Request {
	return Request{
		ProtocolVersion: Version,
		OperationID:     "01K3OP",
		RunID:           "iris-20260925-abcdefgh",
		Action:          ActionPrepare,
		PlanSHA256:      strings.Repeat("a", 64),
	}
}

func TestRequestValidate(t *testing.T) {
	if err := sampleRequest().Validate(); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	cases := map[string]func(*Request){
		"bad version":    func(r *Request) { r.ProtocolVersion = 99 },
		"bad operation":  func(r *Request) { r.OperationID = "has space" },
		"bad run id":     func(r *Request) { r.RunID = "UPPER" },
		"bad action":     func(r *Request) { r.Action = "exec" },
		"missing plan":   func(r *Request) { r.PlanSHA256 = "" },
		"bad plan":       func(r *Request) { r.PlanSHA256 = "xyz" },
		"runblock no id": func(r *Request) { r.Action = ActionRunBlock; r.BlockID = "" },
	}
	for name, mutate := range cases {
		r := sampleRequest()
		mutate(&r)
		if err := r.Validate(); err == nil {
			t.Errorf("%s: Validate = nil, want error", name)
		}
	}
}

func TestInspectStatusAllowEmptyPlan(t *testing.T) {
	for _, a := range []Action{ActionInspect, ActionStatus} {
		r := sampleRequest()
		r.Action = a
		r.PlanSHA256 = ""
		if err := r.Validate(); err != nil {
			t.Errorf("%s without plan rejected: %v", a, err)
		}
	}
}

func TestDecodeRequestStrict(t *testing.T) {
	good := `{"protocolVersion":1,"operationId":"op1","runId":"r-1","action":"inspect"}`
	if _, err := DecodeRequest(strings.NewReader(good)); err != nil {
		t.Fatalf("good request: %v", err)
	}
	unknown := `{"protocolVersion":1,"operationId":"op1","runId":"r-1","action":"inspect","extra":true}`
	if _, err := DecodeRequest(strings.NewReader(unknown)); err == nil {
		t.Fatal("DecodeRequest accepted an unknown field")
	}
	trailing := good + good
	if _, err := DecodeRequest(strings.NewReader(trailing)); err == nil {
		t.Fatal("DecodeRequest accepted trailing data")
	}
}

func TestEventValidation(t *testing.T) {
	if err := (Event{Seq: 1, Kind: KindPhase, Phase: "qualify", Status: StatusStarted}).Validate(); err != nil {
		t.Fatalf("valid phase: %v", err)
	}
	if err := (Event{Seq: 1, Kind: KindArtifact, Path: "/abs", SHA256: strings.Repeat("a", 64)}).Validate(); err == nil {
		t.Fatal("artifact event accepted absolute path")
	}
	if err := (Event{Seq: 1, Kind: KindResult, Status: StatusCompleted}).Validate(); err != nil {
		t.Fatalf("valid result: %v", err)
	}
	if err := (Event{Seq: 1, Kind: "bogus"}).Validate(); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestEventStreamRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEventEncoder(&buf)
	mustEmit(t, enc, Event{Kind: KindPhase, Phase: "qualify", Status: StatusStarted})
	mustEmit(t, enc, Event{Kind: KindCheck, Name: "cgroup-v2", Status: StatusPassed})
	mustEmit(t, enc, Event{Kind: KindArtifact, Path: "samples/judger.ndjson", SHA256: strings.Repeat("b", 64)})
	mustEmit(t, enc, Event{Kind: KindResult, Status: StatusCompleted, ReceiptSHA256: strings.Repeat("c", 64)})

	r := NewEventReader(&buf)
	count := 0
	for {
		_, err := r.Decode()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		count++
	}
	if count != 4 {
		t.Fatalf("decoded %d events, want 4", count)
	}
	if err := r.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if r.LastSeq() != 4 {
		t.Fatalf("LastSeq = %d, want 4", r.LastSeq())
	}
}

func TestEventReaderRejectsGapAndMissingTerminal(t *testing.T) {
	gap := `{"seq":1,"kind":"phase","phase":"a","status":"started"}
{"seq":3,"kind":"result","status":"completed"}
`
	r := NewEventReader(strings.NewReader(gap))
	if _, err := r.Decode(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Decode(); err == nil {
		t.Fatal("accepted a sequence gap")
	}

	noTerminal := `{"seq":1,"kind":"phase","phase":"a","status":"started"}
`
	r2 := NewEventReader(strings.NewReader(noTerminal))
	for {
		if _, err := r2.Decode(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if err := r2.Finish(); err == nil {
		t.Fatal("Finish accepted a stream without a terminal result")
	}
}

func TestEventReaderRejectsAfterTerminal(t *testing.T) {
	stream := `{"seq":1,"kind":"result","status":"completed"}
{"seq":2,"kind":"result","status":"completed"}
`
	r := NewEventReader(strings.NewReader(stream))
	if _, err := r.Decode(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Decode(); err == nil {
		t.Fatal("accepted an event after the terminal result")
	}
}

func TestValidRunID(t *testing.T) {
	for _, ok := range []string{"iris-20260925-abcdefgh", "r", "run-1"} {
		if !ValidRunID(ok) {
			t.Errorf("ValidRunID(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "-leading", "trailing-", "UP", "has space", "a/b"} {
		if ValidRunID(bad) {
			t.Errorf("ValidRunID(%q) = true", bad)
		}
	}
}

func mustEmit(t *testing.T, enc *EventEncoder, ev Event) {
	t.Helper()
	if err := enc.Emit(ev); err != nil {
		t.Fatal(err)
	}
}
