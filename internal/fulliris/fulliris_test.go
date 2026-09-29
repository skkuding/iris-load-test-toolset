package fulliris

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMakeRequestOmitsExternalTestcases(t *testing.T) {
	d := t.TempDir()
	source := filepath.Join(d, "source.cpp")
	if err := os.WriteFile(source, []byte("int main() {}"), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := makeRequest(Config{SourcePath: source, ProblemID: 568, Language: "cpp", TimeLimitMS: 1000, MemoryLimitBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if _, ok := v["userTestcases"]; ok || strings.Contains(string(b), "userTestcases") {
		t.Fatalf("request unexpectedly contains userTestcases: %s", b)
	}
}

func TestValidateConfigRejectsUnsafeInputs(t *testing.T) {
	d := t.TempDir()
	file := filepath.Join(d, "x")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	c := Config{RunID: "Run", BlockID: "b", Workers: 1, Repetitions: 1, IrisImage: "iris:latest", RabbitMQImage: "rabbit@sha256:" + strings.Repeat("a", 64), JudgerDigest: "sha256:" + strings.Repeat("b", 64), SourcePath: file, SecretEnvPath: file, RunDir: d, RuntimeDir: d, CgroupMount: d, ProblemID: 1, Language: "cpp", TimeLimitMS: 1, MemoryLimitBytes: 1, TestcasesPerRequest: 1, Timeout: time.Second}
	if err := validateConfig(d, c); err == nil {
		t.Fatal("accepted invalid run id and unpinned image")
	}
}

func TestParseActualNestedAndTerminalBodies(t *testing.T) {
	p, err := parseActual("judge", []byte(`{"submissionId":17,"resultCode":0,"judgeResult":{"testcaseId":42,"cpuTime":12,"realTime":20,"memory":4096,"errorCode":0}}`))
	if err != nil || p.submission != "17" || p.testcase != "42" || p.cpu != 12 || p.real != 20 || p.memory != 4096 || p.resultCode != "0" {
		t.Fatalf("parsed testcase = %+v, err=%v", p, err)
	}
	p, err = parseActual("submission", []byte(`{"submissionId":17,"judgeResults":[{"testcaseId":42}]}`))
	if err != nil || !p.terminal || p.submission != "17" {
		t.Fatalf("parsed terminal = %+v, err=%v", p, err)
	}
}

func TestReceiptImageTagsAreDistinct(t *testing.T) {
	b, err := json.Marshal(Receipt{IrisImage: "iris@sha256:" + strings.Repeat("a", 64), RabbitMQImage: "rabbit@sha256:" + strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"irisImage":""`) || !strings.Contains(string(b), `"rabbitMqImage"`) {
		t.Fatalf("bad receipt tags: %s", b)
	}
}

func TestRuntimeEnvIsPerWorkerAndDoesNotUseSharedConsumerTag(t *testing.T) {
	d := t.TempDir()
	secret := filepath.Join(d, "secret.env")
	if err := os.WriteFile(secret, []byte("DB_HOST=db\nAWS_REGION=us-east-1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c := Config{RunID: "run", BlockID: "block", Workers: 2, SecretEnvPath: secret, RuntimeDir: d, S3Bucket: "bucket-name"}
	if err := writeRuntimeFiles(c, "rabbit", "secret"); err != nil {
		t.Fatal(err)
	}
	a, err := os.ReadFile(filepath.Join(d, "fulliris-env-block-000"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(d, "fulliris-env-block-001"))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) == string(b) || !strings.Contains(string(a), "RABBITMQ_DEFAULT_PASS=secret") || !strings.Contains(string(a), "TESTCASE_BUCKET_NAME=bucket-name") {
		t.Fatalf("worker env not generated correctly")
	}
	if !strings.Contains(string(a), "APP_ENV=production") || !strings.Contains(string(a), "DATABASE_CONNECTION_LIMIT=1") {
		t.Fatalf("worker env does not reproduce production settings")
	}
	if !strings.Contains(string(a), "JUDGE_REQUEST_CONSUMER_TAG=fulliris-run-block-request-000") || !strings.Contains(string(b), "JUDGE_REQUEST_CONSUMER_TAG=fulliris-run-block-request-001") {
		t.Fatalf("consumer tags are not unique")
	}
}

func TestNamesIncludeRunAndBlock(t *testing.T) {
	n := names(Config{RunID: "run", BlockID: "block", Workers: 2})
	if n.rabbit != "iris-loadtest-run-block-rabbit" || n.workers[1] != "iris-loadtest-run-block-worker-001" {
		t.Fatalf("unexpected names: %+v", n)
	}
}
