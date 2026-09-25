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
		host:         "codedang8",
		profile:      "isolated-1s",
		runID:        "iris-20260925-abcdefgh",
		irisDigest:   "sha256:" + strings.Repeat("b", 64),
		judgerDigest: strings.Repeat("c", 64),
		seed:         7,
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
	_, _, err := o.buildPlan(context.Background(), testConfig(t))
	if err == nil {
		t.Fatal("accepted a malformed fixture spec")
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
	for _, name := range []string{"provision", "qualify", "analyze", "resume"} {
		if err := notImplemented(name); err == nil || !strings.Contains(err.Error(), "not implemented") {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}
