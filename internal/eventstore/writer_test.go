package eventstore

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jwwsjlm/douyinLive/v2/internal/storage"
)

const testPrivacyKeyID = "0123456789abcdef"

type writerFixture struct {
	store     *storage.Store
	writer    *Writer
	sessionID string
	now       time.Time
}

func newWriterFixture(t *testing.T) writerFixture {
	t.Helper()
	ctx := context.Background()
	layout, err := storage.PrepareLayout(t.TempDir())
	if err != nil {
		t.Fatalf("PrepareLayout() error = %v", err)
	}
	store, err := storage.Open(ctx, layout, storage.OpenOptions{})
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	if _, err := store.Writer().Exec(`INSERT INTO rooms(
		id, live_id, alias, created_at, updated_at
	) VALUES ('writer-room', 'writer-live', 'writer', 1, 1)`); err != nil {
		t.Fatalf("insert room: %v", err)
	}
	if _, err := store.Writer().Exec(`INSERT INTO live_sessions(
		id, room_config_id, status, started_at, clock_source, data_path,
		schema_version, created_at, updated_at
	) VALUES ('writer-session', 'writer-room', 'recording', 1, 'received',
		'rooms/writer', 1, 1, 1)`); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	writer, err := NewWriter(store.Writer())
	if err != nil {
		t.Fatalf("NewWriter() error = %v", err)
	}
	return writerFixture{
		store: store, writer: writer, sessionID: "writer-session",
		now: time.Date(2026, 7, 17, 11, 0, 0, 0, time.UTC),
	}
}

func TestWriterPersistsBatchAndCheckpointAtomically(t *testing.T) {
	fixture := newWriterFixture(t)
	event := fixture.sourceEvent(1, "event-1", "dedupe-1")
	combo := GiftComboState{
		SessionID: fixture.sessionID, ComboKey: "combo-1", Status: ComboOpen,
		UserHash: "user-hash", GiftID: "gift-1", GiftName: "rose", TotalCount: 1,
		FirstSequence: 1, LastSequence: 1, StartedAt: fixture.now,
		UpdatedAt: fixture.now, NormalizerVersion: "normalizer-v1",
	}
	gap := CaptureGap{
		ID: "gap-1", SessionID: fixture.sessionID, Kind: "event_persistence",
		StartedAt: fixture.now, StartOffsetMS: 0, Severity: "warning",
		ReasonCode: "PERSISTENCE_BUSY", DetailsJSON: `{}`,
		DedupeKey: "gap-dedupe-1",
	}
	batch := Batch{
		SessionID: fixture.sessionID, PreviousSequence: 0,
		Events: []Event{event}, GiftCombos: []GiftComboState{combo},
		Gaps: []CaptureGap{gap}, Checkpoint: fixture.checkpoint(1, CheckpointOpen),
	}
	if err := fixture.writer.PersistBatch(context.Background(), batch); err != nil {
		t.Fatalf("PersistBatch() error = %v", err)
	}

	assertRowCount(t, fixture.store.Reader(), "live_events", 1)
	assertRowCount(t, fixture.store.Reader(), "gift_combo_states", 1)
	assertRowCount(t, fixture.store.Reader(), "capture_gaps", 1)
	checkpoint, found, err := fixture.writer.Checkpoint(context.Background(), fixture.sessionID)
	if err != nil || !found {
		t.Fatalf("Checkpoint() = (%+v, %v, %v)", checkpoint, found, err)
	}
	if !sameCheckpointPosition(checkpoint, batch.Checkpoint) || checkpoint.UpdatedAt != batch.Checkpoint.UpdatedAt {
		t.Fatalf("Checkpoint() = %+v, want %+v", checkpoint, batch.Checkpoint)
	}
}

