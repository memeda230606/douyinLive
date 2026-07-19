package capture

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	managedProcessTestAttemptID = "0198f6e4-4d00-7000-8000-000000000001"
	managedProcessTestNamespace = "0123456789abcdef0123456789abcdef"
)

type fakeProcessPipe struct {
	mu       sync.Mutex
	data     []byte
	closes   int
	closeErr error
	writeErr error
}

func (p *fakeProcessPipe) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (p *fakeProcessPipe) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.writeErr != nil {
		return 0, p.writeErr
	}
	p.data = append(p.data, data...)
	return len(data), nil
}

func (p *fakeProcessPipe) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closes++
	return p.closeErr
}

func (p *fakeProcessPipe) snapshot() ([]byte, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.data...), p.closes
}

type fakeProcessCommand struct {
	stdin  *fakeProcessPipe
	stdout *fakeProcessPipe
	stderr *fakeProcessPipe

	stdinErr     error
	stdoutErr    error
	stderrErr    error
	configureErr error
	startErr     error
	resumeErr    error
	waitErr      error
	killErr      error
	handleErr    error

	waitRelease <-chan struct{}
	waitEntered chan struct{}
	waitOnce    sync.Once
	lifecycle   context.Context

	configureCalls atomic.Int32
	startCalls     atomic.Int32
	resumeCalls    atomic.Int32
	waitCalls      atomic.Int32
	killCalls      atomic.Int32
	handleCall     atomic.Int32

	orderMu *sync.Mutex
	order   *[]string
}

func newFakeProcessCommand() *fakeProcessCommand {
	release := make(chan struct{})
	close(release)
	return &fakeProcessCommand{
		stdin: &fakeProcessPipe{}, stdout: &fakeProcessPipe{}, stderr: &fakeProcessPipe{},
		waitRelease: release, waitEntered: make(chan struct{}),
	}
}

func (c *fakeProcessCommand) log(value string) {
	if c.orderMu == nil || c.order == nil {
		return
	}
	c.orderMu.Lock()
	*c.order = append(*c.order, value)
	c.orderMu.Unlock()
}

func (c *fakeProcessCommand) stdinPipe() (io.WriteCloser, error) {
	return c.stdin, c.stdinErr
}
func (c *fakeProcessCommand) stdoutPipe() (io.ReadCloser, error) {
	return c.stdout, c.stdoutErr
}
func (c *fakeProcessCommand) stderrPipe() (io.ReadCloser, error) {
	return c.stderr, c.stderrErr
}
func (c *fakeProcessCommand) configure() error {
	c.configureCalls.Add(1)
	c.log("configure")
	return c.configureErr
}
func (c *fakeProcessCommand) start() error {
	c.startCalls.Add(1)
	c.log("start")
	return c.startErr
}
func (c *fakeProcessCommand) resume() error {
	c.resumeCalls.Add(1)
	c.log("resume")
	return c.resumeErr
}
func (c *fakeProcessCommand) wait() error {
	c.waitCalls.Add(1)
	c.log("wait")
	c.waitOnce.Do(func() { close(c.waitEntered) })
	if c.waitRelease != nil {
		select {
		case <-c.waitRelease:
		case <-c.lifecycle.Done():
		}
	}
	return c.waitErr
}
func (c *fakeProcessCommand) kill() error {
	c.killCalls.Add(1)
	c.log("kill")
	return c.killErr
}
func (c *fakeProcessCommand) withHandle(call func(uintptr) error) error {
	c.handleCall.Add(1)
	if c.handleErr != nil {
		return c.handleErr
	}
	return call(42)
}

type fakeProcessJob struct {
	assignErr    error
	terminateErr error
	closeErr     error

	assignCalls    atomic.Int32
	terminateCalls atomic.Int32
	closeCalls     atomic.Int32

	closeHook func()
	orderMu   *sync.Mutex
	order     *[]string
	closeOnce sync.Once
}

func (j *fakeProcessJob) log(value string) {
	if j.orderMu == nil || j.order == nil {
		return
	}
	j.orderMu.Lock()
	*j.order = append(*j.order, value)
	j.orderMu.Unlock()
}

