//go:build windows && p3accacceptance

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	p3ACCLauncherTestRoleEnvironment       = "P3ACC_LAUNCHER_TEST_ROLE"
	p3ACCLauncherTestHelperEnvironment     = "P3ACC_LAUNCHER_TEST_HELPER_MODE"
	p3ACCLauncherTestMarkerEnvironment     = "P3ACC_LAUNCHER_TEST_MARKER"
	p3ACCLauncherTestAckEnvironment        = "P3ACC_LAUNCHER_TEST_ACK"
	p3ACCLauncherTestInnerEnvironment      = "P3ACC_LAUNCHER_TEST_INNER_JOB"
	p3ACCLauncherTestDiagnosticEnvironment = "P3ACC_LAUNCHER_TEST_DIAGNOSTIC"
)

var (
	p3ACCLauncherTestCreateJobProcedure = windows.NewLazySystemDLL("kernel32.dll").NewProc("CreateJobObjectW")
	p3ACCLauncherTestSetErrorProcedure  = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetLastError")
)

type p3ACCLauncherTestIdentity struct {
	ProcessID         uint32 `json:"processId"`
	StartedAtUTCTicks int64  `json:"startedAtUtcTicks"`
}

type p3ACCLauncherTestMarker struct {
	Intermediary   p3ACCLauncherTestIdentity `json:"intermediary"`
	Child          p3ACCLauncherTestIdentity `json:"child"`
	LiveURLPresent bool                      `json:"liveUrlPresent"`
}

func TestMain(m *testing.M) {
	base := strings.ToLower(filepath.Base(os.Args[0]))
	if strings.HasPrefix(base, "p3acc-launcher-runner-") {
		arguments := os.Args[1:]
		if len(arguments) > 0 && arguments[0] == "--" {
			arguments = arguments[1:]
		}
		diagnosticPath := os.Getenv(p3ACCLauncherTestDiagnosticEnvironment)
		ops := defaultP3ACCLauncherOps()
		diagnostic := &p3ACCLauncherTestDiagnostic{}
		if diagnosticPath != "" {
			ops = diagnostic.instrument(ops)
		}
		err := runP3ACCLauncher(arguments, ops)
		if diagnosticPath != "" {
			payload := p3ACCLauncherTestStableError(err) + ";" + strings.Join(diagnostic.stages, ",")
			_ = os.WriteFile(diagnosticPath, []byte(payload), 0o600)
		}
		if err == nil {
			os.Exit(0)
		}
		if errors.Is(err, errP3ACCLauncherConfiguration) {
			os.Exit(2)
		}
		os.Exit(3)
	}
	if strings.HasPrefix(base, "p3acc-helper-") {
		os.Exit(runP3ACCLauncherTestHelper())
	}
	if os.Getenv(p3ACCLauncherTestRoleEnvironment) == "launcher" {
		arguments := os.Args[1:]
		if len(arguments) > 0 && arguments[0] == "--" {
			arguments = arguments[1:]
		}
		os.Exit(p3ACCLauncherExitCode(arguments, defaultP3ACCLauncherOps()))
	}
	os.Exit(m.Run())
}

func p3ACCLauncherTestStableError(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, errP3ACCLauncherConfiguration):
		return "configuration"
	case errors.Is(err, errP3ACCLauncherIsolation):
		return "isolation"
	case errors.Is(err, errP3ACCLauncherStart):
		return "start"
	case errors.Is(err, errP3ACCLauncherResume):
		return "resume"
	case errors.Is(err, errP3ACCLauncherHandshake):
		return "handshake"
	case errors.Is(err, errP3ACCLauncherCleanup):
		return "cleanup"
	default:
		return "unknown"
	}
}

type p3ACCLauncherTestDiagnostic struct {
	stages []string
}

func (diagnostic *p3ACCLauncherTestDiagnostic) add(stage, result string) {
	diagnostic.stages = append(diagnostic.stages, stage+"="+result)
}

func p3ACCLauncherTestNativeError(err error) string {
	if err == nil || errors.Is(err, syscall.Errno(0)) {
		return "ok"
	}
	errno := syscall.Errno(0)
	if errors.As(err, &errno) {
		return strconv.FormatUint(uint64(errno), 10)
	}
	return "other"
}

