package capture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/google/uuid"
)

var (
	errManagedProcessConfiguration = errors.New("capture process configuration is invalid")
	errManagedProcessPipes         = errors.New("capture process pipe setup failed")
	errManagedProcessIsolation     = errors.New("capture process isolation failed")
	errManagedProcessStart         = errors.New("capture process start failed")
	errManagedProcessResume        = errors.New("capture process resume failed")
	errManagedProcessControl       = errors.New("capture process control failed")
	errManagedProcessWait          = errors.New("capture process wait failed")
	errManagedProcessCleanup       = errors.New("capture process cleanup failed")
)

const managedProcessTerminateExitCode uint32 = 1

const (
	recorderAttemptJobNamePrefix = `Global\DouyinLive.Recorder.v1.`
	recorderJobNamespaceBytes    = 16
)

type RecorderProcessRecoveryStatus string

const (
	RecorderProcessRecoveryClean      RecorderProcessRecoveryStatus = "clean"
	RecorderProcessRecoveryTerminated RecorderProcessRecoveryStatus = "terminated"
	RecorderProcessRecoveryFailed     RecorderProcessRecoveryStatus = "failed"

	RecorderProcessRecoveryContextErrorCode        = "PROCESS_RECOVERY_CONTEXT_INVALID"
	RecorderProcessRecoveryInterruptedErrorCode    = "PROCESS_RECOVERY_INTERRUPTED"
	RecorderProcessRecoveryInvalidAttemptErrorCode = "PROCESS_RECOVERY_ATTEMPT_INVALID"
	RecorderProcessRecoveryOpenErrorCode           = "PROCESS_RECOVERY_OPEN_FAILED"
	RecorderProcessRecoveryQueryErrorCode          = "PROCESS_RECOVERY_QUERY_FAILED"
	RecorderProcessRecoveryTerminateErrorCode      = "PROCESS_RECOVERY_TERMINATE_FAILED"
	RecorderProcessRecoveryIncompleteErrorCode     = "PROCESS_RECOVERY_TERMINATION_INCOMPLETE"
	RecorderProcessRecoveryCloseErrorCode          = "PROCESS_RECOVERY_CLOSE_FAILED"
)

var (
	errRecorderProcessRecoveryContext        = errors.New(RecorderProcessRecoveryContextErrorCode)
	errRecorderProcessRecoveryInterrupted    = errors.New(RecorderProcessRecoveryInterruptedErrorCode)
	errRecorderProcessRecoveryInvalidAttempt = errors.New(RecorderProcessRecoveryInvalidAttemptErrorCode)
	errRecorderProcessRecoveryOpen           = errors.New(RecorderProcessRecoveryOpenErrorCode)
	errRecorderProcessRecoveryQuery          = errors.New(RecorderProcessRecoveryQueryErrorCode)
	errRecorderProcessRecoveryTerminate      = errors.New(RecorderProcessRecoveryTerminateErrorCode)
	errRecorderProcessRecoveryIncomplete     = errors.New(RecorderProcessRecoveryIncompleteErrorCode)
	errRecorderProcessRecoveryClose          = errors.New(RecorderProcessRecoveryCloseErrorCode)
)

// RecorderProcessRecoveryResult deliberately carries no process ID,
// command line, filesystem path, stream URL, or native error text.
type RecorderProcessRecoveryResult struct {
	Found      bool                          `json:"found"`
	Terminated bool                          `json:"terminated"`
	Status     RecorderProcessRecoveryStatus `json:"status"`
	ErrorCode  string                        `json:"errorCode,omitempty"`
}

// inspectRecorderAttemptProcess checks only the Job Object derived from the
// supplied UUIDv7 attempt ID. It never enumerates processes or matches a PID.
func inspectRecorderAttemptProcess(
	ctx context.Context,
	jobNamespace string,
	attemptID string,
) (RecorderProcessRecoveryResult, error) {
	if ctx == nil {
		return recorderProcessRecoveryFailure(false, false, RecorderProcessRecoveryContextErrorCode, errRecorderProcessRecoveryContext)
	}
	if ctx.Err() != nil {
		return recorderProcessRecoveryFailure(false, false, RecorderProcessRecoveryInterruptedErrorCode, errRecorderProcessRecoveryInterrupted)
	}
	jobName, valid := recorderAttemptJobName(jobNamespace, attemptID)
	if !valid {
		return recorderProcessRecoveryFailure(false, false, RecorderProcessRecoveryInvalidAttemptErrorCode, errRecorderProcessRecoveryInvalidAttempt)
	}
	return recoverPlatformRecorderAttemptProcess(ctx, jobName)
}