func (j *fakeProcessJob) assign(processCommand) error {
	j.assignCalls.Add(1)
	j.log("assign")
	return j.assignErr
}
func (j *fakeProcessJob) terminate(uint32) error {
	j.terminateCalls.Add(1)
	j.log("terminate")
	return j.terminateErr
}
func (j *fakeProcessJob) close() error {
	var result error
	j.closeOnce.Do(func() {
		j.closeCalls.Add(1)
		j.log("close")
		if j.closeHook != nil {
			j.closeHook()
		}
		result = j.closeErr
	})
	if result == nil && j.closeCalls.Load() > 0 {
		return j.closeErr
	}
	return result
}

func fakeProcessDependencies(command *fakeProcessCommand, job *fakeProcessJob) processDependencies {
	return processDependencies{
		newCommand: func(lifecycle context.Context, _ processConfig) processCommand {
			command.lifecycle = lifecycle
			return command
		},
		newJob: func(string, string) (processJob, error) { return job, nil },
	}
}

func closeTestProcessStreams(t *testing.T, streams processStreams) {
	t.Helper()
	if err := errors.Join(streams.Stdout.Close(), streams.Stderr.Close()); err != nil {
		t.Fatalf("close process streams: %v", err)
	}
}

type outputOverrideProcessCommand struct {
	*fakeProcessCommand
	stdoutOverride io.ReadCloser
	stderrOverride io.ReadCloser
}

func (c *outputOverrideProcessCommand) stdoutPipe() (io.ReadCloser, error) {
	return c.stdoutOverride, c.stdoutErr
}

func (c *outputOverrideProcessCommand) stderrPipe() (io.ReadCloser, error) {
	return c.stderrOverride, c.stderrErr
}

type countedProcessReadCloser struct {
	io.ReadCloser
	closes atomic.Int32
}

func (r *countedProcessReadCloser) Close() error {
	r.closes.Add(1)
	return r.ReadCloser.Close()
}

func overrideOutputDependencies(command *outputOverrideProcessCommand, job *fakeProcessJob) processDependencies {
	return processDependencies{
		newCommand: func(lifecycle context.Context, _ processConfig) processCommand {
			command.lifecycle = lifecycle
			return command
		},
		newJob: func(string, string) (processJob, error) { return job, nil },
	}
}

func TestProcessConfigFormattingRedactsExecutableArgumentsAndEnvironment(t *testing.T) {
	config := processConfig{
		Path:                 `C:\private\ffmpeg.exe`,
		Dir:                  `D:\private\recordings`,
		Args:                 []string{"https://example.invalid/live?token=argument-secret", "-progress"},
		Env:                  []string{"ACCESS_TOKEN=environment-secret", "PRIVATE_PATH=C:\\private"},
		RecorderJobNamespace: "namespace-secret",
		RecorderAttemptID:    "https://example.invalid/?token=attempt-secret",
	}
	formatted := []string{fmt.Sprint(config), fmt.Sprintf("%+v", config), fmt.Sprintf("%#v", config)}
	for _, value := range formatted {
		for _, secret := range []string{"example.invalid", "argument-secret", "environment-secret", "private", "ffmpeg.exe", "recordings", "ACCESS_TOKEN"} {
			if strings.Contains(value, secret) {
				t.Fatalf("formatted process config leaked %q", secret)
			}
		}
		if !strings.Contains(value, "args:2") || !strings.Contains(value, "env:2") {
			t.Fatalf("formatted process config omitted safe counts: %s", value)
		}
	}

	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("managed process", slog.Any("config", config))
	serialized := []string{string(encoded), logs.String()}
	for _, value := range serialized {
		for _, secret := range []string{
			"example.invalid", "argument-secret", "environment-secret", "private",
			"ffmpeg.exe", "recordings", "ACCESS_TOKEN",
		} {
			if strings.Contains(value, secret) {
				t.Fatalf("serialized process config leaked %q", secret)
			}
		}
		if !strings.Contains(value, `"args_count":2`) || !strings.Contains(value, `"env_count":2`) {
			t.Fatalf("serialized process config omitted safe counts: %s", value)
		}
	}
}