func TestWriterDuplicateDedupeAdvancesCheckpointButTrueConstraintRollsBack(t *testing.T) {
	t.Run("targeted dedupe", func(t *testing.T) {
		fixture := newWriterFixture(t)
		first := Batch{
			SessionID:  fixture.sessionID,
			Events:     []Event{fixture.sourceEvent(1, "event-1", "same-dedupe")},
			Checkpoint: fixture.checkpoint(1, CheckpointOpen),
		}
		if err := fixture.writer.PersistBatch(context.Background(), first); err != nil {
			t.Fatalf("persist first batch: %v", err)
		}
		second := Batch{
			SessionID: fixture.sessionID, PreviousSequence: 1,
			Events:     []Event{fixture.sourceEvent(2, "event-2", "same-dedupe")},
			Checkpoint: fixture.checkpoint(2, CheckpointOpen),
		}
		if err := fixture.writer.PersistBatch(context.Background(), second); err != nil {
			t.Fatalf("persist duplicate batch: %v", err)
		}
		assertRowCount(t, fixture.store.Reader(), "live_events", 1)
		checkpoint, found, err := fixture.writer.Checkpoint(context.Background(), fixture.sessionID)
		if err != nil || !found || checkpoint.CommittedSequence != 2 {
			t.Fatalf("Checkpoint() = (%+v, %v, %v), want sequence 2", checkpoint, found, err)
		}
	})

	t.Run("unrelated primary key", func(t *testing.T) {
		fixture := newWriterFixture(t)
		first := Batch{
			SessionID:  fixture.sessionID,
			Events:     []Event{fixture.sourceEvent(1, "shared-id", "dedupe-1")},
			Checkpoint: fixture.checkpoint(1, CheckpointOpen),
		}
		if err := fixture.writer.PersistBatch(context.Background(), first); err != nil {
			t.Fatalf("persist first batch: %v", err)
		}
		second := Batch{
			SessionID: fixture.sessionID, PreviousSequence: 1,
			Events: []Event{fixture.sourceEvent(2, "shared-id", "dedupe-2")},
			Gaps: []CaptureGap{{
				ID: "must-rollback", SessionID: fixture.sessionID, Kind: "event_persistence",
				StartedAt: fixture.now, Severity: "error", ReasonCode: "COMMIT_FAILED",
				DedupeKey: "must-rollback", DetailsJSON: `{}`,
			}},
			Checkpoint: fixture.checkpoint(2, CheckpointOpen),
		}
		err := fixture.writer.PersistBatch(context.Background(), second)
		if !errors.Is(err, ErrPersistenceConstraint) {
			t.Fatalf("PersistBatch() error = %v, want ErrPersistenceConstraint", err)
		}
		if err.Error() != ErrPersistenceConstraint.Error() {
			t.Fatalf("constraint error leaked database detail: %q", err)
		}
		assertRowCount(t, fixture.store.Reader(), "live_events", 1)
		assertRowCount(t, fixture.store.Reader(), "capture_gaps", 0)
		checkpoint, _, err := fixture.writer.Checkpoint(context.Background(), fixture.sessionID)
		if err != nil || checkpoint.CommittedSequence != 1 {
			t.Fatalf("checkpoint after rollback = (%+v, %v)", checkpoint, err)
		}
	})

	t.Run("source sequence collision", func(t *testing.T) {
		fixture := newWriterFixture(t)
		first := Batch{
			SessionID:  fixture.sessionID,
			Events:     []Event{fixture.sourceEvent(1, "event-1", "dedupe-1")},
			Checkpoint: fixture.checkpoint(1, CheckpointOpen),
		}
		if err := fixture.writer.PersistBatch(context.Background(), first); err != nil {
			t.Fatalf("persist first batch: %v", err)
		}
		collision := first
		collision.Events = []Event{fixture.sourceEvent(1, "event-other", "dedupe-other")}
		err := fixture.writer.PersistBatch(context.Background(), collision)
		if !errors.Is(err, ErrPersistenceConstraint) {
			t.Fatalf("source sequence collision error = %v, want ErrPersistenceConstraint", err)
		}
		assertRowCount(t, fixture.store.Reader(), "live_events", 1)
	})
}

