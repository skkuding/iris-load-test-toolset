package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/skkuding/iris-load-test-toolset/internal/orchestrator"
	"github.com/skkuding/iris-load-test-toolset/internal/protocol"
	"github.com/skkuding/iris-load-test-toolset/internal/transport"
)

func cmdStatus(args []string) error {
	fs := newFlagSet("status")
	var (
		configPath     = fs.String("config", "", "configuration JSON file")
		runID          = fs.String("run", "", "run id")
		host           = fs.String("host", "", "target SSH alias (remote status)")
		agentOverride  = fs.String("agent", "", "remote agent path override")
		sshControlPath = fs.String("ssh-control-path", "", "explicit OpenSSH ControlPath")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *runID == "" {
		return fmt.Errorf("--run is required")
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if *host == "" {
		store := orchestrator.FileStore{Root: filepath.Join(cfg.Paths.ResultRoot, ".state")}
		st, err := store.Load(*runID)
		if err != nil {
			return err
		}
		data, err := json.MarshalIndent(st, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, string(data))
		return nil
	}
	if _, ok := cfg.AllowedHost(*host); !ok {
		return fmt.Errorf("host %q is not allowlisted", *host)
	}
	opID, err := newOpID()
	if err != nil {
		return err
	}
	agent := transport.AgentClient{
		SSH:         makeSSH(cfg, *host, *sshControlPath),
		RemoteAgent: agentPath(cfg, *agentOverride),
		Stderr:      os.Stderr,
	}
	terminal, err := agent.Invoke(context.Background(), protocol.Request{
		ProtocolVersion: protocol.Version,
		OperationID:     opID,
		RunID:           *runID,
		Action:          protocol.ActionStatus,
	}, func(ev protocol.Event) error {
		fmt.Fprintf(os.Stderr, "  %s %s%s %s %s\n", ev.Kind, ev.Phase, ev.Name, ev.Status, ev.Message)
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "remote status: %s\n", terminal.Status)
	return nil
}
