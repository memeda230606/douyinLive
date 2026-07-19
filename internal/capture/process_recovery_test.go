package capture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const (
	processRecoverySessionID    = "019b5fa7-1d1d-7b35-8840-2b330c72d470"
	processRecoveryAttempt1     = "019b5fa7-1d1e-7657-9cf5-673fe4e29bc2"
	processRecoveryAttempt2     = "019b5fa7-1d1f-7585-a53a-6a38c68ea4d7"
	processRecoveryAttempt3     = "019b5fa7-1d20-7d85-995f-3b7c0f332bbc"
	processRecoveryJobNamespace = "0123456789abcdef0123456789abcdef"
)

type stubSessionMediaSnapshotLoader struct {
	snapshot MediaSnapshot
	err      error
	calls    []string
}

func (loader *stubSessionMediaSnapshotLoader) LoadSnapshot(
	_ context.Context,
	sessionID string,
) (MediaSnapshot, error) {
	loader.calls = append(loader.calls, sessionID)
	return loader.snapshot, loader.err
}

func TestSessionProcessRecovererEmptyAndMissingSnapshots(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		loader *stubSessionMediaSnapshotLoader
	}{
		{
			name: "empty durable attempts",
			loader: &stubSessionMediaSnapshotLoader{snapshot: MediaSnapshot{
				Session: SessionMedia{SessionID: processRecoverySessionID},
			}},
		},
		{
			name: "missing snapshot",
			loader: &stubSessionMediaSnapshotLoader{
				err: errors.Join(ErrSessionMediaNotFound, errors.New("not durable")),
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			inspectCalls := 0
			recoverer := mustSessionProcessRecoverer(
				t, test.loader,
				func(context.Context, string, string) (RecorderProcessRecoveryResult, error) {
					inspectCalls++
					return RecorderProcessRecoveryResult{}, errors.New("unexpected inspector")
				},
			)
			result, err := recoverer.RecoverSessionProcesses(
				context.Background(), processRecoverySessionID,
			)
			if err != nil {
				t.Fatalf("RecoverSessionProcesses error = %v", err)
			}
			if result != (SessionProcessRecoveryResult{State: SessionProcessRecoveryClean}) {
				t.Fatalf("result = %#v", result)
			}
			if inspectCalls != 0 {
				t.Fatalf("inspect calls = %d, want 0", inspectCalls)
			}
			if len(test.loader.calls) != 1 ||
				test.loader.calls[0] != processRecoverySessionID {
				t.Fatalf("loader calls = %v", test.loader.calls)
			}
		})
	}
}

func TestSessionProcessRecovererChecksDurableAttemptsInOrdinalOrder(t *testing.T) {
	t.Parallel()
	loader := &stubSessionMediaSnapshotLoader{snapshot: MediaSnapshot{
		Session: SessionMedia{
			SessionID: processRecoverySessionID,
			Attempts: []MediaAttempt{
				validProcessRecoveryAttempt(processRecoveryAttempt3, 3),
				validProcessRecoveryAttempt(processRecoveryAttempt1, 1),
				validProcessRecoveryAttempt(processRecoveryAttempt2, 2),
			},
		},
	}}
	var inspected []string
	results := map[string]RecorderProcessRecoveryResult{
		processRecoveryAttempt1: {
			Status: RecorderProcessRecoveryClean,
		},
		processRecoveryAttempt2: {
			Found: true, Terminated: true,
			Status: RecorderProcessRecoveryTerminated,
		},
		processRecoveryAttempt3: {
			Found: true, Status: RecorderProcessRecoveryClean,
		},
	}
	recoverer := mustSessionProcessRecoverer(
		t, loader,
		func(_ context.Context, jobNamespace, attemptID string) (RecorderProcessRecoveryResult, error) {
			if jobNamespace != processRecoveryJobNamespace {
				t.Fatalf("inspector namespace = %q", jobNamespace)
			}
			inspected = append(inspected, attemptID)
			return results[attemptID], nil
		},
	)
	for run := 0; run < 20; run++ {
		inspected = nil
		result, err := recoverer.RecoverSessionProcesses(
			context.Background(), processRecoverySessionID,
		)
		if err != nil {
			t.Fatalf("run %d error = %v", run, err)
		}
		want := SessionProcessRecoveryResult{
			AttemptsChecked: 3, ProcessesFound: 2, ProcessesTerminated: 1,
			State: SessionProcessRecoveryRecovered,
		}
		if result != want {
			t.Fatalf("run %d result = %#v, want %#v", run, result, want)
		}
		wantOrder := []string{
			processRecoveryAttempt1, processRecoveryAttempt2, processRecoveryAttempt3,
		}
		if fmt.Sprint(inspected) != fmt.Sprint(wantOrder) {
			t.Fatalf("run %d order = %v, want %v", run, inspected, wantOrder)
		}
	}
}

