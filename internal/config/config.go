// Package config loads and strictly validates controller configuration.
//
// The on-disk format is JSON, not YAML, so the toolchain stays on the Go
// standard library. Unknown fields are rejected to catch typos and stale keys.
// Defaults are applied before validation and never override an explicit value.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/skkuding/iris-load-test-toolset/internal/artifact"
	"github.com/skkuding/iris-load-test-toolset/internal/runplan"
)

// SchemaVersion is the configuration schema this build accepts.
const SchemaVersion = 1

// Default state and runtime roots from the proposal.
const (
	DefaultVarRoot = "/var/lib/iris-bench"
	DefaultRunRoot = "/run/iris-bench"
	DefaultBinRoot = "/opt/iris-bench/bin"
	DefaultSockets = "~/.ssh/sockets"
)

// Config is the top-level controller configuration.
type Config struct {
	SchemaVersion int                `json:"schemaVersion"`
	ToolVersion   string             `json:"toolVersion,omitempty"`
	Iris          Iris               `json:"iris"`
	Profiles      map[string]Profile `json:"profiles"`
	Hosts         []Host             `json:"hosts"`
	Paths         Paths              `json:"paths,omitempty"`
	Limits        Limits             `json:"limits,omitempty"`
}

// Iris names the default images.
type Iris struct {
	Image         string `json:"image,omitempty"`
	RabbitMQImage string `json:"rabbitmqImage,omitempty"`
}

// Profile is a named measurement configuration.
type Profile struct {
	Suite       string `json:"suite"`
	Workers     int    `json:"workers"`
	Repetitions int    `json:"repetitions"`
	CPUList     string `json:"cpuList,omitempty"`
	NUMAPolicy  string `json:"numaPolicy,omitempty"`
	Turbo       string `json:"turbo,omitempty"`
}

// Host is an allowlisted benchmark host identity.
type Host struct {
	Alias        string `json:"alias"`
	HostIdentity string `json:"hostIdentity,omitempty"`
	Allow        bool   `json:"allow"`
}

// Paths overrides default filesystem roots.
type Paths struct {
	VarRoot    string `json:"varRoot,omitempty"`
	RunRoot    string `json:"runRoot,omitempty"`
	BinRoot    string `json:"binRoot,omitempty"`
	SocketDir  string `json:"socketDir,omitempty"`
	ResultRoot string `json:"resultRoot,omitempty"`
}

// Limits bounds run scope.
type Limits struct {
	MaxWorkers         int `json:"maxWorkers,omitempty"`
	MaxRunSeconds      int `json:"maxRunSeconds,omitempty"`
	MaxQueueDepth      int `json:"maxQueueDepth,omitempty"`
	MaxDiskMiB         int `json:"maxDiskMiB,omitempty"`
	StopOnErrorRatePct int `json:"stopOnErrorRatePct,omitempty"`
}

var cpuListPattern = regexp.MustCompile(`^[0-9]+(-[0-9]+)?(,[0-9]+(-[0-9]+)?)*$`)

// Default returns a configuration with defaults applied.
func Default() Config {
	return Config{
		SchemaVersion: SchemaVersion,
		ToolVersion:   "0.1.0",
		Iris:          Iris{Image: runplan.DefaultIrisImage},
		Profiles:      map[string]Profile{},
		Paths: Paths{
			VarRoot:    DefaultVarRoot,
			RunRoot:    DefaultRunRoot,
			BinRoot:    DefaultBinRoot,
			SocketDir:  DefaultSockets,
			ResultRoot: "runs",
		},
		Limits: Limits{MaxWorkers: 32, MaxRunSeconds: 3600, MaxQueueDepth: 100000, MaxDiskMiB: 40960},
	}
}

// Load reads path, strictly decodes it, applies defaults, and validates.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	cfg, err := Decode(data)
	if err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// Decode strictly decodes configuration bytes, applies defaults, and validates.
