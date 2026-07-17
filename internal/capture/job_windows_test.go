//go:build windows

package capture

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

type windowsJobProbe struct {
	createErr    error
	setErr       error
	assignErr    error
	terminateErr error
	closeErr     error

	createCalls    atomic.Int32
	setCalls       atomic.Int32
	assignCalls    atomic.Int32
	terminateCalls atomic.Int32
	closeCalls     atomic.Int32

	limitFlags uint32
	jobHandle  windows.Handle
	procHandle windows.Handle
	exitCode   uint32
}

func (p *windowsJobProbe) ops() windowsJobOps {
	return windowsJobOps{
		create: func() (windows.Handle, error) {
			p.createCalls.Add(1)
			return windows.Handle(100), p.createErr
		},
		setLimits: func(job windows.Handle, limits *windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION) error {
			p.setCalls.Add(1)
			p.jobHandle = job
			p.limitFlags = limits.BasicLimitInformation.LimitFlags
			return p.setErr
		},
		assign: func(job, process windows.Handle) error {
			p.assignCalls.Add(1)
			p.jobHandle, p.procHandle = job, process
			return p.assignErr
		},
		terminate: func(job windows.Handle, exitCode uint32) error {
			p.terminateCalls.Add(1)
			p.jobHandle, p.exitCode = job, exitCode
			return p.terminateErr
		},
		close: func(job windows.Handle) error {
			p.closeCalls.Add(1)
			p.jobHandle = job
			return p.closeErr
		},
	}
}

func TestWindowsJobCreateFailureDoesNotSetOrCloseInvalidHandle(t *testing.T) {
	secret := errors.New("sensitive create detail")
	probe := &windowsJobProbe{createErr: secret}
	job, err := newWindowsProcessJobWithOps(probe.ops())
	if job != nil || !errors.Is(err, errManagedProcessIsolation) || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("new job = (%v, %v), want masked create failure", job, err)
	}
	if probe.createCalls.Load() != 1 || probe.setCalls.Load() != 0 || probe.closeCalls.Load() != 0 {
		t.Fatalf("create failure calls = create:%d set:%d close:%d", probe.createCalls.Load(), probe.setCalls.Load(), probe.closeCalls.Load())
	}
}

func TestWindowsJobSetFailureClosesHandleAndMasksFaults(t *testing.T) {
	secret := errors.New("sensitive set detail")
	probe := &windowsJobProbe{setErr: secret, closeErr: secret}
	job, err := newWindowsProcessJobWithOps(probe.ops())
	if job != nil || !errors.Is(err, errManagedProcessIsolation) || !errors.Is(err, errManagedProcessCleanup) {
		t.Fatalf("new job = (%v, %v), want isolation and cleanup", job, err)
	}
	if strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("set failure leaked detail: %v", err)
	}
	if probe.createCalls.Load() != 1 || probe.setCalls.Load() != 1 || probe.closeCalls.Load() != 1 {
		t.Fatalf("set failure calls = create:%d set:%d close:%d", probe.createCalls.Load(), probe.setCalls.Load(), probe.closeCalls.Load())
	}
}

func TestWindowsJobSetsKillOnCloseBeforeAssigningNativeHandle(t *testing.T) {
	probe := &windowsJobProbe{}
	job, err := newWindowsProcessJobWithOps(probe.ops())
	if err != nil {
		t.Fatal(err)
	}
	command := newFakeProcessCommand()
	if err := job.assign(command); err != nil {
		t.Fatal(err)
	}
	if probe.limitFlags != windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE {
		t.Fatalf("job limit flags = %#x, want KILL_ON_JOB_CLOSE", probe.limitFlags)
	}
	if probe.jobHandle != windows.Handle(100) || probe.procHandle != windows.Handle(42) || command.handleCall.Load() != 1 {
		t.Fatalf("assigned handles = job:%v process:%v calls:%d", probe.jobHandle, probe.procHandle, command.handleCall.Load())
	}
	if err := job.close(); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsJobAssignAndTerminateErrorsAreStableAndMasked(t *testing.T) {
	secret := errors.New("https://secret.invalid/process")
	probe := &windowsJobProbe{assignErr: secret, terminateErr: secret}
	job, err := newWindowsProcessJobWithOps(probe.ops())
	if err != nil {
		t.Fatal(err)
	}
	if err := job.assign(newFakeProcessCommand()); !errors.Is(err, errManagedProcessIsolation) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("assign error = %v", err)
	}
	if err := job.terminate(77); !errors.Is(err, errManagedProcessControl) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("terminate error = %v", err)
	}
	if probe.exitCode != 77 {
		t.Fatalf("terminate exit code = %d, want 77", probe.exitCode)
	}
	_ = job.close()
}

