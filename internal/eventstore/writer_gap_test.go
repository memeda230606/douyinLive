package eventstore

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWriterEventPersistenceGapMonotonicUpsert(t *testing.T) {
	fixture := newWriterFixture(t)
	ledger, err := OpenDropLedger(t.TempDir(), fixture.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := ledger.Merge(DropDelta{
		Count: 5, StartedAt: fixture.now, EndedAt: fixture.now.Add(time.Second),
		StartOffsetMS: 10, EndOffsetMS: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := fixture.checkpoint(0, CheckpointOpen)
	persist := func(gap CaptureGap) error {
		return fixture.writer.PersistBatch(context.Background(), Batch{
			SessionID: fixture.sessionID, Gaps: []CaptureGap{gap}, Checkpoint: checkpoint,
		})
	}
	if err := persist(first.Gap); err != nil {
		t.Fatal(err)
	}
	if err := persist(first.Gap); err != nil {
		t.Fatalf("equal replay: %v", err)
	}

	stale := first.Gap
	stale.DetailsJSON = "{\"count\":3}"
	staleEnd := fixture.now.Add(500 * time.Millisecond)
	stale.EndedAt = &staleEnd
	staleOffset := int64(15)
	stale.EndOffsetMS = &staleOffset
	if err := persist(stale); err != nil {
		t.Fatalf("dominated stale replay: %v", err)
	}

	newer, err := ledger.Merge(DropDelta{
		Count: 2, StartedAt: fixture.now.Add(2 * time.Second),
		EndedAt: fixture.now.Add(3 * time.Second), StartOffsetMS: 30, EndOffsetMS: 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := persist(newer.Gap); err != nil {
		t.Fatalf("monotonic update: %v", err)
	}

	regressiveEnd := newer.Gap
	regressiveEnd.DetailsJSON = "{\"count\":8}"
	badEnd := fixture.now.Add(2 * time.Second)
	regressiveEnd.EndedAt = &badEnd
	badOffset := int64(35)
	regressiveEnd.EndOffsetMS = &badOffset
	if err := persist(regressiveEnd); !errors.Is(err, ErrCheckpointConflict) {
		t.Fatalf("regressive update error = %v, want ErrCheckpointConflict", err)
	}
	var details string
	var endedAt, endOffset int64
	if err := fixture.store.Reader().QueryRow(`SELECT details_json, ended_at, end_offset_ms
		FROM capture_gaps WHERE session_id = ? AND dedupe_key = ?`,
		fixture.sessionID, newer.Gap.DedupeKey).Scan(&details, &endedAt, &endOffset); err != nil {
		t.Fatal(err)
	}
	if details != "{\"count\":7}" || endedAt != newer.Gap.EndedAt.UnixMilli() || endOffset != 40 {
		t.Fatalf("gap regressed: details=%s ended=%d offset=%d", details, endedAt, endOffset)
	}
}
