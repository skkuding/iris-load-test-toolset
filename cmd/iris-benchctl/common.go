package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/skkuding/iris-load-test-toolset/internal/artifact"
	"github.com/skkuding/iris-load-test-toolset/internal/config"
	"github.com/skkuding/iris-load-test-toolset/internal/runplan"
	"github.com/skkuding/iris-load-test-toolset/internal/transport"
)

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// planOptions are the flags shared by plan and run.
type planOptions struct {
	configPath          string
	host                string
	profile             string
	suite               string
	runID               string
	irisDigest          string
	judgerDigest        string
	judgerDigestFile    string
	benchmarkImage      string
	resolveImage        bool
	seed                int64
	fixtures            stringList
	expectedOutputs     stringList
	benchBinary         string
	benchBinarySHA      string
	localBenchBinary    string
	qualificationReport string
	productionCompat    bool
	sshControlPath      string
}

func (o *planOptions) register(fs *flag.FlagSet) {
	fs.StringVar(&o.configPath, "config", "", "configuration JSON file")
	fs.StringVar(&o.host, "host", "", "target SSH alias from the allowlist")
	fs.StringVar(&o.profile, "profile", "", "profile name from configuration")
	fs.StringVar(&o.suite, "suite", "", "override suite (judger or iris)")
	fs.StringVar(&o.runID, "run-id", "", "explicit run id (default generated)")
	fs.StringVar(&o.irisDigest, "iris-digest", "", "resolved Iris manifest digest (sha256:...)")
	fs.StringVar(&o.judgerDigest, "judger-digest", "", "Judger artifact SHA-256 (raw hex or sha256:...)")
	fs.StringVar(&o.judgerDigestFile, "judger-digest-file", "", "file whose SHA-256 is the Judger digest")
	fs.StringVar(&o.benchmarkImage, "benchmark-image", "", "immutable local judger-bench Docker image ID (sha256:<64 lowercase hex>)")
	fs.BoolVar(&o.resolveImage, "resolve-image", false, "resolve the Iris tag with docker buildx imagetools")
	fs.Int64Var(&o.seed, "seed", 1, "run random seed")
	fs.Var(&o.fixtures, "fixture", "fixture name=path (repeatable)")
	fs.Var(&o.expectedOutputs, "expected-output", "expected output name=path (repeatable; must match --fixture)")
	fs.StringVar(&o.benchBinary, "bench-binary", "", "REMOTE precompiled direct-suite workload binary")
	fs.StringVar(&o.benchBinarySHA, "bench-binary-sha256", "", "SHA-256 of the REMOTE --bench-binary")
	fs.BoolVar(&o.productionCompat, "production-compat", false, "accept stock alpha.4 root-level sandbox cgroups as an uncontained, non-comparable population")
	fs.StringVar(&o.sshControlPath, "ssh-control-path", "", "explicit OpenSSH ControlPath (default <socketDir>/iris-bench-<host>)")
}

func loadConfig(path string) (config.Config, error) {
	if path == "" {
		return config.Config{}, errors.New("--config is required")
	}
	return config.Load(path)
}