func TestWindowsJobCloseIsConcurrentAndIdempotent(t *testing.T) {
	probe := &windowsJobProbe{}
	job, err := newWindowsProcessJobWithOps(probe.ops())
	if err != nil {
		t.Fatal(err)
	}
	const callers = 64
	var wait sync.WaitGroup
	wait.Add(callers)
	for range callers {
		go func() {
			defer wait.Done()
			if closeErr := job.close(); closeErr != nil {
				t.Errorf("close error = %v", closeErr)
			}
		}()
	}
	wait.Wait()
	if probe.closeCalls.Load() != 1 {
		t.Fatalf("CloseHandle calls = %d, want 1", probe.closeCalls.Load())
	}
	if err := job.terminate(1); err != nil {
		t.Fatalf("terminate after close = %v", err)
	}
	if probe.terminateCalls.Load() != 0 {
		t.Fatalf("TerminateJobObject calls after close = %d", probe.terminateCalls.Load())
	}
}

const windowsJobTreeHelperEnv = "DOUYINLIVE_JOB_TREE_HELPER"
const windowsJobTreeMarkerEnv = "DOUYINLIVE_JOB_TREE_MARKER"

var (
	errWindowsJobTreeHandshake = errors.New("job tree helper handshake failed")
	errWindowsJobTreeQuery     = errors.New("job tree process query failed")
	errWindowsJobTreeResidual  = errors.New("job tree process remained active")
	errWindowsJobTreeMarker    = errors.New("suspended process marker check failed")
)

type windowsJobTreePIDs struct {
	parent int
	child  int
}