func recorderJobNamespace(root string) (string, bool) {
	canonicalRoot, err := canonicalMediaRoot(root)
	if err != nil {
		return "", false
	}
	canonicalRoot = filepath.ToSlash(filepath.Clean(canonicalRoot))
	if runtime.GOOS == "windows" {
		canonicalRoot = strings.ToLower(canonicalRoot)
	}
	digest := sha256.Sum256([]byte("douyinlive-recorder-job-v1\x00" + canonicalRoot))
	return hex.EncodeToString(digest[:recorderJobNamespaceBytes]), true
}

func validRecorderJobNamespace(value string) bool {
	if value == "" || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == recorderJobNamespaceBytes
}

func recorderAttemptJobName(jobNamespace, attemptID string) (string, bool) {
	if !validRecorderJobNamespace(jobNamespace) {
		return "", false
	}
	parsed, err := uuid.Parse(attemptID)
	if err != nil || parsed.Version() != 7 {
		return "", false
	}
	return recorderAttemptJobNamePrefix + jobNamespace + "." + parsed.String(), true
}

func recorderProcessRecoveryFailure(
	found bool,
	terminated bool,
	errorCode string,
	stableError error,
) (RecorderProcessRecoveryResult, error) {
	return RecorderProcessRecoveryResult{
		Found: found, Terminated: terminated,
		Status: RecorderProcessRecoveryFailed, ErrorCode: errorCode,
	}, stableError
}

type processConfig struct {
	Path                 string
	Args                 []string
	Dir                  string
	Env                  []string
	RecorderJobNamespace string
	RecorderAttemptID    string
}

func (c processConfig) String() string {
	return fmt.Sprintf("processConfig{args:%d env:%d}", len(c.Args), len(c.Env))
}

func (c processConfig) GoString() string {
	return c.String()
}

func (c processConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Args int `json:"args_count"`
		Env  int `json:"env_count"`
	}{
		Args: len(c.Args),
		Env:  len(c.Env),
	})
}

func (c processConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int("args_count", len(c.Args)),
		slog.Int("env_count", len(c.Env)),
	)
}

type processStreams struct {
	Stdout io.ReadCloser
	Stderr io.ReadCloser
}

type trackedProcessReader struct {
	reader  io.ReadCloser
	drained chan struct{}

	drainOnce sync.Once
	closeOnce sync.Once
	closeErr  error
}

func newTrackedProcessReader(reader io.ReadCloser) *trackedProcessReader {
	return &trackedProcessReader{reader: reader, drained: make(chan struct{})}
}

func (r *trackedProcessReader) Read(buffer []byte) (int, error) {
	read, err := r.reader.Read(buffer)
	if err != nil {
		r.markDrained()
	}
	if errors.Is(err, io.EOF) {
		return read, err
	}
	if err != nil {
		return read, errManagedProcessPipes
	}
	return read, nil
}

func (r *trackedProcessReader) Close() error {
	r.closeOnce.Do(func() {
		err := r.reader.Close()
		if err == nil || errors.Is(err, os.ErrClosed) {
			r.closeErr = nil
		} else {
			r.closeErr = errManagedProcessCleanup
		}
	})
	r.markDrained()
	return r.closeErr
}

func (r *trackedProcessReader) waitDrained() {
	<-r.drained
}

func (r *trackedProcessReader) markDrained() {
	r.drainOnce.Do(func() {
		close(r.drained)
	})
}

// processCommand is deliberately narrower than exec.Cmd. Keeping process and
// job operations behind this interface makes every fail-closed branch
// deterministic to test without starting a real recorder.
type processCommand interface {
	stdinPipe() (io.WriteCloser, error)
	stdoutPipe() (io.ReadCloser, error)
	stderrPipe() (io.ReadCloser, error)
	configure() error
	start() error
	resume() error
	wait() error
	kill() error
	withHandle(func(uintptr) error) error
}

