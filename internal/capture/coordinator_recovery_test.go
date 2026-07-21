package capture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	douyinLive "github.com/jwwsjlm/douyinLive/v2"
)

type runtimeRecoveryJournal struct {
	repository SessionRepository
	gapID      string

	mu                sync.Mutex
	beginFailures     int
	completeFailures  int
	exhaustFailures   int
	closeFailures     int
	beginCalls        int
	successfulBegins  int
	completeCalls     int
	exhaustCalls      int
	closeCalls        int
	beginInputs       []BeginRecorderRecoveryInput
	completeInputs    []CompleteRecorderRecoveryInput
	exhaustInputs     []ExhaustRecorderRecoveryInput
	closeInputs       []CloseRecorderRecoveryInput
	beginSucceeded    chan struct{}
	completeAttempted chan struct{}
	completeSucceeded chan struct{}
	exhaustSucceeded  chan struct{}
	closeSucceeded    chan struct{}
}

func newRuntimeRecoveryJournal(
	repository SessionRepository,
	gapID string,
) *runtimeRecoveryJournal {
	return &runtimeRecoveryJournal{
		repository:        repository,
		gapID:             gapID,
		beginSucceeded:    make(chan struct{}, 16),
		completeAttempted: make(chan struct{}, 16),
		completeSucceeded: make(chan struct{}, 16),
		exhaustSucceeded:  make(chan struct{}, 16),
		closeSucceeded:    make(chan struct{}, 16),
	}
}

func (journal *runtimeRecoveryJournal) BeginRecorderRecovery(
	ctx context.Context,
	input BeginRecorderRecoveryInput,
) (RecorderRecoveryJournalEntry, error) {
	journal.mu.Lock()
	journal.beginCalls++
	journal.beginInputs = append(journal.beginInputs, input)
	if journal.beginFailures > 0 {
		journal.beginFailures--
		journal.mu.Unlock()
		return RecorderRecoveryJournalEntry{}, errors.New("injected begin failure")
	}
	journal.mu.Unlock()
	next, err := journal.repository.Transition(ctx, TransitionSessionInput{
		ID:                      input.SessionID,
		ExpectedStatus:          input.ExpectedStatus,
		ExpectedRecordingStatus: input.ExpectedRecordingStatus,
		ExpectedOperationID:     input.ExpectedOperationID,
		Status:                  input.ExpectedStatus,
		RecordingStatus:         RecordingReconnecting,
	})
	if err != nil {
		return RecorderRecoveryJournalEntry{}, err
	}
	journal.mu.Lock()
	journal.successfulBegins++
	journal.mu.Unlock()
	signalRuntimeRecovery(journal.beginSucceeded)
	return RecorderRecoveryJournalEntry{Session: next, GapID: journal.gapID}, nil
}

func (journal *runtimeRecoveryJournal) CompleteRecorderRecovery(
	ctx context.Context,
	input CompleteRecorderRecoveryInput,
) (LiveSession, error) {
	journal.mu.Lock()
	journal.completeCalls++
	journal.completeInputs = append(journal.completeInputs, input)
	fail := journal.completeFailures > 0
	if fail {
		journal.completeFailures--
	}
	journal.mu.Unlock()
	signalRuntimeRecovery(journal.completeAttempted)
	if fail {
		return LiveSession{}, errors.New("injected complete failure")
	}
	next, err := journal.repository.Transition(ctx, TransitionSessionInput{
		ID:                      input.SessionID,
		ExpectedStatus:          input.ExpectedStatus,
		ExpectedRecordingStatus: input.ExpectedRecordingStatus,
		ExpectedOperationID:     input.ExpectedOperationID,
		Status:                  input.ExpectedStatus,
		RecordingStatus:         RecordingActive,
	})
	if err == nil {
		signalRuntimeRecovery(journal.completeSucceeded)
	}
	return next, err
}

func (journal *runtimeRecoveryJournal) ExhaustRecorderRecovery(
	ctx context.Context,
	input ExhaustRecorderRecoveryInput,
) (LiveSession, error) {
	journal.mu.Lock()
	journal.exhaustCalls++
	journal.exhaustInputs = append(journal.exhaustInputs, input)
	if journal.exhaustFailures > 0 {
		journal.exhaustFailures--
		journal.mu.Unlock()
		return LiveSession{}, errors.New("injected exhaust failure")
	}
	journal.mu.Unlock()
	next, err := journal.repository.Transition(ctx, TransitionSessionInput{
		ID:                      input.SessionID,
		ExpectedStatus:          input.ExpectedStatus,
		ExpectedRecordingStatus: input.ExpectedRecordingStatus,
		ExpectedOperationID:     input.ExpectedOperationID,
		Status:                  input.ExpectedStatus,
		RecordingStatus:         RecordingUnavailable,
	})
	if err == nil {
		signalRuntimeRecovery(journal.exhaustSucceeded)
	}
	return next, err
}

func (journal *runtimeRecoveryJournal) CloseRecorderRecovery(
	_ context.Context,
	input CloseRecorderRecoveryInput,
) error {
	journal.mu.Lock()
	journal.closeCalls++
	journal.closeInputs = append(journal.closeInputs, input)
	if journal.closeFailures > 0 {
		journal.closeFailures--
		journal.mu.Unlock()
		return errors.New("injected close failure")
	}
	journal.mu.Unlock()
	signalRuntimeRecovery(journal.closeSucceeded)
	return nil
}

type failCloseRecorderRecoveryJournal struct {
	RecorderRecoveryJournal

	mu            sync.Mutex
	closeFailures int
	closeInputs   []CloseRecorderRecoveryInput
}

func (journal *failCloseRecorderRecoveryJournal) CloseRecorderRecovery(
	ctx context.Context,
	input CloseRecorderRecoveryInput,
) error {
	journal.mu.Lock()
	journal.closeInputs = append(journal.closeInputs, input)
	if journal.closeFailures > 0 {
		journal.closeFailures--
		journal.mu.Unlock()
		return errors.New("injected close failure")
	}
	journal.mu.Unlock()
	return journal.RecorderRecoveryJournal.CloseRecorderRecovery(ctx, input)
}

