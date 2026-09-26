package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/skkuding/iris-load-test-toolset/internal/containment"
)

// scopedExecGuardEnv is a private recursion guard set on the agent process that
// systemd-run launches. The guarded child verifies its own cgroup instead of
// re-executing again, so a delegation that strands the agent outside the
// configured parent fails closed rather than looping forever.
const scopedExecGuardEnv = "IRIS_BENCH_AGENT_SCOPED_PARENT"

// defaultSystemdRun is the transient-scope launcher used for the self-reexec.
const defaultSystemdRun = "systemd-run"

// isWorkerExec reports whether argv selects the internal worker-exec wrapper.
// main dispatches that path before any self-reexec decision: the worker wrapper
// is already launched by the scoped agent and must never try to scope itself.
func isWorkerExec(argv []string) bool {
	return len(argv) > 1 && argv[1] == "worker-exec"
}

// scopedSelfReexec is the decision made before the agent reads any protocol
// input. The zero value means "proceed in place".
type scopedSelfReexec struct {
	// Reexec is true when the current process must be replaced by a fresh
	// agent running inside a systemd-run user scope.
	Reexec bool
	// Guarded is true when the private guard was already present, meaning this
	// process is the child launched by systemd-run.
	Guarded bool
	// RelParent is the delegated parent as a mount-relative cgroup path.
	RelParent string
}

// validateDelegatedParent confirms parent is an absolute path strictly beneath
// the cgroup v2 unified mount. The mount root itself is never accepted: joining
// it would let a run fall back to root-level cgroups.
func validateDelegatedParent(mount, parent string) error {
	if parent == "" {
		return errors.New("cgroup-parent is required to be non-empty")
	}
	if !filepath.IsAbs(parent) {
		return fmt.Errorf("cgroup-parent %q must be an absolute path", parent)
	}
	m := mount
	if m == "" {
		m = containment.DefaultMount
	}
	if !filepath.IsAbs(m) {
		return fmt.Errorf("cgroup-mount %q must be an absolute path", mount)
	}
	m = filepath.Clean(m)
	clean := filepath.Clean(parent)
	if clean == m {
		return fmt.Errorf("cgroup-parent %q is the cgroup mount root", parent)
	}
	if !strings.HasPrefix(clean, m+string(os.PathSeparator)) {
		return fmt.Errorf("cgroup-parent %q is not beneath cgroup mount %q", parent, m)
	}
	return nil
}

// decideScopedSelfReexec is the pure decision boundary. currentCgroup is the
// mount-relative cgroup path of the running process. A non-empty parent is
// required; the parent is validated before any comparison. The guard value is
// compared against the configured parent so a stale guard from another run does
// not suppress a legitimate re-exec.
func decideScopedSelfReexec(guardValue, mount, parent, currentCgroup string) (scopedSelfReexec, error) {
	var d scopedSelfReexec
	if parent == "" {
		return d, nil
	}
	if err := validateDelegatedParent(mount, parent); err != nil {
		return d, err
	}
	rel, err := (&containment.Manager{Mount: mount}).RelToMount(parent)
	if err != nil {
		return d, err
	}
	d.RelParent = rel
	if guardValue == parent {
		d.Guarded = true
		return d, nil
	}
	d.Reexec = !containment.IsWithin(rel, currentCgroup)
	return d, nil
}

// selfReexecArgv builds the argv for the systemd-run self-reexec. It never uses
// a shell: every argument is a separate argv entry, and the original argument
// vector is passed through unchanged after "--". systemd-run runs scope
// commands synchronously and propagates their exit status while inheriting the
// caller's environment and standard descriptors.
func selfReexecArgv(systemdRun, executable string, args []string) []string {
	argv := []string{
		systemdRun,
		"--user",
		"--scope",
		"--quiet",
		"--collect",
		"--",
		executable,
	}
	return append(argv, args...)
}

// withEnv returns env with every existing assignment of key removed and a
// single assignment appended, so a caller-supplied duplicate cannot shadow the
// guard value.
func withEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			continue
		}
		out = append(out, e)
	}
	return append(out, prefix+value)
}

// scopedExec carries the OS boundaries needed to decide on and perform the
// self-reexec. Every field is injectable so the decision can be exercised
// without touching cgroups or replacing the test process image.
type scopedExec struct {
	mount      string
	parent     string
	args       []string
	systemdRun string

	getenv     func(string) string
	getpid     func() int
	executable func() (string, error)
	cgroupOf   func(int) (string, error)
	lookPath   func(string) (string, error)
	exec       func(string, []string, []string) error
}

func (s scopedExec) withDefaults() scopedExec {
	if s.systemdRun == "" {
		s.systemdRun = defaultSystemdRun
	}
	if s.lookPath == nil {
		s.lookPath = exec.LookPath
	}
	if s.getenv == nil {
		s.getenv = os.Getenv
	}
	if s.getpid == nil {
		s.getpid = os.Getpid
	}
	if s.executable == nil {
		s.executable = os.Executable
	}
	if s.cgroupOf == nil {
		s.cgroupOf = func(pid int) (string, error) { return containment.RealProc{}.Cgroup(pid) }
	}
	if s.exec == nil {
		s.exec = syscall.Exec
	}
	return s
}

// apply decides whether the agent must relaunch itself inside the delegated
// parent and, when required, replaces the process image. It returns nil when
// the process may proceed. On the guarded path it verifies the current cgroup
// is beneath the configured parent and fails closed otherwise.
func (s scopedExec) apply() error {
	s = s.withDefaults()
	current := ""
	if s.parent != "" {
		got, err := s.cgroupOf(s.getpid())
		if err != nil {
			return fmt.Errorf("self-reexec: read current cgroup: %w", err)
		}
		current = got
	}
	d, err := decideScopedSelfReexec(s.getenv(scopedExecGuardEnv), s.mount, s.parent, current)
	if err != nil {
		return fmt.Errorf("self-reexec: %w", err)
	}
	if d.Guarded {
		if !containment.IsWithin(d.RelParent, current) {
			return fmt.Errorf("self-reexec: current cgroup %q is not beneath delegated parent %q", current, s.parent)
		}
		return nil
	}
	if !d.Reexec {
		return nil
	}
	exe, err := s.executable()
	if err != nil {
		return fmt.Errorf("self-reexec: locate executable: %w", err)
	}
	if !filepath.IsAbs(exe) {
		return fmt.Errorf("self-reexec: executable %q is not absolute", exe)
	}
	// syscall.Exec does not search PATH, so resolve the launcher first.
	launcher, err := s.lookPath(s.systemdRun)
	if err != nil {
		return fmt.Errorf("self-reexec: locate %s: %w", s.systemdRun, err)
	}
	if !filepath.IsAbs(launcher) {
		return fmt.Errorf("self-reexec: %s resolved to non-absolute path %q", s.systemdRun, launcher)
	}
	argv := selfReexecArgv(s.systemdRun, exe, s.args)
	env := withEnv(os.Environ(), scopedExecGuardEnv, s.parent)
	if err := s.exec(launcher, argv, env); err != nil {
		return fmt.Errorf("self-reexec: %w", err)
	}
	return nil
}
