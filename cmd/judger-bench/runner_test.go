package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// writeFakeJudger writes a fake alpha.4 Judger executable that records its
// arguments and environment, writes a deterministic output file, and prints
// body on stdout. It requires no privileges.
func writeFakeJudger(t *testing.T, dir, record, body string) string {
	t.Helper()
	script := "#!/bin/sh\n" +
		": > " + shellQuote(record) + "\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\" >> " + shellQuote(record) + "; done\n" +
		"printf 'CONTAINER_ID=%s\\n' \"$CONTAINER_ID\" >> " + shellQuote(record) + "\n" +
		"out=\"\"\n" +
		"for a in \"$@\"; do case \"$a\" in --output_path=*) out=\"${a#--output_path=}\";; esac; done\n" +
		"if [ -n \"$out\" ]; then printf 'judger-stdout\\n' > \"$out\"; fi\n" +
		"printf '%s' " + shellQuote(body) + "\n"
	return writeScript(t, dir, "fakejudger.sh", script)
}

func judgerJSON(cgroup string, result, errCode int) string {
	return fmt.Sprintf(`{"cpu_time":12,"real_time":34,"memory":5678,"signal":0,"exit_code":0,"error":%d,"result":%d,"cgroup_path":%q}`,
		errCode, result, cgroup)
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

func hasLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

func hasPrefix(lines []string, prefix string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func expectedOutput(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "expected.out")
	if err := os.WriteFile(p, []byte("judger-stdout\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCompileUsesProductionFlagsAndRecordsDigest(t *testing.T) {
	work := t.TempDir()
	source := filepath.Join(work, "source.cpp")
	if err := os.WriteFile(source, []byte("int main(){return 0;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(work, "compiler-args.txt")
	// The fake compiler records the exact argv, then emits a fake binary at the
	// path following "-o".
	compiler := writeScript(t, work, "fakec.sh", "#!/bin/sh\n"+
		": > "+shellQuote(record)+"\n"+
		"out=\"\"\nprev=\"\"\n"+
		"for a in \"$@\"; do\n"+
		"  printf '%s\\n' \"$a\" >> "+shellQuote(record)+"\n"+
		"  if [ \"$prev\" = \"-o\" ]; then out=\"$a\"; fi\n"+
		"  prev=\"$a\"\n"+
		"done\n"+
		"printf '#!/bin/sh\\nexit 0\\n' > \"$out\"\n"+
		"chmod +x \"$out\"\n")

	samples, err := runBench(context.Background(), options{
		Mode:     modeCompile,
		RunID:    "iris-20260925-abcdefgh",
		BlockID:  "isolated-1s-01",
		Worker:   "w0",
		Fixture:  "cpp",
		Source:   source,
		WorkDir:  work,
		Compiler: compiler,
		Timeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].Status != "success" {
		t.Fatalf("compile samples = %+v", samples)
	}
	if samples[0].OutputSHA256 == "" || samples[0].OutputBytes == 0 {
		t.Fatal("compile sample missing binary digest")
	}

	args := readLines(t, record)
	for _, want := range []string{"-DONLINE_JUDGE", "-O2", "-Wall", "-std=c++14", "-lm", "-o", source} {
		if !hasLine(args, want) {
			t.Fatalf("production compile flag %q missing from %v", want, args)
		}
	}
}

func TestExecuteInvokesJudgerAndValidatesCgroup(t *testing.T) {
	work := t.TempDir()
	bin := filepath.Join(work, defaultBinaryName)
	writeExecutable(t, bin, "#!/bin/sh\nexit 0\n")
	input := filepath.Join(work, "1.in")
	if err := os.WriteFile(input, []byte("1 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent := "/sys/fs/cgroup/bench.slice"
	record := filepath.Join(work, "judger-args.txt")
	judger := writeFakeJudger(t, work, record, judgerJSON(parent+"/sandbox-abc/box-123", 0, 0))

	samples, err := runBench(context.Background(), options{
		Mode:                 modeExecute,
		RunID:                "iris-20260925-abcdefgh",
		BlockID:              "isolated-1s-01",
		Worker:               "w0",
		Fixture:              "cpp",
		Binary:               bin,
		Input:                input,
		WorkDir:              work,
		ExpectedOutput:       expectedOutput(t, work),
		Iterations:           2,
		JudgerPath:           judger,
		ContainerID:          "abc",
		ExpectedCgroupParent: parent,
		UID:                  65534,
		GID:                  65534,
		SeccompRule:          "c_cpp",
		MaxCPUTimeMs:         1000,
		MaxRealTimeMs:        3000,
		MaxMemoryBytes:       64 << 20,
		MaxOutputBytes:       1 << 20,
		MaxStackBytes:        8 << 20,
		Timeout:              3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 {
		t.Fatalf("execute samples = %d, want 2", len(samples))
	}
	for _, s := range samples {
		if s.Status != "success" {
			t.Fatalf("sample failed: %+v", s)
		}
		if !s.CgroupContained {
			t.Fatalf("sample not contained: %+v", s)
		}
		if s.CPUTimeMs != 12 || s.RealTimeMs != 34 || s.MemoryBytes != 5678 {
			t.Fatalf("judger fields not parsed: %+v", s)
		}
		if s.ExitCode != 0 || s.Signal != 0 || s.ErrorCode != 0 || s.ResultCode != 0 {
			t.Fatalf("judger codes not parsed: %+v", s)
		}
		if s.CgroupPath != parent+"/sandbox-abc/box-123" {
			t.Fatalf("cgroup path = %q", s.CgroupPath)
		}
		if s.JudgerPID <= 0 {
			t.Fatalf("judger pid not recorded: %+v", s)
		}
		if s.OutputBytes != int64(len("judger-stdout\n")) {
			t.Fatalf("output bytes = %d", s.OutputBytes)
		}
	}
}

func TestExecutePassesExplicitArguments(t *testing.T) {
	work := t.TempDir()
	bin := filepath.Join(work, defaultBinaryName)
	writeExecutable(t, bin, "#!/bin/sh\nexit 0\n")
	input := filepath.Join(work, "1.in")
	if err := os.WriteFile(input, []byte("1 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent := "/sys/fs/cgroup/bench.slice"
	record := filepath.Join(work, "judger-args.txt")
	judger := writeFakeJudger(t, work, record, judgerJSON(parent+"/sandbox-abc/box-1", 0, 0))

	_, err := runBench(context.Background(), options{
		Mode:                 modeExecute,
		RunID:                "r",
		BlockID:              "b",
		Worker:               "w0",
		Fixture:              "cpp",
		Binary:               bin,
		Input:                input,
		WorkDir:              work,
		ExpectedOutput:       expectedOutput(t, work),
		Iterations:           1,
		JudgerPath:           judger,
		ContainerID:          "abc",
		ExpectedCgroupParent: parent,
		UID:                  1234,
		GID:                  4321,
		SeccompRule:          "c_cpp",
		MaxCPUTimeMs:         1000,
		MaxRealTimeMs:        3000,
		MaxMemoryBytes:       64 << 20,
		MaxStackBytes:        8 << 20,
		MaxOutputBytes:       1 << 20,
		MaxProcessNumber:     64,
		Env:                  []string{"PATH=/usr/bin", "LANG=en_US.UTF-8"},
		RunArgs:              []string{"extra"},
		Timeout:              3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	args := readLines(t, record)
	for _, want := range []string{
		"--max_cpu_time=1000",
		"--max_real_time=3000",
		"--max_memory=67108864",
		"--max_stack=8388608",
		"--max_output_size=1048576",
		"--max_process_number=64",
		"--uid=1234",
		"--gid=4321",
		"--seccomp_rule_name=c_cpp",
		"--memory_limit_check_only=0",
		"--args=extra",
		"--env=PATH=/usr/bin",
		"--env=LANG=en_US.UTF-8",
		"CONTAINER_ID=abc",
	} {
		if !hasLine(args, want) {
			t.Fatalf("required judger argument %q missing from %v", want, args)
		}
	}
	if !hasPrefix(args, "--exe_path="+bin) {
		t.Fatalf("exe_path missing from %v", args)
	}
	for _, prefix := range []string{"--input_path=", "--output_path=", "--error_path=", "--log_path="} {
		if !hasPrefix(args, prefix) {
			t.Fatalf("required path argument %q missing from %v", prefix, args)
		}
	}
	// The harness must never move a running process into a cgroup.
	for _, a := range args {
		if strings.Contains(a, "cgroup.procs") || strings.HasPrefix(a, "--pid") {
			t.Fatalf("unexpected PID/cgroup mutation argument %q", a)
		}
	}
}

func TestExecuteCgroupEscapeIsRejected(t *testing.T) {
	work := t.TempDir()
	bin := filepath.Join(work, defaultBinaryName)
	writeExecutable(t, bin, "#!/bin/sh\nexit 0\n")
	parent := "/sys/fs/cgroup/bench.slice"
	record := filepath.Join(work, "judger-args.txt")
	// The reported cgroup is a sibling outside the delegated parent.
	judger := writeFakeJudger(t, work, record, judgerJSON("/sys/fs/cgroup/sandbox-escaped/box-1", 0, 0))

	samples, err := runBench(context.Background(), options{
		Mode:                 modeExecute,
		RunID:                "r",
		BlockID:              "b",
		Binary:               bin,
		WorkDir:              work,
		ExpectedOutput:       expectedOutput(t, work),
		Iterations:           1,
		JudgerPath:           judger,
		ContainerID:          "escaped",
		ExpectedCgroupParent: parent,
		Timeout:              3 * time.Second,
	})
	if err == nil || !errors.Is(err, errCgroupEscape) {
		t.Fatalf("runBench error = %v, want cgroup escape", err)
	}
	if len(samples) != 1 {
		t.Fatalf("samples = %d, want 1 (evidence retained)", len(samples))
	}
	if samples[0].Status != "cgroup_escape" || samples[0].CgroupContained {
		t.Fatalf("escape sample not rejected: %+v", samples[0])
	}
}

func TestExecuteProductionCompatAcceptsRunSandboxDescendantAndMarksNonComparable(t *testing.T) {
	work := t.TempDir()
	bin := filepath.Join(work, defaultBinaryName)
	writeExecutable(t, bin, "#!/bin/sh\nexit 0\n")
	record := filepath.Join(work, "judger-args.txt")
	judger := writeFakeJudger(t, work, record, judgerJSON("/sandbox-abc/box-123", 0, 0))
	samples, err := runBench(context.Background(), options{
		Mode: modeExecute, Binary: bin, WorkDir: work, ExpectedOutput: expectedOutput(t, work),
		Iterations: 1, JudgerPath: judger, ContainerID: "abc",
		ExpectedCgroupParent: "/sys/fs/cgroup/bench.slice", ProductionCompat: true, Timeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := samples[0]
	if s.Status != "success" || s.CgroupContained || s.Comparable || s.ContainmentMode != "production-compat" {
		t.Fatalf("production compatibility sample = %+v", s)
	}

	wrong := writeFakeJudger(t, work, record, judgerJSON("/sandbox-other", 0, 0))
	_, err = runBench(context.Background(), options{
		Mode: modeExecute, Binary: bin, WorkDir: work, ExpectedOutput: expectedOutput(t, work),
		Iterations: 1, JudgerPath: wrong, ContainerID: "abc",
		ExpectedCgroupParent: "/sys/fs/cgroup/bench.slice", ProductionCompat: true, Timeout: 3 * time.Second,
	})
	if !errors.Is(err, errCgroupEscape) {
		t.Fatalf("wrong root sandbox error = %v, want cgroup escape", err)
	}
}

func TestExecuteProductionCompatAcceptsFilesystemPathForm(t *testing.T) {
	work := t.TempDir()
	bin := filepath.Join(work, defaultBinaryName)
	writeExecutable(t, bin, "#!/bin/sh\nexit 0\n")
	record := filepath.Join(work, "judger-args.txt")
	// Live alpha.4 reports the full filesystem path while the monitor compares
	// mount-relative paths.
	judger := writeFakeJudger(t, work, record, judgerJSON("/sys/fs/cgroup/sandbox-abc/box-123", 0, 0))
	samples, err := runBench(context.Background(), options{
		Mode: modeExecute, Binary: bin, WorkDir: work, ExpectedOutput: expectedOutput(t, work),
		Iterations: 1, JudgerPath: judger, ContainerID: "abc",
		ExpectedCgroupParent: "/sys/fs/cgroup/bench.slice", ProductionCompat: true, Timeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := samples[0]
	if s.Status != "success" || s.CgroupContained || s.Comparable || s.ContainmentMode != "production-compat" {
		t.Fatalf("production compatibility sample = %+v", s)
	}
}

func TestProductionCompatAcceptsBothPathForms(t *testing.T) {
	cases := []struct {
		name string
		got  string
		ok   bool
	}{
		{"mount-relative-root", "/sandbox-abc", true},
		{"mount-relative-descendant", "/sandbox-abc/box-123", true},
		{"filesystem-root", "/sys/fs/cgroup/sandbox-abc", true},
		{"filesystem-descendant", "/sys/fs/cgroup/sandbox-abc/box-123", true},
		{"prefix-confusion-relative", "/sandbox-abc-evil/child", false},
		{"prefix-confusion-filesystem", "/sys/fs/cgroup/sandbox-abc-evil/child", false},
		{"unrelated", "/sandbox-other", false},
		{"unrelated-filesystem", "/sys/fs/cgroup/other", false},
		{"traversal", "/sys/fs/cgroup/sandbox-abc/../../etc", false},
		{"relative", "sandbox-abc", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateProductionCgroup("abc", tc.got)
			if tc.ok && err != nil {
				t.Fatalf("validateProductionCgroup(%q) = %v", tc.got, err)
			}
			if !tc.ok && !errors.Is(err, errCgroupEscape) {
				t.Fatalf("validateProductionCgroup(%q) = %v, want cgroup escape", tc.got, err)
			}
		})
	}
}

func TestProductionCompatRejectsUnsafeContainerID(t *testing.T) {
	for _, id := range []string{"", ".", "..", "../etc", "a/b"} {
		if err := validateProductionCgroup(id, "/etc"); !errors.Is(err, errCgroupEscape) {
			t.Fatalf("container id %q accepted: %v", id, err)
		}
	}
}

func TestOutputMismatchPreservesSpecificJudgerStatus(t *testing.T) {
	work := t.TempDir()
	bin := filepath.Join(work, defaultBinaryName)
	writeExecutable(t, bin, "#!/bin/sh\nexit 0\n")
	expected := filepath.Join(work, "different.out")
	if err := os.WriteFile(expected, []byte("different\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	judger := writeFakeJudger(t, work, filepath.Join(work, "args.txt"), judgerJSON("/sys/fs/cgroup/bench.slice/sandbox-abc", 4, 0))
	samples, err := runBench(context.Background(), options{
		Mode: modeExecute, Binary: bin, WorkDir: work, ExpectedOutput: expected,
		Iterations: 1, JudgerPath: judger, ContainerID: "abc",
		ExpectedCgroupParent: "/sys/fs/cgroup/bench.slice", Timeout: 3 * time.Second,
	})
	if err == nil || samples[0].Status != "runtime_error" || samples[0].OutputMatches {
		t.Fatalf("samples=%+v err=%v", samples, err)
	}
}

func TestExecuteRejectsOutputMismatch(t *testing.T) {
	work := t.TempDir()
	bin := filepath.Join(work, defaultBinaryName)
	writeExecutable(t, bin, "#!/bin/sh\nexit 0\n")
	expected := filepath.Join(work, "different.out")
	if err := os.WriteFile(expected, []byte("different\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(work, "judger-args.txt")
	judger := writeFakeJudger(t, work, record, judgerJSON("/sys/fs/cgroup/bench.slice/sandbox-abc", 0, 0))
	samples, err := runBench(context.Background(), options{
		Mode: modeExecute, Binary: bin, WorkDir: work, ExpectedOutput: expected,
		Iterations: 1, JudgerPath: judger, ContainerID: "abc",
		ExpectedCgroupParent: "/sys/fs/cgroup/bench.slice", Timeout: 3 * time.Second,
	})
	if err == nil || samples[0].Status != "wrong_answer" || samples[0].OutputMatches {
		t.Fatalf("output mismatch samples=%+v err=%v", samples, err)
	}
}

func TestExecuteRejectsMalformedJudgerJSON(t *testing.T) {
	work := t.TempDir()
	bin := filepath.Join(work, defaultBinaryName)
	writeExecutable(t, bin, "#!/bin/sh\nexit 0\n")
	parent := "/sys/fs/cgroup/bench.slice"
	record := filepath.Join(work, "judger-args.txt")
	judger := writeFakeJudger(t, work, record, "this is not json")

	samples, err := runBench(context.Background(), options{
		Mode:                 modeExecute,
		RunID:                "r",
		BlockID:              "b",
		Binary:               bin,
		WorkDir:              work,
		ExpectedOutput:       expectedOutput(t, work),
		Iterations:           1,
		JudgerPath:           judger,
		ContainerID:          "abc",
		ExpectedCgroupParent: parent,
		Timeout:              3 * time.Second,
	})
	if err == nil {
		t.Fatal("malformed Judger result was not enforced")
	}
	if samples[0].Status != "failed" || samples[0].CgroupContained {
		t.Fatalf("malformed sample not rejected: %+v", samples[0])
	}
	if !strings.Contains(samples[0].Error, "parse judger json") {
		t.Fatalf("error = %q", samples[0].Error)
	}
}

func TestExecuteSurfacesJudgerResultCodes(t *testing.T) {
	cases := []struct {
		result  int
		errCode int
		status  string
	}{
		{3, 0, "memory_limit_exceeded"},
		{2, 0, "real_time_limit_exceeded"},
		{4, 0, "runtime_error"},
		{0, -1, "judger_error"},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			work := t.TempDir()
			bin := filepath.Join(work, defaultBinaryName)
			writeExecutable(t, bin, "#!/bin/sh\nexit 0\n")
			parent := "/sys/fs/cgroup/bench.slice"
			record := filepath.Join(work, "judger-args.txt")
			judger := writeFakeJudger(t, work, record, judgerJSON(parent+"/sandbox-abc/box-1", tc.result, tc.errCode))

			samples, err := runBench(context.Background(), options{
				Mode:                 modeExecute,
				RunID:                "r",
				BlockID:              "b",
				Binary:               bin,
				WorkDir:              work,
				ExpectedOutput:       expectedOutput(t, work),
				Iterations:           1,
				JudgerPath:           judger,
				ContainerID:          "abc",
				ExpectedCgroupParent: parent,
				Timeout:              3 * time.Second,
			})
			if err == nil {
				t.Fatal("non-success Judger result was not enforced")
			}
			if samples[0].Status != tc.status {
				t.Fatalf("status = %q, want %q (%+v)", samples[0].Status, tc.status, samples[0])
			}
		})
	}
}

func TestExecuteOuterTimeoutKillsJudger(t *testing.T) {
	work := t.TempDir()
	bin := filepath.Join(work, defaultBinaryName)
	writeExecutable(t, bin, "#!/bin/sh\nexit 0\n")
	parent := "/sys/fs/cgroup/bench.slice"
	judger := writeScript(t, work, "slowjudger.sh", "#!/bin/sh\nsleep 5\n")

	samples, err := runBench(context.Background(), options{
		Mode:                 modeExecute,
		RunID:                "r",
		BlockID:              "b",
		Binary:               bin,
		WorkDir:              work,
		ExpectedOutput:       expectedOutput(t, work),
		Iterations:           1,
		JudgerPath:           judger,
		ContainerID:          "abc",
		ExpectedCgroupParent: parent,
		Timeout:              150 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("Judger timeout was not enforced")
	}
	if samples[0].Status != "judger_timeout" {
		t.Fatalf("status = %q, want judger_timeout", samples[0].Status)
	}
}

func TestExecuteRequiresContainerIDAndParent(t *testing.T) {
	work := t.TempDir()
	bin := filepath.Join(work, defaultBinaryName)
	writeExecutable(t, bin, "#!/bin/sh\nexit 0\n")
	base := options{Mode: modeExecute, Binary: bin, WorkDir: work, ExpectedOutput: expectedOutput(t, work), JudgerPath: "/bin/true"}
	if _, err := runBench(context.Background(), base); err == nil {
		t.Fatal("execute accepted a missing container id")
	}
	base.ContainerID = "abc"
	if _, err := runBench(context.Background(), base); err == nil {
		t.Fatal("execute accepted a missing cgroup parent")
	}
}

func TestValidateCgroupBeneathTable(t *testing.T) {
	cases := []struct {
		name     string
		parent   string
		reported string
		ok       bool
	}{
		{"descendant", "/sys/fs/cgroup/bench.slice", "/sys/fs/cgroup/bench.slice/sandbox-x/box-1", true},
		{"equal", "/sys/fs/cgroup/bench.slice", "/sys/fs/cgroup/bench.slice", true},
		{"prefix-confusion", "/sys/fs/cgroup/bench", "/sys/fs/cgroup/bench-evil/box-1", false},
		{"unrelated", "/sys/fs/cgroup/bench.slice", "/sys/fs/cgroup/sandbox-x/box-1", false},
		{"traversal", "/sys/fs/cgroup/bench.slice", "/sys/fs/cgroup/bench.slice/../../sandbox/box", false},
		{"relative-parent", "sys/fs/cgroup", "/sys/fs/cgroup/box", false},
		{"relative-reported", "/sys/fs/cgroup", "sandbox/box", false},
		{"empty-reported", "/sys/fs/cgroup", "", false},
		{"root-parent", "/", "/sys/fs/cgroup/box", false},
		{"cgroup-mount-parent", "/sys/fs/cgroup", "/sys/fs/cgroup/box", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCgroupBeneath(tc.parent, tc.reported)
			if tc.ok && err != nil {
				t.Fatalf("validateCgroupBeneath(%q,%q) = %v", tc.parent, tc.reported, err)
			}
			if !tc.ok && !errors.Is(err, errCgroupEscape) {
				t.Fatalf("validateCgroupBeneath(%q,%q) = %v, want escape", tc.parent, tc.reported, err)
			}
		})
	}
}

func TestExecuteVerifiesPinnedJudgerDigest(t *testing.T) {
	work := t.TempDir()
	bin := filepath.Join(work, defaultBinaryName)
	writeExecutable(t, bin, "#!/bin/sh\nexit 0\n")
	judger := writeScript(t, work, "fakejudger.sh", "#!/bin/sh\necho '{}'\n")
	_, err := runBench(context.Background(), options{
		Mode:                 modeExecute,
		Binary:               bin,
		WorkDir:              work,
		ExpectedOutput:       expectedOutput(t, work),
		Iterations:           1,
		JudgerPath:           judger,
		JudgerSHA256:         judgerAlpha4AMD64SHA256,
		ContainerID:          "abc",
		ExpectedCgroupParent: "/sys/fs/cgroup/bench.slice",
		Timeout:              3 * time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "does not match pinned") {
		t.Fatalf("runBench error = %v, want digest mismatch", err)
	}
}

func TestExecuteRejectsCgroupRootParent(t *testing.T) {
	work := t.TempDir()
	bin := filepath.Join(work, defaultBinaryName)
	writeExecutable(t, bin, "#!/bin/sh\nexit 0\n")
	_, err := runBench(context.Background(), options{
		Mode:                 modeExecute,
		Binary:               bin,
		WorkDir:              work,
		ExpectedOutput:       expectedOutput(t, work),
		Iterations:           1,
		JudgerPath:           "/bin/true",
		ContainerID:          "abc",
		ExpectedCgroupParent: "/sys/fs/cgroup",
		Timeout:              3 * time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "must not be the cgroup root") {
		t.Fatalf("runBench error = %v, want cgroup-root rejection", err)
	}
}

func TestExecuteDefaultSandboxIdentityIsProduction(t *testing.T) {
	work := t.TempDir()
	bin := filepath.Join(work, defaultBinaryName)
	writeExecutable(t, bin, "#!/bin/sh\nexit 0\n")
	parent := "/sys/fs/cgroup/bench.slice"
	record := filepath.Join(work, "judger-args.txt")
	judger := writeFakeJudger(t, work, record, judgerJSON(parent+"/sandbox-abc/box-1", 0, 0))
	_, err := runBench(context.Background(), options{
		Mode:                 modeExecute,
		Binary:               bin,
		WorkDir:              work,
		ExpectedOutput:       expectedOutput(t, work),
		Iterations:           1,
		JudgerPath:           judger,
		ContainerID:          "abc",
		ExpectedCgroupParent: parent,
		UID:                  -1,
		GID:                  -1,
		Timeout:              3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	args := readLines(t, record)
	for _, want := range []string{"--uid=0", "--gid=0"} {
		if !hasLine(args, want) {
			t.Fatalf("production sandbox default %q missing from %v", want, args)
		}
	}
}

func TestWriteSamplesAtomic(t *testing.T) {
	work := t.TempDir()
	out := filepath.Join(work, "samples", "judger.ndjson")
	samples := []sample{
		{RunID: "r-1", Phase: "execute", Iteration: 1, Status: "success"},
		{RunID: "r-1", Phase: "execute", Iteration: 0, Status: "success"},
	}
	if err := writeSamples(out, samples); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	var lineCount int
	for sc.Scan() {
		var s sample
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			t.Fatal(err)
		}
		lineCount++
	}
	if lineCount != 2 {
		t.Fatalf("wrote %d lines, want 2", lineCount)
	}
	if _, err := os.Stat(out + ".tmp"); err == nil {
		t.Fatal("temp file left behind")
	}
}

func TestInvalidModeAndMissingSource(t *testing.T) {
	if _, err := runBench(context.Background(), options{Mode: "bogus"}); err == nil {
		t.Fatal("runBench accepted an invalid mode")
	}
	if _, err := runBench(context.Background(), options{Mode: modeCompile, WorkDir: t.TempDir()}); err == nil {
		t.Fatal("compile accepted a missing source")
	}
}

func TestMainFlagParsing(t *testing.T) {
	// Ensure the command rejects a missing --output before touching the host.
	err := run([]string{"--mode", "compile"})
	if err == nil || !strings.Contains(err.Error(), "--output") {
		t.Fatalf("run() = %v, want --output error", err)
	}
}
