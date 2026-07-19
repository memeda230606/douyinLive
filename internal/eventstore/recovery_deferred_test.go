package eventstore

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestClassifyRecoveryFinalizationErrorMarksAmbiguousFailuresDeferred(t *testing.T) {
	native := errors.New("native sqlite detail must stay private")
	testCases := []struct {
		name string
		err  error
		want error
	}{
		{
			name: "stable persistence degradation",
			err:  errors.Join(ErrPersistenceDegraded, native),
			want: ErrPersistenceDegraded,
		},
		{
			name: "database busy before stabilization",
			err:  errors.Join(ErrPersistenceBusy, native),
			want: ErrPersistenceDegraded,
		},
		{
			name: "component cancellation with healthy caller",
			err:  errors.Join(context.Canceled, native),
			want: ErrPersistenceDegraded,
		},
		{
			name: "component deadline with healthy caller",
			err:  errors.Join(context.DeadlineExceeded, native),
			want: ErrPersistenceDegraded,
		},
		{
			name: "manager closed",
			err:  errors.Join(ErrManagerClosed, native),
			want: ErrManagerClosed,
		},
		{
			name: "session already open",
			err:  errors.Join(ErrSessionAlreadyOpen, native),
			want: ErrSessionAlreadyOpen,
		},
		{
			name: "event manager not ready",
			err:  errors.Join(ErrEventManagerNotReady, native),
			want: ErrEventManagerNotReady,
		},
		{
			name: "drop ledger IO",
			err:  errors.Join(ErrDropLedgerIO, native),
			want: ErrDropLedgerIO,
		},
		{
			name: "unknown status",
			err:  native,
			want: ErrPersistenceDegraded,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			got := classifyRecoveryFinalizationError(context.Background(), testCase.err)
			if !errors.Is(got, ErrRecoveryDeferred) || !errors.Is(got, testCase.want) {
				t.Fatalf("classifyRecoveryFinalizationError() = %v", got)
			}
			if strings.Contains(got.Error(), native.Error()) {
				t.Fatalf("classified error leaks native detail: %q", got)
			}
		})
	}
}

func TestClassifyRecoveryFinalizationErrorKeepsDamagePermanent(t *testing.T) {
	native := errors.New("private damaged path detail")
	testCases := []struct {
		name string
		err  error
		want error
	}{
		{name: "privacy key mismatch", err: errors.Join(ErrPrivacyKeyMismatch, native), want: ErrPrivacyKeyMismatch},
		{name: "session path invalid", err: errors.Join(ErrSessionPathInvalid, native), want: ErrSessionPathInvalid},
		{name: "recovery cutoff invalid", err: errors.Join(ErrRecoveryCutoff, native), want: ErrRecoveryCutoff},
		{name: "event spool fatal", err: errors.Join(ErrEventSpoolFatal, native), want: ErrEventSpoolFatal},
		{name: "frame corrupt", err: errors.Join(ErrFrameCorrupt, native), want: ErrEventSpoolFatal},
		{name: "drop ledger corrupt", err: errors.Join(ErrDropLedgerCorrupt, native), want: ErrDropLedgerCorrupt},
		{name: "drop ledger invalid", err: errors.Join(ErrDropLedgerInvalid, native), want: ErrDropLedgerInvalid},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			got := classifyRecoveryFinalizationError(context.Background(), testCase.err)
			if !errors.Is(got, testCase.want) || errors.Is(got, ErrRecoveryDeferred) {
				t.Fatalf("classifyRecoveryFinalizationError() = %v", got)
			}
			if strings.Contains(got.Error(), native.Error()) {
				t.Fatalf("classified error leaks native detail: %q", got)
			}
		})
	}
}

func TestClassifyRecoveryFinalizationErrorPreservesCallerCancellation(t *testing.T) {
	native := errors.New("private cancellation detail")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := classifyRecoveryFinalizationError(ctx, errors.Join(ErrPersistenceDegraded, native))
	if !errors.Is(got, context.Canceled) || errors.Is(got, ErrRecoveryDeferred) {
		t.Fatalf("classifyRecoveryFinalizationError() = %v", got)
	}
	if strings.Contains(got.Error(), native.Error()) {
		t.Fatalf("classified error leaks cancellation detail: %q", got)
	}
}

