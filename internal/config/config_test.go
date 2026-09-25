package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const minimal = `{
  "schemaVersion": 1,
  "iris": {"image": "ghcr.io/skkuding/codedang-iris:stage"},
  "profiles": {"isolated-1s": {"suite": "judger", "workers": 1, "repetitions": 5, "cpuList": "2-3", "numaPolicy": "bind", "turbo": "off"}},
  "hosts": [{"alias": "codedang8", "hostIdentity": "server8", "allow": true}]
}`

func TestDecodeAppliesDefaults(t *testing.T) {
	cfg, err := Decode([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ToolVersion != "0.1.0" {
		t.Errorf("toolVersion = %q", cfg.ToolVersion)
	}
	if cfg.Paths.VarRoot != DefaultVarRoot || cfg.Paths.RunRoot != DefaultRunRoot {
		t.Errorf("paths defaults not applied: %+v", cfg.Paths)
	}
	if cfg.Paths.SocketDir != DefaultSockets {
		t.Errorf("socketDir = %q", cfg.Paths.SocketDir)
	}
	if cfg.Limits.MaxWorkers != 32 {
		t.Errorf("maxWorkers = %d", cfg.Limits.MaxWorkers)
	}
	if h, ok := cfg.AllowedHost("codedang8"); !ok || h.HostIdentity != "server8" {
		t.Errorf("AllowedHost = %+v, %v", h, ok)
	}
}

func TestDecodeRejectsUnknownField(t *testing.T) {
	bad := strings.Replace(minimal, `"schemaVersion": 1,`, `"schemaVersion": 1, "mystery": true,`, 1)
	if _, err := Decode([]byte(bad)); err == nil {
		t.Fatal("Decode accepted an unknown field")
	}
}

func TestDecodeRejectsTrailingData(t *testing.T) {
	if _, err := Decode([]byte(minimal + minimal)); err == nil {
		t.Fatal("Decode accepted trailing data")
	}
}

func TestValidateFailures(t *testing.T) {
	cases := map[string]string{
		"bad schema":      `{"schemaVersion":2,"iris":{"image":"x"},"profiles":{},"hosts":[{"alias":"h","allow":true}]}`,
		"no hosts":        `{"schemaVersion":1,"iris":{"image":"x"},"profiles":{}}`,
		"no allowed host": `{"schemaVersion":1,"iris":{"image":"x"},"profiles":{},"hosts":[{"alias":"h","allow":false}]}`,
		"bad suite":       `{"schemaVersion":1,"iris":{"image":"x"},"profiles":{"p":{"suite":"other","workers":1,"repetitions":1}},"hosts":[{"alias":"h","allow":true}]}`,
		"bad workers":     `{"schemaVersion":1,"iris":{"image":"x"},"profiles":{"p":{"suite":"judger","workers":0,"repetitions":1}},"hosts":[{"alias":"h","allow":true}]}`,
		"bad cpuList":     `{"schemaVersion":1,"iris":{"image":"x"},"profiles":{"p":{"suite":"judger","workers":1,"repetitions":1,"cpuList":"a-b"}},"hosts":[{"alias":"h","allow":true}]}`,
		"bad turbo":       `{"schemaVersion":1,"iris":{"image":"x"},"profiles":{"p":{"suite":"judger","workers":1,"repetitions":1,"turbo":"maybe"}},"hosts":[{"alias":"h","allow":true}]}`,
	}
	for name, doc := range cases {
		if _, err := Decode([]byte(doc)); err == nil {
			t.Errorf("%s: Decode = nil, want error", name)
		}
	}
}

func TestProfileExceedsWorkerLimit(t *testing.T) {
	doc := `{"schemaVersion":1,"iris":{"image":"x"},"limits":{"maxWorkers":2},"profiles":{"p":{"suite":"judger","workers":8,"repetitions":1}},"hosts":[{"alias":"h","allow":true}]}`
	if _, err := Decode([]byte(doc)); err == nil {
		t.Fatal("Decode accepted workers above the limit")
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(minimal), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("Load accepted a missing file")
	}
}

func TestValidateFixtureSet(t *testing.T) {
	good := FixtureSet{Name: "cpp-runtime-v1"}
	good.Digests = append(good.Digests, struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	}{Path: "source.cpp", SHA256: strings.Repeat("a", 64)})
	if err := ValidateFixtureSet(good); err != nil {
		t.Fatal(err)
	}
	bad := good
	bad.Digests[0].Path = "../escape"
	if err := ValidateFixtureSet(bad); err == nil {
		t.Fatal("ValidateFixtureSet accepted a traversal path")
	}
}
