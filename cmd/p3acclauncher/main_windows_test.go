//go:build windows && p3accacceptance

package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

const p3ACCLauncherTestNonce = "00112233445566778899aabbccddeeff"

type p3ACCLauncherTestTransaction struct {
	state     *p3ACCLauncherTestState
	commitErr error
	abortErr  error
}

func (t *p3ACCLauncherTestTransaction) Commit() error {
	t.state.order = append(t.state.order, "commit")
	if t.commitErr == nil {
		t.state.committed++
	}
	return t.commitErr
}

func (t *p3ACCLauncherTestTransaction) Abort() error {
	t.state.order = append(t.state.order, "abort")
	t.state.aborted++
	return t.abortErr
}

type p3ACCLauncherTestState struct {
	order          []string
	fail           string
	configuration  p3ACCLauncherConfiguration
	handshake      p3ACCLauncherHandshake
	committed      int
	aborted        int
	terminatedJob  int
	terminatedRoot int
}

func p3ACCLauncherTestConfiguration(t *testing.T) p3ACCLauncherConfiguration {
	t.Helper()
	root := t.TempDir()
	app := filepath.Join(root, "app.exe")
	if err := os.WriteFile(app, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p3ACCLauncherConfiguration{
		JobName:  p3ACCLauncherJobNamePrefix + p3ACCLauncherTestNonce,
		JobNonce: p3ACCLauncherTestNonce,
		App:      app, WorkingDir: root, Handshake: filepath.Join(root, "handshake.json"),
	}
}

func p3ACCLauncherTestArguments(configuration p3ACCLauncherConfiguration) []string {
	return []string{
		"--job-name", configuration.JobName,
		"--job-nonce", configuration.JobNonce,
		"--app", configuration.App,
		"--working-dir", configuration.WorkingDir,
		"--handshake", configuration.Handshake,
	}
}

func p3ACCLauncherTestOps(state *p3ACCLauncherTestState) p3ACCLauncherOps {
	injected := errors.New("injected")
	return p3ACCLauncherOps{
		openJob: func(name string) (windows.Handle, error) {
			state.order = append(state.order, "open")
			if state.fail == "open" {
				return 0, injected
			}
			return windows.Handle(11), nil
		},
		assignProcess: func(job, process windows.Handle) error {
			state.order = append(state.order, "assign-self")
			if state.fail == "assign" {
				return injected
			}
			return nil
		},
		isProcessInJob: func(process, job windows.Handle) (bool, error) {
			stage := "is-self"
			if process == windows.Handle(33) {
				stage = "is-child"
			}
			state.order = append(state.order, stage)
			if state.fail == stage {
				return false, injected
			}
			return true, nil
		},
		jobLimitFlags: func(job windows.Handle) (uint32, error) {
			state.order = append(state.order, "limits")
			switch state.fail {
			case "limits-error":
				return 0, injected
			case "limits-missing-kill":
				return 0, nil
			case "limits-breakaway":
				return windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK, nil
			default:
				return windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, nil
			}
		},
		currentProcess:   func() windows.Handle { return windows.Handle(22) },
		currentProcessID: func() uint32 { return 44 },
		processStartTicks: func(process windows.Handle) (int64, error) {
			stage := "ticks-self"
			value := int64(1001)
			if process == windows.Handle(33) {
				stage = "ticks-child"
				value = 1002
			}
			state.order = append(state.order, stage)
			if state.fail == stage {
				return 0, injected
			}
			return value, nil
		},
		startSuspended: func(configuration p3ACCLauncherConfiguration) (p3ACCLauncherProcess, error) {
			state.order = append(state.order, "start")
			state.configuration = configuration
			if state.fail == "start" {
				return p3ACCLauncherProcess{}, injected
			}
			return p3ACCLauncherProcess{Process: 33, Thread: 34, ProcessID: 55}, nil
		},
		resumeProcess: func(process windows.Handle) error {
			state.order = append(state.order, "resume")
			if state.fail == "resume" {
				return injected
			}
			return nil
		},
		terminateJob: func(job windows.Handle, code uint32) error {
			state.order = append(state.order, "terminate-job")
			state.terminatedJob++
			if state.fail == "terminate-job" {
				return injected
			}
			return nil
		},
		terminateAndWait: func(process windows.Handle) error {
			state.order = append(state.order, "terminate-root")
			state.terminatedRoot++
			return nil
		},
		closeHandle: func(handle windows.Handle) error {
			stage := "close-other"
			switch handle {
			case 11:
				stage = "close-job"
			case 33:
				stage = "close-process"
			case 34:
				stage = "close-thread"
			}
			state.order = append(state.order, stage)
			if state.fail == stage {
				return injected
			}
			return nil
		},
		prepareHandshake: func(path string, handshake p3ACCLauncherHandshake) (p3ACCHandshakeTransaction, error) {
			state.order = append(state.order, "prepare")
			state.handshake = handshake
			if state.fail == "prepare" {
				return nil, injected
			}
			transaction := &p3ACCLauncherTestTransaction{state: state}
			if state.fail == "commit" {
				transaction.commitErr = injected
			}
			return transaction, nil
		},
		unsetenv: func(name string) error {
			state.order = append(state.order, "unset")
			if state.fail == "unset" {
				return injected
			}
			return nil
		},
	}
}

func TestP3ACCLauncherStrictConfiguration(t *testing.T) {
	configuration := p3ACCLauncherTestConfiguration(t)
	arguments := p3ACCLauncherTestArguments(configuration)
	parsed, err := parseP3ACCLauncherConfiguration(arguments)
	if err != nil || parsed != configuration {
		t.Fatalf("valid configuration = %#v, %v", parsed, err)
	}

	invalid := [][]string{
		nil,
		append(append([]string(nil), arguments...), "extra"),
		append([]string{"--job-name", configuration.JobName}, arguments[2:8]...),
		append([]string{"--job-name=" + configuration.JobName}, arguments[2:]...),
	}
	wrongNonce := append([]string(nil), arguments...)
	wrongNonce[3] = strings.ToUpper(p3ACCLauncherTestNonce)
	invalid = append(invalid, wrongNonce)
	wrongName := append([]string(nil), arguments...)
	wrongName[1] = `Global\DouyinLive.P3ACC.v1.` + p3ACCLauncherTestNonce
	invalid = append(invalid, wrongName)
	for _, candidate := range invalid {
		if _, err := parseP3ACCLauncherConfiguration(candidate); !errors.Is(err, errP3ACCLauncherConfiguration) {
			t.Fatalf("invalid arguments accepted: %#v", candidate)
		}
	}
}

func TestP3ACCLauncherSuccessOrderAndHandshake(t *testing.T) {
	configuration := p3ACCLauncherTestConfiguration(t)
	state := &p3ACCLauncherTestState{}
	if err := launchP3ACCApp(configuration, p3ACCLauncherTestOps(state)); err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{
		"open", "assign-self", "is-self", "limits", "ticks-self", "start",
		"is-child", "ticks-child", "prepare", "unset", "resume", "close-thread",
		"commit", "close-process", "close-job",
	}
	if !reflect.DeepEqual(state.order, wantOrder) {
		t.Fatalf("order = %#v", state.order)
	}
	if state.configuration != configuration || state.committed != 1 || state.aborted != 0 ||
		state.terminatedJob != 0 || state.terminatedRoot != 0 {
		t.Fatalf("state = %#v", state)
	}
	wantHandshake := p3ACCLauncherHandshake{
		Schema: p3ACCLauncherHandshakeSchema, JobNonce: p3ACCLauncherTestNonce,
		LauncherProcessID: 44, LauncherStartedAtUTCTicks: 1001,
		AppProcessID: 55, AppStartedAtUTCTicks: 1002,
	}
	if state.handshake != wantHandshake {
		t.Fatalf("handshake = %#v", state.handshake)
	}
}

func TestP3ACCLauncherFailuresAreFailClosed(t *testing.T) {
	for _, test := range []struct {
		stage       string
		wantError   error
		started     bool
		transaction bool
	}{
		{stage: "open", wantError: errP3ACCLauncherIsolation},
		{stage: "assign", wantError: errP3ACCLauncherIsolation},
		{stage: "is-self", wantError: errP3ACCLauncherIsolation},
		{stage: "limits-error", wantError: errP3ACCLauncherIsolation},
		{stage: "limits-missing-kill", wantError: errP3ACCLauncherIsolation},
		{stage: "limits-breakaway", wantError: errP3ACCLauncherIsolation},
		{stage: "start", wantError: errP3ACCLauncherStart},
		{stage: "is-child", wantError: errP3ACCLauncherIsolation, started: true},
		{stage: "ticks-child", wantError: errP3ACCLauncherStart, started: true},
		{stage: "prepare", wantError: errP3ACCLauncherHandshake, started: true},
		{stage: "unset", wantError: errP3ACCLauncherCleanup, started: true, transaction: true},
		{stage: "resume", wantError: errP3ACCLauncherResume, started: true, transaction: true},
		{stage: "close-thread", wantError: errP3ACCLauncherCleanup, started: true, transaction: true},
		{stage: "commit", wantError: errP3ACCLauncherHandshake, started: true, transaction: true},
	} {
		t.Run(test.stage, func(t *testing.T) {
			state := &p3ACCLauncherTestState{fail: test.stage}
			err := launchP3ACCApp(p3ACCLauncherTestConfiguration(t), p3ACCLauncherTestOps(state))
			if !errors.Is(err, test.wantError) {
				t.Fatalf("error = %v", err)
			}
			if state.committed != 0 {
				t.Fatal("success handshake committed on failure")
			}
			if (state.terminatedJob == 1) != test.started || (state.terminatedRoot == 1) != test.started {
				t.Fatalf("termination counts = %d/%d", state.terminatedJob, state.terminatedRoot)
			}
			if (state.aborted == 1) != test.transaction {
				t.Fatalf("abort count = %d", state.aborted)
			}
		})
	}
}

func TestP3ACCLauncherFiletimeTicks(t *testing.T) {
	const unixEpochFiletimeValue = 116444736000000000
	unixEpochFiletime := uint64(unixEpochFiletimeValue)
	value := windows.Filetime{
		LowDateTime: uint32(unixEpochFiletime & 0xffffffff), HighDateTime: uint32(unixEpochFiletime >> 32),
	}
	ticks, err := p3ACCLauncherFiletimeTicks(value)
	if err != nil || ticks != 621355968000000000 {
		t.Fatalf("Unix epoch ticks = %d, %v", ticks, err)
	}
	overflow := p3ACCMaximumInt64 - p3ACCDotNet1601OffsetTicks + 1
	if _, err := p3ACCLauncherFiletimeTicks(windows.Filetime{
		LowDateTime: uint32(overflow), HighDateTime: uint32(overflow >> 32),
	}); !errors.Is(err, errP3ACCLauncherIsolation) {
		t.Fatal("overflow FILETIME accepted")
	}
}

func TestP3ACCLauncherHandshakeIsAtomicAndNonReplacing(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "handshake.json")
	handshake := p3ACCLauncherHandshake{
		Schema: p3ACCLauncherHandshakeSchema, JobNonce: p3ACCLauncherTestNonce,
		LauncherProcessID: 1, LauncherStartedAtUTCTicks: 2,
		AppProcessID: 3, AppStartedAtUTCTicks: 4,
	}
	transaction, err := prepareP3ACCLauncherHandshake(target, handshake)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("target was visible before commit")
	}
	if _, err := os.Stat(target + ".tmp"); err != nil {
		t.Fatal("temporary handshake missing")
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	decoded := p3ACCLauncherHandshake{}
	if json.Unmarshal(payload, &decoded) != nil || decoded != handshake {
		t.Fatalf("payload = %s", payload)
	}
	if strings.Contains(string(payload), "P3ACC_LIVE_URL") || strings.Contains(string(payload), root) {
		t.Fatal("handshake leaked environment or path")
	}

	if err := os.WriteFile(target+".tmp", []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareP3ACCLauncherHandshake(target, handshake); !errors.Is(err, errP3ACCLauncherHandshake) {
		t.Fatal("stale temporary handshake was accepted")
	}
	if err := os.Remove(target + ".tmp"); err != nil {
		t.Fatal(err)
	}
	second, err := prepareP3ACCLauncherHandshake(target, handshake)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Commit(); !errors.Is(err, errP3ACCLauncherHandshake) {
		t.Fatal("existing target was replaced")
	}
	if err := second.Abort(); err != nil {
		t.Fatal(err)
	}
	preserved, err := os.ReadFile(target)
	if err != nil || !reflect.DeepEqual(preserved, payload) {
		t.Fatal("existing target changed")
	}
}
