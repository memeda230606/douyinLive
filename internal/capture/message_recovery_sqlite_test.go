package capture

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	douyinLive "github.com/jwwsjlm/douyinLive/v2"
)

func TestSQLiteMessageRecoveryPreservesFirstDisconnectAndCommitAmbiguity(t *testing.T) {
	fixture := openRecorderRecoveryFixture(t)
	ctx := context.Background()
	disconnectedAt := fixture.now.Add(-30 * time.Second)
	firstOperationID := newV7(t)
	begin := BeginMessageRecoveryInput{
		SessionID:               fixture.session.ID,
		ExpectedStatus:          fixture.session.Status,
		ExpectedRecordingStatus: RecordingActive,
		ExpectedOperationID:     fixture.session.OperationID,
		OperationID:             firstOperationID,
		ErrorCode:               MessageDisconnectErrorCode,
		OccurredAt:              disconnectedAt,
	}
	entry, err := fixture.repository.beginMessageRecovery(ctx, begin, ambiguousMessageCommit)
	if err != nil {
		t.Fatalf("beginMessageRecovery(ambiguous commit) error = %v", err)
	}
	if entry.Session.OperationID != firstOperationID ||
		entry.Session.RecordingStatus != RecordingReconnecting ||
		validateUUIDv7("message gap", entry.GapID) != nil {
		t.Fatalf("beginMessageRecovery() = %+v", entry)
	}
	firstVersion := entry.Session.UpdatedAt
	replayed, err := fixture.repository.BeginMessageRecovery(ctx, begin)
	if err != nil {
		t.Fatalf("BeginMessageRecovery(replay) error = %v", err)
	}
	if replayed.GapID != entry.GapID || replayed.Session.UpdatedAt != firstVersion {
		t.Fatalf("BeginMessageRecovery(replay) = %+v", replayed)
	}

	retryAt := fixture.now
	secondOperationID := newV7(t)
	retry := BeginMessageRecoveryInput{
		SessionID:               fixture.session.ID,
		ExpectedStatus:          fixture.session.Status,
		ExpectedRecordingStatus: RecordingReconnecting,
		ExpectedOperationID:     firstOperationID,
		OperationID:             secondOperationID,
		ErrorCode:               MessageRecoveryRetryErrorCode,
		OccurredAt:              retryAt,
	}
	reused, err := fixture.repository.BeginMessageRecovery(ctx, retry)
	if err != nil {
		t.Fatalf("BeginMessageRecovery(retry) error = %v", err)
	}
	if reused.GapID != entry.GapID || reused.Session.OperationID != secondOperationID {
		t.Fatalf("BeginMessageRecovery(retry) = %+v", reused)
	}
	openGap := fixture.messageGap(t, entry.GapID)
	if openGap.StartedAtMS != disconnectedAt.UnixMilli() ||
		openGap.Details.LastOccurredAtMS != retryAt.UnixMilli() ||
		openGap.Details.BeginAttempts != 2 ||
		openGap.EndedAtMS != nil || openGap.Recovered ||
		strings.Contains(openGap.DetailsJSON, "://") ||
		strings.Contains(openGap.DetailsJSON, fixture.session.DataPath) {
		t.Fatalf("reused message gap = %+v", openGap)
	}
	if count := fixture.messageGapCount(t); count != 1 {
		t.Fatalf("message gap count = %d, want 1", count)
	}

	completedAt := fixture.now.Add(time.Second)
	finish := FinishMessageRecoveryInput{
		SessionID:               fixture.session.ID,
		GapID:                   entry.GapID,
		ExpectedStatus:          fixture.session.Status,
		ExpectedRecordingStatus: RecordingReconnecting,
		ExpectedOperationID:     secondOperationID,
		TargetRecordingStatus:   RecordingActive,
		Recovered:               true,
		ErrorCode:               MessageRecoveryRecoveredErrorCode,
		CompletedAt:             completedAt,
	}
	completed, err := fixture.repository.finishMessageRecovery(ctx, finish, ambiguousMessageCommit)
	if err != nil {
		t.Fatalf("finishMessageRecovery(ambiguous commit) error = %v", err)
	}
	if completed.RecordingStatus != RecordingActive || completed.OperationID != secondOperationID {
		t.Fatalf("finishMessageRecovery() = %+v", completed)
	}
	completedVersion := completed.UpdatedAt
	replayedCompletion, err := fixture.repository.FinishMessageRecovery(ctx, finish)
	if err != nil {
		t.Fatalf("FinishMessageRecovery(replay) error = %v", err)
	}
	if replayedCompletion.UpdatedAt != completedVersion {
		t.Fatalf("completion replay version = %d, want %d", replayedCompletion.UpdatedAt, completedVersion)
	}
	closedGap := fixture.messageGap(t, entry.GapID)
	if closedGap.EndedAtMS == nil || *closedGap.EndedAtMS != completedAt.UnixMilli() ||
		!closedGap.Recovered || closedGap.StartedAtMS != disconnectedAt.UnixMilli() {
		t.Fatalf("closed message gap = %+v", closedGap)
	}
}