func TestSessionProcessRecovererFailsClosedOnInspectionFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		process    RecorderProcessRecoveryResult
		inspectErr error
		cause      error
	}{
		{
			name: "open failure",
			process: RecorderProcessRecoveryResult{
				Status:    RecorderProcessRecoveryFailed,
				ErrorCode: RecorderProcessRecoveryOpenErrorCode,
			},
			inspectErr: errRecorderProcessRecoveryOpen,
			cause:      errRecorderProcessRecoveryOpen,
		},
		{
			name: "query failure",
			process: RecorderProcessRecoveryResult{
				Found: true, Status: RecorderProcessRecoveryFailed,
				ErrorCode: RecorderProcessRecoveryQueryErrorCode,
			},
			inspectErr: errRecorderProcessRecoveryQuery,
			cause:      errRecorderProcessRecoveryQuery,
		},
		{
			name: "terminate failure",
			process: RecorderProcessRecoveryResult{
				Found: true, Status: RecorderProcessRecoveryFailed,
				ErrorCode: RecorderProcessRecoveryTerminateErrorCode,
			},
			inspectErr: errRecorderProcessRecoveryTerminate,
			cause:      errRecorderProcessRecoveryTerminate,
		},
		{
			name: "termination incomplete",
			process: RecorderProcessRecoveryResult{
				Found: true, Terminated: true, Status: RecorderProcessRecoveryFailed,
				ErrorCode: RecorderProcessRecoveryIncompleteErrorCode,
			},
			inspectErr: errRecorderProcessRecoveryIncomplete,
			cause:      errRecorderProcessRecoveryIncomplete,
		},
		{
			name: "close failure",
			process: RecorderProcessRecoveryResult{
				Found: true, Terminated: true, Status: RecorderProcessRecoveryFailed,
				ErrorCode: RecorderProcessRecoveryCloseErrorCode,
			},
			inspectErr: errRecorderProcessRecoveryClose,
			cause:      errRecorderProcessRecoveryClose,
		},
		{
			name: "malformed success",
			process: RecorderProcessRecoveryResult{
				Terminated: true, Status: RecorderProcessRecoveryClean,
			},
			cause: errSessionProcessRecoveryInspectionInvalid,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			loader := &stubSessionMediaSnapshotLoader{snapshot: MediaSnapshot{
				Session: SessionMedia{
					SessionID: processRecoverySessionID,
					Attempts: []MediaAttempt{
						validProcessRecoveryAttempt(processRecoveryAttempt1, 1),
						validProcessRecoveryAttempt(processRecoveryAttempt2, 2),
					},
				},
			}}
			calls := 0
			recoverer := mustSessionProcessRecoverer(
				t, loader,
				func(context.Context, string, string) (RecorderProcessRecoveryResult, error) {
					calls++
					return test.process, test.inspectErr
				},
			)
			for run := 0; run < 20; run++ {
				calls = 0
				result, err := recoverer.RecoverSessionProcesses(
					context.Background(), processRecoverySessionID,
				)
				if !errors.Is(err, ErrSessionProcessRecovery) {
					t.Fatalf("run %d error = %v", run, err)
				}
				if !errors.Is(err, test.cause) {
					t.Fatalf("run %d error does not contain cause %v", run, test.cause)
				}
				if result.State != SessionProcessRecoveryFailed ||
					result.ErrorCode != SessionProcessRecoveryProcessFailedCode ||
					result.AttemptsChecked != 1 {
					t.Fatalf("run %d result = %#v", run, result)
				}
				if calls != 1 {
					t.Fatalf("run %d calls = %d, want fail-fast 1", run, calls)
				}
			}
		})
	}
}

