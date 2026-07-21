//go:build windows && p3accacceptance

package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	p3ACCLauncherHandshakeSchema = "P3ACC-LAUNCHER/v1"
	p3ACCLauncherJobNamePrefix   = `Global\DouyinLive.P3ACC.App.`
	p3ACCLauncherNonceBytes      = 16
	p3ACCLauncherTerminateCode   = 1
	p3ACCLauncherWaitMillis      = 10_000
	p3ACCDotNet1601OffsetTicks   = uint64(504911232000000000)
	p3ACCMaximumInt64            = uint64(1<<63 - 1)

	p3ACCJobObjectAssignProcessAccess = 0x0001
	p3ACCJobObjectQueryAccess         = 0x0004
	p3ACCJobObjectTerminateAccess     = 0x0008
	p3ACCWaitTimeout                  = 0x00000102
	p3ACCStillActive                  = 259
)

var (
	errP3ACCLauncherConfiguration = errors.New("P3ACC_LAUNCHER_CONFIGURATION_INVALID")
	errP3ACCLauncherIsolation     = errors.New("P3ACC_LAUNCHER_ISOLATION_FAILED")
	errP3ACCLauncherStart         = errors.New("P3ACC_LAUNCHER_START_FAILED")
	errP3ACCLauncherResume        = errors.New("P3ACC_LAUNCHER_RESUME_FAILED")
	errP3ACCLauncherHandshake     = errors.New("P3ACC_LAUNCHER_HANDSHAKE_FAILED")
	errP3ACCLauncherCleanup       = errors.New("P3ACC_LAUNCHER_CLEANUP_FAILED")

	p3ACCOpenJobObjectProcedure   = windows.NewLazySystemDLL("kernel32.dll").NewProc("OpenJobObjectW")
	p3ACCIsProcessInJobProcedure  = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")
	p3ACCNtResumeProcessProcedure = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")
)

type p3ACCLauncherConfiguration struct {
	JobName    string
	JobNonce   string
	App        string
	WorkingDir string
	Handshake  string
}

type p3ACCLauncherHandshake struct {
	Schema                    string `json:"schema"`
	JobNonce                  string `json:"jobNonce"`
	LauncherProcessID         uint32 `json:"launcherProcessId"`
	LauncherStartedAtUTCTicks int64  `json:"launcherStartedAtUtcTicks"`
	AppProcessID              uint32 `json:"appProcessId"`
	AppStartedAtUTCTicks      int64  `json:"appStartedAtUtcTicks"`
}

type p3ACCLauncherProcess struct {
	Process   windows.Handle
	Thread    windows.Handle
	ProcessID uint32
}

type p3ACCHandshakeTransaction interface {
	Commit() error
	Abort() error
}

type p3ACCLauncherOps struct {
	openJob           func(string) (windows.Handle, error)
	assignProcess     func(windows.Handle, windows.Handle) error
	isProcessInJob    func(windows.Handle, windows.Handle) (bool, error)
	jobLimitFlags     func(windows.Handle) (uint32, error)
	currentProcess    func() windows.Handle
	currentProcessID  func() uint32
	processStartTicks func(windows.Handle) (int64, error)
	startSuspended    func(p3ACCLauncherConfiguration) (p3ACCLauncherProcess, error)
	resumeProcess     func(windows.Handle) error
	terminateJob      func(windows.Handle, uint32) error
	terminateAndWait  func(windows.Handle) error
	closeHandle       func(windows.Handle) error
	prepareHandshake  func(string, p3ACCLauncherHandshake) (p3ACCHandshakeTransaction, error)
	unsetenv          func(string) error
}

func main() {
	os.Exit(p3ACCLauncherExitCode(os.Args[1:], defaultP3ACCLauncherOps()))
}

func p3ACCLauncherExitCode(arguments []string, ops p3ACCLauncherOps) int {
	err := runP3ACCLauncher(arguments, ops)
	if err == nil {
		return 0
	}
	if errors.Is(err, errP3ACCLauncherConfiguration) {
		return 2
	}
	return 3
}