func TestManagerRecoverAndCloseSessionDefersStableDatabaseFailure(t *testing.T) {
	fixture := newManagerFixture(t, nil)
	closedDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := closedDB.Close(); err != nil {
		t.Fatal(err)
	}
	originalDB := fixture.writer.db
	fixture.writer.db = closedDB
	_, recoveryErr := fixture.manager.RecoverAndCloseSession(
		context.Background(), fixture.descriptor, fixture.now.Add(time.Second),
	)
	fixture.writer.db = originalDB
	if !errors.Is(recoveryErr, ErrRecoveryDeferred) ||
		!errors.Is(recoveryErr, ErrPersistenceDegraded) {
		t.Fatalf("RecoverAndCloseSession() error = %v", recoveryErr)
	}
	if strings.Contains(recoveryErr.Error(), "database is closed") {
		t.Fatalf("RecoverAndCloseSession() leaked native database error: %q", recoveryErr)
	}
}

func TestManagerRecoverAndCloseSessionDefersClosedManager(t *testing.T) {
	fixture := newManagerFixture(t, nil)
	if err := fixture.manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := fixture.manager.RecoverAndCloseSession(
		context.Background(), fixture.descriptor, fixture.now.Add(time.Second),
	)
	if !errors.Is(err, ErrRecoveryDeferred) || !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("RecoverAndCloseSession() error = %v", err)
	}
}

func TestManagerRecoverAndCloseSessionDoesNotDeferPermanentFailure(t *testing.T) {
	fixture := newManagerFixture(t, nil)
	wantCutoff := fixture.now.Add(5 * time.Second)
	event := fixture.sourceEvent(1, "privacy-cutoff-event", "privacy-cutoff-dedupe")
	event.ReceivedAt = wantCutoff
	event.SessionOffsetMS = wantCutoff.Sub(fixture.now).Milliseconds()
	checkpoint := fixture.checkpoint(1, CheckpointOpen)
	checkpoint.PrivacyKeyID = "fedcba9876543210"
	if err := fixture.writer.PersistBatch(context.Background(), Batch{
		SessionID: fixture.sessionID, Events: []Event{event}, Checkpoint: checkpoint,
	}); err != nil {
		t.Fatal(err)
	}
	cutoff, err := fixture.manager.RecoverAndCloseSession(
		context.Background(), fixture.descriptor, fixture.now.Add(time.Second),
	)
	if !errors.Is(err, ErrPrivacyKeyMismatch) || errors.Is(err, ErrRecoveryDeferred) {
		t.Fatalf("RecoverAndCloseSession() error = %v", err)
	}
	if !cutoff.Equal(wantCutoff) {
		t.Fatalf("RecoverAndCloseSession() cutoff = %v, want %v", cutoff, wantCutoff)
	}
	if strings.Contains(err.Error(), fixture.dataRoot) {
		t.Fatalf("permanent recovery error leaked data root: %q", err)
	}
}

func TestManagerRecoverAndCloseSessionRefreshesCutoffForInvalidSessionPath(t *testing.T) {
	fixture := newManagerFixture(t, nil)
	wantCutoff := fixture.now.Add(4 * time.Second)
	event := fixture.sourceEvent(1, "path-cutoff-event", "path-cutoff-dedupe")
	event.ReceivedAt = wantCutoff
	event.SessionOffsetMS = wantCutoff.Sub(fixture.now).Milliseconds()
	checkpoint := fixture.checkpoint(1, CheckpointOpen)
	checkpoint.PrivacyKeyID = fixture.manager.privacy.KeyID()
	if err := fixture.writer.PersistBatch(context.Background(), Batch{
		SessionID: fixture.sessionID, Events: []Event{event}, Checkpoint: checkpoint,
	}); err != nil {
		t.Fatal(err)
	}
	descriptor := fixture.descriptor
	descriptor.DataPath = "../private/" + fixture.sessionID
	cutoff, err := fixture.manager.RecoverAndCloseSession(
		context.Background(), descriptor, fixture.now.Add(time.Second),
	)
	if !errors.Is(err, ErrSessionPathInvalid) || errors.Is(err, ErrRecoveryDeferred) {
		t.Fatalf("RecoverAndCloseSession() error = %v", err)
	}
	if !cutoff.Equal(wantCutoff) {
		t.Fatalf("RecoverAndCloseSession() cutoff = %v, want %v", cutoff, wantCutoff)
	}
	if strings.Contains(err.Error(), descriptor.DataPath) || strings.Contains(err.Error(), fixture.dataRoot) {
		t.Fatalf("invalid-path recovery leaked path detail: %q", err)
	}
}

