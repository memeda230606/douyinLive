//go:build windows

package capture

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/windows"
)

const recorderJobRecoveryTestAttemptID = "0198f6e4-4d00-7000-8000-000000000001"
const recorderJobRecoveryOtherAttemptID = "0198f6e4-4d00-7000-8000-000000000002"
const recorderJobRecoveryTestNamespace = "0123456789abcdef0123456789abcdef"
const windowsGlobalJobHolderHelperEnv = "DOUYINLIVE_GLOBAL_JOB_HOLDER_HELPER"
const windowsGlobalJobHolderNameEnv = "DOUYINLIVE_GLOBAL_JOB_HOLDER_NAME"

type windowsJobRecoveryProbe struct {
	openHandle   windows.Handle
	openFound    bool
	openErr      error
	active       []uint32
	queryErrAt   int
	queryErr     error
	terminateErr error
	closeErr     error
	waitErr      error
	waitHook     func()

	openCalls      int
	queryCalls     int
	terminateCalls int
	closeCalls     int
	waitCalls      int
	openedName     string
	terminatedJob  windows.Handle
	closedJob      windows.Handle
	exitCode       uint32
}

func (p *windowsJobRecoveryProbe) ops() windowsJobRecoveryOps {
	return windowsJobRecoveryOps{
		open: func(name string) (windows.Handle, bool, error) {
			p.openCalls++
			p.openedName = name
			return p.openHandle, p.openFound, p.openErr
		},
		queryActive: func(windows.Handle) (uint32, error) {
			p.queryCalls++
			if p.queryErrAt > 0 && p.queryCalls == p.queryErrAt {
				return 0, p.queryErr
			}
			if len(p.active) == 0 {
				return 0, nil
			}
			index := p.queryCalls - 1
			if index >= len(p.active) {
				index = len(p.active) - 1
			}
			return p.active[index], nil
		},
		terminate: func(handle windows.Handle, exitCode uint32) error {
			p.terminateCalls++
			p.terminatedJob = handle
			p.exitCode = exitCode
			return p.terminateErr
		},
		close: func(handle windows.Handle) error {
			p.closeCalls++
			p.closedJob = handle
			return p.closeErr
		},
		wait: func(context.Context, time.Duration) error {
			p.waitCalls++
			if p.waitHook != nil {
				p.waitHook()
			}
			return p.waitErr
		},
	}
}

func TestRecorderAttemptJobNameCanonicalizesUUIDv7WithoutSensitiveData(t *testing.T) {
	jobName, valid := recorderAttemptJobName(
		recorderJobRecoveryTestNamespace, strings.ToUpper(recorderJobRecoveryTestAttemptID),
	)
	if !valid {
		t.Fatal("canonical UUIDv7 was rejected")
	}
	want := recorderAttemptJobNamePrefix + recorderJobRecoveryTestNamespace + "." + recorderJobRecoveryTestAttemptID
	if jobName != want {
		t.Fatalf("job name = %q, want %q", jobName, want)
	}
	for _, forbidden := range []string{"http", "://", "?", "&", "/", "token"} {
		if strings.Contains(strings.ToLower(jobName), forbidden) {
			t.Fatalf("job name contains forbidden data %q", forbidden)
		}
	}
}