func TestManagedProcessCreatesJobBeforeStartAndAssignsAfterStart(t *testing.T) {
	release := make(chan struct{})
	command := newFakeProcessCommand()
	command.waitRelease = release
	job := &fakeProcessJob{}
	var order []string
	var orderMu sync.Mutex
	command.order, command.orderMu = &order, &orderMu
	job.order, job.orderMu = &order, &orderMu
	var createdNamespace, createdAttemptID string
	dependencies := processDependencies{
		newCommand: func(lifecycle context.Context, _ processConfig) processCommand {
			command.lifecycle = lifecycle
			return command
		},
		newJob: func(jobNamespace, attemptID string) (processJob, error) {
			createdNamespace = jobNamespace
			createdAttemptID = attemptID
			orderMu.Lock()
			order = append(order, "create_set_job")
			orderMu.Unlock()
			return job, nil
		},
	}
	process, streams, err := startManagedProcessWithDependencies(
		context.Background(), processConfig{
			Path: "ffmpeg", RecorderJobNamespace: managedProcessTestNamespace,
			RecorderAttemptID: managedProcessTestAttemptID,
		}, dependencies,
	)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-command.waitEntered:
		t.Fatal("command Wait started before output streams were drained")
	default:
	}
	orderMu.Lock()
	gotOrder := append([]string(nil), order...)
	orderMu.Unlock()
	if !containsOrderedSubsequence(gotOrder, []string{"create_set_job", "configure", "start", "assign", "resume"}) {
		t.Fatalf("process setup order = %v", gotOrder)
	}
	if createdNamespace != managedProcessTestNamespace || createdAttemptID != managedProcessTestAttemptID {
		t.Fatalf("created Job identity = namespace:%q attempt:%q", createdNamespace, createdAttemptID)
	}
	closeTestProcessStreams(t, streams)
	select {
	case <-command.waitEntered:
	case <-time.After(time.Second):
		t.Fatal("command Wait did not start after output streams were closed")
	}
	close(release)
	if err := process.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestManagedProcessRejectsInvalidRecorderAttemptBeforeSideEffects(t *testing.T) {
	const secretAttempt = "https://secret.invalid/live?token=private"
	var commandCalls, jobCalls atomic.Int32
	dependencies := processDependencies{
		newCommand: func(context.Context, processConfig) processCommand {
			commandCalls.Add(1)
			return newFakeProcessCommand()
		},
		newJob: func(string, string) (processJob, error) {
			jobCalls.Add(1)
			return &fakeProcessJob{}, nil
		},
	}
	process, streams, err := startManagedProcessWithDependencies(
		context.Background(),
		processConfig{Path: "ffmpeg", RecorderAttemptID: secretAttempt},
		dependencies,
	)
	if process != nil || streams.Stdout != nil || streams.Stderr != nil || !errors.Is(err, errManagedProcessConfiguration) {
		t.Fatalf("invalid attempt result = process:%v streams:%#v err:%v", process, streams, err)
	}
	if commandCalls.Load() != 0 || jobCalls.Load() != 0 {
		t.Fatalf("invalid attempt side effects = command:%d job:%d", commandCalls.Load(), jobCalls.Load())
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "http") {
		t.Fatalf("invalid attempt leaked detail: %v", err)
	}
}

func TestManagedProcessRejectsRecorderAttemptWithoutNamespaceBeforeSideEffects(t *testing.T) {
	var commandCalls, jobCalls atomic.Int32
	dependencies := processDependencies{
		newCommand: func(context.Context, processConfig) processCommand {
			commandCalls.Add(1)
			return newFakeProcessCommand()
		},
		newJob: func(string, string) (processJob, error) {
			jobCalls.Add(1)
			return &fakeProcessJob{}, nil
		},
	}
	process, streams, err := startManagedProcessWithDependencies(
		context.Background(),
		processConfig{Path: "ffmpeg", RecorderAttemptID: managedProcessTestAttemptID},
		dependencies,
	)
	if process != nil || streams.Stdout != nil || streams.Stderr != nil ||
		!errors.Is(err, errManagedProcessConfiguration) {
		t.Fatalf("missing namespace result = process:%v streams:%#v err:%v", process, streams, err)
	}
	if commandCalls.Load() != 0 || jobCalls.Load() != 0 {
		t.Fatalf("missing namespace side effects = command:%d job:%d", commandCalls.Load(), jobCalls.Load())
	}
}