func TestSQLiteMessageAndRecorderRecoveryConvergeInEitherOrder(t *testing.T) {
	for _, recorderFirst := range []bool{false, true} {
		name := "message_first"
		if recorderFirst {
			name = "recorder_first"
		}
		t.Run(name, func(t *testing.T) {
			fixture := openRecorderRecoveryFixture(t)
			ctx := context.Background()
			messageOperationID := newV7(t)
			var messageEntry MessageRecoveryJournalEntry
			var recorderEntry RecorderRecoveryJournalEntry
			var err error
			if recorderFirst {
				recorderInput := fixture.beginInput()
				recorderInput.OccurredAt = fixture.now.Add(-20 * time.Second)
				recorderInput.ErrorCode = RecorderNetworkFailureErrorCode
				recorderEntry, err = fixture.repository.BeginRecorderRecovery(ctx, recorderInput)
				if err != nil {
					t.Fatalf("BeginRecorderRecovery() error = %v", err)
				}
				messageEntry, err = fixture.repository.BeginMessageRecovery(ctx, BeginMessageRecoveryInput{
					SessionID: fixture.session.ID, ExpectedStatus: fixture.session.Status,
					ExpectedRecordingStatus: RecordingReconnecting,
					ExpectedOperationID:     fixture.session.OperationID, OperationID: messageOperationID,
					ErrorCode: MessageDisconnectErrorCode, OccurredAt: fixture.now.Add(-10 * time.Second),
				})
			} else {
				messageEntry, err = fixture.repository.BeginMessageRecovery(ctx, BeginMessageRecoveryInput{
					SessionID: fixture.session.ID, ExpectedStatus: fixture.session.Status,
					ExpectedRecordingStatus: RecordingActive,
					ExpectedOperationID:     fixture.session.OperationID, OperationID: messageOperationID,
					ErrorCode: MessageDisconnectErrorCode, OccurredAt: fixture.now.Add(-20 * time.Second),
				})
				if err == nil {
					recorderInput := fixture.beginInput()
					recorderInput.ExpectedRecordingStatus = RecordingReconnecting
					recorderInput.ExpectedOperationID = messageOperationID
					recorderInput.OccurredAt = fixture.now.Add(-10 * time.Second)
					recorderInput.ErrorCode = RecorderNetworkFailureErrorCode
					recorderEntry, err = fixture.repository.BeginRecorderRecovery(ctx, recorderInput)
				}
			}
			if err != nil {
				t.Fatalf("begin concurrent recovery order error = %v", err)
			}
			completedAt := fixture.now.Add(time.Second)
			finish := FinishMessageRecoveryInput{
				SessionID: fixture.session.ID, GapID: messageEntry.GapID,
				ExpectedStatus: fixture.session.Status, ExpectedRecordingStatus: RecordingReconnecting,
				ExpectedOperationID: messageOperationID, TargetRecordingStatus: RecordingActive,
				Recovered: true, ErrorCode: MessageRecoveryRecoveredErrorCode, CompletedAt: completedAt,
				RecorderGapID: recorderEntry.GapID, RecorderRestartAttempts: 1,
				RecorderErrorCode: RecorderNetworkFailureErrorCode,
			}
			completed, finishErr := fixture.repository.FinishMessageRecovery(ctx, finish)
			if finishErr != nil {
				t.Fatalf("FinishMessageRecovery() error = %v", finishErr)
			}
			if completed.RecordingStatus != RecordingActive || completed.OperationID != messageOperationID {
				t.Fatalf("combined completion = %+v", completed)
			}
			messageGap := fixture.messageGap(t, messageEntry.GapID)
			recorderGap := fixture.gap(t, recorderEntry.GapID)
			if messageGap.EndedAtMS == nil || recorderGap.EndedAtMS == nil ||
				*messageGap.EndedAtMS != completedAt.UnixMilli() ||
				*recorderGap.EndedAtMS != completedAt.UnixMilli() ||
				!messageGap.Recovered || !recorderGap.Recovered {
				t.Fatalf("combined gaps = message:%+v recorder:%+v", messageGap, recorderGap)
			}
		})
	}
}

