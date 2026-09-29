// Command iris-benchctl is the operator-facing controller. Each subcommand is
// intentionally thin: it loads configuration, builds an immutable run plan,
// and delegates to the orchestrator or transport. Provisioning remains an
// explicit external Ansible operation.
package main

import (
	"flag"
	"fmt"
	"os"
)

const toolVersion = "0.1.0"

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	var err error
	switch args[0] {
	case "plan":
		err = cmdPlan(args[1:])
	case "run":
		err = cmdRun(args[1:])
	case "collect":
		err = cmdCollect(args[1:])
	case "status":
		err = cmdStatus(args[1:])
	case "analyze":
		err = cmdAnalyze(args[1:])
	case "provision", "qualify", "resume":
		err = notImplemented(args[0])
	case "help", "-h", "--help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "iris-benchctl:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `iris-benchctl - reproducible Iris runtime benchmark controller

Usage:
  iris-benchctl plan    --config FILE --host ALIAS --profile NAME [flags]
  iris-benchctl run     --config FILE --host ALIAS --profile NAME [flags]
  iris-benchctl collect --config FILE --host ALIAS --run RUNID [flags]
  iris-benchctl status  --config FILE --run RUNID [--host ALIAS]
  iris-benchctl analyze --run RUN_DIRECTORY

Implemented: plan, run, collect, status, analyze (direct and external-Iris suites).
Not implemented in this build: provision, qualify, resume.
`)
}

func notImplemented(name string) error {
	return fmt.Errorf("%s is not implemented in the minimum viable core", name)
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}