func TestManagedProcessUsesPrivateLifecycleAndWritesGracefulQuit(t *testing.T) {
	release := make(chan struct{})
	command := newFakeProcessCommand()
	command.waitRelease = release
	job := &fakeProcessJob{}
	callerCtx, cancelCaller := context.WithCancel(context.Background())
	process, streams, err := startManagedProcessWithDependencies(
		callerCtx,
		processConfig{Path: "ffmpeg", Args: []string{"sensitive-url"}},
		fakeProcessDependencies(command, job),
	)
	if err != nil {
		t.Fatal(err)
	}
	stdout, stdoutOK := streams.Stdout.(*trackedProcessReader)
	stderr, stderrOK := streams.Stderr.(*trackedProcessReader)
	if !stdoutOK || !stderrOK || stdout.reader != command.stdout || stderr.reader != command.stderr {
		t.Fatal("start did not return the configured output pipes")
	}
	closeTestProcessStreams(t, streams)
	<-command.waitEntered
	cancelCaller()
	select {
	case <-command.lifecycle.Done():
		t.Fatal("caller cancellation reached private process lifecycle")
	case <-time.After(10 * time.Millisecond):
	}
	if err := process.writeQuit(); err != nil {
		t.Fatal(err)
	}
	data, closes := command.stdin.snapshot()
	if string(data) != "q\n" || closes != 0 {
		t.Fatalf("stdin before process exit = (%q, closes=%d)", data, closes)
	}
	close(release)
	if err := process.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, closes = command.stdin.snapshot()
	if closes != 1 || job.closeCalls.Load() != 1 {
		t.Fatalf("cleanup calls = stdin:%d job:%d, want one each", closes, job.closeCalls.Load())
	}
	select {
	case <-command.lifecycle.Done():
	default:
		t.Fatal("private lifecycle was not cancelled during final cleanup")
	}
}

func TestManagedProcessWaitContextDoesNotKillProcess(t *testing.T) {
	release := make(chan struct{})
	command := newFakeProcessCommand()
	command.waitRelease = release
	job := &fakeProcessJob{}
	process, streams, err := startManagedProcessWithDependencies(
		context.Background(), processConfig{Path: "ffmpeg"}, fakeProcessDependencies(command, job),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := process.wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait error = %v, want deadline", err)
	}
	if command.killCalls.Load() != 0 {
		t.Fatalf("wait cancellation killed process %d times", command.killCalls.Load())
	}
	select {
	case <-process.done():
		t.Fatal("wait cancellation completed the process")
	default:
	}
	closeTestProcessStreams(t, streams)
	close(release)
	if err := process.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestManagedProcessHasSingleWaitOwnerForConcurrentObservers(t *testing.T) {
	release := make(chan struct{})
	command := newFakeProcessCommand()
	command.waitRelease = release
	command.waitErr = errors.New("sensitive-url-in-wait")
	job := &fakeProcessJob{}
	process, streams, err := startManagedProcessWithDependencies(
		context.Background(), processConfig{Path: "ffmpeg"}, fakeProcessDependencies(command, job),
	)
	if err != nil {
		t.Fatal(err)
	}
	closeTestProcessStreams(t, streams)
	<-command.waitEntered
	const observers = 32
	results := make(chan error, observers)
	for range observers {
		go func() { results <- process.wait(context.Background()) }()
	}
	close(release)
	for range observers {
		result := <-results
		if !errors.Is(result, errManagedProcessWait) || strings.Contains(result.Error(), "sensitive") {
			t.Fatalf("wait result = %v, want stable masked error", result)
		}
	}
	if command.waitCalls.Load() != 1 {
		t.Fatalf("command Wait calls = %d, want 1", command.waitCalls.Load())
	}
}

func TestManagedProcessStartFailureClosesPreparedResources(t *testing.T) {
	secret := errors.New("sensitive start args")
	command := newFakeProcessCommand()
	command.startErr = secret
	job := &fakeProcessJob{}
	_, _, err := startManagedProcessWithDependencies(
		context.Background(), processConfig{Path: "ffmpeg", Args: []string{"https://secret"}},
		fakeProcessDependencies(command, job),
	)
	if !errors.Is(err, errManagedProcessStart) || strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), "https") {
		t.Fatalf("start error = %v, want stable masked error", err)
	}
	if command.killCalls.Load() != 0 || command.waitCalls.Load() != 0 || job.closeCalls.Load() != 1 {
		t.Fatalf("failed start cleanup = kill:%d wait:%d close:%d", command.killCalls.Load(), command.waitCalls.Load(), job.closeCalls.Load())
	}
	for name, pipe := range map[string]*fakeProcessPipe{"stdin": command.stdin, "stdout": command.stdout, "stderr": command.stderr} {
		if _, closes := pipe.snapshot(); closes != 1 {
			t.Fatalf("%s closes = %d, want 1", name, closes)
		}
	}
}

