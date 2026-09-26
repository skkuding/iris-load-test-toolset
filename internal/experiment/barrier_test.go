package experiment

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

func TestReadyBarrierGatesWorkersUntilRelease(t *testing.T) {
	b, err := NewReadyBarrier(3)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	var mu sync.Mutex
	ready := 0
	released := 0
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		gate := b.NewGate()
		release, readyFile := gate.Files()
		wg.Add(1)
		go func() {
			defer wg.Done()
			mu.Lock()
			ready++
			mu.Unlock()
			if _, err := readyFile.Write([]byte{1}); err != nil {
				return
			}
			buf := make([]byte, 1)
			if _, err := io.ReadFull(release, buf); err != nil {
				return
			}
			mu.Lock()
			released++
			mu.Unlock()
		}()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := b.WaitReady(ctx.Done()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotReady := ready
	gotReleasedBefore := released
	mu.Unlock()
	if gotReady != 3 {
		t.Fatalf("ready = %d, want 3", gotReady)
	}
	if gotReleasedBefore != 0 {
		t.Fatalf("workers released before Release: %d", gotReleasedBefore)
	}
	if err := b.Release(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if released != 3 {
		t.Fatalf("released = %d, want 3", released)
	}
}

func TestReadyBarrierWaitReadyTimesOut(t *testing.T) {
	b, err := NewReadyBarrier(1)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.WaitReady(ctx.Done()); err == nil {
		t.Fatal("expected timeout error")
	}
}
