// Package qualification validates sealed host qualification evidence.
package qualification

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/skkuding/iris-load-test-toolset/internal/artifact"
	"github.com/skkuding/iris-load-test-toolset/internal/runplan"
)

type report struct {
	Schema            string `json:"schema"`
	InventoryHostname string `json:"inventory_hostname"`
	Nodename          string `json:"nodename"`
	Compatible        bool   `json:"compatible"`
	Verification      *struct {
		Failures *[]json.RawMessage `json:"failures"`
	} `json:"verification"`
}

// Validate re-hashes a copied report and checks its acceptance semantics
// against the report digest and host identity sealed into the plan.
func Validate(path string, plan runplan.Plan) error {
	sealed := plan.Qualification.Report
	if sealed == nil {
		return nil
	}
	sum, _, err := artifact.HashFile(path)
	if err != nil {
		return fmt.Errorf("qualification report: %w", err)
	}
	if sum != sealed.SHA256 {
		return fmt.Errorf("qualification report sha256 %s does not match sealed %s", sum, sealed.SHA256)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("qualification report: %w", err)
	}
	var got report
	if err := json.Unmarshal(data, &got); err != nil {
		return fmt.Errorf("qualification report: %w", err)
	}
	if got.Schema != "iris-benchmark-qualification/v1" {
		return fmt.Errorf("qualification report has unsupported schema %q", got.Schema)
	}
	if !got.Compatible {
		return errors.New("qualification report is not compatible")
	}
	if got.Verification == nil || got.Verification.Failures == nil {
		return errors.New("qualification report has no verification failures evidence")
	}
	if len(*got.Verification.Failures) != 0 {
		return fmt.Errorf("qualification report has %d verification failure(s)", len(*got.Verification.Failures))
	}
	want := strings.TrimSpace(plan.Target.HostIdentity)
	if want == "" {
		want = plan.Target.SSHAlias
	}
	if got.Nodename != want && got.InventoryHostname != want {
		return fmt.Errorf("qualification report host identity is %q/%q, want %q", got.InventoryHostname, got.Nodename, want)
	}
	return nil
}