func (journal *failCloseRecorderRecoveryJournal) snapshotCloseInputs() []CloseRecorderRecoveryInput {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	return append([]CloseRecorderRecoveryInput(nil), journal.closeInputs...)
}

func signalRuntimeRecovery(channel chan struct{}) {
	select {
	case channel <- struct{}{}:
	default:
	}
}

type runtimeRecoveryClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *runtimeRecoveryClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *runtimeRecoveryClock) Advance(delay time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(delay)
	clock.mu.Unlock()
}

type runtimeRecoveryScheduler struct {
	mu      sync.Mutex
	delays  []time.Duration
	clock   *runtimeRecoveryClock
	advance bool
	started chan time.Duration
	wait    func(context.Context, time.Duration) error
}

func (scheduler *runtimeRecoveryScheduler) Wait(
	ctx context.Context,
	delay time.Duration,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	scheduler.mu.Lock()
	scheduler.delays = append(scheduler.delays, delay)
	scheduler.mu.Unlock()
	if scheduler.advance && scheduler.clock != nil {
		scheduler.clock.Advance(delay)
	}
	if scheduler.started != nil {
		select {
		case scheduler.started <- delay:
		default:
		}
	}
	if scheduler.wait != nil {
		return scheduler.wait(ctx, delay)
	}
	return ctx.Err()
}

func (scheduler *runtimeRecoveryScheduler) snapshotDelays() []time.Duration {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	return append([]time.Duration(nil), scheduler.delays...)
}

type runtimeRecoveryTakeoverFailureRepository struct {
	SessionRepository
	mu       sync.Mutex
	failures int
}

func (repository *runtimeRecoveryTakeoverFailureRepository) Transition(
	ctx context.Context,
	input TransitionSessionInput,
) (LiveSession, error) {
	if input.NextOperationID != "" &&
		input.Status == SessionRecording &&
		input.RecordingStatus == RecordingReconnecting {
		repository.mu.Lock()
		if repository.failures > 0 {
			repository.failures--
			repository.mu.Unlock()
			return LiveSession{}, errors.New("injected external takeover failure")
		}
		repository.mu.Unlock()
	}
	return repository.SessionRepository.Transition(ctx, input)
}

func waitRuntimeRecoverySignal(t *testing.T, channel <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func waitRuntimeRecoveryDelay(
	t *testing.T,
	channel <-chan time.Duration,
	label string,
) time.Duration {
	t.Helper()
	select {
	case delay := <-channel:
		return delay
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
		return 0
	}
}

func runtimeRecoveryRecorderCounts(recorder *fakeEventRecorder) (int, int) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.rebinds, recorder.stops
}

func runtimeRecoveryJournalCounts(
	journal *runtimeRecoveryJournal,
) (begin, successfulBegin, complete, exhaust, close int) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	return journal.beginCalls,
		journal.successfulBegins,
		journal.completeCalls,
		journal.exhaustCalls,
		journal.closeCalls
}

func openRuntimeRecoverySession(
	t *testing.T,
	recorder *fakeEventRecorder,
	scheduler *runtimeRecoveryScheduler,
	policy RecorderRecoveryPolicy,
) (Session, *runtimeRecoveryJournal, *runtimeRecoveryClock, string) {
	t.Helper()
	repository, store, _, roomID, now := openRepository(t)
	clock := &runtimeRecoveryClock{now: now}
	scheduler.clock = clock
	journal := newRuntimeRecoveryJournal(repository, newV7(t))
	// This fixture isolates recorder recovery. Do not accidentally advertise
	// the SQLite message journal through the repository interface.
	recorderOnlyRepository := struct{ SessionRepository }{SessionRepository: repository}
	coordinator, err := newTestCoordinator(recorderOnlyRepository, CoordinatorOptions{
		Now:               clock.Now,
		RecoveryJournal:   journal,
		RecoveryPolicy:    policy,
		RecoveryScheduler: scheduler,
		RecorderFactory: func(context.Context, LiveSession, OpenRequest, CaptureSource) (Recorder, error) {
			return recorder, nil
		},
	})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	session, err := coordinator.Open(context.Background(), OpenRequest{
		RoomConfigID:  roomID,
		OperationID:   newV7(t),
		RecordEnabled: true,
		StartedAt:     now,
	}, newFakeCaptureSource(nil))
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, _ = session.Finalize(ctx, newV7(t), FinalizeShutdown)
		cancel()
		_ = store.Close()
	})
	return session, journal, clock, recorder.currentAttemptID
}

func emitRuntimeRecoveryExit(
	recorder *fakeEventRecorder,
	attemptID string,
	errorCode string,
	occurredAt time.Time,
) {
	recorder.events <- RecorderEvent{
		Kind:       RecorderEventProcessExited,
		AttemptID:  attemptID,
		ErrorCode:  errorCode,
		OccurredAt: occurredAt.UTC().UnixMilli(),
	}
}

func assertRuntimeRecoveryStatus(
	t *testing.T,
	session Session,
	expected RecordingStatus,
) LiveSession {
	t.Helper()
	return waitForRecordingStatus(t, session, expected)
}

func formatRuntimeRecoveryDelays(delays []time.Duration) string {
	return fmt.Sprint(delays)
}