func TestManagedProcessAssignFailureIsFailClosedDespiteCleanupFaults(t *testing.T) {
	secret := errors.New("https://secret.invalid/live")
	command := newFakeProcessCommand()
	command.killErr = secret
	command.waitErr = secret
	command.stdin.closeErr = secret
	job := &fakeProcessJob{assignErr: secret, closeErr: secret}
	var order []string
	var orderMu sync.Mutex
	command.order, command.orderMu = &order, &orderMu
	job.order, job.orderMu = &order, &orderMu
	_, _, err := startManagedProcessWithDependencies(
		context.Background(), processConfig{Path: "ffmpeg", Args: []string{secret.Error()}},
		fakeProcessDependencies(command, job),
	)
	if !errors.Is(err, errManagedProcessIsolation) || !errors.Is(err, errManagedProcessControl) || !errors.Is(err, errManagedProcessWait) || !errors.Is(err, errManagedProcessCleanup) {
		t.Fatalf("assign failure = %v, want isolation/control/wait/cleanup", err)
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "https") {
		t.Fatalf("assign failure leaked command detail: %v", err)
	}
	if command.killCalls.Load() != 1 || command.waitCalls.Load() != 1 || job.closeCalls.Load() != 1 {
		t.Fatalf("assign cleanup = kill:%d wait:%d close:%d", command.killCalls.Load(), command.waitCalls.Load(), job.closeCalls.Load())
	}
	orderMu.Lock()
	gotOrder := append([]string(nil), order...)
	orderMu.Unlock()
	if !containsOrderedSubsequence(gotOrder, []string{"configure", "start", "assign", "kill", "wait", "close"}) || command.resumeCalls.Load() != 0 {
		t.Fatalf("assign failure order = %v", gotOrder)
	}
}

func TestManagedProcessJobCreationFailureDoesNotStartCommand(t *testing.T) {
	command := newFakeProcessCommand()
	secret := errors.New("sensitive job creation detail")
	dependencies := processDependencies{
		newCommand: func(lifecycle context.Context, _ processConfig) processCommand {
			command.lifecycle = lifecycle
			return command
		},
		newJob: func(string, string) (processJob, error) { return nil, secret },
	}
	_, _, err := startManagedProcessWithDependencies(context.Background(), processConfig{Path: "ffmpeg"}, dependencies)
	if !errors.Is(err, errManagedProcessIsolation) || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("job creation error = %v, want stable masked error", err)
	}
	if command.startCalls.Load() != 0 {
		t.Fatalf("command started %d times before job creation", command.startCalls.Load())
	}
}

func TestManagedProcessConfigureFailureDoesNotStart(t *testing.T) {
	secret := errors.New("sensitive suspended setup detail")
	command := newFakeProcessCommand()
	command.configureErr = secret
	job := &fakeProcessJob{}
	_, _, err := startManagedProcessWithDependencies(
		context.Background(), processConfig{Path: "ffmpeg"}, fakeProcessDependencies(command, job),
	)
	if !errors.Is(err, errManagedProcessIsolation) || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("configure failure = %v, want stable isolation error", err)
	}
	if command.configureCalls.Load() != 1 || command.startCalls.Load() != 0 || command.resumeCalls.Load() != 0 ||
		command.killCalls.Load() != 0 || command.waitCalls.Load() != 0 || job.assignCalls.Load() != 0 || job.closeCalls.Load() != 1 {
		t.Fatalf(
			"configure failure calls = configure:%d start:%d resume:%d kill:%d wait:%d assign:%d close:%d",
			command.configureCalls.Load(), command.startCalls.Load(), command.resumeCalls.Load(), command.killCalls.Load(),
			command.waitCalls.Load(), job.assignCalls.Load(), job.closeCalls.Load(),
		)
	}
	for name, pipe := range map[string]*fakeProcessPipe{"stdin": command.stdin, "stdout": command.stdout, "stderr": command.stderr} {
		if _, closes := pipe.snapshot(); closes != 1 {
			t.Fatalf("%s closes = %d, want 1", name, closes)
		}
	}
}

