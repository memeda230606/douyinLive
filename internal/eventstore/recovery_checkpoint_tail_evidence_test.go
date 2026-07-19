package eventstore

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestManagerRecoverAndCloseSessionRejectsSourceBeyondClosedCheckpoint(t *testing.T) {
	fixture := newManagerFixture(t, nil)
	first := fixture.sourceEvent(1, "closed-source-1", "closed-source-dedupe-1")
	second := fixture.sourceEvent(2, "closed-source-2", "closed-source-dedupe-2")
	first.ReceivedAt = fixture.now.Add(time.Second)
	first.SessionOffsetMS = 1000
	second.ReceivedAt = fixture.now.Add(5 * time.Second)
	second.SessionOffsetMS = 5000
	checkpoint := fixture.checkpoint(2, CheckpointOpen)
	checkpoint.PrivacyKeyID = fixture.manager.privacy.KeyID()
	if err := fixture.writer.PersistBatch(context.Background(), Batch{
		SessionID: fixture.sessionID, Events: []Event{first, second}, Checkpoint: checkpoint,
	}); err != nil {
		t.Fatal(err)
	}
	replaceEventCheckpointForRecoveryTest(t, fixture, 1, CheckpointClosed)

	cutoff, err := fixture.manager.RecoverAndCloseSession(
		context.Background(), fixture.descriptor, fixture.now.Add(500*time.Millisecond),
	)
	if !errors.Is(err, ErrRecoveryCutoff) || errors.Is(err, ErrRecoveryDeferred) {
		t.Fatalf("RecoverAndCloseSession() error = %v", err)
	}
	if !cutoff.Equal(second.ReceivedAt) {
		t.Fatalf("RecoverAndCloseSession() cutoff = %v, want %v", cutoff, second.ReceivedAt)
	}
}