type processJob interface {
	assign(processCommand) error
	terminate(uint32) error
	close() error
}

type processDependencies struct {
	newCommand func(context.Context, processConfig) processCommand
	newJob     func(string, string) (processJob, error)
}

type execProcessCommand struct {
	command *exec.Cmd
}

func (c *execProcessCommand) stdinPipe() (io.WriteCloser, error) {
	return c.command.StdinPipe()
}

func (c *execProcessCommand) stdoutPipe() (io.ReadCloser, error) {
	return c.command.StdoutPipe()
}

func (c *execProcessCommand) stderrPipe() (io.ReadCloser, error) {
	return c.command.StderrPipe()
}

func (c *execProcessCommand) start() error {
	return c.command.Start()
}

func (c *execProcessCommand) wait() error {
	return c.command.Wait()
}

func (c *execProcessCommand) kill() error {
	if c.command.Process == nil {
		return os.ErrProcessDone
	}
	return c.command.Process.Kill()
}

func (c *execProcessCommand) withHandle(call func(uintptr) error) error {
	if c.command.Process == nil {
		return os.ErrProcessDone
	}
	var callErr error
	handleErr := c.command.Process.WithHandle(func(handle uintptr) {
		callErr = call(handle)
	})
	return errors.Join(handleErr, callErr)
}

func defaultProcessDependencies() processDependencies {
	return processDependencies{
		newCommand: func(lifecycleCtx context.Context, config processConfig) processCommand {
			command := exec.CommandContext(lifecycleCtx, config.Path, config.Args...)
			command.Dir = config.Dir
			if config.Env != nil {
				command.Env = append([]string(nil), config.Env...)
			}
			return &execProcessCommand{command: command}
		},
		newJob: newPlatformProcessJob,
	}
}

type managedProcess struct {
	command         processCommand
	job             processJob
	stdin           io.WriteCloser
	stdout          *trackedProcessReader
	stderr          *trackedProcessReader
	lifecycleCancel context.CancelFunc
	doneCh          chan struct{}

	resultMu sync.RWMutex
	result   error

	stdinMu     sync.Mutex
	stdinClosed bool

	terminateProcessOnce sync.Once
	terminateProcessErr  error
	terminateTreeOnce    sync.Once
	terminateTreeErr     error
	closeOnce            sync.Once
	closeErr             error
}

// startManagedProcess uses callerCtx only as a pre-start guard. The command is
// intentionally attached to a private background lifecycle context so a
// factory or monitor timeout cannot kill a successfully started recorder.
func startManagedProcess(callerCtx context.Context, config processConfig) (*managedProcess, processStreams, error) {
	return startManagedProcessWithDependencies(callerCtx, config, defaultProcessDependencies())
}