func runP3ACCLauncher(arguments []string, ops p3ACCLauncherOps) error {
	configuration, err := parseP3ACCLauncherConfiguration(arguments)
	if err != nil {
		if ops.unsetenv != nil {
			_ = ops.unsetenv("P3ACC_LIVE_URL")
		}
		return errP3ACCLauncherConfiguration
	}
	return launchP3ACCApp(configuration, ops)
}

func parseP3ACCLauncherConfiguration(arguments []string) (p3ACCLauncherConfiguration, error) {
	if !strictP3ACCLauncherArguments(arguments) {
		return p3ACCLauncherConfiguration{}, errP3ACCLauncherConfiguration
	}
	flags := flag.NewFlagSet("p3acclauncher", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jobName := flags.String("job-name", "", "")
	jobNonce := flags.String("job-nonce", "", "")
	app := flags.String("app", "", "")
	workingDir := flags.String("working-dir", "", "")
	handshake := flags.String("handshake", "", "")
	if flags.Parse(arguments) != nil || flags.NArg() != 0 {
		return p3ACCLauncherConfiguration{}, errP3ACCLauncherConfiguration
	}
	configuration := p3ACCLauncherConfiguration{
		JobName: *jobName, JobNonce: *jobNonce, App: *app,
		WorkingDir: *workingDir, Handshake: *handshake,
	}
	if !validP3ACCLauncherNonce(configuration.JobNonce) ||
		configuration.JobName != p3ACCLauncherJobNamePrefix+configuration.JobNonce ||
		!p3ACCLauncherAbsoluteCleanPath(configuration.App) ||
		!p3ACCLauncherAbsoluteCleanPath(configuration.WorkingDir) ||
		!p3ACCLauncherAbsoluteCleanPath(configuration.Handshake) {
		return p3ACCLauncherConfiguration{}, errP3ACCLauncherConfiguration
	}
	return configuration, nil
}

func strictP3ACCLauncherArguments(arguments []string) bool {
	expected := map[string]struct{}{
		"--job-name": {}, "--job-nonce": {}, "--app": {},
		"--working-dir": {}, "--handshake": {},
	}
	if len(arguments) != len(expected)*2 {
		return false
	}
	seen := make(map[string]struct{}, len(expected))
	for index := 0; index < len(arguments); index += 2 {
		name := arguments[index]
		if _, valid := expected[name]; !valid {
			return false
		}
		if _, duplicate := seen[name]; duplicate {
			return false
		}
		seen[name] = struct{}{}
	}
	return len(seen) == len(expected)
}

func validP3ACCLauncherNonce(value string) bool {
	if len(value) != p3ACCLauncherNonceBytes*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == p3ACCLauncherNonceBytes
}

func p3ACCLauncherAbsoluteCleanPath(value string) bool {
	return value != "" && filepath.IsAbs(value) && filepath.Clean(value) == value
}

func validateP3ACCLauncherFilesystem(configuration p3ACCLauncherConfiguration) error {
	app, err := os.Stat(configuration.App)
	if err != nil || !app.Mode().IsRegular() {
		return errP3ACCLauncherConfiguration
	}
	working, err := os.Stat(configuration.WorkingDir)
	if err != nil || !working.IsDir() {
		return errP3ACCLauncherConfiguration
	}
	parent, err := os.Stat(filepath.Dir(configuration.Handshake))
	if err != nil || !parent.IsDir() {
		return errP3ACCLauncherConfiguration
	}
	if _, err := os.Lstat(configuration.Handshake); !errors.Is(err, os.ErrNotExist) {
		return errP3ACCLauncherConfiguration
	}
	return nil
}

func validP3ACCLauncherOps(ops p3ACCLauncherOps) bool {
	return ops.openJob != nil && ops.assignProcess != nil && ops.isProcessInJob != nil &&
		ops.jobLimitFlags != nil && ops.currentProcess != nil && ops.currentProcessID != nil &&
		ops.processStartTicks != nil &&
		ops.startSuspended != nil && ops.resumeProcess != nil && ops.terminateAndWait != nil &&
		ops.terminateJob != nil && ops.closeHandle != nil && ops.prepareHandshake != nil && ops.unsetenv != nil
}

func launchP3ACCApp(configuration p3ACCLauncherConfiguration, ops p3ACCLauncherOps) error {
	if !validP3ACCLauncherOps(ops) {
		return errP3ACCLauncherConfiguration
	}
	unset := false
	defer func() {
		if !unset {
			_ = ops.unsetenv("P3ACC_LIVE_URL")
		}
	}()

	job, err := ops.openJob(configuration.JobName)
	if err != nil || job == 0 {
		return errP3ACCLauncherIsolation
	}
	jobOpen := true
	defer func() {
		if jobOpen {
			_ = ops.closeHandle(job)
		}
	}()

	self := ops.currentProcess()
	if self == 0 || ops.assignProcess(job, self) != nil {
		return errP3ACCLauncherIsolation
	}
	contained, err := ops.isProcessInJob(self, job)
	if err != nil || !contained {
		return errP3ACCLauncherIsolation
	}
	limitFlags, err := ops.jobLimitFlags(job)
	forbiddenLimits := uint32(
		windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK | windows.JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK,
	)
	if err != nil || limitFlags&windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE == 0 ||
		limitFlags&forbiddenLimits != 0 {
		return errP3ACCLauncherIsolation
	}
	launcherProcessID := ops.currentProcessID()
	launcherStartedAtUTCTicks, err := ops.processStartTicks(self)
	if err != nil || launcherStartedAtUTCTicks < 1 || launcherProcessID < 1 {
		return errP3ACCLauncherIsolation
	}

	// Filesystem inspection is deliberately after self-assignment. For a valid
	// invocation, no process or file side effect may precede Job containment.
	if err := validateP3ACCLauncherFilesystem(configuration); err != nil {
		return err
	}

	child, err := ops.startSuspended(configuration)
	if err != nil || child.Process == 0 || child.Thread == 0 || child.ProcessID < 1 {
		if child.Process != 0 || child.Thread != 0 {
			return errors.Join(errP3ACCLauncherStart, cleanupP3ACCLauncherChild(job, child, nil, ops))
		}
		return errP3ACCLauncherStart
	}
	childOwned := true
	defer func() {
		if childOwned {
			_ = cleanupP3ACCLauncherChild(job, child, nil, ops)
		}
	}()
	failChild := func(base error, transaction p3ACCHandshakeTransaction) error {
		childOwned = false
		return errors.Join(base, cleanupP3ACCLauncherChild(job, child, transaction, ops))
	}

	contained, err = ops.isProcessInJob(child.Process, job)
	if err != nil || !contained {
		return failChild(errP3ACCLauncherIsolation, nil)
	}
	appStartedAtUTCTicks, err := ops.processStartTicks(child.Process)
	if err != nil || appStartedAtUTCTicks < 1 {
		return failChild(errP3ACCLauncherStart, nil)
	}

	handshake := p3ACCLauncherHandshake{
		Schema: p3ACCLauncherHandshakeSchema, JobNonce: configuration.JobNonce,
		LauncherProcessID: launcherProcessID, LauncherStartedAtUTCTicks: launcherStartedAtUTCTicks,
		AppProcessID: child.ProcessID, AppStartedAtUTCTicks: appStartedAtUTCTicks,
	}
	transaction, err := ops.prepareHandshake(configuration.Handshake, handshake)
	if err != nil || transaction == nil {
		return failChild(errP3ACCLauncherHandshake, transaction)
	}

	if err := ops.unsetenv("P3ACC_LIVE_URL"); err != nil {
		return failChild(errP3ACCLauncherCleanup, transaction)
	}
	unset = true
	if err := ops.resumeProcess(child.Process); err != nil {
		return failChild(errP3ACCLauncherResume, transaction)
	}
	if err := ops.closeHandle(child.Thread); err != nil {
		return failChild(errP3ACCLauncherCleanup, transaction)
	}
	child.Thread = 0
	if err := transaction.Commit(); err != nil {
		return failChild(errP3ACCLauncherHandshake, transaction)
	}

	childOwned = false
	_ = ops.closeHandle(child.Process)
	child.Process = 0
	_ = ops.closeHandle(job)
	jobOpen = false
	return nil
}

func cleanupP3ACCLauncherChild(job windows.Handle, child p3ACCLauncherProcess, transaction p3ACCHandshakeTransaction, ops p3ACCLauncherOps) error {
	failed := false
	if transaction != nil && transaction.Abort() != nil {
		failed = true
	}
	if job == 0 || ops.terminateJob(job, p3ACCLauncherTerminateCode) != nil {
		failed = true
	}
	if child.Process != 0 && ops.terminateAndWait(child.Process) != nil {
		failed = true
	}
	if child.Thread != 0 && ops.closeHandle(child.Thread) != nil {
		failed = true
	}
	if child.Process != 0 && ops.closeHandle(child.Process) != nil {
		failed = true
	}
	if failed {
		return errP3ACCLauncherCleanup
	}
	return nil
}

func defaultP3ACCLauncherOps() p3ACCLauncherOps {
	return p3ACCLauncherOps{
		openJob:           openP3ACCLauncherJob,
		assignProcess:     windows.AssignProcessToJobObject,
		isProcessInJob:    p3ACCLauncherProcessInJob,
		jobLimitFlags:     p3ACCLauncherJobLimitFlags,
		currentProcess:    windows.CurrentProcess,
		currentProcessID:  windows.GetCurrentProcessId,
		processStartTicks: p3ACCLauncherProcessStartTicks,
		startSuspended:    startP3ACCLauncherProcessSuspended,
		resumeProcess:     resumeP3ACCLauncherProcess,
		terminateJob:      windows.TerminateJobObject,
		terminateAndWait:  terminateAndWaitP3ACCLauncherProcess,
		closeHandle:       windows.CloseHandle,
		prepareHandshake:  prepareP3ACCLauncherHandshake,
		unsetenv:          os.Unsetenv,
	}
}

func openP3ACCLauncherJob(name string) (windows.Handle, error) {
	namePointer, err := windows.UTF16PtrFromString(name)
	if err != nil || p3ACCOpenJobObjectProcedure.Find() != nil {
		return 0, errP3ACCLauncherIsolation
	}
	result, _, callErr := p3ACCOpenJobObjectProcedure.Call(
		p3ACCJobObjectAssignProcessAccess|p3ACCJobObjectQueryAccess|p3ACCJobObjectTerminateAccess,
		0,
		uintptr(unsafe.Pointer(namePointer)),
	)
	if result == 0 {
		if callErr == nil || errors.Is(callErr, syscall.Errno(0)) {
			return 0, errP3ACCLauncherIsolation
		}
		return 0, errP3ACCLauncherIsolation
	}
	return windows.Handle(result), nil
}

func p3ACCLauncherProcessInJob(process, job windows.Handle) (bool, error) {
	if process == 0 || job == 0 || p3ACCIsProcessInJobProcedure.Find() != nil {
		return false, errP3ACCLauncherIsolation
	}
	var contained int32
	result, _, callErr := p3ACCIsProcessInJobProcedure.Call(
		uintptr(process), uintptr(job), uintptr(unsafe.Pointer(&contained)),
	)
	if result == 0 {
		if callErr == nil || errors.Is(callErr, syscall.Errno(0)) {
			return false, errP3ACCLauncherIsolation
		}
		return false, errP3ACCLauncherIsolation
	}
	return contained != 0, nil
}

func p3ACCLauncherJobLimitFlags(job windows.Handle) (uint32, error) {
	if job == 0 {
		return 0, errP3ACCLauncherIsolation
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	var returnedLength uint32
	if err := windows.QueryInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)),
		uint32(unsafe.Sizeof(limits)),
		&returnedLength,
	); err != nil {
		return 0, errP3ACCLauncherIsolation
	}
	return limits.BasicLimitInformation.LimitFlags, nil
}