func TestManagerRecoverAndCloseSessionIgnoresAggregateBeyondClosedCheckpoint(t *testing.T) {
	fixture := newManagerFixture(t, nil)
	source := fixture.sourceEvent(1, "closed-source", "closed-source-dedupe")
	sourceTail := fixture.sourceEvent(2, "closed-source-tail", "closed-source-tail-dedupe")
	aggregate := fixture.aggregateEvent(2, "closed-aggregate", "closed-aggregate-dedupe")
	source.ReceivedAt = fixture.now.Add(time.Second)
	source.SessionOffsetMS = 1000
	sourceTail.ReceivedAt = fixture.now.Add(5 * time.Second)
	sourceTail.SessionOffsetMS = 5000
	aggregate.ReceivedAt = fixture.now.Add(7 * time.Second)
	aggregate.SessionOffsetMS = 7000
	checkpoint := fixture.checkpoint(2, CheckpointOpen)
	checkpoint.PrivacyKeyID = fixture.manager.privacy.KeyID()
	if err := fixture.writer.PersistBatch(context.Background(), Batch{
		SessionID: fixture.sessionID, Events: []Event{source, sourceTail}, Checkpoint: checkpoint,
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.writer.PersistBatch(context.Background(), Batch{
		SessionID: fixture.sessionID, PreviousSequence: checkpoint.CommittedSequence,
		Events: []Event{aggregate}, Checkpoint: checkpoint,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Writer().Exec(
		"DELETE FROM live_events WHERE session_id = ? AND id = ?",
		fixture.sessionID, sourceTail.ID,
	); err != nil {
		t.Fatal(err)
	}
	replaceEventCheckpointForRecoveryTest(t, fixture, 1, CheckpointClosed)

	cutoff, err := fixture.manager.RecoverAndCloseSession(
		context.Background(), fixture.descriptor, fixture.now.Add(500*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("RecoverAndCloseSession() error = %v", err)
	}
	if !cutoff.Equal(aggregate.ReceivedAt) {
		t.Fatalf("RecoverAndCloseSession() cutoff = %v, want %v", cutoff, aggregate.ReceivedAt)
	}
}

func TestClosedCheckpointSourceSequenceQueryFailureClassifiesDeferred(t *testing.T) {
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
	_, found, queryErr := fixture.writer.latestRecoverySourceSequence(
		context.Background(), fixture.sessionID,
	)
	fixture.writer.db = originalDB
	if found {
		t.Fatal("closed database reported a source sequence")
	}
	classified := classifyRecoveryFinalizationError(
		context.Background(), stablePersistenceError(queryErr),
	)
	if !errors.Is(classified, ErrRecoveryDeferred) ||
		!errors.Is(classified, ErrPersistenceDegraded) {
		t.Fatalf("classified source query error = %v", classified)
	}
	if strings.Contains(classified.Error(), "database is closed") {
		t.Fatalf("classified source query leaked native error: %q", classified)
	}
}

func TestMissingCheckpointUsesOnlyLocalDropLedgerGapEvidence(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		startedAt   time.Duration
		endedAt     *time.Duration
		wantCutoff  time.Duration
		reasonCode  string
		wantFailure bool
	}{
		{
			name: "closed local drop gap uses ended at", startedAt: 3 * time.Second,
			endedAt: durationPointer(5 * time.Second), wantCutoff: 5 * time.Second,
			reasonCode: eventDroppedLocalReasonCode, wantFailure: true,
		},
		{
			name: "open local drop gap uses started at", startedAt: 4 * time.Second,
			wantCutoff: 4 * time.Second, reasonCode: eventDroppedLocalReasonCode,
			wantFailure: true,
		},
		{
			name: "capture startup event gap is ignored", startedAt: 6 * time.Second,
			endedAt: durationPointer(8 * time.Second), wantCutoff: time.Second,
			reasonCode: "STARTUP_EVENT_RECOVERY_INCOMPLETE", wantFailure: false,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newManagerFixture(t, nil)
			minimum := fixture.now.Add(time.Second)
			gap := CaptureGap{
				ID:        "checkpoint-gap-" + strings.ReplaceAll(testCase.name, " ", "-"),
				SessionID: fixture.sessionID, Kind: eventPersistenceGapKind,
				StartedAt:     fixture.now.Add(testCase.startedAt),
				StartOffsetMS: testCase.startedAt.Milliseconds(),
				Severity:      "error", Recovered: false, ReasonCode: testCase.reasonCode,
				DetailsJSON: `{"count":1}`,
				DedupeKey:   "checkpoint-gap-dedupe-" + strings.ReplaceAll(testCase.name, " ", "-"),
			}
			if testCase.endedAt != nil {
				endedAt := fixture.now.Add(*testCase.endedAt)
				endOffset := testCase.endedAt.Milliseconds()
				gap.EndedAt = &endedAt
				gap.EndOffsetMS = &endOffset
			}
			checkpoint := fixture.checkpoint(0, CheckpointOpen)
			checkpoint.PrivacyKeyID = fixture.manager.privacy.KeyID()
			if err := fixture.writer.PersistBatch(context.Background(), Batch{
				SessionID: fixture.sessionID, Gaps: []CaptureGap{gap}, Checkpoint: checkpoint,
			}); err != nil {
				t.Fatal(err)
			}
			deleteEventCheckpointForRecoveryTest(t, fixture)

			cutoff, err := fixture.manager.RecoverAndCloseSession(
				context.Background(), fixture.descriptor, minimum,
			)
			if testCase.wantFailure {
				if !errors.Is(err, ErrRecoveryCutoff) || errors.Is(err, ErrRecoveryDeferred) {
					t.Fatalf("RecoverAndCloseSession() error = %v", err)
				}
			} else if err != nil {
				t.Fatalf("RecoverAndCloseSession() error = %v", err)
			}
			want := fixture.now.Add(testCase.wantCutoff)
			if !cutoff.Equal(want) {
				t.Fatalf("RecoverAndCloseSession() cutoff = %v, want %v", cutoff, want)
			}
		})
	}
}

func replaceEventCheckpointForRecoveryTest(
	t *testing.T,
	fixture managerFixture,
	sequence int64,
	state CheckpointState,
) {
	t.Helper()
	deleteEventCheckpointForRecoveryTest(t, fixture)
	checkpoint := fixture.checkpoint(sequence, state)
	checkpoint.PrivacyKeyID = fixture.manager.privacy.KeyID()
	if _, err := fixture.store.Writer().Exec(`INSERT INTO event_ingest_checkpoints(
		session_id, committed_sequence, state, privacy_key_id,
		spool_file, spool_offset, raw_file, raw_offset, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		checkpoint.SessionID, checkpoint.CommittedSequence, checkpoint.State,
		checkpoint.PrivacyKeyID, checkpoint.Spool.File, checkpoint.Spool.Offset,
		checkpoint.Raw.File, checkpoint.Raw.Offset, checkpoint.UpdatedAt.UnixMilli(),
	); err != nil {
		t.Fatal(err)
	}
}

func deleteEventCheckpointForRecoveryTest(t *testing.T, fixture managerFixture) {
	t.Helper()
	if _, err := fixture.store.Writer().Exec(
		"DELETE FROM event_ingest_checkpoints WHERE session_id = ?", fixture.sessionID,
	); err != nil {
		t.Fatal(err)
	}
}

func durationPointer(value time.Duration) *time.Duration { return &value }
