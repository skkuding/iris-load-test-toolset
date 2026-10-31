// iris-bench-rdt samples passive Intel RDT counters while a benchmark block runs.
// Run it in a privileged, network-disabled container with an output bind mount.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type counters struct {
	LLCOccupancyBytes uint64 `json:"llcOccupancyBytes"`
	MBMTotalBytes     uint64 `json:"mbmTotalBytes"`
	MBMLocalBytes     uint64 `json:"mbmLocalBytes"`
}

type record struct {
	SchemaVersion int                 `json:"schemaVersion"`
	Scope         string              `json:"scope"`
	ObservedAtNs  int64               `json:"observedAtNs"`
	Domains       map[string]counters `json:"domains"`
}

func main() {
	var output, ready, mountpoint string
	var interval time.Duration
	flag.StringVar(&output, "output", "", "host-mounted NDJSON output path")
	flag.StringVar(&ready, "ready", "", "host-mounted readiness marker")
	flag.StringVar(&mountpoint, "mount", "/sys/fs/resctrl", "resctrl mount inside this container")
	flag.DurationVar(&interval, "interval", time.Second, "sampling interval")
	flag.Parse()
	if output == "" || ready == "" || interval < 100*time.Millisecond || !filepath.IsAbs(output) || !filepath.IsAbs(ready) {
		fmt.Fprintln(os.Stderr, "absolute --output and --ready paths and interval >= 100ms are required")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, terminationSignal())
	defer stop()
	if err := run(ctx, mountpoint, output, ready, interval); err != nil {
		fmt.Fprintln(os.Stderr, "iris-bench-rdt:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, mountpoint, output, ready string, interval time.Duration) error {
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		return err
	}
	if err := mountResctrl(mountpoint); err != nil {
		return fmt.Errorf("mount resctrl: %w", err)
	}
	defer unmountResctrl(mountpoint)
	if err := checkDefaultGroup(mountpoint); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	sample := func() error {
		r, err := readRecord(mountpoint, time.Now().UnixNano())
		if err != nil {
			return err
		}
		if err := json.NewEncoder(w).Encode(r); err != nil {
			return err
		}
		return w.Flush()
	}
	if err := sample(); err != nil {
		return err
	}
	if err := os.WriteFile(ready, []byte("ready\n"), 0o644); err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return sample()
		case <-ticker.C:
			if err := sample(); err != nil {
				return err
			}
		}
	}
}

func checkDefaultGroup(root string) error {
	data, err := os.ReadFile(filepath.Join(root, "info", "L3_MON", "mon_features"))
	if err != nil {
		return fmt.Errorf("L3 monitoring unavailable: %w", err)
	}
	for _, name := range []string{"llc_occupancy", "mbm_total_bytes", "mbm_local_bytes"} {
		if !strings.Contains("\n"+string(data)+"\n", "\n"+name+"\n") {
			return fmt.Errorf("L3 monitoring lacks %s", name)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() && e.Name() != "info" && e.Name() != "mon_data" && e.Name() != "mon_groups" {
			return fmt.Errorf("non-default resctrl group %q prevents host-default attribution", e.Name())
		}
	}
	groups, err := os.ReadDir(filepath.Join(root, "mon_groups"))
	if err != nil {
		return err
	}
	if len(groups) != 0 {
		return errors.New("non-default resctrl monitoring groups prevent host-default attribution")
	}
	return nil
}

func readRecord(root string, at int64) (record, error) {
	paths, err := filepath.Glob(filepath.Join(root, "mon_data", "mon_L3_*"))
	if err != nil || len(paths) == 0 {
		return record{}, errors.New("no L3 monitoring domains")
	}
	sort.Strings(paths)
	r := record{SchemaVersion: 1, Scope: "resctrl-default-host", ObservedAtNs: at, Domains: make(map[string]counters, len(paths))}
	for _, dir := range paths {
		read := func(name string) (uint64, error) {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return 0, err
			}
			v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("%s/%s: %w", dir, name, err)
			}
			return v, nil
		}
		var c counters
		if c.LLCOccupancyBytes, err = read("llc_occupancy"); err != nil {
			return record{}, err
		}
		if c.MBMTotalBytes, err = read("mbm_total_bytes"); err != nil {
			return record{}, err
		}
		if c.MBMLocalBytes, err = read("mbm_local_bytes"); err != nil {
			return record{}, err
		}
		r.Domains[filepath.Base(dir)] = c
	}
	return r, nil
}
