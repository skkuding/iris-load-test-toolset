package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// lock takes an exclusive per-run lock in a host-wide tmpfs lock directory. It
// returns a release function. A held lock means another agent operation for the
// same run is in progress and the caller must not mutate host state. Keeping
// locks outside rtDir prevents read-only post-cleanup operations such as bundle
// from recreating an otherwise-clean run directory.
func (a *agent) lock(runID string) (func(), error) {
	dir := filepath.Join(a.runRoot, ".locks")
	if err := os.MkdirAll(dir, 0o770); err != nil {
		return nil, fmt.Errorf("create runtime lock dir: %w", err)
	}
	path := filepath.Join(dir, runID+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("run %s is locked by another operation", runID)
		}
		return nil, err
	}
	if _, err := f.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	release := func() {
		_ = f.Close()
		_ = os.Remove(path)
	}
	return release, nil
}
