// Package fulliris runs one run-scoped, externally-backed Iris benchmark block.
package fulliris

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"
	"github.com/rabbitmq/amqp091-go"
	"github.com/skkuding/iris-load-test-toolset/internal/artifact"
	"github.com/skkuding/iris-load-test-toolset/internal/containment"
)

const (
	exchange     = "iris.e.direct.judge"
	requestQueue = "client.q.judge.benchmark"
	resultQueue  = "iris.q.judge.benchmark-result"
	requestKey   = "judge.benchmark"
	resultKey    = "judge.benchmark.result"
	version      = 1
	// cleanupTimeout bounds teardown, which at high replica counts removes many
	// sandbox roots and container logs and must not share the measurement budget.
	cleanupTimeout = 5 * time.Minute
)

// Config describes one block. Images must be immutable digest references.
type Config struct {
	RunID, BlockID                                                                        string
	Workers, Repetitions                                                                  int
	CPUList, CgroupMount                                                                  string
	IrisImage, RabbitMQImage, JudgerDigest, SourcePath, SecretEnvPath, RunDir, RuntimeDir string
	S3Bucket                                                                              string
	ProblemID                                                                             int
	Language                                                                              string
	TimeLimitMS                                                                           int
	MemoryLimitBytes                                                                      int64
	TestcasesPerRequest                                                                   int
	MessageIDStart                                                                        int64
	Timeout                                                                               time.Duration
}

// Result is the persisted receipt and its canonical JSON hash.
type Result struct {
	Receipt       Receipt
	ReceiptSHA256 string
}

// Receipt is the auditable conservation record for a block.
type Receipt struct {
	SchemaVersion                    int    `json:"schemaVersion"`
	RunID                            string `json:"runId"`
	BlockID                          string `json:"blockId"`
	Status                           string `json:"status"`
	ExpectedRequests                 int    `json:"expectedRequests"`
	PublishedRequests                int    `json:"publishedRequests"`
	ConfirmedRequests                int    `json:"confirmedRequests"`
	ExpectedTestcaseDeliveries       int    `json:"expectedTestcaseDeliveries"`
	ObservedTestcaseDeliveries       int    `json:"observedTestcaseDeliveries"`
	ExpectedTerminalDeliveries       int    `json:"expectedTerminalDeliveries"`
	ObservedTerminalDeliveries       int    `json:"observedTerminalDeliveries"`
	DuplicateCount                   int    `json:"duplicateCount"`
	RedeliveredCount                 int    `json:"redeliveredCount"`
	UnexpectedCount                  int    `json:"unexpectedCount"`
	MissingCount                     int    `json:"missingCount"`
	QueueDepthBeforePublish          int    `json:"queueDepthBeforePublish"`
	QueueDepthAfterPublish           int    `json:"queueDepthAfterPublish"`
	QueueDepthAfterCompletion        int    `json:"queueDepthAfterCompletion"`
	RequestQueueDepthAfterCompletion int    `json:"requestQueueDepthAfterCompletion"`
	ResultQueueDepthAfterCompletion  int    `json:"resultQueueDepthAfterCompletion"`
	RDSActiveTestcases               int    `json:"rdsActiveTestcases"`
	SampleCount                      int    `json:"sampleCount"`
	SampleSHA256                     string `json:"sampleSha256"`
	IrisImage                        string `json:"irisImage"`
	RabbitMQImage                    string `json:"rabbitMqImage"`
	StartedAtUnixNano                int64  `json:"startedAtUnixNano"`
	FinishedAtUnixNano               int64  `json:"finishedAtUnixNano"`
	DurationMS                       int64  `json:"durationMs"`
}

type sample struct {
	SchemaVersion int    `json:"schemaVersion"`
	RunID         string `json:"runId"`
	BlockID       string `json:"blockId"`
	WorkerID      string `json:"workerId"`
	RequestID     string `json:"requestId"`
	Iteration     int    `json:"iteration"`
	Status        string `json:"status"`
	CPUTimeMS     int64  `json:"cpuTimeMs"`
	RealTimeMS    int64  `json:"realTimeMs"`
	MemoryBytes   int64  `json:"memoryBytes"`
	PublishAtNS   int64  `json:"publishAtNs"`
	ReceiveAtNS   int64  `json:"receiveAtNs"`
	EndToEndMS    int64  `json:"endToEndMs"`
	ResultCode    string `json:"resultCode,omitempty"`
	ErrorCode     string `json:"errorCode,omitempty"`
	TestcaseID    string `json:"testcaseId,omitempty"`
	Redelivered   bool   `json:"redelivered"`
}

var idRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
var runRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,126}[a-z0-9])?$`)
var imageRE = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)

// Validate performs all checks that do not require Docker or RabbitMQ.
func validateConfig(runDir string, cfg Config) error {
	if !runRE.MatchString(cfg.RunID) || !idRE.MatchString(cfg.BlockID) {
		return errors.New("fulliris: invalid run or block identifier")
	}
	if cfg.Workers <= 0 || cfg.Repetitions <= 0 || cfg.ProblemID <= 0 || cfg.TimeLimitMS <= 0 || cfg.MemoryLimitBytes <= 0 || cfg.TestcasesPerRequest <= 0 || cfg.MessageIDStart <= 0 || cfg.Workers > int(^uint(0)>>1)/cfg.Repetitions || cfg.MessageIDStart > (1<<63-1)-int64(cfg.Workers*cfg.Repetitions) {
		return errors.New("fulliris: invalid positive configuration")
	}
	if cfg.Language == "" || !safeName(cfg.S3Bucket) {
		return errors.New("fulliris: language and S3Bucket are required")
	}
	if cfg.Timeout <= 0 {
		return errors.New("fulliris: timeout must be positive")
	}
	if !imageRE.MatchString(cfg.IrisImage) || !imageRE.MatchString(cfg.RabbitMQImage) {
		return errors.New("fulliris: images must be name@sha256:<64 hex>")
	}
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(cfg.JudgerDigest) {
		return errors.New("fulliris: Judger digest must be sha256:<64 hex>")
	}
	if filepath.Clean(runDir) != filepath.Clean(cfg.RunDir) || !filepath.IsAbs(cfg.RunDir) || filepath.Clean(cfg.RunDir) != cfg.RunDir || !filepath.IsAbs(cfg.RuntimeDir) || filepath.Clean(cfg.RuntimeDir) != cfg.RuntimeDir || !filepath.IsAbs(cfg.SourcePath) || !filepath.IsAbs(cfg.SecretEnvPath) {
		return errors.New("fulliris: paths must be absolute and runDir must equal cfg.RunDir")
	}
	if err := safeDir(cfg.RunDir); err != nil {
		return err
	}
	if err := safeDir(cfg.RuntimeDir); err != nil {
		return err
	}
	for _, p := range []string{cfg.SourcePath, cfg.SecretEnvPath} {
		st, err := os.Lstat(p)
		if err != nil || !st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0 || filepath.Clean(p) != p {
			return fmt.Errorf("fulliris: required regular file %q", p)
		}
	}
	if cfg.CPUList != "" {
		if _, err := containment.SplitCPUList(cfg.CPUList, cfg.Workers); err != nil {
			return err
		}
	}
	if !filepath.IsAbs(cfg.CgroupMount) || filepath.Clean(cfg.CgroupMount) != cfg.CgroupMount {
		return errors.New("fulliris: invalid cgroup mount")
	}
	if err := safeDir(cfg.CgroupMount); err != nil {
		return err
	}
	return nil
}

func safeName(s string) bool {
	return s != "" && len(s) <= 255 && regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*[a-z0-9]$`).MatchString(s)
}

func safeDir(p string) error {
	st, err := os.Stat(p)
	if err != nil || !st.IsDir() {
		return fmt.Errorf("fulliris: directory %q is unavailable", p)
	}
	return nil
}

type docker interface {
	Run(context.Context, ...string) ([]byte, error)
}
type cliDocker struct{}

func (cliDocker) Run(ctx context.Context, args ...string) ([]byte, error) {
	return execCommandContext(ctx, args...)
}

// execCommandContext is a variable seam for tests and keeps Docker invocation argv-only.
var execCommandContext = func(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, sanitizeLog(out))
	}
	return out, nil
}

// Run executes the block. Docker is intentionally kept behind newDocker for callers/tests
// that provide an in-process implementation in this package.
func Run(ctx context.Context, cfg Config) (out Result, err error) {
	if err = validateConfig(cfg.RunDir, cfg); err != nil {
		return out, err
	}
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > cfg.Timeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}
	d := newDocker()
	name := names(cfg)
	started := time.Now().UTC()
	cleaned := false
	defer func() {
		if cleaned {
			return
		}
		cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		err = errors.Join(err, cleanup(cleanCtx, d, name, cfg))
	}()
	if err = os.MkdirAll(filepath.Join(cfg.RunDir, "samples"), 0755); err != nil {
		return out, err
	}
	if err = os.MkdirAll(filepath.Join(cfg.RunDir, "receipts"), 0755); err != nil {
		return out, err
	}
	if err = os.MkdirAll(filepath.Join(cfg.RunDir, "logs"), 0755); err != nil {
		return out, err
	}
	// The complete live path is intentionally explicit; failures never leave a container unmanaged.
	creds, e := randomToken()
	if e != nil {
		return out, e
	}
	if err = d.verifyJudger(ctx, cfg); err != nil {
		return out, err
	}
	rdsCount, err := probeRDS(ctx, cfg.SecretEnvPath, cfg.ProblemID)
	if err != nil {
		return out, err
	}
	if err = d.network(ctx, "create", name.network); err != nil {
		return out, err
	}
	if err = d.rabbit(ctx, cfg, name, creds); err != nil {
		return out, err
	}
	port, e := d.port(ctx, name.rabbit)
	if e != nil {
		return out, e
	}
	conn, e := dialRabbit(ctx, port, creds)
	if e != nil {
		return out, e
	}
	defer conn.Close()
	if err = topology(conn); err != nil {
		return out, err
	}
	msgs := cfg.Workers * cfg.Repetitions
	collector, err := startCollector(conn, cfg)
	if err != nil {
		return out, err
	}
	confirmed, before, after, publishedAt, err := publishRequests(ctx, conn, cfg, msgs)
	if err != nil {
		return out, err
	}
	if after != msgs {
		return out, fmt.Errorf("fulliris: request queue depth %d, want %d", after, msgs)
	}
	if err = writeRuntimeFiles(cfg, "fulliris-rabbit", creds); err != nil {
		return out, err
	}
	if err = d.iris(ctx, cfg, name); err != nil {
		return out, err
	}
	if err = d.startIris(ctx, name); err != nil {
		return out, err
	}
	if err = waitReady(ctx, cfg.RuntimeDir, cfg.Workers); err != nil {
		return out, err
	}
	if err = releaseGate(cfg); err != nil {
		return out, err
	}
	if err = collectResults(ctx, collector, cfg, msgs, started, confirmed, before, after, publishedAt, rdsCount, func() error {
		return d.requireRunning(ctx, name.workers)
	}, &out); err != nil {
		return out, err
	}
	cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err = cleanup(cleanCtx, d, name, cfg); err != nil {
		return out, err
	}
	cleaned = true
	if err = scanEvidence(cfg.RunDir, cfg.SecretEnvPath, creds); err != nil {
		return out, err
	}
	return out, nil
}

