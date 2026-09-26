// Package containment manages cgroup v2 subtrees for benchmark workers.
//
// The benchmark must prove that every compiler and submission process runs in
// the intended cgroup, CPU set, and NUMA memory set. This package never reports
// containment it cannot prove: a missing controller, an absent delegated
// parent, or an effective limit that differs from the request is an error, not
// a warning.
//
// All filesystem and process reads go through the FS and Proc interfaces so the
// logic can be exercised with fake filesystems in unprivileged CI, where cgroup
// delegation is unavailable.
package containment

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// DefaultMount is the conventional cgroup v2 unified mount point.
const DefaultMount = "/sys/fs/cgroup"

// Errors surfaced by this package.
var (
	// ErrUnsupported means the host is not a usable cgroup v2 host, a required
	// controller file is missing, or the operation is not permitted. Callers
	// must surface this rather than claim containment.
	ErrUnsupported = errors.New("containment: cgroup v2 containment unsupported")
	// ErrNotDelegated means the parent cgroup was not explicitly delegated to
	// the benchmark user.
	ErrNotDelegated = errors.New("containment: parent cgroup is not delegated")
	// ErrEscape means a process left its assigned subtree.
	ErrEscape = errors.New("containment: process escaped its cgroup")
	// ErrInvalid means the containment request is malformed.
	ErrInvalid = errors.New("containment: invalid containment request")
	// ErrNotEmpty means a cgroup still holds member processes or child cgroups
	// and therefore must not be removed.
	ErrNotEmpty = errors.New("containment: cgroup is not empty")
)

// RequiredControllers are the controllers a delegated parent must expose.
var RequiredControllers = []string{"cpu", "cpuset"}

// FS abstracts the filesystem operations used against the cgroup tree. The
// real implementation is the OS; tests substitute an in-memory tree.
type FS interface {
	ReadFile(name string) ([]byte, error)
	WriteFile(name string, data []byte, perm fs.FileMode) error
	MkdirAll(path string, perm fs.FileMode) error
	Remove(path string) error
	Stat(name string) (fs.FileInfo, error)
	ReadDir(name string) ([]fs.DirEntry, error)
}

// Proc abstracts the process boundary. CgroupOf reads /proc/<pid>/cgroup;
// Descendants walks the process tree; Exists tests liveness.
type Proc interface {
	// Cgroup returns the cgroup v2 path of pid relative to the unified mount,
	// for example "/iris-bench/run/worker-01".
	Cgroup(pid int) (string, error)
	// Descendants returns pid and every descendant of pid. Order is not
	// significant. A stale pid yields an error.
	Descendants(pid int) ([]int, error)
	// Exists reports whether pid currently exists.
	Exists(pid int) (bool, error)
}

// RealFS is the operating-system FS implementation.
type RealFS struct{}

// ReadFile implements FS.
func (RealFS) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

// WriteFile implements FS.
func (RealFS) WriteFile(name string, data []byte, perm fs.FileMode) error {
	return os.WriteFile(name, data, perm)
}

// MkdirAll implements FS.
func (RealFS) MkdirAll(path string, perm fs.FileMode) error { return os.MkdirAll(path, perm) }

// Remove implements FS.
func (RealFS) Remove(path string) error { return os.Remove(path) }

// Stat implements FS.
func (RealFS) Stat(name string) (fs.FileInfo, error) { return os.Stat(name) }

// ReadDir implements FS.
func (RealFS) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }

// Config describes one worker subtree.
type Config struct {
	// Parent is the absolute filesystem path of the explicitly delegated
	// parent cgroup, for example /sys/fs/cgroup/iris-bench.
	Parent string
	// Name is a relative subpath created below Parent. It must contain safe
	// path components and normally includes the run and block IDs so subtrees
	// are unique per run and per worker.
	Name string
	// CPUSet is written verbatim to cpuset.cpus.
	CPUSet string
	// Mems is written verbatim to cpuset.mems.
	Mems string
	// CPUMax is written verbatim to cpu.max, for example "100000 100000".
	CPUMax string
}

