package containment

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"testing"
	"time"
)

// fakeFS is an in-memory cgroup tree used to exercise containment without
// cgroup delegation. Writing a control file also seeds its ".effective" file
// unless an override is configured or a test placed one, mimicking the kernel.
type fakeFS struct {
	files       map[string]string
	dirs        map[string]bool
	effective   map[string]string
	denyWrite   map[string]bool
	removeOrder []string
}

func newFakeFS() *fakeFS {
	return &fakeFS{
		files:     map[string]string{},
		dirs:      map[string]bool{"/": true},
		effective: map[string]string{},
		denyWrite: map[string]bool{},
	}
}

func TestInspectCgroupIsReadOnlyAndExact(t *testing.T) {
	f := newFakeFS()
	f.mkdir("/sys/fs/cgroup/sandbox-run-worker")
	f.put("/sys/fs/cgroup/sandbox-run-worker/cgroup.procs", "42\n7\n")
	m := &Manager{Mount: "/sys/fs/cgroup", FS: f}

	got, err := m.InspectCgroup("/sandbox-run-worker")
	if err != nil || !got.Exists || fmt.Sprint(got.Members) != "[7 42]" {
		t.Fatalf("inspection = %+v, %v", got, err)
	}
	if !f.dirs["/sys/fs/cgroup/sandbox-run-worker"] {
		t.Fatal("inspection removed the cgroup")
	}
	if _, err := m.InspectCgroup("/sandbox-run-worker/../other"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unsafe path err = %v, want ErrInvalid", err)
	}
	absent, err := m.InspectCgroup("/sandbox-absent")
	if err != nil || absent.Exists {
		t.Fatalf("absent inspection = %+v, %v", absent, err)
	}
}

