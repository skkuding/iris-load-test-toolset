package transport

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skkuding/iris-load-test-toolset/internal/protocol"
)

func sshForTest(t *testing.T) (SSH, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return SSH{Host: "codedang8", Opts: Options{BatchMode: true, ConnectTimeout: 15}}, home
}

func TestSocketPathUnderHomeSockets(t *testing.T) {
	s, home := sshForTest(t)
	p, err := s.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".ssh", "sockets", "iris-bench-codedang8")
	if p != want {
		t.Fatalf("socket path = %q, want %q", p, want)
	}
	info, err := os.Stat(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir mode = %v, want 0700", info.Mode().Perm())
	}
}

func TestSocketPathExplicitControlPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := SSH{
		Host: "codedang8",
		Opts: Options{
			BatchMode:      true,
			ConnectTimeout: 15,
			ControlPath:    "~/.ssh/sockets/codedang8.sock",
		},
	}
	p, err := s.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".ssh", "sockets", "codedang8.sock")
	if p != want {
		t.Fatalf("socket path = %q, want %q", p, want)
	}
	args, err := s.BaseArgs()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "ControlPath="+want) {
		t.Fatalf("BaseArgs missing explicit ControlPath: %v", args)
	}
}

func TestSocketPathRejectsRelativeControlPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := SSH{Host: "codedang8", Opts: Options{ControlPath: "relative.sock"}}
	if _, err := s.SocketPath(); err == nil {
		t.Fatal("expected error for relative control path")
	}
}

func TestBaseArgsControlPath(t *testing.T) {
	s, home := sshForTest(t)
	args, err := s.BaseArgs()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	want := "ControlPath=" + filepath.Join(home, ".ssh", "sockets", "iris-bench-codedang8")
	if !strings.Contains(joined, want) {
		t.Fatalf("BaseArgs missing explicit control path: %v", args)
	}
	if !strings.Contains(joined, "BatchMode=yes") {
		t.Fatal("BaseArgs missing BatchMode")
	}
}

func TestCommandArgsSeparatesHostAndRemote(t *testing.T) {
	s, _ := sshForTest(t)
	args, err := s.CommandArgs([]string{"/opt/iris-bench/bin/0.1.0/iris-bench-agent", "--plan-file", "/tmp/p.json"})
	if err != nil {
		t.Fatal(err)
	}
	// The host must be its own argv entry, never concatenated.
	found := false
	for i, a := range args {
		if a == "--" && i+1 < len(args) && args[i+1] == "codedang8" {
			found = true
		}
		if strings.Contains(a, " ") {
			t.Fatalf("argument %q contains a space", a)
		}
	}
	if !found {
		t.Fatalf("host not passed as a separate argument: %v", args)
	}
	if args[len(args)-1] != "/tmp/p.json" {
		t.Fatalf("last arg = %q", args[len(args)-1])
	}
}

func TestCommandArgsRejectsUnsafeToken(t *testing.T) {
	s, _ := sshForTest(t)
	if _, err := s.CommandArgs([]string{"/bin/sh", "-c", "rm -rf /"}); err == nil {
		t.Fatal("CommandArgs accepted an unsafe remote token")
	}
}

