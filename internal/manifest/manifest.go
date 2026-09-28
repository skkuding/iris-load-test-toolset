// Package manifest constructs and validates the immutable run manifest that
// accompanies a result bundle. The manifest records identity, digests,
// effective controls, and validation outcomes, and must never contain secret
// values.
package manifest

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/skkuding/iris-load-test-toolset/internal/artifact"
	"github.com/skkuding/iris-load-test-toolset/internal/protocol"
	"github.com/skkuding/iris-load-test-toolset/internal/runplan"
)

// SchemaVersion is the manifest schema this build writes and accepts.
const SchemaVersion = 1

// Outcome records one validation result.
type Outcome struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Manifest is the run-level metadata for a result bundle.
type Manifest struct {
	SchemaVersion        int                      `json:"schemaVersion"`
	RunID                string                   `json:"runId"`
	PlanSHA256           string                   `json:"planSha256"`
	ToolVersion          string                   `json:"toolVersion"`
	AgentVersion         string                   `json:"agentVersion,omitempty"`
	GitCommit            string                   `json:"gitCommit,omitempty"`
	GitDirty             bool                     `json:"gitDirty"`
	CreatedAt            time.Time                `json:"createdAt"`
	Images               map[string]runplan.Image `json:"images"`
	JudgerDigest         string                   `json:"judgerDigest,omitempty"`
	WorkloadBinarySHA256 string                   `json:"workloadBinarySha256"`
	Fixtures             []runplan.Fixture        `json:"fixtures,omitempty"`
	HostFacts            map[string]string        `json:"hostFacts,omitempty"`
	Outcomes             []Outcome                `json:"outcomes,omitempty"`
	Overrides            []string                 `json:"overrides,omitempty"`
	Comparable           bool                     `json:"comparable"`
	ContainmentMode      string                   `json:"containmentMode"`
	Suite                string                   `json:"suite,omitempty"`
}

// Build derives a manifest from a plan and run identity.
func Build(plan runplan.Plan, planSHA256, agentVersion, gitCommit string, dirty bool, now time.Time) (Manifest, error) {
	if !artifact.ValidSHA256(planSHA256) {
		return Manifest{}, errors.New("manifest: invalid plan digest")
	}
	m := Manifest{
		SchemaVersion:        SchemaVersion,
		RunID:                plan.RunID,
		PlanSHA256:           planSHA256,
		ToolVersion:          plan.ToolVersion,
		AgentVersion:         agentVersion,
		GitCommit:            gitCommit,
		GitDirty:             dirty,
		CreatedAt:            now.UTC(),
		Images:               plan.Images,
		JudgerDigest:         plan.JudgerDigest,
		WorkloadBinarySHA256: plan.WorkloadBinary.SHA256,
		Fixtures:             append([]runplan.Fixture(nil), plan.Fixtures...),
		Comparable:           true,
		ContainmentMode:      plan.ContainmentMode,
		Suite:                plan.Suite,
	}
	if plan.ContainmentMode == runplan.ContainmentProductionCompat {
		m.MarkOverride("stock Judger alpha.4 root-level sandbox cgroups; uncontained production-compat population")
	}
	if plan.Qualification.Report == nil {
		m.MarkOverride("no sealed Ansible qualification report; run is accepted only as non-comparable")
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// Validate checks required identity and digest fields.
func (m Manifest) Validate() error {
	if m.SchemaVersion != SchemaVersion {
		return fmt.Errorf("manifest: unsupported schema %d", m.SchemaVersion)
	}
	if !protocol.ValidRunID(m.RunID) {
		return errors.New("manifest: invalid run id")
	}
	if !artifact.ValidSHA256(m.PlanSHA256) {
		return errors.New("manifest: invalid plan digest")
	}
	if m.ToolVersion == "" {
		return errors.New("manifest: tool version is required")
	}
	if len(m.Images) == 0 {
		return errors.New("manifest: at least one image is required")
	}
	for name, img := range m.Images {
		if img.Digest != "" && !runplan.ValidImageDigest(img.Digest) {
			return fmt.Errorf("manifest: image %q has invalid digest", name)
		}
	}
	if m.JudgerDigest != "" && !strings.HasPrefix(m.JudgerDigest, "sha256:") {
		return fmt.Errorf("manifest: judger digest must be a sha256 reference")
	}
	if m.Suite != "iris" && !artifact.ValidSHA256(m.WorkloadBinarySHA256) {
		return errors.New("manifest: invalid workload binary digest")
	}
	for _, o := range m.Outcomes {
		if o.Name == "" {
			return errors.New("manifest: outcome name is required")
		}
		switch o.Status {
		case protocol.StatusPassed, protocol.StatusFailed, protocol.StatusUnsupported:
		default:
			return fmt.Errorf("manifest: outcome %q has invalid status %q", o.Name, o.Status)
		}
	}
	if len(m.Overrides) > 0 && m.Comparable {
		return errors.New("manifest: overridden runs must be marked non-comparable")
	}
	if m.ContainmentMode != runplan.ContainmentIsolated && m.ContainmentMode != runplan.ContainmentProductionCompat {
		return fmt.Errorf("manifest: invalid containment mode %q", m.ContainmentMode)
	}
	if m.ContainmentMode == runplan.ContainmentProductionCompat && m.Comparable {
		return errors.New("manifest: production-compat runs must be non-comparable")
	}
	return nil
}

// AddOutcome appends a validation outcome and updates comparability: any
// failed or unsupported check makes the run non-comparable.
func (m *Manifest) AddOutcome(name, status, detail string) {
	m.Outcomes = append(m.Outcomes, Outcome{Name: name, Status: status, Detail: detail})
	if status != protocol.StatusPassed {
		m.Comparable = false
	}
}

// MarkOverride records a diagnostic-only override and forces non-comparable.
func (m *Manifest) MarkOverride(reason string) {
	if reason == "" {
		reason = "unspecified override"
	}
	m.Overrides = append(m.Overrides, reason)
	m.Comparable = false
}

// SecretsAbsent fails if any known secret value appears in manifest fields.
func (m Manifest) SecretsAbsent(secrets []string) error {
	probe := strings.Join([]string{
		m.RunID, m.PlanSHA256, m.ToolVersion, m.AgentVersion, m.GitCommit, m.JudgerDigest, m.WorkloadBinarySHA256,
	}, "\n")
	for _, hv := range m.HostFacts {
		probe += "\n" + hv
	}
	for _, o := range m.Outcomes {
		probe += "\n" + o.Detail
	}
	for _, s := range secrets {
		if len(s) >= 4 && strings.Contains(probe, s) {
			return errors.New("manifest: potential secret value present")
		}
	}
	return nil
}

// WriteAtomic persists the manifest as JSON.
func (m Manifest) WriteAtomic(path string) error {
	if err := m.Validate(); err != nil {
		return err
	}
	return artifact.WriteJSONAtomic(path, m, 0o644)
}
