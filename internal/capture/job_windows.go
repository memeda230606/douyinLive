//go:build windows

package capture

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	ntResumeProcessProcedure = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")
	createJobObjectProcedure = windows.NewLazySystemDLL("kernel32.dll").NewProc("CreateJobObjectW")
	openJobObjectProcedure   = windows.NewLazySystemDLL("kernel32.dll").NewProc("OpenJobObjectW")
	setLastErrorProcedure    = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetLastError")
)

const (
	windowsJobObjectQueryAccess         = 0x0004
	windowsJobObjectTerminateAccess     = 0x0008
	recorderProcessRecoveryPollInterval = 10 * time.Millisecond
	recorderProcessRecoveryMaximumPolls = 300
)

type windowsJobBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

type windowsJobRecoveryOps struct {
	open        func(string) (windows.Handle, bool, error)
	queryActive func(windows.Handle) (uint32, error)
	terminate   func(windows.Handle, uint32) error
	close       func(windows.Handle) error
	wait        func(context.Context, time.Duration) error
}

type windowsJobCreateProvider struct {
	create func() (windows.Handle, error)
	close  func(windows.Handle) error
}

type windowsJobOps struct {
	create    func(string) (windows.Handle, bool, error)
	setLimits func(windows.Handle, *windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION) error
	assign    func(windows.Handle, windows.Handle) error
	terminate func(windows.Handle, uint32) error
	close     func(windows.Handle) error
}

type windowsProcessJob struct {
	handle windows.Handle
	ops    windowsJobOps

	mu       sync.Mutex
	closed   bool
	closeErr error
}

func defaultWindowsJobOps() windowsJobOps {
	return windowsJobOps{
		create: createWindowsJobObject,
		setLimits: func(job windows.Handle, limits *windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION) error {
			result, err := windows.SetInformationJobObject(
				job,
				windows.JobObjectExtendedLimitInformation,
				uintptr(unsafe.Pointer(limits)),
				uint32(unsafe.Sizeof(*limits)),
			)
			if err != nil {
				return err
			}
			if result == 0 {
				return errors.New("set job limits returned false")
			}
			return nil
		},
		assign: windows.AssignProcessToJobObject,
		terminate: func(job windows.Handle, exitCode uint32) error {
			return windows.TerminateJobObject(job, exitCode)
		},
		close: windows.CloseHandle,
	}
}

func defaultWindowsJobRecoveryOps() windowsJobRecoveryOps {
	return windowsJobRecoveryOps{
		open:        openWindowsJobObject,
		queryActive: queryWindowsJobActiveProcesses,
		terminate:   windows.TerminateJobObject,
		close:       windows.CloseHandle,
		wait: func(ctx context.Context, delay time.Duration) error {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	}
}

func createWindowsJobObject(name string) (windows.Handle, bool, error) {
	var namePointer *uint16
	if name != "" {
		var err error
		namePointer, err = windows.UTF16PtrFromString(name)
		if err != nil {
			return 0, false, err
		}
	}
	if err := createJobObjectProcedure.Find(); err != nil {
		return 0, false, err
	}
	if err := setLastErrorProcedure.Find(); err != nil {
		return 0, false, err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	return createWindowsJobObjectWithProvider(windowsJobCreateProvider{
		create: func() (windows.Handle, error) {
			_, _, _ = setLastErrorProcedure.Call(0)
			result, _, callErr := createJobObjectProcedure.Call(
				0,
				uintptr(unsafe.Pointer(namePointer)),
			)
			return windows.Handle(result), callErr
		},
		close: windows.CloseHandle,
	})
}

func createWindowsJobObjectWithProvider(
	provider windowsJobCreateProvider,
) (windows.Handle, bool, error) {
	if provider.create == nil || provider.close == nil {
		return 0, false, errManagedProcessConfiguration
	}
	handle, callErr := provider.create()
	if handle == 0 {
		return 0, false, errManagedProcessIsolation
	}
	if errors.Is(callErr, windows.ERROR_ALREADY_EXISTS) {
		return handle, false, nil
	}
	if callErr == nil || errors.Is(callErr, syscall.Errno(0)) {
		return handle, true, nil
	}
	closeErr := maskedProcessError(provider.close(handle), errManagedProcessCleanup)
	return 0, false, errors.Join(errManagedProcessIsolation, closeErr)
}

func openWindowsJobObject(name string) (windows.Handle, bool, error) {
	namePointer, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, false, err
	}
	if err := openJobObjectProcedure.Find(); err != nil {
		return 0, false, err
	}
	result, _, callErr := openJobObjectProcedure.Call(
		uintptr(windowsJobObjectQueryAccess|windowsJobObjectTerminateAccess),
		0,
		uintptr(unsafe.Pointer(namePointer)),
	)
	if result == 0 {
		if errors.Is(callErr, windows.ERROR_FILE_NOT_FOUND) {
			return 0, false, nil
		}
		if callErr == nil || errors.Is(callErr, syscall.Errno(0)) {
			callErr = syscall.EINVAL
		}
		return 0, false, callErr
	}
	return windows.Handle(result), true, nil
}

func queryWindowsJobActiveProcesses(handle windows.Handle) (uint32, error) {
	information := windowsJobBasicAccountingInformation{}
	var returnedLength uint32
	err := windows.QueryInformationJobObject(
		handle,
		windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&information)),
		uint32(unsafe.Sizeof(information)),
		&returnedLength,
	)
	if err != nil {
		return 0, err
	}
	return information.ActiveProcesses, nil
}