func (diagnostic *p3ACCLauncherTestDiagnostic) instrument(base p3ACCLauncherOps) p3ACCLauncherOps {
	current := windows.CurrentProcess()
	if p3ACCIsProcessInJobProcedure.Find() != nil {
		diagnostic.add("preAnyJob", "proc-missing")
	} else {
		contained := int32(0)
		result, _, callErr := p3ACCIsProcessInJobProcedure.Call(
			uintptr(current), 0, uintptr(unsafe.Pointer(&contained)),
		)
		if result == 0 {
			diagnostic.add("preAnyJob", "error:"+p3ACCLauncherTestNativeError(callErr))
		} else if contained != 0 {
			diagnostic.add("preAnyJob", "yes")
		} else {
			diagnostic.add("preAnyJob", "no")
		}
	}

	base.openJob = func(name string) (windows.Handle, error) {
		namePointer, err := windows.UTF16PtrFromString(name)
		if err != nil || p3ACCOpenJobObjectProcedure.Find() != nil {
			diagnostic.add("openJob", "setup")
			return 0, errP3ACCLauncherIsolation
		}
		result, _, callErr := p3ACCOpenJobObjectProcedure.Call(
			p3ACCJobObjectAssignProcessAccess|p3ACCJobObjectQueryAccess|p3ACCJobObjectTerminateAccess,
			0, uintptr(unsafe.Pointer(namePointer)),
		)
		if result == 0 {
			diagnostic.add("openJob", "error:"+p3ACCLauncherTestNativeError(callErr))
			for _, probe := range []struct {
				name   string
				access uintptr
			}{
				{name: "openQuery", access: p3ACCJobObjectQueryAccess},
				{name: "openAssign", access: p3ACCJobObjectAssignProcessAccess},
				{name: "openTerminate", access: p3ACCJobObjectTerminateAccess},
			} {
				probeResult, _, probeErr := p3ACCOpenJobObjectProcedure.Call(
					probe.access, 0, uintptr(unsafe.Pointer(namePointer)),
				)
				if probeResult == 0 {
					diagnostic.add(probe.name, "error:"+p3ACCLauncherTestNativeError(probeErr))
				} else {
					diagnostic.add(probe.name, "ok")
					_ = windows.CloseHandle(windows.Handle(probeResult))
				}
			}
			return 0, errP3ACCLauncherIsolation
		}
		diagnostic.add("openJob", "ok")
		return windows.Handle(result), nil
	}
	originalAssign := base.assignProcess
	base.assignProcess = func(job, process windows.Handle) error {
		err := originalAssign(job, process)
		diagnostic.add("assignSelf", p3ACCLauncherTestNativeError(err))
		return err
	}
	originalInJob := base.isProcessInJob
	base.isProcessInJob = func(process, job windows.Handle) (bool, error) {
		contained, err := originalInJob(process, job)
		stage := "childInJob"
		if process == current {
			stage = "selfInJob"
		}
		if err != nil {
			diagnostic.add(stage, "error:"+p3ACCLauncherTestNativeError(err))
		} else {
			diagnostic.add(stage, strconv.FormatBool(contained))
		}
		return contained, err
	}
	originalLimits := base.jobLimitFlags
	base.jobLimitFlags = func(job windows.Handle) (uint32, error) {
		flags, err := originalLimits(job)
		if err != nil {
			diagnostic.add("jobLimits", "error:"+p3ACCLauncherTestNativeError(err))
		} else {
			diagnostic.add("jobLimits", strconv.FormatUint(uint64(flags), 10))
		}
		return flags, err
	}
	originalStart := base.startSuspended
	base.startSuspended = func(configuration p3ACCLauncherConfiguration) (p3ACCLauncherProcess, error) {
		process, err := originalStart(configuration)
		diagnostic.add("startSuspended", p3ACCLauncherTestNativeError(err))
		return process, err
	}
	return base
}

func runP3ACCLauncherTestHelper() int {
	switch os.Getenv(p3ACCLauncherTestHelperEnvironment) {
	case "sleep":
		for {
			time.Sleep(time.Hour)
		}
	case "topology-app":
		return runP3ACCLauncherTopologyApp()
	case "intermediary":
		return runP3ACCLauncherIntermediary()
	case "nested-app":
		return runP3ACCLauncherNestedApp()
	default:
		return 1
	}
}

func runP3ACCLauncherTopologyApp() int {
	executable, err := os.Executable()
	if err != nil {
		return 1
	}
	command := exec.Command(executable)
	command.Env = p3ACCLauncherTestEnvironment(os.Environ(), map[string]string{
		p3ACCLauncherTestHelperEnvironment: "intermediary",
	})
	if command.Start() != nil || command.Process.Release() != nil {
		return 1
	}
	for {
		time.Sleep(time.Hour)
	}
}

