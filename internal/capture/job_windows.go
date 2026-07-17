//go:build windows

package capture

import (
	"errors"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var ntResumeProcessProcedure = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

type windowsJobOps struct {
	create    func() (windows.Handle, error)
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
		create: func() (windows.Handle, error) {
			return windows.CreateJobObject(nil, nil)
		},
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

func newPlatformProcessJob() (processJob, error) {
	return newWindowsProcessJobWithOps(defaultWindowsJobOps())
}

func newWindowsProcessJobWithOps(ops windowsJobOps) (processJob, error) {
	if ops.create == nil || ops.setLimits == nil || ops.assign == nil || ops.terminate == nil || ops.close == nil {
		return nil, errManagedProcessConfiguration
	}
	handle, err := ops.create()
	if err != nil || handle == 0 {
		return nil, errManagedProcessIsolation
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
