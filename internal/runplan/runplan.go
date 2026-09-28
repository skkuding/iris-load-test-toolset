// Package runplan builds, seals, and validates the immutable resolved run plan.
//
// The plan is the single source of truth for a run. Its SHA-256 is computed
// over canonical JSON and is carried by every request, event, and artifact.
// Image tags must be resolved to manifest digests before a plan is runnable.
package runplan

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/skkuding/iris-load-test-toolset/internal/artifact"
	"github.com/skkuding/iris-load-test-toolset/internal/protocol"
)

// SchemaVersion is the run-plan schema this build writes and accepts.
const SchemaVersion = 1

// DefaultIrisImage is the stage selector used when configuration omits one.
// It is a mutable tag and must be resolved to a digest before execution.
const DefaultIrisImage = "ghcr.io/skkuding/codedang-iris:stage"

const (
	ContainmentIsolated         = "isolated"
	ContainmentProductionCompat = "production-compat"
)

// Target identifies the benchmark host.
type Target struct {
	SSHAlias      string `json:"sshAlias"`
	HostIdentity  string `json:"hostIdentity,omitempty"`
	HostKeySHA256 string `json:"hostKeySha256,omitempty"`
}

// Image is a container image reference with an optional resolved manifest or
// local image digest of the form "sha256:<64 hex>".
type Image struct {
	Reference string `json:"reference"`
	Digest    string `json:"digest,omitempty"`
}

// Fixture is a checksummed benchmark input.
type Fixture struct {
	Name                 string `json:"name"`
	Path                 string `json:"path"`
	SHA256               string `json:"sha256"`
	ExpectedOutputPath   string `json:"expectedOutputPath"`
	ExpectedOutputSHA256 string `json:"expectedOutputSha256"`
}

// StagedFile is an immutable remote file dependency.
type StagedFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// IrisSuite describes an external-data Iris measurement.
type IrisSuite struct {
	Mode                string     `json:"mode"`
	Source              StagedFile `json:"source"`
	SecretEnvironment   StagedFile `json:"secretEnvironment"`
	ProblemID           int        `json:"problemId"`
	Language            string     `json:"language"`
	TimeLimitMS         int        `json:"timeLimitMs"`
	MemoryLimitBytes    int64      `json:"memoryLimitBytes"`
	TestcasesPerRequest int        `json:"testcasesPerRequest"`
	MessageIDStart      int64      `json:"messageIdStart"`
	S3Bucket            string     `json:"s3Bucket"`
}

// Block is one controlled measurement unit.
type Block struct {
	ID          string `json:"id"`
	Suite       string `json:"suite"`
	Profile     string `json:"profile"`
	Workers     int    `json:"workers"`
	CPUList     string `json:"cpuList,omitempty"`
	NUMAPolicy  string `json:"numaPolicy,omitempty"`
	Repetitions int    `json:"repetitions"`
}

// Qualification describes the required host facts and thresholds.
type Qualification struct {
	RequiredFacts    []string    `json:"requiredFacts,omitempty"`
	MinPhysicalCores int         `json:"minPhysicalCores,omitempty"`
	RequireCgroupV2  bool        `json:"requireCgroupV2,omitempty"`
	MaxLoad1         float64     `json:"maxLoad1,omitempty"`
	MaxRunSeconds    int         `json:"maxRunSeconds,omitempty"`
	Report           *StagedFile `json:"report,omitempty"`
}

// Expected declares the counts later validation must confirm.
type Expected struct {
	Blocks          int `json:"blocks"`
	SamplesPerBlock int `json:"samplesPerBlock,omitempty"`
}

// Cleanup describes the allowed teardown scope.
type Cleanup struct {
	RemoveContainers bool `json:"removeContainers"`
	RemoveRunDir     bool `json:"removeRunDir"`
	PreserveEvidence bool `json:"preserveEvidence"`
}

