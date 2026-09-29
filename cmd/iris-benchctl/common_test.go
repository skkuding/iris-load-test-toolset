package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skkuding/iris-load-test-toolset/internal/config"
)

const ctlConfig = `{
  "schemaVersion": 1,
  "iris": {"image": "ghcr.io/skkuding/codedang-iris:stage", "rabbitmqImage": "rabbitmq:3-management"},
  "profiles": {"isolated-1s": {"suite": "judger", "workers": 1, "repetitions": 5, "cpuList": "2-3"}},
  "hosts": [{"alias": "codedang8", "hostIdentity": "server8", "allow": true}]
}`

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Decode([]byte(ctlConfig))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func baseOptions() planOptions {
	return planOptions{
		host:            "codedang8",
		profile:         "isolated-1s",
		runID:           "iris-20260925-abcdefgh",
		irisDigest:      "sha256:" + strings.Repeat("b", 64),
		judgerDigest:    strings.Repeat("c", 64),
		benchBinary:     "/tmp/precompiled-workload",
		benchBinarySHA:  strings.Repeat("d", 64),
		seed:            7,
		fixtures:        stringList{"cpp-runtime-v1=../../fixtures/568/15850.in"},
		expectedOutputs: stringList{"cpp-runtime-v1=../../fixtures/568/15850.out"},
	}
}

