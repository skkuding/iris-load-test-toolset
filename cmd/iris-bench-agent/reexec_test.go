package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestIsWorkerExec(t *testing.T) {
	cases := []struct {
		argv []string
		want bool
	}{
		{[]string{"iris-bench-agent"}, false},
		{[]string{"iris-bench-agent", "inspect"}, false},
		{[]string{"iris-bench-agent", "worker-exec"}, true},
		{[]string{"iris-bench-agent", "worker-exec", "--cgroup", "/sys/fs/cgroup/x"}, true},
		{nil, false},
	}
	for _, tc := range cases {
		if got := isWorkerExec(tc.argv); got != tc.want {
			t.Fatalf("isWorkerExec(%v) = %v, want %v", tc.argv, got, tc.want)
		}
	}
}

func TestValidateDelegatedParent(t *testing.T) {
	cases := []struct {
		name    string
		mount   string
		parent  string
		wantErr bool
	}{
		{"default mount subtree", "", "/sys/fs/cgroup/iris-bench", false},
		{"explicit mount subtree", "/sys/fs/cgroup", "/sys/fs/cgroup/user.slice/user-1001.slice/user@1001.service", false},
		{"custom mount subtree", "/cgroup2", "/cgroup2/iris-bench", false},
		{"relative", "", "sys/fs/cgroup/iris-bench", true},
		{"empty", "", "", true},
		{"mount root", "", "/sys/fs/cgroup", true},
		{"sibling prefix", "", "/sys/fs/cgroup-other", true},
		{"outside mount", "", "/etc/iris-bench", true},
		{"parent traversal", "", "/sys/fs/cgroup/../etc", true},
		{"relative mount", "cgroup2", "/cgroup2/iris-bench", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDelegatedParent(tc.mount, tc.parent)
			if tc.wantErr && err == nil {
				t.Fatalf("validateDelegatedParent(%q, %q) = nil, want error", tc.mount, tc.parent)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateDelegatedParent(%q, %q) = %v, want nil", tc.mount, tc.parent, err)
			}
		})
	}
}

func TestSelfReexecArgvUsesNoShell(t *testing.T) {
	args := []string{"--cgroup-parent", "/sys/fs/cgroup/iris-bench", "inspect"}
	got := selfReexecArgv("systemd-run", "/opt/iris-bench/bin/iris-bench-agent", args)
	want := []string{
		"systemd-run", "--user", "--scope", "--quiet", "--collect",
		"--", "/opt/iris-bench/bin/iris-bench-agent",
		"--cgroup-parent", "/sys/fs/cgroup/iris-bench", "inspect",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selfReexecArgv = %#v, want %#v", got, want)
	}
	for _, a := range got {
		switch a {
		case "sh", "bash", "-c":
			t.Fatalf("self-reexec argv must not invoke a shell: %#v", got)
		}
	}
}