// Handle is one created worker subtree.
type Handle interface {
	// Name is the relative subtree name.
	Name() string
	// FSPath is the absolute filesystem path of the subtree.
	FSPath() string
	// RelPath is the cgroup path relative to the unified mount, as it appears
	// in /proc/<pid>/cgroup.
	RelPath() string
	// Config returns the request that created the subtree.
	Config() Config
	// Attach moves pid into the subtree.
	Attach(pid int) error
	// Verify re-reads the effective limits and confirms they match Config.
	Verify() error
	// Members lists the PIDs currently in the subtree.
	Members() ([]int, error)
	// Remove deletes the subtree. It fails while processes remain.
	Remove() error
	// Exists reports whether the subtree still exists.
	Exists() (bool, error)
}

// Inspection is a read-only snapshot of one exact mount-relative cgroup.
type Inspection struct {
	Path    string
	Exists  bool
	Members []int
}

// Manager creates and validates cgroup subtrees.
type Manager struct {
	// Mount is the cgroup v2 unified mount. Empty means DefaultMount.
	Mount string
	// FS is the filesystem boundary. Nil means RealFS.
	FS FS
	// Proc is the process boundary. Nil means RealProc.
	Proc Proc
	// Controllers overrides RequiredControllers.
	Controllers []string
}

func (m *Manager) mount() string {
	if m.Mount == "" {
		return DefaultMount
	}
	return filepath.Clean(m.Mount)
}

func (m *Manager) fs() FS {
	if m.FS == nil {
		return RealFS{}
	}
	return m.FS
}

// Proc boundary accessor.
func (m *Manager) proc() Proc {
	if m.Proc == nil {
		return RealProc{}
	}
	return m.Proc
}

func (m *Manager) controllers() []string {
	if len(m.Controllers) == 0 {
		return RequiredControllers
	}
	return m.Controllers
}

// RelToMount converts an absolute cgroup filesystem path to the mount-relative
// path used by /proc/<pid>/cgroup. It returns an error if path is not below
// the mount.
func (m *Manager) RelToMount(path string) (string, error) {
	mount := m.mount()
	clean := filepath.Clean(path)
	if clean == mount {
		return "/", nil
	}
	if !strings.HasPrefix(clean, mount+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %q is not below cgroup mount %q", ErrInvalid, path, mount)
	}
	rel := strings.TrimPrefix(clean, mount)
	return "/" + strings.TrimPrefix(rel, string(os.PathSeparator)), nil
}

// NormalizeMountRelative converts a cgroup path to the mount-relative form used
// by /proc/<pid>/cgroup. It accepts either a path already relative to the
// unified mount such as "/sandbox-x/box-1" or an absolute filesystem path below
// mount such as "/sys/fs/cgroup/sandbox-x/box-1". It is purely lexical and does
// not touch the filesystem. The second result is false when path is empty,
// relative, the mount root, or otherwise unusable. A path below a different
// prefix is treated as already mount-relative and left intact so callers can
// still reject it by comparison.
func NormalizeMountRelative(mount, path string) (string, bool) {
	if path == "" {
		return "", false
	}
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return "", false
	}
	m := filepath.Clean(mount)
	if clean == m {
		return "", false
	}
	if strings.HasPrefix(clean, m+string(os.PathSeparator)) {
		clean = strings.TrimPrefix(clean, m)
	}
	if clean == "/" || clean == "" {
		return "", false
	}
	return "/" + strings.TrimPrefix(clean, string(os.PathSeparator)), true
}

// WithinMountRelative reports whether reported equals root or is a descendant
// of root. Either argument may be a mount-relative cgroup path or an absolute
// path below the cgroup mount, so both reported forms are accepted. Sibling
// prefix confusion such as "/sandbox-a-evil" against "/sandbox-a" is rejected.
func WithinMountRelative(mount, root, reported string) bool {
	r, ok := NormalizeMountRelative(mount, root)
	if !ok || r == "/" {
		return false
	}
	c, ok := NormalizeMountRelative(mount, reported)
	if !ok {
		return false
	}
	return c == r || strings.HasPrefix(c, r+"/")
}