func startManagedProcessWithDependencies(callerCtx context.Context, config processConfig, dependencies processDependencies) (*managedProcess, processStreams, error) {
	if callerCtx == nil || config.Path == "" || dependencies.newCommand == nil || dependencies.newJob == nil {
		return nil, processStreams{}, errManagedProcessConfiguration
	}
	if config.RecorderAttemptID == "" {
		if config.RecorderJobNamespace != "" {
			return nil, processStreams{}, errManagedProcessConfiguration
		}
	} else {
		if _, valid := recorderAttemptJobName(
			config.RecorderJobNamespace, config.RecorderAttemptID,
		); !valid {
			return nil, processStreams{}, errManagedProcessConfiguration
		}
	}
	if err := callerCtx.Err(); err != nil {
		return nil, processStreams{}, err
	}

	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	command := dependencies.newCommand(lifecycleCtx, processConfig{
		Path: config.Path,
		Args: append([]string(nil), config.Args...),
		Dir:  config.Dir,
		Env:  appendOptionalStrings(config.Env),
	})
	if command == nil {
		lifecycleCancel()
		return nil, processStreams{}, errManagedProcessConfiguration
	}

	stdin, err := command.stdinPipe()
	if err != nil || stdin == nil {
		lifecycleCancel()
		return nil, processStreams{}, errManagedProcessPipes
	}
	stdout, err := command.stdoutPipe()
	if err != nil || stdout == nil {
		cleanupErr := closeProcessPipes(stdin)
		lifecycleCancel()
		return nil, processStreams{}, errors.Join(errManagedProcessPipes, cleanupErr)
	}
	stderr, err := command.stderrPipe()
	if err != nil || stderr == nil {
		cleanupErr := closeProcessPipes(stdin, stdout)
		lifecycleCancel()
		return nil, processStreams{}, errors.Join(errManagedProcessPipes, cleanupErr)
	}
	trackedStdout := newTrackedProcessReader(stdout)
	trackedStderr := newTrackedProcessReader(stderr)

	job, err := dependencies.newJob(
		config.RecorderJobNamespace, config.RecorderAttemptID,
	)
	if err != nil || job == nil {
		cleanupErr := closeProcessPipes(stdin, trackedStdout, trackedStderr)
		lifecycleCancel()
		var jobErr error = errManagedProcessIsolation
		if errors.Is(err, errManagedProcessCleanup) {
			jobErr = errors.Join(jobErr, errManagedProcessCleanup)
		}
		return nil, processStreams{}, errors.Join(jobErr, cleanupErr)
	}
	if err := callerCtx.Err(); err != nil {
		cleanupErr := closeProcessStartup(job, lifecycleCancel, stdin, trackedStdout, trackedStderr)
		return nil, processStreams{}, errors.Join(err, cleanupErr)
	}
	if err := command.configure(); err != nil {
		cleanupErr := closeProcessStartup(job, lifecycleCancel, stdin, trackedStdout, trackedStderr)
		return nil, processStreams{}, errors.Join(errManagedProcessIsolation, cleanupErr)
	}
	if err := command.start(); err != nil {
		cleanupErr := closeProcessStartup(job, lifecycleCancel, stdin, trackedStdout, trackedStderr)
		return nil, processStreams{}, errors.Join(errManagedProcessStart, cleanupErr)
	}
	if err := job.assign(command); err != nil {
		// No wait goroutine exists yet, so this branch is the sole Wait owner.
		return nil, processStreams{}, failClosedStartedProcess(
			errManagedProcessIsolation, false, command, job, lifecycleCancel, stdin, trackedStdout, trackedStderr,
		)
	}
	if err := command.resume(); err != nil {
		return nil, processStreams{}, failClosedStartedProcess(
			errManagedProcessResume, true, command, job, lifecycleCancel, stdin, trackedStdout, trackedStderr,
		)
	}

	process := &managedProcess{
		command: command, job: job, stdin: stdin,
		lifecycleCancel: lifecycleCancel,
		doneCh:          make(chan struct{}),
		stdout:          trackedStdout,
		stderr:          trackedStderr,
	}
	go process.ownWait()
	return process, processStreams{Stdout: trackedStdout, Stderr: trackedStderr}, nil
}

func appendOptionalStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string(nil), values...)
}

func failClosedStartedProcess(
	primary error,
	assigned bool,
	command processCommand,
	job processJob,
	cancel context.CancelFunc,
	stdin io.Closer,
	stdout io.Closer,
	stderr io.Closer,
) error {
	// No wait goroutine exists on this path, so this function is the sole Wait
	// owner. Explicit Kill comes first; lifecycle cancellation backs it up.
	killErr := maskedProcessError(command.kill(), errManagedProcessControl)
	cancel()
	outputCloseErr := closeProcessPipes(stdout, stderr)
	var jobCloseErr error
	if assigned {
		// Once Assign succeeded, KILL_ON_JOB_CLOSE is the final fail-closed
		// guarantee even if both direct Kill and lifecycle cancellation fail.
		jobCloseErr = maskedProcessError(job.close(), errManagedProcessCleanup)
	}
	waitErr := maskedProcessError(command.wait(), errManagedProcessWait)
	if !assigned {
		jobCloseErr = maskedProcessError(job.close(), errManagedProcessCleanup)
	}
	stdinCloseErr := closeProcessPipes(stdin)
	return errors.Join(
		primary, killErr, outputCloseErr, waitErr, jobCloseErr, stdinCloseErr,
	)
}

