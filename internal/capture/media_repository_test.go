package capture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLiteRepositoryMediaLifecycleUsesCASAndIndependentDirtyState(t *testing.T) {
	ctx := context.Background()
	repo, store, layout, roomID, now := openRepository(t)
	defer store.Close()

	liveSession, err := repo.Create(ctx, CreateSessionInput{
		RoomConfigID: roomID,
		OperationID:  newV7(t),
		StartedAt:    now,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	mediaPath := liveSession.DataPath + "/media"
	opened, err := repo.OpenSessionMedia(ctx, OpenSessionMediaInput{
		SessionID: liveSession.ID, RelativePath: mediaPath, StartedAt: now.UnixMilli(),
	})
	if err != nil {
		t.Fatalf("OpenSessionMedia() error = %v", err)
	}
	if opened.Session.SessionID != liveSession.ID || opened.Session.RootID != nil ||
		opened.Session.RelativePath != mediaPath || opened.Session.State != SessionMediaOpen ||
		opened.Session.ManifestRevision != 0 || !opened.Session.ManifestDirty {
		t.Fatalf("initial media snapshot = %+v", opened)
	}
	if len(opened.Session.Attempts) != 0 || len(opened.Segments) != 0 || len(opened.Artifacts) != 0 {
		t.Fatalf("initial media snapshot is not empty: %+v", opened)
	}
	replayed, err := repo.OpenSessionMedia(ctx, OpenSessionMediaInput{
		SessionID: liveSession.ID, RelativePath: mediaPath, StartedAt: now.AddDate(0, 0, 1).UnixMilli(),
	})
	if err != nil || replayed.Session.ManifestRevision != 0 {
		t.Fatalf("idempotent OpenSessionMedia() = (%+v, %v)", replayed, err)
	}
	if _, err := repo.OpenSessionMedia(ctx, OpenSessionMediaInput{
		SessionID: liveSession.ID, RelativePath: mediaPath + "-other", StartedAt: now.UnixMilli(),
	}); !errors.Is(err, ErrSessionMediaConflict) {
		t.Fatalf("conflicting OpenSessionMedia() error = %v, want ErrSessionMediaConflict", err)
	}

	firstAttempt := testMediaAttempt(t, 1, now.UnixMilli())
	secondAttempt := testMediaAttempt(t, 2, now.UnixMilli()+1_000)
	segment := testMediaSegment(t, mediaPath, firstAttempt, 1, now.UnixMilli())
	artifact := testMediaArtifact(t, mediaPath, segment, now.UnixMilli())
	mediaEpoch := now.UnixMilli() + 2_000
	persisted, err := repo.PersistMediaSnapshot(ctx, PersistMediaSnapshotInput{
		SessionID: liveSession.ID, ExpectedRevision: 0, State: SessionMediaFinalizing,
		MediaEpochAt: &mediaEpoch, Attempts: []MediaAttempt{firstAttempt, secondAttempt},
		Segments: []MediaSegment{segment}, Artifacts: []MediaArtifact{artifact},
		UpdatedAt: now.UnixMilli() + 3_000,
	})
	if err != nil {
		t.Fatalf("PersistMediaSnapshot() error = %v", err)
	}
	if persisted.Session.ManifestRevision != 1 || !persisted.Session.ManifestDirty ||
		persisted.Session.State != SessionMediaFinalizing || persisted.Session.MediaEpochAt == nil ||
		*persisted.Session.MediaEpochAt != mediaEpoch {
		t.Fatalf("persisted media session = %+v", persisted.Session)
	}
	if len(persisted.Session.Attempts) != 2 || persisted.Session.Attempts[0].Ordinal != 1 ||
		persisted.Session.Attempts[1].Ordinal != 2 {
		t.Fatalf("attempt ordering = %+v", persisted.Session.Attempts)
	}
	if len(persisted.Segments) != 1 || persisted.Segments[0].ID != segment.ID {
		t.Fatalf("segments = %+v", persisted.Segments)
	}
	if len(persisted.Artifacts) != 1 || persisted.Artifacts[0].ID != artifact.ID ||
		persisted.Artifacts[0].Status != MediaArtifactPendingTranscode {
		t.Fatalf("artifacts = %+v", persisted.Artifacts)
	}

	var storedEpoch int64
	var storedClock ClockSource
	if err := store.Reader().QueryRow(`SELECT media_epoch_at, clock_source FROM live_sessions WHERE id = ?`,
		liveSession.ID).Scan(&storedEpoch, &storedClock); err != nil {
		t.Fatalf("read live session media clock: %v", err)
	}
	if storedEpoch != mediaEpoch || storedClock != ClockMedia {
		t.Fatalf("live session media clock = (%d, %q), want (%d, %q)", storedEpoch, storedClock, mediaEpoch, ClockMedia)
	}
	manifestPath := filepath.Join(layout.Root, filepath.FromSlash(liveSession.DataPath), "session.json")
	payload, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("ReadFile(session.json) error = %v", err)
	}
	var manifest LiveSession
	if err := json.Unmarshal(payload, &manifest); err != nil {
		t.Fatalf("decode session.json: %v", err)
	}
	if manifest.MediaEpochAt == nil || *manifest.MediaEpochAt != mediaEpoch || manifest.ClockSource != ClockMedia {
		t.Fatalf("session.json media clock = %+v", manifest)
	}

	if cleared, err := repo.ClearDirty(ctx, liveSession.ID, 0); err != nil || cleared {
		t.Fatalf("ClearDirty(stale) = (%v, %v), want (false, nil)", cleared, err)
	}
	if cleared, err := repo.ClearDirty(ctx, liveSession.ID, 1); err != nil || !cleared {
		t.Fatalf("ClearDirty(current) = (%v, %v), want (true, nil)", cleared, err)
	}
	if cleared, err := repo.ClearDirty(ctx, liveSession.ID, 1); err != nil || cleared {
		t.Fatalf("ClearDirty(replay) = (%v, %v), want (false, nil)", cleared, err)
	}

	staleSegment := testMediaSegment(t, mediaPath, firstAttempt, 2, now.UnixMilli()+4_000)
	if _, err := repo.PersistMediaSnapshot(ctx, PersistMediaSnapshotInput{
		SessionID: liveSession.ID, ExpectedRevision: 0, State: SessionMediaCompleted,
		Attempts: []MediaAttempt{firstAttempt, secondAttempt},
		Segments: []MediaSegment{segment, staleSegment}, Artifacts: []MediaArtifact{artifact},
	}); !errors.Is(err, ErrMediaSnapshotConflict) {
		t.Fatalf("stale PersistMediaSnapshot() error = %v, want ErrMediaSnapshotConflict", err)
	}

	changedIdentity := segment
	changedIdentity.RelativePath = mediaPath + "/segments/renamed.mkv"
	if _, err := repo.PersistMediaSnapshot(ctx, PersistMediaSnapshotInput{
		SessionID: liveSession.ID, ExpectedRevision: 1, State: SessionMediaCompleted,
		Attempts: []MediaAttempt{firstAttempt, secondAttempt},
		Segments: []MediaSegment{changedIdentity}, Artifacts: []MediaArtifact{artifact},
	}); !errors.Is(err, ErrMediaSnapshotConflict) {
		t.Fatalf("identity-changing PersistMediaSnapshot() error = %v, want ErrMediaSnapshotConflict", err)
	}

	illegalArtifact := artifact
	illegalArtifact.Status = MediaArtifactStatus("uploaded")
	if _, err := repo.PersistMediaSnapshot(ctx, PersistMediaSnapshotInput{
		SessionID: liveSession.ID, ExpectedRevision: 1, State: SessionMediaCompleted,
		Attempts: []MediaAttempt{firstAttempt, secondAttempt},
		Segments: []MediaSegment{segment}, Artifacts: []MediaArtifact{illegalArtifact},
	}); !errors.Is(err, ErrMediaContractInvalid) {
		t.Fatalf("invalid artifact status error = %v, want ErrMediaContractInvalid", err)
	}

	final, err := repo.LoadSnapshot(ctx, liveSession.ID)
	if err != nil {
		t.Fatalf("LoadSnapshot() after rejected writes error = %v", err)
	}
	if final.Session.ManifestRevision != 1 || final.Session.ManifestDirty ||
		len(final.Segments) != 1 || final.Segments[0].RelativePath != segment.RelativePath ||
		len(final.Artifacts) != 1 || final.Artifacts[0].Status != MediaArtifactPendingTranscode {
		t.Fatalf("rejected writes changed durable snapshot: %+v", final)
	}
}

func TestSQLiteRepositoryMediaAttemptJournalIsAppendOnlyAndMonotonic(t *testing.T) {
	ctx := context.Background()
	repo, store, _, roomID, now := openRepository(t)
	defer store.Close()
	liveSession, err := repo.Create(ctx, CreateSessionInput{
		RoomConfigID: roomID, OperationID: newV7(t), StartedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	mediaPath := liveSession.DataPath + "/media"
	if _, err := repo.OpenSessionMedia(ctx, OpenSessionMediaInput{
		SessionID: liveSession.ID, RelativePath: mediaPath, StartedAt: now.UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	first := testMediaAttempt(t, 1, now.UnixMilli())
	second := testMediaAttempt(t, 2, now.UnixMilli()+1_000)
	baselineAttempts := []MediaAttempt{first, second}
	baseline, err := repo.PersistMediaSnapshot(ctx, PersistMediaSnapshotInput{
		SessionID: liveSession.ID, ExpectedRevision: 0, State: SessionMediaOpen,
		Attempts: baselineAttempts,
	})
	if err != nil || baseline.Session.ManifestRevision != 1 {
		t.Fatalf("persist baseline attempts = (%+v, %v)", baseline.Session, err)
	}
	rejectedSegment := testMediaSegment(t, mediaPath, first, 1, now.UnixMilli())

	replaceID := second
	replaceID.ID = newV7(t)
	replaceOrdinal := second
	replaceOrdinal.Ordinal = 3
	replaceStartedAt := second
	replaceStartedAt.StartedAt++
	replaceSegmentSeconds := second
	replaceSegmentSeconds.SegmentSeconds = 600
	replaceVariant := second
	replaceVariant.VariantID = "alternate"
	replaceProtocol := second
	replaceProtocol.Protocol = "hls"
	replaceQualityKey := second
	replaceQualityKey.QualityKey = "hd"
	replaceQuality := second
	replaceQuality.Quality = "hd"
	replaceCodec := second
	replaceCodec.Codec = "h265"
	replaceBitrate := second
	replaceBitrate.Bitrate++
	regressCommitted := first
	regressCommitted.Committed, regressCommitted.Clean = false, false
	regressClean := first
	regressClean.Clean = false
	cleanWithoutCommitted := testMediaAttempt(t, 3, now.UnixMilli()+2_000)
	cleanWithoutCommitted.Committed, cleanWithoutCommitted.Clean = false, true

	tests := []struct {
		name     string
		attempts []MediaAttempt
		wantErr  error
	}{
		{name: "delete all", attempts: nil, wantErr: ErrMediaSnapshotConflict},
		{name: "truncate prefix", attempts: []MediaAttempt{first}, wantErr: ErrMediaSnapshotConflict},
		{name: "replace id", attempts: []MediaAttempt{first, replaceID}, wantErr: ErrMediaSnapshotConflict},
		{name: "replace ordinal", attempts: []MediaAttempt{first, replaceOrdinal}, wantErr: ErrMediaSnapshotConflict},
		{name: "replace started at", attempts: []MediaAttempt{first, replaceStartedAt}, wantErr: ErrMediaSnapshotConflict},
		{name: "replace segment seconds", attempts: []MediaAttempt{first, replaceSegmentSeconds}, wantErr: ErrMediaSnapshotConflict},
		{name: "replace variant", attempts: []MediaAttempt{first, replaceVariant}, wantErr: ErrMediaSnapshotConflict},
		{name: "replace protocol", attempts: []MediaAttempt{first, replaceProtocol}, wantErr: ErrMediaSnapshotConflict},
		{name: "replace quality key", attempts: []MediaAttempt{first, replaceQualityKey}, wantErr: ErrMediaSnapshotConflict},
		{name: "replace quality", attempts: []MediaAttempt{first, replaceQuality}, wantErr: ErrMediaSnapshotConflict},
		{name: "replace codec", attempts: []MediaAttempt{first, replaceCodec}, wantErr: ErrMediaSnapshotConflict},
		{name: "replace bitrate", attempts: []MediaAttempt{first, replaceBitrate}, wantErr: ErrMediaSnapshotConflict},
		{name: "regress committed", attempts: []MediaAttempt{regressCommitted, second}, wantErr: ErrMediaSnapshotConflict},
		{name: "regress clean", attempts: []MediaAttempt{regressClean, second}, wantErr: ErrMediaSnapshotConflict},
		{name: "reorder", attempts: []MediaAttempt{second, first}, wantErr: ErrMediaContractInvalid},
		{
			name:     "clean without committed",
			attempts: []MediaAttempt{first, second, cleanWithoutCommitted},
			wantErr:  ErrMediaContractInvalid,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := repo.PersistMediaSnapshot(ctx, PersistMediaSnapshotInput{
				SessionID: liveSession.ID, ExpectedRevision: 1, State: SessionMediaOpen,
				Attempts: test.attempts, Segments: []MediaSegment{rejectedSegment},
			}); !errors.Is(err, test.wantErr) {
				t.Fatalf("PersistMediaSnapshot() error = %v, want %v", err, test.wantErr)
			}
			unchanged, err := repo.LoadSnapshot(ctx, liveSession.ID)
			if err != nil {
				t.Fatal(err)
			}
			if unchanged.Session.ManifestRevision != 1 ||
				len(unchanged.Session.Attempts) != len(baselineAttempts) ||
				len(unchanged.Segments) != 0 {
				t.Fatalf("rejected journal mutation changed snapshot: %+v", unchanged)
			}
			for index := range baselineAttempts {
				if unchanged.Session.Attempts[index] != baselineAttempts[index] {
					t.Fatalf("attempt %d changed: got %+v want %+v",
						index, unchanged.Session.Attempts[index], baselineAttempts[index])
				}
			}
		})
	}

	third := testMediaAttempt(t, 3, now.UnixMilli()+2_000)
	third.Committed, third.Clean = false, false
	appendedAttempts := append(append([]MediaAttempt(nil), baselineAttempts...), third)
	appended, err := repo.PersistMediaSnapshot(ctx, PersistMediaSnapshotInput{
		SessionID: liveSession.ID, ExpectedRevision: 1, State: SessionMediaOpen,
		Attempts: appendedAttempts,
	})
	if err != nil || appended.Session.ManifestRevision != 2 {
		t.Fatalf("append attempt = (%+v, %v)", appended.Session, err)
	}
	third.Committed = true
	committedAttempts := append(append([]MediaAttempt(nil), baselineAttempts...), third)
	committed, err := repo.PersistMediaSnapshot(ctx, PersistMediaSnapshotInput{
		SessionID: liveSession.ID, ExpectedRevision: 2, State: SessionMediaOpen,
		Attempts: committedAttempts,
	})
	if err != nil || committed.Session.ManifestRevision != 3 ||
		!committed.Session.Attempts[2].Committed || committed.Session.Attempts[2].Clean {
		t.Fatalf("commit attempt = (%+v, %v)", committed.Session, err)
	}
	third.Clean = true
	cleanAttempts := append(append([]MediaAttempt(nil), baselineAttempts...), third)
	cleaned, err := repo.PersistMediaSnapshot(ctx, PersistMediaSnapshotInput{
		SessionID: liveSession.ID, ExpectedRevision: 3, State: SessionMediaOpen,
		Attempts: cleanAttempts,
	})
	if err != nil || cleaned.Session.ManifestRevision != 4 ||
		!cleaned.Session.Attempts[2].Committed || !cleaned.Session.Attempts[2].Clean {
		t.Fatalf("clean attempt = (%+v, %v)", cleaned.Session, err)
	}
}

func TestSQLiteRepositoryMediaBindsExternalRootAndPreventsDeletion(t *testing.T) {
	ctx := context.Background()
	repo, store, _, roomID, now := openRepository(t)
	defer store.Close()
	liveSession, err := repo.Create(ctx, CreateSessionInput{
		RoomConfigID: roomID, OperationID: newV7(t), StartedAt: now,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	root, err := repo.RegisterRecordingRoot(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("RegisterRecordingRoot() error = %v", err)
	}
	relativePath := "sessions/" + liveSession.ID + "/media"
	opened, err := repo.OpenSessionMedia(ctx, OpenSessionMediaInput{
		SessionID: liveSession.ID, RootID: &root.ID, RelativePath: relativePath,
		StartedAt: now.UnixMilli(),
	})
	if err != nil {
		t.Fatalf("OpenSessionMedia(external) error = %v", err)
	}
	if opened.Session.RootID == nil || *opened.Session.RootID != root.ID {
		t.Fatalf("external RootID = %v, want %q", opened.Session.RootID, root.ID)
	}
	if _, err := store.Writer().Exec(`DELETE FROM recording_roots WHERE id = ?`, root.ID); err == nil {
		t.Fatal("deleting an in-use recording root unexpectedly succeeded")
	}
	if _, err := repo.OpenSessionMedia(ctx, OpenSessionMediaInput{
		SessionID: liveSession.ID, RelativePath: relativePath, StartedAt: now.UnixMilli(),
	}); !errors.Is(err, ErrSessionMediaConflict) {
		t.Fatalf("changing external media location error = %v, want ErrSessionMediaConflict", err)
	}
}

func TestSQLiteRepositoryMediaRejectsUnsafeAndCorruptContracts(t *testing.T) {
	ctx := context.Background()
	repo, store, _, roomID, now := openRepository(t)
	defer store.Close()
	liveSession, err := repo.Create(ctx, CreateSessionInput{
		RoomConfigID: roomID, OperationID: newV7(t), StartedAt: now,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	for _, relativePath := range []string{
		"", "../escape", "/absolute", "C:/absolute", `rooms\escape`,
		"rooms/%2e%2e/escape", "https://secret.invalid/media", "rooms/media?token=secret",
	} {
		if _, err := repo.OpenSessionMedia(ctx, OpenSessionMediaInput{
			SessionID: liveSession.ID, RelativePath: relativePath, StartedAt: now.UnixMilli(),
		}); !errors.Is(err, ErrMediaContractInvalid) {
			t.Fatalf("OpenSessionMedia(%q) error = %v, want ErrMediaContractInvalid", relativePath, err)
		}
	}
	if _, err := repo.LoadSnapshot(ctx, liveSession.ID); !errors.Is(err, ErrSessionMediaNotFound) {
		t.Fatalf("LoadSnapshot() after rejected opens error = %v, want ErrSessionMediaNotFound", err)
	}

	mediaPath := liveSession.DataPath + "/media"
	if _, err := repo.OpenSessionMedia(ctx, OpenSessionMediaInput{
		SessionID: liveSession.ID, RelativePath: mediaPath, StartedAt: now.UnixMilli(),
	}); err != nil {
		t.Fatalf("OpenSessionMedia(valid) error = %v", err)
	}
	unsafeAttempt := testMediaAttempt(t, 1, now.UnixMilli())
	unsafeAttempt.VariantID = "https://private.invalid/stream?token=secret"
	if _, err := repo.PersistMediaSnapshot(ctx, PersistMediaSnapshotInput{
		SessionID: liveSession.ID, ExpectedRevision: 0, State: SessionMediaOpen,
		Attempts: []MediaAttempt{unsafeAttempt},
	}); !errors.Is(err, ErrMediaContractInvalid) {
		t.Fatalf("PersistMediaSnapshot(unsafe attempt) error = %v, want ErrMediaContractInvalid", err)
	}
	unchanged, err := repo.LoadSnapshot(ctx, liveSession.ID)
	if err != nil || unchanged.Session.ManifestRevision != 0 || len(unchanged.Session.Attempts) != 0 {
		t.Fatalf("unsafe attempt changed snapshot = (%+v, %v)", unchanged, err)
	}

	corrupt := `[{"id":"` + newV7(t) + `","ordinal":1,"startedAt":1,"segmentSeconds":300,"committed":false,"clean":false,"protocol":"flv","codec":"h264","unknown":"secret"}]`
	if _, err := store.Writer().Exec(`UPDATE session_media SET attempts_json = ? WHERE session_id = ?`, corrupt, liveSession.ID); err != nil {
		t.Fatalf("inject corrupt attempts_json: %v", err)
	}
	if _, err := repo.LoadSnapshot(ctx, liveSession.ID); !errors.Is(err, ErrMediaContractInvalid) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("LoadSnapshot(corrupt attempts) error = %v, want sanitized ErrMediaContractInvalid", err)
	}

	oversized := strings.Repeat("x", maxMediaAttemptsJSONBytes+1)
	if _, err := store.Writer().Exec(`UPDATE session_media SET attempts_json = ? WHERE session_id = ?`, oversized, liveSession.ID); err != nil {
		t.Fatalf("inject oversized attempts_json: %v", err)
	}
	if _, err := repo.LoadSnapshot(ctx, liveSession.ID); !errors.Is(err, ErrMediaContractInvalid) {
		t.Fatalf("LoadSnapshot(oversized attempts) error = %v, want ErrMediaContractInvalid", err)
	}
}

func TestSQLiteRepositoryBoundsDurableMediaCardinalityAcrossCASAndReads(t *testing.T) {
	ctx := context.Background()
	repo, store, _, roomID, now := openRepository(t)
	defer store.Close()
	liveSession, err := repo.Create(ctx, CreateSessionInput{
		RoomConfigID: roomID, OperationID: newV7(t), StartedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	mediaPath := liveSession.DataPath + "/media"
	if _, err := repo.OpenSessionMedia(ctx, OpenSessionMediaInput{
		SessionID: liveSession.ID, RelativePath: mediaPath, StartedAt: now.UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	attempt := testMediaAttempt(t, 1, now.UnixMilli())
	segments, artifacts := maximumDurableMediaFixture(t, mediaPath, attempt, now.UnixMilli())
	persisted, err := repo.PersistMediaSnapshot(ctx, PersistMediaSnapshotInput{
		SessionID: liveSession.ID, ExpectedRevision: 0, State: SessionMediaFinalizing,
		Attempts: []MediaAttempt{attempt}, Segments: segments, Artifacts: artifacts,
	})
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Session.ManifestRevision != 1 || len(persisted.Segments) != maximumMediaSegments ||
		len(persisted.Artifacts) != maximumMediaArtifacts {
		t.Fatalf("maximum durable snapshot = revision %d segments %d artifacts %d",
			persisted.Session.ManifestRevision, len(persisted.Segments), len(persisted.Artifacts))
	}

	extraSegment := durableMediaSegmentFixture(
		t, mediaPath, attempt, maximumMediaSegments+1, now.UnixMilli()+maximumMediaSegments+1,
	)
	extraArtifact := durableMediaArtifactFixture(
		t, mediaPath, extraSegment, MediaArtifactASRWAV, maximumMediaSegments+1, now.UnixMilli(),
	)
	for round := 1; round <= 20; round++ {
		if _, err := repo.PersistMediaSnapshot(ctx, PersistMediaSnapshotInput{
			SessionID: liveSession.ID, ExpectedRevision: 1, State: SessionMediaCompleted,
			Attempts: []MediaAttempt{attempt}, Segments: []MediaSegment{extraSegment},
			Artifacts: []MediaArtifact{extraArtifact},
		}); !errors.Is(err, ErrMediaContractInvalid) {
			t.Fatalf("round %d accumulated over-limit error = %v, want ErrMediaContractInvalid", round, err)
		}
	}
	unchanged, err := repo.LoadSnapshot(ctx, liveSession.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Session.ManifestRevision != 1 || len(unchanged.Segments) != maximumMediaSegments ||
		len(unchanged.Artifacts) != maximumMediaArtifacts {
		t.Fatalf("failed cardinality CAS changed durable snapshot: revision %d segments %d artifacts %d",
			unchanged.Session.ManifestRevision, len(unchanged.Segments), len(unchanged.Artifacts))
	}

	tx, err := store.Writer().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := upsertMediaSegment(ctx, tx, liveSession.ID, extraSegment); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := upsertMediaArtifact(ctx, tx, liveSession.ID, extraArtifact); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for round := 1; round <= 20; round++ {
		if _, err := repo.loadMediaSegments(ctx, liveSession.ID); !errors.Is(err, ErrMediaContractInvalid) {
			t.Fatalf("round %d max+1 segment load error = %v, want ErrMediaContractInvalid", round, err)
		}
		if _, err := repo.loadMediaArtifacts(ctx, liveSession.ID); !errors.Is(err, ErrMediaContractInvalid) {
			t.Fatalf("round %d max+1 artifact load error = %v, want ErrMediaContractInvalid", round, err)
		}
	}
	if _, err := repo.LoadSnapshot(ctx, liveSession.ID); !errors.Is(err, ErrMediaContractInvalid) {
		t.Fatalf("LoadSnapshot(max+1) error = %v, want ErrMediaContractInvalid", err)
	}
}

func TestSQLiteRepositoryRejectsInvalidDurableMediaRowsOnEveryLoad(t *testing.T) {
	ctx := context.Background()
	repo, store, _, roomID, now := openRepository(t)
	defer store.Close()
	firstSession, err := repo.Create(ctx, CreateSessionInput{
		RoomConfigID: roomID, OperationID: newV7(t), StartedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstPath := firstSession.DataPath + "/media"
	if _, err := repo.OpenSessionMedia(ctx, OpenSessionMediaInput{
		SessionID: firstSession.ID, RelativePath: firstPath, StartedAt: now.UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	firstAttempt := testMediaAttempt(t, 1, now.UnixMilli())
	invalidSegment := durableMediaSegmentFixture(t, firstPath, firstAttempt, 1, now.UnixMilli())
	invalidSegment.ID = "invalid-segment-id"
	tx, err := store.Writer().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := upsertMediaSegment(ctx, tx, firstSession.ID, invalidSegment); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for round := 1; round <= 20; round++ {
		if _, err := repo.loadMediaSegments(ctx, firstSession.ID); !errors.Is(err, ErrMediaContractInvalid) {
			t.Fatalf("round %d invalid segment load error = %v, want ErrMediaContractInvalid", round, err)
		}
	}
	if _, err := repo.LoadSnapshot(ctx, firstSession.ID); !errors.Is(err, ErrMediaContractInvalid) {
		t.Fatalf("LoadSnapshot(invalid segment) error = %v, want ErrMediaContractInvalid", err)
	}

	if _, err := store.Writer().Exec(`DELETE FROM live_sessions WHERE id = ?`, firstSession.ID); err != nil {
		t.Fatal(err)
	}
	secondSession, err := repo.Create(ctx, CreateSessionInput{
		RoomConfigID: roomID, OperationID: newV7(t), StartedAt: now.AddDate(0, 0, 1),
	})
	if err != nil {
		t.Fatal(err)
	}
	secondPath := secondSession.DataPath + "/media"
	if _, err := repo.OpenSessionMedia(ctx, OpenSessionMediaInput{
		SessionID: secondSession.ID, RelativePath: secondPath, StartedAt: now.AddDate(0, 0, 1).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	secondAttempt := testMediaAttempt(t, 1, now.AddDate(0, 0, 1).UnixMilli())
	validSegment := durableMediaSegmentFixture(t, secondPath, secondAttempt, 1, now.UnixMilli())
	invalidArtifact := durableMediaArtifactFixture(
		t, secondPath, validSegment, MediaArtifactASRWAV, 1, now.UnixMilli(),
	)
	invalidArtifact.ID = "invalid-artifact-id"
	tx, err = store.Writer().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := upsertMediaSegment(ctx, tx, secondSession.ID, validSegment); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := upsertMediaArtifact(ctx, tx, secondSession.ID, invalidArtifact); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for round := 1; round <= 20; round++ {
		if _, err := repo.loadMediaArtifacts(ctx, secondSession.ID); !errors.Is(err, ErrMediaContractInvalid) {
			t.Fatalf("round %d invalid artifact load error = %v, want ErrMediaContractInvalid", round, err)
		}
	}
	if _, err := repo.LoadSnapshot(ctx, secondSession.ID); !errors.Is(err, ErrMediaContractInvalid) {
		t.Fatalf("LoadSnapshot(invalid artifact) error = %v, want ErrMediaContractInvalid", err)
	}
}

func maximumDurableMediaFixture(
	t *testing.T,
	mediaPath string,
	attempt MediaAttempt,
	startedAt int64,
) ([]MediaSegment, []MediaArtifact) {
	t.Helper()
	segments := make([]MediaSegment, maximumMediaSegments)
	artifacts := make([]MediaArtifact, 0, maximumMediaArtifacts)
	for index := range segments {
		sequence := index + 1
		segment := durableMediaSegmentFixture(t, mediaPath, attempt, sequence, startedAt+int64(index))
		segments[index] = segment
		artifacts = append(artifacts,
			durableMediaArtifactFixture(t, mediaPath, segment, MediaArtifactASRWAV, sequence, startedAt),
			durableMediaArtifactFixture(t, mediaPath, segment, MediaArtifactPlaybackMP4, sequence, startedAt),
		)
	}
	return segments, artifacts
}

func durableMediaSegmentFixture(
	t *testing.T,
	mediaPath string,
	attempt MediaAttempt,
	sequence int,
	startedAt int64,
) MediaSegment {
	t.Helper()
	return MediaSegment{
		ID: newV7(t), Sequence: sequence,
		RelativePath: fmt.Sprintf("%s/segments/segment-%06d.mkv", mediaPath, sequence),
		Container:    "mkv", VideoCodec: "h264", AudioCodec: "aac",
		StartedAt: startedAt, EndedAt: startedAt + 1, DurationMS: 1, SizeBytes: 1,
		Status: MediaSegmentComplete, AttemptID: attempt.ID, AttemptSequence: sequence,
		SourceRelativePath: fmt.Sprintf(
			"%s/attempts/%s/segment-%06d.mkv.partial", mediaPath, attempt.ID, sequence,
		),
		ProbeVersion: mediaProbeVersion,
	}
}

func durableMediaArtifactFixture(
	t *testing.T,
	mediaPath string,
	segment MediaSegment,
	kind MediaArtifactKind,
	sequence int,
	createdAt int64,
) MediaArtifact {
	t.Helper()
	directory, name, container, codec := "audio", fmt.Sprintf("asr-%06d.wav", sequence), "wav", "pcm_s16le"
	if kind == MediaArtifactPlaybackMP4 {
		directory, name, container, codec = "artifacts", fmt.Sprintf("playback-%06d.mp4", sequence), "mp4", "h264"
	}
	return MediaArtifact{
		ID: newV7(t), MediaSegmentID: segment.ID, Kind: kind,
		RelativePath: fmt.Sprintf("%s/%s/%s", mediaPath, directory, name),
		Container:    container, Codec: codec, DurationMS: 1, SizeBytes: 1,
		Status: MediaArtifactPending, CreatedAt: createdAt, UpdatedAt: createdAt,
	}
}

func TestMediaContractsRedactPathsAndRejectUnsafeAttemptJSON(t *testing.T) {
	attempt := testMediaAttempt(t, 1, 1)
	if _, err := json.Marshal(attempt); err != nil {
		t.Fatalf("json.Marshal(valid attempt) error = %v", err)
	}
	unsafe := attempt
	unsafe.Quality = "https://private.invalid/origin?token=secret"
	if _, err := json.Marshal(unsafe); !errors.Is(err, ErrMediaContractInvalid) {
		t.Fatalf("json.Marshal(unsafe attempt) error = %v, want ErrMediaContractInvalid", err)
	}

	secretPath := "rooms/private/session/secret.mkv"
	values := []string{
		(SessionMedia{SessionID: newV7(t), RelativePath: secretPath, State: SessionMediaOpen}).String(),
		(MediaSegment{ID: newV7(t), RelativePath: secretPath, SourceRelativePath: secretPath}).String(),
		(MediaArtifact{ID: newV7(t), RelativePath: secretPath}).String(),
	}
	for _, value := range values {
		if strings.Contains(value, secretPath) || !strings.Contains(value, "redacted") {
			t.Fatalf("String() did not redact media path: %q", value)
		}
	}
}

func testMediaAttempt(t *testing.T, ordinal int, startedAt int64) MediaAttempt {
	t.Helper()
	return MediaAttempt{
		ID: newV7(t), Ordinal: ordinal, StartedAt: startedAt, SegmentSeconds: 300,
		Committed: true, Clean: true, VariantID: "main", Protocol: "flv",
		QualityKey: "origin", Quality: "origin", Codec: "h264", Bitrate: 2_000_000,
	}
}

func testMediaSegment(t *testing.T, mediaPath string, attempt MediaAttempt, sequence int, startedAt int64) MediaSegment {
	t.Helper()
	ptsStart := int64(10)
	ptsEnd := int64(1_010)
	return MediaSegment{
		ID: newV7(t), Sequence: sequence,
		RelativePath: mediaPath + "/segments/segment-" + strings.Repeat("0", 5) + string(rune('0'+sequence)) + ".mkv",
		Container:    "matroska", VideoCodec: "h264", AudioCodec: "aac",
		StartedAt: startedAt, EndedAt: startedAt + 1_000,
		PTSStartMS: &ptsStart, PTSEndMS: &ptsEnd, DurationMS: 1_000, SizeBytes: 42,
		SHA256: strings.Repeat("a", 64), Status: MediaSegmentComplete,
		AttemptID: attempt.ID, AttemptSequence: sequence,
		SourceRelativePath: mediaPath + "/attempts/" + attempt.ID + "/segment-" +
			string(rune('0'+sequence)) + ".partial",
		ProbeVersion: "ffprobe-8.1.2",
	}
}

func testMediaArtifact(t *testing.T, mediaPath string, segment MediaSegment, createdAt int64) MediaArtifact {
	t.Helper()
	return MediaArtifact{
		ID: newV7(t), MediaSegmentID: segment.ID, Kind: MediaArtifactPlaybackMP4,
		RelativePath: mediaPath + "/artifacts/" + segment.ID + ".mp4",
		Container:    "mp4", Codec: "h264", DurationMS: 1_000, SizeBytes: 84,
		SHA256: strings.Repeat("b", 64), SourceSHA256: segment.SHA256,
		Status: MediaArtifactPendingTranscode, CreatedAt: createdAt, UpdatedAt: createdAt,
	}
}