func TestSQLiteMessageRecoveryPermanentAndFinalizeClose(t *testing.T) {
	t.Run("permanent", func(t *testing.T) {
		fixture := openRecorderRecoveryFixture(t)
		operationID := newV7(t)
		entry, err := fixture.repository.BeginMessageRecovery(context.Background(), BeginMessageRecoveryInput{
			SessionID: fixture.session.ID, ExpectedStatus: fixture.session.Status,
			ExpectedRecordingStatus: RecordingActive,
			ExpectedOperationID:     fixture.session.OperationID, OperationID: operationID,
			ErrorCode: MessageDisconnectErrorCode, OccurredAt: fixture.now.Add(-time.Second),
		})
		if err != nil {
			t.Fatal(err)
		}
		completedAt := fixture.now
		next, err := fixture.repository.FinishMessageRecovery(context.Background(), FinishMessageRecoveryInput{
			SessionID: fixture.session.ID, GapID: entry.GapID,
			ExpectedStatus: fixture.session.Status, ExpectedRecordingStatus: RecordingReconnecting,
			ExpectedOperationID: operationID, TargetRecordingStatus: RecordingUnavailable,
			Recovered: false, ErrorCode: MessageRecoveryExhaustedErrorCode, CompletedAt: completedAt,
		})
		if err != nil || next.RecordingStatus != RecordingUnavailable {
			t.Fatalf("permanent finish = (%+v, %v)", next, err)
		}
		gap := fixture.messageGap(t, entry.GapID)
		if gap.EndedAtMS == nil || gap.Recovered || gap.ReasonCode != MessageRecoveryExhaustedErrorCode {
			t.Fatalf("permanent message gap = %+v", gap)
		}
	})

	t.Run("finalize_with_recorder", func(t *testing.T) {
		fixture := openRecorderRecoveryFixture(t)
		ctx := context.Background()
		messageOperationID := newV7(t)
		messageEntry, err := fixture.repository.BeginMessageRecovery(ctx, BeginMessageRecoveryInput{
			SessionID: fixture.session.ID, ExpectedStatus: fixture.session.Status,
			ExpectedRecordingStatus: RecordingActive,
			ExpectedOperationID:     fixture.session.OperationID, OperationID: messageOperationID,
			ErrorCode: MessageDisconnectErrorCode, OccurredAt: fixture.now.Add(-20 * time.Second),
		})
		if err != nil {
			t.Fatal(err)
		}
		recorderInput := fixture.beginInput()
		recorderInput.ExpectedRecordingStatus = RecordingReconnecting
		recorderInput.ExpectedOperationID = messageOperationID
		recorderInput.ErrorCode = RecorderNetworkFailureErrorCode
		recorderInput.OccurredAt = fixture.now.Add(-10 * time.Second)
		recorderEntry, err := fixture.repository.BeginRecorderRecovery(ctx, recorderInput)
		if err != nil {
			t.Fatal(err)
		}
		finalizeOperationID := newV7(t)
		finalizing, err := fixture.repository.Transition(ctx, TransitionSessionInput{
			ID: fixture.session.ID, ExpectedStatus: fixture.session.Status,
			ExpectedRecordingStatus: RecordingReconnecting, ExpectedOperationID: messageOperationID,
			Status: SessionFinalizing, RecordingStatus: RecordingFinalizing,
			NextOperationID: finalizeOperationID,
		})
		if err != nil {
			t.Fatal(err)
		}
		closedAt := fixture.now.Add(time.Second)
		closeInput := CloseMessageRecoveryInput{
			SessionID: finalizing.ID, GapID: messageEntry.GapID,
			ExpectedStatus: finalizing.Status, ExpectedRecordingStatus: finalizing.RecordingStatus,
			ExpectedOperationID: finalizing.OperationID, ErrorCode: MessageRecoveryFinalizedErrorCode,
			ClosedAt: closedAt, RecorderGapID: recorderEntry.GapID,
			RecorderErrorCode: RecorderNetworkFailureErrorCode,
		}
		if err := fixture.repository.closeMessageRecovery(ctx, closeInput, ambiguousMessageCommit); err != nil {
			t.Fatalf("closeMessageRecovery(ambiguous commit) error = %v", err)
		}
		if err := fixture.repository.CloseMessageRecovery(ctx, closeInput); err != nil {
			t.Fatalf("CloseMessageRecovery(replay) error = %v", err)
		}
		messageGap := fixture.messageGap(t, messageEntry.GapID)
		recorderGap := fixture.gap(t, recorderEntry.GapID)
		if messageGap.EndedAtMS == nil || recorderGap.EndedAtMS == nil ||
			messageGap.Recovered || recorderGap.Recovered ||
			*messageGap.EndedAtMS != closedAt.UnixMilli() ||
			*recorderGap.EndedAtMS != closedAt.UnixMilli() {
			t.Fatalf("finalize gaps = message:%+v recorder:%+v", messageGap, recorderGap)
		}
	})
}