func TestSessionProcessRecovererCancellationStopsBeforeNextAttempt(t *testing.T) {
	t.Parallel()
	loader := &stubSessionMediaSnapshotLoader{snapshot: MediaSnapshot{
		Session: SessionMedia{
			SessionID: processRecoverySessionID,
			Attempts: []MediaAttempt{
				validProcessRecoveryAttempt(processRecoveryAttempt1, 1),
				validProcessRecoveryAttempt(processRecoveryAttempt2, 2),
			},
		},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	recoverer := mustSessionProcessRecoverer(
		t, loader,
		func(context.Context, string, string) (RecorderProcessRecoveryResult, error) {
			calls++
			cancel()
			return RecorderProcessRecoveryResult{
				Status: RecorderProcessRecoveryClean,
			}, nil
		},
	)
	result, err := recoverer.RecoverSessionProcesses(ctx, processRecoverySessionID)
	if !errors.Is(err, ErrSessionProcessRecovery) ||
		!errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if result.State != SessionProcessRecoveryFailed ||
		result.ErrorCode != SessionProcessRecoveryInterruptedCode ||
		result.AttemptsChecked != 1 || calls != 1 {
		t.Fatalf("result = %#v calls = %d", result, calls)
	}
}

func TestSessionProcessRecovererSnapshotAndPrivacyFailures(t *testing.T) {
	t.Parallel()
	const secret = `D:\capture\room.flv https://live.example/private`
	tests := []struct {
		name       string
		loader     *stubSessionMediaSnapshotLoader
		inspectErr error
		wantCode   string
	}{
		{
			name: "snapshot load",
			loader: &stubSessionMediaSnapshotLoader{
				err: errors.New(secret),
			},
			wantCode: SessionProcessRecoverySnapshotFailedCode,
		},
		{
			name: "process inspect",
			loader: &stubSessionMediaSnapshotLoader{snapshot: MediaSnapshot{
				Session: SessionMedia{
					SessionID: processRecoverySessionID,
					Attempts: []MediaAttempt{
						validProcessRecoveryAttempt(processRecoveryAttempt1, 1),
					},
				},
			}},
			inspectErr: errors.New(secret),
			wantCode:   SessionProcessRecoveryProcessFailedCode,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			recoverer := mustSessionProcessRecoverer(
				t, test.loader,
				func(context.Context, string, string) (RecorderProcessRecoveryResult, error) {
					return RecorderProcessRecoveryResult{
						Status: RecorderProcessRecoveryFailed,
					}, test.inspectErr
				},
			)
			result, err := recoverer.RecoverSessionProcesses(
				context.Background(), processRecoverySessionID,
			)
			if !errors.Is(err, ErrSessionProcessRecovery) {
				t.Fatalf("error = %v", err)
			}
			encoded, encodeErr := json.Marshal(result)
			if encodeErr != nil {
				t.Fatalf("json.Marshal: %v", encodeErr)
			}
			output := fmt.Sprintf("%v\n%+v\n%#v\n%#v\n%s", err, err, err, result, encoded)
			if strings.Contains(output, `D:\capture`) ||
				strings.Contains(output, "https://") {
				t.Fatalf("private data leaked: %s", output)
			}
			if result.ErrorCode != test.wantCode {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestSessionProcessRecovererRejectsConfigurationAndSnapshotMismatch(t *testing.T) {
	t.Parallel()
	if _, err := NewSessionProcessRecoverer(nil); !errors.Is(err, ErrSessionProcessRecovery) {
		t.Fatalf("NewSessionProcessRecoverer(nil) error = %v", err)
	}
	if _, err := newSessionProcessRecoverer(nil, "", nil); !errors.Is(err, ErrSessionProcessRecovery) {
		t.Fatalf("newSessionProcessRecoverer(nil) error = %v", err)
	}

	recoverer := mustSessionProcessRecoverer(
		t,
		&stubSessionMediaSnapshotLoader{snapshot: MediaSnapshot{
			Session: SessionMedia{SessionID: processRecoveryAttempt1},
		}},
		func(context.Context, string, string) (RecorderProcessRecoveryResult, error) {
			t.Fatal("inspector must not run for mismatched snapshot")
			return RecorderProcessRecoveryResult{}, nil
		},
	)
	result, err := recoverer.RecoverSessionProcesses(
		context.Background(), processRecoverySessionID,
	)
	if !errors.Is(err, ErrSessionProcessRecovery) ||
		result.ErrorCode != SessionProcessRecoverySnapshotFailedCode {
		t.Fatalf("result = %#v error = %v", result, err)
	}
}

func TestNewSessionProcessRecovererDerivesRepositoryJobNamespace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	want, valid := recorderJobNamespace(root)
	if !valid {
		t.Fatal("derive expected recorder Job namespace")
	}
	recoverer, err := NewSessionProcessRecoverer(&SQLiteRepository{dataRoot: root})
	if err != nil {
		t.Fatalf("NewSessionProcessRecoverer error = %v", err)
	}
	durable, ok := recoverer.(*durableSessionProcessRecoverer)
	if !ok {
		t.Fatalf("recoverer type = %T", recoverer)
	}
	if durable.jobNamespace != want || !validRecorderJobNamespace(durable.jobNamespace) {
		t.Fatalf("repository Job namespace = %q, want %q", durable.jobNamespace, want)
	}
}

func validProcessRecoveryAttempt(id string, ordinal int) MediaAttempt {
	return MediaAttempt{
		ID: id, Ordinal: ordinal, StartedAt: 1, SegmentSeconds: 300,
		Protocol: "flv", Codec: "h264",
	}
}

func mustSessionProcessRecoverer(
	t *testing.T,
	loader sessionMediaSnapshotLoader,
	inspect sessionProcessInspector,
) SessionProcessRecoverer {
	t.Helper()
	recoverer, err := newSessionProcessRecoverer(loader, processRecoveryJobNamespace, inspect)
	if err != nil {
		t.Fatalf("newSessionProcessRecoverer error = %v", err)
	}
	return recoverer
}