func p3ACCLauncherProcessStartTicks(process windows.Handle) (int64, error) {
	if process == 0 {
		return 0, errP3ACCLauncherIsolation
	}
	creation := windows.Filetime{}
	exit := windows.Filetime{}
	kernel := windows.Filetime{}
	user := windows.Filetime{}
	if err := windows.GetProcessTimes(process, &creation, &exit, &kernel, &user); err != nil {
		return 0, errP3ACCLauncherIsolation
	}
	return p3ACCLauncherFiletimeTicks(creation)
}

func p3ACCLauncherFiletimeTicks(value windows.Filetime) (int64, error) {
	raw := uint64(value.HighDateTime)<<32 | uint64(value.LowDateTime)
	if raw > p3ACCMaximumInt64-p3ACCDotNet1601OffsetTicks {
		return 0, errP3ACCLauncherIsolation
	}
	ticks := raw + p3ACCDotNet1601OffsetTicks
	if ticks == 0 {
		return 0, errP3ACCLauncherIsolation
	}
	return int64(ticks), nil
}

func startP3ACCLauncherProcessSuspended(configuration p3ACCLauncherConfiguration) (p3ACCLauncherProcess, error) {
	appPointer, err := windows.UTF16PtrFromString(configuration.App)
	if err != nil {
		return p3ACCLauncherProcess{}, errP3ACCLauncherStart
	}
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine([]string{configuration.App}))
	if err != nil {
		return p3ACCLauncherProcess{}, errP3ACCLauncherStart
	}
	workingDirectory, err := windows.UTF16PtrFromString(configuration.WorkingDir)
	if err != nil {
		return p3ACCLauncherProcess{}, errP3ACCLauncherStart
	}
	startup := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
	process := windows.ProcessInformation{}
	err = windows.CreateProcess(
		appPointer, commandLine, nil, nil, false,
		windows.CREATE_SUSPENDED|windows.CREATE_UNICODE_ENVIRONMENT,
		nil, workingDirectory, &startup, &process,
	)
	if err != nil {
		return p3ACCLauncherProcess{}, errP3ACCLauncherStart
	}
	return p3ACCLauncherProcess{
		Process: process.Process, Thread: process.Thread, ProcessID: process.ProcessId,
	}, nil
}