func runP3ACCLauncherIntermediary() int {
	executable, err := os.Executable()
	if err != nil {
		return 1
	}
	command := exec.Command(executable)
	command.Env = p3ACCLauncherTestEnvironment(os.Environ(), map[string]string{
		p3ACCLauncherTestHelperEnvironment: "sleep",
	})
	if err := command.Start(); err != nil {
		return 1
	}
	childIdentity, err := p3ACCLauncherTestOSProcessIdentity(command.Process)
	if err != nil {
		_ = command.Process.Kill()
		return 1
	}
	selfTicks, err := p3ACCLauncherProcessStartTicks(windows.CurrentProcess())
	if err != nil {
		_ = command.Process.Kill()
		return 1
	}
	marker := p3ACCLauncherTestMarker{
		Intermediary: p3ACCLauncherTestIdentity{
			ProcessID: windows.GetCurrentProcessId(), StartedAtUTCTicks: selfTicks,
		},
		Child: childIdentity, LiveURLPresent: os.Getenv("P3ACC_LIVE_URL") != "",
	}
	if err := p3ACCLauncherTestWriteMarker(os.Getenv(p3ACCLauncherTestMarkerEnvironment), marker); err != nil {
		_ = command.Process.Kill()
		return 1
	}
	if command.Process.Release() != nil {
		return 1
	}
	acknowledgement := os.Getenv(p3ACCLauncherTestAckEnvironment)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(acknowledgement); err == nil {
			return 0
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 1
}

func runP3ACCLauncherNestedApp() int {
	innerJob, err := p3ACCLauncherTestCreateJob(os.Getenv(p3ACCLauncherTestInnerEnvironment))
	if err != nil {
		return 1
	}
	executable, err := os.Executable()
	if err != nil {
		_ = windows.CloseHandle(innerJob)
		return 1
	}
	if os.Setenv(p3ACCLauncherTestHelperEnvironment, "sleep") != nil {
		_ = windows.CloseHandle(innerJob)
		return 1
	}
	child, err := startP3ACCLauncherProcessSuspended(p3ACCLauncherConfiguration{
		App: executable, WorkingDir: filepath.Dir(executable),
	})
	if err != nil {
		_ = windows.CloseHandle(innerJob)
		return 1
	}
	failed := true
	defer func() {
		if failed {
			_ = terminateAndWaitP3ACCLauncherProcess(child.Process)
			_ = windows.CloseHandle(child.Thread)
			_ = windows.CloseHandle(child.Process)
			_ = windows.CloseHandle(innerJob)
		}
	}()
	if windows.AssignProcessToJobObject(innerJob, child.Process) != nil {
		return 1
	}
	contained, err := p3ACCLauncherProcessInJob(child.Process, innerJob)
	if err != nil || !contained {
		return 1
	}
	startedAtUTCTicks, err := p3ACCLauncherProcessStartTicks(child.Process)
	if err != nil || resumeP3ACCLauncherProcess(child.Process) != nil {
		return 1
	}
	if windows.CloseHandle(child.Thread) != nil {
		return 1
	}
	child.Thread = 0
	marker := p3ACCLauncherTestMarker{
		Child: p3ACCLauncherTestIdentity{
			ProcessID: child.ProcessID, StartedAtUTCTicks: startedAtUTCTicks,
		},
		LiveURLPresent: os.Getenv("P3ACC_LIVE_URL") != "",
	}
	if p3ACCLauncherTestWriteMarker(os.Getenv(p3ACCLauncherTestMarkerEnvironment), marker) != nil {
		return 1
	}
	if windows.CloseHandle(child.Process) != nil {
		return 1
	}
	child.Process = 0
	failed = false
	for {
		time.Sleep(time.Hour)
	}
}

func p3ACCLauncherTestEnvironment(current []string, replacements map[string]string) []string {
	result := make([]string, 0, len(current)+len(replacements))
	for _, entry := range current {
		name, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		if _, replaced := replacements[name]; replaced {
			continue
		}
		result = append(result, entry)
	}
	for name, value := range replacements {
		result = append(result, name+"="+value)
	}
	return result
}

func p3ACCLauncherTestWriteMarker(path string, marker p3ACCLauncherTestMarker) error {
	if path == "" {
		return errors.New("marker path missing")
	}
	payload, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	written, writeErr := file.Write(payload)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || written != len(payload) || syncErr != nil || closeErr != nil {
		_ = os.Remove(temporary)
		return errors.New("marker write failed")
	}
	from, err := windows.UTF16PtrFromString(temporary)
	if err != nil {
		_ = os.Remove(temporary)
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func p3ACCLauncherTestCreateJob(name string) (windows.Handle, error) {
	return p3ACCLauncherTestCreateJobWithMandatoryLabel(name, "")
}

func p3ACCLauncherTestCreateInteractiveOuterJob(name string) (windows.Handle, error) {
	return p3ACCLauncherTestCreateJobWithMandatoryLabel(name, "S:P(ML;;NW;;;ME)")
}

func p3ACCLauncherTestCreateJobWithMandatoryLabel(name, mandatoryLabel string) (windows.Handle, error) {
	namePointer, err := windows.UTF16PtrFromString(name)
	if err != nil || name == "" || p3ACCLauncherTestCreateJobProcedure.Find() != nil ||
		p3ACCLauncherTestSetErrorProcedure.Find() != nil {
		return 0, errors.New("test job configuration failed")
	}
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || tokenUser == nil || tokenUser.User.Sid == nil {
		return 0, errors.New("test job identity failed")
	}
	userSID := tokenUser.User.Sid.String()
	descriptor, err := windows.SecurityDescriptorFromString(
		"O:" + userSID + "G:" + userSID + "D:P(A;;GA;;;SY)(A;;GA;;;" + userSID + ")" + mandatoryLabel,
	)
	if err != nil || descriptor == nil {
		return 0, errors.New("test job security descriptor failed")
	}
	attributes := windows.SecurityAttributes{
		Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor,
	}
	runtime.LockOSThread()
	_, _, _ = p3ACCLauncherTestSetErrorProcedure.Call(0)
	result, _, callErr := p3ACCLauncherTestCreateJobProcedure.Call(
		uintptr(unsafe.Pointer(&attributes)), uintptr(unsafe.Pointer(namePointer)),
	)
	runtime.UnlockOSThread()
	runtime.KeepAlive(descriptor)
	if result == 0 || errors.Is(callErr, windows.ERROR_ALREADY_EXISTS) ||
		(callErr != nil && !errors.Is(callErr, syscall.Errno(0))) {
		if result != 0 {
			_ = windows.CloseHandle(windows.Handle(result))
		}
		return 0, errors.New("test job create failed")
	}
	job := windows.Handle(result)
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	set, setErr := windows.SetInformationJobObject(
		job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits)),
	)
	if setErr != nil || set == 0 {
		_ = windows.CloseHandle(job)
		return 0, errors.New("test job limits failed")
	}
	return job, nil
}

func p3ACCLauncherTestNonceValue(t *testing.T) string {
	t.Helper()
	value := make([]byte, p3ACCLauncherNonceBytes)
	if _, err := rand.Read(value); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(value)
}

func p3ACCLauncherTestCopyExecutable(t *testing.T, directory, nonce string) string {
	t.Helper()
	sourcePath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	targetPath := filepath.Join(directory, "p3acc-helper-"+nonce+".exe")
	target, err := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		t.Fatal(err)
	}
	if err := target.Sync(); err != nil {
		_ = target.Close()
		t.Fatal(err)
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	return targetPath
}

func p3ACCLauncherTestOSProcessIdentity(process *os.Process) (p3ACCLauncherTestIdentity, error) {
	if process == nil || process.Pid < 1 {
		return p3ACCLauncherTestIdentity{}, errors.New("process identity missing")
	}
	var ticks int64
	var callbackErr error
	withHandleErr := process.WithHandle(func(handle uintptr) {
		ticks, callbackErr = p3ACCLauncherProcessStartTicks(windows.Handle(handle))
	})
	if withHandleErr != nil || callbackErr != nil {
		return p3ACCLauncherTestIdentity{}, errors.New("process identity failed")
	}
	return p3ACCLauncherTestIdentity{ProcessID: uint32(process.Pid), StartedAtUTCTicks: ticks}, nil
}

func p3ACCLauncherTestOpenExactProcess(identity p3ACCLauncherTestIdentity) (windows.Handle, error) {
	if identity.ProcessID < 1 || identity.StartedAtUTCTicks < 1 {
		return 0, errors.New("invalid process identity")
	}
	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE,
		false, identity.ProcessID,
	)
	if err != nil {
		return 0, err
	}
	ticks, err := p3ACCLauncherProcessStartTicks(handle)
	if err != nil || ticks != identity.StartedAtUTCTicks {
		_ = windows.CloseHandle(handle)
		return 0, errors.New("process identity changed")
	}
	return handle, nil
}

