// Package transport wraps the installed OpenSSH client and local process
// execution. It never embeds an SSH implementation and never builds a remote
// shell command by string interpolation: all arguments are separate argv
// entries, and only fixed, validated tokens are placed on the remote command
// line. Untrusted data travels through stdin.
package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/skkuding/iris-load-test-toolset/internal/protocol"
)

// Defaults matching the proposal.
const (
	DefaultSocketDir      = "~/.ssh/sockets"
	DefaultConnectTimeout = 15
	DefaultControlPersist = "600"
)

// remoteTokenPattern guarantees a token cannot be reinterpreted by the remote
// shell that OpenSSH invokes.
var remoteTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_./=:@+-]+$`)

// Options configures an SSH wrapper.
type Options struct {
	SSHBinary      string
	SCPBinary      string
	SocketDir      string
	BatchMode      bool
	ConnectTimeout int
	ControlPersist string
	ExtraOptions   []string
}

func (o Options) withDefaults() Options {
	if o.SSHBinary == "" {
		o.SSHBinary = "ssh"
	}
	if o.SCPBinary == "" {
		o.SCPBinary = "scp"
	}
	if o.SocketDir == "" {
		o.SocketDir = DefaultSocketDir
	}
	if o.ConnectTimeout == 0 {
		o.ConnectTimeout = DefaultConnectTimeout
	}
	if o.ControlPersist == "" {
		o.ControlPersist = DefaultControlPersist
	}
	return o
}

// Validate rejects options that would corrupt argv or smuggle newlines.
func (o Options) Validate() error {
	for _, opt := range o.ExtraOptions {
		if opt == "" || strings.ContainsAny(opt, "\x00\n\r") {
			return fmt.Errorf("transport: invalid ssh option %q", opt)
		}
	}
	if o.ConnectTimeout < 1 {
		return errors.New("transport: connect timeout must be positive")
	}
	if strings.ContainsAny(o.ControlPersist, "\x00\n\r ") {
		return errors.New("transport: invalid control persist value")
	}
	return nil
}

// SSH invokes the agent on one host through the OpenSSH client.
type SSH struct {
	Host string
	Opts Options
}

// ExpandSocketDir resolves a leading ~ using the current user's home.
func ExpandSocketDir(dir string) (string, error) {
	if dir == "" {
		dir = DefaultSocketDir
	}
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("transport: socket dir %q must be absolute", dir)
	}
	return dir, nil
}

// SocketPath returns the deterministic ControlPath for the host, creates the
// socket directory with mode 0700, and enforces the unix socket length limit.
func (s SSH) SocketPath() (string, error) {
	opts := s.Opts.withDefaults()
	if s.Host == "" {
		return "", errors.New("transport: empty host")
	}
	dir, err := ExpandSocketDir(opts.SocketDir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("transport: create socket dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	safe := sanitizeHost(s.Host)
	p := filepath.Join(dir, "iris-bench-"+safe)
	if len(p) >= 104 {
		return "", fmt.Errorf("transport: control socket path too long: %q", p)
	}
	return p, nil
}

func sanitizeHost(host string) string {
	var b strings.Builder
	for _, c := range host {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
			b.WriteRune(c)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// BaseArgs returns the connection options shared by ssh and scp.
func (s SSH) BaseArgs() ([]string, error) {
	if s.Host == "" {
		return nil, errors.New("transport: empty host")
	}
	opts := s.Opts.withDefaults()
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	sock, err := s.SocketPath()
	if err != nil {
		return nil, err
	}
	args := []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + sock,
		"-o", "ControlPersist=" + opts.ControlPersist,
		"-o", fmt.Sprintf("ConnectTimeout=%d", opts.ConnectTimeout),
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
	}
	if opts.BatchMode {
		args = append(args, "-o", "BatchMode=yes")
	}
	for _, opt := range opts.ExtraOptions {
		args = append(args, "-o", opt)
	}
	return args, nil
}

// ValidateRemoteToken rejects a remote command token that is not provably safe
// for the remote shell.
func ValidateRemoteToken(tok string) error {
	if tok == "" {
		return errors.New("transport: empty remote token")
	}
	if !remoteTokenPattern.MatchString(tok) {
		return fmt.Errorf("transport: unsafe remote token %q", tok)
	}
	return nil
}

// CommandArgs builds the full argv for one agent invocation. remote is the
// fixed remote command vector, e.g. the agent path plus a fixed verb.
func (s SSH) CommandArgs(remote []string) ([]string, error) {
	base, err := s.BaseArgs()
	if err != nil {
		return nil, err
	}
	if len(remote) == 0 {
		return nil, errors.New("transport: empty remote command")
	}
	for _, tok := range remote {
		if err := ValidateRemoteToken(tok); err != nil {
			return nil, err
		}
	}
	args := append([]string(nil), base...)
	args = append(args, "--", s.Host)
	args = append(args, remote...)
	return args, nil
}

// Exec runs a local command with args, wiring stdin/stdout/stderr.
func Exec(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// OSExecer runs local commands and captures stdout.
type OSExecer struct{}

// Output runs name with args and returns stdout.
func (OSExecer) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// Download copies a remote path to a local path over scp reusing the control
// socket. remoteRemote is validated as a single safe token.
func (s SSH) Download(ctx context.Context, remotePath, localPath string) error {
	if !filepath.IsAbs(remotePath) {
		return fmt.Errorf("transport: remote path must be absolute: %q", remotePath)
	}
	if err := ValidateRemoteToken(remotePath); err != nil {
		return err
	}
	opts := s.Opts.withDefaults()
	base, err := s.BaseArgs()
	if err != nil {
		return err
	}
	args := append([]string(nil), base...)
	args = append(args, "--", s.Host+":"+remotePath, localPath)
	cmd := exec.CommandContext(ctx, opts.SCPBinary, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Upload copies a local file to an absolute remote path over scp, reusing the
// control socket.
func (s SSH) Upload(ctx context.Context, localPath, remotePath string) error {
	if !filepath.IsAbs(remotePath) {
		return fmt.Errorf("transport: remote path must be absolute: %q", remotePath)
	}
	if err := ValidateRemoteToken(remotePath); err != nil {
		return err
	}
	opts := s.Opts.withDefaults()
	base, err := s.BaseArgs()
	if err != nil {
		return err
	}
	args := append([]string(nil), base...)
	args = append(args, "--", localPath, s.Host+":"+remotePath)
	cmd := exec.CommandContext(ctx, opts.SCPBinary, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// AgentClient invokes the staged agent through SSH, sending one JSON request
// on stdin and validating the NDJSON event stream on stdout.
type AgentClient struct {
	SSH         SSH
	RemoteAgent string
	RemoteArgs  []string
	Stderr      io.Writer
}

// Invoke sends req, forwards each event to sink (which may be nil), and returns
// the single terminal result event.
func (c AgentClient) Invoke(ctx context.Context, req protocol.Request, sink func(protocol.Event) error) (protocol.Event, error) {
	if c.RemoteAgent == "" || !strings.HasPrefix(c.RemoteAgent, "/") {
		return protocol.Event{}, errors.New("transport: remote agent path must be absolute")
	}
	if err := req.Validate(); err != nil {
		return protocol.Event{}, err
	}
	remote := append([]string{c.RemoteAgent}, c.RemoteArgs...)
	args, err := c.SSH.CommandArgs(remote)
	if err != nil {
		return protocol.Event{}, err
	}
	var reqBuf bytes.Buffer
	if err := protocol.EncodeRequest(&reqBuf, req); err != nil {
		return protocol.Event{}, err
	}
	opts := c.SSH.Opts.withDefaults()
	stderr := c.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	cmd := exec.CommandContext(ctx, opts.SSHBinary, args...)
	cmd.Stdin = &reqBuf
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return protocol.Event{}, err
	}
	if err := cmd.Start(); err != nil {
		return protocol.Event{}, err
	}
	reader := protocol.NewEventReader(stdout)
	var terminal protocol.Event
	for {
		ev, derr := reader.Decode()
		if derr == io.EOF {
			break
		}
		if derr != nil {
			_ = cmd.Wait()
			return protocol.Event{}, derr
		}
		if sink != nil {
			if err := sink(ev); err != nil {
				_ = cmd.Wait()
				return protocol.Event{}, err
			}
		}
		if ev.Kind == protocol.KindResult {
			terminal = ev
		}
	}
	if err := reader.Finish(); err != nil {
		_ = cmd.Wait()
		return protocol.Event{}, err
	}
	if err := cmd.Wait(); err != nil {
		return terminal, fmt.Errorf("transport: agent exited: %w", err)
	}
	return terminal, nil
}
