//go:build windows && p3accacceptance && p3accinteractive

package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

const (
	p3ACCLauncherInteractiveGateEnvironment = "P3ACC_RUN_INTERACTIVE_TASK_TEST"
)

func TestP3ACCLauncherRealInteractiveScheduledTaskNestedJob(t *testing.T) {
	if os.Getenv(p3ACCLauncherInteractiveGateEnvironment) != "1" {
		t.Fatalf(
			"%s must be exactly 1 for the explicit p3accinteractive Scheduled Task gate",
			p3ACCLauncherInteractiveGateEnvironment,
		)
	}
	t.Log("P3ACC_INTERACTIVE_SCHEDULED_TASK_GATE=RUNNING")
	nonce := p3ACCLauncherTestNonceValue(t)
	root := t.TempDir()
	outerName := p3ACCLauncherJobNamePrefix + nonce
	outer, err := p3ACCLauncherTestCreateInteractiveOuterJob(outerName)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if outer != 0 {
			if err := p3ACCLauncherTestTerminateAndCloseJob(&outer, 10*time.Second); err != nil {
				t.Errorf("interactive outer Job cleanup failed: %v", err)
			}
		}
	}()

	helper := p3ACCLauncherTestCopyExecutable(t, root, nonce)
	runner := p3ACCLauncherTestCopyRunner(t, root, nonce)
	markerPath := filepath.Join(root, "interactive-nested.json")
	handshakePath := filepath.Join(root, "interactive-handshake.json")
	statusPath := filepath.Join(root, "interactive-action.status")
	diagnosticPath := filepath.Join(root, "interactive-runner.diagnostic")
	innerName := `Global\DouyinLive.P3ACC.Test.Interactive.Inner.` + nonce
	configuration := p3ACCLauncherConfiguration{
		JobName: outerName, JobNonce: nonce, App: helper,
		WorkingDir: root, Handshake: handshakePath,
	}
	launchPath := filepath.Join(root, "interactive-launch.ps1")
	if err := p3ACCLauncherTestWriteInteractiveLaunch(
		launchPath, runner, configuration, markerPath, statusPath, diagnosticPath, innerName,
	); err != nil {
		t.Fatal(err)
	}
	taskName := "DouyinLive.P3ACC.LauncherGate." + nonce
	registered := true
	defer func() {
		if registered {
			if err := p3ACCLauncherTestRemoveInteractiveTask(taskName); err != nil {
				t.Errorf("interactive task cleanup failed: %v", err)
			}
		}
	}()
	if err := p3ACCLauncherTestStartInteractiveTask(taskName, launchPath); err != nil {
		t.Fatal(err)
	}

	handshake := p3ACCLauncherTestWaitHandshake(t, handshakePath, statusPath, nonce)
	launcherIdentity := p3ACCLauncherTestIdentity{
		ProcessID:         handshake.LauncherProcessID,
		StartedAtUTCTicks: handshake.LauncherStartedAtUTCTicks,
	}
	if !p3ACCLauncherTestWaitExactIdentityGone(launcherIdentity, 10*time.Second) {
		t.Fatal("interactive launcher did not release its Job handle and exit")
	}
	p3ACCLauncherTestWaitRunnerSuccess(t, statusPath)
	app, err := p3ACCLauncherTestOpenExactProcess(p3ACCLauncherTestIdentity{
		ProcessID: handshake.AppProcessID, StartedAtUTCTicks: handshake.AppStartedAtUTCTicks,
	})
	if err != nil {
		t.Fatal(err)
	}
	appOpen := true
	defer func() {
		if appOpen {
			_ = windows.CloseHandle(app)
		}
	}()
	marker := p3ACCLauncherTestWaitMarker(t, markerPath)
	if !marker.LiveURLPresent {
		t.Fatal("interactive app did not inherit the launch environment")
	}
	child, err := p3ACCLauncherTestOpenExactProcess(marker.Child)
	if err != nil {
		t.Fatal(err)
	}
	childOpen := true
	defer func() {
		if childOpen {
			_ = windows.CloseHandle(child)
		}
	}()
	inner, err := openP3ACCLauncherJob(innerName)
	if err != nil {
		t.Fatal(err)
	}
	innerOpen := true
	defer func() {
		if innerOpen {
			_ = windows.CloseHandle(inner)
		}
	}()

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
			t.Fatalf("interactive nested Job membership = %v, %v", contained, err)
		}
	}
	if err := windows.TerminateJobObject(inner, p3ACCLauncherTerminateCode); err != nil {
		t.Fatal(err)
	}
	if !p3ACCLauncherTestWaitExited(child, 5*time.Second) || !p3ACCLauncherTestIsAlive(app) {
		t.Fatal("interactive inner Job termination affected the outer app")
	}
	if err := windows.CloseHandle(child); err != nil {
		t.Fatal(err)
	}
	childOpen = false
	if err := windows.CloseHandle(inner); err != nil {
		t.Fatal(err)
	}
	innerOpen = false
	if err := windows.CloseHandle(outer); err != nil {
		t.Fatal(err)
	}
	outer = 0
	if !p3ACCLauncherTestWaitExited(app, 5*time.Second) {
		t.Fatal("interactive outer Job close left the app alive")
	}
	if err := windows.CloseHandle(app); err != nil {
		t.Fatal(err)
	}
	appOpen = false

	if err := p3ACCLauncherTestRemoveInteractiveTask(taskName); err != nil {
		t.Fatal(err)
	}
	registered = false
	for _, path := range []string{
		handshakePath + ".tmp", markerPath + ".tmp",
	} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("interactive gate left a temporary control file")
		}
	}
	if err := p3ACCLauncherTestRemoveFiles(
		handshakePath, markerPath, statusPath, diagnosticPath, launchPath, helper, runner,
	); err != nil {
		t.Fatal(err)
	}
	t.Log("P3ACC_INTERACTIVE_SCHEDULED_TASK_GATE=PASSED")
}