func (c *execProcessCommand) configure() error {
	if c == nil || c.command == nil {
		return errManagedProcessConfiguration
	}
	if c.command.SysProcAttr == nil {
		c.command.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.command.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	return nil
}

func (c *execProcessCommand) resume() error {
	if err := ntResumeProcessProcedure.Find(); err != nil {
		return errManagedProcessResume
	}
	return c.withHandle(func(processHandle uintptr) error {
		status, _, _ := ntResumeProcessProcedure.Call(processHandle)
		if status != 0 {
			return errManagedProcessResume
		}
		return nil
	})
}

func newPlatformProcessJob(jobNamespace, attemptID string) (processJob, error) {
	return newWindowsProcessJobWithOps(jobNamespace, attemptID, defaultWindowsJobOps())
}

func recoverPlatformRecorderAttemptProcess(ctx context.Context, jobName string) (RecorderProcessRecoveryResult, error) {
	return recoverWindowsRecorderAttemptProcessWithOps(ctx, jobName, defaultWindowsJobRecoveryOps())
}

func recoverWindowsRecorderAttemptProcessWithOps(
	ctx context.Context,
	jobName string,
	ops windowsJobRecoveryOps,
) (result RecorderProcessRecoveryResult, resultErr error) {
	if ctx == nil {
		return recorderProcessRecoveryFailure(false, false, RecorderProcessRecoveryContextErrorCode, errRecorderProcessRecoveryContext)
	}
	if ctx.Err() != nil {
		return recorderProcessRecoveryFailure(false, false, RecorderProcessRecoveryInterruptedErrorCode, errRecorderProcessRecoveryInterrupted)
	}
	if jobName == "" || ops.open == nil || ops.queryActive == nil || ops.terminate == nil || ops.close == nil || ops.wait == nil {
		return recorderProcessRecoveryFailure(false, false, RecorderProcessRecoveryOpenErrorCode, errRecorderProcessRecoveryOpen)
	}

	handle, found, openErr := ops.open(jobName)
	if openErr != nil || (found && handle == 0) || (!found && handle != 0) {
		if handle != 0 {
			_ = ops.close(handle)
		}
		return recorderProcessRecoveryFailure(false, false, RecorderProcessRecoveryOpenErrorCode, errRecorderProcessRecoveryOpen)
	}
	if !found {
		return RecorderProcessRecoveryResult{Status: RecorderProcessRecoveryClean}, nil
	}

	result = RecorderProcessRecoveryResult{Found: true, Status: RecorderProcessRecoveryClean}
	defer func() {
		if closeErr := ops.close(handle); closeErr != nil && resultErr == nil {
			result, resultErr = recorderProcessRecoveryFailure(
				true, result.Terminated,
				RecorderProcessRecoveryCloseErrorCode,
				errRecorderProcessRecoveryClose,
			)
		}
	}()

	activeProcesses, queryErr := ops.queryActive(handle)
	if queryErr != nil {
		return recorderProcessRecoveryFailure(true, false, RecorderProcessRecoveryQueryErrorCode, errRecorderProcessRecoveryQuery)
	}
	if activeProcesses == 0 {
		return result, nil
	}
	if terminateErr := ops.terminate(handle, managedProcessTerminateExitCode); terminateErr != nil {
		return recorderProcessRecoveryFailure(true, false, RecorderProcessRecoveryTerminateErrorCode, errRecorderProcessRecoveryTerminate)
	}

	for poll := 0; ; poll++ {
		activeProcesses, queryErr = ops.queryActive(handle)
		if queryErr != nil {
			return recorderProcessRecoveryFailure(true, false, RecorderProcessRecoveryQueryErrorCode, errRecorderProcessRecoveryQuery)
		}
		if activeProcesses == 0 {
			result.Terminated = true
			result.Status = RecorderProcessRecoveryTerminated
			return result, nil
		}
		if poll >= recorderProcessRecoveryMaximumPolls {
			return recorderProcessRecoveryFailure(true, false, RecorderProcessRecoveryIncompleteErrorCode, errRecorderProcessRecoveryIncomplete)
		}
		if waitErr := ops.wait(ctx, recorderProcessRecoveryPollInterval); waitErr != nil {
			return recorderProcessRecoveryFailure(true, false, RecorderProcessRecoveryInterruptedErrorCode, errRecorderProcessRecoveryInterrupted)
		}
	}
}

func newWindowsProcessJobWithOps(
	jobNamespace string,
	attemptID string,
	ops windowsJobOps,
) (processJob, error) {
	if ops.create == nil || ops.setLimits == nil || ops.assign == nil || ops.terminate == nil || ops.close == nil {
		return nil, errManagedProcessConfiguration
	}
	jobName := ""
	if attemptID == "" {
		if jobNamespace != "" {
			return nil, errManagedProcessConfiguration
		}
	} else {
		var valid bool
		jobName, valid = recorderAttemptJobName(jobNamespace, attemptID)
		if !valid {
			return nil, errManagedProcessConfiguration
		}
	}
	handle, fresh, err := ops.create(jobName)
	if err != nil || handle == 0 || !fresh {
		createErr := error(errManagedProcessIsolation)
		if errors.Is(err, errManagedProcessCleanup) {
			createErr = errors.Join(createErr, errManagedProcessCleanup)
		}
		var closeErr error
		if handle != 0 {
			closeErr = maskedProcessError(ops.close(handle), errManagedProcessCleanup)
		}
		return nil, errors.Join(createErr, closeErr)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if err := ops.setLimits(handle, &limits); err != nil {
		closeErr := maskedProcessError(ops.close(handle), errManagedProcessCleanup)
		return nil, errors.Join(errManagedProcessIsolation, closeErr)
	}
	return &windowsProcessJob{handle: handle, ops: ops}, nil
}

func (j *windowsProcessJob) assign(command processCommand) error {
	if command == nil {
		return errManagedProcessIsolation
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return errManagedProcessIsolation
	}
	err := command.withHandle(func(processHandle uintptr) error {
		return j.ops.assign(j.handle, windows.Handle(processHandle))
	})
	return maskedProcessError(err, errManagedProcessIsolation)
}

func (j *windowsProcessJob) terminate(exitCode uint32) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	return maskedProcessError(j.ops.terminate(j.handle, exitCode), errManagedProcessControl)
}

func (j *windowsProcessJob) close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return j.closeErr
	}
	j.closed = true
	j.closeErr = maskedProcessError(j.ops.close(j.handle), errManagedProcessCleanup)
	return j.closeErr
}