func TestManagedProcessResumeFailureIsFailClosedDespiteCleanupFaults(t *testing.T) {
	secret := errors.New("sensitive resume URL detail")
	command := newFakeProcessCommand()
	command.resumeErr = secret
	command.killErr = secret
	command.waitErr = secret
	command.stdin.closeErr = secret
	job := &fakeProcessJob{closeErr: secret}
	var order []string
	var orderMu sync.Mutex
	command.order, command.orderMu = &order, &orderMu
	job.order, job.orderMu = &order, &orderMu
	_, _, err := startManagedProcessWithDependencies(
		context.Background(), processConfig{Path: "ffmpeg", Args: []string{"redacted"}},
		fakeProcessDependencies(command, job),
	)
	if !errors.Is(err, errManagedProcessResume) || !errors.Is(err, errManagedProcessControl) ||
		!errors.Is(err, errManagedProcessWait) || !errors.Is(err, errManagedProcessCleanup) {
		t.Fatalf("resume failure = %v, want resume/control/wait/cleanup", err)
	}
	if strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), "URL") {
		t.Fatalf("resume failure leaked detail: %v", err)
	}
	if command.configureCalls.Load() != 1 || command.startCalls.Load() != 1 || command.resumeCalls.Load() != 1 ||
		command.killCalls.Load() != 1 || command.waitCalls.Load() != 1 || job.assignCalls.Load() != 1 || job.closeCalls.Load() != 1 {
		t.Fatalf(
			"resume failure calls = configure:%d start:%d resume:%d kill:%d wait:%d assign:%d close:%d",
			command.configureCalls.Load(), command.startCalls.Load(), command.resumeCalls.Load(), command.killCalls.Load(),
			command.waitCalls.Load(), job.assignCalls.Load(), job.closeCalls.Load(),
		)
	}
	orderMu.Lock()
	gotOrder := append([]string(nil), order...)
	orderMu.Unlock()
	if !containsOrderedSubsequence(gotOrder, []string{"configure", "start", "assign", "resume", "kill", "close", "wait"}) {
		t.Fatalf("resume failure order = %v", gotOrder)
	}
}

func TestManagedProcessCallerCancellationBeforeStartHasNoSideEffects(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var commandCalls, jobCalls atomic.Int32
	dependencies := processDependencies{
		newCommand: func(context.Context, processConfig) processCommand {
			commandCalls.Add(1)
			return newFakeProcessCommand()
		},
		newJob: func(string, string) (processJob, error) {
			jobCalls.Add(1)
			return &fakeProcessJob{}, nil
		},
	}
	_, _, err := startManagedProcessWithDependencies(ctx, processConfig{Path: "ffmpeg"}, dependencies)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-start cancellation error = %v", err)
	}
	if commandCalls.Load() != 0 || jobCalls.Load() != 0 {
		t.Fatalf("pre-start side effects = command:%d job:%d", commandCalls.Load(), jobCalls.Load())
	}
}

func TestManagedProcessTerminateMethodsAndCloseAreIdempotent(t *testing.T) {
	release := make(chan struct{})
	command := newFakeProcessCommand()
	command.waitRelease = release
	job := &fakeProcessJob{}
	process, streams, err := startManagedProcessWithDependencies(
		context.Background(), processConfig{Path: "ffmpeg"}, fakeProcessDependencies(command, job),
	)
	if err != nil {
		t.Fatal(err)
	}
	closeTestProcessStreams(t, streams)
	<-command.waitEntered
	const callers = 24
	var wait sync.WaitGroup
	for range callers {
		wait.Add(2)
		go func() { defer wait.Done(); _ = process.terminateProcess() }()
		go func() { defer wait.Done(); _ = process.terminateTree() }()
	}
	wait.Wait()
	for range callers {
		wait.Add(1)
		go func() { defer wait.Done(); _ = process.close() }()
	}
	wait.Wait()
	close(release)
	_ = process.wait(context.Background())
	if command.killCalls.Load() != 1 || job.terminateCalls.Load() != 1 || job.closeCalls.Load() != 1 {
		t.Fatalf("idempotent calls = kill:%d terminate:%d close:%d", command.killCalls.Load(), job.terminateCalls.Load(), job.closeCalls.Load())
	}
}