func TestManagerRecoverAndCloseSessionRefreshesCutoffAfterPermanentTailDamage(t *testing.T) {
	fixture := newManagerFixture(t, func(options *ManagerOptions) {
		options.BatchSize = 2
	})
	appendRecoveryTailWithInvalidThirdEvent(t, fixture)
	minimum := fixture.now.Add(500 * time.Millisecond)
	wantCutoff := fixture.now.Add(2 * time.Second)

	cutoff, err := fixture.manager.RecoverAndCloseSession(
		context.Background(), fixture.descriptor, minimum,
	)
	if !errors.Is(err, ErrEventSpoolFatal) || errors.Is(err, ErrRecoveryDeferred) {
		t.Fatalf("RecoverAndCloseSession() error = %v", err)
	}
	if !cutoff.Equal(wantCutoff) {
		t.Fatalf("RecoverAndCloseSession() cutoff = %v, want %v", cutoff, wantCutoff)
	}
	checkpoint, found, checkpointErr := fixture.writer.Checkpoint(
		context.Background(), fixture.sessionID,
	)
	if checkpointErr != nil || !found || checkpoint.CommittedSequence != 2 {
		t.Fatalf("partially recovered checkpoint = (%+v, %v, %v)", checkpoint, found, checkpointErr)
	}
}

func TestManagerRecoverAndCloseSessionDefersWhenDamageCutoffRefreshFails(t *testing.T) {
	var inspectCommitted func()
	fixture := newManagerFixture(t, func(options *ManagerOptions) {
		options.BatchSize = 2
		options.Now = func() time.Time {
			if inspectCommitted != nil {
				inspectCommitted()
			}
			return time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
		}
	})
	appendRecoveryTailWithInvalidThirdEvent(t, fixture)
	closedDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := closedDB.Close(); err != nil {
		t.Fatal(err)
	}
	originalDB := fixture.writer.db
	defer func() { fixture.writer.db = originalDB }()
	var inspectErr error
	swapped := false
	inspectCommitted = func() {
		if swapped || inspectErr != nil {
			return
		}
		checkpoint, found, checkpointErr := fixture.writer.Checkpoint(
			context.Background(), fixture.sessionID,
		)
		if checkpointErr != nil {
			inspectErr = checkpointErr
			return
		}
		if found && checkpoint.CommittedSequence == 2 {
			fixture.writer.db = closedDB
			swapped = true
		}
	}
	minimum := fixture.now.Add(500 * time.Millisecond)
	cutoff, recoveryErr := fixture.manager.RecoverAndCloseSession(
		context.Background(), fixture.descriptor, minimum,
	)
	fixture.writer.db = originalDB
	inspectCommitted = nil
	if inspectErr != nil {
		t.Fatalf("inspect committed checkpoint: %v", inspectErr)
	}
	if !swapped {
		t.Fatal("database was not replaced after the durable prefix commit")
	}
	if !errors.Is(recoveryErr, ErrRecoveryDeferred) ||
		!errors.Is(recoveryErr, ErrPersistenceDegraded) ||
		errors.Is(recoveryErr, ErrEventSpoolFatal) {
		t.Fatalf("RecoverAndCloseSession() error = %v", recoveryErr)
	}
	if !cutoff.Equal(minimum) {
		t.Fatalf("deferred recovery cutoff = %v, want prior trusted %v", cutoff, minimum)
	}
	if strings.Contains(recoveryErr.Error(), "database is closed") {
		t.Fatalf("deferred recovery leaked native database error: %q", recoveryErr)
	}
	checkpoint, found, checkpointErr := fixture.writer.Checkpoint(
		context.Background(), fixture.sessionID,
	)
	if checkpointErr != nil || !found || checkpoint.CommittedSequence != 2 {
		t.Fatalf("durable prefix checkpoint = (%+v, %v, %v)", checkpoint, found, checkpointErr)
	}
}