func TestWriterRollsBackAllRowsWhenGapForeignKeyFails(t *testing.T) {
	fixture := newWriterFixture(t)
	batch := Batch{
		SessionID: fixture.sessionID,
		Events:    []Event{fixture.sourceEvent(1, "event-1", "dedupe-1")},
		Gaps: []CaptureGap{{
			ID: "gap-1", SessionID: fixture.sessionID, MediaSegmentID: "missing-segment",
			Kind: "event_persistence", StartedAt: fixture.now, Severity: "error",
			ReasonCode: "COMMIT_FAILED", DedupeKey: "gap-1", DetailsJSON: `{}`,
		}},
		Checkpoint: fixture.checkpoint(1, CheckpointOpen),
	}
	err := fixture.writer.PersistBatch(context.Background(), batch)
	if !errors.Is(err, ErrPersistenceConstraint) {
		t.Fatalf("PersistBatch() error = %v, want ErrPersistenceConstraint", err)
	}
	assertRowCount(t, fixture.store.Reader(), "live_events", 0)
	assertRowCount(t, fixture.store.Reader(), "capture_gaps", 0)
	if checkpoint, found, err := fixture.writer.Checkpoint(context.Background(), fixture.sessionID); err != nil || found {
		t.Fatalf("Checkpoint() after rollback = (%+v, %v, %v)", checkpoint, found, err)
	}
}

func TestWriterClosesCrossBatchGiftComboWithLateAggregate(t *testing.T) {
	fixture := newWriterFixture(t)
	open := GiftComboState{
		SessionID: fixture.sessionID, ComboKey: "combo-1", Status: ComboOpen,
		UserHash: "user-hash", GiftID: "gift-1", GiftName: "rose", TotalCount: 1,
		FirstSequence: 1, LastSequence: 1, StartedAt: fixture.now,
		UpdatedAt: fixture.now, NormalizerVersion: "normalizer-v1",
	}
	first := Batch{
		SessionID:  fixture.sessionID,
		Events:     []Event{fixture.sourceEvent(1, "source-1", "source-1")},
		GiftCombos: []GiftComboState{open},
		Checkpoint: fixture.checkpoint(1, CheckpointOpen),
	}
	if err := fixture.writer.PersistBatch(context.Background(), first); err != nil {
		t.Fatalf("persist open combo: %v", err)
	}

	closedAt := fixture.now.Add(time.Second)
	aggregate := fixture.aggregateEvent(1, "aggregate-1", "aggregate-1")
	closed := open
	closed.Status = ComboClosed
	closed.TotalCount = 2
	closed.UpdatedAt = closedAt
	closed.ClosedAt = &closedAt
	closed.AggregateEventID = aggregate.ID
	second := Batch{
		SessionID: fixture.sessionID, PreviousSequence: 1,
		Events: []Event{aggregate}, GiftCombos: []GiftComboState{closed},
		Checkpoint: fixture.checkpoint(1, CheckpointOpen),
	}
	second.Checkpoint.UpdatedAt = closedAt
	if err := fixture.writer.PersistBatch(context.Background(), second); err != nil {
		t.Fatalf("persist late aggregate close: %v", err)
	}
	if err := fixture.writer.PersistBatch(context.Background(), second); err != nil {
		t.Fatalf("replay late aggregate close: %v", err)
	}
	assertRowCount(t, fixture.store.Reader(), "live_events", 2)
	var status, aggregateID string
	var totalCount int64
	if err := fixture.store.Reader().QueryRow(`SELECT status, total_count, aggregate_event_id
		FROM gift_combo_states WHERE session_id = ? AND combo_key = 'combo-1'`, fixture.sessionID).Scan(
		&status, &totalCount, &aggregateID,
	); err != nil {
		t.Fatalf("read closed combo: %v", err)
	}
	if status != "closed" || totalCount != 2 || aggregateID != aggregate.ID {
		t.Fatalf("closed combo = (%q, %d, %q)", status, totalCount, aggregateID)
	}
	checkpoint, _, err := fixture.writer.Checkpoint(context.Background(), fixture.sessionID)
	if err != nil || checkpoint.CommittedSequence != 1 {
		t.Fatalf("checkpoint after late aggregate = (%+v, %v)", checkpoint, err)
	}
}

