package eventstore

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestManagerRecoverAndCloseSessionRejectsMissingCheckpointWithDurableEvidence(t *testing.T) {
	fixture := newManagerFixture(t, nil)
	eventAt := fixture.now.Add(4 * time.Second)
	foldAt := fixture.now.Add(6 * time.Second)
	event := fixture.sourceEvent(1, "missing-checkpoint-event", "missing-checkpoint-dedupe")
	event.ReceivedAt = eventAt
	event.SessionOffsetMS = eventAt.Sub(fixture.now).Milliseconds()
	combo := GiftComboState{
		SessionID: fixture.sessionID, ComboKey: "missing-checkpoint-combo",
		Status: ComboOpen, GiftID: "gift-1", GiftName: "Star", TotalCount: 1,
		FirstSequence: 1, LastSequence: 1, StartedAt: eventAt, UpdatedAt: foldAt,
		NormalizerVersion: "normalizer-v1",
	}
	checkpoint := fixture.checkpoint(1, CheckpointOpen)
	checkpoint.PrivacyKeyID = fixture.manager.privacy.KeyID()
	if err := fixture.writer.PersistBatch(context.Background(), Batch{
		SessionID: fixture.sessionID, Events: []Event{event},
		GiftCombos: []GiftComboState{combo}, Checkpoint: checkpoint,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Writer().Exec(
		"DELETE FROM event_ingest_checkpoints WHERE session_id = ?", fixture.sessionID,
	); err != nil {
		t.Fatal(err)
	}
	if _, found, err := fixture.writer.Checkpoint(context.Background(), fixture.sessionID); err != nil || found {
		t.Fatalf("checkpoint after external delete found=%v error=%v", found, err)
	}

	minimum := fixture.now.Add(time.Second)
	cutoff, err := fixture.manager.RecoverAndCloseSession(
		context.Background(), fixture.descriptor, minimum,
	)
	if !errors.Is(err, ErrRecoveryCutoff) || errors.Is(err, ErrRecoveryDeferred) {
		t.Fatalf("RecoverAndCloseSession() error = %v", err)
	}
	if !cutoff.Equal(foldAt) {
		t.Fatalf("RecoverAndCloseSession() cutoff = %v, want %v", cutoff, foldAt)
	}
	var events, openFolds int
	if err := fixture.store.Reader().QueryRow(
		"SELECT COUNT(*) FROM live_events WHERE session_id = ?", fixture.sessionID,
	).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Reader().QueryRow(
		"SELECT COUNT(*) FROM gift_combo_states WHERE session_id = ? AND status = 'open'",
		fixture.sessionID,
	).Scan(&openFolds); err != nil {
		t.Fatal(err)
	}
	if events != 1 || openFolds != 1 {
		t.Fatalf("durable evidence events/folds = %d/%d", events, openFolds)
	}
}

func TestManagerRecoverAndCloseSessionAllowsTrulyEmptyMissingCheckpoint(t *testing.T) {
	fixture := newManagerFixture(t, nil)
	minimum := fixture.now.Add(time.Second)
	for attempt := 1; attempt <= 2; attempt++ {
		cutoff, err := fixture.manager.RecoverAndCloseSession(
			context.Background(), fixture.descriptor, minimum,
		)
		if err != nil {
			t.Fatalf("attempt %d RecoverAndCloseSession() error = %v", attempt, err)
		}
		if !cutoff.Equal(minimum) {
			t.Fatalf("attempt %d cutoff = %v, want %v", attempt, cutoff, minimum)
		}
	}
}

func TestMissingCheckpointEvidenceQueryFailureClassifiesDeferred(t *testing.T) {
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
	_, evidence, queryErr := fixture.manager.latestRecoveryCutoffAndEvidence(
		context.Background(), fixture.sessionID, fixture.now.Add(time.Second),
	)
	fixture.writer.db = originalDB
	if evidence || !errors.Is(queryErr, ErrPersistenceDegraded) {
		t.Fatalf("latestRecoveryCutoffAndEvidence() evidence=%v error=%v", evidence, queryErr)
	}
	classified := classifyRecoveryFinalizationError(context.Background(), queryErr)
	if !errors.Is(classified, ErrRecoveryDeferred) ||
		!errors.Is(classified, ErrPersistenceDegraded) {
		t.Fatalf("classified evidence query error = %v", classified)
	}
	if strings.Contains(classified.Error(), "database is closed") {
		t.Fatalf("classified evidence query leaked native error: %q", classified)
	}
}