// InspectCgroup reports whether one exact mount-relative cgroup and any direct
// members remain. It never removes or otherwise mutates the inspected cgroup.
func (m *Manager) InspectCgroup(cgroupPath string) (Inspection, error) {
	clean := filepath.Clean(cgroupPath)
	if !filepath.IsAbs(cgroupPath) || clean == "/" || clean != cgroupPath {
		return Inspection{}, fmt.Errorf("%w: unsafe cgroup path %q", ErrInvalid, cgroupPath)
	}
	full := filepath.Join(m.mount(), strings.TrimPrefix(clean, "/"))
	info, err := m.fs().Stat(full)
	if errors.Is(err, fs.ErrNotExist) {
		return Inspection{Path: clean}, nil
	}
	if err != nil {
		return Inspection{}, fmt.Errorf("containment: inspect %s: %w", clean, err)
	}
	if !info.IsDir() {
		return Inspection{}, fmt.Errorf("containment: cgroup %s is not a directory", clean)
	}
	data, err := m.fs().ReadFile(filepath.Join(full, "cgroup.procs"))
	if err != nil {
		return Inspection{}, fmt.Errorf("containment: read members of %s: %w", clean, err)
	}
	members, err := parseMembers(data, clean)
	if err != nil {
		return Inspection{}, err
	}
	return Inspection{Path: clean, Exists: true, Members: members}, nil
}

// ValidateSandboxRemovalPaths is the read-only proof used before removing a
// run-scoped production-compat sandbox root and the empty "box-<safe-id>"
// child cgroups that live alpha.4 leaves under it, one per iteration.
//
// It accepts only the exact direct mount-root name "sandbox-<id>" with a single
// safe id component. A present root must be a directory whose cgroup.procs is
// empty. Every direct child directory must be named "box-<safe-id>", must have
// an empty cgroup.procs, and must have no child cgroup of its own
// ("grandchild"). Any member process, deeper child, unsafe name, or unexpected
// directory refuses the whole removal via ErrNotEmpty or ErrInvalid and leaves
// the cgroup untouched so its evidence is preserved. It never removes or
// otherwise mutates the inspected cgroups.
//
// The returned slice holds canonical full filesystem paths in removal order:
// every validated box child first (sorted by name), then the root last. A root
// that does not exist returns (nil, nil).
func (m *Manager) ValidateSandboxRemovalPaths(cgroupPath string) ([]string, error) {
	name, err := validateSandboxRootPath(cgroupPath)
	if err != nil {
		return nil, err
	}
	full := filepath.Join(m.mount(), name)

	info, err := m.fs().Stat(full)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("containment: stat sandbox root %s: %w", cgroupPath, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: sandbox root %s is not a directory", ErrInvalid, cgroupPath)
	}
	if err := m.requireEmptyProcs(cgroupPath); err != nil {
		return nil, err
	}
	entries, err := m.fs().ReadDir(full)
	if err != nil {
		return nil, fmt.Errorf("containment: list sandbox root %s: %w", cgroupPath, err)
	}
	var boxes []string
	for _, e := range entries {
		if !e.IsDir() {
			if strings.HasPrefix(e.Name(), "box-") {
				return nil, fmt.Errorf("%w: sandbox root %s has unexpected non-directory %q", ErrInvalid, cgroupPath, e.Name())
			}
			continue
		}
		if err := validateBoxName(e.Name()); err != nil {
			return nil, fmt.Errorf("%w: sandbox root %s: %v", ErrInvalid, cgroupPath, err)
		}
		boxes = append(boxes, e.Name())
	}
	sort.Strings(boxes)
	paths := make([]string, 0, len(boxes)+1)
	for _, box := range boxes {
		childRel := filepath.Join(cgroupPath, box)
		if err := m.requireEmptyProcs(childRel); err != nil {
			return nil, err
		}
		child := filepath.Join(full, box)
		childEntries, err := m.fs().ReadDir(child)
		if err != nil {
			return nil, fmt.Errorf("containment: list sandbox box %s: %w", childRel, err)
		}
		for _, ge := range childEntries {
			if ge.IsDir() {
				return nil, fmt.Errorf("%w: sandbox box %s still has grandchild cgroup %q", ErrNotEmpty, childRel, ge.Name())
			}
		}
		paths = append(paths, child)
	}
	return append(paths, full), nil
}

// requireEmptyProcs proves the mount-relative cgroup holds no member processes.
func (m *Manager) requireEmptyProcs(cgroupPath string) error {
	full := filepath.Join(m.mount(), strings.TrimPrefix(filepath.Clean(cgroupPath), "/"))
	data, err := m.fs().ReadFile(filepath.Join(full, "cgroup.procs"))
	if err != nil {
		return fmt.Errorf("containment: read members of %s: %w", cgroupPath, err)
	}
	members, err := parseMembers(data, cgroupPath)
	if err != nil {
		return err
	}
	if len(members) > 0 {
		return fmt.Errorf("%w: sandbox cgroup %s still has member pids %v", ErrNotEmpty, cgroupPath, members)
	}
	return nil
}