func TestCoordinatorRecorderRecoveryBoundsUncertainEventClock(t *testing.T) {
	tests := []struct {
		name           string
		clockShift     time.Duration
		eventAt        func(startedAt, now time.Time) int64
		wantOccurredAt func(startedAt, now time.Time) time.Time
		wantUncertain  bool
	}{
		{
			name:       "normal",
			clockShift: 10 * time.Second,
			eventAt: func(_ time.Time, now time.Time) int64 {
				return now.Add(-2 * time.Second).UnixMilli()
			},
			wantOccurredAt: func(_ time.Time, now time.Time) time.Time {
				return now.Add(-2 * time.Second)
			},
		},
		{
			name:       "zero",
			clockShift: 10 * time.Second,
			eventAt: func(time.Time, time.Time) int64 {
				return 0
			},
			wantOccurredAt: func(_ time.Time, now time.Time) time.Time { return now },
			wantUncertain:  true,
		},
		{
			name:       "future",
			clockShift: 10 * time.Second,
			eventAt: func(_ time.Time, now time.Time) int64 {
				return now.Add(time.Minute).UnixMilli()
			},
			wantOccurredAt: func(_ time.Time, now time.Time) time.Time { return now },
			wantUncertain:  true,
		},
		{
			name:       "before_session_start",
			clockShift: 10 * time.Second,
			eventAt: func(startedAt, _ time.Time) int64 {
				return startedAt.Add(-time.Minute).UnixMilli()
			},
			wantOccurredAt: func(startedAt, _ time.Time) time.Time { return startedAt },
			wantUncertain:  true,
		},
		{
			name:       "wall_clock_rolled_before_session_start",
			clockShift: -time.Minute,
			eventAt: func(startedAt, _ time.Time) int64 {
				return startedAt.UnixMilli()
			},
			wantOccurredAt: func(startedAt, _ time.Time) time.Time { return startedAt },
			wantUncertain:  true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			attemptID := newV7(t)
			recorder := newFakeEventRecorder(attemptID)
			scheduler := &runtimeRecoveryScheduler{
				wait: func(ctx context.Context, _ time.Duration) error {
					<-ctx.Done()
					return ctx.Err()
				},
			}
			session, journal, clock, emittedAttemptID := openRuntimeRecoverySession(
				t, recorder, scheduler, RecorderRecoveryPolicy{},
			)
			clock.Advance(test.clockShift)
			startedAt := time.UnixMilli(session.Snapshot().StartedAt).UTC()
			now := clock.Now().UTC()
			recorder.events <- RecorderEvent{
				Kind: RecorderEventProcessExited, AttemptID: emittedAttemptID,
				ErrorCode:  RecorderNetworkFailureErrorCode,
				OccurredAt: test.eventAt(startedAt, now),
			}
			waitRuntimeRecoverySignal(t, journal.beginSucceeded, "clock-bounded recovery begin")
			journal.mu.Lock()
			input := journal.beginInputs[0]
			journal.mu.Unlock()
			wantOccurredAt := test.wantOccurredAt(startedAt, now).UTC()
			if !input.OccurredAt.Equal(wantOccurredAt) ||
				input.ClockUncertain != test.wantUncertain ||
				input.ErrorCode != RecorderNetworkFailureErrorCode {
				t.Fatalf("bounded begin input = %+v, want occurred=%s uncertain=%t code=%s",
					input, wantOccurredAt, test.wantUncertain, RecorderNetworkFailureErrorCode)
			}
			assertRuntimeRecoveryStatus(t, session, RecordingReconnecting)
		})
	}
}

func TestCoordinatorRecorderRecoveryDefaultSchedule(t *testing.T) {
	attemptID := newV7(t)
	recorder := newFakeEventRecorder(attemptID)
	scheduler := &runtimeRecoveryScheduler{advance: true}
	var journal *runtimeRecoveryJournal
	var callMu sync.Mutex
	rebindCalls := 0
	beginMissing := false
	recorder.rebindFunc = func(context.Context, CaptureSource) error {
		callMu.Lock()
		defer callMu.Unlock()
		rebindCalls++
		journal.mu.Lock()
		beginMissing = beginMissing || journal.successfulBegins == 0
		journal.mu.Unlock()
		if rebindCalls < 5 {
			return ErrRecorderNetworkFailure
		}
		return nil
	}
	session, openedJournal, clock, emittedAttemptID := openRuntimeRecoverySession(
		t,
		recorder,
		scheduler,
		RecorderRecoveryPolicy{},
	)
	journal = openedJournal
	emitRuntimeRecoveryExit(
		recorder,
		emittedAttemptID,
		RecorderNetworkFailureErrorCode,
		clock.Now(),
	)
	waitRuntimeRecoverySignal(t, journal.completeSucceeded, "recovery completion")
	if current := assertRuntimeRecoveryStatus(t, session, RecordingActive); current.Status != SessionRecording {
		t.Fatalf("recovered session = %+v", current)
	}
	rebinds, stops := runtimeRecoveryRecorderCounts(recorder)
	if rebinds != 5 || stops != 0 {
		t.Fatalf("recorder calls = rebind:%d stop:%d, want rebind:5 stop:0", rebinds, stops)
	}
	callMu.Lock()
	missing := beginMissing
	callMu.Unlock()
	if missing {
		t.Fatal("Recorder.Rebind ran before BeginRecorderRecovery succeeded")
	}
	want := []time.Duration{
		time.Second,
		2 * time.Second,
		5 * time.Second,
		10 * time.Second,
		10 * time.Second,
	}
	if got := scheduler.snapshotDelays(); formatRuntimeRecoveryDelays(got) != formatRuntimeRecoveryDelays(want) {
		t.Fatalf("recovery delays = %v, want %v", got, want)
	}
}

