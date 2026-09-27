package runplan

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func validInput() BuildInput {
	return BuildInput{
		ToolVersion: "0.1.0",
		RunID:       "iris-20260925-abcdefgh",
		Seed:        42,
		Target:      Target{SSHAlias: "codedang8"},
		Suite:       "judger",
		Profile:     "isolated-1s",
		Fixtures: []Fixture{
			{Name: "cpp", Path: "fixtures/input.txt", SHA256: strings.Repeat("a", 64), ExpectedOutputPath: "fixtures/output.txt", ExpectedOutputSHA256: strings.Repeat("d", 64)},
		},
		Images: map[string]Image{
			"iris": {Reference: DefaultIrisImage, Digest: "sha256:" + strings.Repeat("b", 64)},
		},
		JudgerDigest:   "sha256:" + strings.Repeat("c", 64),
		WorkloadBinary: StagedFile{Path: "/tmp/workload", SHA256: strings.Repeat("e", 64)},
		Blocks: []Block{
			{ID: "isolated-1s-01", Suite: "judger", Profile: "isolated-1s", Workers: 1, Repetitions: 5},
		},
		Qualification: Qualification{MaxLoad1: 1.0},
	}
}

func TestBuildAndDigestDeterministic(t *testing.T) {
	in := validInput()
	p1, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	d1, err := p1.Digest()
	if err != nil {
		t.Fatal(err)
	}
	d2, _ := p2.Digest()
	if d1 != d2 {
		t.Fatalf("digest not deterministic: %s != %s", d1, d2)
	}
	if len(d1) != 64 {
		t.Fatalf("digest length = %d, want 64", len(d1))
	}

	in.Seed = 43
	p3, _ := Build(in)
	d3, _ := p3.Digest()
	if d3 == d1 {
		t.Fatal("digest did not change when the plan changed")
	}
}

func TestBuildDefaultsIrisImage(t *testing.T) {
	in := validInput()
	in.Images = nil
	p, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if p.Images["iris"].Reference != DefaultIrisImage {
		t.Fatalf("iris reference = %q, want default", p.Images["iris"].Reference)
	}
}

func TestValidateRunnable(t *testing.T) {
	in := validInput()
	in.Images = map[string]Image{"iris": {Reference: DefaultIrisImage}}
	p, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ValidateRunnable(); err == nil {
		t.Fatal("ValidateRunnable accepted an unresolved image")
	}
	in.Images = map[string]Image{"iris": {Reference: DefaultIrisImage, Digest: "sha256:" + strings.Repeat("b", 64)}}
	in.JudgerDigest = ""
	p2, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := p2.ValidateRunnable(); err == nil {
		t.Fatal("ValidateRunnable accepted a missing judger digest")
	}
}

func TestProductionCompatRequiresSealedLocalBenchmarkImage(t *testing.T) {
	in := validInput()
	in.ContainmentMode = ContainmentProductionCompat
	if _, err := Build(in); err == nil || !strings.Contains(err.Error(), "requires a judger-bench OCI image") {
		t.Fatalf("Build error = %v", err)
	}
	imageID := "sha256:" + strings.Repeat("f", 64)
	in.Images["judger-bench"] = Image{Reference: imageID, Digest: imageID}
	if _, err := Build(in); err != nil {
		t.Fatalf("Build with local image ID: %v", err)
	}
	in.Images["judger-bench"] = Image{Reference: "judger-bench:latest", Digest: imageID}
	if _, err := Build(in); err == nil {
		t.Fatal("Build accepted a mutable benchmark image reference")
	}
}

func TestBuildRejectsInvalidQualificationLoad(t *testing.T) {
	in := validInput()
	in.Qualification.MaxLoad1 = 0
	if _, err := Build(in); err == nil || !strings.Contains(err.Error(), "maxLoad1") {
		t.Fatalf("Build error = %v, want maxLoad1 validation", err)
	}
}

func TestValidImageDigest(t *testing.T) {
	if !ValidImageDigest("sha256:" + strings.Repeat("a", 64)) {
		t.Fatal("rejected valid digest")
	}
	for _, bad := range []string{"", "sha256:short", strings.Repeat("a", 64), "sha256:" + strings.Repeat("A", 64)} {
		if ValidImageDigest(bad) {
			t.Errorf("ValidImageDigest(%q) = true", bad)
		}
	}
}

func TestGenerateRunID(t *testing.T) {
	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	id, err := GenerateRunID(at, bytes.NewReader(bytes.Repeat([]byte{0}, 5)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "iris-20260925-") {
		t.Fatalf("id = %q, want date prefix", id)
	}
	if id2, _ := GenerateRunID(at, bytes.NewReader(bytes.Repeat([]byte{0}, 5))); id2 != id {
		t.Fatalf("generated ids differ: %s != %s", id, id2)
	}
}

type fakeRunner struct {
	out []byte
	err error
}

func (f fakeRunner) Output(_ context.Context, _ string, _ ...string) ([]byte, error) {
	return f.out, f.err
}

func TestDockerResolver(t *testing.T) {
	digest := "sha256:" + strings.Repeat("d", 64)
	r := DockerResolver{Runner: fakeRunner{out: []byte(digest + "\n")}}
	got, err := r.Resolve(context.Background(), DefaultIrisImage)
	if err != nil {
		t.Fatal(err)
	}
	if got != digest {
		t.Fatalf("resolved = %q, want %q", got, digest)
	}
	// An already-pinned reference bypasses the runner.
	got, err = r.Resolve(context.Background(), "img@"+digest)
	if err != nil || got != digest {
		t.Fatalf("pinned resolve = %q, %v", got, err)
	}
	// Unexpected output is rejected.
	bad := DockerResolver{Runner: fakeRunner{out: []byte("latest\n")}}
	if _, err := bad.Resolve(context.Background(), "img:tag"); err == nil {
		t.Fatal("resolver accepted a non-digest output")
	}
}

func TestStaticResolver(t *testing.T) {
	digest := "sha256:" + strings.Repeat("e", 64)
	s := StaticResolver{DefaultIrisImage: digest}
	if got, err := s.Resolve(context.Background(), DefaultIrisImage); err != nil || got != digest {
		t.Fatalf("static resolve = %q, %v", got, err)
	}
	if _, err := s.Resolve(context.Background(), "other:tag"); err == nil {
		t.Fatal("static resolver accepted an unknown reference")
	}
}