func TestDecideScopedSelfReexec(t *testing.T) {
	const mount = "/sys/fs/cgroup"
	const parent = "/sys/fs/cgroup/user.slice/user-1001.slice/user@1001.service"
	const relParent = "/user.slice/user-1001.slice/user@1001.service"
	const inside = "/user.slice/user-1001.slice/user@1001.service/app.slice/run-abc.scope"
	const outside = "/user.slice/user-1001.slice/session-1.scope"

	t.Run("no parent", func(t *testing.T) {
		d, err := decideScopedSelfReexec("", mount, "", outside)
		if err != nil || d != (scopedSelfReexec{}) {
			t.Fatalf("decision = %+v, err = %v", d, err)
		}
	})
	t.Run("outside parent reexecs", func(t *testing.T) {
		d, err := decideScopedSelfReexec("", mount, parent, outside)
		if err != nil {
			t.Fatal(err)
		}
		if !d.Reexec || d.Guarded || d.RelParent != relParent {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("inside parent proceeds", func(t *testing.T) {
		d, err := decideScopedSelfReexec("", mount, parent, inside)
		if err != nil {
			t.Fatal(err)
		}
		if d.Reexec || d.Guarded {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("guard inside parent is verified", func(t *testing.T) {
		d, err := decideScopedSelfReexec(parent, mount, parent, inside)
		if err != nil {
			t.Fatal(err)
		}
		if d.Reexec || !d.Guarded {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("guard outside parent does not loop", func(t *testing.T) {
		d, err := decideScopedSelfReexec(parent, mount, parent, outside)
		if err != nil {
			t.Fatal(err)
		}
		if d.Reexec || !d.Guarded {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("mismatched guard reexecs", func(t *testing.T) {
		d, err := decideScopedSelfReexec("/other/parent", mount, parent, outside)
		if err != nil {
			t.Fatal(err)
		}
		if !d.Reexec || d.Guarded {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("invalid parent rejected", func(t *testing.T) {
		if _, err := decideScopedSelfReexec("", mount, "relative", outside); err == nil {
			t.Fatal("relative parent accepted")
		}
		if _, err := decideScopedSelfReexec("", mount, mount, outside); err == nil {
			t.Fatal("mount root accepted")
		}
	})
}

func TestWithEnvReplacesGuard(t *testing.T) {
	env := []string{"A=1", scopedExecGuardEnv + "=stale", "B=2"}
	got := withEnv(env, scopedExecGuardEnv, "/delegated/parent")
	var count int
	for _, e := range got {
		if strings.HasPrefix(e, scopedExecGuardEnv+"=") {
			count++
			if e != scopedExecGuardEnv+"=/delegated/parent" {
				t.Fatalf("guard assignment = %q", e)
			}
		}
	}
	if count != 1 {
		t.Fatalf("guard assignments = %d in %v", count, got)
	}
}

func TestScopedExecApplyReexecsOutsideParent(t *testing.T) {
	const mount = "/sys/fs/cgroup"
	const parent = "/sys/fs/cgroup/user.slice/user-1001.slice/user@1001.service"
	const outside = "/user.slice/user-1001.slice/session-1.scope"

	var (
		called  bool
		gotName string
		gotArgv []string
		gotEnv  []string
	)
	s := scopedExec{
		mount:      mount,
		parent:     parent,
		args:       []string{"--cgroup-parent", parent, "inspect"},
		systemdRun: "systemd-run",
		getenv:     func(string) string { return "" },
		getpid:     func() int { return 42 },
		cgroupOf:   func(int) (string, error) { return outside, nil },
		executable: func() (string, error) { return "/opt/iris-bench/bin/iris-bench-agent", nil },
		lookPath:   func(string) (string, error) { return "/usr/bin/systemd-run", nil },
		exec: func(name string, argv, env []string) error {
			called = true
			gotName = name
			gotArgv = argv
			gotEnv = env
			return nil
		},
	}
	if err := s.apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !called {
		t.Fatal("outside-parent agent did not self-reexec")
	}
	if gotName != "/usr/bin/systemd-run" {
		t.Fatalf("exec name = %q", gotName)
	}
	for _, want := range []string{"--scope", "--collect", "--", "/opt/iris-bench/bin/iris-bench-agent", "inspect"} {
		if !containsArgument(gotArgv, want) {
			t.Fatalf("exec argv missing %q: %v", want, gotArgv)
		}
	}
	guard := scopedExecGuardEnv + "=" + parent
	if !containsArgument(gotEnv, guard) {
		t.Fatalf("exec env missing %q: %v", guard, gotEnv)
	}
}

func TestScopedExecApplyAlreadyWithinParent(t *testing.T) {
	const mount = "/sys/fs/cgroup"
	const parent = "/sys/fs/cgroup/user.slice/user-1001.slice/user@1001.service"
	const inside = "/user.slice/user-1001.slice/user@1001.service/app.slice/run-abc.scope"

	s := scopedExec{
		mount:    mount,
		parent:   parent,
		getenv:   func(string) string { return "" },
		getpid:   func() int { return 42 },
		cgroupOf: func(int) (string, error) { return inside, nil },
		exec: func(string, []string, []string) error {
			t.Fatal("already-contained agent must not re-exec")
			return nil
		},
	}
	if err := s.apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}
}

func TestScopedExecApplyGuardedChildProceedsWithinParent(t *testing.T) {
	const mount = "/sys/fs/cgroup"
	const parent = "/sys/fs/cgroup/user.slice/user-1001.slice/user@1001.service"
	const inside = "/user.slice/user-1001.slice/user@1001.service/app.slice/run-abc.scope"

	s := scopedExec{
		mount:    mount,
		parent:   parent,
		getenv:   func(string) string { return parent },
		getpid:   func() int { return 42 },
		cgroupOf: func(int) (string, error) { return inside, nil },
		exec: func(string, []string, []string) error {
			t.Fatal("guarded child must not re-exec")
			return nil
		},
	}
	if err := s.apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}
}

func TestScopedExecApplyGuardedChildFailsClosedOutsideParent(t *testing.T) {
	const mount = "/sys/fs/cgroup"
	const parent = "/sys/fs/cgroup/user.slice/user-1001.slice/user@1001.service"
	const outside = "/user.slice/user-1001.slice/session-1.scope"

	s := scopedExec{
		mount:    mount,
		parent:   parent,
		getenv:   func(string) string { return parent },
		getpid:   func() int { return 42 },
		cgroupOf: func(int) (string, error) { return outside, nil },
		exec: func(string, []string, []string) error {
			t.Fatal("guarded child must not re-exec")
			return nil
		},
	}
	err := s.apply()
	if err == nil || !strings.Contains(err.Error(), "not beneath") {
		t.Fatalf("guarded child error = %v, want fail-closed membership error", err)
	}
}

func TestScopedExecApplyWithoutParentDoesNothing(t *testing.T) {
	s := scopedExec{
		getenv: func(string) string { return "" },
		cgroupOf: func(int) (string, error) {
			t.Fatal("cgroup must not be read without a configured parent")
			return "", nil
		},
		exec: func(string, []string, []string) error {
			t.Fatal("no parent must not re-exec")
			return nil
		},
	}
	if err := s.apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}
}

func TestScopedExecApplyRejectsInvalidParent(t *testing.T) {
	s := scopedExec{
		mount:    "/sys/fs/cgroup",
		parent:   "/etc/not-a-cgroup",
		getenv:   func(string) string { return "" },
		getpid:   func() int { return 42 },
		cgroupOf: func(int) (string, error) { return "/session-1.scope", nil },
	}
	if err := s.apply(); err == nil {
		t.Fatal("invalid delegated parent accepted")
	}
}

func TestScopedExecApplyRejectsNonAbsoluteExecutable(t *testing.T) {
	const parent = "/sys/fs/cgroup/user.slice/user-1001.slice/user@1001.service"
	s := scopedExec{
		mount:      "/sys/fs/cgroup",
		parent:     parent,
		getenv:     func(string) string { return "" },
		getpid:     func() int { return 42 },
		cgroupOf:   func(int) (string, error) { return "/session-1.scope", nil },
		executable: func() (string, error) { return "iris-bench-agent", nil },
		exec: func(string, []string, []string) error {
			t.Fatal("non-absolute executable must not be exec'd")
			return nil
		},
	}
	if err := s.apply(); err == nil || !strings.Contains(err.Error(), "not absolute") {
		t.Fatalf("error = %v, want non-absolute executable rejection", err)
	}
}

func TestScopedExecApplyRejectsMissingLauncher(t *testing.T) {
	const parent = "/sys/fs/cgroup/user.slice/user-1001.slice/user@1001.service"
	s := scopedExec{
		mount:      "/sys/fs/cgroup",
		parent:     parent,
		getenv:     func(string) string { return "" },
		getpid:     func() int { return 42 },
		cgroupOf:   func(int) (string, error) { return "/session-1.scope", nil },
		executable: func() (string, error) { return "/opt/iris-bench/bin/iris-bench-agent", nil },
		lookPath: func(string) (string, error) {
			return "", errors.New("executable file not found in $PATH")
		},
		exec: func(string, []string, []string) error {
			t.Fatal("missing launcher must not be exec'd")
			return nil
		},
	}
	if err := s.apply(); err == nil || !strings.Contains(err.Error(), "locate systemd-run") {
		t.Fatalf("error = %v, want missing launcher rejection", err)
	}
}

func TestScopedExecApplyRejectsRelativeLauncher(t *testing.T) {
	const parent = "/sys/fs/cgroup/user.slice/user-1001.slice/user@1001.service"
	s := scopedExec{
		mount:      "/sys/fs/cgroup",
		parent:     parent,
		getenv:     func(string) string { return "" },
		getpid:     func() int { return 42 },
		cgroupOf:   func(int) (string, error) { return "/session-1.scope", nil },
		executable: func() (string, error) { return "/opt/iris-bench/bin/iris-bench-agent", nil },
		lookPath:   func(string) (string, error) { return "systemd-run", nil },
		exec: func(string, []string, []string) error {
			t.Fatal("relative launcher must not be exec'd")
			return nil
		},
	}
	if err := s.apply(); err == nil || !strings.Contains(err.Error(), "non-absolute") {
		t.Fatalf("error = %v, want relative launcher rejection", err)
	}
}