func TestWriterCheckpointCannotAdvanceAheadOrDriftPrivacyKey(t *testing.T) {
	fixture := newWriterFixture(t)
	ahead := Batch{
		SessionID:  fixture.sessionID,
		Events:     []Event{fixture.sourceEvent(1, "event-1", "event-1")},
		Checkpoint: fixture.checkpoint(2, CheckpointOpen),
	}
	if err := fixture.writer.PersistBatch(context.Background(), ahead); !errors.Is(err, ErrInvalidBatch) {
		t.Fatalf("ahead checkpoint error = %v, want ErrInvalidBatch", err)
	}
	assertRowCount(t, fixture.store.Reader(), "live_events", 0)

	first := Batch{
		SessionID:  fixture.sessionID,
		Events:     []Event{fixture.sourceEvent(1, "event-1", "event-1")},
		Checkpoint: fixture.checkpoint(1, CheckpointOpen),
	}
	if err := fixture.writer.PersistBatch(context.Background(), first); err != nil {
		t.Fatalf("persist first batch: %v", err)
	}
	closing := Batch{
		SessionID: fixture.sessionID, PreviousSequence: 1,
		Checkpoint: fixture.checkpoint(1, CheckpointClosing),
	}
	if err := fixture.writer.PersistBatch(context.Background(), closing); err != nil {
		t.Fatalf("transition checkpoint to closing: %v", err)
	}
	drift := closing
	drift.Checkpoint.State = CheckpointClosed
	drift.Checkpoint.PrivacyKeyID = "fedcba9876543210"
	if err := fixture.writer.PersistBatch(context.Background(), drift); !errors.Is(err, ErrCheckpointConflict) {
		t.Fatalf("privacy key drift error = %v, want ErrCheckpointConflict", err)
	}
	checkpoint, _, err := fixture.writer.Checkpoint(context.Background(), fixture.sessionID)
	if err != nil || checkpoint.State != CheckpointClosing || checkpoint.PrivacyKeyID != testPrivacyKeyID {
		t.Fatalf("checkpoint after key drift = (%+v, %v)", checkpoint, err)
	}
	closed := closing
	closed.Checkpoint.State = CheckpointClosed
	if err := fixture.writer.PersistBatch(context.Background(), closed); err != nil {
		t.Fatalf("transition checkpoint to closed: %v", err)
	}
	late := Batch{
		SessionID: fixture.sessionID, PreviousSequence: 1,
		Events:     []Event{fixture.aggregateEvent(1, "late-after-close", "late-after-close")},
		Checkpoint: closed.Checkpoint,
	}
	if err := fixture.writer.PersistBatch(context.Background(), late); !errors.Is(err, ErrCheckpointConflict) {
		t.Fatalf("late event after close error = %v, want ErrCheckpointConflict", err)
	}
	advance := Batch{
		SessionID: fixture.sessionID, PreviousSequence: 1,
		Events:     []Event{fixture.sourceEvent(2, "source-after-close", "source-after-close")},
		Checkpoint: fixture.checkpoint(2, CheckpointClosed),
	}
	if err := fixture.writer.PersistBatch(context.Background(), advance); !errors.Is(err, ErrCheckpointConflict) {
		t.Fatalf("checkpoint advance after close error = %v, want ErrCheckpointConflict", err)
	}
	assertRowCount(t, fixture.store.Reader(), "live_events", 1)
}

func TestWriterRejectsSensitiveOrAbsolutePathsWithoutEcho(t *testing.T) {
	fixture := newWriterFixture(t)
	batch := Batch{
		SessionID:  fixture.sessionID,
		Events:     []Event{fixture.sourceEvent(1, "event-1", "event-1")},
		Checkpoint: fixture.checkpoint(1, CheckpointOpen),
	}
	secretPath := `C:/private/operator/events.wal`
	batch.Checkpoint.Spool.File = secretPath
	err := fixture.writer.PersistBatch(context.Background(), batch)
	if !errors.Is(err, ErrInvalidBatch) {
		t.Fatalf("PersistBatch() error = %v, want ErrInvalidBatch", err)
	}
	if strings.Contains(err.Error(), secretPath) || err.Error() != ErrInvalidBatch.Error() {
		t.Fatalf("invalid batch error leaked path: %q", err)
	}
}

