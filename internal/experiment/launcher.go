package experiment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/skkuding/iris-load-test-toolset/internal/containment"
)

// ExecLauncher starts worker processes through a small exec wrapper that
// attaches itself to the assigned cgroup, signals the readiness barrier, waits
// for release, and only then executes the measured command. Timing-sensitive
// work never begins before containment is verified.
type ExecLauncher struct {
	// Wrapper is the absolute path to the binary implementing "worker-exec".
	Wrapper string
	// Mount is the cgroup v2 mount the wrapper reports membership against.
	Mount string
	// Stderr receives worker process output.
	Stderr io.Writer
	// WaitDelay bounds how long Stop waits after SIGKILL.
	WaitDelay time.Duration
	// Barrier is created by NewExecLauncher.
	Barrier *ReadyBarrier

	mu      sync.Mutex
	started []*execWorker
	closed  bool
}

// NewExecLauncher creates a launcher and its readiness barrier for workers.
func NewExecLauncher(workers int, wrapper string) (*ExecLauncher, error) {
	if !filepath.IsAbs(wrapper) {
		return nil, errors.New("experiment: worker wrapper must be an absolute path")
	}
	b, err := NewReadyBarrier(workers)
	if err != nil {
		return nil, err
	}
	return &ExecLauncher{Wrapper: wrapper, Barrier: b, WaitDelay: 2 * time.Second}, nil
}

// Start implements Launcher.
func (l *ExecLauncher) Start(_ context.Context, spec WorkerSpec, h containment.Handle) (Worker, error) {
	if l.Barrier == nil {
		return nil, errors.New("experiment: launcher barrier is not initialized")
	}
	command, err := exec.LookPath(spec.Command)
	if err != nil {
		return nil, fmt.Errorf("experiment: locate worker command %q: %w", spec.Command, err)
	}
	gate := l.Barrier.NewGate()
	release, ready := gate.Files()
	args := []string{
		"worker-exec",
		"--cgroup", h.FSPath(),
		"--cgroup-mount", l.Mount,
		"--release-fd", "3",
		"--ready-fd", "4",
		"--",
		command,
	}
	args = append(args, spec.Args...)
	cmd := exec.Command(l.Wrapper, args...)
	cmd.ExtraFiles = []*os.File{release, ready}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if l.Stderr != nil {
		cmd.Stdout = l.Stderr
		cmd.Stderr = l.Stderr
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("experiment: start worker %s: %w", spec.ID, err)
	}
	w := newExecWorker(cmd, l.WaitDelay)
	l.mu.Lock()
	l.started = append(l.started, w)
	l.mu.Unlock()
	return w, nil
}

// WaitReady implements Launcher.
func (l *ExecLauncher) WaitReady(ctx context.Context) error {
	if l.Barrier == nil {
		return errors.New("experiment: launcher barrier is not initialized")
	}
	return l.Barrier.WaitReady(ctx.Done())
}

// Release implements Launcher.
func (l *ExecLauncher) Release() error {
	if l.Barrier == nil {
		return errors.New("experiment: launcher barrier is not initialized")
	}
	return l.Barrier.Release()
}

// Close stops any remaining workers and releases the barrier. It is safe to
// call more than once.
func (l *ExecLauncher) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	started := append([]*execWorker(nil), l.started...)
	l.mu.Unlock()

	for _, w := range started {
		_ = w.Stop()
	}
	if l.Barrier != nil {
		return l.Barrier.Close()
	}
	return nil
}

// execWorker owns one started worker process group.
type execWorker struct {
	pid       int
	done      chan struct{}
	err       error
	waitDelay time.Duration
}

func newExecWorker(cmd *exec.Cmd, waitDelay time.Duration) *execWorker {
	if waitDelay <= 0 {
		waitDelay = 2 * time.Second
	}
	w := &execWorker{pid: cmd.Process.Pid, done: make(chan struct{}), waitDelay: waitDelay}
	go func() {
		w.err = cmd.Wait()
		close(w.done)
	}()
	return w
}

// PID implements Worker.
func (w *execWorker) PID() int { return w.pid }

// Wait implements Worker. The error is written before done is closed, so the
// receive establishes the necessary happens-before relationship.
func (w *execWorker) Wait() error {
	<-w.done
	return w.err
}

// Stop implements Worker. It kills the whole process group and waits briefly
// for the OS to reap it.
func (w *execWorker) Stop() error {
	if w.pid <= 0 {
		return nil
	}
	_ = syscall.Kill(-w.pid, syscall.SIGKILL)
	select {
	case <-w.done:
	case <-time.After(w.waitDelay):
		_ = syscall.Kill(-w.pid, syscall.SIGKILL)
	}
	return nil
}
