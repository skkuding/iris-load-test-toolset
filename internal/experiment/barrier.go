package experiment

import (
	"fmt"
	"io"
	"os"
	"sync"
)

// ReadyBarrier is a local readiness barrier for worker processes.
//
// Every worker is launched with two pipe descriptors. It writes one byte to
// its ready pipe to signal readiness, then blocks reading one byte from the
// release pipe. The coordinator waits for one byte from every worker before
// writing the release bytes. Workers therefore do not begin measured work
// until all of them have started and reached the gate.
type ReadyBarrier struct {
	readyR   *os.File
	readyW   *os.File
	releaseR *os.File
	releaseW *os.File
	count    int
	closed   bool
	mu       sync.Mutex
}

// NewReadyBarrier creates a barrier for count workers.
func NewReadyBarrier(count int) (*ReadyBarrier, error) {
	if count < 1 {
		return nil, fmt.Errorf("experiment: barrier requires at least one worker")
	}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	releaseR, releaseW, err := os.Pipe()
	if err != nil {
		_ = readyR.Close()
		_ = readyW.Close()
		return nil, err
	}
	return &ReadyBarrier{readyR: readyR, readyW: readyW, releaseR: releaseR, releaseW: releaseW, count: count}, nil
}

// BarrierGate is one worker's end of the barrier.
type BarrierGate struct {
	release *os.File
	ready   *os.File
}

// Files returns the descriptors the worker wrapper must inherit as fd 3
// (release) and fd 4 (ready).
func (g *BarrierGate) Files() (release, ready *os.File) { return g.release, g.ready }

// NewGate returns the worker end of the barrier.
func (b *ReadyBarrier) NewGate() *BarrierGate {
	return &BarrierGate{release: b.releaseR, ready: b.readyW}
}

// WaitReady blocks until every worker has signalled readiness, the context is
// done, or the barrier is closed.
func (b *ReadyBarrier) WaitReady(ctxDone <-chan struct{}) error {
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		for i := 0; i < b.count; i++ {
			if _, err := io.ReadFull(b.readyR, buf); err != nil {
				b.mu.Lock()
				closed := b.closed
				b.mu.Unlock()
				if closed {
					done <- fmt.Errorf("experiment: barrier closed before all workers were ready")
					return
				}
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case <-ctxDone:
		return fmt.Errorf("experiment: readiness barrier timed out")
	case err := <-done:
		return err
	}
}

// Release unblocks every waiting worker.
func (b *ReadyBarrier) Release() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return fmt.Errorf("experiment: barrier is closed")
	}
	buf := make([]byte, 1)
	for i := 0; i < b.count; i++ {
		if _, err := b.releaseW.Write(buf); err != nil {
			return err
		}
	}
	return nil
}

// Close releases the barrier resources. It is safe to call more than once.
func (b *ReadyBarrier) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	var first error
	for _, f := range []*os.File{b.readyR, b.readyW, b.releaseR, b.releaseW} {
		if f == nil {
			continue
		}
		if err := f.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