// Plan is the immutable resolved plan. No field may be mutated after sealing.
type Plan struct {
	SchemaVersion             int              `json:"schemaVersion"`
	ToolVersion               string           `json:"toolVersion"`
	RunID                     string           `json:"runId"`
	Seed                      int64            `json:"seed"`
	Target                    Target           `json:"target"`
	Suite                     string           `json:"suite"`
	Profile                   string           `json:"profile"`
	Fixtures                  []Fixture        `json:"fixtures"`
	Blocks                    []Block          `json:"blocks"`
	Images                    map[string]Image `json:"images"`
	JudgerDigest              string           `json:"judgerDigest,omitempty"`
	WorkloadBinary            StagedFile       `json:"workloadBinary"`
	ContainmentMode           string           `json:"containmentMode"`
	RabbitMQDefinitionsDigest string           `json:"rabbitmqDefinitionsDigest,omitempty"`
	RDSGeneration             string           `json:"rdsGeneration,omitempty"`
	S3ObjectSet               string           `json:"s3ObjectSet,omitempty"`
	Qualification             Qualification    `json:"qualification"`
	Expected                  Expected         `json:"expected"`
	Cleanup                   Cleanup          `json:"cleanup"`
	IrisSuite                 *IrisSuite       `json:"irisSuite,omitempty"`
}

// BuildInput is the caller-supplied, pre-resolution plan description.
type BuildInput struct {
	ToolVersion               string
	RunID                     string
	Seed                      int64
	Target                    Target
	Suite                     string
	Profile                   string
	Fixtures                  []Fixture
	Blocks                    []Block
	Images                    map[string]Image
	JudgerDigest              string
	WorkloadBinary            StagedFile
	ContainmentMode           string
	RabbitMQDefinitionsDigest string
	RDSGeneration             string
	S3ObjectSet               string
	Qualification             Qualification
	Expected                  Expected
	Cleanup                   Cleanup
	IrisSuite                 *IrisSuite
}

// Build validates input and returns a structurally valid plan. It does not
// require resolved image digests; use ValidateRunnable for that.
func Build(in BuildInput) (Plan, error) {
	p := Plan{
		SchemaVersion:             SchemaVersion,
		ToolVersion:               in.ToolVersion,
		RunID:                     in.RunID,
		Seed:                      in.Seed,
		Target:                    in.Target,
		Suite:                     in.Suite,
		Profile:                   in.Profile,
		Fixtures:                  append([]Fixture(nil), in.Fixtures...),
		Blocks:                    append([]Block(nil), in.Blocks...),
		Images:                    map[string]Image{},
		JudgerDigest:              in.JudgerDigest,
		WorkloadBinary:            in.WorkloadBinary,
		ContainmentMode:           in.ContainmentMode,
		RabbitMQDefinitionsDigest: in.RabbitMQDefinitionsDigest,
		RDSGeneration:             in.RDSGeneration,
		S3ObjectSet:               in.S3ObjectSet,
		Qualification:             in.Qualification,
		Expected:                  in.Expected,
		Cleanup:                   in.Cleanup,
		IrisSuite:                 in.IrisSuite,
	}
	for k, v := range in.Images {
		if v.Reference == "" && k == "iris" {
			v.Reference = DefaultIrisImage
		}
		p.Images[k] = v
	}
	if _, ok := p.Images["iris"]; !ok {
		p.Images["iris"] = Image{Reference: DefaultIrisImage}
	}
	if p.Expected.Blocks == 0 {
		p.Expected.Blocks = len(p.Blocks)
	}
	if p.ContainmentMode == "" {
		p.ContainmentMode = ContainmentIsolated
	}
	if err := p.Validate(); err != nil {
		return Plan{}, err
	}
	return p, nil
}

// CanonicalJSON returns the deterministic JSON encoding used for the digest.
func (p Plan) CanonicalJSON() ([]byte, error) {
	return json.Marshal(p)
}