func TestManagerRecoverAndCloseSessionAuditsClosedCheckpointWithOpenFold(t *testing.T) {
	t.Parallel()
	fixture := newManagerFixture(t, nil)
	foldAt := fixture.now.Add(4 * time.Second)
	event := fixture.sourceEvent(1, "closed-fold-source", "closed-fold-source")
	event.ReceivedAt = foldAt
	event.SessionOffsetMS = foldAt.Sub(fixture.now).Milliseconds()
	combo := GiftComboState{
		SessionID: fixture.sessionID, ComboKey: "orphan-open-fold", Status: ComboOpen,
		GiftID: "gift", TotalCount: 1, FirstSequence: 1, LastSequence: 1,
		StartedAt: fixture.now, UpdatedAt: foldAt, NormalizerVersion: "recovery-test-v1",
	}
	if err := fixture.writer.PersistBatch(context.Background(), Batch{
		SessionID: fixture.sessionID, Events: []Event{event},
		GiftCombos: []GiftComboState{combo},
		Checkpoint: fixture.checkpoint(1, CheckpointOpen),
	}); err != nil {
		t.Fatal(err)
	}
	// Simulate an externally edited database: the durable fold remains valid,
	// while only its checkpoint is incorrectly marked as closed.
	if _, err := fixture.store.Writer().Exec(`UPDATE event_ingest_checkpoints
		SET state = 'closing' WHERE session_id = ?`, fixture.sessionID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Writer().Exec(`UPDATE event_ingest_checkpoints
		SET state = 'closed' WHERE session_id = ?`, fixture.sessionID,
	); err != nil {
		t.Fatal(err)
	}
	cutoff, err := fixture.manager.RecoverAndCloseSession(
		context.Background(), fixture.descriptor, fixture.now.Add(time.Second),
	)
	if !errors.Is(err, ErrRecoveryCutoff) || errors.Is(err, ErrRecoveryDeferred) {
		t.Fatalf("RecoverAndCloseSession() error = %v", err)
	}
	if !cutoff.Equal(foldAt) {
		t.Fatalf("RecoverAndCloseSession() cutoff = %v, want %v", cutoff, foldAt)
	}
	checkpoint, found, checkpointErr := fixture.writer.Checkpoint(
		context.Background(), fixture.sessionID,
	)
	if checkpointErr != nil || !found || checkpoint.State != CheckpointClosed {
		t.Fatalf("closed checkpoint = (%+v, %t, %v)", checkpoint, found, checkpointErr)
	}
	folds, foldErr := fixture.writer.OpenGiftFolds(
		context.Background(), fixture.sessionID, nil, 2,
	)
	if foldErr != nil || len(folds) != 1 {
		t.Fatalf("open folds = (%d, %v), want preserved audit evidence", len(folds), foldErr)
	}
}

func appendRecoveryTailWithInvalidThirdEvent(t *testing.T, fixture managerFixture) {
	t.Helper()
	eventsRoot, err := resolveSessionEventsRoot(fixture.dataRoot, fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := Checkpoint{
		SessionID: fixture.sessionID, State: CheckpointOpen,
		PrivacyKeyID: fixture.manager.privacy.KeyID(), UpdatedAt: fixture.now,
	}
	if err := fixture.writer.PersistBatch(context.Background(), Batch{
		SessionID: fixture.sessionID, Checkpoint: checkpoint,
	}); err != nil {
		t.Fatal(err)
	}
	options := fixture.manager.options.SpoolOptions(eventsRoot)
	options.Root = eventsRoot
	spool, err := OpenSpool(options)
	if err != nil {
		t.Fatal(err)
	}
	envelopes := make([]IngestEnvelope, 3)
	for index := range envelopes {
		sequence := int64(index + 1)
		envelopes[index] = IngestEnvelope{
			SessionID: fixture.sessionID,
			EventID:   "018f0000-0000-7000-8000-00000000060" + string(rune('1'+index)),
			Sequence:  sequence, Method: methodChat,
			PlatformRoomID:  fixture.descriptor.PlatformRoomID,
			ReceivedAt:      fixture.now.Add(time.Duration(sequence) * time.Second),
			SessionOffsetMS: sequence * 1000,
			Payload:         []byte{byte(sequence)},
		}
	}
	// The frame is structurally valid, but belongs to another session. Spool
	// encoding admits it while startup recovery must reject the semantic tail.
	envelopes[2].SessionID = "other-session"
	results, appendErr := spool.AppendBatch(context.Background(), envelopes)
	closeErr := spool.Close(context.Background())
	if appendErr != nil || len(results) != 3 || closeErr != nil {
		t.Fatalf("AppendBatch() results=%d error=%v close=%v", len(results), appendErr, closeErr)
	}
}