func TestBuildPlanOffline(t *testing.T) {
	o := baseOptions()
	plan, sha, err := o.buildPlan(context.Background(), testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(sha) != 64 {
		t.Fatalf("plan sha = %q", sha)
	}
	if len(plan.Blocks) != 1 || plan.Blocks[0].ID != "isolated-1s-01" {
		t.Fatalf("blocks = %+v", plan.Blocks)
	}
	if plan.Images["iris"].Digest != "sha256:"+strings.Repeat("b", 64) {
		t.Fatalf("iris image = %+v", plan.Images["iris"])
	}
	if plan.JudgerDigest != "sha256:"+strings.Repeat("c", 64) {
		t.Fatalf("judger digest = %q", plan.JudgerDigest)
	}
	if plan.Qualification.MaxLoad1 != 1.0 {
		t.Fatalf("sealed maxLoad1 = %v, want 1.0", plan.Qualification.MaxLoad1)
	}
	if _, ok := plan.Images["rabbitmq"]; !ok {
		t.Fatal("rabbitmq image not carried into plan")
	}

	// The same inputs produce the same digest.
	o2 := baseOptions()
	_, sha2, err := o2.buildPlan(context.Background(), testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if sha != sha2 {
		t.Fatalf("digest not stable: %s != %s", sha, sha2)
	}
}

func TestBuildPlanRequiresResolvedDigest(t *testing.T) {
	o := baseOptions()
	o.irisDigest = ""
	_, _, err := o.buildPlan(context.Background(), testConfig(t))
	if err == nil || !strings.Contains(err.Error(), "resolved digest") {
		t.Fatalf("err = %v, want unresolved digest error", err)
	}
}

func TestBuildPlanRequiresJudgerDigest(t *testing.T) {
	o := baseOptions()
	o.judgerDigest = ""
	_, _, err := o.buildPlan(context.Background(), testConfig(t))
	if err == nil || !strings.Contains(err.Error(), "judger digest") {
		t.Fatalf("err = %v, want judger digest error", err)
	}
}

func TestBuildPlanSealsBenchmarkImage(t *testing.T) {
	o := baseOptions()
	o.benchmarkImage = "sha256:" + strings.Repeat("e", 64)
	plan, _, err := o.buildPlan(context.Background(), testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	image := plan.Images["judger-bench"]
	if image.Reference != o.benchmarkImage || image.Digest != o.benchmarkImage {
		t.Fatalf("sealed benchmark image = %+v", image)
	}
}

func TestBuildPlanBenchmarkImageMustBeExactLocalID(t *testing.T) {
	for _, image := range []string{"judger-bench:latest", strings.Repeat("e", 64), "sha256:" + strings.Repeat("E", 64)} {
		o := baseOptions()
		o.benchmarkImage = image
		if _, _, err := o.buildPlan(context.Background(), testConfig(t)); err == nil || !strings.Contains(err.Error(), "--benchmark-image") {
			t.Fatalf("image %q error = %v", image, err)
		}
	}
}

func TestBuildPlanProductionCompatRequiresOCI(t *testing.T) {
	o := baseOptions()
	o.productionCompat = true
	if _, _, err := o.buildPlan(context.Background(), testConfig(t)); err == nil || !strings.Contains(err.Error(), "requires --benchmark-image") {
		t.Fatalf("error = %v", err)
	}
}

func TestBuildPlanRejectsUnknownHost(t *testing.T) {
	o := baseOptions()
	o.host = "production-k8s"
	_, _, err := o.buildPlan(context.Background(), testConfig(t))
	if err == nil || !strings.Contains(err.Error(), "allowlisted") {
		t.Fatalf("err = %v, want allowlist error", err)
	}
}

func TestBuildPlanHashesFixtures(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.cpp")
	if err := os.WriteFile(path, []byte("int main(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := baseOptions()
	o.fixtures = stringList{"cpp-runtime-v1=" + path}
	expected := filepath.Join(dir, "expected.txt")
	if err := os.WriteFile(expected, []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o.expectedOutputs = stringList{"cpp-runtime-v1=" + expected}
	plan, _, err := o.buildPlan(context.Background(), testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Fixtures) != 1 || len(plan.Fixtures[0].SHA256) != 64 {
		t.Fatalf("fixtures = %+v", plan.Fixtures)
	}
}

func TestBuildPlanBadFixtureSpec(t *testing.T) {
	o := baseOptions()
	o.fixtures = stringList{"missing-equals"}
	o.expectedOutputs = nil
	_, _, err := o.buildPlan(context.Background(), testConfig(t))
	if err == nil {
		t.Fatal("accepted a malformed fixture spec")
	}
}

func TestBuildExternalIrisPlan(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.cpp")
	env := filepath.Join(dir, "iris.env")
	if err := os.WriteFile(source, []byte("int main(){}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env, []byte("SECRET=value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.Profiles["external-1"] = config.Profile{Suite: "iris", Workers: 1, Repetitions: 5, CPUList: "2"}
	o := baseOptions()
	o.profile = "external-1"
	o.productionCompat = true
	o.fixtures = nil
	o.expectedOutputs = nil
	o.benchBinary = ""
	o.benchBinarySHA = ""
	o.irisSource = source
	o.irisEnvFile = env
	o.rabbitmqDigest = "sha256:" + strings.Repeat("e", 64)
	o.problemID = 15850
	o.language = "Cpp"
	o.timeLimitMS = 1000
	o.memoryLimitBytes = 1024
	o.testcasesPerRequest = 1
	o.messageIDStart = 2000001
	o.rdsGeneration = "rds-1"
	o.s3ObjectSet = "objects-1"
	o.s3Bucket = "bucket"
	plan, _, err := o.buildPlan(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if plan.IrisSuite == nil || len(plan.Fixtures) != 0 || plan.WorkloadBinary.Path != "" {
		t.Fatalf("iris plan = %+v", plan)
	}
	if plan.Images["rabbitmq"].Digest != o.rabbitmqDigest {
		t.Fatalf("rabbitmq image = %+v", plan.Images["rabbitmq"])
	}
}

func TestNewOpID(t *testing.T) {
	a, err := newOpID()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newOpID()
	if a == b || !strings.HasPrefix(a, "op-") {
		t.Fatalf("op ids = %q, %q", a, b)
	}
}

func TestNotImplemented(t *testing.T) {
	for _, name := range []string{"provision", "qualify", "resume"} {
		if err := notImplemented(name); err == nil || !strings.Contains(err.Error(), "not implemented") {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}