// buildPlan resolves a run plan and its SHA-256. It requires a resolved image
// digest and a Judger digest; tag-only plans are blocked here by design.
func (o *planOptions) buildPlan(ctx context.Context, cfg config.Config) (runplan.Plan, string, error) {
	if o.host == "" {
		return runplan.Plan{}, "", errors.New("--host is required")
	}
	host, ok := cfg.AllowedHost(o.host)
	if !ok {
		return runplan.Plan{}, "", fmt.Errorf("host %q is not allowlisted", o.host)
	}
	if o.profile == "" {
		return runplan.Plan{}, "", errors.New("--profile is required")
	}
	prof, ok := cfg.Profiles[o.profile]
	if !ok {
		return runplan.Plan{}, "", fmt.Errorf("unknown profile %q", o.profile)
	}
	suite := prof.Suite
	if o.suite != "" {
		suite = o.suite
	}
	switch suite {
	case "judger", "iris":
	default:
		return runplan.Plan{}, "", fmt.Errorf("invalid suite %q", suite)
	}
	runID := o.runID
	if runID == "" {
		id, err := runplan.GenerateRunID(time.Now(), rand.Reader)
		if err != nil {
			return runplan.Plan{}, "", err
		}
		runID = id
	}
	fixtures, err := o.resolveFixtures(runID)
	if err != nil {
		return runplan.Plan{}, "", err
	}
	irisDigest := o.irisDigest
	if irisDigest == "" && o.resolveImage {
		resolver := runplan.DockerResolver{Runner: transport.OSExecer{}}
		irisDigest, err = resolver.Resolve(ctx, cfg.Iris.Image)
		if err != nil {
			return runplan.Plan{}, "", err
		}
	}
	judger, err := o.resolveJudger()
	if err != nil {
		return runplan.Plan{}, "", err
	}
	workload, err := o.resolveWorkload(runID)
	if err != nil {
		return runplan.Plan{}, "", err
	}
	qualification := runplan.Qualification{RequireCgroupV2: true, MinPhysicalCores: 1, MaxLoad1: cfg.Limits.MaxLoad1, MaxRunSeconds: cfg.Limits.MaxRunSeconds}
	if o.qualificationReport != "" {
		sum, _, err := artifact.HashFile(o.qualificationReport)
		if err != nil {
			return runplan.Plan{}, "", fmt.Errorf("qualification report: %w", err)
		}
		qualification.Report = &runplan.StagedFile{Path: stagedPath(runID, "qualification.json"), SHA256: sum}
	}
	images := map[string]runplan.Image{
		"iris": {Reference: cfg.Iris.Image, Digest: irisDigest},
	}
	if o.benchmarkImage != "" {
		if !runplan.ValidImageDigest(o.benchmarkImage) {
			return runplan.Plan{}, "", fmt.Errorf("--benchmark-image must be exactly sha256:<64 lowercase hex>")
		}
		images["judger-bench"] = runplan.Image{Reference: o.benchmarkImage, Digest: o.benchmarkImage}
	}
	if o.productionCompat && o.benchmarkImage == "" {
		return runplan.Plan{}, "", errors.New("--production-compat requires --benchmark-image")
	}
	if cfg.Iris.RabbitMQImage != "" {
		images["rabbitmq"] = runplan.Image{Reference: cfg.Iris.RabbitMQImage}
	}
	blockID := o.profile + "-01"
	plan, err := runplan.Build(runplan.BuildInput{
		ToolVersion:    toolVersion,
		RunID:          runID,
		Seed:           o.seed,
		Target:         runplan.Target{SSHAlias: host.Alias, HostIdentity: host.HostIdentity},
		Suite:          suite,
		Profile:        o.profile,
		Fixtures:       fixtures,
		Images:         images,
		JudgerDigest:   judger,
		WorkloadBinary: workload,
		ContainmentMode: func() string {
			if o.productionCompat {
				return runplan.ContainmentProductionCompat
			}
			return runplan.ContainmentIsolated
		}(),
		Blocks: []runplan.Block{{
			ID:          blockID,
			Suite:       suite,
			Profile:     o.profile,
			Workers:     prof.Workers,
			CPUList:     prof.CPUList,
			NUMAPolicy:  prof.NUMAPolicy,
			Repetitions: prof.Repetitions,
		}},
		Qualification: qualification,
		Cleanup:       runplan.Cleanup{RemoveContainers: true, RemoveRunDir: true, PreserveEvidence: true},
	})
	if err != nil {
		return runplan.Plan{}, "", err
	}
	if err := plan.ValidateRunnable(); err != nil {
		return runplan.Plan{}, "", err
	}
	sha, err := plan.Digest()
	if err != nil {
		return runplan.Plan{}, "", err
	}
	return plan, sha, nil
}