func TestCoordinatorRecorderRecoveryAttemptAndWindowBounds(t *testing.T) {
	t.Run("maximum ten attempts", func(t *testing.T) {
		attemptID := newV7(t)
		recorder := newFakeEventRecorder(attemptID)
		recorder.rebindFunc = func(context.Context, CaptureSource) error {
			return ErrRecorderNetworkFailure
		}
		scheduler := &runtimeRecoveryScheduler{advance: true}
		session, journal, clock, emittedAttemptID := openRuntimeRecoverySession(
			t,
			recorder,
			scheduler,
			RecorderRecoveryPolicy{},
		)
		emitRuntimeRecoveryExit(
			recorder,
			emittedAttemptID,
			RecorderNetworkFailureErrorCode,
			clock.Now(),
		)
		waitRuntimeRecoverySignal(t, journal.exhaustSucceeded, "attempt exhaustion")
		assertRuntimeRecoveryStatus(t, session, RecordingUnavailable)
		rebinds, _ := runtimeRecoveryRecorderCounts(recorder)
		if rebinds != defaultRecorderRecoveryMaximumAttempts {
			t.Fatalf("rebind calls = %d, want %d", rebinds, defaultRecorderRecoveryMaximumAttempts)
		}
		delays := scheduler.snapshotDelays()
		if len(delays) != defaultRecorderRecoveryMaximumAttempts {
			t.Fatalf("scheduled retries = %d, want %d: %v", len(delays), defaultRecorderRecoveryMaximumAttempts, delays)
		}
		journal.mu.Lock()
		exhaust := journal.exhaustInputs[len(journal.exhaustInputs)-1]
		journal.mu.Unlock()
		if exhaust.RestartAttempts != defaultRecorderRecoveryMaximumAttempts ||
			exhaust.ErrorCode != RecorderRecoveryRetryExhaustedErrorCode {
			t.Fatalf("exhaust input = %+v", exhaust)
		}
	})

	t.Run("five minute window", func(t *testing.T) {
		attemptID := newV7(t)
		recorder := newFakeEventRecorder(attemptID)
		recorder.rebindFunc = func(context.Context, CaptureSource) error {
			return ErrRecorderNetworkFailure
		}
		scheduler := &runtimeRecoveryScheduler{}
		scheduler.wait = func(ctx context.Context, _ time.Duration) error {
			scheduler.clock.Advance(defaultRecorderRecoveryWindow)
			return ctx.Err()
		}
		session, journal, clock, emittedAttemptID := openRuntimeRecoverySession(
			t,
			recorder,
			scheduler,
			RecorderRecoveryPolicy{},
		)
		emitRuntimeRecoveryExit(
			recorder,
			emittedAttemptID,
			RecorderNetworkFailureErrorCode,
			clock.Now(),
		)
		waitRuntimeRecoverySignal(t, journal.exhaustSucceeded, "window exhaustion")
		assertRuntimeRecoveryStatus(t, session, RecordingUnavailable)
		rebinds, _ := runtimeRecoveryRecorderCounts(recorder)
		if rebinds != 0 {
			t.Fatalf("rebind calls after elapsed recovery window = %d, want 0", rebinds)
		}
		if got := scheduler.snapshotDelays(); len(got) != 1 || got[0] != time.Second {
			t.Fatalf("window-bound delays = %v, want [1s]", got)
		}
	})
}

func TestCoordinatorRecorderRecoveryPermanentErrorsDoNotRetry(t *testing.T) {
	for _, errorCode := range []string{
		RecorderLocalResourceErrorCode,
		RecorderDependencyFailureErrorCode,
	} {
		t.Run(errorCode, func(t *testing.T) {
			attemptID := newV7(t)
			recorder := newFakeEventRecorder(attemptID)
			scheduler := &runtimeRecoveryScheduler{advance: true}
			session, journal, clock, emittedAttemptID := openRuntimeRecoverySession(
				t,
				recorder,
				scheduler,
				RecorderRecoveryPolicy{},
			)
			emitRuntimeRecoveryExit(recorder, emittedAttemptID, errorCode, clock.Now())
			waitRuntimeRecoverySignal(t, journal.exhaustSucceeded, "permanent-error exhaustion")
			assertRuntimeRecoveryStatus(t, session, RecordingUnavailable)
			rebinds, _ := runtimeRecoveryRecorderCounts(recorder)
			if rebinds != 0 {
				t.Fatalf("permanent error rebind calls = %d, want 0", rebinds)
			}
			begin, successful, _, exhaust, _ := runtimeRecoveryJournalCounts(journal)
			if begin != 1 || successful != 1 || exhaust != 1 {
				t.Fatalf("journal calls = begin:%d successful:%d exhaust:%d", begin, successful, exhaust)
			}
			journal.mu.Lock()
			persistedCode := journal.exhaustInputs[0].ErrorCode
			journal.mu.Unlock()
			if persistedCode != errorCode {
				t.Fatalf("persisted permanent code = %q, want %q", persistedCode, errorCode)
			}
		})
	}
}

func TestCoordinatorRecorderRecoveryBeginPrecedesRebind(t *testing.T) {
	attemptID := newV7(t)
	recorder := newFakeEventRecorder(attemptID)
	scheduler := &runtimeRecoveryScheduler{advance: true}
	var journal *runtimeRecoveryJournal
	var checkMu sync.Mutex
	rebindBeforeBegin := false
	recorder.rebindFunc = func(context.Context, CaptureSource) error {
		journal.mu.Lock()
		begun := journal.successfulBegins > 0
		journal.mu.Unlock()
		checkMu.Lock()
		rebindBeforeBegin = rebindBeforeBegin || !begun
		checkMu.Unlock()
		return nil
	}
	_, openedJournal, clock, emittedAttemptID := openRuntimeRecoverySession(
		t,
		recorder,
		scheduler,
		RecorderRecoveryPolicy{},
	)
	journal = openedJournal
	journal.mu.Lock()
	journal.beginFailures = 1
	journal.mu.Unlock()
	emitRuntimeRecoveryExit(recorder, emittedAttemptID, RecorderProcessExitedErrorCode, clock.Now())
	waitRuntimeRecoverySignal(t, journal.completeSucceeded, "completion after begin retry")
	begin, successful, complete, _, _ := runtimeRecoveryJournalCounts(journal)
	rebinds, _ := runtimeRecoveryRecorderCounts(recorder)
	checkMu.Lock()
	violation := rebindBeforeBegin
	checkMu.Unlock()
	if begin != 2 || successful != 1 || complete != 1 || rebinds != 1 || violation {
		t.Fatalf(
			"begin fencing = calls:%d successful:%d complete:%d rebinds:%d violation:%t",
			begin,
			successful,
			complete,
			rebinds,
			violation,
		)
	}
}

