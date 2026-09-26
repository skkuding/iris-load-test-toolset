package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/skkuding/iris-load-test-toolset/internal/analyze"
	"github.com/skkuding/iris-load-test-toolset/internal/manifest"
	"github.com/skkuding/iris-load-test-toolset/internal/runplan"
	"github.com/skkuding/iris-load-test-toolset/internal/telemetry"
)

type analysisReport struct {
	SchemaVersion int                  `json:"schemaVersion"`
	Samples       []string             `json:"samples"`
	Summary       analyze.Summary      `json:"summary"`
	Telemetry     telemetry.Assessment `json:"telemetry"`
	Comparable    bool                 `json:"comparable"`
	Reasons       []string             `json:"reasons,omitempty"`
}

func cmdAnalyze(args []string) error {
	return runAnalyze(args, os.Stdout)
}

func runAnalyze(args []string, out io.Writer) error {
	fs := newFlagSet("analyze")
	runDir := fs.String("run", "", "collected run directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *runDir == "" {
		return errors.New("--run is required")
	}
	planData, err := os.ReadFile(filepath.Join(*runDir, "plan.json"))
	if err != nil {
		return fmt.Errorf("read plan.json: %w", err)
	}
	var plan runplan.Plan
	if err := json.Unmarshal(planData, &plan); err != nil {
		return fmt.Errorf("parse plan.json: %w", err)
	}
	if err := plan.Validate(); err != nil {
		return err
	}
	paths := make([]string, 0, len(plan.Blocks))
	files := make([]string, 0, len(plan.Blocks))
	blockIDs := make([]string, 0, len(plan.Blocks))
	for _, block := range plan.Blocks {
		rel := filepath.ToSlash(filepath.Join("samples", block.ID+".ndjson"))
		paths = append(paths, filepath.Join(*runDir, filepath.FromSlash(rel)))
		files = append(files, rel)
		blockIDs = append(blockIDs, block.ID)
	}
	summary, err := analyze.NDJSONFiles(paths)
	if err != nil {
		return err
	}

	t := telemetry.AssessBlocks(*runDir, blockIDs)
	report := analysisReport{
		SchemaVersion: 1,
		Samples:       files,
		Summary:       summary,
		Telemetry:     t,
		Comparable:    t.Comparable,
		Reasons:       append([]string(nil), t.Reasons...),
	}
	if _, err := os.Stat(filepath.Join(*runDir, "COMPLETE")); err != nil {
		report.reject("collected bundle has no COMPLETE marker")
	}
	manifestPath := filepath.Join(*runDir, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		report.reject("collected bundle has no readable manifest.json")
	} else {
		var m manifest.Manifest
		if json.Unmarshal(data, &m) != nil || m.Validate() != nil {
			report.reject("collected bundle has an invalid manifest.json")
		} else if !m.Comparable {
			report.reject("run manifest is marked non-comparable")
		}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

func (r *analysisReport) reject(reason string) {
	r.Comparable = false
	r.Reasons = append(r.Reasons, reason)
}