type blockingResumeProcessCommand struct {
	processCommand
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (c *blockingResumeProcessCommand) resume() error {
	c.once.Do(func() {
		close(c.entered)
	})
	<-c.release
	return c.processCommand.resume()
}

func TestWindowsManagedProcessTerminatesRealJobTree(t *testing.T) {
	testCases := []struct {
		name      string
		terminate func(*managedProcess) error
	}{
		{name: "terminate_tree", terminate: func(process *managedProcess) error { return process.terminateTree() }},
		{name: "close", terminate: func(process *managedProcess) error { return process.close() }},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			process, streams, err := startManagedProcess(context.Background(), processConfig{
				Path: os.Args[0],
				Args: []string{"-test.run=^TestWindowsJobTreeHelper$"},
				Env:  windowsJobTreeHelperEnvironment("parent"),
			})
			if err != nil {
				t.Fatal(err)
			}

			var pids windowsJobTreePIDs
			var drains sync.WaitGroup
			handshake := make(chan struct {
				pids windowsJobTreePIDs
				err  error
			}, 1)
			drains.Add(2)
			go func() {
				defer drains.Done()
				defer streams.Stdout.Close()
				reader := bufio.NewReader(streams.Stdout)
				line, readErr := reader.ReadString('\n')
				var result windowsJobTreePIDs
				if readErr == nil {
					_, readErr = fmt.Sscanf(strings.TrimSpace(line), "%d %d", &result.parent, &result.child)
				}
				if readErr != nil || result.parent <= 0 || result.child <= 0 {
					handshake <- struct {
						pids windowsJobTreePIDs
						err  error
					}{err: errWindowsJobTreeHandshake}
				} else {
					handshake <- struct {
						pids windowsJobTreePIDs
						err  error
					}{pids: result}
				}
				_, _ = io.Copy(io.Discard, reader)
			}()
			go func() {
				defer drains.Done()
				defer streams.Stderr.Close()
				_, _ = io.Copy(io.Discard, streams.Stderr)
			}()

			t.Cleanup(func() {
				_ = process.terminateTree()
				_ = process.close()
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_ = process.wait(cleanupCtx)
				if pids.parent > 0 && pids.child > 0 {
					_ = waitWindowsJobTreeGone(pids, 3*time.Second)
				}
			})

			// The q command is a test-only spawn gate. The helper cannot create
			// its child until after startManagedProcess has assigned the parent
			// to the Job, proving that the child inherits Job membership.
			if err := process.writeQuit(); err != nil {
				t.Fatal(err)
			}
			select {
			case result := <-handshake:
				if result.err != nil {
					t.Fatal(result.err)
				}
				pids = result.pids
			case <-time.After(5 * time.Second):
				t.Fatal(errWindowsJobTreeHandshake)
			}
			for _, pid := range []int{pids.parent, pids.child} {
				running, queryErr := windowsProcessRunning(pid)
				if queryErr != nil {
					t.Fatal(queryErr)
				}
				if !running {
					t.Fatal(errWindowsJobTreeHandshake)
				}
			}

			if err := testCase.terminate(process); err != nil {
				t.Fatal(err)
			}
			waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			waitErr := process.wait(waitCtx)
			cancel()
			if errors.Is(waitErr, context.DeadlineExceeded) {
				t.Fatal(waitErr)
			}
			if waitErr != nil && !errors.Is(waitErr, errManagedProcessWait) {
				t.Fatal(waitErr)
			}
			drains.Wait()
			if err := waitWindowsJobTreeGone(pids, 5*time.Second); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWindowsManagedProcessIsSuspendedUntilJobAssignAndResume(t *testing.T) {
	markerPath := filepath.Join(t.TempDir(), "executed.marker")
	resumeEntered := make(chan struct{})
	resumeRelease := make(chan struct{})
	dependencies := defaultProcessDependencies()
	newCommand := dependencies.newCommand
	dependencies.newCommand = func(lifecycle context.Context, config processConfig) processCommand {
		return &blockingResumeProcessCommand{
			processCommand: newCommand(lifecycle, config),
			entered:        resumeEntered,
			release:        resumeRelease,
		}
	}
	type startResult struct {
		process *managedProcess
		streams processStreams
		err     error
	}
	started := make(chan startResult, 1)
	go func() {
		process, streams, err := startManagedProcessWithDependencies(
			context.Background(),
			processConfig{
				Path: os.Args[0],
				Args: []string{"-test.run=^TestWindowsJobTreeHelper$"},
				Env: append(
					windowsJobTreeHelperEnvironment("marker"),
					windowsJobTreeMarkerEnv+"="+markerPath,
				),
			},
			dependencies,
		)
		started <- startResult{process: process, streams: streams, err: err}
	}()
	select {
	case <-resumeEntered:
	case result := <-started:
		if result.err != nil {
			t.Fatal(result.err)
		}
		t.Fatal(errWindowsJobTreeMarker)
	case <-time.After(5 * time.Second):
		t.Fatal(errWindowsJobTreeMarker)
	}
	time.Sleep(200 * time.Millisecond)
	_, statErr := os.Stat(markerPath)
	executedWhileSuspended := statErr == nil
	markerQueryFailed := statErr != nil && !errors.Is(statErr, os.ErrNotExist)
	close(resumeRelease)

	var result startResult
	select {
	case result = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal(errWindowsJobTreeMarker)
	}
	if result.err != nil || result.process == nil {
		t.Fatal(result.err)
	}
	var drains sync.WaitGroup
	drains.Add(2)
	go func() {
		defer drains.Done()
		defer result.streams.Stdout.Close()
		_, _ = io.Copy(io.Discard, result.streams.Stdout)
	}()
	go func() {
		defer drains.Done()
		defer result.streams.Stderr.Close()
		_, _ = io.Copy(io.Discard, result.streams.Stderr)
	}()
	markerAppeared, markerErr := waitWindowsMarker(markerPath, 3*time.Second)
	terminateErr := result.process.terminateTree()
	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	waitErr := result.process.wait(waitCtx)
	cancel()
	drains.Wait()
	if terminateErr != nil || (waitErr != nil && !errors.Is(waitErr, errManagedProcessWait)) {
		t.Fatal(errors.Join(terminateErr, waitErr))
	}
	if markerQueryFailed || markerErr != nil || executedWhileSuspended || !markerAppeared {
		t.Fatal(errWindowsJobTreeMarker)
	}
}

func TestWindowsJobTreeHelper(t *testing.T) {
	switch os.Getenv(windowsJobTreeHelperEnv) {
	case "parent":
		reader := bufio.NewReader(os.Stdin)
		line, err := reader.ReadString('\n')
		if err != nil || line != "q\n" {
			os.Exit(81)
		}
		child := exec.Command(os.Args[0], "-test.run=^TestWindowsJobTreeHelper$")
		child.Env = windowsJobTreeHelperEnvironment("child")
		child.Stdout = io.Discard
		child.Stderr = io.Discard
		if err := child.Start(); err != nil {
			os.Exit(82)
		}
		_, _ = fmt.Fprintf(os.Stdout, "%d %d\n", os.Getpid(), child.Process.Pid)
		for {
			time.Sleep(time.Hour)
		}
	case "marker":
		markerPath := os.Getenv(windowsJobTreeMarkerEnv)
		if markerPath == "" {
			os.Exit(83)
		}
		marker, err := os.OpenFile(markerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(84)
		}
		if err := marker.Close(); err != nil {
			os.Exit(85)
		}
		for {
			time.Sleep(time.Hour)
		}
	case "child":
		for {
			time.Sleep(time.Hour)
		}
	}
}

func windowsJobTreeHelperEnvironment(mode string) []string {
	helperPrefix := strings.ToUpper(windowsJobTreeHelperEnv) + "="
	markerPrefix := strings.ToUpper(windowsJobTreeMarkerEnv) + "="
	environment := make([]string, 0, len(os.Environ())+1)
	for _, value := range os.Environ() {
		upperValue := strings.ToUpper(value)
		if strings.HasPrefix(upperValue, helperPrefix) || strings.HasPrefix(upperValue, markerPrefix) {
			continue
		}
		environment = append(environment, value)
	}
	return append(environment, windowsJobTreeHelperEnv+"="+mode)
}

func windowsProcessRunning(pid int) (bool, error) {
	if pid <= 0 {
		return false, errWindowsJobTreeQuery
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return false, nil
		}
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return true, nil
		}
		return false, errWindowsJobTreeQuery
	}
	defer windows.CloseHandle(handle)
	result, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		return false, errWindowsJobTreeQuery
	}
	switch result {
	case uint32(windows.WAIT_OBJECT_0):
		return false, nil
	case uint32(windows.WAIT_TIMEOUT):
		return true, nil
	default:
		return false, errWindowsJobTreeQuery
	}
}

func waitWindowsJobTreeGone(pids windowsJobTreePIDs, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		anyRunning := false
		for _, pid := range []int{pids.parent, pids.child} {
			running, err := windowsProcessRunning(pid)
			if err != nil {
				return err
			}
			anyRunning = anyRunning || running
		}
		if !anyRunning {
			return nil
		}
		if time.Now().After(deadline) {
			return errWindowsJobTreeResidual
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitWindowsMarker(path string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		_, err := os.Stat(path)
		switch {
		case err == nil:
			return true, nil
		case !errors.Is(err, os.ErrNotExist):
			return false, errWindowsJobTreeMarker
		case time.Now().After(deadline):
			return false, nil
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}