// RemoveEmptySandboxRoot removes a direct child of the cgroup mount named
// "sandbox-<safe-id>" plus the empty "box-<safe-id>" children live alpha.4
// leaves under it. It validates every path through ValidateSandboxRemovalPaths
// first and then removes the canonical paths in child-first, root-last order, so
// nothing is removed until every cgroup is proven to hold no member processes,
// no grandchildren, and no unexpected directory names. A residual member,
// deeper child, or unexpected child is reported via ErrNotEmpty or ErrInvalid
// and every cgroup is left untouched so its evidence is preserved. A root that
// is already absent is not an error.
func (m *Manager) RemoveEmptySandboxRoot(cgroupPath string) error {
	paths, err := m.ValidateSandboxRemovalPaths(cgroupPath)
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err := m.fs().Remove(path); err != nil {
			return fmt.Errorf("containment: remove sandbox cgroup %s: %w", path, err)
		}
	}
	return nil
}

// validateBoxName accepts only a single safe "box-<id>" directory name.
func validateBoxName(name string) error {
	const prefix = "box-"
	if !strings.HasPrefix(name, prefix) {
		return fmt.Errorf("unexpected cgroup directory %q", name)
	}
	if !safeCgroupID(strings.TrimPrefix(name, prefix)) {
		return fmt.Errorf("unsafe box id in %q", name)
	}
	return nil
}

// validateSandboxRootPath accepts only a clean, absolute, single-component path
// named "sandbox-<safe-id>" directly below the cgroup mount root and returns
// the mount-root directory name.
func validateSandboxRootPath(cgroupPath string) (string, error) {
	clean := filepath.Clean(cgroupPath)
	if cgroupPath == "" || !filepath.IsAbs(cgroupPath) || clean != cgroupPath {
		return "", fmt.Errorf("%w: unsafe sandbox root path %q", ErrInvalid, cgroupPath)
	}
	if filepath.Dir(clean) != string(os.PathSeparator) {
		return "", fmt.Errorf("%w: sandbox root %q must be a direct child of the cgroup mount", ErrInvalid, cgroupPath)
	}
	name := filepath.Base(clean)
	const prefix = "sandbox-"
	if !strings.HasPrefix(name, prefix) {
		return "", fmt.Errorf("%w: sandbox root %q must be named sandbox-<id>", ErrInvalid, cgroupPath)
	}
	if !safeCgroupID(strings.TrimPrefix(name, prefix)) {
		return "", fmt.Errorf("%w: unsafe sandbox id in %q", ErrInvalid, cgroupPath)
	}
	return name, nil
}

// safeCgroupID reports whether id is a single safe cgroup path component with
// no separators or relative-path components.
func safeCgroupID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// CheckDelegation verifies that parent is an existing, explicitly delegated
// cgroup v2 parent: the unified hierarchy is present, every required
// controller is enabled in the parent's cgroup.subtree_control, and the parent
// holds no processes (no-internal-process rule). It performs no mutation.
func (m *Manager) CheckDelegation(parent string) error {
	fsys := m.fs()
	mount := m.mount()

	if _, err := fsys.Stat(filepath.Join(mount, "cgroup.controllers")); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrUnsupported, filepath.Join(mount, "cgroup.controllers"), err)
	}
	if parent == "" || !filepath.IsAbs(parent) {
		return fmt.Errorf("%w: parent must be an absolute path", ErrInvalid)
	}
	parent = filepath.Clean(parent)
	if parent != mount && !strings.HasPrefix(parent, mount+string(os.PathSeparator)) {
		return fmt.Errorf("%w: parent %q is not below cgroup mount %q", ErrInvalid, parent, mount)
	}
	// The unified mount root is never a delegated parent. Accepting it would
	// let Judger fall back to root-level /sandbox-* cgroups, which is exactly
	// the escape this benchmark must prevent.
	if parent == mount {
		return fmt.Errorf("%w: parent %q is the cgroup mount root, not a delegated subtree", ErrNotDelegated, parent)
	}
	info, err := fsys.Stat(parent)
	if err != nil {
		return fmt.Errorf("%w: stat parent %q: %v", ErrNotDelegated, parent, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: parent %q is not a directory", ErrNotDelegated, parent)
	}
	sub, err := fsys.ReadFile(filepath.Join(parent, "cgroup.subtree_control"))
	if err != nil {
		return fmt.Errorf("%w: read %s/cgroup.subtree_control: %v", ErrNotDelegated, parent, err)
	}
	enabled := fieldSet(string(sub))
	for _, want := range m.controllers() {
		if !enabled[want] {
			return fmt.Errorf("%w: parent %q does not delegate controller %q", ErrNotDelegated, parent, want)
		}
	}
	procs, err := fsys.ReadFile(filepath.Join(parent, "cgroup.procs"))
	if err != nil {
		return fmt.Errorf("%w: read %s/cgroup.procs: %v", ErrNotDelegated, parent, err)
	}
	if len(strings.Fields(string(procs))) > 0 {
		return fmt.Errorf("%w: parent %q contains processes", ErrNotDelegated, parent)
	}
	return nil
}

