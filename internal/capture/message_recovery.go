package capture

import (
	"context"
	"errors"
	"time"
)

const (
	MessageDisconnectErrorCode        = "MESSAGE_DISCONNECT"
	MessageRecoveryRetryErrorCode     = "MESSAGE_REBIND_RETRY"
	MessageSubscriptionErrorCode      = "MESSAGE_SUBSCRIPTION_FAILED"
	MessageRecoveryRecoveredErrorCode = "MESSAGE_RECONNECTED"
	MessageRecoveryExhaustedErrorCode = "MESSAGE_REBIND_EXHAUSTED"
	MessageRecoveryFinalizedErrorCode = "MESSAGE_FINALIZED"
)

// MessageRecoveryJournal persists the message connection gap independently
// from recorder process recovery. Begin rotates the operation and opens or
// reuses one message_disconnect gap in the same transaction. Finish closes
// that gap, an optional concurrent recorder gap, and the recording state in
// one transaction.
type MessageRecoveryJournal interface {
	BeginMessageRecovery(context.Context, BeginMessageRecoveryInput) (MessageRecoveryJournalEntry, error)
	FinishMessageRecovery(context.Context, FinishMessageRecoveryInput) (LiveSession, error)
	CloseMessageRecovery(context.Context, CloseMessageRecoveryInput) error
}

// MessageDisconnectSession is an optional runtime surface used by the room
// supervisor at the instant an established message connection ends.
type MessageDisconnectSession interface {
	MarkMessageDisconnected(context.Context, string, time.Time) (LiveSession, error)
}

type MessageRecoveryJournalEntry struct {
	Session LiveSession
	GapID   string
}

type BeginMessageRecoveryInput struct {
	SessionID               string
	ExpectedStatus          SessionStatus
	ExpectedRecordingStatus RecordingStatus
	ExpectedOperationID     string
	OperationID             string
	ErrorCode               string
	OccurredAt              time.Time
}

type FinishMessageRecoveryInput struct {
	SessionID               string
	GapID                   string
	ExpectedStatus          SessionStatus
	ExpectedRecordingStatus RecordingStatus
	ExpectedOperationID     string
	TargetRecordingStatus   RecordingStatus
	Recovered               bool
	ErrorCode               string
	CompletedAt             time.Time
	RecorderGapID           string
	RecorderRestartAttempts int
	RecorderErrorCode       string
}

type CloseMessageRecoveryInput struct {
	SessionID               string
	GapID                   string
	ExpectedStatus          SessionStatus
	ExpectedRecordingStatus RecordingStatus
	ExpectedOperationID     string
	ErrorCode               string
	ClosedAt                time.Time
	RecorderGapID           string
	RecorderRestartAttempts int
	RecorderErrorCode       string
}

func messageRecoveryBeginRecording(status RecordingStatus) RecordingStatus {
	if status == RecordingActive || status == RecordingReconnecting {
		return RecordingReconnecting
	}
	return status
}