func p3ACCLauncherTestCopyRunner(t *testing.T, directory, nonce string) string {
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
	targetPath := filepath.Join(directory, "p3acc-launcher-runner-"+nonce+".exe")
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

func p3ACCLauncherTestPowerShellLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func p3ACCLauncherTestWriteInteractiveLaunch(
	path string,
	runner string,
	configuration p3ACCLauncherConfiguration,
	markerPath string,
	statusPath string,
	diagnosticPath string,
	innerName string,
) error {
	literal := p3ACCLauncherTestPowerShellLiteral
	lines := []string{
		"Set-StrictMode -Version Latest",
		"$ErrorActionPreference = 'Stop'",
		"$statusPath = " + literal(statusPath),
		"[IO.File]::WriteAllText($statusPath, 'action-started')",
		"$env:" + p3ACCLauncherTestDiagnosticEnvironment + " = " + literal(diagnosticPath),
		"$env:" + p3ACCLauncherTestHelperEnvironment + " = 'nested-app'",
		"$env:" + p3ACCLauncherTestMarkerEnvironment + " = " + literal(markerPath),
		"$env:" + p3ACCLauncherTestInnerEnvironment + " = " + literal(innerName),
		"$env:P3ACC_LIVE_URL = 'https://secret.invalid/live'",
		"try {",
		"    & " + literal(runner) + " '--' '--job-name' " + literal(configuration.JobName) +
			" '--job-nonce' " + literal(configuration.JobNonce) +
			" '--app' " + literal(configuration.App) +
			" '--working-dir' " + literal(configuration.WorkingDir) +
			" '--handshake' " + literal(configuration.Handshake),
		"    $runnerExitCode = $LASTEXITCODE",
		"    $runnerDiagnostic = 'missing'",
		"    if (Test-Path -LiteralPath " + literal(diagnosticPath) + ") { $runnerDiagnostic = [IO.File]::ReadAllText(" + literal(diagnosticPath) + ") }",
		"    [IO.File]::WriteAllText($statusPath, ('runner-exit:' + $runnerExitCode + ':' + $runnerDiagnostic))",
		"    exit $runnerExitCode",
		"} catch {",
		"    [IO.File]::WriteAllText($statusPath, 'action-failed')",
		"    exit 71",
		"} finally {",
		"    Remove-Item Env:P3ACC_LIVE_URL,Env:" + p3ACCLauncherTestHelperEnvironment +
			",Env:" + p3ACCLauncherTestMarkerEnvironment + ",Env:" + p3ACCLauncherTestInnerEnvironment +
			",Env:" + p3ACCLauncherTestDiagnosticEnvironment + " -ErrorAction SilentlyContinue",
		"}",
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	payload := []byte(strings.Join(lines, "\r\n") + "\r\n")
	written, writeErr := file.Write(payload)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || written != len(payload) || syncErr != nil || closeErr != nil {
		return errors.New("interactive launch script write failed")
	}
	return nil
}

func p3ACCLauncherTestRunPowerShell(script string) error {
	runes := utf16.Encode([]rune(script))
	payload := make([]byte, len(runes)*2)
	for index, value := range runes {
		binary.LittleEndian.PutUint16(payload[index*2:], value)
	}
	encoded := base64.StdEncoding.EncodeToString(payload)
	command := exec.Command(
		"powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-EncodedCommand", encoded,
	)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return errors.New("interactive task command failed")
	}
	return nil
}

func p3ACCLauncherTestStartInteractiveTask(taskName, launchPath string) error {
	literal := p3ACCLauncherTestPowerShellLiteral
	script := strings.Join([]string{
		"$ErrorActionPreference = 'Stop'",
		"$taskName = " + literal(taskName),
		"$launchPath = " + literal(launchPath),
		"$arguments = '-NoProfile -NonInteractive -WindowStyle Hidden -ExecutionPolicy Bypass -File \"' + $launchPath + '\"'",
		"$action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument $arguments",
		"$principal = New-ScheduledTaskPrincipal -UserId ([Security.Principal.WindowsIdentity]::GetCurrent().Name) -LogonType Interactive -RunLevel Limited",
		"$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit (New-TimeSpan -Minutes 2) -MultipleInstances IgnoreNew",
		"Register-ScheduledTask -TaskName $taskName -Action $action -Principal $principal -Settings $settings -Force | Out-Null",
		"Start-ScheduledTask -TaskName $taskName",
	}, "; ")
	return p3ACCLauncherTestRunPowerShell(script)
}

func p3ACCLauncherTestRemoveInteractiveTask(taskName string) error {
	literal := p3ACCLauncherTestPowerShellLiteral
	script := strings.Join([]string{
		"$ErrorActionPreference = 'Stop'",
		"$taskName = " + literal(taskName),
		"$tasks = @(Get-ScheduledTask -ErrorAction Stop | Where-Object { $_.TaskName -ceq $taskName })",
		"if ($tasks.Count -gt 1) { exit 42 }",
		"if ($tasks.Count -eq 1) {",
		"    Stop-ScheduledTask -InputObject $tasks[0] -ErrorAction Stop",
		"    Unregister-ScheduledTask -TaskName $taskName -Confirm:$false -ErrorAction Stop",
		"}",
		"$remaining = @(Get-ScheduledTask -ErrorAction Stop | Where-Object { $_.TaskName -ceq $taskName })",
		"if ($remaining.Count -ne 0) { exit 42 }",
	}, "; ")
	return p3ACCLauncherTestRunPowerShell(script)
}

func p3ACCLauncherTestWaitRunnerSuccess(t *testing.T, statusPath string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		payload, err := os.ReadFile(statusPath)
		if err == nil {
			status := strings.TrimSpace(string(payload))
			if strings.HasPrefix(status, "runner-exit:0:ok;") {
				return
			}
			if strings.HasPrefix(status, "runner-exit:") {
				t.Fatalf("interactive runner reported failure (%s)", status)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("interactive runner success status timeout")
}

func p3ACCLauncherTestWaitHandshake(t *testing.T, path, statusPath, nonce string) p3ACCLauncherHandshake {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		payload, err := os.ReadFile(path)
		if err == nil {
			handshake := p3ACCLauncherHandshake{}
			if json.Unmarshal(payload, &handshake) == nil &&
				handshake.Schema == p3ACCLauncherHandshakeSchema &&
				handshake.JobNonce == nonce && handshake.AppProcessID > 0 {
				return handshake
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	status := "status-missing"
	if payload, err := os.ReadFile(statusPath); err == nil {
		candidate := strings.TrimSpace(string(payload))
		if candidate == "action-started" || candidate == "action-failed" ||
			strings.HasPrefix(candidate, "runner-exit:") {
			status = candidate
		}
	}
	t.Fatalf("interactive launcher handshake timeout (%s)", status)
	return p3ACCLauncherHandshake{}
}

func p3ACCLauncherTestWaitExactIdentityGone(identity p3ACCLauncherTestIdentity, timeout time.Duration) bool {
	if identity.ProcessID < 1 || identity.StartedAtUTCTicks < 1 {
		return false
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		handle, err := windows.OpenProcess(
			windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE,
			false, identity.ProcessID,
		)
		if err != nil {
			return errors.Is(err, windows.ERROR_INVALID_PARAMETER)
		}
		ticks, ticksErr := p3ACCLauncherProcessStartTicks(handle)
		if ticksErr != nil {
			_ = windows.CloseHandle(handle)
			return false
		}
		if ticks != identity.StartedAtUTCTicks {
			_ = windows.CloseHandle(handle)
			return true
		}
		gone := p3ACCLauncherTestWaitExited(handle, 200*time.Millisecond)
		_ = windows.CloseHandle(handle)
		if gone {
			return true
		}
	}
	return false
}

func p3ACCLauncherTestRemoveFiles(paths ...string) error {
	deadline := time.Now().Add(10 * time.Second)
	remaining := append([]string(nil), paths...)
	for len(remaining) > 0 && time.Now().Before(deadline) {
		next := remaining[:0]
		for _, path := range remaining {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				next = append(next, path)
			}
		}
		remaining = next
		if len(remaining) > 0 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if len(remaining) > 0 {
		return errors.New("interactive gate artifact cleanup failed")
	}
	return nil
}