func TestNormalizeMountRelative(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
		ok   bool
	}{
		{"mount-relative", "/sandbox-x/box-1", "/sandbox-x/box-1", true},
		{"filesystem", "/sys/fs/cgroup/sandbox-x/box-1", "/sandbox-x/box-1", true},
		{"filesystem-root-child", "/sys/fs/cgroup/sandbox-x", "/sandbox-x", true},
		{"mount-root", "/sys/fs/cgroup", "", false},
		{"slash", "/", "", false},
		{"empty", "", "", false},
		{"relative", "sandbox-x", "", false},
		{"prefix-sibling", "/sys/fs/cgroupfoo/sandbox-x", "/sys/fs/cgroupfoo/sandbox-x", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := NormalizeMountRelative(DefaultMount, tc.path)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("NormalizeMountRelative(%q) = %q, %v; want %q, %v", tc.path, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestWithinMountRelative(t *testing.T) {
	cases := []struct {
		name     string
		root     string
		reported string
		ok       bool
	}{
		{"mount-relative-root", "/sandbox-x", "/sandbox-x", true},
		{"mount-relative-descendant", "/sandbox-x", "/sandbox-x/box-1", true},
		{"filesystem-root", "/sandbox-x", "/sys/fs/cgroup/sandbox-x", true},
		{"filesystem-descendant", "/sandbox-x", "/sys/fs/cgroup/sandbox-x/box-1", true},
		{"filesystem-root-input", "/sys/fs/cgroup/sandbox-x", "/sandbox-x/box-1", true},
		{"prefix-confusion", "/sandbox-x", "/sandbox-x-evil/box-1", false},
		{"prefix-confusion-filesystem", "/sandbox-x", "/sys/fs/cgroup/sandbox-x-evil/box-1", false},
		{"other-root", "/sandbox-x", "/sandbox-y", false},
		{"relative", "/sandbox-x", "sandbox-x/box-1", false},
		{"empty", "/sandbox-x", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WithinMountRelative(DefaultMount, tc.root, tc.reported); got != tc.ok {
				t.Fatalf("WithinMountRelative(%q, %q) = %v, want %v", tc.root, tc.reported, got, tc.ok)
			}
		})
	}
}

func TestValidateSandboxRemovalPathsReturnsChildrenThenRoot(t *testing.T) {
	f := newFakeFS()
	f.mkdir("/sys/fs/cgroup/sandbox-run-worker")
	f.put("/sys/fs/cgroup/sandbox-run-worker/cgroup.procs", "")
	for _, b := range []string{"box-5", "box-1", "box-3", "box-2", "box-4"} {
		f.mkdir("/sys/fs/cgroup/sandbox-run-worker/" + b)
		f.put("/sys/fs/cgroup/sandbox-run-worker/"+b+"/cgroup.procs", "")
	}
	paths, err := manager(f).ValidateSandboxRemovalPaths("/sandbox-run-worker")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/sys/fs/cgroup/sandbox-run-worker/box-1",
		"/sys/fs/cgroup/sandbox-run-worker/box-2",
		"/sys/fs/cgroup/sandbox-run-worker/box-3",
		"/sys/fs/cgroup/sandbox-run-worker/box-4",
		"/sys/fs/cgroup/sandbox-run-worker/box-5",
		"/sys/fs/cgroup/sandbox-run-worker",
	}
	if fmt.Sprint(paths) != fmt.Sprint(want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	if !f.dirs["/sys/fs/cgroup/sandbox-run-worker"] {
		t.Fatal("validator mutated the root")
	}
	absent, err := manager(f).ValidateSandboxRemovalPaths("/sandbox-absent")
	if err != nil || absent != nil {
		t.Fatalf("absent validate = %v, %v; want nil, nil", absent, err)
	}
}

func TestValidateSandboxRemovalPathsRefusesEvidence(t *testing.T) {
	cases := []struct {
		name    string
		procs   string
		box     string
		boxProc string
		extra   string
		wantErr error
		want    string
	}{
		{"root-member", "42\n", "", "", "", ErrNotEmpty, "42"},
		{"box-member", "", "box-1", "7\n", "", ErrNotEmpty, "7"},
		{"grandchild", "", "box-1", "", "box-1/grand-1", ErrNotEmpty, "grand-1"},
		{"unexpected-child", "", "other", "", "", ErrInvalid, "other"},
		{"unsafe-box", "", "box-..", "", "", ErrInvalid, "box-.."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeFS()
			f.mkdir("/sys/fs/cgroup/sandbox-run-worker")
			f.put("/sys/fs/cgroup/sandbox-run-worker/cgroup.procs", tc.procs)
			if tc.box != "" {
				f.mkdir("/sys/fs/cgroup/sandbox-run-worker/" + tc.box)
				f.put("/sys/fs/cgroup/sandbox-run-worker/"+tc.box+"/cgroup.procs", tc.boxProc)
			}
			if tc.extra != "" {
				f.mkdir("/sys/fs/cgroup/sandbox-run-worker/" + tc.extra)
			}
			paths, err := manager(f).ValidateSandboxRemovalPaths("/sandbox-run-worker")
			if !errors.Is(err, tc.wantErr) || paths != nil {
				t.Fatalf("validate = %v, %v; want %v", paths, err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %q does not preserve %q evidence", err, tc.want)
			}
			if !f.dirs["/sys/fs/cgroup/sandbox-run-worker"] {
				t.Fatal("validator mutated the root")
			}
		})
	}
}

func TestValidateSandboxRemovalPathsRejectsUnsafePaths(t *testing.T) {
	f := newFakeFS()
	f.mkdir("/sys/fs/cgroup/sandbox-run-worker/box-123")
	m := manager(f)
	for _, p := range []string{
		"/sandbox-run-worker/box-123",
		"/sys/fs/cgroup/sandbox-run-worker",
		"/sandbox-",
		"/sandbox-..",
		"/sandbox-a/b",
		"/other-x",
		"sandbox-run-worker",
		"",
		"/",
	} {
		if _, err := m.ValidateSandboxRemovalPaths(p); !errors.Is(err, ErrInvalid) {
			t.Fatalf("ValidateSandboxRemovalPaths(%q) = %v, want ErrInvalid", p, err)
		}
	}
}

func TestRemoveEmptySandboxRootRemovesBoxesInOrder(t *testing.T) {
	f := newFakeFS()
	f.mkdir("/sys/fs/cgroup/sandbox-run-worker")
	f.put("/sys/fs/cgroup/sandbox-run-worker/cgroup.procs", "")
	for _, b := range []string{"box-2", "box-1", "box-3"} {
		f.mkdir("/sys/fs/cgroup/sandbox-run-worker/" + b)
		f.put("/sys/fs/cgroup/sandbox-run-worker/"+b+"/cgroup.procs", "")
	}
	f.removeOrder = nil
	m := manager(f)
	if err := m.RemoveEmptySandboxRoot("/sandbox-run-worker"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/sys/fs/cgroup/sandbox-run-worker/box-1",
		"/sys/fs/cgroup/sandbox-run-worker/box-2",
		"/sys/fs/cgroup/sandbox-run-worker/box-3",
		"/sys/fs/cgroup/sandbox-run-worker",
	}
	if fmt.Sprint(f.removeOrder) != fmt.Sprint(want) {
		t.Fatalf("remove order = %v, want %v", f.removeOrder, want)
	}
	for _, p := range want {
		if f.dirs[p] {
			t.Fatalf("%s still exists after removal", p)
		}
	}
	if err := m.RemoveEmptySandboxRoot("/sandbox-absent"); err != nil {
		t.Fatalf("absent root should be a no-op: %v", err)
	}
}

func TestRemoveEmptySandboxRootPreservesOnUnexpectedChild(t *testing.T) {
	f := newFakeFS()
	f.mkdir("/sys/fs/cgroup/sandbox-run-worker")
	f.put("/sys/fs/cgroup/sandbox-run-worker/cgroup.procs", "")
	f.mkdir("/sys/fs/cgroup/sandbox-run-worker/other")
	err := manager(f).RemoveEmptySandboxRoot("/sandbox-run-worker")
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if !f.dirs["/sys/fs/cgroup/sandbox-run-worker"] || !f.dirs["/sys/fs/cgroup/sandbox-run-worker/other"] {
		t.Fatal("unexpected child caused a partial removal")
	}
}

func TestRemoveEmptySandboxRootRejectsUnsafePaths(t *testing.T) {
	f := newFakeFS()
	f.mkdir("/sys/fs/cgroup/sandbox-run-worker/box-123")
	m := manager(f)
	for _, p := range []string{
		"/sandbox-run-worker/box-123",
		"/sys/fs/cgroup/sandbox-run-worker",
		"/sandbox-",
		"/sandbox-..",
		"/sandbox-a/b",
		"/other-x",
		"sandbox-run-worker",
		"",
		"/",
	} {
		if err := m.RemoveEmptySandboxRoot(p); !errors.Is(err, ErrInvalid) {
			t.Fatalf("RemoveEmptySandboxRoot(%q) = %v, want ErrInvalid", p, err)
		}
	}
}

func (f *fakeFS) put(p, v string) { f.files[path.Clean(p)] = v }

func (f *fakeFS) get(p string) (string, bool) { v, ok := f.files[path.Clean(p)]; return v, ok }

func (f *fakeFS) mkdir(dir string) {
	dir = path.Clean(dir)
	f.dirs[dir] = true
	for {
		parent := path.Dir(dir)
		if parent == dir {
			return
		}
		f.dirs[parent] = true
		dir = parent
	}
}

func (f *fakeFS) ReadFile(name string) ([]byte, error) {
	if v, ok := f.get(name); ok {
		return []byte(v), nil
	}
	if f.dirs[path.Clean(name)] {
		return nil, fmt.Errorf("read %s: is a directory", name)
	}
	return nil, fmt.Errorf("open %s: %w", name, fs.ErrNotExist)
}

func (f *fakeFS) WriteFile(name string, data []byte, _ fs.FileMode) error {
	clean := path.Clean(name)
	if f.denyWrite[clean] {
		return fmt.Errorf("write %s: %w", name, fs.ErrPermission)
	}
	if !f.dirs[path.Dir(clean)] {
		return fmt.Errorf("write %s: %w", name, fs.ErrNotExist)
	}
	f.files[clean] = string(data)
	eff := clean + ".effective"
	if _, ok := f.files[eff]; !ok {
		if o, ok := f.effective[clean]; ok {
			f.files[eff] = o
		} else {
			f.files[eff] = string(data)
		}
	}
	return nil
}

func (f *fakeFS) MkdirAll(p string, _ fs.FileMode) error {
	f.mkdir(p)
	return nil
}

func (f *fakeFS) Remove(p string) error {
	clean := path.Clean(p)
	if _, ok := f.files[clean]; ok {
		delete(f.files, clean)
		f.removeOrder = append(f.removeOrder, clean)
		return nil
	}
	if f.dirs[clean] {
		// The kernel refuses rmdir while processes remain, but control files
		// disappear with the directory. Model both.
		if procs, ok := f.files[clean+"/cgroup.procs"]; ok && len(strings.Fields(procs)) > 0 {
			return fmt.Errorf("remove %s: directory not empty", p)
		}
		prefix := clean + "/"
		for k := range f.files {
			if k == clean || strings.HasPrefix(k, prefix) {
				delete(f.files, k)
			}
		}
		for k := range f.dirs {
			if k == clean || strings.HasPrefix(k, prefix) {
				delete(f.dirs, k)
			}
		}
		f.removeOrder = append(f.removeOrder, clean)
		return nil
	}
	return fmt.Errorf("remove %s: %w", p, fs.ErrNotExist)
}

func (f *fakeFS) Stat(name string) (fs.FileInfo, error) {
	clean := path.Clean(name)
	if f.dirs[clean] {
		return fakeInfo{name: path.Base(clean), dir: true}, nil
	}
	if v, ok := f.files[clean]; ok {
		return fakeInfo{name: path.Base(clean), size: int64(len(v))}, nil
	}
	return nil, fmt.Errorf("stat %s: %w", name, fs.ErrNotExist)
}

func (f *fakeFS) ReadDir(name string) ([]fs.DirEntry, error) {
	clean := path.Clean(name)
	if !f.dirs[clean] {
		return nil, fmt.Errorf("readdir %s: %w", name, fs.ErrNotExist)
	}
	seen := map[string]bool{}
	prefix := clean + "/"
	for k := range f.files {
		if strings.HasPrefix(k, prefix) {
			seen[strings.SplitN(strings.TrimPrefix(k, prefix), "/", 2)[0]] = false
		}
	}
	for k := range f.dirs {
		if k != clean && strings.HasPrefix(k, prefix) {
			seen[strings.SplitN(strings.TrimPrefix(k, prefix), "/", 2)[0]] = true
		}
	}
	var out []fs.DirEntry
	for n, isDir := range seen {
		out = append(out, fakeInfo{name: n, dir: isDir})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

type fakeInfo struct {
	name string
	size int64
	dir  bool
}

func (f fakeInfo) Name() string               { return f.name }
func (f fakeInfo) Size() int64                { return f.size }
func (f fakeInfo) Mode() fs.FileMode          { return 0o644 }
func (f fakeInfo) ModTime() time.Time         { return time.Time{} }
func (f fakeInfo) IsDir() bool                { return f.dir }
func (f fakeInfo) Sys() any                   { return nil }
func (f fakeInfo) Info() (fs.FileInfo, error) { return f, nil }
func (f fakeInfo) Type() fs.FileMode          { return f.Mode() }

// fakeProc models /proc for tests.
type fakeProc struct {
	cgroups  map[int]string
	children map[int][]int
	missing  map[int]bool
}

func newFakeProc() *fakeProc {
	return &fakeProc{cgroups: map[int]string{}, children: map[int][]int{}, missing: map[int]bool{}}
}

func (p *fakeProc) Cgroup(pid int) (string, error) {
	if p.missing[pid] {
		return "", fmt.Errorf("proc %d gone", pid)
	}
	cg, ok := p.cgroups[pid]
	if !ok {
		return "", fmt.Errorf("proc %d gone", pid)
	}
	return cg, nil
}

func (p *fakeProc) Exists(pid int) (bool, error) {
	if p.missing[pid] {
		return false, nil
	}
	_, ok := p.cgroups[pid]
	return ok, nil
}

func (p *fakeProc) Descendants(pid int) ([]int, error) {
	if p.missing[pid] {
		return nil, fmt.Errorf("proc %d gone", pid)
	}
	if _, ok := p.cgroups[pid]; !ok {
		return nil, fmt.Errorf("proc %d gone", pid)
	}
	seen := map[int]bool{}
	var walk func(int)
	walk = func(cur int) {
		if seen[cur] {
			return
		}
		seen[cur] = true
		for _, c := range p.children[cur] {
			walk(c)
		}
	}
	walk(pid)
	out := make([]int, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Ints(out)
	return out, nil
}

var testMount = "/sys/fs/cgroup"

func delegatedFS() *fakeFS {
	f := newFakeFS()
	f.put("/sys/fs/cgroup/cgroup.controllers", "cpu cpuset memory io\n")
	f.mkdir("/sys/fs/cgroup/iris-bench")
	f.put("/sys/fs/cgroup/iris-bench/cgroup.subtree_control", "cpu cpuset\n")
	f.put("/sys/fs/cgroup/iris-bench/cgroup.procs", "")
	return f
}

func manager(f *fakeFS) *Manager {
	return &Manager{Mount: testMount, FS: f, Proc: newFakeProc()}
}

func TestParseCPUListAndFormat(t *testing.T) {
	got, err := ParseCPUList("0-3,7,9-10")
	if err != nil {
		t.Fatal(err)
	}
	want := []int{0, 1, 2, 3, 7, 9, 10}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("parse = %v, want %v", got, want)
	}
	if out := FormatCPUList(got); out != "0-3,7,9-10" {
		t.Fatalf("format = %q", out)
	}
	for _, bad := range []string{"", "3-1", "a", "1,,2", "-1"} {
		if _, err := ParseCPUList(bad); err == nil {
			t.Fatalf("ParseCPUList(%q) succeeded, want error", bad)
		}
	}
}

func TestSplitCPUList(t *testing.T) {
	got, err := SplitCPUList("0-3", 2)
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != "0,2" || got[1] != "1,3" {
		t.Fatalf("split = %v", got)
	}
	if _, err := SplitCPUList("0", 2); err == nil {
		t.Fatal("expected oversubscription to fail")
	}
}

func TestEqualCPUSetsAndIsWithin(t *testing.T) {
	if !EqualCPUSets("0-2", "0,1,2") {
		t.Fatal("expected equal sets")
	}
	if EqualCPUSets("0-2", "0,1") {
		t.Fatal("expected unequal sets")
	}
	if !IsWithin("/a/b", "/a/b/c") || !IsWithin("/a/b", "/a/b") {
		t.Fatal("expected within")
	}
	if IsWithin("/a/b", "/a/bc") || IsWithin("/a/b", "/a") {
		t.Fatal("expected not within")
	}
}

func TestCheckDelegationUnsupportedWithoutControllers(t *testing.T) {
	f := newFakeFS() // no cgroup.controllers
	if err := manager(f).CheckDelegation("/sys/fs/cgroup/iris-bench"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestCheckDelegationMissingController(t *testing.T) {
	f := delegatedFS()
	f.put("/sys/fs/cgroup/iris-bench/cgroup.subtree_control", "cpu\n")
	if err := manager(f).CheckDelegation("/sys/fs/cgroup/iris-bench"); !errors.Is(err, ErrNotDelegated) {
		t.Fatalf("err = %v, want ErrNotDelegated", err)
	}
}

func TestCheckDelegationParentHasProcesses(t *testing.T) {
	f := delegatedFS()
	f.put("/sys/fs/cgroup/iris-bench/cgroup.procs", "42\n")
	if err := manager(f).CheckDelegation("/sys/fs/cgroup/iris-bench"); !errors.Is(err, ErrNotDelegated) {
		t.Fatalf("err = %v, want ErrNotDelegated", err)
	}
}

func TestCheckDelegationRejectsOutsideMount(t *testing.T) {
	f := delegatedFS()
	if err := manager(f).CheckDelegation("/tmp/not-cgroup"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestCheckDelegationRejectsMountRoot(t *testing.T) {
	// The unified mount root is not a delegated subtree: accepting it would
	// allow root-level /sandbox-* cgroups.
	if err := manager(delegatedFS()).CheckDelegation(testMount); !errors.Is(err, ErrNotDelegated) {
		t.Fatalf("err = %v, want ErrNotDelegated", err)
	}
}

func TestCheckDelegationPasses(t *testing.T) {
	if err := manager(delegatedFS()).CheckDelegation("/sys/fs/cgroup/iris-bench"); err != nil {
		t.Fatal(err)
	}
}

func TestCreateWritesAndVerifies(t *testing.T) {
	f := delegatedFS()
	h, err := manager(f).Create(Config{Parent: "/sys/fs/cgroup/iris-bench", Name: "run-1/block-1/worker-01", CPUSet: "0,1", Mems: "0", CPUMax: "200000 100000"})
	if err != nil {
		t.Fatal(err)
	}
	if h.RelPath() != "/iris-bench/run-1/block-1/worker-01" {
		t.Fatalf("rel = %q", h.RelPath())
	}
	if err := h.Verify(); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if v, _ := f.get("/sys/fs/cgroup/iris-bench/run-1/block-1/worker-01/cpuset.cpus"); strings.TrimSpace(v) != "0,1" {
		t.Fatalf("cpuset.cpus = %q", v)
	}
}

func TestCreateDetectsEffectiveMismatch(t *testing.T) {
	f := delegatedFS()
	target := "/sys/fs/cgroup/iris-bench/run-1/cpuset.cpus"
	f.effective[target] = "0" // kernel says the parent restricted us
	_, err := manager(f).Create(Config{Parent: "/sys/fs/cgroup/iris-bench", Name: "run-1", CPUSet: "0,1", Mems: "0", CPUMax: "100000 100000"})
	if err == nil {
		t.Fatal("expected effective mismatch error")
	}
	if errors.Is(err, ErrUnsupported) {
		t.Fatalf("mismatch should be a plain failure, got %v", err)
	}
}

func TestCreateControlWriteUnsupported(t *testing.T) {
	f := delegatedFS()
	f.denyWrite["/sys/fs/cgroup/iris-bench/w/cpu.max"] = true
	_, err := manager(f).Create(Config{Parent: "/sys/fs/cgroup/iris-bench", Name: "w", CPUSet: "0", Mems: "0", CPUMax: "100000 100000"})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestCreateRejectsDuplicate(t *testing.T) {
	f := delegatedFS()
	m := manager(f)
	cfg := Config{Parent: "/sys/fs/cgroup/iris-bench", Name: "dup", CPUSet: "0", Mems: "0", CPUMax: "100000 100000"}
	if _, err := m.Create(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(cfg); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestCreateRejectsUnsafeName(t *testing.T) {
	f := delegatedFS()
	for _, name := range []string{"../escape", "/abs", "a/../../b"} {
		_, err := manager(f).Create(Config{Parent: "/sys/fs/cgroup/iris-bench", Name: name, CPUSet: "0", Mems: "0", CPUMax: "100000 100000"})
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("name %q: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestAttachMembersRemoveExists(t *testing.T) {
	f := delegatedFS()
	h, err := manager(f).Create(Config{Parent: "/sys/fs/cgroup/iris-bench", Name: "w", CPUSet: "0", Mems: "0", CPUMax: "100000 100000"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Attach(1234); err != nil {
		t.Fatal(err)
	}
	members, err := h.Members()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(members) != "[1234]" {
		t.Fatalf("members = %v", members)
	}
	if exists, _ := h.Exists(); !exists {
		t.Fatal("expected subtree to exist")
	}
	if err := h.Remove(); err == nil {
		t.Fatal("expected remove to fail while members remain")
	}
	f.put("/sys/fs/cgroup/iris-bench/w/cgroup.procs", "")
	if err := h.Remove(); err != nil {
		t.Fatal(err)
	}
	if exists, _ := h.Exists(); exists {
		t.Fatal("expected subtree removed")
	}
}

func TestAttachPIDRejectsBadInput(t *testing.T) {
	f := newFakeFS()
	f.mkdir("/sys/fs/cgroup/x")
	if err := AttachPID(f, "", 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if err := AttachPID(f, "/sys/fs/cgroup/x", 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestDescendantsAndEscape(t *testing.T) {
	p := newFakeProc()
	p.cgroups[100] = "/iris-bench/run/w1"
	p.cgroups[101] = "/iris-bench/run/w1"
	p.cgroups[102] = "/sandbox-abc" // escaped root-level cgroup
	p.children[100] = []int{101, 102}
	m := &Manager{Mount: testMount, FS: newFakeFS(), Proc: p}
	desc, err := m.Descendants(100)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(desc) != "[100 101 102]" {
		t.Fatalf("descendants = %v", desc)
	}
	var escaped []int
	for _, pid := range desc {
		cg, err := m.CgroupOf(pid)
		if err != nil {
			continue
		}
		if !IsWithin("/iris-bench/run/w1", cg) {
			escaped = append(escaped, pid)
		}
	}
	if fmt.Sprint(escaped) != "[102]" {
		t.Fatalf("escaped = %v", escaped)
	}
}
