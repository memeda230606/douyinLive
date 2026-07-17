package eventstore

import (
	"context"
	"testing"
	"time"

	"github.com/jwwsjlm/douyinlive-proto/generated/new_douyin"
)

func TestManagerRecoveryHydratesSameCheckpointIdleClosure(t *testing.T) {
	fixture := newManagerFixture(t, nil)
	eventsRoot, err := resolveSessionEventsRoot(fixture.dataRoot, fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	initial := Checkpoint{
		SessionID: fixture.sessionID, State: CheckpointOpen,
		PrivacyKeyID: fixture.manager.privacy.KeyID(), UpdatedAt: fixture.now,
	}
	if err := fixture.writer.PersistBatch(context.Background(), Batch{
		SessionID: fixture.sessionID, Checkpoint: initial,
	}); err != nil {
		t.Fatal(err)
	}
	gift := &new_douyin.Webcast_Im_GiftMessage{
		Common: &new_douyin.Webcast_Im_Common{MsgId: 601},
		GiftId: 17, GroupId: 123, RepeatCount: 1,
		Gift: &new_douyin.Webcast_Data_GiftStruct{Name: "Moon", Combo: true},
	}
	envelope := IngestEnvelope{
		SessionID: fixture.sessionID, EventID: "018f0000-0000-7000-8000-000000000601",
		Sequence: 1, Method: methodGift, ReceivedAt: fixture.now,
		Payload: managerProtoPayload(t, gift),
	}
	spool, err := OpenSpool(DefaultSpoolOptions(eventsRoot))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spool.AppendBatch(context.Background(), []IngestEnvelope{envelope}); err != nil {
		t.Fatal(err)
	}
	if err := spool.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.RecoverSession(context.Background(), fixture.descriptor); err != nil {
		t.Fatal(err)
	}
	checkpoint, found, err := fixture.writer.Checkpoint(context.Background(), fixture.sessionID)
	if err != nil || !found || checkpoint.CommittedSequence != 1 {
		t.Fatalf("checkpoint = (%+v, %v, %v)", checkpoint, found, err)
	}
	normalizer, err := NewNormalizer(fixture.manager.privacy, DefaultNormalizerVersion)
	if err != nil {
		t.Fatal(err)
	}
	result := normalizer.NormalizeDetailed(envelope)
	aggregator := NewGiftComboAggregator(DefaultGiftComboIdle)
	if _, err := aggregator.Observe(result.Event, *result.Gift); err != nil {
		t.Fatal(err)
	}
	updates := aggregator.FlushIdle(fixture.now.Add(11 * time.Second))
	if len(updates) != 1 || updates[0].Aggregate == nil || updates[0].State.Status != ComboClosed {
		t.Fatalf("idle updates = %#v", updates)
	}
	samePosition := checkpoint
	samePosition.UpdatedAt = fixture.now.Add(11 * time.Second)
	if err := fixture.writer.PersistBatch(context.Background(), Batch{
		SessionID: fixture.sessionID, PreviousSequence: checkpoint.CommittedSequence,
		Events: []Event{*updates[0].Aggregate}, GiftCombos: []GiftComboState{updates[0].State},
		Checkpoint: samePosition,
	}); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewManager(context.Background(), ManagerOptions{
		DataRoot: fixture.dataRoot, Writer: fixture.writer, Credentials: fixture.credentials,
		BatchInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Shutdown(context.Background())
	sink, err := restarted.OpenSession(context.Background(), fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	gift.Common.MsgId = 602
	gift.RepeatCount = 2
	sink.Accept(managerLiveMessage(methodGift, managerProtoPayload(t, gift), fixture.now.Add(12*time.Second)))
	if err := sink.FlushAndClose(context.Background()); err != nil {
		t.Fatalf("close after hydrated restart = %v", err)
	}
	var status string
	var aggregateID string
	if err := fixture.store.Reader().QueryRow(`SELECT status, aggregate_event_id
		FROM gift_combo_states WHERE session_id = ?`, fixture.sessionID).Scan(
		&status, &aggregateID,
	); err != nil {
		t.Fatal(err)
	}
	if status != "closed" || aggregateID != updates[0].State.AggregateEventID {
		t.Fatalf("hydrated combo = status=%q aggregate=%q want=%q",
			status, aggregateID, updates[0].State.AggregateEventID)
	}
	var aggregates int
	if err := fixture.store.Reader().QueryRow(`SELECT COUNT(*) FROM live_events
		WHERE session_id = ? AND event_role = 'aggregate'`, fixture.sessionID).Scan(&aggregates); err != nil {
		t.Fatal(err)
	}
	if aggregates != 1 {
		t.Fatalf("aggregate count after hydrated restart = %d", aggregates)
	}
}
