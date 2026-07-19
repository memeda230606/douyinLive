package eventstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jwwsjlm/douyinlive-proto/generated/new_douyin"
)

func TestManagerRecoverAndCloseSessionReplaysTailClosesFoldsAndIsIdempotent(t *testing.T) {
	fixture := newManagerFixture(t, nil)
	messageAt := fixture.now.Add(time.Second)
	cutoff := fixture.now.Add(2 * time.Second)
	appendRecoveryGiftSpool(t, fixture, messageAt)

	recoveredCutoff, err := fixture.manager.RecoverAndCloseSession(
		context.Background(), fixture.descriptor, cutoff,
	)
	if err != nil {
		t.Fatalf("RecoverAndCloseSession() error = %v", err)
	}
	if !recoveredCutoff.Equal(cutoff) {
		t.Fatalf("RecoverAndCloseSession() cutoff = %v, want %v", recoveredCutoff, cutoff)
	}
	assertRecoveredSessionClosed(t, fixture, cutoff)

	var eventsBefore int
	if err := fixture.store.Reader().QueryRow(
		`SELECT COUNT(*) FROM live_events WHERE session_id = ?`, fixture.sessionID,
	).Scan(&eventsBefore); err != nil {
		t.Fatal(err)
	}
	_, err = fixture.manager.RecoverAndCloseSession(
		context.Background(), fixture.descriptor, cutoff.Add(time.Second),
	)
	if err != nil {
		t.Fatalf("idempotent RecoverAndCloseSession() error = %v", err)
	}
	var eventsAfter int
	if err := fixture.store.Reader().QueryRow(
		`SELECT COUNT(*) FROM live_events WHERE session_id = ?`, fixture.sessionID,
	).Scan(&eventsAfter); err != nil {
		t.Fatal(err)
	}
	if eventsAfter != eventsBefore {
		t.Fatalf("idempotent recovery changed event count from %d to %d", eventsBefore, eventsAfter)
	}
}

func TestManagerRecoverAndCloseSessionElevatesCutoffToRecoveredEvent(t *testing.T) {
	fixture := newManagerFixture(t, nil)
	messageAt := fixture.now.Add(2 * time.Second)
	appendRecoveryGiftSpool(t, fixture, messageAt)

	cutoff, err := fixture.manager.RecoverAndCloseSession(
		context.Background(), fixture.descriptor, fixture.now.Add(time.Second),
	)
	if err != nil {
		t.Fatalf("RecoverAndCloseSession() error = %v", err)
	}
	if !cutoff.Equal(messageAt) {
		t.Fatalf("RecoverAndCloseSession() cutoff = %v, want %v", cutoff, messageAt)
	}
	assertRecoveredSessionClosed(t, fixture, messageAt)
}

func TestManagerRecoverAndCloseSessionRejectsLiveOwner(t *testing.T) {
	fixture := newManagerFixture(t, nil)
	sink, err := fixture.manager.OpenSession(context.Background(), fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.manager.RecoverAndCloseSession(
		context.Background(), fixture.descriptor, fixture.now.Add(time.Second),
	)
	if !errors.Is(err, ErrSessionAlreadyOpen) {
		t.Fatalf("RecoverAndCloseSession() error = %v, want %v", err, ErrSessionAlreadyOpen)
	}
	if err := sink.FlushAndClose(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func appendRecoveryGiftSpool(t *testing.T, fixture managerFixture, receivedAt time.Time) {
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
	gift := &new_douyin.Webcast_Im_GiftMessage{
		Common:      &new_douyin.Webcast_Im_Common{MsgId: 401},
		GiftId:      9,
		GroupId:     88,
		RepeatCount: 1,
		Gift:        &new_douyin.Webcast_Data_GiftStruct{Name: "Star", Combo: true},
	}
	spool, err := OpenSpool(DefaultSpoolOptions(eventsRoot))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spool.AppendBatch(context.Background(), []IngestEnvelope{{
		SessionID:      fixture.sessionID,
		EventID:        "018f0000-0000-7000-8000-000000000401",
		Sequence:       1,
		Method:         methodGift,
		PlatformRoomID: fixture.descriptor.PlatformRoomID,
		ReceivedAt:     receivedAt, SessionOffsetMS: receivedAt.Sub(fixture.now).Milliseconds(),
		Payload: managerProtoPayload(t, gift),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := spool.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func assertRecoveredSessionClosed(t *testing.T, fixture managerFixture, cutoff time.Time) {
	t.Helper()
	checkpoint, found, err := fixture.writer.Checkpoint(context.Background(), fixture.sessionID)
	if err != nil || !found {
		t.Fatalf("Checkpoint() found=%v error=%v", found, err)
	}
	if checkpoint.State != CheckpointClosed || checkpoint.CommittedSequence != 1 {
		t.Fatalf("closed checkpoint = %+v", checkpoint)
	}
	var status string
	var closedAt int64
	if err := fixture.store.Reader().QueryRow(
		`SELECT status, closed_at FROM gift_combo_states WHERE session_id = ?`,
		fixture.sessionID,
	).Scan(&status, &closedAt); err != nil {
		t.Fatal(err)
	}
	if status != string(ComboClosed) || closedAt != cutoff.UnixMilli() {
		t.Fatalf("closed combo = status %q at %d, want %d", status, closedAt, cutoff.UnixMilli())
	}
	var sources, aggregates int
	if err := fixture.store.Reader().QueryRow(
		`SELECT COUNT(*) FROM live_events WHERE session_id = ? AND event_role = ?`,
		fixture.sessionID, EventRoleSource,
	).Scan(&sources); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Reader().QueryRow(
		`SELECT COUNT(*) FROM live_events WHERE session_id = ? AND event_role = ?`,
		fixture.sessionID, EventRoleAggregate,
	).Scan(&aggregates); err != nil {
		t.Fatal(err)
	}
	if sources != 1 || aggregates != 1 {
		t.Fatalf("source/aggregate counts = %d/%d", sources, aggregates)
	}
}