func (o *planOptions) resolveFixtures(runID string) ([]runplan.Fixture, error) {
	expected := make(map[string]string, len(o.expectedOutputs))
	for _, spec := range o.expectedOutputs {
		name, path, ok := strings.Cut(spec, "=")
		if !ok || name == "" || path == "" {
			return nil, fmt.Errorf("invalid --expected-output %q (want name=path)", spec)
		}
		if _, duplicate := expected[name]; duplicate {
			return nil, fmt.Errorf("duplicate --expected-output name %q", name)
		}
		expected[name] = path
	}
	var out []runplan.Fixture
	seen := make(map[string]bool, len(o.fixtures))
	for _, spec := range o.fixtures {
		name, path, ok := strings.Cut(spec, "=")
		if !ok || name == "" || path == "" {
			return nil, fmt.Errorf("invalid --fixture %q (want name=path)", spec)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate --fixture name %q", name)
		}
		seen[name] = true
		expectedPath, ok := expected[name]
		if !ok {
			return nil, fmt.Errorf("fixture %q has no matching --expected-output", name)
		}
		path, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("fixture %q path: %w", name, err)
		}
		expectedPath, err = filepath.Abs(expectedPath)
		if err != nil {
			return nil, fmt.Errorf("expected output %q path: %w", name, err)
		}
		sum, _, err := artifact.HashFile(path)
		if err != nil {
			return nil, fmt.Errorf("fixture %q: %w", name, err)
		}
		expectedSum, _, err := artifact.HashFile(expectedPath)
		if err != nil {
			return nil, fmt.Errorf("expected output %q: %w", name, err)
		}
		i := len(out)
		out = append(out, runplan.Fixture{Name: name, Path: stagedPath(runID, fmt.Sprintf("fixture-%02d.input", i)), SHA256: sum, ExpectedOutputPath: stagedPath(runID, fmt.Sprintf("fixture-%02d.expected", i)), ExpectedOutputSHA256: expectedSum})
	}
	if len(expected) != len(seen) {
		return nil, errors.New("every --expected-output must have a matching --fixture")
	}
	return out, nil
}

func (o *planOptions) resolveWorkload(runID string) (runplan.StagedFile, error) {
	if o.localBenchBinary != "" {
		if o.benchBinary != "" || o.benchBinarySHA != "" {
			return runplan.StagedFile{}, errors.New("--local-bench-binary cannot be combined with remote --bench-binary flags")
		}
		sum, _, err := artifact.HashFile(o.localBenchBinary)
		if err != nil {
			return runplan.StagedFile{}, fmt.Errorf("local workload binary: %w", err)
		}
		return runplan.StagedFile{Path: stagedPath(runID, "workload"), SHA256: sum}, nil
	}
	if o.benchBinary == "" || o.benchBinarySHA == "" {
		return runplan.StagedFile{}, errors.New("direct suite requires --local-bench-binary or both remote --bench-binary and --bench-binary-sha256")
	}
	if !filepath.IsAbs(o.benchBinary) {
		return runplan.StagedFile{}, errors.New("--bench-binary REMOTE path must be absolute")
	}
	sum := strings.TrimPrefix(o.benchBinarySHA, "sha256:")
	if !artifact.ValidSHA256(sum) {
		return runplan.StagedFile{}, errors.New("--bench-binary-sha256 is invalid")
	}
	return runplan.StagedFile{Path: o.benchBinary, SHA256: sum}, nil
}

func stagedPath(runID, suffix string) string {
	return filepath.Join("/tmp", "iris-bench-"+runID, suffix)
}

func (o *planOptions) resolveJudger() (string, error) {
	if o.judgerDigestFile != "" {
		sum, _, err := artifact.HashFile(o.judgerDigestFile)
		if err != nil {
			return "", fmt.Errorf("judger file: %w", err)
		}
		return "sha256:" + sum, nil
	}
	if o.judgerDigest == "" {
		return "", errors.New("judger digest is required (--judger-digest or --judger-digest-file)")
	}
	d := o.judgerDigest
	if !strings.HasPrefix(d, "sha256:") {
		d = "sha256:" + d
	}
	if !runplan.ValidImageDigest(d) {
		return "", fmt.Errorf("invalid judger digest %q", o.judgerDigest)
	}
	return d, nil
}

func newOpID() (string, error) {
	buf := make([]byte, 10)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "op-" + hex.EncodeToString(buf), nil
}

// agentPath builds the versioned remote agent path.
func agentPath(cfg config.Config, override string) string {
	if override != "" {
		return override
	}
	return filepath.Join(cfg.Paths.BinRoot, toolVersion, "iris-bench-agent")
}

func makeSSH(cfg config.Config, host, controlPath string) transport.SSH {
	if controlPath == "" {
		controlPath = cfg.Paths.SSHControlPath
	}
	return transport.SSH{
		Host: host,
		Opts: transport.Options{
			SocketDir:      cfg.Paths.SocketDir,
			ControlPath:    controlPath,
			BatchMode:      true,
			ConnectTimeout: transport.DefaultConnectTimeout,
		},
	}
}
