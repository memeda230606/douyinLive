package eventstore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDropLedgerMonotonicMergeAndOldAcknowledge(t *testing.T) {
	root := t.TempDir()
	ledger, err := OpenDropLedger(root, "drop-session")
	if err != nil {
		t.Fatal(err)
	}
	if _, pending := ledger.Pending(); pending {
		t.Fatal("new ledger unexpectedly pending")
	}
	started := time.Date(2026, 7, 17, 12, 0, 0, 123456789, time.UTC)
	first, err := ledger.Merge(DropDelta{
		Count: 2, StartedAt: started, EndedAt: started.Add(time.Second),
		StartOffsetMS: 10, EndOffsetMS: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := ledger.Merge(DropDelta{
		Count: 3, StartedAt: started.Add(-time.Second), EndedAt: started,
		StartOffsetMS: 1, EndOffsetMS: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.TotalCount != 5 || second.Gap.ID != first.Gap.ID ||
		second.Gap.DedupeKey != first.Gap.DedupeKey ||
		!second.Gap.StartedAt.Equal(first.Gap.StartedAt) ||
		!second.Gap.EndedAt.Equal(*first.Gap.EndedAt) ||
		*second.Gap.EndOffsetMS != *first.Gap.EndOffsetMS {
		t.Fatalf("second snapshot is not monotonic: first=%+v second=%+v", first, second)
	}
	var details map[string]int64
	if err := json.Unmarshal([]byte(second.Gap.DetailsJSON), &details); err != nil {
		t.Fatal(err)
	}
	if len(details) != 1 || details["count"] != 5 {
		t.Fatalf("details = %#v", details)
	}
	if err := ledger.Acknowledge(first); err != nil {
		t.Fatal(err)
	}
	pending, ok := ledger.Pending()
	if !ok || pending.TotalCount != 5 {
		t.Fatalf("old ack covered newer merge: (%+v, %v)", pending, ok)
	}

	reopened, err := OpenDropLedger(root, "drop-session")
	if err != nil {
		t.Fatal(err)
	}
	pending, ok = reopened.Pending()
	if !ok || pending.TotalCount != 5 {
		t.Fatalf("reopened pending = (%+v, %v)", pending, ok)
	}
	if err := reopened.Acknowledge(pending); err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Pending(); ok {
		t.Fatal("fully acknowledged ledger remained pending")
	}
	third, err := reopened.Merge(DropDelta{
		Count: 1, StartedAt: started.Add(3 * time.Second), EndedAt: started.Add(4 * time.Second),
		StartOffsetMS: 30, EndOffsetMS: 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	if third.TotalCount != 6 || third.Gap.ID != first.Gap.ID ||
		third.Gap.DedupeKey != first.Gap.DedupeKey || *third.Gap.EndOffsetMS != 40 {
		t.Fatalf("third snapshot = %+v", third)
	}
	if _, err := os.Stat(filepath.Join(root, dropLedgerFilename+".tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file remains: %v", err)
	}
}

func TestDropLedgerCommitBeforeAcknowledgeReplaysAndMerges(t *testing.T) {
	fixture := newWriterFixture(t)
	root := t.TempDir()
	ledger, err := OpenDropLedger(root, fixture.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := ledger.Merge(DropDelta{
		Count: 4, StartedAt: fixture.now, EndedAt: fixture.now.Add(time.Second),
		StartOffsetMS: 100, EndOffsetMS: 200,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := fixture.checkpoint(0, CheckpointOpen)
	persist := func(snapshot DropSnapshot) error {
		return fixture.writer.PersistBatch(context.Background(), Batch{
			SessionID: fixture.sessionID, Gaps: []CaptureGap{snapshot.Gap},
			Checkpoint: checkpoint,
		})
	}
	if err := persist(first); err != nil {
		t.Fatalf("first persist: %v", err)
	}

	// Simulate a process crash after SQLite commit but before sidecar ack.
	restarted, err := OpenDropLedger(root, fixture.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	replay, ok := restarted.Pending()
	if !ok || replay.TotalCount != first.TotalCount {
		t.Fatalf("restart pending = (%+v, %v)", replay, ok)
	}
	if err := persist(replay); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}

	newer, err := restarted.Merge(DropDelta{
		Count: 3, StartedAt: fixture.now.Add(2 * time.Second),
		EndedAt: fixture.now.Add(3 * time.Second), StartOffsetMS: 300, EndOffsetMS: 400,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Acknowledge(replay); err != nil {
		t.Fatal(err)
	}
	if pending, ok := restarted.Pending(); !ok || pending.TotalCount != 7 {
		t.Fatalf("new drops were acknowledged by old snapshot: (%+v, %v)", pending, ok)
	}
	if err := persist(newer); err != nil {
		t.Fatalf("persist merged total: %v", err)
	}
	if err := restarted.Acknowledge(newer); err != nil {
		t.Fatal(err)
	}
	if _, ok := restarted.Pending(); ok {
		t.Fatal("merged snapshot remained pending")
	}
	var details string
	var endedAt, endOffset int64
	if err := fixture.store.Reader().QueryRow(`SELECT details_json, ended_at, end_offset_ms
		FROM capture_gaps WHERE session_id = ? AND dedupe_key = ?`,
		fixture.sessionID, newer.Gap.DedupeKey).Scan(&details, &endedAt, &endOffset); err != nil {
		t.Fatal(err)
	}
	if details != "{\"count\":7}" || endedAt != newer.Gap.EndedAt.UnixMilli() || endOffset != 400 {
		t.Fatalf("persisted cumulative gap = details=%s ended=%d offset=%d", details, endedAt, endOffset)
	}
}

func TestDropLedgerRejectsCorruptAndInvalidState(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, dropLedgerFilename), []byte("{\"version\":1}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDropLedger(root, "drop-session"); !errors.Is(err, ErrDropLedgerCorrupt) {
		t.Fatalf("corrupt open error = %v", err)
	}
	if _, err := OpenDropLedger(root, ""); !errors.Is(err, ErrDropLedgerInvalid) {
		t.Fatalf("invalid session error = %v", err)
	}
}
