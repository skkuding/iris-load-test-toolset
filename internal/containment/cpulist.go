package containment

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ParseCPUList parses a Linux CPU list such as "0-3,7,9-10" into a sorted,
// de-duplicated slice. It rejects empty, unsorted, or reversed ranges.
func ParseCPUList(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("%w: empty cpu list", ErrInvalid)
	}
	seen := map[int]struct{}{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("%w: empty cpu range in %q", ErrInvalid, s)
		}
		lo, hi, err := parseRange(part)
		if err != nil {
			return nil, err
		}
		for c := lo; c <= hi; c++ {
			seen[c] = struct{}{}
		}
	}
	out := make([]int, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Ints(out)
	return out, nil
}

func parseRange(part string) (int, int, error) {
	loStr, hiStr, hasDash := strings.Cut(part, "-")
	lo, err := strconv.Atoi(strings.TrimSpace(loStr))
	if err != nil || lo < 0 {
		return 0, 0, fmt.Errorf("%w: bad cpu %q", ErrInvalid, part)
	}
	if !hasDash {
		return lo, lo, nil
	}
	hi, err := strconv.Atoi(strings.TrimSpace(hiStr))
	if err != nil || hi < 0 {
		return 0, 0, fmt.Errorf("%w: bad cpu %q", ErrInvalid, part)
	}
	if hi < lo {
		return 0, 0, fmt.Errorf("%w: reversed cpu range %q", ErrInvalid, part)
	}
	return lo, hi, nil
}

// FormatCPUList renders a CPU set in compact range form.
func FormatCPUList(cpus []int) string {
	if len(cpus) == 0 {
		return ""
	}
	sorted := append([]int(nil), cpus...)
	sort.Ints(sorted)
	var parts []string
	start, prev := sorted[0], sorted[0]
	flush := func(lo, hi int) {
		if lo == hi {
			parts = append(parts, strconv.Itoa(lo))
			return
		}
		parts = append(parts, strconv.Itoa(lo)+"-"+strconv.Itoa(hi))
	}
	for _, c := range sorted[1:] {
		if c == prev || c == prev+1 {
			prev = c
			continue
		}
		flush(start, prev)
		start, prev = c, c
	}
	flush(start, prev)
	return strings.Join(parts, ",")
}

// EqualCPUSets reports whether two CPU list strings describe the same set.
func EqualCPUSets(a, b string) bool {
	as, err := ParseCPUList(a)
	if err != nil {
		return false
	}
	bs, err := ParseCPUList(b)
	if err != nil {
		return false
	}
	if len(as) != len(bs) {
		return false
	}
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// SplitCPUList partitions cpus across workers round-robin, then formats each
// worker's assignment. It fails rather than oversubscribing: every worker must
// receive at least one CPU, and no CPU is shared between workers.
func SplitCPUList(list string, workers int) ([]string, error) {
	if workers < 1 {
		return nil, fmt.Errorf("%w: workers must be positive", ErrInvalid)
	}
	cpus, err := ParseCPUList(list)
	if err != nil {
		return nil, err
	}
	if len(cpus) < workers {
		return nil, fmt.Errorf("%w: cpu list %q has %d cpus for %d workers", ErrInvalid, list, len(cpus), workers)
	}
	out := make([]string, workers)
	for i, c := range cpus {
		w := i % workers
		out[w] = appendCPU(out[w], c)
	}
	for i, s := range out {
		parsed, err := ParseCPUList(s)
		if err != nil {
			return nil, err
		}
		out[i] = FormatCPUList(parsed)
	}
	return out, nil
}

func appendCPU(existing string, cpu int) string {
	if existing == "" {
		return strconv.Itoa(cpu)
	}
	return existing + "," + strconv.Itoa(cpu)
}

// CountCPUs returns the number of CPUs in a list string.
func CountCPUs(list string) (int, error) {
	cpus, err := ParseCPUList(list)
	if err != nil {
		return 0, err
	}
	return len(cpus), nil
}