// Create makes a new worker subtree below Parent and writes the requested
// cpuset.cpus, cpuset.mems, and cpu.max. It then reads them back and confirms
// the effective values match. A mismatch removes the new subtree and fails.
func (m *Manager) Create(cfg Config) (Handle, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	fsys := m.fs()
	parent := filepath.Clean(cfg.Parent)
	name := filepath.Clean(cfg.Name)
	path := filepath.Join(parent, name)

	if _, err := fsys.Stat(path); err == nil {
		return nil, fmt.Errorf("%w: subtree %q already exists", ErrInvalid, path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("containment: stat %q: %w", path, err)
	}
	if err := fsys.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("%w: create %q: %v", ErrUnsupported, path, err)
	}

	cleanup := func(err error) (Handle, error) {
		_ = fsys.Remove(path)
		return nil, err
	}
	for _, w := range []struct{ file, val string }{
		{"cpuset.cpus", cfg.CPUSet},
		{"cpuset.mems", cfg.Mems},
		{"cpu.max", cfg.CPUMax},
	} {
		if w.val == "" {
			continue
		}
		if err := writeControl(fsys, filepath.Join(path, w.file), w.val); err != nil {
			return cleanup(err)
		}
	}
	rel, err := m.RelToMount(path)
	if err != nil {
		return cleanup(err)
	}
	h := &subtree{mgr: m, name: name, path: path, rel: rel, cfg: cfg}
	if err := h.Verify(); err != nil {
		return cleanup(err)
	}
	return h, nil
}

func validateConfig(cfg Config) error {
	if cfg.Parent == "" || !filepath.IsAbs(cfg.Parent) {
		return fmt.Errorf("%w: parent must be absolute", ErrInvalid)
	}
	if cfg.Name == "" || filepath.IsAbs(cfg.Name) {
		return fmt.Errorf("%w: name %q must be relative", ErrInvalid, cfg.Name)
	}
	name := filepath.Clean(cfg.Name)
	if name == "." || name == ".." || strings.HasPrefix(name, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("%w: unsafe name %q", ErrInvalid, cfg.Name)
	}
	for _, seg := range strings.Split(name, string(os.PathSeparator)) {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%w: unsafe name segment in %q", ErrInvalid, cfg.Name)
		}
	}
	if cfg.CPUSet == "" {
		return fmt.Errorf("%w: cpuset.cpus is required", ErrInvalid)
	}
	if cfg.Mems == "" {
		return fmt.Errorf("%w: cpuset.mems is required", ErrInvalid)
	}
	return nil
}

func writeControl(fsys FS, path, val string) error {
	if err := fsys.WriteFile(path, []byte(val+"\n"), 0o644); err != nil {
		return fmt.Errorf("%w: write %s=%q: %v", ErrUnsupported, filepath.Base(path), val, err)
	}
	return nil
}

type subtree struct {
	mgr  *Manager
	name string
	path string
	rel  string
	cfg  Config
}

func (s *subtree) Name() string    { return s.name }
func (s *subtree) FSPath() string  { return s.path }
func (s *subtree) RelPath() string { return s.rel }
func (s *subtree) Config() Config  { return s.cfg }
func (s *subtree) fsys() FS        { return s.mgr.fs() }