func TestWriterReplayCannotBackfillMissingSourceBehindCheckpoint(t *testing.T) {
	fixture := newWriterFixture(t)
	first := Batch{
		SessionID:  fixture.sessionID,
		Events:     []Event{fixture.sourceEvent(1, "source-1", "same-dedupe")},
		Checkpoint: fixture.checkpoint(1, CheckpointOpen),
	}
	if err := fixture.writer.PersistBatch(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	duplicateAdvance := Batch{
		SessionID: fixture.sessionID, PreviousSequence: 1,
		Events:     []Event{fixture.sourceEvent(2, "source-2", "same-dedupe")},
		Checkpoint: fixture.checkpoint(2, CheckpointOpen),
	}
	if err := fixture.writer.PersistBatch(context.Background(), duplicateAdvance); err != nil {
		t.Fatal(err)
	}
	assertRowCount(t, fixture.store.Reader(), "live_events", 1)

	backfill := duplicateAdvance
	backfill.Events = []Event{fixture.sourceEvent(2, "source-backfill", "new-dedupe")}
	if err := fixture.writer.PersistBatch(context.Background(), backfill); !errors.Is(err, ErrCheckpointConflict) {
		t.Fatalf("same-checkpoint source backfill error = %v", err)
	}
	assertRowCount(t, fixture.store.Reader(), "live_events", 1)
	checkpoint, found, err := fixture.writer.Checkpoint(context.Background(), fixture.sessionID)
	if err != nil || !found || !sameCheckpointPosition(checkpoint, duplicateAdvance.Checkpoint) {
		t.Fatalf("checkpoint after rejected backfill = (%+v, %v, %v)", checkpoint, found, err)
	}

	aggregate := fixture.aggregateEvent(2, "late-aggregate", "late-aggregate")
	lateAggregate := duplicateAdvance
	lateAggregate.PreviousSequence = 2
	lateAggregate.Events = []Event{aggregate}
	if err := fixture.writer.PersistBatch(context.Background(), lateAggregate); err != nil {
		t.Fatalf("same-checkpoint aggregate should remain allowed: %v", err)
	}
	assertRowCount(t, fixture.store.Reader(), "live_events", 2)
}

func (fixture writerFixture) sourceEvent(sequence int64, id, dedupe string) Event {
	return Event{
		ID: id, SessionID: fixture.sessionID, IngestSequence: sequence,
		Role: EventRoleSource, Method: "WebcastChatMessage", Kind: EventChat,
		DedupeKey: dedupe, ReceivedAt: fixture.now.Add(time.Duration(sequence) * time.Millisecond),
		SessionOffsetMS: sequence, ClockConfidence: 1, NormalizedJSON: `{}`,
		Raw:         RawRef{File: "events/raw.bin", Offset: sequence * 10, Length: RawFrameHeaderSize, CRC32C: uint32(sequence)},
		ParseStatus: ParseParsed, NormalizerVersion: "normalizer-v1",
	}
}

func (fixture writerFixture) aggregateEvent(sequence int64, id, dedupe string) Event {
	event := fixture.sourceEvent(sequence, id, dedupe)
	event.Role = EventRoleAggregate
	event.Method = "GiftComboAggregate"
	event.Kind = EventGift
	event.Raw = RawRef{}
	return event
}

func (fixture writerFixture) checkpoint(sequence int64, state CheckpointState) Checkpoint {
	checkpoint := Checkpoint{
		SessionID: fixture.sessionID, CommittedSequence: sequence,
		State: state, PrivacyKeyID: testPrivacyKeyID,
		UpdatedAt: fixture.now.Add(time.Duration(sequence) * time.Millisecond),
	}
	if sequence > 0 {
		checkpoint.Spool = SpoolPosition{File: "events/ingest.wal", Offset: sequence * 100}
		checkpoint.Raw = SpoolPosition{File: "events/raw.bin", Offset: sequence * 10}
	}
	return checkpoint
}

func assertRowCount(t *testing.T, db interface {
	QueryRow(string, ...any) *sql.Row
}, table string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&got); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if got != want {
		t.Fatalf("%s row count = %d, want %d", table, got, want)
	}
}