func resumeP3ACCLauncherProcess(process windows.Handle) error {
	if process == 0 || p3ACCNtResumeProcessProcedure.Find() != nil {
		return errP3ACCLauncherResume
	}
	status, _, _ := p3ACCNtResumeProcessProcedure.Call(uintptr(process))
	if status != 0 {
		return errP3ACCLauncherResume
	}
	return nil
}

func terminateAndWaitP3ACCLauncherProcess(process windows.Handle) error {
	if process == 0 {
		return nil
	}
	exitCode := uint32(0)
	if err := windows.GetExitCodeProcess(process, &exitCode); err != nil {
		return errP3ACCLauncherCleanup
	}
	if exitCode == p3ACCStillActive {
		if err := windows.TerminateProcess(process, p3ACCLauncherTerminateCode); err != nil {
			return errP3ACCLauncherCleanup
		}
	}
	result, err := windows.WaitForSingleObject(process, p3ACCLauncherWaitMillis)
	if err != nil || result == p3ACCWaitTimeout || result != windows.WAIT_OBJECT_0 {
		return errP3ACCLauncherCleanup
	}
	return nil
}

type p3ACCFileHandshakeTransaction struct {
	temporary string
	target    string
	committed bool
}

func prepareP3ACCLauncherHandshake(path string, handshake p3ACCLauncherHandshake) (p3ACCHandshakeTransaction, error) {
	payload, err := json.Marshal(handshake)
	if err != nil {
		return nil, errP3ACCLauncherHandshake
	}
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, errP3ACCLauncherHandshake
	}
	failed := false
	written, writeErr := file.Write(payload)
	if writeErr != nil || written != len(payload) {
		failed = true
	}
	if !failed && file.Sync() != nil {
		failed = true
	}
	if file.Close() != nil {
		failed = true
	}
	if failed {
		_ = os.Remove(temporary)
		return nil, errP3ACCLauncherHandshake
	}
	return &p3ACCFileHandshakeTransaction{temporary: temporary, target: path}, nil
}

func (t *p3ACCFileHandshakeTransaction) Commit() error {
	if t == nil || t.committed || t.temporary == "" || t.target == "" {
		return errP3ACCLauncherHandshake
	}
	from, err := windows.UTF16PtrFromString(t.temporary)
	if err != nil {
		return errP3ACCLauncherHandshake
	}
	to, err := windows.UTF16PtrFromString(t.target)
	if err != nil {
		return errP3ACCLauncherHandshake
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return errP3ACCLauncherHandshake
	}
	t.committed = true
	return nil
}

func (t *p3ACCFileHandshakeTransaction) Abort() error {
	if t == nil || t.committed || t.temporary == "" {
		return nil
	}
	if err := os.Remove(t.temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errP3ACCLauncherCleanup
	}
	return nil
}