type containerNames struct {
	network, rabbit, gate string
	workers               []string
}

func names(c Config) containerNames {
	p := "iris-loadtest-" + c.RunID + "-" + c.BlockID
	n := containerNames{network: p + "-net", rabbit: p + "-rabbit", gate: p + "-gate"}
	for i := 0; i < c.Workers; i++ {
		n.workers = append(n.workers, fmt.Sprintf("%s-worker-%03d", p, i))
	}
	return n
}

type dockerAPI struct{ docker }

func newDocker() dockerAPI { return dockerAPI{cliDocker{}} }
func (d dockerAPI) network(ctx context.Context, action, name string) error {
	_, e := d.Run(ctx, "network", action, "--label", "com.skkuding.iris-load-test=true", name)
	return e
}
func (d dockerAPI) verifyJudger(ctx context.Context, c Config) error {
	out, err := d.Run(ctx, "run", "--rm", "--entrypoint", "/bin/sha256sum", c.IrisImage, "/app/sandbox/libjudger.so")
	if err != nil {
		return fmt.Errorf("fulliris: verify Iris Judger: %w", err)
	}
	want := strings.TrimPrefix(c.JudgerDigest, "sha256:")
	matched := false
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == want && fields[1] == "/app/sandbox/libjudger.so" {
			matched = true
			break
		}
	}
	if !matched {
		return errors.New("fulliris: Iris Judger digest does not match the sealed plan")
	}
	return nil
}
func (d dockerAPI) rabbit(ctx context.Context, c Config, n containerNames, token string) error {
	p := filepath.Join(c.RuntimeDir, "fulliris-rabbit-env-"+c.BlockID)
	if e := artifact.WriteFileAtomic(p, []byte("RABBITMQ_DEFAULT_USER=loadtest\nRABBITMQ_DEFAULT_PASS="+token+"\nRABBITMQ_DEFAULT_VHOST=loadtest\n"), 0600); e != nil {
		return e
	}
	_, e := d.Run(ctx, "create", "--name", n.rabbit, "--network", n.network, "--network-alias", "fulliris-rabbit", "--publish", "127.0.0.1::5672", "--label", "com.skkuding.iris-load-test=true", "--label", "com.skkuding.iris-load-test.run-id="+c.RunID, "--label", "com.skkuding.iris-load-test.block-id="+c.BlockID, "--label", "com.skkuding.iris-load-test.role=rabbit", "--env-file", p, c.RabbitMQImage)
	if e != nil {
		return e
	}
	_, e = d.Run(ctx, "start", n.rabbit)
	return e
}
func (d dockerAPI) iris(ctx context.Context, c Config, n containerNames) error {
	cpus := []string{}
	if c.CPUList != "" {
		cpus, _ = containment.SplitCPUList(c.CPUList, c.Workers)
	}
	for i, name := range n.workers {
		args := []string{"create", "--name", name, "--network", n.network, "--privileged", "--cgroupns", "host", "--cpus", "1", "--memory", "1610612736", "--label", "com.skkuding.iris-load-test=true", "--label", "com.skkuding.iris-load-test.run-id=" + c.RunID, "--label", "com.skkuding.iris-load-test.block-id=" + c.BlockID, "--label", "com.skkuding.iris-load-test.role=iris", "--env-file", filepath.Join(c.RuntimeDir, fmt.Sprintf("fulliris-env-%s-%03d", c.BlockID, i)), "--volume", c.CgroupMount + ":/sys/fs/cgroup:rw", "--volume", c.RuntimeDir + ":/fulliris-runtime:rw", "--entrypoint", "/bin/sh"}
		if len(cpus) > 0 {
			args = append(args, "--cpuset-cpus", cpus[i])
		}
		args = append(args, c.IrisImage, "/fulliris-runtime/fulliris-wrapper-"+c.BlockID)
		if _, e := d.Run(ctx, args...); e != nil {
			return e
		}
	}
	return nil
}
func (d dockerAPI) startIris(ctx context.Context, n containerNames) error {
	_, e := d.Run(ctx, append([]string{"start"}, n.workers...)...)
	return e
}
func (d dockerAPI) requireRunning(ctx context.Context, names []string) error {
	for _, name := range names {
		out, err := d.Run(ctx, "inspect", "--format", "{{.State.Running}}", name)
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(out)) != "true" {
			return fmt.Errorf("fulliris: Iris worker %s exited before result conservation completed", name)
		}
	}
	return nil
}
func writeRuntimeFiles(c Config, rabbitHost, token string) error {
	b, e := os.ReadFile(c.SecretEnvPath)
	if e != nil {
		return e
	}
	for i := 0; i < c.Workers; i++ {
		vars := fmt.Sprintf("\nAPP_ENV=production\nDATABASE_CONNECTION_LIMIT=1\nRABBITMQ_DEFAULT_USER=loadtest\nRABBITMQ_DEFAULT_PASS=%s\nRABBITMQ_DEFAULT_VHOST=loadtest\nRABBITMQ_HOST=%s\nRABBITMQ_PORT=5672\nRABBITMQ_SSL=false\nJUDGE_REQUEST_CONSUMER_CONNECTION_NAME=fulliris-%s-%s-worker-%03d\nJUDGE_REQUEST_QUEUE_NAME=%s\nJUDGE_REQUEST_CONSUMER_TAG=fulliris-%s-%s-request-%03d\nJUDGE_RESULT_PRODUCER_CONNECTION_NAME=fulliris-%s-%s-worker-%03d\nJUDGE_RESULT_EXCHANGE_NAME=%s\nJUDGE_RESULT_ROUTING_KEY=%s\nTESTCASE_BUCKET_NAME=%s\nDISABLE_INSTRUMENTATION=true\nFULLIRIS_WORKER=%d\n", token, rabbitHost, c.RunID, c.BlockID, i, requestQueue, c.RunID, c.BlockID, i, c.RunID, c.BlockID, i, exchange, resultKey, c.S3Bucket, i)
		p := filepath.Join(c.RuntimeDir, fmt.Sprintf("fulliris-env-%s-%03d", c.BlockID, i))
		if e = artifact.WriteFileAtomic(p, append(append([]byte(nil), b...), []byte(vars)...), 0600); e != nil {
			return e
		}
	}
	script := "#!/bin/sh\nset -eu\nmarker=/fulliris-runtime/fulliris-ready-${FULLIRIS_WORKER}\n: > ${marker}\nwhile [ ! -f /fulliris-runtime/fulliris-gate-" + c.BlockID + " ]; do sleep 0.01; done\nexec /entrypoint.sh\n"
	return artifact.WriteFileAtomic(filepath.Join(c.RuntimeDir, "fulliris-wrapper-"+c.BlockID), []byte(script), 0755)
}
func waitReady(ctx context.Context, dir string, n int) error {
	for {
		ok := true
		for i := 0; i < n; i++ {
			if _, e := os.Stat(filepath.Join(dir, fmt.Sprintf("fulliris-ready-%d", i))); e != nil {
				ok = false
				break
			}
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
func releaseGate(c Config) error {
	p := filepath.Join(c.RuntimeDir, "fulliris-gate-"+c.BlockID)
	tmp := p + ".tmp"
	if e := os.WriteFile(tmp, []byte("release\n"), 0600); e != nil {
		return e
	}
	return os.Rename(tmp, p)
}
func (d dockerAPI) port(ctx context.Context, n string) (string, error) {
	b, e := d.Run(ctx, "port", n, "5672/tcp")
	if e != nil {
		return "", e
	}
	s := strings.TrimSpace(string(b))
	_, p, e := net.SplitHostPort(s)
	if e != nil {
		return "", e
	}
	return p, nil
}
func cleanup(ctx context.Context, d dockerAPI, n containerNames, c Config) error {
	var es []error
	containerIDs := make([]string, len(n.workers))
	for i, x := range n.workers {
		if b, e := d.Run(ctx, "inspect", "--format", "{{.Id}}", x); e == nil {
			containerIDs[i] = strings.TrimSpace(string(b))
		} else if !notFound(e) {
			es = append(es, e)
		}
		if b, e := d.Run(ctx, "logs", "--tail", "10000", x); e == nil {
			if e := artifact.WriteFileAtomic(filepath.Join(c.RunDir, "logs", fmt.Sprintf("%s-%03d.log", c.BlockID, i)), sanitizeLog(b), 0644); e != nil {
				es = append(es, e)
			}
		} else if !notFound(e) {
			es = append(es, e)
		}
	}
	for _, x := range append(n.workers, n.rabbit, n.gate) {
		if _, e := d.Run(ctx, "rm", "--force", x); e != nil {
			if !notFound(e) {
				es = append(es, e)
			}
		}
	}
	if e := removeSandboxRoots(ctx, d, c, containerIDs); e != nil {
		es = append(es, e)
	}
	if _, e := d.Run(ctx, "network", "rm", n.network); e != nil && !notFound(e) {
		es = append(es, e)
	}
	for i := -1; i < c.Workers; i++ {
		p := filepath.Join(c.RuntimeDir, fmt.Sprintf("fulliris-env-%s-%03d", c.BlockID, i))
		if i < 0 {
			p = filepath.Join(c.RuntimeDir, "fulliris-rabbit-env-"+c.BlockID)
		}
		if e := os.Remove(p); e != nil && !os.IsNotExist(e) {
			es = append(es, e)
		}
	}
	for i := 0; i < c.Workers; i++ {
		for _, p := range []string{filepath.Join(c.RuntimeDir, fmt.Sprintf("fulliris-ready-%d", i))} {
			if e := os.Remove(p); e != nil && !os.IsNotExist(e) {
				es = append(es, e)
			}
		}
	}
	for _, p := range []string{filepath.Join(c.RuntimeDir, "fulliris-wrapper-"+c.BlockID), filepath.Join(c.RuntimeDir, "fulliris-gate-"+c.BlockID)} {
		if e := os.Remove(p); e != nil && !os.IsNotExist(e) {
			es = append(es, e)
		}
	}
	return errors.Join(es...)
}

// removeSandboxRoots validates every worker's run-scoped alpha.4 sandbox root,
// then removes all roots and their empty box children with a single privileged
// helper container. One helper per block keeps teardown fast at high replica
// counts; spawning one helper per worker made the shared cleanup budget expire
// before all roots were gone. Consecutive roots are independent, so
// concatenating each root's child-first, root-last path order remains safe.
func removeSandboxRoots(ctx context.Context, d dockerAPI, c Config, containerIDs []string) error {
	idPattern := regexp.MustCompile(`^[0-9a-f]{64}$`)
	mgr := &containment.Manager{Mount: c.CgroupMount}
	var roots, allPaths []string
	for _, id := range containerIDs {
		if id == "" {
			continue
		}
		if !idPattern.MatchString(id) {
			return fmt.Errorf("fulliris: unsafe Iris container id %q", id)
		}
		root := "/sandbox-" + id
		paths, err := mgr.ValidateSandboxRemovalPaths(root)
		if err != nil {
			return err
		}
		if len(paths) == 0 {
			continue
		}
		roots = append(roots, root)
		allPaths = append(allPaths, paths...)
	}
	if len(allPaths) == 0 {
		return nil
	}
	helper := "iris-loadtest-" + c.RunID + "-" + c.BlockID + "-cgroup-cleanup"
	args := []string{"run", "--rm", "--name", helper, "--label", "com.skkuding.iris-load-test=true", "--label", "com.skkuding.iris-load-test.run-id=" + c.RunID, "--label", "com.skkuding.iris-load-test.block-id=" + c.BlockID, "--label", "com.skkuding.iris-load-test.role=cleanup", "--privileged", "--cgroupns", "host", "--entrypoint", "/bin/rmdir", "--volume", c.CgroupMount + ":" + c.CgroupMount + ":rw", c.IrisImage}
	args = append(args, allPaths...)
	if _, err := d.Run(ctx, args...); err != nil {
		return fmt.Errorf("fulliris: remove %d sandbox root(s): %w", len(roots), err)
	}
	for _, root := range roots {
		paths, err := mgr.ValidateSandboxRemovalPaths(root)
		if err != nil {
			return err
		}
		if len(paths) != 0 {
			return fmt.Errorf("fulliris: sandbox root %s remains after cleanup", root)
		}
	}
	return nil
}
func notFound(e error) bool {
	s := strings.ToLower(e.Error())
	return strings.Contains(s, "no such container") || strings.Contains(s, "no such object") || strings.Contains(s, "not found")
}
func sanitizeLog(b []byte) []byte {
	if len(b) > 1<<20 {
		b = b[:1<<20]
	}
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c == '\n' || c == '\r' || c == '\t' || c >= 32 && c < 127 {
			out = append(out, c)
		} else {
			out = append(out, '?')
		}
	}
	return out
}

func randomToken() (string, error) {
	b := make([]byte, 24)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return hex.EncodeToString(b), nil
}
func dialRabbit(ctx context.Context, port, token string) (*amqp091.Connection, error) {
	var c *amqp091.Connection
	var e error
	end := time.Now().Add(30 * time.Second)
	for time.Now().Before(end) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		c, e = amqp091.Dial("amqp://loadtest:" + token + "@127.0.0.1:" + port + "/loadtest")
		if e == nil {
			return c, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, e
}
func topology(c *amqp091.Connection) error {
	ch, e := c.Channel()
	if e != nil {
		return e
	}
	defer ch.Close()
	if e = ch.ExchangeDeclare(exchange, "direct", true, false, false, false, nil); e != nil {
		return e
	}
	for _, q := range []string{requestQueue, resultQueue} {
		if _, e = ch.QueueDeclare(q, true, false, false, false, nil); e != nil {
			return e
		}
		if _, e = ch.QueuePurge(q, false); e != nil {
			return e
		}
	}
	if e = ch.QueueBind(requestQueue, requestKey, exchange, false, nil); e != nil {
		return e
	}
	if e = ch.QueueBind(resultQueue, resultKey, exchange, false, nil); e != nil {
		return e
	}
	for _, q := range []string{requestQueue, resultQueue} {
		state, e := ch.QueueInspect(q)
		if e != nil {
			return e
		}
		if state.Messages != 0 {
			return fmt.Errorf("fulliris: queue %q was not empty after purge", q)
		}
	}
	return nil
}

type request struct {
	Code        string `json:"code"`
	Language    string `json:"language"`
	ProblemID   int    `json:"problemId"`
	TimeLimit   int    `json:"timeLimit"`
	MemoryLimit int64  `json:"memoryLimit"`
}

func makeRequest(c Config) ([]byte, error) {
	b, e := os.ReadFile(c.SourcePath)
	if e != nil {
		return nil, e
	}
	return json.Marshal(request{string(b), c.Language, c.ProblemID, c.TimeLimitMS, c.MemoryLimitBytes})
}

type collector struct {
	ch         *amqp091.Channel
	deliveries <-chan amqp091.Delivery
}

func startCollector(conn *amqp091.Connection, c Config) (collector, error) {
	ch, e := conn.Channel()
	if e != nil {
		return collector{}, e
	}
	if e = ch.Qos(c.Workers*c.Repetitions*(c.TestcasesPerRequest+1), 0, false); e != nil {
		ch.Close()
		return collector{}, e
	}
	d, e := ch.Consume(resultQueue, "fulliris-"+c.RunID+"-"+c.BlockID, false, false, false, false, nil)
	if e != nil {
		ch.Close()
		return collector{}, e
	}
	return collector{ch, d}, nil
}
func publishRequests(ctx context.Context, conn *amqp091.Connection, c Config, total int) (confirmed, before, after int, publishedAt map[string]int64, err error) {
	ch, e := conn.Channel()
	if e != nil {
		return 0, 0, 0, nil, e
	}
	defer ch.Close()
	if e = ch.Confirm(false); e != nil {
		return 0, 0, 0, nil, e
	}
	q, e := ch.QueueInspect(requestQueue)
	if e != nil {
		return 0, 0, 0, nil, e
	}
	before = q.Messages
	publishedAt = make(map[string]int64, total)
	conf := ch.NotifyPublish(make(chan amqp091.Confirmation, total))
	ret := ch.NotifyReturn(make(chan amqp091.Return, total))
	body, e := makeRequest(c)
	if e != nil {
		return 0, 0, 0, nil, e
	}
	for i := 0; i < total; i++ {
		id := strconv.FormatInt(c.MessageIDStart+int64(i), 10)
		publishedAt[id] = time.Now().UnixNano()
		if e = ch.PublishWithContext(ctx, exchange, requestKey, true, false, amqp091.Publishing{MessageId: id, Type: "judge", DeliveryMode: 2, Timestamp: time.Now(), Body: body}); e != nil {
			return confirmed, before, after, publishedAt, e
		}
	}
	for confirmed < total {
		select {
		case x := <-conf:
			if !x.Ack {
				return confirmed, before, after, publishedAt, errors.New("fulliris: publisher nack")
			}
			confirmed++
		case r := <-ret:
			return confirmed, before, after, publishedAt, fmt.Errorf("fulliris: returned message %q", r.RoutingKey)
		case <-ctx.Done():
			return confirmed, before, after, publishedAt, ctx.Err()
		}
	}
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case r := <-ret:
			return confirmed, before, after, publishedAt, fmt.Errorf("fulliris: returned message %q", r.RoutingKey)
		case <-timer.C:
			q, e = ch.QueueInspect(requestQueue)
			if e != nil {
				return confirmed, before, after, publishedAt, e
			}
			return confirmed, before, q.Messages, publishedAt, nil
		case <-ctx.Done():
			return confirmed, before, after, publishedAt, ctx.Err()
		}
	}
}

type parsed struct {
	submission, testcase, resultCode, errorCode, errorText string
	cpu, real, memory                                      int64
	terminal                                               bool
}

func parseActual(typ string, body []byte) (parsed, error) {
	var raw struct {
		SubmissionID json.RawMessage `json:"submissionId"`
		ResultCode   json.RawMessage `json:"resultCode"`
		JudgeResult  struct {
			TestcaseID json.RawMessage `json:"testcaseId"`
			CPU        json.RawMessage `json:"cpuTime"`
			Real       json.RawMessage `json:"realTime"`
			Memory     json.RawMessage `json:"memory"`
			ErrorCode  json.RawMessage `json:"errorCode"`
			Error      json.RawMessage `json:"error"`
		} `json:"judgeResult"`
		JudgeResults []json.RawMessage `json:"judgeResults"`
	}
	if e := json.Unmarshal(body, &raw); e != nil {
		return parsed{}, e
	}
	var p parsed
	if len(raw.SubmissionID) == 0 {
		return p, errors.New("missing submissionId")
	}
	p.submission = scalarString(raw.SubmissionID)
	if p.submission == "" {
		return p, errors.New("invalid submissionId")
	}
	if typ == "judge" {
		if len(raw.JudgeResult.TestcaseID) == 0 {
			return p, errors.New("missing judgeResult")
		}
		p.testcase = scalarString(raw.JudgeResult.TestcaseID)
		if p.testcase == "" {
			return p, errors.New("invalid testcaseId")
		}
		p.resultCode = scalarString(raw.ResultCode)
		_ = json.Unmarshal(raw.JudgeResult.CPU, &p.cpu)
		_ = json.Unmarshal(raw.JudgeResult.Real, &p.real)
		_ = json.Unmarshal(raw.JudgeResult.Memory, &p.memory)
		p.errorCode = scalarString(raw.JudgeResult.ErrorCode)
		p.errorText = scalarString(raw.JudgeResult.Error)
		return p, nil
	}
	if typ != "submission" || len(raw.JudgeResults) == 0 {
		return p, errors.New("invalid terminal submission")
	}
	p.terminal = true
	return p, nil
}
func scalarString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}
func collectResults(ctx context.Context, col collector, c Config, total int, started time.Time, confirmed, before, after int, publishedAt map[string]int64, rdsCount int, health func() error, out *Result) error {
	defer col.ch.Close()
	want := map[string]bool{}
	for i := 0; i < total; i++ {
		want[strconv.FormatInt(c.MessageIDStart+int64(i), 10)] = true
	}
	seen := map[string]bool{}
	samples := []sample{}
	dup, unexpected, redel, term, test := 0, 0, 0, 0, 0
	deadline := time.NewTimer(c.Timeout)
	defer deadline.Stop()
	healthTicker := time.NewTicker(time.Second)
	defer healthTicker.Stop()
	for term < total || test < total*c.TestcasesPerRequest {
		select {
		case d, ok := <-col.deliveries:
			if !ok {
				return errors.New("fulliris: result delivery channel closed")
			}
			p, e := parseActual(d.Type, d.Body)
			if e != nil {
				unexpected++
				_ = d.Ack(false)
				continue
			}
			if !want[p.submission] {
				unexpected++
				_ = d.Ack(false)
				continue
			}
			key := d.Type + "/" + p.submission + "/" + p.testcase
			if seen[key] {
				dup++
				_ = d.Ack(false)
				continue
			}
			seen[key] = true
			if d.Redelivered {
				redel++
			}
			if p.terminal {
				term++
			} else {
				test++
				status := "success"
				if p.resultCode != "0" || p.errorCode != "0" {
					status = "failed"
				}
				received := time.Now().UnixNano()
				published := publishedAt[p.submission]
				samples = append(samples, sample{version, c.RunID, c.BlockID, "unknown", p.submission, 1, status, p.cpu, p.real, p.memory, published, received, (received - published) / int64(time.Millisecond), p.resultCode, p.errorCode, p.testcase, d.Redelivered})
			}
			_ = d.Ack(false)
		case <-deadline.C:
			return errors.New("fulliris: result collection timeout")
		case <-healthTicker.C:
			if err := health(); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if dup != 0 || unexpected != 0 || redel != 0 || test != total*c.TestcasesPerRequest || term != total {
		return errors.New("fulliris: result conservation failure")
	}
	data := bytesNDJSON(samples)
	sum := sha256.Sum256(data)
	sh := hex.EncodeToString(sum[:])
	if e := artifact.WriteFileAtomic(filepath.Join(c.RunDir, "samples", c.BlockID+".ndjson"), data, 0644); e != nil {
		return e
	}
	resultState, e := col.ch.QueueInspect(resultQueue)
	if e != nil {
		return e
	}
	requestState, e := col.ch.QueueInspect(requestQueue)
	if e != nil {
		return e
	}
	finish := time.Now().UTC()
	r := Receipt{SchemaVersion: version, RunID: c.RunID, BlockID: c.BlockID, Status: "passed", ExpectedRequests: total, PublishedRequests: total, ConfirmedRequests: confirmed, ExpectedTestcaseDeliveries: total * c.TestcasesPerRequest, ObservedTestcaseDeliveries: test, ExpectedTerminalDeliveries: total, ObservedTerminalDeliveries: term, DuplicateCount: dup, RedeliveredCount: redel, UnexpectedCount: unexpected, MissingCount: total*c.TestcasesPerRequest - test + total - term, QueueDepthBeforePublish: before, QueueDepthAfterPublish: after, QueueDepthAfterCompletion: requestState.Messages + resultState.Messages, RequestQueueDepthAfterCompletion: requestState.Messages, ResultQueueDepthAfterCompletion: resultState.Messages, RDSActiveTestcases: rdsCount, SampleCount: len(samples), SampleSHA256: sh, IrisImage: c.IrisImage, RabbitMQImage: c.RabbitMQImage, StartedAtUnixNano: started.UnixNano(), FinishedAtUnixNano: finish.UnixNano(), DurationMS: finish.Sub(started).Milliseconds()}
	raw, _ := json.MarshalIndent(r, "", "  ")
	raw = append(raw, '\n')
	if e = artifact.WriteFileAtomic(filepath.Join(c.RunDir, "receipts", c.BlockID+".json"), raw, 0644); e != nil {
		return e
	}
	h := sha256.Sum256(raw)
	out.Receipt = r
	out.ReceiptSHA256 = hex.EncodeToString(h[:])
	if requestState.Messages != 0 || resultState.Messages != 0 {
		return errors.New("fulliris: request or result queue not empty")
	}
	return nil
}

func bytesNDJSON(v []sample) []byte {
	var b strings.Builder
	for _, x := range v {
		d, _ := json.Marshal(x)
		b.Write(d)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// Validate reparses both artifacts and checks their cryptographic and conservation invariants.
func Validate(runDir string, cfg Config) error { _, e := ValidateReceipt(runDir, cfg); return e }
func ValidateReceipt(runDir string, cfg Config) (Result, error) {
	var r Receipt
	raw, e := os.ReadFile(filepath.Join(runDir, "receipts", cfg.BlockID+".json"))
	if e != nil {
		return Result{}, e
	}
	if e = json.Unmarshal(raw, &r); e != nil {
		return Result{}, e
	}
	if e = validateConfig(runDir, cfg); e != nil {
		return Result{}, e
	}
	sd, e := os.ReadFile(filepath.Join(runDir, "samples", cfg.BlockID+".ndjson"))
	if e != nil {
		return Result{}, e
	}
	h := sha256.Sum256(sd)
	if hex.EncodeToString(h[:]) != r.SampleSHA256 {
		return Result{}, errors.New("fulliris: sample hash mismatch")
	}
	count := 0
	sc := bufio.NewScanner(strings.NewReader(string(sd)))
	for sc.Scan() {
		var s sample
		if e = json.Unmarshal(sc.Bytes(), &s); e != nil {
			return Result{}, e
		}
		if s.SchemaVersion != version || s.RunID != cfg.RunID || s.BlockID != cfg.BlockID || s.Status != "success" || s.RequestID == "" {
			return Result{}, errors.New("fulliris: invalid sample record")
		}
		count++
	}
	if e = sc.Err(); e != nil {
		return Result{}, e
	}
	total := cfg.Workers * cfg.Repetitions
	if r.SchemaVersion != version || r.RunID != cfg.RunID || r.BlockID != cfg.BlockID || r.IrisImage != cfg.IrisImage || r.RabbitMQImage != cfg.RabbitMQImage || r.Status != "passed" || r.ExpectedRequests != total || r.PublishedRequests != total || r.ConfirmedRequests != total || r.ExpectedTestcaseDeliveries != total*cfg.TestcasesPerRequest || r.ObservedTestcaseDeliveries != r.ExpectedTestcaseDeliveries || r.ExpectedTerminalDeliveries != total || r.ObservedTerminalDeliveries != total || r.DuplicateCount != 0 || r.RedeliveredCount != 0 || r.UnexpectedCount != 0 || r.MissingCount != 0 || r.QueueDepthBeforePublish != 0 || r.QueueDepthAfterPublish != total || r.QueueDepthAfterCompletion != 0 || r.RequestQueueDepthAfterCompletion != 0 || r.ResultQueueDepthAfterCompletion != 0 || r.RDSActiveTestcases <= 0 || count != r.SampleCount {
		return Result{}, errors.New("fulliris: receipt conservation failure")
	}
	x := sha256.Sum256(raw)
	return Result{r, hex.EncodeToString(x[:])}, nil
}

func probeRDS(ctx context.Context, envPath string, problemID int) (int, error) {
	data, err := os.ReadFile(envPath)
	if err != nil {
		return 0, err
	}
	databaseURL := ""
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok && key == "DATABASE_URL" {
			databaseURL = value
			break
		}
	}
	if databaseURL == "" {
		return 0, errors.New("fulliris: secret environment has no DATABASE_URL")
	}
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return 0, fmt.Errorf("fulliris: open benchmark RDS: %w", err)
	}
	defer db.Close()
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := db.PingContext(probeCtx); err != nil {
		return 0, fmt.Errorf("fulliris: ping benchmark RDS: %w", err)
	}
	var count int
	if err := db.QueryRowContext(probeCtx, `SELECT count(*) FROM public.problem_testcase WHERE problem_id = $1 AND is_outdated = false`, problemID).Scan(&count); err != nil {
		return 0, fmt.Errorf("fulliris: fingerprint benchmark RDS problem %d: %w", problemID, err)
	}
	if count <= 0 {
		return 0, fmt.Errorf("fulliris: benchmark RDS problem %d has no active testcase", problemID)
	}
	return count, nil
}

func scanEvidence(runDir, secretEnvPath, generatedSecret string) error {
	data, err := os.ReadFile(secretEnvPath)
	if err != nil {
		return err
	}
	secrets := []string{generatedSecret}
	for _, line := range strings.Split(string(data), "\n") {
		_, value, ok := strings.Cut(line, "=")
		if ok && len(value) >= 4 {
			secrets = append(secrets, value)
		}
	}
	inv, err := artifact.BuildInventory(runDir)
	if err != nil {
		return err
	}
	return artifact.ScanForSecrets(runDir, inv, secrets)
}