// Attach implements Handle.
func (s *subtree) Attach(pid int) error {
	if err := ValidatePID(pid); err != nil {
		return err
	}
	return writeControl(s.fsys(), filepath.Join(s.path, "cgroup.procs"), strconv.Itoa(pid))
}

// Verify implements Handle.
func (s *subtree) Verify() error {
	fsys := s.fsys()
	checks := []struct {
		file, want, kind string
	}{
		{"cpuset.cpus.effective", s.cfg.CPUSet, "cpu"},
		{"cpuset.mems.effective", s.cfg.Mems, "mems"},
		{"cpu.max", s.cfg.CPUMax, "cpumax"},
	}
	for _, c := range checks {
		if c.want == "" {
			continue
		}
		data, err := fsys.ReadFile(filepath.Join(s.path, c.file))
		if err != nil {
			return fmt.Errorf("%w: read %s: %v", ErrUnsupported, filepath.Join(s.path, c.file), err)
		}
		got := strings.TrimSpace(string(data))
		var ok bool
		switch c.kind {
		case "cpu":
			ok = EqualCPUSets(got, c.want)
		case "mems":
			ok = EqualCPUSets(got, c.want)
		default:
			ok = normalizeFields(got) == normalizeFields(c.want)
		}
		if !ok {
			return fmt.Errorf("containment: %s effective %q does not match requested %q", c.file, got, c.want)
		}
	}
	return nil
}

// Members implements Handle.
func (s *subtree) Members() ([]int, error) {
	data, err := s.fsys().ReadFile(filepath.Join(s.path, "cgroup.procs"))
	if err != nil {
		return nil, fmt.Errorf("containment: read members of %s: %w", s.path, err)
	}
	return parseMembers(data, s.path)
}

func parseMembers(data []byte, path string) ([]int, error) {
	var pids []int
	for _, f := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("containment: bad pid %q in %s", f, path)
		}
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids, nil
}

// Remove implements Handle.
func (s *subtree) Remove() error {
	if err := s.fsys().Remove(s.path); err != nil {
		return fmt.Errorf("containment: remove %s: %w", s.path, err)
	}
	return nil
}

// Exists implements Handle.
func (s *subtree) Exists() (bool, error) {
	_, err := s.fsys().Stat(s.path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// CgroupOf returns the mount-relative cgroup path for pid.
func (m *Manager) CgroupOf(pid int) (string, error) {
	if err := ValidatePID(pid); err != nil {
		return "", err
	}
	return m.proc().Cgroup(pid)
}

// Descendants returns pid and all of its descendants.
func (m *Manager) Descendants(pid int) ([]int, error) {
	if err := ValidatePID(pid); err != nil {
		return nil, err
	}
	return m.proc().Descendants(pid)
}

// ValidatePID rejects non-positive process identifiers.
func ValidatePID(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("%w: invalid pid %d", ErrInvalid, pid)
	}
	return nil
}

// AttachPID writes pid into the cgroup.procs of the absolute cgroup path.
func AttachPID(fsys FS, cgroupFSPath string, pid int) error {
	if fsys == nil {
		fsys = RealFS{}
	}
	if cgroupFSPath == "" || !filepath.IsAbs(cgroupFSPath) {
		return fmt.Errorf("%w: cgroup path must be absolute", ErrInvalid)
	}
	if err := ValidatePID(pid); err != nil {
		return err
	}
	return writeControl(fsys, filepath.Join(cgroupFSPath, "cgroup.procs"), strconv.Itoa(pid))
}

// IsWithin reports whether cgroupPath is root or a descendant of root. Both are
// mount-relative cgroup paths.
func IsWithin(root, cgroupPath string) bool {
	r := normalizeRel(root)
	c := normalizeRel(cgroupPath)
	if r == "" || c == "" {
		return false
	}
	return c == r || strings.HasPrefix(c, r+"/")
}

func normalizeRel(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = "/" + strings.Trim(strings.TrimPrefix(p, "/"), "/")
	if p == "/" {
		return "/"
	}
	return filepath.ToSlash(filepath.Clean(p))
}

func fieldSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, f := range strings.Fields(s) {
		m[f] = true
	}
	return m
}

func normalizeFields(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