func TestManagedProcessCleanupAndControlErrorsAreMasked(t *testing.T) {
	secret := errors.New("sensitive credential URL")
	release := make(chan struct{})
	command := newFakeProcessCommand()
	command.waitRelease = release
	command.stdin.writeErr = secret
	command.stdin.closeErr = secret
	job := &fakeProcessJob{terminateErr: secret, closeErr: secret}
	process, _, err := startManagedProcessWithDependencies(
		context.Background(), processConfig{Path: "ffmpeg"}, fakeProcessDependencies(command, job),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.writeQuit(); !errors.Is(err, errManagedProcessControl) || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("writeQuit error = %v", err)
	}
	if err := process.terminateTree(); !errors.Is(err, errManagedProcessControl) || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("terminateTree error = %v", err)
	}
	if err := process.close(); !errors.Is(err, errManagedProcessCleanup) || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("close error = %v", err)
	}
	close(release)
	if err := process.wait(context.Background()); !errors.Is(err, errManagedProcessCleanup) || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("wait cleanup error = %v", err)
	}
}

func TestManagedProcessDrainsBothOutputTailsBeforeWait(t *testing.T) {
	stdoutReader, stdoutWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()
	command := &outputOverrideProcessCommand{
		fakeProcessCommand: newFakeProcessCommand(),
		stdoutOverride:     stdoutReader,
		stderrOverride:     stderrReader,
	}
	process, streams, err := startManagedProcessWithDependencies(
		context.Background(),
		processConfig{Path: "ffmpeg"},
		overrideOutputDependencies(command, &fakeProcessJob{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	writes := make(chan error, 2)
	go func() {
		_, writeErr := io.WriteString(stdoutWriter, "stdout-tail")
		writes <- errors.Join(writeErr, stdoutWriter.Close())
	}()
	go func() {
		_, writeErr := io.WriteString(stderrWriter, "stderr-tail")
		writes <- errors.Join(writeErr, stderrWriter.Close())
	}()
	stdoutTail, err := io.ReadAll(streams.Stdout)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-command.waitEntered:
		t.Fatal("command Wait started before stderr tail was drained")
	default:
	}
	stderrTail, err := io.ReadAll(streams.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if string(stdoutTail) != "stdout-tail" || string(stderrTail) != "stderr-tail" {
		t.Fatalf("drained tails = (%q, %q)", stdoutTail, stderrTail)
	}
	for range 2 {
		if err := <-writes; err != nil {
			t.Fatalf("write output tail: %v", err)
		}
	}
	select {
	case <-command.waitEntered:
	case <-time.After(time.Second):
		t.Fatal("command Wait did not start after both tails were drained")
	}
	if err := process.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestManagedProcessExplicitOutputCloseUnlocksWaitOnce(t *testing.T) {
	stdoutReader, stdoutWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()
	defer stdoutWriter.Close()
	defer stderrWriter.Close()
	countedStdout := &countedProcessReadCloser{ReadCloser: stdoutReader}
	countedStderr := &countedProcessReadCloser{ReadCloser: stderrReader}
	command := &outputOverrideProcessCommand{
		fakeProcessCommand: newFakeProcessCommand(),
		stdoutOverride:     countedStdout,
		stderrOverride:     countedStderr,
	}
	process, streams, err := startManagedProcessWithDependencies(
		context.Background(),
		processConfig{Path: "ffmpeg"},
		overrideOutputDependencies(command, &fakeProcessJob{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-command.waitEntered:
		t.Fatal("command Wait started before explicit output close")
	default:
	}
	for range 2 {
		if err := streams.Stdout.Close(); err != nil {
			t.Fatal(err)
		}
		if err := streams.Stderr.Close(); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-command.waitEntered:
	case <-time.After(time.Second):
		t.Fatal("command Wait did not start after explicit output close")
	}
	if err := process.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if countedStdout.closes.Load() != 1 || countedStderr.closes.Load() != 1 {
		t.Fatalf("underlying output closes = stdout:%d stderr:%d", countedStdout.closes.Load(), countedStderr.closes.Load())
	}
}

func TestTrackedProcessReaderEOFAndCloseAreIdempotent(t *testing.T) {
	underlying := &countedProcessReadCloser{ReadCloser: io.NopCloser(strings.NewReader(""))}
	reader := newTrackedProcessReader(underlying)
	for range 2 {
		if read, err := reader.Read(make([]byte, 1)); read != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("empty read = (%d, %v)", read, err)
		}
	}
	select {
	case <-reader.drained:
	default:
		t.Fatal("EOF did not mark reader drained")
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if underlying.closes.Load() != 1 {
		t.Fatalf("underlying Close calls = %d, want 1", underlying.closes.Load())
	}
}

type failingProcessReadCloser struct {
	err    error
	closes atomic.Int32
}

func (r *failingProcessReadCloser) Read([]byte) (int, error) {
	return 0, r.err
}

func (r *failingProcessReadCloser) Close() error {
	r.closes.Add(1)
	return nil
}

func TestTrackedProcessReaderTerminalErrorReleasesBarrier(t *testing.T) {
	underlying := &failingProcessReadCloser{err: errors.New("sensitive pipe URL")}
	reader := newTrackedProcessReader(underlying)
	if read, err := reader.Read(make([]byte, 1)); read != 0 || !errors.Is(err, errManagedProcessPipes) || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("terminal read = (%d, %v), want masked pipe error", read, err)
	}
	select {
	case <-reader.drained:
	default:
		t.Fatal("terminal read error did not release drain barrier")
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if underlying.closes.Load() != 1 {
		t.Fatalf("underlying Close calls = %d, want 1", underlying.closes.Load())
	}
}

func TestManagedProcessRealCommandSurvivesCallerCancellation(t *testing.T) {
	if os.Getenv("DOUYINLIVE_PROCESS_HELPER") == "1" {
		t.Skip("helper subprocess")
	}
	callerCtx, cancelCaller := context.WithCancel(context.Background())
	process, streams, err := startManagedProcess(callerCtx, processConfig{
		Path: os.Args[0],
		Args: []string{"-test.run=^TestManagedProcessHelper$"},
		Env:  append(os.Environ(), "DOUYINLIVE_PROCESS_HELPER=1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	stdout := bufio.NewReader(streams.Stdout)
	stderr := bufio.NewReader(streams.Stderr)
	if line, readErr := stdout.ReadString('\n'); readErr != nil || line != "ready\n" {
		t.Fatalf("helper stdout = (%q, %v)", line, readErr)
	}
	if line, readErr := stderr.ReadString('\n'); readErr != nil || line != "diagnostic\n" {
		t.Fatalf("helper stderr = (%q, %v)", line, readErr)
	}
	drained := make(chan error, 2)
	go func() {
		_, copyErr := io.Copy(io.Discard, stdout)
		drained <- copyErr
	}()
	go func() {
		_, copyErr := io.Copy(io.Discard, stderr)
		drained <- copyErr
	}()
	cancelCaller()
	if err := process.wait(callerCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait with caller context = %v", err)
	}
	select {
	case <-process.done():
		t.Fatal("real command exited when caller context was cancelled")
	case <-time.After(20 * time.Millisecond):
	}
	if err := process.writeQuit(); err != nil {
		t.Fatal(err)
	}
	if err := process.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-drained; err != nil {
			t.Fatalf("drain helper output: %v", err)
		}
	}
}

func TestManagedProcessHelper(t *testing.T) {
	if os.Getenv("DOUYINLIVE_PROCESS_HELPER") != "1" {
		return
	}
	_, _ = fmt.Fprintln(os.Stdout, "ready")
	_, _ = fmt.Fprintln(os.Stderr, "diagnostic")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil || line != "q\n" {
		os.Exit(3)
	}
	os.Exit(0)
}
