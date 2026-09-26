package telemetry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/skkuding/iris-load-test-toolset/internal/artifact"
)

var processStart = time.Now()

// Snapshot is one reading of the host thermal and CPU throttling interfaces.
type Snapshot struct {
	TemperaturesMilliC map[string]int64  `json:"temperaturesMilliC,omitempty"`
	ThrottleCounters   map[string]uint64 `json:"throttleCounters,omitempty"`
}

// Sampler reads host telemetry at a point in time.
type Sampler interface {
	Snapshot() (Snapshot, error)
}

// SysfsSampler reads Linux thermal zones and CPU thermal throttle counters.
// Root is injectable for tests and defaults to the host filesystem root.
type SysfsSampler struct {
	Root string
}

// Snapshot reads every available supported sysfs source. It fails closed when
// the host exposes no usable source.
func (s SysfsSampler) Snapshot() (Snapshot, error) {
	root := s.Root
	if root == "" {
		root = string(os.PathSeparator)
	}
	patterns := []struct {
		pattern  string
		throttle bool
	}{
		{"sys/class/thermal/thermal_zone*/temp", false},
		{"sys/devices/system/cpu/cpu*/thermal_throttle/core_throttle_count", true},
		{"sys/devices/system/cpu/cpu*/thermal_throttle/package_throttle_count", true},
	}
	out := Snapshot{TemperaturesMilliC: map[string]int64{}, ThrottleCounters: map[string]uint64{}}
	for _, source := range patterns {
		paths, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(source.pattern)))
		if err != nil {
			return Snapshot{}, err
		}
		sort.Strings(paths)
		for _, path := range paths {
			data, err := os.ReadFile(path)
			if err != nil {
				return Snapshot{}, fmt.Errorf("read %s: %w", path, err)
			}
			value := strings.TrimSpace(string(data))
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return Snapshot{}, err
			}
			key := filepath.ToSlash(rel)
			if source.throttle {
				n, err := strconv.ParseUint(value, 10, 64)
				if err != nil {
					return Snapshot{}, fmt.Errorf("parse %s: %w", path, err)
				}
				out.ThrottleCounters[key] = n
				continue
			}
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return Snapshot{}, fmt.Errorf("parse %s: %w", path, err)
			}
			if n < -273150 || n > 250000 {
				return Snapshot{}, fmt.Errorf("temperature %s is outside the valid milli-Celsius range", path)
			}
			out.TemperaturesMilliC[key] = n
		}
	}
	if len(out.TemperaturesMilliC) == 0 && len(out.ThrottleCounters) == 0 {
		return Snapshot{}, errors.New("telemetry: no readable sysfs thermal temperatures or CPU throttle counters")
	}
	return out, nil
}

// Record is one NDJSON telemetry record.
type Record struct {
	SchemaVersion int               `json:"schemaVersion"`
	BlockID       string            `json:"blockId"`
	Phase         string            `json:"phase"`
	CapturedAt    time.Time         `json:"capturedAt"`
	MonotonicNS   int64             `json:"monotonicNs"`
	Temperatures  map[string]int64  `json:"temperaturesMilliC,omitempty"`
	Throttle      map[string]uint64 `json:"throttleCounters,omitempty"`
}

// Capture takes a telemetry snapshot immediately before and after run and
// atomically writes both records, including when run itself fails.
func Capture(path, blockID string, sampler Sampler, run func() error) error {
	if sampler == nil {
		return errors.New("telemetry: sampler is required")
	}
	before, err := sampleRecord(blockID, "before", sampler)
	if err != nil {
		return err
	}
	runErr := run()
	after, telemetryErr := sampleRecord(blockID, "after", sampler)
	if telemetryErr == nil {
		var data strings.Builder
		for _, record := range []Record{before, after} {
			line, err := json.Marshal(record)
			if err != nil {
				telemetryErr = err
				break
			}
			data.Write(line)
			data.WriteByte('\n')
		}
		if telemetryErr == nil {
			telemetryErr = artifact.WriteFileAtomic(path, []byte(data.String()), 0o644)
		}
	}
	return errors.Join(runErr, telemetryErr)
}

func sampleRecord(blockID, phase string, sampler Sampler) (Record, error) {
	snapshot, err := sampler.Snapshot()
	if err != nil {
		return Record{}, fmt.Errorf("telemetry %s snapshot: %w", phase, err)
	}
	monotonic := time.Since(processStart).Nanoseconds()
	if monotonic < 1 {
		monotonic = 1
	}
	return Record{
		SchemaVersion: 1,
		BlockID:       blockID,
		Phase:         phase,
		CapturedAt:    time.Now().UTC(),
		MonotonicNS:   monotonic,
		Temperatures:  snapshot.TemperaturesMilliC,
		Throttle:      snapshot.ThrottleCounters,
	}, nil
}
