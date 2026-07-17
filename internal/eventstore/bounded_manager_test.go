package eventstore

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jwwsjlm/douyinlive-proto/generated/new_douyin"
)

func TestGiftComboAggregatorHardCapacityIncludesClosedTombstones(t *testing.T) {
	const capacity = 32
	aggregator := NewGiftComboAggregatorWithCapacity(time.Second, capacity)
	at := time.Unix(1_900_000_000, 0).UTC()
	for index := 0; index < capacity; index++ {
		observation := GiftObservation{
			SessionID: "session", SourceEventID: newEventID(),
			Sequence: int64(index + 1), ReceivedAt: at.Add(time.Duration(index) * time.Millisecond),
			GiftID: "gift", GroupID: uint64(index + 1), Count: 1,
			Combo: false, RepeatEnd: true, NormalizerVersion: "v1",
		}
		if _, err := aggregator.Observe(giftSource(int64(index+1), observation.ReceivedAt), observation); err != nil {
			t.Fatal(err)
		}
		if aggregator.EntryCount() > capacity {
			t.Fatalf("entry count = %d, capacity = %d", aggregator.EntryCount(), capacity)
		}
	}
	observation := GiftObservation{
		SessionID: "session", SourceEventID: newEventID(), Sequence: capacity + 1,
		ReceivedAt: at.Add(time.Second), GiftID: "gift", GroupID: 999,
		Count: 1, Combo: true, NormalizerVersion: "v1",
	}
	if _, err := aggregator.Observe(giftSource(capacity+1, observation.ReceivedAt), observation); err != nil {
		t.Fatalf("oldest tombstone should be evicted for a new open fold: %v", err)
	}
	if aggregator.EntryCount() > capacity {
		t.Fatalf("entry count after eviction = %d", aggregator.EntryCount())
	}
}