func closeProcessStartup(job processJob, cancel context.CancelFunc, pipes ...io.Closer) error {
	jobErr := maskedProcessError(job.close(), errManagedProcessCleanup)
	cancel()
	return errors.Join(jobErr, closeProcessPipes(pipes...))
}

func closeProcessPipes(pipes ...io.Closer) error {
	var failed bool
	for _, pipe := range pipes {
		if pipe == nil {
			continue
		}
		err := pipe.Close()
		if err != nil && !errors.Is(err, os.ErrClosed) {
			failed = true
		}
	}
	if failed {
		return errManagedProcessCleanup
	}
	return nil
}

func maskedProcessError(err, stable error) error {
	if err == nil || errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return stable
}

func (p *managedProcess) ownWait() {
	p.stdout.waitDrained()
	p.stderr.waitDrained()
	waitErr := maskedProcessError(p.command.wait(), errManagedProcessWait)
	stdinErr := p.closeStdin()
	cleanupErr := p.close()

	p.resultMu.Lock()
	p.result = errors.Join(waitErr, stdinErr, cleanupErr)
	p.resultMu.Unlock()
	close(p.doneCh)
}

func (p *managedProcess) writeQuit() error {
	if p == nil {
		return errManagedProcessControl
	}
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	if p.stdinClosed || p.stdin == nil {
		return errManagedProcessControl
	}
	written, err := p.stdin.Write([]byte("q\n"))
	if err != nil || written != 2 {
		return errManagedProcessControl
	}
	return nil
}

func (p *managedProcess) terminateProcess() error {
	if p == nil {
		return errManagedProcessControl
	}
	if p.hasExited() {
		return nil
	}
	p.terminateProcessOnce.Do(func() {
		p.terminateProcessErr = maskedProcessError(p.command.kill(), errManagedProcessControl)
	})
	return p.terminateProcessErr
}

func (p *managedProcess) terminateTree() error {
	if p == nil {
		return errManagedProcessControl
	}
	if p.hasExited() {
		return nil
	}
	p.terminateTreeOnce.Do(func() {
		p.terminateTreeErr = maskedProcessError(p.job.terminate(managedProcessTerminateExitCode), errManagedProcessControl)
	})
	return p.terminateTreeErr
}

// wait observes the sole Wait owner. Cancelling ctx only stops this observer;
// it never cancels the private process lifecycle context.
func (p *managedProcess) wait(ctx context.Context) error {
	if p == nil || ctx == nil {
		return errManagedProcessConfiguration
	}
	select {
	case <-p.doneCh:
		return p.waitResult()
	default:
	}
	select {
	case <-p.doneCh:
		return p.waitResult()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *managedProcess) done() <-chan struct{} {
	if p == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return p.doneCh
}

// close is the shared idempotent last-resort cleanup path. Closing the Windows
// Job kills the tree first; lifecycle cancellation only backs that up. stdin
// is closed by ownWait after the process has actually exited, never before a
// caller has had the opportunity to write the graceful q command.
func (p *managedProcess) close() error {
	if p == nil {
		return nil
	}
	p.closeOnce.Do(func() {
		jobErr := maskedProcessError(p.job.close(), errManagedProcessCleanup)
		p.lifecycleCancel()
		outputErr := closeProcessPipes(p.stdout, p.stderr)
		p.closeErr = errors.Join(jobErr, outputErr)
	})
	return p.closeErr
}

func (p *managedProcess) closeStdin() error {
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	if p.stdinClosed {
		return nil
	}
	p.stdinClosed = true
	if p.stdin == nil {
		return nil
	}
	err := p.stdin.Close()
	if err == nil || errors.Is(err, os.ErrClosed) {
		return nil
	}
	return errManagedProcessCleanup
}

func (p *managedProcess) hasExited() bool {
	select {
	case <-p.doneCh:
		return true
	default:
		return false
	}
}

func (p *managedProcess) waitResult() error {
	p.resultMu.RLock()
	defer p.resultMu.RUnlock()
	return p.result
}