func TestCoordinatorMessageRecoveryRetainsOwnerUntilLaterOperationSucceeds(t *testing.T) {
	repository, store, _, roomID, baseNow := openRepository(t)
	defer store.Close()
	now := baseNow
	recorder := &fakeRecorder{}
	recorder.rebindFunc = func(context.Context, CaptureSource) error {
		recorder.mu.Lock()
		calls := recorder.rebinds
		recorder.mu.Unlock()
		if calls == 1 {
			return ErrRecorderNetworkFailure
		}
		return nil
	}
	coordinator, err := newTestCoordinator(repository, CoordinatorOptions{
		Now: func() time.Time { return now },
		RecorderFactory: func(context.Context, LiveSession, OpenRequest, CaptureSource) (Recorder, error) {
			return recorder, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	firstSource := newFakeCaptureSource(nil)
	session, err := coordinator.Open(context.Background(), OpenRequest{
		RoomConfigID: roomID, OperationID: newV7(t), RecordEnabled: true, StartedAt: now,
	}, firstSource)
	if err != nil {
		t.Fatal(err)
	}
	disconnectedAt := now.Add(time.Second)
	now = disconnectedAt
	marked, err := session.(MessageDisconnectSession).MarkMessageDisconnected(
		context.Background(), newV7(t), disconnectedAt,
	)
	if err != nil || marked.RecordingStatus != RecordingReconnecting {
		t.Fatalf("MarkMessageDisconnected() = (%+v, %v)", marked, err)
	}
	now = disconnectedAt.Add(30 * time.Second)
	secondSource := newFakeCaptureSource(nil)
	retrySnapshot, retryErr := session.Rebind(context.Background(), newV7(t), secondSource)
	if !errors.Is(retryErr, ErrCaptureRebindRetryable) ||
		retrySnapshot.RecordingStatus != RecordingReconnecting ||
		strings.Contains(retryErr.Error(), "network") {
		t.Fatalf("retryable Rebind() = (%+v, %v)", retrySnapshot, retryErr)
	}
	recorder.mu.Lock()
	stopsAfterRetry := recorder.stops
	recorder.mu.Unlock()
	if stopsAfterRetry != 0 {
		t.Fatalf("retryable Rebind stopped owner %d times", stopsAfterRetry)
	}
	now = disconnectedAt.Add(45 * time.Second)
	thirdSource := newFakeCaptureSource(nil)
	recovered, err := session.Rebind(context.Background(), newV7(t), thirdSource)
	if err != nil || recovered.RecordingStatus != RecordingActive {
		t.Fatalf("recovered Rebind() = (%+v, %v)", recovered, err)
	}
	runtime := session.(*sessionRuntime)
	runtime.mu.Lock()
	staleRecorderCode := runtime.recoveryErrorCode
	runtime.mu.Unlock()
	if staleRecorderCode != "" {
		t.Fatalf("recovered runtime retained recorder error code %q", staleRecorderCode)
	}
	gap := singleMessageGap(t, store.Writer(), recovered.ID)
	if gap.StartedAtMS != disconnectedAt.UnixMilli() || gap.EndedAtMS == nil ||
		*gap.EndedAtMS != now.UnixMilli() || !gap.Recovered {
		t.Fatalf("runtime message gap = %+v", gap)
	}
	if _, err := session.Finalize(context.Background(), newV7(t), FinalizeShutdown); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorMessageSubscriptionFailureKeepsRecorderOwner(t *testing.T) {
	repository, store, _, roomID, now := openRepository(t)
	defer store.Close()
	recorder := &fakeRecorder{}
	coordinator, err := newTestCoordinator(repository, CoordinatorOptions{
		Now: func() time.Time { return now },
		RecorderFactory: func(context.Context, LiveSession, OpenRequest, CaptureSource) (Recorder, error) {
			return recorder, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := coordinator.Open(context.Background(), OpenRequest{
		RoomConfigID: roomID, OperationID: newV7(t), RecordEnabled: true, StartedAt: now,
	}, newFakeCaptureSource(nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.(MessageDisconnectSession).MarkMessageDisconnected(
		context.Background(), newV7(t), now,
	); err != nil {
		t.Fatal(err)
	}
	failedSource := &messageRecoveryNoSubscriptionSource{}
	failed, err := session.Rebind(context.Background(), newV7(t), failedSource)
	if !errors.Is(err, ErrCaptureSubscription) || failed.RecordingStatus != RecordingReconnecting {
		t.Fatalf("subscription failure Rebind() = (%+v, %v)", failed, err)
	}
	recorder.mu.Lock()
	if recorder.stops != 0 || recorder.rebinds != 0 {
		t.Fatalf("subscription failure recorder calls = rebind:%d stop:%d", recorder.rebinds, recorder.stops)
	}
	recorder.mu.Unlock()
	recovered, err := session.Rebind(context.Background(), newV7(t), newFakeCaptureSource(nil))
	if err != nil || recovered.RecordingStatus != RecordingActive {
		t.Fatalf("subscription recovery Rebind() = (%+v, %v)", recovered, err)
	}
	if _, err := session.Finalize(context.Background(), newV7(t), FinalizeShutdown); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorMessageAndProcessExitRecoveryAreFencedInEitherOrder(t *testing.T) {
	for _, recorderFirst := range []bool{false, true} {
		name := "message_first"
		if recorderFirst {
			name = "recorder_first"
		}
		t.Run(name, func(t *testing.T) {
			scheduler := &runtimeRecoveryScheduler{
				started: make(chan time.Duration, 4),
				wait: func(ctx context.Context, _ time.Duration) error {
					<-ctx.Done()
					return ctx.Err()
				},
			}
			session, recorder, writer, now := openMessageEventRuntime(t, scheduler)
			runtime := session.(*sessionRuntime)
			if recorderFirst {
				emitRuntimeRecoveryExit(
					recorder,
					recorder.currentAttemptID,
					RecorderNetworkFailureErrorCode,
					now,
				)
				waitRuntimeRecoveryDelay(t, scheduler.started, "recorder recovery wait")
				if _, err := session.(MessageDisconnectSession).MarkMessageDisconnected(
					context.Background(), newV7(t), now.Add(time.Second),
				); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := session.(MessageDisconnectSession).MarkMessageDisconnected(
					context.Background(), newV7(t), now,
				); err != nil {
					t.Fatal(err)
				}
				emitRuntimeRecoveryExit(
					recorder,
					recorder.currentAttemptID,
					RecorderNetworkFailureErrorCode,
					now.Add(time.Second),
				)
				deadline := time.Now().Add(3 * time.Second)
				for {
					runtime.mu.Lock()
					attached := runtime.recoveryGapID != ""
					runtime.mu.Unlock()
					if attached {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("recorder gap was not attached to message recovery")
					}
					time.Sleep(time.Millisecond)
				}
			}

			runtime.mu.Lock()
			messageGapID := runtime.messageRecoveryGapID
			recorderGapID := runtime.recoveryGapID
			runtime.mu.Unlock()
			if messageGapID == "" || recorderGapID == "" {
				t.Fatalf("runtime gaps = message:%t recorder:%t", messageGapID != "", recorderGapID != "")
			}
			recovered, err := session.Rebind(
				context.Background(), newV7(t), newFakeCaptureSource(nil),
			)
			if err != nil || recovered.RecordingStatus != RecordingActive {
				t.Fatalf("combined runtime Rebind() = (%+v, %v)", recovered, err)
			}
			messageGap, found, err := queryMessageRecoveryGap(
				context.Background(), writer,
				messageRecoveryGapSelectSQL+` WHERE session_id = ? AND id = ?`,
				recovered.ID, messageGapID,
			)
			if err != nil || !found {
				t.Fatalf("query runtime message gap = (%+v, %t, %v)", messageGap, found, err)
			}
			recorderGap, found, err := queryRecorderRecoveryGap(
				context.Background(), writer,
				recorderRecoveryGapSelectSQL+` WHERE session_id = ? AND id = ?`,
				recovered.ID, recorderGapID,
			)
			if err != nil || !found {
				t.Fatalf("query runtime recorder gap = (%+v, %t, %v)", recorderGap, found, err)
			}
			if messageGap.EndedAtMS == nil || recorderGap.EndedAtMS == nil ||
				!messageGap.Recovered || !recorderGap.Recovered ||
				*messageGap.EndedAtMS != *recorderGap.EndedAtMS {
				t.Fatalf("runtime combined gaps = message:%+v recorder:%+v", messageGap, recorderGap)
			}
			recorder.mu.Lock()
			rebinds, stops := recorder.rebinds, recorder.stops
			recorder.mu.Unlock()
			if rebinds != 1 || stops != 0 {
				t.Fatalf("runtime recorder calls = rebind:%d stop:%d", rebinds, stops)
			}
			if _, err := session.Finalize(context.Background(), newV7(t), FinalizeShutdown); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func openMessageEventRuntime(
	t *testing.T,
	scheduler *runtimeRecoveryScheduler,
) (Session, *fakeEventRecorder, *sql.DB, time.Time) {
	t.Helper()
	repository, store, _, roomID, now := openRepository(t)
	recorder := newFakeEventRecorder(newV7(t))
	coordinator, err := newTestCoordinator(repository, CoordinatorOptions{
		Now:               func() time.Time { return now.Add(2 * time.Second) },
		RecoveryScheduler: scheduler,
		RecoveryPolicy: RecorderRecoveryPolicy{
			Backoff: []time.Duration{time.Second}, MaxAttempts: 2, Window: time.Minute,
		},
		RecorderFactory: func(ctx context.Context, session LiveSession, _ OpenRequest, _ CaptureSource) (Recorder, error) {
			opened, openErr := repository.OpenSessionMedia(ctx, OpenSessionMediaInput{
				SessionID: session.ID, RelativePath: "message-runtime/" + session.ID,
				StartedAt: now.UnixMilli(),
			})
			if openErr != nil {
				return nil, openErr
			}
			_, persistErr := repository.PersistMediaSnapshot(ctx, PersistMediaSnapshotInput{
				SessionID: session.ID, ExpectedRevision: opened.Session.ManifestRevision,
				State: SessionMediaOpen,
				Attempts: []MediaAttempt{{
					ID: recorder.currentAttemptID, Ordinal: 1, StartedAt: now.UnixMilli(),
					SegmentSeconds: 300, Committed: true, Protocol: "flv", Codec: "h264",
				}},
				UpdatedAt: now.Add(time.Second).UnixMilli(),
			})
			if persistErr != nil {
				return nil, persistErr
			}
			return recorder, nil
		},
	})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	session, err := coordinator.Open(context.Background(), OpenRequest{
		RoomConfigID: roomID, OperationID: newV7(t), RecordEnabled: true, StartedAt: now,
	}, newFakeCaptureSource(nil))
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return session, recorder, store.Writer(), now.Add(2 * time.Second)
}

type messageRecoveryNoSubscriptionSource struct{}

func (*messageRecoveryNoSubscriptionSource) ResolveStreams() ([]douyinLive.ResolvedStream, error) {
	return nil, nil
}

func (*messageRecoveryNoSubscriptionSource) SubscribeMessage(douyinLive.LiveMessageHandler) string {
	return ""
}

func (*messageRecoveryNoSubscriptionSource) Unsubscribe(string) {}

func ambiguousMessageCommit(tx *sql.Tx) error {
	if err := tx.Commit(); err != nil {
		return err
	}
	return errors.New("ambiguous commit outcome")
}

func (fixture *recorderRecoveryFixture) messageGap(t *testing.T, gapID string) messageRecoveryGap {
	t.Helper()
	gap, found, err := queryMessageRecoveryGap(
		context.Background(), fixture.writer,
		messageRecoveryGapSelectSQL+` WHERE session_id = ? AND id = ?`,
		fixture.session.ID, gapID,
	)
	if err != nil || !found {
		t.Fatalf("query message gap = (%+v, %t, %v)", gap, found, err)
	}
	return gap
}

func (fixture *recorderRecoveryFixture) messageGapCount(t *testing.T) int {
	t.Helper()
	var count int
	if err := fixture.writer.QueryRow(
		`SELECT COUNT(*) FROM capture_gaps WHERE session_id = ? AND kind = ?`,
		fixture.session.ID, messageRecoveryGapKind,
	).Scan(&count); err != nil {
		t.Fatalf("count message gaps: %v", err)
	}
	return count
}

func singleMessageGap(t *testing.T, writer *sql.DB, sessionID string) messageRecoveryGap {
	t.Helper()
	gap, found, err := queryMessageRecoveryGap(
		context.Background(), writer,
		messageRecoveryGapSelectSQL+` WHERE session_id = ? AND kind = ?`,
		sessionID, messageRecoveryGapKind,
	)
	if err != nil || !found {
		t.Fatalf("query single message gap = (%+v, %t, %v)", gap, found, err)
	}
	return gap
}

func TestSessionRecoveryEventFormattingRedactsCorrelationIDs(t *testing.T) {
	sessionID := newV7(t)
	operationID := newV7(t)
	event := SessionRecoveryEvent{
		SessionID:       sessionID,
		OperationID:     operationID,
		State:           SessionRecoveryRetryScheduled,
		RecordingStatus: RecordingReconnecting,
		ErrorCode:       RecorderNetworkFailureErrorCode,
		RetryAt:         123,
		RestartAttempt:  2,
		OccurredAt:      100,
	}

	for _, formatted := range []string{
		event.String(),
		event.GoString(),
		fmt.Sprintf("%v", event),
		fmt.Sprintf("%#v", event),
	} {
		if strings.Contains(formatted, sessionID) ||
			strings.Contains(formatted, operationID) {
			t.Fatalf("recovery event formatter leaked correlation: %s", formatted)
		}
		if !strings.Contains(formatted, string(event.State)) ||
			!strings.Contains(formatted, event.ErrorCode) {
			t.Fatalf("recovery event formatter lost safe state: %s", formatted)
		}
	}
}

type invalidSuccessMessageRecoveryJournal struct {
	delegate      MessageRecoveryJournal
	invalidBegin  bool
	invalidFinish bool
}

func (journal *invalidSuccessMessageRecoveryJournal) BeginMessageRecovery(
	ctx context.Context,
	input BeginMessageRecoveryInput,
) (MessageRecoveryJournalEntry, error) {
	if journal.invalidBegin {
		return MessageRecoveryJournalEntry{}, nil
	}
	return journal.delegate.BeginMessageRecovery(ctx, input)
}

func (journal *invalidSuccessMessageRecoveryJournal) FinishMessageRecovery(
	ctx context.Context,
	input FinishMessageRecoveryInput,
) (LiveSession, error) {
	if journal.invalidFinish {
		return LiveSession{}, nil
	}
	return journal.delegate.FinishMessageRecovery(ctx, input)
}

func (journal *invalidSuccessMessageRecoveryJournal) CloseMessageRecovery(
	ctx context.Context,
	input CloseMessageRecoveryInput,
) error {
	return journal.delegate.CloseMessageRecovery(ctx, input)
}

func TestCoordinatorMessageRecoveryFailsClosedOnInvalidSuccessDTO(t *testing.T) {
	t.Run("begin", func(t *testing.T) {
		repository, store, _, roomID, now := openRepository(t)
		defer store.Close()
		journal := &invalidSuccessMessageRecoveryJournal{
			delegate:     repository,
			invalidBegin: true,
		}
		coordinator, err := newTestCoordinator(repository, CoordinatorOptions{
			Now:            func() time.Time { return now },
			MessageJournal: journal,
		})
		if err != nil {
			t.Fatal(err)
		}
		session, err := coordinator.Open(context.Background(), OpenRequest{
			RoomConfigID: roomID,
			OperationID:  newV7(t),
			StartedAt:    now,
		}, newFakeCaptureSource(nil))
		if err != nil {
			t.Fatal(err)
		}
		before := session.Snapshot()
		marked, markErr := session.(MessageDisconnectSession).MarkMessageDisconnected(
			context.Background(), newV7(t), now.Add(time.Second),
		)
		if !errors.Is(markErr, ErrRecoveryContractInvalid) {
			t.Fatalf("invalid begin error = %v, want contract failure", markErr)
		}
		if marked.ID != before.ID || marked.OperationID != before.OperationID ||
			marked.RecordingStatus != before.RecordingStatus {
			t.Fatal("invalid begin mutated the returned session")
		}
		runtime := session.(*sessionRuntime)
		runtime.mu.Lock()
		messageGapID := runtime.messageRecoveryGapID
		runtime.mu.Unlock()
		if messageGapID != "" {
			t.Fatal("invalid begin installed a runtime message gap")
		}
		if _, err := session.Finalize(context.Background(), newV7(t), FinalizeShutdown); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("finish", func(t *testing.T) {
		repository, store, _, roomID, now := openRepository(t)
		defer store.Close()
		journal := &invalidSuccessMessageRecoveryJournal{
			delegate:      repository,
			invalidFinish: true,
		}
		recorder := &fakeRecorder{}
		coordinator, err := newTestCoordinator(repository, CoordinatorOptions{
			Now:            func() time.Time { return now },
			MessageJournal: journal,
			RecorderFactory: func(context.Context, LiveSession, OpenRequest, CaptureSource) (Recorder, error) {
				return recorder, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		firstSource := newFakeCaptureSource(nil)
		session, err := coordinator.Open(context.Background(), OpenRequest{
			RoomConfigID:  roomID,
			OperationID:   newV7(t),
			RecordEnabled: true,
			StartedAt:     now,
		}, firstSource)
		if err != nil {
			t.Fatal(err)
		}
		secondSource := newFakeCaptureSource(nil)
		rebound, rebindErr := session.Rebind(context.Background(), newV7(t), secondSource)
		if !errors.Is(rebindErr, ErrRecoveryContractInvalid) {
			t.Fatalf("invalid finish error = %v, want contract failure", rebindErr)
		}
		if rebound.RecordingStatus != RecordingReconnecting {
			t.Fatalf("invalid finish recording status = %q", rebound.RecordingStatus)
		}
		secondSource.mu.Lock()
		unsubscribed := secondSource.unsubscribed
		activeSubscriptions := len(secondSource.handlers)
		secondSource.mu.Unlock()
		if unsubscribed != 1 || activeSubscriptions != 0 {
			t.Fatalf("invalid finish source ownership = unsubscribed:%d active:%d", unsubscribed, activeSubscriptions)
		}
		runtime := session.(*sessionRuntime)
		runtime.mu.Lock()
		ownerRetained := runtime.source == firstSource && runtime.subscriptionID != ""
		runtime.mu.Unlock()
		if !ownerRetained {
			t.Fatal("invalid finish replaced the established message owner")
		}
		if _, err := session.Finalize(context.Background(), newV7(t), FinalizeShutdown); err != nil {
			t.Fatal(err)
		}
	})
}