func p3ACCLauncherTestWaitMarker(t *testing.T, path string) p3ACCLauncherTestMarker {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		payload, err := os.ReadFile(path)
		if err == nil {
			marker := p3ACCLauncherTestMarker{}
			if json.Unmarshal(payload, &marker) == nil && marker.Child.ProcessID > 0 {
				return marker
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("helper marker timeout")
	return p3ACCLauncherTestMarker{}
}

func p3ACCLauncherTestWaitExited(handle windows.Handle, timeout time.Duration) bool {
	result, err := windows.WaitForSingleObject(handle, uint32(timeout/time.Millisecond))
	return err == nil && result == windows.WAIT_OBJECT_0
}

func p3ACCLauncherTestIsAlive(handle windows.Handle) bool {
	result, err := windows.WaitForSingleObject(handle, 0)
	return err == nil && result == p3ACCWaitTimeout
}

type p3ACCLauncherTestJobAccounting struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

type p3ACCLauncherTestJobProcessIDListHeader struct {
	NumberOfAssignedProcesses uint32
	NumberOfProcessIDsInList  uint32
}

type p3ACCLauncherTestJobDrainObservation struct {
	ActiveProcesses     uint32
	AssignedProcesses   uint32
	ListedProcesses     uint32
	ProcessListComplete bool
}

type p3ACCLauncherTestJobDrainProbe func(windows.Handle) (p3ACCLauncherTestJobDrainObservation, error)

func p3ACCLauncherTestObserveJobDrain(job windows.Handle) (p3ACCLauncherTestJobDrainObservation, error) {
	if job == 0 {
		return p3ACCLauncherTestJobDrainObservation{}, errors.New("test Job drain handle missing")
	}
	accounting := p3ACCLauncherTestJobAccounting{}
	accountingSize := uint32(unsafe.Sizeof(accounting))
	var accountingLength uint32
	if err := windows.QueryInformationJobObject(
		job,
		windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&accounting)),
		accountingSize,
		&accountingLength,
	); err != nil || accountingLength != accountingSize {
		return p3ACCLauncherTestJobDrainObservation{}, fmt.Errorf(
			"query test Job accounting: length=%d/%d err=%w",
			accountingLength, accountingSize, err,
		)
	}

	processListBuffer := make([]uintptr, 16)
	processListSize := uint32(len(processListBuffer)) * uint32(unsafe.Sizeof(processListBuffer[0]))
	var processListLength uint32
	processListErr := windows.QueryInformationJobObject(
		job,
		windows.JobObjectBasicProcessIdList,
		uintptr(unsafe.Pointer(&processListBuffer[0])),
		processListSize,
		&processListLength,
	)
	header := (*p3ACCLauncherTestJobProcessIDListHeader)(unsafe.Pointer(&processListBuffer[0]))
	headerSize := uint32(unsafe.Sizeof(p3ACCLauncherTestJobProcessIDListHeader{}))
	processCapacity := uint32((processListSize - headerSize) / uint32(unsafe.Sizeof(uintptr(0))))
	complete := processListErr == nil &&
		processListLength >= headerSize && processListLength <= processListSize &&
		header.NumberOfAssignedProcesses == header.NumberOfProcessIDsInList &&
		header.NumberOfProcessIDsInList <= processCapacity
	partial := errors.Is(processListErr, windows.ERROR_MORE_DATA) &&
		header.NumberOfAssignedProcesses > header.NumberOfProcessIDsInList &&
		header.NumberOfProcessIDsInList <= processCapacity
	if !complete && !partial {
		return p3ACCLauncherTestJobDrainObservation{}, fmt.Errorf(
			"query test Job process list: assigned=%d listed=%d length=%d/%d err=%w",
			header.NumberOfAssignedProcesses, header.NumberOfProcessIDsInList,
			processListLength, processListSize, processListErr,
		)
	}
	return p3ACCLauncherTestJobDrainObservation{
		ActiveProcesses:     accounting.ActiveProcesses,
		AssignedProcesses:   header.NumberOfAssignedProcesses,
		ListedProcesses:     header.NumberOfProcessIDsInList,
		ProcessListComplete: complete,
	}, nil
}