func Decode(data []byte) (Config, error) {
	cfg := Default()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode: %w", err)
	}
	if dec.More() {
		return Config{}, errors.New("decode: trailing data")
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// ApplyDefaults fills unspecified values without overriding explicit ones.
func (c *Config) ApplyDefaults() {
	d := Default()
	if c.SchemaVersion == 0 {
		c.SchemaVersion = d.SchemaVersion
	}
	if c.ToolVersion == "" {
		c.ToolVersion = d.ToolVersion
	}
	if c.Iris.Image == "" {
		c.Iris.Image = d.Iris.Image
	}
	if c.Profiles == nil {
		c.Profiles = map[string]Profile{}
	}
	if c.Paths.VarRoot == "" {
		c.Paths.VarRoot = d.Paths.VarRoot
	}
	if c.Paths.RunRoot == "" {
		c.Paths.RunRoot = d.Paths.RunRoot
	}
	if c.Paths.BinRoot == "" {
		c.Paths.BinRoot = d.Paths.BinRoot
	}
	if c.Paths.SocketDir == "" {
		c.Paths.SocketDir = d.Paths.SocketDir
	}
	if c.Paths.ResultRoot == "" {
		c.Paths.ResultRoot = d.Paths.ResultRoot
	}
	if c.Limits.MaxWorkers == 0 {
		c.Limits.MaxWorkers = d.Limits.MaxWorkers
	}
	if c.Limits.MaxRunSeconds == 0 {
		c.Limits.MaxRunSeconds = d.Limits.MaxRunSeconds
	}
	if c.Limits.MaxQueueDepth == 0 {
		c.Limits.MaxQueueDepth = d.Limits.MaxQueueDepth
	}
	if c.Limits.MaxDiskMiB == 0 {
		c.Limits.MaxDiskMiB = d.Limits.MaxDiskMiB
	}
}

// Validate enforces schema, image, profile, host, and path rules.
func (c Config) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("config: unsupported schema %d", c.SchemaVersion)
	}
	if strings.TrimSpace(c.Iris.Image) == "" {
		return errors.New("config: iris.image is required")
	}
	for name := range c.Profiles {
		if err := validateID(name); err != nil {
			return fmt.Errorf("config: invalid profile name: %w", err)
		}
	}
	for name, p := range c.Profiles {
		if err := p.Validate(c.Limits); err != nil {
			return fmt.Errorf("config: profile %q: %w", name, err)
		}
	}
	if len(c.Hosts) == 0 {
		return errors.New("config: at least one allowlisted host is required")
	}
	allowed := 0
	seen := map[string]struct{}{}
	for i, h := range c.Hosts {
		if strings.TrimSpace(h.Alias) == "" {
			return fmt.Errorf("config: host %d: alias is required", i)
		}
		if err := validateID(h.Alias); err != nil {
			return fmt.Errorf("config: host %d: invalid alias: %w", i, err)
		}
		if _, dup := seen[h.Alias]; dup {
			return fmt.Errorf("config: duplicate host alias %q", h.Alias)
		}
		seen[h.Alias] = struct{}{}
		if h.Allow {
			allowed++
		}
	}
	if allowed == 0 {
		return errors.New("config: no host is explicitly allowed")
	}
	for _, p := range []struct{ name, val string }{
		{"paths.varRoot", c.Paths.VarRoot},
		{"paths.runRoot", c.Paths.RunRoot},
		{"paths.binRoot", c.Paths.BinRoot},
	} {
		if !strings.HasPrefix(p.val, "/") {
			return fmt.Errorf("config: %s must be absolute", p.name)
		}
	}
	if c.Paths.SocketDir == "" {
		return errors.New("config: paths.socketDir is required")
	}
	if c.Limits.MaxWorkers < 1 {
		return errors.New("config: limits.maxWorkers must be positive")
	}
	if c.Limits.MaxRunSeconds < 1 {
		return errors.New("config: limits.maxRunSeconds must be positive")
	}
	return nil
}

// Validate checks one profile against the configured limits.
func (p Profile) Validate(limits Limits) error {
	switch p.Suite {
	case "judger", "iris":
	default:
		return fmt.Errorf("suite must be judger or iris, got %q", p.Suite)
	}
	if p.Workers < 1 {
		return errors.New("workers must be positive")
	}
	if p.Workers > limits.MaxWorkers {
		return fmt.Errorf("workers %d exceeds limit %d", p.Workers, limits.MaxWorkers)
	}
	if p.Repetitions < 1 {
		return errors.New("repetitions must be positive")
	}
	if p.CPUList != "" && !cpuListPattern.MatchString(p.CPUList) {
		return fmt.Errorf("invalid cpuList %q", p.CPUList)
	}
	if p.NUMAPolicy != "" {
		switch p.NUMAPolicy {
		case "default", "preferred", "bind", "interleave":
		default:
			return fmt.Errorf("invalid numaPolicy %q", p.NUMAPolicy)
		}
	}
	if p.Turbo != "" {
		switch p.Turbo {
		case "on", "off":
		default:
			return fmt.Errorf("invalid turbo %q", p.Turbo)
		}
	}
	return nil
}

// AllowedHost returns the allowlisted host with the given alias.
func (c Config) AllowedHost(alias string) (Host, bool) {
	for _, h := range c.Hosts {
		if h.Alias == alias && h.Allow {
			return h, true
		}
	}
	return Host{}, false
}

// FixtureSet is a checksummed group of fixture files.
type FixtureSet struct {
	Name    string `json:"name"`
	Root    string `json:"root"`
	Digests []struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	} `json:"digests"`
}

// ValidateFixtureSet verifies relative paths and digests.
func ValidateFixtureSet(f FixtureSet) error {
	if f.Name == "" {
		return errors.New("config: fixture set name is required")
	}
	for _, d := range f.Digests {
		if err := artifact.ValidateRelativePath(d.Path); err != nil {
			return fmt.Errorf("config: fixture %q: %w", f.Name, err)
		}
		if !artifact.ValidSHA256(d.SHA256) {
			return fmt.Errorf("config: fixture %q path %q: invalid sha256", f.Name, d.Path)
		}
	}
	return nil
}

func validateID(s string) error {
	if s == "" || len(s) > 128 {
		return fmt.Errorf("invalid identifier length")
	}
	for i, c := range s {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !ok && !(c == '-' || c == '_') {
			return fmt.Errorf("invalid character %q", c)
		}
		if i == 0 && !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return errors.New("must start with alphanumeric")
		}
	}
	return nil
}