func TestRecorderJobNamespaceIsStableDistinctAndPathPrivate(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "private-root-token")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	otherRoot := t.TempDir()
	first, firstValid := recorderJobNamespace(root)
	cleaned, cleanedValid := recorderJobNamespace(filepath.Join(root, "."))
	caseChanged, caseValid := recorderJobNamespace(strings.ToUpper(root))
	other, otherValid := recorderJobNamespace(otherRoot)
	if !firstValid || !cleanedValid || !caseValid || !otherValid {
		t.Fatalf("namespace validity = first:%t clean:%t case:%t other:%t", firstValid, cleanedValid, caseValid, otherValid)
	}
	if first != cleaned || first != caseChanged {
		t.Fatalf("same root namespaces differ = %q / %q / %q", first, cleaned, caseChanged)
	}
	if first == other {
		t.Fatalf("distinct roots share namespace = %q", first)
	}
	jobName, valid := recorderAttemptJobName(first, recorderJobRecoveryTestAttemptID)
	if !valid || !strings.HasPrefix(jobName, `Global\DouyinLive.Recorder.v1.`) {
		t.Fatalf("global job name = %q valid:%t", jobName, valid)
	}
	lowerName := strings.ToLower(jobName)
	for _, forbidden := range []string{
		strings.ToLower(root), strings.ToLower(base), "private-root-token", `:\`, "/",
	} {
		if strings.Contains(lowerName, forbidden) {
			t.Fatalf("job name leaked path token %q: %q", forbidden, jobName)
		}
	}
}

func TestInspectRecorderAttemptProcessRejectsInvalidInputWithStableResult(t *testing.T) {
	tests := []struct {
		name      string
		ctx       context.Context
		attemptID string
		wantCode  string
		wantErr   error
	}{
		{name: "nil_context", wantCode: RecorderProcessRecoveryContextErrorCode, wantErr: errRecorderProcessRecoveryContext},
		{name: "invalid_attempt", ctx: context.Background(), attemptID: "https://secret.invalid/live?token=private", wantCode: RecorderProcessRecoveryInvalidAttemptErrorCode, wantErr: errRecorderProcessRecoveryInvalidAttempt},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tests = append(tests, struct {
		name      string
		ctx       context.Context
		attemptID string
		wantCode  string
		wantErr   error
	}{name: "cancelled", ctx: ctx, attemptID: recorderJobRecoveryTestAttemptID, wantCode: RecorderProcessRecoveryInterruptedErrorCode, wantErr: errRecorderProcessRecoveryInterrupted})

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := inspectRecorderAttemptProcess(
				test.ctx, recorderJobRecoveryTestNamespace, test.attemptID,
			)
			if result.Status != RecorderProcessRecoveryFailed || result.Found || result.Terminated || result.ErrorCode != test.wantCode || !errors.Is(err, test.wantErr) {
				t.Fatalf("invalid result = (%#v, %v)", result, err)
			}
			assertRecorderRecoveryOutputIsStable(t, result, err)
		})
	}
}

func TestRecoverWindowsRecorderAttemptProcessReportsMissingJobClean(t *testing.T) {
	probe := &windowsJobRecoveryProbe{}
	result, err := recoverWindowsRecorderAttemptProcessWithOps(
		context.Background(), recorderAttemptJobNamePrefix+recorderJobRecoveryTestNamespace+"."+recorderJobRecoveryTestAttemptID, probe.ops(),
	)
	if err != nil || result.Status != RecorderProcessRecoveryClean || result.Found || result.Terminated || result.ErrorCode != "" {
		t.Fatalf("missing result = (%#v, %v)", result, err)
	}
	if probe.openCalls != 1 || probe.queryCalls != 0 || probe.terminateCalls != 0 || probe.closeCalls != 0 {
		t.Fatalf("missing calls = open:%d query:%d terminate:%d close:%d", probe.openCalls, probe.queryCalls, probe.terminateCalls, probe.closeCalls)
	}
}

func TestRecoverWindowsRecorderAttemptProcessReportsEmptyJobCleanAndCloses(t *testing.T) {
	probe := &windowsJobRecoveryProbe{openHandle: 41, openFound: true, active: []uint32{0}}
	result, err := recoverWindowsRecorderAttemptProcessWithOps(
		context.Background(), recorderAttemptJobNamePrefix+recorderJobRecoveryTestNamespace+"."+recorderJobRecoveryTestAttemptID, probe.ops(),
	)
	if err != nil || !result.Found || result.Terminated || result.Status != RecorderProcessRecoveryClean {
		t.Fatalf("empty result = (%#v, %v)", result, err)
	}
	if probe.queryCalls != 1 || probe.terminateCalls != 0 || probe.closeCalls != 1 || probe.closedJob != 41 {
		t.Fatalf("empty calls = query:%d terminate:%d close:%d handle:%v", probe.queryCalls, probe.terminateCalls, probe.closeCalls, probe.closedJob)
	}
}

func TestRecoverWindowsRecorderAttemptProcessTerminatesOnlyOpenedJobAndConfirmsZero(t *testing.T) {
	probe := &windowsJobRecoveryProbe{openHandle: 42, openFound: true, active: []uint32{3, 1, 0}}
	result, err := recoverWindowsRecorderAttemptProcessWithOps(
		context.Background(), recorderAttemptJobNamePrefix+recorderJobRecoveryTestNamespace+"."+recorderJobRecoveryTestAttemptID, probe.ops(),
	)
	if err != nil || !result.Found || !result.Terminated || result.Status != RecorderProcessRecoveryTerminated || result.ErrorCode != "" {
		t.Fatalf("terminated result = (%#v, %v)", result, err)
	}
	if probe.openedName != recorderAttemptJobNamePrefix+recorderJobRecoveryTestNamespace+"."+recorderJobRecoveryTestAttemptID || probe.terminatedJob != 42 || probe.closedJob != 42 || probe.exitCode != managedProcessTerminateExitCode {
		t.Fatalf("job correlation = name:%q terminated:%v closed:%v code:%d", probe.openedName, probe.terminatedJob, probe.closedJob, probe.exitCode)
	}
	if probe.queryCalls != 3 || probe.terminateCalls != 1 || probe.waitCalls != 1 || probe.closeCalls != 1 {
		t.Fatalf("termination calls = query:%d terminate:%d wait:%d close:%d", probe.queryCalls, probe.terminateCalls, probe.waitCalls, probe.closeCalls)
	}
}

func TestRecoverWindowsRecorderAttemptProcessDoesNotReuseOldResult(t *testing.T) {
	firstName := recorderAttemptJobNamePrefix + recorderJobRecoveryTestNamespace + "." + recorderJobRecoveryTestAttemptID
	secondName := recorderAttemptJobNamePrefix + recorderJobRecoveryTestNamespace + "." + recorderJobRecoveryOtherAttemptID
	queryCalls := 0
	openedNames := make([]string, 0, 2)
	ops := windowsJobRecoveryOps{
		open: func(name string) (windows.Handle, bool, error) {
			openedNames = append(openedNames, name)
			if name == firstName {
				return 51, true, nil
			}
			return 0, false, nil
		},
		queryActive: func(windows.Handle) (uint32, error) {
			queryCalls++
			if queryCalls == 1 {
				return 1, nil
			}
			return 0, nil
		},
		terminate: func(windows.Handle, uint32) error { return nil },
		close:     func(windows.Handle) error { return nil },
		wait:      func(context.Context, time.Duration) error { return nil },
	}
	first, firstErr := recoverWindowsRecorderAttemptProcessWithOps(context.Background(), firstName, ops)
	second, secondErr := recoverWindowsRecorderAttemptProcessWithOps(context.Background(), secondName, ops)
	if firstErr != nil || !first.Terminated || secondErr != nil || second.Found || second.Terminated || second.Status != RecorderProcessRecoveryClean {
		t.Fatalf("old/new results = (%#v, %v) / (%#v, %v)", first, firstErr, second, secondErr)
	}
	if len(openedNames) != 2 || openedNames[0] != firstName || openedNames[1] != secondName || queryCalls != 2 {
		t.Fatalf("old/new correlation = names:%v queries:%d", openedNames, queryCalls)
	}
}

func TestRecoverWindowsRecorderAttemptProcessMasksOperationFailures(t *testing.T) {
	secret := errors.New("https://secret.invalid/live?token=private")
	tests := []struct {
		name           string
		probe          *windowsJobRecoveryProbe
		wantCode       string
		wantErr        error
		wantFound      bool
		wantTerminated bool
		wantTerminate  int
		wantClose      int
	}{
		{name: "open", probe: &windowsJobRecoveryProbe{openErr: secret}, wantCode: RecorderProcessRecoveryOpenErrorCode, wantErr: errRecorderProcessRecoveryOpen},
		{name: "query", probe: &windowsJobRecoveryProbe{openHandle: 61, openFound: true, queryErrAt: 1, queryErr: secret}, wantCode: RecorderProcessRecoveryQueryErrorCode, wantErr: errRecorderProcessRecoveryQuery, wantFound: true, wantClose: 1},
		{name: "terminate", probe: &windowsJobRecoveryProbe{openHandle: 62, openFound: true, active: []uint32{1}, terminateErr: secret}, wantCode: RecorderProcessRecoveryTerminateErrorCode, wantErr: errRecorderProcessRecoveryTerminate, wantFound: true, wantTerminate: 1, wantClose: 1},
		{name: "post_terminate_query", probe: &windowsJobRecoveryProbe{openHandle: 63, openFound: true, active: []uint32{1}, queryErrAt: 2, queryErr: secret}, wantCode: RecorderProcessRecoveryQueryErrorCode, wantErr: errRecorderProcessRecoveryQuery, wantFound: true, wantTerminate: 1, wantClose: 1},
		{name: "close_after_clean", probe: &windowsJobRecoveryProbe{openHandle: 64, openFound: true, active: []uint32{0}, closeErr: secret}, wantCode: RecorderProcessRecoveryCloseErrorCode, wantErr: errRecorderProcessRecoveryClose, wantFound: true, wantClose: 1},
		{name: "close_after_terminate", probe: &windowsJobRecoveryProbe{openHandle: 65, openFound: true, active: []uint32{1, 0}, closeErr: secret}, wantCode: RecorderProcessRecoveryCloseErrorCode, wantErr: errRecorderProcessRecoveryClose, wantFound: true, wantTerminated: true, wantTerminate: 1, wantClose: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := recoverWindowsRecorderAttemptProcessWithOps(
				context.Background(), recorderAttemptJobNamePrefix+recorderJobRecoveryTestNamespace+"."+recorderJobRecoveryTestAttemptID, test.probe.ops(),
			)
			if result.Status != RecorderProcessRecoveryFailed || result.ErrorCode != test.wantCode || result.Found != test.wantFound || result.Terminated != test.wantTerminated || !errors.Is(err, test.wantErr) {
				t.Fatalf("failure result = (%#v, %v)", result, err)
			}
			if test.probe.terminateCalls != test.wantTerminate || test.probe.closeCalls != test.wantClose {
				t.Fatalf("failure calls = terminate:%d close:%d", test.probe.terminateCalls, test.probe.closeCalls)
			}
			assertRecorderRecoveryOutputIsStable(t, result, err)
		})
	}
}

func TestRecoverWindowsRecorderAttemptProcessBoundsWaitAndHonorsCancellation(t *testing.T) {
	t.Run("incomplete", func(t *testing.T) {
		probe := &windowsJobRecoveryProbe{openHandle: 71, openFound: true, active: []uint32{1}}
		result, err := recoverWindowsRecorderAttemptProcessWithOps(
			context.Background(), recorderAttemptJobNamePrefix+recorderJobRecoveryTestNamespace+"."+recorderJobRecoveryTestAttemptID, probe.ops(),
		)
		if result.ErrorCode != RecorderProcessRecoveryIncompleteErrorCode || !errors.Is(err, errRecorderProcessRecoveryIncomplete) || !result.Found || result.Terminated {
			t.Fatalf("incomplete result = (%#v, %v)", result, err)
		}
		if probe.terminateCalls != 1 || probe.waitCalls != recorderProcessRecoveryMaximumPolls || probe.closeCalls != 1 {
			t.Fatalf("incomplete calls = terminate:%d wait:%d close:%d", probe.terminateCalls, probe.waitCalls, probe.closeCalls)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		probe := &windowsJobRecoveryProbe{openHandle: 72, openFound: true, active: []uint32{1}}
		probe.waitHook = cancel
		probe.waitErr = context.Canceled
		result, err := recoverWindowsRecorderAttemptProcessWithOps(
			ctx, recorderAttemptJobNamePrefix+recorderJobRecoveryTestNamespace+"."+recorderJobRecoveryTestAttemptID, probe.ops(),
		)
		if result.ErrorCode != RecorderProcessRecoveryInterruptedErrorCode || !errors.Is(err, errRecorderProcessRecoveryInterrupted) || !result.Found || result.Terminated {
			t.Fatalf("cancelled result = (%#v, %v)", result, err)
		}
		if probe.waitCalls != 1 || probe.closeCalls != 1 {
			t.Fatalf("cancelled calls = wait:%d close:%d", probe.waitCalls, probe.closeCalls)
		}
	})
}

func TestWindowsNamedJobCreationUsesAttemptAndRejectsExistingOrInvalidName(t *testing.T) {
	probe := &windowsJobProbe{}
	job, err := newWindowsProcessJobWithOps(recorderJobRecoveryTestNamespace, recorderJobRecoveryTestAttemptID, probe.ops())
	if err != nil || job == nil {
		t.Fatal(err)
	}
	if probe.jobName != recorderAttemptJobNamePrefix+recorderJobRecoveryTestNamespace+"."+recorderJobRecoveryTestAttemptID {
		t.Fatalf("created job name = %q", probe.jobName)
	}
	if err := job.close(); err != nil {
		t.Fatal(err)
	}

	existingProbe := &windowsJobProbe{}
	existingOps := existingProbe.ops()
	existingOps.create = func(name string) (windows.Handle, bool, error) {
		existingProbe.createCalls.Add(1)
		existingProbe.jobName = name
		return 81, false, nil
	}
	existing, existingErr := newWindowsProcessJobWithOps(recorderJobRecoveryTestNamespace, recorderJobRecoveryTestAttemptID, existingOps)
	if existing != nil || !errors.Is(existingErr, errManagedProcessIsolation) || existingProbe.setCalls.Load() != 0 || existingProbe.closeCalls.Load() != 1 {
		t.Fatalf("existing job result = job:%v err:%v set:%d close:%d", existing, existingErr, existingProbe.setCalls.Load(), existingProbe.closeCalls.Load())
	}

	invalidProbe := &windowsJobProbe{}
	invalid, invalidErr := newWindowsProcessJobWithOps(recorderJobRecoveryTestNamespace, "https://secret.invalid/?token=private", invalidProbe.ops())
	if invalid != nil || !errors.Is(invalidErr, errManagedProcessConfiguration) || invalidProbe.createCalls.Load() != 0 || strings.Contains(invalidErr.Error(), "secret") {
		t.Fatalf("invalid job result = job:%v err:%v create:%d", invalid, invalidErr, invalidProbe.createCalls.Load())
	}

	missingNamespaceProbe := &windowsJobProbe{}
	missingNamespace, missingNamespaceErr := newWindowsProcessJobWithOps(
		"", recorderJobRecoveryTestAttemptID, missingNamespaceProbe.ops(),
	)
	if missingNamespace != nil || !errors.Is(missingNamespaceErr, errManagedProcessConfiguration) ||
		missingNamespaceProbe.createCalls.Load() != 0 {
		t.Fatalf("missing namespace result = job:%v err:%v create:%d", missingNamespace, missingNamespaceErr, missingNamespaceProbe.createCalls.Load())
	}
}

func TestWindowsGlobalJobHolderHelper(t *testing.T) {
	if os.Getenv(windowsGlobalJobHolderHelperEnv) != "1" {
		return
	}
	jobName := os.Getenv(windowsGlobalJobHolderNameEnv)
	handle, fresh, err := createWindowsJobObject(jobName)
	if err != nil || handle == 0 || !fresh {
		t.Fatalf("create Global Job = (%v, %t, %v)", handle, fresh, err)
	}
	defer func() {
		if closeErr := windows.CloseHandle(handle); closeErr != nil {
			t.Errorf("close Global Job: %v", closeErr)
		}
	}()
	if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "release" {
		t.Fatalf("holder release = (%q, %v)", line, err)
	}
}

func TestWindowsGlobalJobCanBeReopenedAcrossProcesses(t *testing.T) {
	attemptID, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	jobName, valid := recorderAttemptJobName(
		recorderJobRecoveryTestNamespace, attemptID.String(),
	)
	if !valid {
		t.Fatal("derive cross-process Global Job name")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWindowsGlobalJobHolderHelper$")
	command.Env = append(
		os.Environ(),
		windowsGlobalJobHolderHelperEnv+"=1",
		windowsGlobalJobHolderNameEnv+"="+jobName,
	)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	released := false
	waited := false
	t.Cleanup(func() {
		if !released {
			_, _ = io.WriteString(stdin, "release\n")
			_ = stdin.Close()
		}
		if !waited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})

	ready, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(ready) != "ready" {
		t.Fatalf("Global Job holder handshake = (%q, %v)", ready, err)
	}

	existingHandle, fresh, err := createWindowsJobObject(jobName)
	if err != nil || existingHandle == 0 || fresh {
		t.Fatalf("cross-process CreateJobObject = (%v, %t, %v), want existing", existingHandle, fresh, err)
	}
	if err := windows.CloseHandle(existingHandle); err != nil {
		t.Fatal(err)
	}

	reopenedHandle, found, err := openWindowsJobObject(jobName)
	if err != nil || reopenedHandle == 0 || !found {
		t.Fatalf("cross-process OpenJobObject = (%v, %t, %v)", reopenedHandle, found, err)
	}
	if err := windows.CloseHandle(reopenedHandle); err != nil {
		t.Fatal(err)
	}

	existingJob, existingErr := newWindowsProcessJobWithOps(
		recorderJobRecoveryTestNamespace, attemptID.String(), defaultWindowsJobOps(),
	)
	if existingJob != nil || !errors.Is(existingErr, errManagedProcessIsolation) ||
		existingErr.Error() != errManagedProcessIsolation.Error() {
		t.Fatalf("existing Global Job admission = (%v, %v)", existingJob, existingErr)
	}

	if _, err := io.WriteString(stdin, "release\n"); err != nil {
		t.Fatal(err)
	}
	released = true
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	waitErr := command.Wait()
	waited = true
	if waitErr != nil {
		t.Fatal(waitErr)
	}

	postHandle, postFound, postErr := openWindowsJobObject(jobName)
	if postHandle != 0 {
		_ = windows.CloseHandle(postHandle)
	}
	if postErr != nil || postFound || postHandle != 0 {
		t.Fatalf("released Global Job = (%v, %t, %v), want absent", postHandle, postFound, postErr)
	}
}

func TestWindowsInspectRecorderAttemptProcessTerminatesRealNamedJob(t *testing.T) {
	attemptID, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	jobRoot := t.TempDir()
	jobNamespace, valid := recorderJobNamespace(jobRoot)
	if !valid {
		t.Fatal("derive real recorder Job namespace")
	}
	markerPath := filepath.Join(jobRoot, "recovery.marker")
	process, streams, err := startManagedProcess(context.Background(), processConfig{
		Path:                 os.Args[0],
		Args:                 []string{"-test.run=TestWindowsJobTreeHelper"},
		Env:                  append(windowsJobTreeHelperEnvironment("marker"), windowsJobTreeMarkerEnv+"="+markerPath),
		RecorderJobNamespace: jobNamespace,
		RecorderAttemptID:    attemptID.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var drains sync.WaitGroup
	drains.Add(2)
	go func() {
		defer drains.Done()
		defer streams.Stdout.Close()
		_, _ = io.Copy(io.Discard, streams.Stdout)
	}()
	go func() {
		defer drains.Done()
		defer streams.Stderr.Close()
		_, _ = io.Copy(io.Discard, streams.Stderr)
	}()
	t.Cleanup(func() {
		_ = process.terminateTree()
		_ = process.close()
	})
	appeared, markerErr := waitWindowsMarker(markerPath, 3*time.Second)
	if markerErr != nil || !appeared {
		t.Fatal(errWindowsJobTreeMarker)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	result, recoveryErr := inspectRecorderAttemptProcess(ctx, jobNamespace, attemptID.String())
	cancel()
	if recoveryErr != nil || !result.Found || !result.Terminated || result.Status != RecorderProcessRecoveryTerminated {
		t.Fatalf("real recovery result = (%#v, %v)", result, recoveryErr)
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	waitErr := process.wait(waitCtx)
	waitCancel()
	drains.Wait()
	if waitErr != nil && !errors.Is(waitErr, errManagedProcessWait) {
		t.Fatal(waitErr)
	}
	clean, cleanErr := inspectRecorderAttemptProcess(context.Background(), jobNamespace, attemptID.String())
	if cleanErr != nil || clean.Found || clean.Terminated || clean.Status != RecorderProcessRecoveryClean {
		t.Fatalf("post-recovery result = (%#v, %v)", clean, cleanErr)
	}
}

func assertRecorderRecoveryOutputIsStable(t *testing.T, result RecorderProcessRecoveryResult, err error) {
	t.Helper()
	encoded, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	rendered := fmt.Sprintf("%s %v", encoded, err)
	for _, forbidden := range []string{"secret.invalid", "token=", "private", "http://", "https://", `C:\`, `D:\`} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("recovery output leaked %q: %s", forbidden, rendered)
		}
	}
}