// Digest returns the lowercase hex SHA-256 of the canonical plan JSON.
func (p Plan) Digest() (string, error) {
	data, err := p.CanonicalJSON()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// Validate checks structural correctness. Image digests are optional here.
func (p Plan) Validate() error {
	if p.SchemaVersion != SchemaVersion {
		return fmt.Errorf("runplan: unsupported schema %d", p.SchemaVersion)
	}
	if !protocol.ValidRunID(p.RunID) {
		return fmt.Errorf("runplan: invalid runId")
	}
	if p.Suite == "" {
		return fmt.Errorf("runplan: suite is required")
	}
	if p.ContainmentMode != ContainmentIsolated && p.ContainmentMode != ContainmentProductionCompat {
		return fmt.Errorf("runplan: invalid containment mode %q", p.ContainmentMode)
	}
	if len(p.Blocks) == 0 {
		return fmt.Errorf("runplan: at least one block is required")
	}
	if p.Expected.Blocks != len(p.Blocks) {
		return fmt.Errorf("runplan: expected blocks %d does not match plan blocks %d", p.Expected.Blocks, len(p.Blocks))
	}
	if p.Qualification.MaxLoad1 <= 0 {
		return errors.New("runplan: qualification maxLoad1 must be positive")
	}
	seen := make(map[string]struct{}, len(p.Blocks))
	for i, b := range p.Blocks {
		if !protocol.ValidOperationID(b.ID) {
			return fmt.Errorf("runplan: block %d: invalid id", i)
		}
		if _, ok := seen[b.ID]; ok {
			return fmt.Errorf("runplan: duplicate block id %q", b.ID)
		}
		seen[b.ID] = struct{}{}
		if b.Workers < 1 {
			return fmt.Errorf("runplan: block %q: workers must be positive", b.ID)
		}
		if b.Repetitions < 0 {
			return fmt.Errorf("runplan: block %q: repetitions must not be negative", b.ID)
		}
		if p.Suite == "iris" && (b.Suite != p.Suite || b.Repetitions < 1) {
			return fmt.Errorf("runplan: iris block %q must match suite and have positive repetitions", b.ID)
		}
	}
	iris, ok := p.Images["iris"]
	if !ok || iris.Reference == "" {
		return errors.New("runplan: iris image reference is required")
	}
	if iris.Digest != "" && !ValidImageDigest(iris.Digest) {
		return fmt.Errorf("runplan: invalid iris digest %q", iris.Digest)
	}
	for name, img := range p.Images {
		if img.Digest != "" && !ValidImageDigest(img.Digest) {
			return fmt.Errorf("runplan: invalid %s digest %q", name, img.Digest)
		}
	}
	if img, ok := p.Images["judger-bench"]; ok {
		if !ValidImageDigest(img.Reference) || img.Digest != img.Reference {
			return errors.New("runplan: judger-bench image must be an immutable local image ID sealed as matching reference and digest")
		}
	}
	if p.Suite == "judger" && p.ContainmentMode == ContainmentProductionCompat {
		if _, ok := p.Images["judger-bench"]; !ok {
			return errors.New("runplan: production-compat requires a judger-bench OCI image")
		}
	}
	for _, f := range p.Fixtures {
		if f.Name == "" || f.Path == "" || f.ExpectedOutputPath == "" {
			return errors.New("runplan: fixture name, input path, and expected output path are required")
		}
		if !artifact.ValidSHA256(f.SHA256) {
			return fmt.Errorf("runplan: fixture %q has invalid sha256", f.Name)
		}
		if !artifact.ValidSHA256(f.ExpectedOutputSHA256) {
			return fmt.Errorf("runplan: fixture %q has invalid expected output sha256", f.Name)
		}
	}
	if p.Suite == "judger" {
		if !validStagedFile(p.WorkloadBinary) {
			return errors.New("runplan: direct judger workload binary path and sha256 are required")
		}
	}
	if p.Suite == "iris" {
		if p.ContainmentMode != ContainmentProductionCompat {
			return errors.New("runplan: iris suite requires production-compat containment")
		}
		if p.IrisSuite == nil {
			return errors.New("runplan: iris suite configuration is required")
		}
		if len(p.Fixtures) != 0 || p.WorkloadBinary.Path != "" || p.WorkloadBinary.SHA256 != "" {
			return errors.New("runplan: iris suite does not accept direct fixtures or workload")
		}
		if !validStagedFile(p.IrisSuite.Source) || !validStagedFile(p.IrisSuite.SecretEnvironment) {
			return errors.New("runplan: iris source and secret environment staged files are invalid")
		}
		if p.IrisSuite.Mode != "external-data" || p.IrisSuite.ProblemID <= 0 || p.IrisSuite.Language == "" || p.IrisSuite.TimeLimitMS <= 0 || p.IrisSuite.MemoryLimitBytes <= 0 || p.IrisSuite.TestcasesPerRequest <= 0 || p.IrisSuite.MessageIDStart <= 0 || p.IrisSuite.S3Bucket == "" {
			return errors.New("runplan: iris suite fields must be positive and mode must be external-data")
		}
		if p.RDSGeneration == "" || p.S3ObjectSet == "" {
			return errors.New("runplan: iris suite requires rds generation and s3 object set")
		}
		if p.IrisSuite.MessageIDStart > 0 && int64(p.Blocks[0].Workers)*int64(p.Blocks[0].Repetitions) > (1<<63-1)-p.IrisSuite.MessageIDStart {
			return errors.New("runplan: iris message ID range overflows")
		}
		if img, ok := p.Images["rabbitmq"]; !ok || img.Reference == "" {
			return errors.New("runplan: iris suite requires a rabbitmq image")
		}
	}
	if p.Qualification.Report != nil && (!validStagedFile(*p.Qualification.Report) || !strings.HasPrefix(p.Qualification.Report.Path, "/tmp/")) {
		return errors.New("runplan: qualification report path and sha256 are invalid")
	}
	if p.JudgerDigest != "" && !artifact.ValidSHA256(strings.TrimPrefix(p.JudgerDigest, "sha256:")) {
		return fmt.Errorf("runplan: invalid judger digest %q", p.JudgerDigest)
	}
	return nil
}

func validStagedFile(f StagedFile) bool {
	return filepath.IsAbs(f.Path) && artifact.ValidSHA256(f.SHA256)
}

// ValidateRunnable requires the resolved digests that execution depends on.
func (p Plan) ValidateRunnable() error {
	if err := p.Validate(); err != nil {
		return err
	}
	iris := p.Images["iris"]
	if iris.Digest == "" {
		return fmt.Errorf("runplan: iris image %q has no resolved digest", iris.Reference)
	}
	if p.Suite == "iris" {
		rabbit, ok := p.Images["rabbitmq"]
		if !ok || rabbit.Digest == "" {
			return errors.New("runplan: rabbitmq image has no resolved digest")
		}
	}
	if p.JudgerDigest == "" {
		return errors.New("runplan: judger digest is required before execution")
	}
	if p.Suite == "judger" && len(p.Fixtures) != 1 {
		return errors.New("runplan: direct judger execution requires exactly one input/expected-output fixture pair")
	}
	return nil
}

// ValidImageDigest reports whether s is "sha256:" plus 64 lowercase hex.
func ValidImageDigest(s string) bool {
	if !strings.HasPrefix(s, "sha256:") {
		return false
	}
	return artifact.ValidSHA256(strings.TrimPrefix(s, "sha256:"))
}

// CommandRunner is the minimal process boundary the resolver uses.
type CommandRunner interface {
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ImageResolver resolves a mutable image reference to a manifest digest.
type ImageResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// DockerResolver resolves digests with `docker buildx imagetools inspect`.
// If ref already contains a digest it is returned unchanged.
type DockerResolver struct {
	Runner CommandRunner
	Binary string
}

// Resolve implements ImageResolver.
func (r DockerResolver) Resolve(ctx context.Context, ref string) (string, error) {
	if ref == "" {
		return "", errors.New("runplan: empty image reference")
	}
	if i := strings.Index(ref, "@"); i >= 0 {
		digest := ref[i+1:]
		if !ValidImageDigest(digest) {
			return "", fmt.Errorf("runplan: invalid digest in reference %q", ref)
		}
		return digest, nil
	}
	if r.Runner == nil {
		return "", errors.New("runplan: no command runner configured for digest resolution")
	}
	bin := r.Binary
	if bin == "" {
		bin = "docker"
	}
	out, err := r.Runner.Output(ctx, bin, "buildx", "imagetools", "inspect", "--format", "{{.Manifest.Digest}}", ref)
	if err != nil {
		return "", fmt.Errorf("runplan: resolve %q: %w", ref, err)
	}
	digest := strings.TrimSpace(string(out))
	if !ValidImageDigest(digest) {
		return "", fmt.Errorf("runplan: resolve %q: unexpected digest output %q", ref, digest)
	}
	return digest, nil
}

// StaticResolver returns pre-resolved digests from a map. It is used for
// offline planning and tests.
type StaticResolver map[string]string

// Resolve implements ImageResolver.
func (s StaticResolver) Resolve(_ context.Context, ref string) (string, error) {
	if d, ok := s[ref]; ok && ValidImageDigest(d) {
		return d, nil
	}
	if i := strings.Index(ref, "@"); i >= 0 {
		d := ref[i+1:]
		if ValidImageDigest(d) {
			return d, nil
		}
	}
	return "", fmt.Errorf("runplan: no static digest for %q", ref)
}

// GenerateRunID returns an "iris-YYYYMMDD-xxxxxxxx" identifier using random
// base32 material from r.
func GenerateRunID(t time.Time, r io.Reader) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	suffix := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf))
	id := fmt.Sprintf("iris-%s-%s", t.UTC().Format("20060102"), suffix)
	if !protocol.ValidRunID(id) {
		return "", fmt.Errorf("runplan: generated invalid run id %q", id)
	}
	return id, nil
}

// WriteStore persists plan JSON atomically.
func (p Plan) WriteStore(path string) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return artifact.WriteFileAtomic(path, data, 0o644)
}