func p3ACCLauncherTestWaitJobDrained(
	job windows.Handle,
	timeout time.Duration,
	probe p3ACCLauncherTestJobDrainProbe,
) error {
	if job == 0 || timeout <= 0 || probe == nil {
		return errors.New("test Job drain wait configuration failed")
	}
	deadline := time.Now().Add(timeout)
	last := p3ACCLauncherTestJobDrainObservation{}
	for {
		observation, err := probe(job)
		if err != nil {
			return fmt.Errorf("observe test Job drain: %w", err)
		}
		last = observation
		if observation.ActiveProcesses == 0 && observation.ProcessListComplete &&
			observation.AssignedProcesses == 0 && observation.ListedProcesses == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf(
				"test Job did not drain: active=%d assigned=%d listed=%d complete=%t",
				last.ActiveProcesses, last.AssignedProcesses, last.ListedProcesses,
				last.ProcessListComplete,
			)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func p3ACCLauncherTestTerminateAndCloseJob(job *windows.Handle, timeout time.Duration) error {
	if job == nil || timeout <= 0 {
		return errors.New("test job cleanup configuration failed")
	}
	if *job == 0 {
		return nil
	}
	jobHandle := *job
	cleanupErrors := make([]error, 0, 3)
	if err := windows.TerminateJobObject(jobHandle, p3ACCLauncherTerminateCode); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("terminate test Job: %w", err))
	}
	if err := p3ACCLauncherTestWaitJobDrained(
		jobHandle, timeout, p3ACCLauncherTestObserveJobDrain,
	); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	*job = 0
	if err := windows.CloseHandle(jobHandle); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("close test Job: %w", err))
	}
	return errors.Join(cleanupErrors...)
}

func TestP3ACCLauncherTestTerminateAndCloseJobWaitsForActiveProcess(t *testing.T) {
	nonce := p3ACCLauncherTestNonceValue(t)
	root := t.TempDir()
	job, err := p3ACCLauncherTestCreateJob("DouyinLive.P3ACC.Test.Cleanup." + nonce)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if job != 0 {
			if err := p3ACCLauncherTestTerminateAndCloseJob(&job, 10*time.Second); err != nil {
				t.Errorf("cleanup test Job failed: %v", err)
			}
		}
	}()
	helper := p3ACCLauncherTestCopyExecutable(t, root, nonce)
	previousMode, hadMode := os.LookupEnv(p3ACCLauncherTestHelperEnvironment)
	if err := os.Setenv(p3ACCLauncherTestHelperEnvironment, "sleep"); err != nil {
		t.Fatal(err)
	}
	child, startErr := startP3ACCLauncherProcessSuspended(p3ACCLauncherConfiguration{
		App: helper, WorkingDir: root,
	})
	restoreErr := os.Unsetenv(p3ACCLauncherTestHelperEnvironment)
	if hadMode {
		restoreErr = os.Setenv(p3ACCLauncherTestHelperEnvironment, previousMode)
	}
	if startErr != nil || restoreErr != nil {
		if child.Process != 0 {
			_ = terminateAndWaitP3ACCLauncherProcess(child.Process)
		}
		if child.Thread != 0 {
			_ = windows.CloseHandle(child.Thread)
		}
		if child.Process != 0 {
			_ = windows.CloseHandle(child.Process)
		}
		t.Fatal("cleanup helper process start failed")
	}
	childOpen := true
	defer func() {
		if childOpen {
			_ = terminateAndWaitP3ACCLauncherProcess(child.Process)
			if child.Thread != 0 {
				_ = windows.CloseHandle(child.Thread)
			}
			_ = windows.CloseHandle(child.Process)
		}
	}()
	if err := windows.AssignProcessToJobObject(job, child.Process); err != nil {
		t.Fatal(err)
	}
	if err := resumeP3ACCLauncherProcess(child.Process); err != nil {
		t.Fatal(err)
	}
	if err := windows.CloseHandle(child.Thread); err != nil {
		t.Fatal(err)
	}
	child.Thread = 0
	if !p3ACCLauncherTestIsAlive(child.Process) {
		t.Fatal("cleanup helper process exited before Job cleanup")
	}
	observation, err := p3ACCLauncherTestObserveJobDrain(job)
	if err != nil {
		t.Fatal(err)
	}
	if observation.ActiveProcesses == 0 || !observation.ProcessListComplete ||
		observation.AssignedProcesses == 0 || observation.ListedProcesses == 0 {
		t.Fatalf("active test Job observation = %#v", observation)
	}
	if err := p3ACCLauncherTestTerminateAndCloseJob(&job, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if job != 0 {
		t.Fatal("test Job cleanup did not zero the handle")
	}
	if err := p3ACCLauncherTestTerminateAndCloseJob(&job, 10*time.Second); err != nil {
		t.Fatalf("test Job cleanup was not idempotent: %v", err)
	}
	if !p3ACCLauncherTestWaitExited(child.Process, time.Second) {
		t.Fatal("Job cleanup returned before the child exited")
	}
	if err := windows.CloseHandle(child.Process); err != nil {
		t.Fatal(err)
	}
	childOpen = false
}

func TestP3ACCLauncherTestWaitJobDrainedRequiresAccountingAndEmptyProcessList(t *testing.T) {
	observations := []p3ACCLauncherTestJobDrainObservation{
		{ActiveProcesses: 0, AssignedProcesses: 1, ListedProcesses: 0},
		{ActiveProcesses: 0, AssignedProcesses: 1, ListedProcesses: 1, ProcessListComplete: true},
		{ActiveProcesses: 1, AssignedProcesses: 0, ListedProcesses: 0, ProcessListComplete: true},
		{ActiveProcesses: 0, AssignedProcesses: 0, ListedProcesses: 0, ProcessListComplete: true},
	}
	probeCalls := 0
	err := p3ACCLauncherTestWaitJobDrained(
		windows.Handle(1), time.Second,
		func(windows.Handle) (p3ACCLauncherTestJobDrainObservation, error) {
			if probeCalls >= len(observations) {
				return p3ACCLauncherTestJobDrainObservation{}, errors.New("unexpected extra drain probe")
			}
			observation := observations[probeCalls]
			probeCalls++
			return observation, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if probeCalls != len(observations) {
		t.Fatalf("drain probe calls = %d, want %d", probeCalls, len(observations))
	}
}

type p3ACCLauncherTestProcessCleanupOps struct {
	terminate func(windows.Handle, uint32) error
	wait      func(windows.Handle, uint32) (uint32, error)
	close     func(windows.Handle) error
}

func p3ACCLauncherTestTerminateWaitAndCloseProcess(
	process *windows.Handle,
	timeout time.Duration,
	ops p3ACCLauncherTestProcessCleanupOps,
) error {
	if process == nil || timeout <= 0 || timeout/time.Millisecond > time.Duration(^uint32(0)) ||
		ops.terminate == nil || ops.wait == nil || ops.close == nil {
		return errors.New("test process cleanup configuration failed")
	}
	if *process == 0 {
		return nil
	}
	processHandle := *process
	cleanupErrors := make([]error, 0, 3)
	if err := ops.terminate(processHandle, p3ACCLauncherTerminateCode); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("terminate unrelated test process: %w", err))
	}
	waitResult, waitErr := ops.wait(processHandle, uint32(timeout/time.Millisecond))
	if waitErr != nil || waitResult != windows.WAIT_OBJECT_0 {
		cleanupErrors = append(cleanupErrors, fmt.Errorf(
			"wait unrelated test process: result=%d err=%w", waitResult, waitErr,
		))
	}
	*process = 0
	if err := ops.close(processHandle); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("close unrelated test process: %w", err))
	}
	return errors.Join(cleanupErrors...)
}

