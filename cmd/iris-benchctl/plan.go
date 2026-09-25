package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
)

func cmdPlan(args []string) error {
	fs := newFlagSet("plan")
	var o planOptions
	o.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	cfg, err := loadConfig(o.configPath)
	if err != nil {
		return err
	}
	plan, sha, err := o.buildPlan(context.Background(), cfg)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, string(data))
	fmt.Fprintf(os.Stderr, "plan sha256: %s\n", sha)
	return nil
}