func (s *sessionRuntime) MarkMessageDisconnected(
	ctx context.Context,
	operationID string,
	occurredAt time.Time,
) (LiveSession, error) {
	if err := coordinatorContext(ctx); err != nil {
		return LiveSession{}, err
	}
	if s.coordinator.messageJournal == nil {
		return s.Snapshot(), nil
	}
	s.mu.Lock()
	if s.finalized || s.finalizing {
		current := s.current
		s.mu.Unlock()
		return current, ErrCaptureFinalized
	}
	if operationID == s.operationID && s.messageRecoveryGapID != "" {
		current := s.current
		s.mu.Unlock()
		return current, nil
	}
	s.mu.Unlock()

	s.cancelRecorderRecoveryIntent()
	s.operationMu.Lock()
	// A process-exit handler may have created a generation while Mark waited
	// for operationMu; cancel again under the operation fence.
	s.cancelRecorderRecoveryIntent()
	defer s.operationMu.Unlock()
	s.mu.Lock()
	if s.finalized || s.finalizing {
		current := s.current
		s.mu.Unlock()
		return current, ErrCaptureFinalized
	}
	if operationID == s.operationID && s.messageRecoveryGapID != "" {
		current := s.current
		s.mu.Unlock()
		return current, nil
	}
	current := s.current
	recorder := s.recorder
	s.mu.Unlock()
	if occurredAt.IsZero() {
		occurredAt = s.coordinator.now().UTC()
	} else {
		occurredAt = occurredAt.UTC()
	}
	if occurredAt.UnixMilli() < current.StartedAt {
		occurredAt = time.UnixMilli(current.StartedAt).UTC()
	}
	entry, beginErr := s.coordinator.messageJournal.BeginMessageRecovery(
		ctx,
		BeginMessageRecoveryInput{
			SessionID:               current.ID,
			ExpectedStatus:          current.Status,
			ExpectedRecordingStatus: current.RecordingStatus,
			ExpectedOperationID:     current.OperationID,
			OperationID:             operationID,
			ErrorCode:               MessageDisconnectErrorCode,
			OccurredAt:              occurredAt,
		},
	)
	confirmed := entry.GapID != "" && entry.Session.ID == current.ID &&
		entry.Session.Status == current.Status &&
		entry.Session.RecordingStatus == messageRecoveryBeginRecording(current.RecordingStatus) &&
		entry.Session.OperationID == operationID
	if !confirmed {
		s.resumeRecorderRecoveryLocked(recorder)
		return current, messageRecoveryPublicError(ctx, beginErr)
	}
	s.mu.Lock()
	s.cancelRecorderProgressLocked()
	s.current = entry.Session
	s.operationID = operationID
	s.messageRecoveryGapID = entry.GapID
	s.messageRecoveryErrorCode = MessageDisconnectErrorCode
	s.messageRecoveryCloseAt = time.Time{}
	s.mu.Unlock()
	return entry.Session, messageRecoveryConfirmedError(ctx, beginErr)
}