func p3ACCLauncherTestStrictlyCleanupUnrelatedProcess(process *windows.Handle, timeout time.Duration) error {
	return p3ACCLauncherTestTerminateWaitAndCloseProcess(
		process,
		timeout,
		p3ACCLauncherTestProcessCleanupOps{
			terminate: windows.TerminateProcess,
			wait:      windows.WaitForSingleObject,
			close:     windows.CloseHandle,
		},
	)
}

func p3ACCLauncherTestReleaseOSProcess(process **os.Process) error {
	if process == nil {
		return errors.New("test OS process release configuration failed")
	}
	if *process == nil {
		return nil
	}
	owned := *process
	*process = nil
	if err := owned.Release(); err != nil {
		return fmt.Errorf("release test OS process: %w", err)
	}
	return nil
}

func p3ACCLauncherTestStrictlyCleanupStartedOSProcess(process **os.Process, timeout time.Duration) error {
	if process == nil || timeout <= 0 || timeout/time.Millisecond > time.Duration(^uint32(0)) {
		return errors.New("test OS process cleanup configuration failed")
	}
	if *process == nil {
		return nil
	}
	owned := *process
	cleanupErrors := make([]error, 0, 3)
	var nativeCleanupErr error
	withHandleErr := owned.WithHandle(func(handle uintptr) {
		processHandle := windows.Handle(handle)
		nativeErrors := make([]error, 0, 2)
		if err := windows.TerminateProcess(processHandle, p3ACCLauncherTerminateCode); err != nil {
			nativeErrors = append(nativeErrors, fmt.Errorf("terminate started test process: %w", err))
		}
		waitResult, waitErr := windows.WaitForSingleObject(
			processHandle, uint32(timeout/time.Millisecond),
		)
		if waitErr != nil || waitResult != windows.WAIT_OBJECT_0 {
			nativeErrors = append(nativeErrors, fmt.Errorf(
				"wait started test process: result=%d err=%w", waitResult, waitErr,
			))
		}
		nativeCleanupErr = errors.Join(nativeErrors...)
	})
	if withHandleErr != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("access started test process handle: %w", withHandleErr))
	}
	if nativeCleanupErr != nil {
		cleanupErrors = append(cleanupErrors, nativeCleanupErr)
	}
	if err := p3ACCLauncherTestReleaseOSProcess(process); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	return errors.Join(cleanupErrors...)
}

func TestP3ACCLauncherUnrelatedProcessCleanupPropagatesEveryStageAndIsIdempotent(t *testing.T) {
	terminateErr := errors.New("terminate injected")
	waitErr := errors.New("wait injected")
	closeErr := errors.New("close injected")
	order := make([]string, 0, 3)
	process := windows.Handle(91)
	err := p3ACCLauncherTestTerminateWaitAndCloseProcess(
		&process,
		time.Second,
		p3ACCLauncherTestProcessCleanupOps{
			terminate: func(handle windows.Handle, code uint32) error {
				order = append(order, "terminate")
				return terminateErr
			},
			wait: func(handle windows.Handle, timeout uint32) (uint32, error) {
				order = append(order, "wait")
				return windows.WAIT_FAILED, waitErr
			},
			close: func(handle windows.Handle) error {
				order = append(order, "close")
				return closeErr
			},
		},
	)
	if !errors.Is(err, terminateErr) || !errors.Is(err, waitErr) || !errors.Is(err, closeErr) {
		t.Fatalf("cleanup error did not preserve every stage: %v", err)
	}
	if process != 0 || strings.Join(order, ",") != "terminate,wait,close" {
		t.Fatalf("cleanup state = handle %d, order %q", process, strings.Join(order, ","))
	}
	if err := p3ACCLauncherTestStrictlyCleanupUnrelatedProcess(&process, time.Second); err != nil {
		t.Fatalf("unrelated process cleanup was not idempotent: %v", err)
	}
}

