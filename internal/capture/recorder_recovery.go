package capture

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	RecorderRecoveryRetryExhaustedErrorCode = "RECORDING_RETRY_EXHAUSTED"
	RecorderRecoveryPersistenceErrorCode    = "RECORDING_RECOVERY_PERSISTENCE_FAILED"
	defaultRecorderRecoveryMaximumAttempts  = 10
	defaultRecorderRecoveryWindow           = 5 * time.Minute
	maximumRecorderRecoveryDelay            = 10 * time.Second
	defaultRecorderRecoveryEventBuffer      = 16
	defaultRecorderRecoveryCompletionRetry  = 100 * time.Millisecond
)

var defaultRecorderRecoveryBackoff = [...]time.Duration{
	time.Second,
	2 * time.Second,
	5 * time.Second,
	10 * time.Second,
}

// RecorderRecoveryPolicy bounds one process-exit recovery generation. Backoff
// repeats its final value after the configured slice is exhausted.
type RecorderRecoveryPolicy struct {
	Backoff     []time.Duration
	MaxAttempts int
	Window      time.Duration
}

func DefaultRecorderRecoveryPolicy() RecorderRecoveryPolicy {
	return RecorderRecoveryPolicy{
		Backoff:     append([]time.Duration(nil), defaultRecorderRecoveryBackoff[:]...),
		MaxAttempts: defaultRecorderRecoveryMaximumAttempts,
		Window:      defaultRecorderRecoveryWindow,
	}
}

func normalizeRecorderRecoveryPolicy(policy RecorderRecoveryPolicy) RecorderRecoveryPolicy {
	defaults := DefaultRecorderRecoveryPolicy()
	if policy.MaxAttempts <= 0 {
		policy.MaxAttempts = defaults.MaxAttempts
	}
	if policy.MaxAttempts > defaultRecorderRecoveryMaximumAttempts {
		policy.MaxAttempts = defaultRecorderRecoveryMaximumAttempts
	}
	if policy.Window <= 0 {
		policy.Window = defaults.Window
	}
	if policy.Window > defaultRecorderRecoveryWindow {
		policy.Window = defaultRecorderRecoveryWindow
	}
	delays := make([]time.Duration, 0, len(policy.Backoff))
	for _, delay := range policy.Backoff {
		if delay <= 0 {
			continue
		}
		if delay > maximumRecorderRecoveryDelay {
			delay = maximumRecorderRecoveryDelay
		}
		delays = append(delays, delay)
	}
	if len(delays) == 0 {
		delays = defaults.Backoff
	}
	policy.Backoff = delays
	return policy
}

func recorderRecoveryDelay(policy RecorderRecoveryPolicy, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	index := attempt - 1
	if index >= len(policy.Backoff) {
		index = len(policy.Backoff) - 1
	}
	return policy.Backoff[index]
}

// RecorderRecoveryScheduler is deliberately small so retry tests can advance
// virtual time without sleeping.
type RecorderRecoveryScheduler interface {
	Wait(context.Context, time.Duration) error
}

type recorderRecoveryTimerScheduler struct{}

