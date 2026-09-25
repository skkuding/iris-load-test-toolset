package containment

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// RealProc reads process cgroup membership and the process tree from /proc.
type RealProc struct {
	// Root overrides the procfs mount. Empty means "/proc".
	Root string
}

func (p RealProc) root() string {
	if p.Root == "" {
		return "/proc"
	}
	return p.Root
}

// Cgroup returns the unified cgroup v2 path recorded in /proc/<pid>/cgroup.
func (p RealProc) Cgroup(pid int) (string, error) {
	data, err := os.ReadFile(filepath.Join(p.root(), strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		// cgroup v2 lines are "0::/some/path".
		hierarchy, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		controllers, path, ok := strings.Cut(rest, ":")
		if !ok {
			continue
		}
		if hierarchy == "0" && controllers == "" {
			path = strings.TrimSpace(path)
			if path == "" {
				return "/", nil
			}
			return path, nil
		}
	}
	return "", fmt.Errorf("containment: no cgroup v2 entry for pid %d", pid)
}

// Exists reports whether the process exists.
func (p RealProc) Exists(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	_, err := os.Stat(filepath.Join(p.root(), strconv.Itoa(pid)))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// Descendants returns pid and all descendants by walking thread children
// files. A process that exits during the walk is treated as a leaf.
func (p RealProc) Descendants(pid int) ([]int, error) {
	seen := map[int]struct{}{}
	queue := []int{pid}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if _, ok := seen[cur]; ok {
			continue
		}
		seen[cur] = struct{}{}
		children, err := p.directChildren(cur)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		queue = append(queue, children...)
	}
	out := make([]int, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Ints(out)
	return out, nil
}

func (p RealProc) directChildren(pid int) ([]int, error) {
	taskDir := filepath.Join(p.root(), strconv.Itoa(pid), "task")
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return nil, err
	}
	seen := map[int]struct{}{}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(taskDir, e.Name(), "children"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		for _, f := range strings.Fields(string(data)) {
			c, err := strconv.Atoi(f)
			if err != nil {
				continue
			}
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