func TestValidateRemoteToken(t *testing.T) {
	for _, ok := range []string{"/opt/x/agent", "--plan-file", "/tmp/a.json", "name=value"} {
		if err := ValidateRemoteToken(ok); err != nil {
			t.Errorf("ValidateRemoteToken(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a b", "a;b", "$(x)", "a\nb"} {
		if err := ValidateRemoteToken(bad); err == nil {
			t.Errorf("ValidateRemoteToken(%q) = nil", bad)
		}
	}
}

func TestOptionsValidate(t *testing.T) {
	if err := (Options{ConnectTimeout: 15, ExtraOptions: []string{"StrictHostKeyChecking=yes"}}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Options{ConnectTimeout: 0}).Validate(); err == nil {
		t.Fatal("Validate accepted zero connect timeout")
	}
	if err := (Options{ConnectTimeout: 1, ExtraOptions: []string{"a\nb"}}).Validate(); err == nil {
		t.Fatal("Validate accepted a newline option")
	}
}

func TestOSExecerOutput(t *testing.T) {
	out, err := (OSExecer{}).Output(context.Background(), "echo", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "hello" {
		t.Fatalf("output = %q", out)
	}
	if _, err := (OSExecer{}).Output(context.Background(), "definitely-not-a-command-xyz"); err == nil {
		t.Fatal("Output accepted a missing command")
	}
}

func TestAgentClientValidatesBeforeSpawn(t *testing.T) {
	s, _ := sshForTest(t)
	c := AgentClient{SSH: s, RemoteAgent: "relative/path"}
	if _, err := c.Invoke(context.Background(), protocol.Request{
		ProtocolVersion: protocol.Version,
		OperationID:     "op-1",
		RunID:           "r-1",
		Action:          protocol.ActionInspect,
	}, nil); err == nil {
		t.Fatal("Invoke accepted a relative agent path")
	}
	c.RemoteAgent = "/opt/agent"
	bad := protocol.Request{ProtocolVersion: 99, OperationID: "op-1", RunID: "r-1", Action: protocol.ActionInspect}
	if _, err := c.Invoke(context.Background(), bad, nil); err == nil {
		t.Fatal("Invoke accepted an invalid request")
	}
}

func TestRunDirAndStagedPathValidation(t *testing.T) {
	dir, err := RunDir("iris-20260925-abcdefgh")
	if err != nil || dir != "/tmp/iris-bench-iris-20260925-abcdefgh" {
		t.Fatalf("RunDir = %q, %v", dir, err)
	}
	for _, bad := range []string{"../escape", "bad/id", ""} {
		if _, err := RunDir(bad); err == nil {
			t.Errorf("RunDir(%q) accepted", bad)
		}
	}
	for _, bad := range []string{
		"/tmp/iris-bench-iris-20260925-abcdefgh",
		"/tmp/iris-bench-iris-20260925-abcdefgh/../escape",
		"/tmp/other/file",
		"/tmp/iris-bench-bad/id/file",
	} {
		if err := validateRunFilePath(bad); err == nil {
			t.Errorf("validateRunFilePath(%q) accepted", bad)
		}
	}
}

func TestSHA256ParsesRemoteDigestAndRejectsUnsafePath(t *testing.T) {
	dir := t.TempDir()
	sshBin := filepath.Join(dir, "fake-ssh")
	digest := strings.Repeat("a", 64)
	script := "#!/bin/sh\nfor last do :; done\nprintf '" + digest + "  %s\\n' \"$last\"\n"
	if err := os.WriteFile(sshBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s := SSH{Host: "codedang8", Opts: Options{SSHBinary: sshBin, SocketDir: filepath.Join(dir, "sockets"), ConnectTimeout: 15}}
	path := "/tmp/iris-bench-iris-20260925-abcdefgh/agent"
	got, err := s.SHA256(context.Background(), path)
	if err != nil || got != digest {
		t.Fatalf("SHA256 = %q, %v", got, err)
	}
	if _, err := s.SHA256(context.Background(), "/tmp/iris-bench-iris-20260925-abcdefgh/../agent"); err == nil {
		t.Fatal("SHA256 accepted traversal")
	}
}

func TestRunDirectoryMethodsRejectInvalidIDBeforeSpawn(t *testing.T) {
	s := SSH{Host: "codedang8", Opts: Options{SSHBinary: "definitely-not-a-command", ConnectTimeout: 15}}
	if _, err := s.CreateRunDir(context.Background(), "../escape"); err == nil {
		t.Fatal("CreateRunDir accepted invalid run id")
	}
	if err := s.RemoveRunDir(context.Background(), "../escape"); err == nil {
		t.Fatal("RemoveRunDir accepted invalid run id")
	}
}

func TestCreateAndRemoveRunDirectoryUseValidatedPrivatePath(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "commands")
	sshBin := filepath.Join(dir, "fake-ssh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + record + "\n" +
		"for arg do case \"$arg\" in %F) printf 'directory\\n';; %a) printf '700\\n';; esac; done\n"
	if err := os.WriteFile(sshBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s := SSH{Host: "codedang8", Opts: Options{SSHBinary: sshBin, SocketDir: filepath.Join(dir, "sockets"), ConnectTimeout: 15}}
	runID := "iris-20260925-abcdefgh"
	got, err := s.CreateRunDir(context.Background(), runID)
	if err != nil || got != "/tmp/iris-bench-"+runID {
		t.Fatalf("CreateRunDir = %q, %v", got, err)
	}
	if err := s.RemoveRunDir(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	commands, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	text := string(commands)
	for _, want := range []string{"mkdir -m 0700 -- " + got, "chmod 0700 -- " + got, "rm -rf -- " + got} {
		if !strings.Contains(text, want) {
			t.Fatalf("remote commands missing %q in %q", want, text)
		}
	}
}