func TestCoordinatorRecorderRecoveryCompletionRetryDoesNotRebind(t *testing.T) {
	attemptID := newV7(t)
	recorder := newFakeEventRecorder(attemptID)
	scheduler := &runtimeRecoveryScheduler{advance: true}
	_, journal, clock, emittedAttemptID := openRuntimeRecoverySession(
		t,
		recorder,
		scheduler,
		RecorderRecoveryPolicy{},
	)
	journal.mu.Lock()
	journal.completeFailures = 1
	journal.mu.Unlock()
	emitRuntimeRecoveryExit(recorder, emittedAttemptID, RecorderProcessExitedErrorCode, clock.Now())
	waitRuntimeRecoverySignal(t, journal.completeSucceeded, "completion persistence retry")
	_, _, complete, _, _ := runtimeRecoveryJournalCounts(journal)
	rebinds, _ := runtimeRecoveryRecorderCounts(recorder)
	journal.mu.Lock()
	firstCompletedAt := journal.completeInputs[0].CompletedAt
	secondCompletedAt := journal.completeInputs[1].CompletedAt
	journal.mu.Unlock()
	if complete != 2 || rebinds != 1 {
		t.Fatalf("completion retry = complete:%d rebind:%d, want complete:2 rebind:1", complete, rebinds)
	}
	if !firstCompletedAt.Equal(secondCompletedAt) {
		t.Fatalf("completion retry changed idempotence time: %s != %s", firstCompletedAt, secondCompletedAt)
	}
}

func TestCoordinatorRecorderRecoveryExhaustionRetryKeepsIdempotenceTime(t *testing.T) {
	attemptID := newV7(t)
	recorder := newFakeEventRecorder(attemptID)
	scheduler := &runtimeRecoveryScheduler{advance: true}
	_, journal, clock, emittedAttemptID := openRuntimeRecoverySession(
		t,
		recorder,
		scheduler,
		RecorderRecoveryPolicy{},
	)
	journal.mu.Lock()
	journal.exhaustFailures = 1
	journal.mu.Unlock()
	emitRuntimeRecoveryExit(
		recorder,
		emittedAttemptID,
		RecorderLocalResourceErrorCode,
		clock.Now(),
	)
	waitRuntimeRecoverySignal(t, journal.exhaustSucceeded, "exhaustion persistence retry")
	_, _, _, exhaust, _ := runtimeRecoveryJournalCounts(journal)
	rebinds, _ := runtimeRecoveryRecorderCounts(recorder)
	journal.mu.Lock()
	firstExhaustedAt := journal.exhaustInputs[0].ExhaustedAt
	secondExhaustedAt := journal.exhaustInputs[1].ExhaustedAt
	journal.mu.Unlock()
	if exhaust != 2 || rebinds != 0 {
		t.Fatalf("exhaustion retry = exhaust:%d rebind:%d, want exhaust:2 rebind:0", exhaust, rebinds)
	}
	if !firstExhaustedAt.Equal(secondExhaustedAt) {
		t.Fatalf("exhaustion retry changed idempotence time: %s != %s", firstExhaustedAt, secondExhaustedAt)
	}
}