func (recorderRecoveryTimerScheduler) Wait(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// RecorderRecoveryJournal owns the atomic session-status and capture-gap
// mutations used by runtime recorder recovery. Begin must durably move the
// session to reconnecting before the caller invokes Recorder.Rebind.
type RecorderRecoveryJournal interface {
	BeginRecorderRecovery(context.Context, BeginRecorderRecoveryInput) (RecorderRecoveryJournalEntry, error)
	CompleteRecorderRecovery(context.Context, CompleteRecorderRecoveryInput) (LiveSession, error)
	ExhaustRecorderRecovery(context.Context, ExhaustRecorderRecoveryInput) (LiveSession, error)
	CloseRecorderRecovery(context.Context, CloseRecorderRecoveryInput) error
}

type RecorderRecoveryJournalEntry struct {
	Session LiveSession
	GapID   string
}

type BeginRecorderRecoveryInput struct {
	SessionID               string
	ExpectedStatus          SessionStatus
	ExpectedRecordingStatus RecordingStatus
	ExpectedOperationID     string
	SourceAttemptID         string
	ErrorCode               string
	OccurredAt              time.Time
	ClockUncertain          bool
}

type CompleteRecorderRecoveryInput struct {
	SessionID               string
	GapID                   string
	ExpectedStatus          SessionStatus
	ExpectedRecordingStatus RecordingStatus
	ExpectedOperationID     string
	RestartAttempts         int
	CompletedAt             time.Time
}

type ExhaustRecorderRecoveryInput struct {
	SessionID               string
	GapID                   string
	ExpectedStatus          SessionStatus
	ExpectedRecordingStatus RecordingStatus
	ExpectedOperationID     string
	RestartAttempts         int
	ErrorCode               string
	ExhaustedAt             time.Time
}

type CloseRecorderRecoveryInput struct {
	SessionID               string
	GapID                   string
	ExpectedStatus          SessionStatus
	ExpectedRecordingStatus RecordingStatus
	ExpectedOperationID     string
	Recovered               bool
	ErrorCode               string
	ClosedAt                time.Time
}

type SessionRecoveryState string

const (
	SessionRecoveryRetryScheduled SessionRecoveryState = "retry_scheduled"
	SessionRecoveryRecovered      SessionRecoveryState = "recovered"
	SessionRecoveryExhausted      SessionRecoveryState = "exhausted"
)

// SessionRecoveryEvent is a public, correlation-only event. It intentionally
// contains no stream URL, filesystem path, recorder attempt ID, or raw error.
type SessionRecoveryEvent struct {
	SessionID       string               `json:"sessionId"`
	OperationID     string               `json:"operationId"`
	State           SessionRecoveryState `json:"state"`
	RecordingStatus RecordingStatus      `json:"recordingStatus"`
	ErrorCode       string               `json:"errorCode"`
	RetryAt         int64                `json:"retryAt,omitempty"`
	RestartAttempt  int                  `json:"restartAttempt"`
	OccurredAt      int64                `json:"occurredAt"`
}

func (event SessionRecoveryEvent) String() string {
	return fmt.Sprintf(
		"session recovery event: session=%s operation=%s state=%s recording=%s code=%s retry_at=%d attempt=%d occurred_at=%d",
		event.SessionID,
		event.OperationID,
		event.State,
		event.RecordingStatus,
		event.ErrorCode,
		event.RetryAt,
		event.RestartAttempt,
		event.OccurredAt,
	)
}

func (event SessionRecoveryEvent) GoString() string {
	return event.String()
}

type SessionRecoveryEventSource interface {
	RecoveryEvents() <-chan SessionRecoveryEvent
}

func normalizeRecorderRecoveryErrorCode(code string) string {
	switch code {
	case RecorderProcessExitedErrorCode,
		RecorderStreamExpiredErrorCode,
		RecorderNetworkFailureErrorCode,
		RecorderUnsupportedInputErrorCode,
		RecorderLocalResourceErrorCode,
		RecorderDependencyFailureErrorCode,
		RecorderRecoveryRetryExhaustedErrorCode,
		RecorderRecoveryPersistenceErrorCode:
		return code
	default:
		return RecorderProcessExitedErrorCode
	}
}

func retryableRecorderRecoveryErrorCode(code string) bool {
	switch normalizeRecorderRecoveryErrorCode(code) {
	case RecorderLocalResourceErrorCode, RecorderDependencyFailureErrorCode:
		return false
	default:
		return true
	}
}

// beginRecorderRecoveryLocked is called with sessionRuntime.operationMu held.
// It returns false only when the recorder watcher must retry the same event.
func (s *sessionRuntime) beginRecorderRecoveryLocked(
	recorder Recorder,
	event RecorderEvent,
) bool {
	journal := s.coordinator.recoveryJournal
	if journal == nil {
		return s.markRecorderUnavailableWithoutRecoveryLocked(recorder)
	}
	errorCode := normalizeRecorderRecoveryErrorCode(event.ErrorCode)

	s.mu.Lock()
	current := s.current
	if s.recoveryGapID != "" && current.RecordingStatus == RecordingReconnecting {
		s.recoveryErrorCode = errorCode
		recoveryCtx, generation := s.startRecorderRecoveryGenerationLocked(recorder)
		s.mu.Unlock()
		if retryableRecorderRecoveryErrorCode(errorCode) {
			go s.runRecorderRecovery(recoveryCtx, generation, recorder)
		} else {
			go s.persistRecorderRecoveryExhaustion(recoveryCtx, generation, recorder, errorCode)
		}
		return true
	}
	s.mu.Unlock()

	now := s.coordinator.now().UTC()
	occurredAt, clockUncertain := boundRecorderRecoveryOccurredAt(
		event.OccurredAt, current.StartedAt, now,
	)
	commitCtx, cancel := s.coordinator.commitContext(context.Background())
	entry, beginErr := journal.BeginRecorderRecovery(commitCtx, BeginRecorderRecoveryInput{
		SessionID:               current.ID,
		ExpectedStatus:          current.Status,
		ExpectedRecordingStatus: current.RecordingStatus,
		ExpectedOperationID:     current.OperationID,
		SourceAttemptID:         event.AttemptID,
		ErrorCode:               errorCode,
		OccurredAt:              occurredAt,
		ClockUncertain:          clockUncertain,
	})
	cancel()
	confirmed := entry.GapID != "" &&
		entry.Session.ID == current.ID &&
		entry.Session.Status == current.Status &&
		entry.Session.RecordingStatus == RecordingReconnecting &&
		entry.Session.OperationID == current.OperationID
	if beginErr != nil && !confirmed {
		if errors.Is(beginErr, ErrRecoveryContractInvalid) ||
			errors.Is(beginErr, ErrRecoveryGapConflict) {
			// Missing/corrupt attempt evidence cannot become valid by retrying.
			return s.markRecorderUnavailableWithoutRecoveryLocked(recorder)
		}
		return false
	}
	if !confirmed {
		return false
	}

	s.mu.Lock()
	s.current = entry.Session
	s.recoveryGapID = entry.GapID
	s.recoveryWindowStartedAt = occurredAt
	s.recoveryAttempts = 0
	s.recoveryErrorCode = errorCode
	s.recoveryCloseAt = time.Time{}
	recoveryCtx, generation := s.startRecorderRecoveryGenerationLocked(recorder)
	s.mu.Unlock()

	if retryableRecorderRecoveryErrorCode(errorCode) {
		go s.runRecorderRecovery(recoveryCtx, generation, recorder)
	} else {
		go s.persistRecorderRecoveryExhaustion(recoveryCtx, generation, recorder, errorCode)
	}
	return true
}

func boundRecorderRecoveryOccurredAt(
	eventOccurredAtMS int64,
	sessionStartedAtMS int64,
	now time.Time,
) (time.Time, bool) {
	nowMS := now.UTC().UnixMilli()
	clockUncertain := false
	// A wall-clock rollback can place "now" before the already durable session
	// start. Treat the session start as the logical upper/lower boundary in that
	// impossible wall-clock interval so the recovery gap is not discarded.
	if nowMS < sessionStartedAtMS {
		nowMS = sessionStartedAtMS
		clockUncertain = true
	}
	occurredAtMS := eventOccurredAtMS
	switch {
	case occurredAtMS <= 0:
		occurredAtMS = nowMS
		clockUncertain = true
	case occurredAtMS < sessionStartedAtMS:
		occurredAtMS = sessionStartedAtMS
		clockUncertain = true
	case occurredAtMS > nowMS:
		occurredAtMS = nowMS
		clockUncertain = true
	}
	return time.UnixMilli(occurredAtMS).UTC(), clockUncertain
}

// resumeRecorderRecoveryLocked restores ownership when an external Rebind
// cancels a generation but fails before durably taking over the session.
// The caller holds operationMu.
func (s *sessionRuntime) resumeRecorderRecoveryLocked(recorder Recorder) {
	s.mu.Lock()
	if s.recoveryGapID == "" ||
		s.finalizing ||
		s.finalized ||
		s.current.Status != SessionRecording ||
		s.current.RecordingStatus != RecordingReconnecting ||
		!sameRecorderInstance(s.recorder, recorder) {
		s.mu.Unlock()
		return
	}
	errorCode := s.recoveryErrorCode
	recoveryCtx, generation := s.startRecorderRecoveryGenerationLocked(recorder)
	s.mu.Unlock()
	if retryableRecorderRecoveryErrorCode(errorCode) {
		go s.runRecorderRecovery(recoveryCtx, generation, recorder)
		return
	}
	go s.persistRecorderRecoveryExhaustion(recoveryCtx, generation, recorder, errorCode)
}

func (s *sessionRuntime) runRecorderRecovery(
	ctx context.Context,
	generation uint64,
	recorder Recorder,
) {
	policy := s.coordinator.recoveryPolicy
	for {
		s.mu.Lock()
		if !s.recorderRecoveryGenerationMatchesLocked(generation, recorder) {
			s.mu.Unlock()
			return
		}
		now := s.coordinator.now().UTC()
		startedAt := s.recoveryWindowStartedAt
		attempts := s.recoveryAttempts
		if attempts >= policy.MaxAttempts || !now.Before(startedAt.Add(policy.Window)) {
			s.mu.Unlock()
			s.persistRecorderRecoveryExhaustion(
				ctx,
				generation,
				recorder,
				RecorderRecoveryRetryExhaustedErrorCode,
			)
			return
		}
		nextAttempt := attempts + 1
		delay := recorderRecoveryDelay(policy, nextAttempt)
		current := s.current
		errorCode := s.recoveryErrorCode
		s.enqueueRecoveryEventLocked(SessionRecoveryEvent{
			SessionID:       current.ID,
			OperationID:     current.OperationID,
			State:           SessionRecoveryRetryScheduled,
			RecordingStatus: RecordingReconnecting,
			ErrorCode:       errorCode,
			RetryAt:         now.Add(delay).UnixMilli(),
			RestartAttempt:  nextAttempt,
			OccurredAt:      now.UnixMilli(),
		})
		s.mu.Unlock()

		if err := s.coordinator.recoveryScheduler.Wait(ctx, delay); err != nil {
			return
		}

		s.operationMu.Lock()
		s.mu.Lock()
		if !s.recorderRecoveryGenerationMatchesLocked(generation, recorder) {
			s.mu.Unlock()
			s.operationMu.Unlock()
			return
		}
		now = s.coordinator.now().UTC()
		startedAt = s.recoveryWindowStartedAt
		if s.recoveryAttempts >= policy.MaxAttempts || !now.Before(startedAt.Add(policy.Window)) {
			s.mu.Unlock()
			s.operationMu.Unlock()
			s.persistRecorderRecoveryExhaustion(
				ctx,
				generation,
				recorder,
				RecorderRecoveryRetryExhaustedErrorCode,
			)
			return
		}
		s.recoveryAttempts++
		source := s.source
		s.mu.Unlock()

		remaining := startedAt.Add(policy.Window).Sub(now)
		callCtx, cancel := context.WithTimeout(ctx, remaining)
		rebindErr := boundedCall(callCtx, func(callCtx context.Context) error {
			return recorder.Rebind(callCtx, source)
		})
		cancel()
		if rebindErr != nil {
			errorCode = recorderRecoveryCodeForError(rebindErr)
			s.mu.Lock()
			currentGeneration := s.recorderRecoveryGenerationMatchesLocked(generation, recorder)
			if currentGeneration {
				s.recoveryErrorCode = errorCode
			}
			s.mu.Unlock()
			s.operationMu.Unlock()
			if !currentGeneration {
				return
			}
			if !retryableRecorderRecoveryErrorCode(errorCode) {
				s.persistRecorderRecoveryExhaustion(ctx, generation, recorder, errorCode)
				return
			}
			continue
		}

		completedAt := s.coordinator.now().UTC()
		completed, retryPersistence := s.completeRecorderRecoveryLocked(
			ctx, generation, recorder, completedAt,
		)
		s.operationMu.Unlock()
		if completed || !retryPersistence {
			return
		}
		s.retryRecorderRecoveryCompletion(ctx, generation, recorder, completedAt)
		return
	}
}

// completeRecorderRecoveryLocked is called with operationMu held. A false,
// true result means only durable completion should be retried; Rebind has
// already succeeded and must not be repeated.
func (s *sessionRuntime) completeRecorderRecoveryLocked(
	ctx context.Context,
	generation uint64,
	recorder Recorder,
	completedAt time.Time,
) (bool, bool) {
	s.mu.Lock()
	if !s.recorderRecoveryGenerationMatchesLocked(generation, recorder) {
		s.mu.Unlock()
		return false, false
	}
	current := s.current
	gapID := s.recoveryGapID
	attempts := s.recoveryAttempts
	s.mu.Unlock()

	commitCtx, cancel := s.coordinator.commitContext(ctx)
	next, completeErr := s.coordinator.recoveryJournal.CompleteRecorderRecovery(
		commitCtx,
		CompleteRecorderRecoveryInput{
			SessionID:               current.ID,
			GapID:                   gapID,
			ExpectedStatus:          current.Status,
			ExpectedRecordingStatus: current.RecordingStatus,
			ExpectedOperationID:     current.OperationID,
			RestartAttempts:         attempts,
			CompletedAt:             completedAt,
		},
	)
	cancel()
	confirmed := next.ID == current.ID &&
		next.Status == current.Status &&
		next.RecordingStatus == RecordingActive &&
		next.OperationID == current.OperationID
	if confirmed {
		s.mu.Lock()
		// operationMu prevents another state mutation. Cancellation intent may
		// have invalidated the generation while the transaction committed, but
		// the confirmed durable state must still be materialized locally.
		if s.current.ID == current.ID && s.current.OperationID == current.OperationID {
			s.current = next
			s.invalidateRecorderRecoveryLocked()
			s.recoveryRecorder = nil
			s.recoveryGapID = ""
			s.recoveryWindowStartedAt = time.Time{}
			s.recoveryAttempts = 0
			s.recoveryErrorCode = ""
			s.recoveryCloseAt = time.Time{}
			s.enqueueRecoveryEventLocked(SessionRecoveryEvent{
				SessionID:       next.ID,
				OperationID:     next.OperationID,
				State:           SessionRecoveryRecovered,
				RecordingStatus: RecordingActive,
				ErrorCode:       "",
				RestartAttempt:  attempts,
				OccurredAt:      s.coordinator.now().UTC().UnixMilli(),
			})
		}
		s.mu.Unlock()
		return true, false
	}
	_ = completeErr
	s.mu.Lock()
	retry := s.recorderRecoveryGenerationMatchesLocked(generation, recorder)
	if retry {
		now := s.coordinator.now().UTC()
		s.enqueueRecoveryEventLocked(SessionRecoveryEvent{
			SessionID:       current.ID,
			OperationID:     current.OperationID,
			State:           SessionRecoveryRetryScheduled,
			RecordingStatus: RecordingReconnecting,
			ErrorCode:       RecorderRecoveryPersistenceErrorCode,
			RetryAt:         now.Add(defaultRecorderRecoveryCompletionRetry).UnixMilli(),
			RestartAttempt:  attempts,
			OccurredAt:      now.UnixMilli(),
		})
	}
	s.mu.Unlock()
	return false, retry
}

func (s *sessionRuntime) retryRecorderRecoveryCompletion(
	ctx context.Context,
	generation uint64,
	recorder Recorder,
	completedAt time.Time,
) {
	for {
		if err := s.coordinator.recoveryScheduler.Wait(
			ctx,
			defaultRecorderRecoveryCompletionRetry,
		); err != nil {
			return
		}
		s.operationMu.Lock()
		completed, retry := s.completeRecorderRecoveryLocked(
			ctx, generation, recorder, completedAt,
		)
		s.operationMu.Unlock()
		if completed || !retry {
			return
		}
	}
}

func (s *sessionRuntime) persistRecorderRecoveryExhaustion(
	ctx context.Context,
	generation uint64,
	recorder Recorder,
	errorCode string,
) {
	errorCode = normalizeRecorderRecoveryErrorCode(errorCode)
	exhaustedAt := s.coordinator.now().UTC()
	for {
		if ctx.Err() != nil {
			return
		}
		s.operationMu.Lock()
		s.mu.Lock()
		if !s.recorderRecoveryGenerationMatchesLocked(generation, recorder) {
			s.mu.Unlock()
			s.operationMu.Unlock()
			return
		}
		current := s.current
		gapID := s.recoveryGapID
		attempts := s.recoveryAttempts
		s.mu.Unlock()

		commitCtx, cancel := s.coordinator.commitContext(ctx)
		next, exhaustErr := s.coordinator.recoveryJournal.ExhaustRecorderRecovery(
			commitCtx,
			ExhaustRecorderRecoveryInput{
				SessionID:               current.ID,
				GapID:                   gapID,
				ExpectedStatus:          current.Status,
				ExpectedRecordingStatus: current.RecordingStatus,
				ExpectedOperationID:     current.OperationID,
				RestartAttempts:         attempts,
				ErrorCode:               errorCode,
				ExhaustedAt:             exhaustedAt,
			},
		)
		cancel()
		confirmed := next.ID == current.ID &&
			next.Status == current.Status &&
			next.RecordingStatus == RecordingUnavailable &&
			next.OperationID == current.OperationID
		if confirmed {
			s.mu.Lock()
			if s.current.ID == current.ID && s.current.OperationID == current.OperationID {
				s.current = next
				s.recoveryErrorCode = errorCode
				s.invalidateRecorderRecoveryLocked()
				s.cancelRecorderEventsLocked()
				s.enqueueRecoveryEventLocked(SessionRecoveryEvent{
					SessionID:       next.ID,
					OperationID:     next.OperationID,
					State:           SessionRecoveryExhausted,
					RecordingStatus: RecordingUnavailable,
					ErrorCode:       errorCode,
					RestartAttempt:  attempts,
					OccurredAt:      s.coordinator.now().UTC().UnixMilli(),
				})
			}
			s.mu.Unlock()
			s.operationMu.Unlock()

			stopCtx, stopCancel := s.coordinator.commitContext(context.Background())
			_ = boundedCall(stopCtx, recorder.Stop)
			stopCancel()
			return
		}
		_ = exhaustErr
		s.mu.Lock()
		retry := s.recorderRecoveryGenerationMatchesLocked(generation, recorder)
		if retry {
			now := s.coordinator.now().UTC()
			s.enqueueRecoveryEventLocked(SessionRecoveryEvent{
				SessionID:       current.ID,
				OperationID:     current.OperationID,
				State:           SessionRecoveryRetryScheduled,
				RecordingStatus: RecordingReconnecting,
				ErrorCode:       RecorderRecoveryPersistenceErrorCode,
				RetryAt:         now.Add(defaultRecorderRecoveryCompletionRetry).UnixMilli(),
				RestartAttempt:  attempts,
				OccurredAt:      now.UnixMilli(),
			})
		}
		s.mu.Unlock()
		s.operationMu.Unlock()
		if !retry {
			return
		}
		if err := s.coordinator.recoveryScheduler.Wait(
			ctx,
			defaultRecorderRecoveryCompletionRetry,
		); err != nil {
			return
		}
	}
}

func (s *sessionRuntime) markRecorderUnavailableWithoutRecoveryLocked(recorder Recorder) bool {
	s.mu.Lock()
	current := s.current
	s.mu.Unlock()
	commitCtx, cancel := s.coordinator.commitContext(context.Background())
	next, transitionErr := s.coordinator.repository.Transition(commitCtx, TransitionSessionInput{
		ID: current.ID, ExpectedStatus: current.Status,
		ExpectedRecordingStatus: current.RecordingStatus, ExpectedOperationID: current.OperationID,
		Status: current.Status, RecordingStatus: RecordingUnavailable,
	})
	cancel()
	confirmed := next.ID != "" && next.RecordingStatus == RecordingUnavailable
	if transitionErr != nil && !confirmed {
		return false
	}
	if !confirmed {
		return false
	}
	s.mu.Lock()
	s.current = next
	if sameRecorderInstance(s.recorder, recorder) {
		s.cancelRecorderEventsLocked()
	}
	s.mu.Unlock()
	stopCtx, stopCancel := s.coordinator.commitContext(context.Background())
	_ = boundedCall(stopCtx, recorder.Stop)
	stopCancel()
	return true
}

func recorderRecoveryCodeForError(err error) string {
	switch {
	case errors.Is(err, ErrRecorderStreamExpired):
		return RecorderStreamExpiredErrorCode
	case errors.Is(err, ErrRecorderNetworkFailure):
		return RecorderNetworkFailureErrorCode
	case errors.Is(err, ErrRecorderUnsupportedInput):
		return RecorderUnsupportedInputErrorCode
	case errors.Is(err, ErrRecorderLocalResource), errors.Is(err, ErrRecorderOutput):
		return RecorderLocalResourceErrorCode
	case errors.Is(err, ErrRecorderDependencyFailure), errors.Is(err, ErrRecorderMediaJournal):
		return RecorderDependencyFailureErrorCode
	default:
		return RecorderProcessExitedErrorCode
	}
}

// finishExternalRecorderRebindLocked is called with operationMu held after an
// externally requested Rebind. An open runtime-recovery gap is completed or
// exhausted atomically with the recording status; otherwise the legacy
// session transition remains in use.
func (s *sessionRuntime) finishExternalRecorderRebindLocked(
	ctx context.Context,
	reconnecting LiveSession,
	target RecordingStatus,
	errorCode string,
) (LiveSession, error) {
	s.mu.Lock()
	gapID := s.recoveryGapID
	attempts := s.recoveryAttempts
	s.mu.Unlock()
	if gapID == "" || s.coordinator.recoveryJournal == nil {
		return s.coordinator.repository.Transition(ctx, TransitionSessionInput{
			ID:                      reconnecting.ID,
			ExpectedStatus:          reconnecting.Status,
			ExpectedRecordingStatus: reconnecting.RecordingStatus,
			ExpectedOperationID:     reconnecting.OperationID,
			Status:                  reconnecting.Status,
			RecordingStatus:         target,
		})
	}
	if target == RecordingActive {
		return s.coordinator.recoveryJournal.CompleteRecorderRecovery(
			ctx,
			CompleteRecorderRecoveryInput{
				SessionID:               reconnecting.ID,
				GapID:                   gapID,
				ExpectedStatus:          reconnecting.Status,
				ExpectedRecordingStatus: reconnecting.RecordingStatus,
				ExpectedOperationID:     reconnecting.OperationID,
				RestartAttempts:         attempts,
				CompletedAt:             s.coordinator.now().UTC(),
			},
		)
	}
	return s.coordinator.recoveryJournal.ExhaustRecorderRecovery(
		ctx,
		ExhaustRecorderRecoveryInput{
			SessionID:               reconnecting.ID,
			GapID:                   gapID,
			ExpectedStatus:          reconnecting.Status,
			ExpectedRecordingStatus: reconnecting.RecordingStatus,
			ExpectedOperationID:     reconnecting.OperationID,
			RestartAttempts:         attempts,
			ErrorCode:               normalizeRecorderRecoveryErrorCode(errorCode),
			ExhaustedAt:             s.coordinator.now().UTC(),
		},
	)
}

func (s *sessionRuntime) materializeExternalRecorderRecoveryLocked(
	next LiveSession,
	target RecordingStatus,
	errorCode string,
) {
	s.current = next
	if s.recoveryGapID == "" {
		return
	}
	if target == RecordingActive {
		s.recoveryGapID = ""
		s.recoveryRecorder = nil
		s.recoveryWindowStartedAt = time.Time{}
		s.recoveryAttempts = 0
		s.recoveryErrorCode = ""
		s.recoveryCloseAt = time.Time{}
		return
	}
	s.recoveryErrorCode = normalizeRecorderRecoveryErrorCode(errorCode)
}
