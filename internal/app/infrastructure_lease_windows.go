//go:build windows

package app

import (
	"errors"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	createApplicationInstanceEventProcedure  = windows.NewLazySystemDLL("kernel32.dll").NewProc("CreateEventW")
	setApplicationInstanceLastErrorProcedure = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetLastError")
)

type windowsApplicationInstanceLease struct {
	mu       sync.Mutex
	handle   windows.Handle
	closed   bool
	closeErr error
}

func acquireApplicationInstanceLease(dataRoot string) (applicationInstanceLease, error) {
	name, err := applicationInstanceLeaseName(dataRoot)
	if err != nil {
		return nil, ErrApplicationInstanceLease
	}
	handle, created, err := createApplicationInstanceEvent(name)
	if err != nil {
		return nil, ErrApplicationInstanceLease
	}
	if !created {
		_ = windows.CloseHandle(handle)
		return nil, ErrApplicationInstanceActive
	}
	return &windowsApplicationInstanceLease{handle: handle}, nil
}

func createApplicationInstanceEvent(name string) (windows.Handle, bool, error) {
	namePointer, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, false, err
	}
	if err := createApplicationInstanceEventProcedure.Find(); err != nil {
		return 0, false, err
	}
	if err := setApplicationInstanceLastErrorProcedure.Find(); err != nil {
		return 0, false, err
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	_, _, _ = setApplicationInstanceLastErrorProcedure.Call(0)
	result, _, callErr := createApplicationInstanceEventProcedure.Call(
		0,
		0,
		0,
		uintptr(unsafe.Pointer(namePointer)),
	)
	if result == 0 {
		if callErr == nil || errors.Is(callErr, syscall.Errno(0)) {
			callErr = syscall.EINVAL
		}
		return 0, false, callErr
	}
	handle := windows.Handle(result)
	if errors.Is(callErr, windows.ERROR_ALREADY_EXISTS) {
		return handle, false, nil
	}
	if callErr != nil && !errors.Is(callErr, syscall.Errno(0)) {
		_ = windows.CloseHandle(handle)
		return 0, false, callErr
	}
	return handle, true, nil
}

func (lease *windowsApplicationInstanceLease) Close() error {
	if lease == nil {
		return nil
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.closed {
		return lease.closeErr
	}
	lease.closed = true
	handle := lease.handle
	lease.handle = 0
	if handle == 0 {
		return nil
	}
	if err := windows.CloseHandle(handle); err != nil {
		lease.closeErr = ErrApplicationInstanceLease
	}
	return lease.closeErr
}