func TestCoordinatorRealRecorderHistoricalExitFinalizesCompletedIncomplete(t *testing.T) {
	repository, store, _, roomID, now := openRepository(t)
	defer store.Close()
	clock := &runtimeRecoveryClock{now: now}
	recoveryStarted := make(chan time.Duration, 1)
	recoveryRelease := make(chan struct{})
	scheduler := &runtimeRecoveryScheduler{
		clock: clock, advance: true, started: recoveryStarted,
		wait: func(ctx context.Context, _ time.Duration) error {
			select {
			case <-recoveryRelease:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
	journal := newRuntimeRecoveryJournal(repository, newV7(t))
	recorderOnlyRepository := struct{ SessionRepository }{SessionRepository: repository}
	firstProcess := newRecorderTestProcess()
	secondProcess := newRecorderTestProcess()
	secondProcess.quitExits = true
	starter := &recorderTestStarter{results: []recorderTestStartResult{
		{process: firstProcess}, {process: secondProcess},
	}}
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{
		{recorderTestCandidate("first", "flv", "hd", "h264", "https://recovery.example.invalid/first.flv", 1)},
		{recorderTestCandidate("second", "flv", "hd", "h264", "https://recovery.example.invalid/second.flv", 1)},
	}}
	var recorder *FFmpegRecorder
	coordinator, err := newTestCoordinator(recorderOnlyRepository, CoordinatorOptions{
		Now:               clock.Now,
		RecoveryJournal:   journal,
		RecoveryScheduler: scheduler,
		RecorderFactory: func(ctx context.Context, _ LiveSession, _ OpenRequest, _ CaptureSource) (Recorder, error) {
			built, buildErr := newFFmpegRecorder(
				ctx, source, recorderTestOptions(t), recorderTestDependencies(starter), nil,
			)
			recorder = built
			return built, buildErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := coordinator.Open(context.Background(), OpenRequest{
		RoomConfigID: roomID, OperationID: newV7(t), RecordEnabled: true, StartedAt: now,
	}, source)
	if err != nil {
		t.Fatal(err)
	}
	finalized := false
	t.Cleanup(func() {
		if finalized {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, _ = session.Finalize(ctx, newV7(t), FinalizeShutdown)
		cancel()
	})

	firstProcess.signal()
	assertRuntimeRecoveryStatus(t, session, RecordingReconnecting)
	if delay := waitRuntimeRecoveryDelay(t, recoveryStarted, "real recorder recovery"); delay != time.Second {
		t.Fatalf("first recovery delay = %s, want 1s", delay)
	}
	close(recoveryRelease)
	assertRuntimeRecoveryStatus(t, session, RecordingActive)
	if recorder == nil || source.resolveCalls() != 2 || len(starter.configSnapshot()) != 2 {
		t.Fatalf("real recovery topology = recorder:%t resolves:%d starts:%d", recorder != nil, source.resolveCalls(), len(starter.configSnapshot()))
	}

	terminal, err := session.Finalize(context.Background(), newV7(t), FinalizeOffline)
	if err != nil {
		t.Fatalf("FinalizeOffline() error = %v", err)
	}
	finalized = true
	if terminal.Status != SessionCompleted || terminal.RecordingStatus != RecordingIncomplete || terminal.EndedAt == nil {
		t.Fatalf("historical exit terminal = %+v, want completed/incomplete", terminal)
	}
	_, _, complete, exhaust, closeCalls := runtimeRecoveryJournalCounts(journal)
	if complete != 1 || exhaust != 0 || closeCalls != 0 {
		t.Fatalf("recovery journal = complete:%d exhaust:%d close:%d", complete, exhaust, closeCalls)
	}
}

func TestCoordinatorRecorderRecoveryFinalizeCancelsTimerAndRebind(t *testing.T) {
	t.Run("scheduled timer", func(t *testing.T) {
		attemptID := newV7(t)
		recorder := newFakeEventRecorder(attemptID)
		started := make(chan time.Duration, 4)
		scheduler := &runtimeRecoveryScheduler{
			started: started,
			wait: func(ctx context.Context, _ time.Duration) error {
				<-ctx.Done()
				return ctx.Err()
			},
		}
		session, journal, clock, emittedAttemptID := openRuntimeRecoverySession(
			t,
			recorder,
			scheduler,
			RecorderRecoveryPolicy{},
		)
		emitRuntimeRecoveryExit(recorder, emittedAttemptID, RecorderProcessExitedErrorCode, clock.Now())
		if delay := waitRuntimeRecoveryDelay(t, started, "scheduled recovery timer"); delay != time.Second {
			t.Fatalf("first retry delay = %s, want 1s", delay)
		}
		terminal, err := session.Finalize(context.Background(), newV7(t), FinalizeOffline)
		if err != nil {
			t.Fatal(err)
		}
		if terminal.Status != SessionCompleted || terminal.RecordingStatus != RecordingIncomplete {
			t.Fatalf("terminal session = %+v", terminal)
		}
		rebinds, _ := runtimeRecoveryRecorderCounts(recorder)
		_, _, complete, _, closeCalls := runtimeRecoveryJournalCounts(journal)
		if rebinds != 0 || complete != 0 || closeCalls != 1 {
			t.Fatalf("timer cancellation = rebind:%d complete:%d close:%d", rebinds, complete, closeCalls)
		}
	})

	t.Run("in-flight rebind", func(t *testing.T) {
		attemptID := newV7(t)
		recorder := newFakeEventRecorder(attemptID)
		rebindStarted := make(chan struct{})
		var startOnce sync.Once
		recorder.rebindFunc = func(ctx context.Context, _ CaptureSource) error {
			startOnce.Do(func() { close(rebindStarted) })
			<-ctx.Done()
			return ctx.Err()
		}
		scheduler := &runtimeRecoveryScheduler{advance: true}
		session, journal, clock, emittedAttemptID := openRuntimeRecoverySession(
			t,
			recorder,
			scheduler,
			RecorderRecoveryPolicy{},
		)
		emitRuntimeRecoveryExit(recorder, emittedAttemptID, RecorderProcessExitedErrorCode, clock.Now())
		select {
		case <-rebindStarted:
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for in-flight rebind")
		}
		terminal, err := session.Finalize(context.Background(), newV7(t), FinalizeOffline)
		if err != nil {
			t.Fatal(err)
		}
		if terminal.Status != SessionCompleted || terminal.RecordingStatus != RecordingIncomplete {
			t.Fatalf("terminal session = %+v", terminal)
		}
		rebinds, _ := runtimeRecoveryRecorderCounts(recorder)
		_, _, complete, _, closeCalls := runtimeRecoveryJournalCounts(journal)
		if rebinds != 1 || complete != 0 || closeCalls != 1 {
			t.Fatalf("in-flight cancellation = rebind:%d complete:%d close:%d", rebinds, complete, closeCalls)
		}
	})
}

func TestCoordinatorFinalizeRetriesSQLiteRecoveryGapCloseBeforeTerminal(t *testing.T) {
	repository, store, _, roomID, now := openRepository(t)
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close storage: %v", err)
		}
	})

	attemptID := newV7(t)
	recorder := newFakeEventRecorder(attemptID)
	sink := &fakeEventSink{}
	source := newFakeCaptureSource(nil)
	clock := &runtimeRecoveryClock{now: now}
	retryStarted := make(chan time.Duration, 1)
	scheduler := &runtimeRecoveryScheduler{
		clock:   clock,
		started: retryStarted,
		wait: func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	journal := &failCloseRecorderRecoveryJournal{
		RecorderRecoveryJournal: repository,
		closeFailures:           1,
	}
	coordinator, err := newTestCoordinator(repository, CoordinatorOptions{
		Now:               clock.Now,
		RecoveryJournal:   journal,
		RecoveryScheduler: scheduler,
		RecorderFactory: func(context.Context, LiveSession, OpenRequest, CaptureSource) (Recorder, error) {
			return recorder, nil
		},
		EventSinkFactory: func(context.Context, LiveSession, OpenRequest) (EventSink, error) {
			return sink, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	startedAt := now.Add(-time.Minute)
	session, err := coordinator.Open(context.Background(), OpenRequest{
		RoomConfigID:  roomID,
		OperationID:   newV7(t),
		RecordEnabled: true,
		StartedAt:     startedAt,
	}, source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, _ = session.Finalize(ctx, newV7(t), FinalizeShutdown)
		cancel()
	})

	current := session.Snapshot()
	opened, err := repository.OpenSessionMedia(context.Background(), OpenSessionMediaInput{
		SessionID:    current.ID,
		RelativePath: "recovery-runtime/" + current.ID,
		StartedAt:    startedAt.UnixMilli(),
	})
	if err != nil {
		t.Fatalf("OpenSessionMedia() error = %v", err)
	}
	attempt := MediaAttempt{
		ID:             attemptID,
		Ordinal:        1,
		StartedAt:      startedAt.Add(time.Second).UnixMilli(),
		SegmentSeconds: 300,
		Committed:      true,
		Clean:          false,
		Protocol:       "flv",
		Codec:          "h264",
	}
	if _, err := repository.PersistMediaSnapshot(context.Background(), PersistMediaSnapshotInput{
		SessionID:        current.ID,
		ExpectedRevision: opened.Session.ManifestRevision,
		State:            SessionMediaOpen,
		Attempts:         []MediaAttempt{attempt},
		UpdatedAt:        now.Add(-time.Second).UnixMilli(),
	}); err != nil {
		t.Fatalf("PersistMediaSnapshot() error = %v", err)
	}

	emitRuntimeRecoveryExit(
		recorder,
		attemptID,
		RecorderNetworkFailureErrorCode,
		clock.Now(),
	)
	if delay := waitRuntimeRecoveryDelay(t, retryStarted, "SQLite recovery retry"); delay != time.Second {
		t.Fatalf("first recovery delay = %s, want 1s", delay)
	}
	reconnecting := assertRuntimeRecoveryStatus(t, session, RecordingReconnecting)
	gap, found, err := queryRecorderRecoveryGap(
		context.Background(),
		store.Writer(),
		recorderRecoveryGapSelectSQL+` WHERE session_id = ? AND kind = ?`,
		reconnecting.ID,
		recorderRecoveryGapKind,
	)
	if err != nil || !found || gap.EndedAtMS != nil {
		t.Fatalf("open runtime recovery gap = (%+v, %t, %v)", gap, found, err)
	}

	firstOperationID := newV7(t)
	first, firstErr := session.Finalize(
		context.Background(),
		firstOperationID,
		FinalizeOffline,
	)
	if firstErr == nil || !strings.Contains(firstErr.Error(), "injected close failure") {
		t.Fatalf("first Finalize() error = %v, want injected gap-close failure", firstErr)
	}
	if first.Status != SessionFinalizing || first.RecordingStatus != RecordingFinalizing ||
		first.OperationID != firstOperationID {
		t.Fatalf("first Finalize() = %+v, want finalizing with first operation", first)
	}
	persistedFirst, err := repository.Get(context.Background(), first.ID)
	if err != nil {
		t.Fatalf("Get(after failed close) error = %v", err)
	}
	gapAfterFirst, found, err := queryRecorderRecoveryGap(
		context.Background(),
		store.Writer(),
		recorderRecoveryGapSelectSQL+` WHERE session_id = ? AND id = ?`,
		first.ID,
		gap.ID,
	)
	if err != nil || !found || gapAfterFirst.EndedAtMS != nil ||
		persistedFirst.Status != SessionFinalizing || persistedFirst.OperationID != firstOperationID {
		t.Fatalf(
			"failed close durable state = session:%+v gap:%+v found:%t err:%v",
			persistedFirst,
			gapAfterFirst,
			found,
			err,
		)
	}
	rebinds, stops := runtimeRecoveryRecorderCounts(recorder)
	sink.mu.Lock()
	flushes := sink.flushes
	sink.mu.Unlock()
	coordinator.registryMu.Lock()
	registered := coordinator.runtimes[roomID] != nil
	coordinator.registryMu.Unlock()
	if rebinds != 0 || stops != 1 || flushes != 1 || !registered {
		t.Fatalf(
			"failed close cleanup = rebind:%d stop:%d flush:%d registered:%t",
			rebinds,
			stops,
			flushes,
			registered,
		)
	}

	secondOperationID := newV7(t)
	terminal, err := session.Finalize(
		context.Background(),
		secondOperationID,
		FinalizeOffline,
	)
	if err != nil {
		t.Fatalf("second Finalize() error = %v", err)
	}
	if terminal.Status != SessionCompleted || terminal.RecordingStatus != RecordingIncomplete ||
		terminal.OperationID != secondOperationID {
		t.Fatalf("second Finalize() = %+v, want completed/incomplete with rotated operation", terminal)
	}
	gapAfterSecond, found, err := queryRecorderRecoveryGap(
		context.Background(),
		store.Writer(),
		recorderRecoveryGapSelectSQL+` WHERE session_id = ? AND id = ?`,
		terminal.ID,
		gap.ID,
	)
	if err != nil || !found || gapAfterSecond.EndedAtMS == nil {
		t.Fatalf("closed runtime recovery gap = (%+v, %t, %v)", gapAfterSecond, found, err)
	}
	var openGaps int
	if err := store.Writer().QueryRow(
		`SELECT COUNT(*) FROM capture_gaps WHERE session_id = ? AND kind = ? AND ended_at IS NULL`,
		terminal.ID,
		recorderRecoveryGapKind,
	).Scan(&openGaps); err != nil || openGaps != 0 {
		t.Fatalf("open recovery gaps after terminal = %d, err = %v", openGaps, err)
	}
	inputs := journal.snapshotCloseInputs()
	if len(inputs) != 2 || inputs[0].ExpectedOperationID != firstOperationID ||
		inputs[1].ExpectedOperationID != secondOperationID ||
		!inputs[0].ClosedAt.Equal(inputs[1].ClosedAt) ||
		gapAfterSecond.EndedAtMS == nil || *gapAfterSecond.EndedAtMS != inputs[1].ClosedAt.UnixMilli() {
		t.Fatalf("gap close retries = %+v, closed gap = %+v", inputs, gapAfterSecond)
	}
	_, stops = runtimeRecoveryRecorderCounts(recorder)
	sink.mu.Lock()
	flushes = sink.flushes
	sink.mu.Unlock()
	coordinator.registryMu.Lock()
	registered = coordinator.runtimes[roomID] != nil
	coordinator.registryMu.Unlock()
	if stops != 1 || flushes != 1 || registered {
		t.Fatalf(
			"retry cleanup = stop:%d flush:%d registered:%t",
			stops,
			flushes,
			registered,
		)
	}
}

func TestCoordinatorExternalRebindSupersedesRecoveryGeneration(t *testing.T) {
	attemptID := newV7(t)
	recorder := newFakeEventRecorder(attemptID)
	started := make(chan time.Duration, 4)
	scheduler := &runtimeRecoveryScheduler{
		started: started,
		wait: func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	session, journal, clock, emittedAttemptID := openRuntimeRecoverySession(
		t,
		recorder,
		scheduler,
		RecorderRecoveryPolicy{},
	)
	emitRuntimeRecoveryExit(recorder, emittedAttemptID, RecorderNetworkFailureErrorCode, clock.Now())
	waitRuntimeRecoveryDelay(t, started, "old recovery generation")
	newOperationID := newV7(t)
	rebound, err := session.Rebind(
		context.Background(),
		newOperationID,
		newFakeCaptureSource(nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	if rebound.OperationID != newOperationID || rebound.RecordingStatus != RecordingActive {
		t.Fatalf("external rebound session = %+v", rebound)
	}
	current := session.Snapshot()
	if current.OperationID != newOperationID || current.RecordingStatus != RecordingActive {
		t.Fatalf("stale generation overwrote external rebind: %+v", current)
	}
	rebinds, _ := runtimeRecoveryRecorderCounts(recorder)
	_, _, complete, exhaust, _ := runtimeRecoveryJournalCounts(journal)
	if rebinds != 1 || complete != 1 || exhaust != 0 {
		t.Fatalf("generation fencing = rebind:%d complete:%d exhaust:%d", rebinds, complete, exhaust)
	}
	journal.mu.Lock()
	completedOperation := journal.completeInputs[0].ExpectedOperationID
	completedAttempts := journal.completeInputs[0].RestartAttempts
	journal.mu.Unlock()
	if completedOperation != newOperationID {
		t.Fatalf("completed operation = %q, want %q", completedOperation, newOperationID)
	}
	if completedAttempts != 0 {
		t.Fatalf("external rebind persisted automated attempts = %d, want 0", completedAttempts)
	}
}

func TestCoordinatorFailedExternalTakeoverResumesRecoveryGeneration(t *testing.T) {
	repository, store, _, roomID, now := openRepository(t)
	clock := &runtimeRecoveryClock{now: now}
	started := make(chan time.Duration, 4)
	scheduler := &runtimeRecoveryScheduler{
		clock:   clock,
		started: started,
		wait: func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	attemptID := newV7(t)
	recorder := newFakeEventRecorder(attemptID)
	journal := newRuntimeRecoveryJournal(repository, newV7(t))
	takeoverRepository := &runtimeRecoveryTakeoverFailureRepository{
		SessionRepository: repository,
		failures:          1,
	}
	coordinator, err := newTestCoordinator(takeoverRepository, CoordinatorOptions{
		Now:               clock.Now,
		RecoveryJournal:   journal,
		RecoveryScheduler: scheduler,
		RecorderFactory: func(context.Context, LiveSession, OpenRequest, CaptureSource) (Recorder, error) {
			return recorder, nil
		},
	})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	initialOperationID := newV7(t)
	session, err := coordinator.Open(context.Background(), OpenRequest{
		RoomConfigID:  roomID,
		OperationID:   initialOperationID,
		RecordEnabled: true,
		StartedAt:     now,
	}, newFakeCaptureSource(nil))
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, _ = session.Finalize(ctx, newV7(t), FinalizeShutdown)
		cancel()
		_ = store.Close()
	})

	emitRuntimeRecoveryExit(
		recorder,
		attemptID,
		RecorderNetworkFailureErrorCode,
		clock.Now(),
	)
	waitRuntimeRecoveryDelay(t, started, "initial recovery generation")
	_, rebindErr := session.Rebind(
		context.Background(),
		newV7(t),
		newFakeCaptureSource(nil),
	)
	if rebindErr == nil {
		t.Fatal("external takeover error = nil, want injected failure")
	}
	if delay := waitRuntimeRecoveryDelay(t, started, "resumed recovery generation"); delay != time.Second {
		t.Fatalf("resumed delay = %s, want 1s", delay)
	}
	current := session.Snapshot()
	if current.OperationID != initialOperationID ||
		current.RecordingStatus != RecordingReconnecting {
		t.Fatalf("failed takeover stranded or overwrote recovery: %+v", current)
	}
	rebinds, _ := runtimeRecoveryRecorderCounts(recorder)
	if rebinds != 0 {
		t.Fatalf("failed takeover invoked recorder %d times", rebinds)
	}
}

func TestCoordinatorRecorderRecoveryEventsAreSanitized(t *testing.T) {
	attemptID := newV7(t)
	recorder := newFakeEventRecorder(attemptID)
	started := make(chan time.Duration, 4)
	scheduler := &runtimeRecoveryScheduler{
		started: started,
		wait: func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	session, _, clock, emittedAttemptID := openRuntimeRecoverySession(
		t,
		recorder,
		scheduler,
		RecorderRecoveryPolicy{},
	)
	eventSource, ok := session.(SessionRecoveryEventSource)
	if !ok {
		t.Fatal("session does not expose sanitized recovery events")
	}
	emitRuntimeRecoveryExit(recorder, emittedAttemptID, RecorderNetworkFailureErrorCode, clock.Now())
	waitRuntimeRecoveryDelay(t, started, "sanitized recovery event")
	var event SessionRecoveryEvent
	select {
	case event = <-eventSource.RecoveryEvents():
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for recovery event")
	}
	if event.SessionID == "" ||
		event.OperationID == "" ||
		event.State != SessionRecoveryRetryScheduled ||
		event.ErrorCode != RecorderNetworkFailureErrorCode ||
		event.RecordingStatus != RecordingReconnecting {
		t.Fatalf("recovery event = %+v", event)
	}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	publicRepresentations := []string{
		string(payload),
		event.String(),
		fmt.Sprintf("%#v", event),
	}
	secrets := []string{
		emittedAttemptID,
		"https://pull.example.invalid/live.flv?token=super-secret",
		"D:\\private\\recordings\\room-1",
		"ffmpeg: connection failed against private upstream",
	}
	for _, representation := range publicRepresentations {
		for _, secret := range secrets {
			if strings.Contains(representation, secret) {
				t.Fatalf("public recovery event leaked %q in %q", secret, representation)
			}
		}
	}
}

func TestRecorderRecoveryPolicyClampsProductionBounds(t *testing.T) {
	policy := normalizeRecorderRecoveryPolicy(RecorderRecoveryPolicy{
		Backoff:     []time.Duration{-time.Second, 30 * time.Second},
		MaxAttempts: 99,
		Window:      time.Hour,
	})
	if policy.MaxAttempts != defaultRecorderRecoveryMaximumAttempts ||
		policy.Window != defaultRecorderRecoveryWindow ||
		len(policy.Backoff) != 1 ||
		policy.Backoff[0] != maximumRecorderRecoveryDelay {
		t.Fatalf("normalized recovery policy = %+v", policy)
	}
}
