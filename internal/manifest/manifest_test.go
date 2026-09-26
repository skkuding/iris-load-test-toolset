package manifest

import (
	"strings"
	"testing"
	"time"

	"github.com/skkuding/iris-load-test-toolset/internal/protocol"
	"github.com/skkuding/iris-load-test-toolset/internal/runplan"
)

func samplePlan(t *testing.T) (runplan.Plan, string) {
	t.Helper()
	p, err := runplan.Build(runplan.BuildInput{
		ToolVersion: "0.1.0",
		RunID:       "iris-20260925-abcdefgh",
		Target:      runplan.Target{SSHAlias: "codedang8"},
		Suite:       "judger",
		Profile:     "isolated-1s",
		Images: map[string]runplan.Image{
			"iris": {Reference: runplan.DefaultIrisImage, Digest: "sha256:" + strings.Repeat("b", 64)},
		},
		JudgerDigest:   "sha256:" + strings.Repeat("c", 64),
		WorkloadBinary: runplan.StagedFile{Path: "/tmp/workload", SHA256: strings.Repeat("e", 64)},
		Qualification:  runplan.Qualification{MaxLoad1: 1.0, Report: &runplan.StagedFile{Path: "/tmp/qualification.json", SHA256: strings.Repeat("f", 64)}},
		Blocks:         []runplan.Block{{ID: "isolated-1s-01", Suite: "judger", Profile: "isolated-1s", Workers: 1, Repetitions: 5}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sha, err := p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return p, sha
}

func TestBuildAndValidate(t *testing.T) {
	p, sha := samplePlan(t)
	m, err := Build(p, sha, "0.1.0", "deadbeef", false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !m.Comparable {
		t.Fatal("fresh manifest should be comparable")
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestFailedOutcomeMakesNonComparable(t *testing.T) {
	p, sha := samplePlan(t)
	m, _ := Build(p, sha, "0.1.0", "", false, time.Now())
	m.AddOutcome("cgroup-containment", protocol.StatusPassed, "")
	if !m.Comparable {
		t.Fatal("passed outcome made run non-comparable")
	}
	m.AddOutcome("cgroup-containment", protocol.StatusFailed, "escape detected")
	if m.Comparable {
		t.Fatal("failed outcome did not make run non-comparable")
	}
}

func TestOverridesRequireNonComparable(t *testing.T) {
	p, sha := samplePlan(t)
	m, _ := Build(p, sha, "0.1.0", "", false, time.Now())
	m.MarkOverride("diagnostic-only")
	if m.Comparable {
		t.Fatal("override did not force non-comparable")
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	m.Comparable = true
	if err := m.Validate(); err == nil {
		t.Fatal("Validate accepted overrides with comparable=true")
	}
}

func TestProductionCompatManifestIsNonComparable(t *testing.T) {
	p, _ := samplePlan(t)
	p.ContainmentMode = runplan.ContainmentProductionCompat
	imageID := "sha256:" + strings.Repeat("f", 64)
	p.Images["judger-bench"] = runplan.Image{Reference: imageID, Digest: imageID}
	sha, err := p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	m, err := Build(p, sha, "0.1.0", "", false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if m.Comparable || len(m.Overrides) != 1 {
		t.Fatalf("production compatibility manifest = %+v", m)
	}
}

func TestMissingQualificationReportIsNonComparable(t *testing.T) {
	p, _ := samplePlan(t)
	p.Qualification.Report = nil
	sha, _ := p.Digest()
	m, err := Build(p, sha, "0.1.0", "", false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if m.Comparable || len(m.Overrides) == 0 {
		t.Fatalf("manifest without qualification report = %+v", m)
	}
}

func TestSecretsAbsent(t *testing.T) {
	p, sha := samplePlan(t)
	m, _ := Build(p, sha, "0.1.0", "", false, time.Now())
	m.HostFacts = map[string]string{"dbUrl": "postgres://u:s3cret@host/db"}
	if err := m.SecretsAbsent([]string{"s3cret"}); err == nil {
		t.Fatal("SecretsAbsent missed a secret in host facts")
	}
	if err := m.SecretsAbsent([]string{"other"}); err != nil {
		t.Fatalf("SecretsAbsent false positive: %v", err)
	}
}

func TestBuildRejectsBadDigest(t *testing.T) {
	p, _ := samplePlan(t)
	if _, err := Build(p, "not-a-digest", "0.1.0", "", false, time.Now()); err == nil {
		t.Fatal("Build accepted an invalid plan digest")
	}
}
