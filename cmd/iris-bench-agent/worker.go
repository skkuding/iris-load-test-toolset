package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/skkuding/iris-load-test-toolset/internal/containment"
)

// runWorkerExec implements the internal "worker-exec" wrapper used by the
// experiment launcher. It attaches itself to the assigned cgroup, verifies the
// membership from /proc, signals the readiness barrier, waits for release, and
// only then replaces itself with the measured command. Measured user code
// therefore never runs outside the verified subtree.
//
// The wrapper is part of this binary rather than a shell script so it cannot be
// re-interpreted or substituted by untrusted input, and so the benchmark
// carries no separate privileged helper.
func runWorkerExec(args []string) error {
	fs := flag.NewFlagSet("worker-exec", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		cgroup    = fs.String("cgroup", "", "absolute cgroup filesystem path")
		mount     = fs.String("cgroup-mount", containment.DefaultMount, "cgroup v2 mount")
		releaseFD = fs.Int("release-fd", 3, "inherited release pipe descriptor")
		readyFD   = fs.Int("ready-fd", 4, "inherited readiness pipe descriptor")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if *cgroup == "" {
		return errors.New("worker-exec: --cgroup is required")
	}
	if len(rest) == 0 {
		return errors.New("worker-exec: a command is required after --")
	}

	pid := os.Getpid()
	if err := containment.AttachPID(containment.RealFS{}, *cgroup, pid); err != nil {
		return fmt.Errorf("worker-exec: attach pid %d: %w", pid, err)
	}
	mgr := &containment.Manager{Mount: *mount, Proc: containment.RealProc{}}
	want, err := mgr.RelToMount(*cgroup)
	if err != nil {
		return fmt.Errorf("worker-exec: %w", err)
	}
	got, err := mgr.CgroupOf(pid)
	if err != nil {
		return fmt.Errorf("worker-exec: verify membership: %w", err)
	}
	if !containment.IsWithin(want, got) {
		return fmt.Errorf("worker-exec: pid %d is in cgroup %q, want %q", pid, got, want)
	}

	ready := os.NewFile(uintptr(*readyFD), "barrier-ready")
	release := os.NewFile(uintptr(*releaseFD), "barrier-release")
	if ready == nil || release == nil {
		return errors.New("worker-exec: invalid barrier descriptors")
	}
	if _, err := ready.Write([]byte{1}); err != nil {
		return fmt.Errorf("worker-exec: signal ready: %w", err)
	}
	if _, err := io.ReadFull(release, make([]byte, 1)); err != nil {
		return fmt.Errorf("worker-exec: wait for release: %w", err)
	}
	// Drop the barrier descriptors so the measured process cannot interfere
	// with the barrier or keep the pipes open after release.
	_ = ready.Close()
	_ = release.Close()

	if err := syscall.Exec(rest[0], rest, os.Environ()); err != nil {
		return fmt.Errorf("worker-exec: exec %q: %w", rest[0], err)
	}
	return nil
}