func (s *sessionRuntime) rebindWithMessageRecoveryLocked(
	ctx context.Context,
	operationID string,
	source CaptureSource,
	current LiveSession,
	recorder Recorder,
) (LiveSession, error) {
	previousRecording := current.RecordingStatus
	beginCode := MessageDisconnectErrorCode
	s.mu.Lock()
	if s.messageRecoveryGapID != "" {
		beginCode = MessageRecoveryRetryErrorCode
	}
	s.mu.Unlock()

	occurredAt := s.coordinator.now().UTC()
	if occurredAt.UnixMilli() < current.StartedAt {
		occurredAt = time.UnixMilli(current.StartedAt).UTC()
	}
	entry, beginErr := s.coordinator.messageJournal.BeginMessageRecovery(
		ctx,
		BeginMessageRecoveryInput{
			SessionID:               current.ID,
			ExpectedStatus:          current.Status,
			ExpectedRecordingStatus: current.RecordingStatus,
			ExpectedOperationID:     current.OperationID,
			OperationID:             operationID,
			ErrorCode:               beginCode,
			OccurredAt:              occurredAt,
		},
	)
	targetAfterBegin := messageRecoveryBeginRecording(previousRecording)
	confirmed := entry.GapID != "" &&
		entry.Session.ID == current.ID &&
		entry.Session.Status == current.Status &&
		entry.Session.RecordingStatus == targetAfterBegin &&
		entry.Session.OperationID == operationID
	if !confirmed {
		s.resumeRecorderRecoveryLocked(recorder)
		return current, messageRecoveryPublicError(ctx, beginErr)
	}

	s.mu.Lock()
	s.cancelRecorderProgressLocked()
	s.current = entry.Session
	s.operationID = operationID
	if s.messageRecoveryGapID != entry.GapID {
		s.messageRecoveryCloseAt = time.Time{}
	}
	s.messageRecoveryGapID = entry.GapID
	s.messageRecoveryErrorCode = beginCode
	s.mu.Unlock()

	newSubscriptionID := s.subscribe(source, operationID)
	if newSubscriptionID == "" {
		s.mu.Lock()
		s.messageRecoveryErrorCode = MessageSubscriptionErrorCode
		s.mu.Unlock()
		return entry.Session, errors.Join(
			messageRecoveryConfirmedError(ctx, beginErr),
			ErrCaptureSubscription,
		)
	}

	needsRecorderRebind := previousRecording == RecordingActive ||
		previousRecording == RecordingReconnecting
	targetRecording := targetAfterBegin
	recovered := true
	recorderErrorCode := ""
	if needsRecorderRebind {
		if recorder == nil {
			recovered = false
			targetRecording = RecordingUnavailable
			recorderErrorCode = RecorderDependencyFailureErrorCode
		} else {
			rebindErr := boundedCall(ctx, func(callCtx context.Context) error {
				return recorder.Rebind(callCtx, source)
			})
			if rebindErr != nil {
				recorderErrorCode = recorderRecoveryCodeForError(rebindErr)
				if retryableRecorderRecoveryErrorCode(recorderErrorCode) {
					source.Unsubscribe(newSubscriptionID)
					s.mu.Lock()
					s.messageRecoveryErrorCode = MessageRecoveryRetryErrorCode
					s.recoveryErrorCode = recorderErrorCode
					s.mu.Unlock()
					return entry.Session, errors.Join(
						messageRecoveryConfirmedError(ctx, beginErr),
						ErrCaptureRebindRetryable,
						ctx.Err(),
					)
				}
				recovered = false
				targetRecording = RecordingUnavailable
			} else {
				targetRecording = RecordingActive
			}
		}
	}

	s.mu.Lock()
	recorderGapID := s.recoveryGapID
	recorderAttempts := s.recoveryAttempts
	if recorderGapID == "" {
		recorderErrorCode = ""
	} else if recorderErrorCode == "" {
		recorderErrorCode = normalizeRecorderRecoveryErrorCode(s.recoveryErrorCode)
	}
	s.mu.Unlock()
	finishCode := MessageRecoveryRecoveredErrorCode
	if !recovered {
		finishCode = MessageRecoveryExhaustedErrorCode
	}
	completedAt := s.coordinator.now().UTC()
	next, finishErr := s.coordinator.messageJournal.FinishMessageRecovery(
		ctx,
		FinishMessageRecoveryInput{
			SessionID:               entry.Session.ID,
			GapID:                   entry.GapID,
			ExpectedStatus:          entry.Session.Status,
			ExpectedRecordingStatus: entry.Session.RecordingStatus,
			ExpectedOperationID:     entry.Session.OperationID,
			TargetRecordingStatus:   targetRecording,
			Recovered:               recovered,
			ErrorCode:               finishCode,
			CompletedAt:             completedAt,
			RecorderGapID:           recorderGapID,
			RecorderRestartAttempts: recorderAttempts,
			RecorderErrorCode:       recorderErrorCode,
		},
	)
	finishConfirmed := next.ID == entry.Session.ID &&
		next.Status == entry.Session.Status &&
		next.OperationID == entry.Session.OperationID &&
		next.RecordingStatus == targetRecording
	if !finishConfirmed {
		source.Unsubscribe(newSubscriptionID)
		return entry.Session, errors.Join(
			messageRecoveryConfirmedError(ctx, beginErr),
			messageRecoveryPublicError(ctx, finishErr),
		)
	}

	s.mu.Lock()
	oldSource, oldSubscriptionID := s.source, s.subscriptionID
	s.materializeMessageRecoveryLocked(
		next,
		entry.GapID,
		recorderGapID,
		targetRecording,
	)
	s.source = source
	s.subscriptionID = newSubscriptionID
	s.recorder = recorder
	if recorder == nil || targetRecording == RecordingUnavailable {
		s.cancelRecorderEventsLocked()
	}
	s.mu.Unlock()
	if oldSource != nil && oldSubscriptionID != "" {
		oldSource.Unsubscribe(oldSubscriptionID)
	}
	if recorder != nil && targetRecording == RecordingActive {
		s.startRecorderProgress(recorder)
	}
	if recorder != nil && targetRecording == RecordingUnavailable {
		if stopErr := boundedCall(ctx, recorder.Stop); stopErr != nil {
			return next, errors.Join(ErrCaptureCleanup, ctx.Err())
		}
	}
	return next, messageRecoveryConfirmedError(ctx, beginErr)
}