func TestP3ACCLauncherStartedOSProcessCleanupOwnsFailureWindowAndIsIdempotent(t *testing.T) {
	nonce := p3ACCLauncherTestNonceValue(t)
	root := t.TempDir()
	helper := p3ACCLauncherTestCopyExecutable(t, root, nonce)
	command := exec.Command(helper)
	command.Env = p3ACCLauncherTestEnvironment(os.Environ(), map[string]string{
		p3ACCLauncherTestHelperEnvironment: "sleep",
	})
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	process := command.Process
	defer func() {
		if err := p3ACCLauncherTestStrictlyCleanupStartedOSProcess(&process, 10*time.Second); err != nil {
			t.Errorf("deferred started-process cleanup failed: %v", err)
		}
	}()
	if process == nil || process.Pid < 1 {
		t.Fatal("started test process ownership missing")
	}
	if err := p3ACCLauncherTestStrictlyCleanupStartedOSProcess(&process, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if process != nil {
		t.Fatal("started test process cleanup did not release ownership")
	}
	if err := p3ACCLauncherTestStrictlyCleanupStartedOSProcess(&process, 10*time.Second); err != nil {
		t.Fatalf("started test process cleanup was not idempotent: %v", err)
	}
}

func p3ACCLauncherTestStart(
	t *testing.T,
	configuration p3ACCLauncherConfiguration,
	environment map[string]string,
) p3ACCLauncherHandshake {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	arguments := append([]string{"--"}, p3ACCLauncherTestArguments(configuration)...)
	command := exec.Command(executable, arguments...)
	environment[p3ACCLauncherTestRoleEnvironment] = "launcher"
	command.Env = p3ACCLauncherTestEnvironment(os.Environ(), environment)
	command.Dir = configuration.WorkingDir
	stdoutPath := filepath.Join(configuration.WorkingDir, "launcher.stdout")
	stderrPath := filepath.Join(configuration.WorkingDir, "launcher.stderr")
	stdout, err := os.OpenFile(stdoutPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(stderrPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	command.Stdout = stdout
	command.Stderr = stderr
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	launcherIdentity, err := p3ACCLauncherTestOSProcessIdentity(command.Process)
	if err != nil {
		launcherProcess := command.Process
		cleanupErr := p3ACCLauncherTestStrictlyCleanupStartedOSProcess(&launcherProcess, 10*time.Second)
		t.Fatalf("launcher identity failed: %v (cleanup: %v)", err, cleanupErr)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("launcher exit: %v", err)
		}
	case <-time.After(15 * time.Second):
		killErr := command.Process.Kill()
		waitStatus := "wait-complete"
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			waitStatus = "wait-timeout"
		}
		t.Fatalf("launcher exit timeout (kill: %v; %s)", killErr, waitStatus)
	}
	if stdout.Sync() != nil || stderr.Sync() != nil {
		t.Fatal("launcher output sync failed")
	}
	stdoutPayload, stdoutErr := os.ReadFile(stdoutPath)
	stderrPayload, stderrErr := os.ReadFile(stderrPath)
	if stdoutErr != nil || stderrErr != nil || len(stdoutPayload) != 0 || len(stderrPayload) != 0 {
		t.Fatalf("launcher emitted output: %d/%d", len(stdoutPayload), len(stderrPayload))
	}
	payload, err := os.ReadFile(configuration.Handshake)
	if err != nil {
		t.Fatal(err)
	}
	handshake := p3ACCLauncherHandshake{}
	if json.Unmarshal(payload, &handshake) != nil ||
		handshake.Schema != p3ACCLauncherHandshakeSchema ||
		handshake.JobNonce != configuration.JobNonce ||
		handshake.LauncherProcessID != launcherIdentity.ProcessID ||
		handshake.LauncherStartedAtUTCTicks != launcherIdentity.StartedAtUTCTicks ||
		handshake.AppProcessID < 1 || handshake.AppStartedAtUTCTicks < 1 {
		t.Fatalf("invalid launcher handshake: %#v", handshake)
	}
	if bytes.Contains(payload, []byte("P3ACC_LIVE_URL")) || bytes.Contains(payload, []byte("secret.invalid")) {
		t.Fatal("launcher handshake leaked environment")
	}
	return handshake
}

func TestP3ACCLauncherRealJobRetainsDisconnectedGrandchildWithoutTouchingOutsideProcess(t *testing.T) {
	nonce := p3ACCLauncherTestNonceValue(t)
	root := t.TempDir()
	outerName := p3ACCLauncherJobNamePrefix + nonce
	outer, err := p3ACCLauncherTestCreateJob(outerName)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if outer != 0 {
			if err := p3ACCLauncherTestTerminateAndCloseJob(&outer, 10*time.Second); err != nil {
				t.Errorf("outer Job cleanup failed: %v", err)
			}
		}
	}()
	helper := p3ACCLauncherTestCopyExecutable(t, root, nonce)
	markerPath := filepath.Join(root, "topology.json")
	ackPath := filepath.Join(root, "intermediary.ack")
	configuration := p3ACCLauncherConfiguration{
		JobName: outerName, JobNonce: nonce, App: helper,
		WorkingDir: root, Handshake: filepath.Join(root, "handshake.json"),
	}
	handshake := p3ACCLauncherTestStart(t, configuration, map[string]string{
		p3ACCLauncherTestHelperEnvironment: "topology-app",
		p3ACCLauncherTestMarkerEnvironment: markerPath,
		p3ACCLauncherTestAckEnvironment:    ackPath,
		"P3ACC_LIVE_URL":                   "https://secret.invalid/live",
	})
	app, err := p3ACCLauncherTestOpenExactProcess(p3ACCLauncherTestIdentity{
		ProcessID: handshake.AppProcessID, StartedAtUTCTicks: handshake.AppStartedAtUTCTicks,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(app)
	marker := p3ACCLauncherTestWaitMarker(t, markerPath)
	if !marker.LiveURLPresent || marker.Intermediary.ProcessID < 1 {
		t.Fatalf("invalid topology marker: %#v", marker)
	}
	intermediary, err := p3ACCLauncherTestOpenExactProcess(marker.Intermediary)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(intermediary)
	grandchild, err := p3ACCLauncherTestOpenExactProcess(marker.Child)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(grandchild)
	if err := os.WriteFile(ackPath, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !p3ACCLauncherTestWaitExited(intermediary, 5*time.Second) || !p3ACCLauncherTestIsAlive(grandchild) {
		t.Fatal("intermediary did not exit while grandchild remained alive")
	}

	unrelatedCommand := exec.Command(helper)
	unrelatedCommand.Env = p3ACCLauncherTestEnvironment(os.Environ(), map[string]string{
		p3ACCLauncherTestHelperEnvironment: "sleep",
	})
	if err := unrelatedCommand.Start(); err != nil {
		t.Fatal(err)
	}
	unrelatedOSProcess := unrelatedCommand.Process
	var unrelated windows.Handle
	defer func() {
		cleanupErrors := make([]error, 0, 2)
		if unrelated != 0 {
			if err := p3ACCLauncherTestStrictlyCleanupUnrelatedProcess(&unrelated, 10*time.Second); err != nil {
				cleanupErrors = append(cleanupErrors, err)
			}
			if err := p3ACCLauncherTestReleaseOSProcess(&unrelatedOSProcess); err != nil {
				cleanupErrors = append(cleanupErrors, err)
			}
		} else if err := p3ACCLauncherTestStrictlyCleanupStartedOSProcess(
			&unrelatedOSProcess, 10*time.Second,
		); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
		if err := errors.Join(cleanupErrors...); err != nil {
			t.Errorf("unrelated helper cleanup failed: %v", err)
		}
	}()
	unrelatedIdentity, err := p3ACCLauncherTestOSProcessIdentity(unrelatedOSProcess)
	if err != nil {
		t.Fatal(err)
	}
	unrelated, err = p3ACCLauncherTestOpenExactProcess(unrelatedIdentity)
	if err != nil {
		t.Fatal(err)
	}
	for _, process := range []windows.Handle{app, grandchild} {
		contained, err := p3ACCLauncherProcessInJob(process, outer)
		if err != nil || !contained {
			t.Fatal("owned process was outside the outer Job")
		}
	}
	if contained, err := p3ACCLauncherProcessInJob(unrelated, outer); err != nil || contained {
		t.Fatal("unrelated identical executable entered the outer Job")
	}
	if err := windows.CloseHandle(outer); err != nil {
		t.Fatal(err)
	}
	outer = 0
	if !p3ACCLauncherTestWaitExited(app, 5*time.Second) ||
		!p3ACCLauncherTestWaitExited(grandchild, 5*time.Second) {
		t.Fatal("closing the final outer Job handle left owned processes alive")
	}
	if !p3ACCLauncherTestIsAlive(unrelated) {
		t.Fatal("closing the outer Job killed an unrelated identical executable")
	}
}

func TestP3ACCLauncherRealNestedJobTerminatesInnerWithoutKillingOuterApp(t *testing.T) {
	nonce := p3ACCLauncherTestNonceValue(t)
	root := t.TempDir()
	outerName := p3ACCLauncherJobNamePrefix + nonce
	outer, err := p3ACCLauncherTestCreateJob(outerName)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if outer != 0 {
			if err := p3ACCLauncherTestTerminateAndCloseJob(&outer, 10*time.Second); err != nil {
				t.Errorf("outer Job cleanup failed: %v", err)
			}
		}
	}()
	helper := p3ACCLauncherTestCopyExecutable(t, root, nonce)
	markerPath := filepath.Join(root, "nested.json")
	innerName := "DouyinLive.P3ACC.Test.Inner." + nonce
	configuration := p3ACCLauncherConfiguration{
		JobName: outerName, JobNonce: nonce, App: helper,
		WorkingDir: root, Handshake: filepath.Join(root, "handshake.json"),
	}
	handshake := p3ACCLauncherTestStart(t, configuration, map[string]string{
		p3ACCLauncherTestHelperEnvironment: "nested-app",
		p3ACCLauncherTestMarkerEnvironment: markerPath,
		p3ACCLauncherTestInnerEnvironment:  innerName,
		"P3ACC_LIVE_URL":                   "https://secret.invalid/live",
	})
	app, err := p3ACCLauncherTestOpenExactProcess(p3ACCLauncherTestIdentity{
		ProcessID: handshake.AppProcessID, StartedAtUTCTicks: handshake.AppStartedAtUTCTicks,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(app)
	marker := p3ACCLauncherTestWaitMarker(t, markerPath)
	if !marker.LiveURLPresent {
		t.Fatal("nested app did not inherit P3ACC_LIVE_URL")
	}
	child, err := p3ACCLauncherTestOpenExactProcess(marker.Child)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(child)
	inner, err := openP3ACCLauncherJob(innerName)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(inner)
	for _, check := range []struct {
		process windows.Handle
		job     windows.Handle
		want    bool
	}{
		{process: app, job: outer, want: true},
		{process: child, job: outer, want: true},
		{process: child, job: inner, want: true},
		{process: app, job: inner, want: false},
	} {
		contained, err := p3ACCLauncherProcessInJob(check.process, check.job)
		if err != nil || contained != check.want {
			t.Fatalf("nested Job membership = %v, %v", contained, err)
		}
	}
	if err := windows.TerminateJobObject(inner, p3ACCLauncherTerminateCode); err != nil {
		t.Fatal(err)
	}
	if !p3ACCLauncherTestWaitExited(child, 5*time.Second) || !p3ACCLauncherTestIsAlive(app) {
		t.Fatal("inner Job termination did not isolate the nested child")
	}
	if err := windows.CloseHandle(outer); err != nil {
		t.Fatal(err)
	}
	outer = 0
	if !p3ACCLauncherTestWaitExited(app, 5*time.Second) {
		t.Fatal("outer Job close left the app alive")
	}
}