func TestManagerHighCardinalityClosedAndLateKeyStayBounded(t *testing.T) {
	fixture := newManagerFixture(t, func(options *ManagerOptions) {
		options.BatchSize = 8
		options.BatchInterval = 5 * time.Millisecond
	})
	sink, err := fixture.manager.OpenSession(context.Background(), fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	const unique = 160
	for index := 0; index < unique; index++ {
		gift := &new_douyin.Webcast_Im_GiftMessage{
			Common: &new_douyin.Webcast_Im_Common{MsgId: uint64(10_000 + index)},
			GiftId: 7, GroupId: uint64(index + 1), RepeatCount: 1, RepeatEnd: 1,
			Gift: &new_douyin.Webcast_Data_GiftStruct{Name: "Rose", Combo: true},
		}
		sink.Accept(managerLiveMessage(
			methodGift, managerProtoPayload(t, gift),
			fixture.now.Add(time.Duration(index)*time.Millisecond),
		))
	}
	late := &new_douyin.Webcast_Im_GiftMessage{
		Common: &new_douyin.Webcast_Im_Common{MsgId: 99_999},
		GiftId: 7, GroupId: 1, RepeatCount: 2,
		Gift: &new_douyin.Webcast_Data_GiftStruct{Name: "Rose", Combo: true},
	}
	sink.Accept(managerLiveMessage(
		methodGift, managerProtoPayload(t, late), fixture.now.Add(time.Second),
	))
	if err := sink.FlushAndClose(context.Background()); err != nil {
		t.Fatal(err)
	}
	var combos, aggregates, sources int
	if err := fixture.store.Reader().QueryRow(`SELECT COUNT(*) FROM gift_combo_states
		WHERE session_id = ?`, fixture.sessionID).Scan(&combos); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Reader().QueryRow(`SELECT COUNT(*) FROM live_events
		WHERE session_id = ? AND event_role = 'aggregate'`, fixture.sessionID).Scan(&aggregates); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Reader().QueryRow(`SELECT COUNT(*) FROM live_events
		WHERE session_id = ? AND event_role = 'source'`, fixture.sessionID).Scan(&sources); err != nil {
		t.Fatal(err)
	}
	if combos != unique || aggregates != unique || sources != unique+1 {
		t.Fatalf("counts combo/aggregate/source = %d/%d/%d", combos, aggregates, sources)
	}
}

func TestManagerLongDegradedUsesSpoolWithoutDeferredMapsAndRecovers(t *testing.T) {
	fixture := newManagerFixture(t, func(options *ManagerOptions) {
		options.BatchSize = 8
		options.BatchInterval = 5 * time.Millisecond
		options.BusyRetryWindow = 20 * time.Millisecond
		options.BusyRetryInitial = time.Millisecond
	})
	sink, err := fixture.manager.OpenSession(context.Background(), fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	runtimeType := reflect.TypeOf((*sessionRuntime)(nil)).Elem()
	for _, forbidden := range []string{"deferredAggregates", "deferredCombos", "deferredGaps", "gifts"} {
		if _, found := runtimeType.FieldByName(forbidden); found {
			t.Fatalf("runtime still contains unbounded field %q", forbidden)
		}
	}
	if sink.runtime.dedupe == nil || sink.runtime.dedupe.capacity != DefaultDedupeCapacity {
		t.Fatalf("runtime dedupe is not explicitly bounded: %+v", sink.runtime.dedupe)
	}
	if _, err := fixture.store.Writer().Exec(`CREATE TRIGGER block_event_persist
		BEFORE INSERT ON live_events BEGIN
			SELECT RAISE(ABORT, 'forced degraded');
		END`); err != nil {
		t.Fatal(err)
	}
	const gifts = 80
	for index := 0; index < gifts; index++ {
		gift := &new_douyin.Webcast_Im_GiftMessage{
			Common: &new_douyin.Webcast_Im_Common{MsgId: uint64(20_000 + index)},
			GiftId: uint64(index + 1), RepeatCount: 1,
			Gift: &new_douyin.Webcast_Data_GiftStruct{Name: "Bounded"},
		}
		sink.Accept(managerLiveMessage(
			methodGift, managerProtoPayload(t, gift),
			fixture.now.Add(time.Duration(index)*time.Millisecond),
		))
	}
	time.Sleep(200 * time.Millisecond)
	checkpoint, _, err := fixture.writer.Checkpoint(context.Background(), fixture.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.CommittedSequence != 0 {
		t.Fatalf("checkpoint advanced during failed atomic fold: %+v", checkpoint)
	}
	if _, err := fixture.store.Writer().Exec(`DROP TRIGGER block_event_persist`); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		checkpoint, found, err := fixture.writer.Checkpoint(context.Background(), fixture.sessionID)
		return err == nil && found && checkpoint.CommittedSequence == gifts
	})
	if err := sink.FlushAndClose(context.Background()); err != nil {
		t.Fatal(err)
	}
	var sources, aggregates, combos int
	if err := fixture.store.Reader().QueryRow(`SELECT COUNT(*) FROM live_events
		WHERE session_id = ? AND event_role = 'source'`, fixture.sessionID).Scan(&sources); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Reader().QueryRow(`SELECT COUNT(*) FROM live_events
		WHERE session_id = ? AND event_role = 'aggregate'`, fixture.sessionID).Scan(&aggregates); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Reader().QueryRow(`SELECT COUNT(*) FROM gift_combo_states
		WHERE session_id = ?`, fixture.sessionID).Scan(&combos); err != nil {
		t.Fatal(err)
	}
	if sources != gifts || aggregates != gifts || combos != gifts {
		t.Fatalf("recovered source/aggregate/combo = %d/%d/%d", sources, aggregates, combos)
	}
}

func TestManagerRecoveryCommitsPendingDropWithTailAtomically(t *testing.T) {
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
	ledger, err := OpenDropLedger(eventsRoot, fixture.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Merge(DropDelta{
		Count: 3, StartedAt: fixture.now.Add(time.Second),
		EndedAt:       fixture.now.Add(3 * time.Second),
		StartOffsetMS: 1000, EndOffsetMS: 3000,
	}); err != nil {
		t.Fatal(err)
	}
	spool, err := OpenSpool(DefaultSpoolOptions(eventsRoot))
	if err != nil {
		t.Fatal(err)
	}
	appends, err := spool.AppendBatch(context.Background(), []IngestEnvelope{{
		SessionID: fixture.sessionID,
		EventID:   "018f0000-0000-7000-8000-000000009001",
		Sequence:  1, Method: methodChat,
		PlatformRoomID:  fixture.descriptor.PlatformRoomID,
		ReceivedAt:      fixture.now.Add(4 * time.Second),
		SessionOffsetMS: 4000, Payload: []byte{1},
	}})
	if err != nil || len(appends) != 1 {
		t.Fatalf("AppendBatch() = (%d, %v)", len(appends), err)
	}
	if err := spool.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Writer().Exec(`CREATE TRIGGER block_gap_persist
		BEFORE INSERT ON capture_gaps BEGIN
			SELECT RAISE(ABORT, 'forced gap failure');
		END`); err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.RecoverSession(context.Background(), fixture.descriptor); !errors.Is(err, ErrPersistenceDegraded) {
		t.Fatalf("RecoverSession() error = %v", err)
	}
	checkpoint, found, err := fixture.writer.Checkpoint(context.Background(), fixture.sessionID)
	if err != nil || !found || checkpoint.CommittedSequence != 0 {
		t.Fatalf("checkpoint after rollback = (%+v, %v, %v)", checkpoint, found, err)
	}
	assertRowCount(t, fixture.store.Reader(), "live_events", 0)
	if _, err := fixture.store.Writer().Exec(`DROP TRIGGER block_gap_persist`); err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.RecoverSession(context.Background(), fixture.descriptor); err != nil {
		t.Fatalf("RecoverSession() retry error = %v", err)
	}
	checkpoint, found, err = fixture.writer.Checkpoint(context.Background(), fixture.sessionID)
	if err != nil || !found || checkpoint.CommittedSequence != 1 {
		t.Fatalf("checkpoint after recovery = (%+v, %v, %v)", checkpoint, found, err)
	}
	assertRowCount(t, fixture.store.Reader(), "live_events", 1)
	var details string
	if err := fixture.store.Reader().QueryRow(`SELECT details_json FROM capture_gaps
		WHERE session_id = ? AND kind = 'event_persistence'`, fixture.sessionID).Scan(&details); err != nil {
		t.Fatal(err)
	}
	if details != `{"count":3}` {
		t.Fatalf("drop details = %s", details)
	}
	reopened, err := OpenDropLedger(eventsRoot, fixture.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot, pending := reopened.Pending(); pending {
		t.Fatalf("ledger remains pending after commit: %+v", snapshot)
	}
}

func TestManagerCommittedDedupeWorksAcrossBatches(t *testing.T) {
	fixture := newManagerFixture(t, func(options *ManagerOptions) {
		options.BatchSize = 1
		options.BatchInterval = 5 * time.Millisecond
	})
	sink, err := fixture.manager.OpenSession(context.Background(), fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	gift := &new_douyin.Webcast_Im_GiftMessage{
		Common: &new_douyin.Webcast_Im_Common{MsgId: 31_001},
		GiftId: 88, RepeatCount: 1,
		Gift: &new_douyin.Webcast_Data_GiftStruct{Name: "Committed"},
	}
	payload := managerProtoPayload(t, gift)
	sink.Accept(managerLiveMessage(methodGift, payload, fixture.now.Add(time.Second)))
	eventually(t, func() bool {
		checkpoint, found, err := fixture.writer.Checkpoint(context.Background(), fixture.sessionID)
		return err == nil && found && checkpoint.CommittedSequence == 1
	})
	if sink.runtime.dedupe.Len() != 1 {
		t.Fatalf("dedupe len after committed first batch = %d", sink.runtime.dedupe.Len())
	}
	sink.Accept(managerLiveMessage(methodGift, payload, fixture.now.Add(2*time.Second)))
	eventually(t, func() bool {
		checkpoint, found, err := fixture.writer.Checkpoint(context.Background(), fixture.sessionID)
		return err == nil && found && checkpoint.CommittedSequence == 2
	})
	if sink.runtime.dedupe.Len() != 1 {
		t.Fatalf("duplicate extended cache cardinality = %d", sink.runtime.dedupe.Len())
	}
	if err := sink.FlushAndClose(context.Background()); err != nil {
		t.Fatal(err)
	}
	var sources, aggregates, combos int
	if err := fixture.store.Reader().QueryRow(`SELECT COUNT(*) FROM live_events
		WHERE session_id = ? AND event_role = 'source'`, fixture.sessionID).Scan(&sources); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Reader().QueryRow(`SELECT COUNT(*) FROM live_events
		WHERE session_id = ? AND event_role = 'aggregate'`, fixture.sessionID).Scan(&aggregates); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Reader().QueryRow(`SELECT COUNT(*) FROM gift_combo_states
		WHERE session_id = ?`, fixture.sessionID).Scan(&combos); err != nil {
		t.Fatal(err)
	}
	if sources != 1 || aggregates != 1 || combos != 1 {
		t.Fatalf("source/aggregate/combo = %d/%d/%d", sources, aggregates, combos)
	}
}

func TestManagerCachedGiftStillClosesTouchedIdleCombo(t *testing.T) {
	fixture := newManagerFixture(t, func(options *ManagerOptions) {
		options.BatchSize = 1
		options.BatchInterval = time.Hour
	})
	sink, err := fixture.manager.OpenSession(context.Background(), fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	gift := &new_douyin.Webcast_Im_GiftMessage{
		Common: &new_douyin.Webcast_Im_Common{MsgId: 32_001},
		GiftId: 89, GroupId: 777, RepeatCount: 1,
		Gift: &new_douyin.Webcast_Data_GiftStruct{Name: "Idle", Combo: true},
	}
	payload := managerProtoPayload(t, gift)
	sink.Accept(managerLiveMessage(methodGift, payload, fixture.now))
	eventually(t, func() bool {
		var status string
		err := fixture.store.Reader().QueryRow(`SELECT status FROM gift_combo_states
			WHERE session_id = ?`, fixture.sessionID).Scan(&status)
		return err == nil && status == "open"
	})
	if sink.runtime.dedupe.Len() != 1 {
		t.Fatalf("dedupe len after open combo = %d", sink.runtime.dedupe.Len())
	}
	sink.Accept(managerLiveMessage(
		methodGift, payload, fixture.now.Add(DefaultGiftComboIdle+time.Second),
	))
	eventually(t, func() bool {
		var status string
		err := fixture.store.Reader().QueryRow(`SELECT status FROM gift_combo_states
			WHERE session_id = ?`, fixture.sessionID).Scan(&status)
		return err == nil && status == "closed"
	})
	if err := sink.FlushAndClose(context.Background()); err != nil {
		t.Fatal(err)
	}
	var sources, aggregates int
	if err := fixture.store.Reader().QueryRow(`SELECT COUNT(*) FROM live_events
		WHERE session_id = ? AND event_role = 'source'`, fixture.sessionID).Scan(&sources); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Reader().QueryRow(`SELECT COUNT(*) FROM live_events
		WHERE session_id = ? AND event_role = 'aggregate'`, fixture.sessionID).Scan(&aggregates); err != nil {
		t.Fatal(err)
	}
	if sources != 1 || aggregates != 1 {
		t.Fatalf("source/aggregate = %d/%d", sources, aggregates)
	}
}