func messageRecoveryPublicError(ctx context.Context, err error) error {
	if err == nil {
		return ErrRecoveryContractInvalid
	}
	if ctx != nil && ctx.Err() != nil {
		return errors.Join(ErrRecoveryPersistence, ctx.Err())
	}
	if errors.Is(err, ErrStaleRecovery) || errors.Is(err, ErrStaleTransition) {
		return ErrStaleTransition
	}
	if errors.Is(err, ErrRecoveryGapConflict) {
		return ErrRecoveryGapConflict
	}
	if errors.Is(err, ErrRecoveryContractInvalid) {
		return ErrRecoveryContractInvalid
	}
	return ErrRecoveryPersistence
}

func messageRecoveryConfirmedError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return ErrRecoveryPersistence
}

func (s *sessionRuntime) materializeMessageRecoveryLocked(
	next LiveSession,
	messageGapID string,
	recorderGapID string,
	target RecordingStatus,
) {
	s.current = next
	if s.messageRecoveryGapID == messageGapID {
		s.messageRecoveryGapID = ""
		s.messageRecoveryErrorCode = ""
		s.messageRecoveryCloseAt = time.Time{}
	}
	if recorderGapID == "" {
		s.recoveryErrorCode = ""
	}
	if recorderGapID == "" || s.recoveryGapID != recorderGapID {
		return
	}
	if target == RecordingActive {
		s.recordProgressRestartCountLocked(s.recoveryAttempts)
	}
	s.invalidateRecorderRecoveryLocked()
	s.recoveryGapID = ""
	s.recoveryRecorder = nil
	s.recoveryWindowStartedAt = time.Time{}
	s.recoveryAttempts = 0
	s.recoveryErrorCode = ""
	s.recoveryCloseAt = time.Time{}
}

// exhaustMessageRecoveryForRecorderLocked is called with operationMu held
// when a permanent recorder event races an open message gap. It terminalizes
// both gaps and recording ownership in one durable transaction.
func (s *sessionRuntime) exhaustMessageRecoveryForRecorderLocked(
	recorder Recorder,
	errorCode string,
) bool {
	s.mu.Lock()
	current := s.current
	messageGapID := s.messageRecoveryGapID
	recorderGapID := s.recoveryGapID
	recorderAttempts := s.recoveryAttempts
	s.mu.Unlock()
	if messageGapID == "" || s.coordinator.messageJournal == nil {
		return s.markRecorderUnavailableWithoutRecoveryLocked(recorder)
	}
	completedAt := s.coordinator.now().UTC()
	commitCtx, cancel := s.coordinator.commitContext(context.Background())
	next, finishErr := s.coordinator.messageJournal.FinishMessageRecovery(
		commitCtx,
		FinishMessageRecoveryInput{
			SessionID:               current.ID,
			GapID:                   messageGapID,
			ExpectedStatus:          current.Status,
			ExpectedRecordingStatus: current.RecordingStatus,
			ExpectedOperationID:     current.OperationID,
			TargetRecordingStatus:   RecordingUnavailable,
			Recovered:               false,
			ErrorCode:               MessageRecoveryExhaustedErrorCode,
			CompletedAt:             completedAt,
			RecorderGapID:           recorderGapID,
			RecorderRestartAttempts: recorderAttempts,
			RecorderErrorCode:       normalizeRecorderRecoveryErrorCode(errorCode),
		},
	)
	cancel()
	confirmed := next.ID == current.ID &&
		next.Status == current.Status &&
		next.OperationID == current.OperationID &&
		next.RecordingStatus == RecordingUnavailable
	if !confirmed || finishErr != nil {
		return false
	}
	s.mu.Lock()
	s.materializeMessageRecoveryLocked(
		next,
		messageGapID,
		recorderGapID,
		RecordingUnavailable,
	)
	s.cancelRecorderEventsLocked()
	s.mu.Unlock()
	stopCtx, stopCancel := s.coordinator.commitContext(context.Background())
	_ = boundedCall(stopCtx, recorder.Stop)
	stopCancel()
	return true
}
