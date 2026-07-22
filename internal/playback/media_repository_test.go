package playback

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
)

func TestRepositoryListsMediaAndLocatesUnifiedTimeline(t *testing.T) {
	fixture := newPlaybackFixture(t)
	defer fixture.close()
	sessionID := fixture.sessionIDs[0]
	if _, err := fixture.writer.Exec(
		`UPDATE live_sessions SET capture_offset_ms = 50 WHERE id = ?`,
		sessionID,
	); err != nil {
		t.Fatalf("update capture offset: %v", err)
	}
	segmentIDs := insertPlaybackMedia(t, fixture, sessionID)

	ctx := context.Background()
	first, err := fixture.repository.ListMediaSegments(
		ctx, MediaFilter{SessionID: sessionID}, PageRequest{Limit: 2},
	)
	if err != nil {
		t.Fatalf("ListMediaSegments() first page error = %v", err)
	}
	if first.Version != ContractVersion || len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatalf("first media page = %+v", first)
	}
	second, err := fixture.repository.ListMediaSegments(
		ctx,
		MediaFilter{SessionID: sessionID},
		PageRequest{Limit: 2, Cursor: first.NextCursor},
	)
	if err != nil {
		t.Fatalf("ListMediaSegments() second page error = %v", err)
	}
	gotIDs := []string{first.Items[0].ID, first.Items[1].ID, second.Items[0].ID}
	if strings.Join(gotIDs, ",") != strings.Join(segmentIDs, ",") {
		t.Fatalf("media keyset order = %v, want %v", gotIDs, segmentIDs)
	}
	if first.Items[0].TimelineStartMS != 50 ||
		first.Items[0].TimelineEndMS != 1050 ||
		first.Items[0].PlaybackArtifactID == "" ||
		len(first.Items[0].Artifacts) != 2 {
		t.Fatalf("mapped first media item = %+v", first.Items[0])
	}
	for _, artifact := range first.Items[0].Artifacts {
		if artifact.Kind == "playback_mp4" && !artifact.DirectPlayback {
			t.Fatalf("playback artifact was not direct: %+v", artifact)
		}
		if artifact.Kind == "asr_wav" && artifact.DirectPlayback {
			t.Fatalf("ASR artifact became direct playback: %+v", artifact)
		}
	}

	completeOnly, err := fixture.repository.ListMediaSegments(
		ctx,
		MediaFilter{SessionID: sessionID, Statuses: []string{"complete"}},
		PageRequest{},
	)
	if err != nil || len(completeOnly.Items) != 1 || completeOnly.Items[0].ID != segmentIDs[0] {
		t.Fatalf("complete media page = %+v, err = %v", completeOnly, err)
	}
	if _, err := fixture.repository.ListMediaSegments(
		ctx,
		MediaFilter{SessionID: sessionID, Statuses: []string{"recovered"}},
		PageRequest{Cursor: first.NextCursor},
	); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("filter-mismatched media cursor error = %v", err)
	}

	direct, err := fixture.repository.LocateMedia(ctx, MediaLocationRequest{
		SessionID: sessionID, SessionOffsetMS: 550,
	})
	if err != nil {
		t.Fatalf("LocateMedia() direct error = %v", err)
	}
	if direct.State != MediaLocationPlaybackMP4 || direct.AdjustedOffsetMS != 600 ||
		direct.Segment == nil || direct.Segment.ID != segmentIDs[0] ||
		direct.SegmentPlaybackMS == nil || *direct.SegmentPlaybackMS != 500 ||
		direct.PlaybackArtifactID == "" {
		t.Fatalf("direct media location = %+v", direct)
	}

	source, err := fixture.repository.LocateMedia(ctx, MediaLocationRequest{
		SessionID: sessionID, SessionOffsetMS: 1150,
	})
	if err != nil {
		t.Fatalf("LocateMedia() source error = %v", err)
	}
	if source.State != MediaLocationSourceMKV || source.AdjustedOffsetMS != 1200 ||
		source.Segment == nil || source.Segment.ID != segmentIDs[1] ||
		source.SegmentPlaybackMS == nil || *source.SegmentPlaybackMS != 100 ||
		source.ReasonCode != "PLAYBACK_MP4_UNAVAILABLE" {
		t.Fatalf("source media location = %+v", source)
	}

	unavailable, err := fixture.repository.LocateMedia(ctx, MediaLocationRequest{
		SessionID: sessionID, SessionOffsetMS: 2150,
	})
	if err != nil {
		t.Fatalf("LocateMedia() unavailable error = %v", err)
	}
	if unavailable.State != MediaLocationGap || unavailable.Segment == nil ||
		unavailable.Segment.ID != segmentIDs[2] ||
		unavailable.ReasonCode != "MEDIA_FINAL_CHANGED" {
		t.Fatalf("unavailable media location = %+v", unavailable)
	}

	gap, err := fixture.repository.LocateMedia(ctx, MediaLocationRequest{
		SessionID: sessionID, SessionOffsetMS: 5000,
	})
	if err != nil || gap.State != MediaLocationGap || gap.Segment != nil ||
		gap.ReasonCode != "MEDIA_TIMELINE_GAP" {
		t.Fatalf("timeline gap = %+v, err = %v", gap, err)
	}
}

func TestPhase4AcceptanceCalibratedTimelineP95(t *testing.T) {
	fixture := newPlaybackFixture(t)
	defer fixture.close()
	sessionID := fixture.sessionIDs[0]
	if _, err := fixture.writer.Exec(
		`UPDATE live_sessions SET capture_offset_ms = 50, clock_source = 'calibrated' WHERE id = ?`,
		sessionID,
	); err != nil {
		t.Fatalf("update calibrated capture offset: %v", err)
	}
	insertPlaybackMedia(t, fixture, sessionID)

	type calibrationSample struct {
		sessionOffsetMS int64
		playbackMS      int64
	}
	samples := []calibrationSample{
		{50, 0}, {175, 125}, {300, 250}, {425, 375}, {550, 500},
		{675, 625}, {800, 750}, {925, 875}, {1050, 0}, {1175, 125},
		{1300, 250}, {1425, 375}, {1550, 500}, {1675, 625}, {1800, 750},
		{1925, 875},
	}
	errorsMS := make([]int64, 0, len(samples))
	for _, sample := range samples {
		located, err := fixture.repository.LocateMedia(context.Background(), MediaLocationRequest{
			SessionID: sessionID, SessionOffsetMS: sample.sessionOffsetMS,
		})
		if err != nil {
			t.Fatalf("LocateMedia(%d) error = %v", sample.sessionOffsetMS, err)
		}
		if located.SegmentPlaybackMS == nil {
			t.Fatalf("LocateMedia(%d) has no playback offset: %+v", sample.sessionOffsetMS, located)
		}
		delta := *located.SegmentPlaybackMS - sample.playbackMS
		if delta < 0 {
			delta = -delta
		}
		errorsMS = append(errorsMS, delta)
	}
	slices.Sort(errorsMS)
	p95Index := (95*len(errorsMS)+99)/100 - 1
	if p95 := errorsMS[p95Index]; p95 > 1500 {
		t.Fatalf("calibrated timeline P95 = %d ms, want <= 1500 ms", p95)
	}
}

func TestRepositoryMediaDTOIsPrivacyAllowlistedAndReadOnly(t *testing.T) {
	fixture := newPlaybackFixture(t)
	defer fixture.close()
	sessionID := fixture.sessionIDs[0]
	insertPlaybackMedia(t, fixture, sessionID)

	var beforeChanges int64
	if err := fixture.writer.QueryRow(`SELECT total_changes()`).Scan(&beforeChanges); err != nil {
		t.Fatalf("read total_changes before media query: %v", err)
	}
	page, err := fixture.repository.ListMediaSegments(
		context.Background(), MediaFilter{SessionID: sessionID}, PageRequest{},
	)
	if err != nil {
		t.Fatalf("ListMediaSegments() error = %v", err)
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatalf("marshal media page: %v", err)
	}
	text := string(encoded)
	for _, forbidden := range []string{
		"private/media", "private/source", strings.Repeat("a", 64),
		strings.Repeat("b", 64), "attempt",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("media DTO leaked %q: %s", forbidden, text)
		}
	}
	for _, required := range []string{
		"playback_mp4", "asr_wav", "timelineStartMs", "directPlayback",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("media DTO lacks %q: %s", required, text)
		}
	}
	var afterChanges int64
	if err := fixture.writer.QueryRow(`SELECT total_changes()`).Scan(&afterChanges); err != nil {
		t.Fatalf("read total_changes after media query: %v", err)
	}
	if afterChanges != beforeChanges {
		t.Fatalf("media queries changed database: before=%d after=%d", beforeChanges, afterChanges)
	}
}

func TestRepositoryMediaRejectsMalformedArguments(t *testing.T) {
	fixture := newPlaybackFixture(t)
	defer fixture.close()
	ctx := context.Background()
	sessionID := fixture.sessionIDs[0]

	if _, err := fixture.repository.ListMediaSegments(
		nil, MediaFilter{SessionID: sessionID}, PageRequest{},
	); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil media context error = %v", err)
	}
	if _, err := fixture.repository.ListMediaSegments(
		ctx,
		MediaFilter{SessionID: sessionID, Statuses: []string{"complete", "complete"}},
		PageRequest{},
	); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("duplicate media status error = %v", err)
	}
	if _, err := fixture.repository.ListMediaSegments(
		ctx,
		MediaFilter{SessionID: sessionID, Statuses: []string{"uploaded"}},
		PageRequest{},
	); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalid media status error = %v", err)
	}
	if _, err := fixture.repository.LocateMedia(ctx, MediaLocationRequest{
		SessionID: sessionID, SessionOffsetMS: -1,
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("negative media offset error = %v", err)
	}
	if _, err := fixture.writer.Exec(
		`UPDATE live_sessions SET capture_offset_ms = 1 WHERE id = ?`, sessionID,
	); err != nil {
		t.Fatalf("update overflow capture offset: %v", err)
	}
	if _, err := fixture.repository.LocateMedia(ctx, MediaLocationRequest{
		SessionID: sessionID, SessionOffsetMS: math.MaxInt64,
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("overflow media offset error = %v", err)
	}
	if _, err := fixture.repository.LocateMedia(ctx, MediaLocationRequest{
		SessionID: newUUIDv7(t), SessionOffsetMS: 0,
	}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("missing media session error = %v", err)
	}
}

func insertPlaybackMedia(
	t *testing.T,
	fixture playbackFixture,
	sessionID string,
) []string {
	t.Helper()
	segmentIDs := []string{newUUIDv7(t), newUUIDv7(t), newUUIDv7(t)}
	digest := strings.Repeat("a", 64)
	rows := []struct {
		id, status, errorCode string
		sequence              int
		startedAt, endedAt    int64
		ptsStart, ptsEnd      int64
	}{
		{segmentIDs[0], "complete", "", 1, 1100, 2100, 0, 1000},
		{segmentIDs[1], "recovered", "", 2, 2100, 3100, 1000, 2000},
		{segmentIDs[2], "corrupt", "MEDIA_FINAL_CHANGED", 3, 3100, 4100, 2000, 3000},
	}
	for _, row := range rows {
		var errorCode any
		if row.errorCode != "" {
			errorCode = row.errorCode
		}
		if _, err := fixture.writer.Exec(`INSERT INTO media_segments(
			id, session_id, sequence, relative_path, container, video_codec,
			audio_codec, started_at, ended_at, pts_start_ms, pts_end_ms,
			duration_ms, size_bytes, sha256, status, error_code
		) VALUES (?, ?, ?, ?, 'mkv', 'h264', 'aac', ?, ?, ?, ?,
			1000, 4096, ?, ?, ?)`,
			row.id, sessionID, row.sequence,
			"private/media/segment-"+row.id+".mkv",
			row.startedAt, row.endedAt, row.ptsStart, row.ptsEnd,
			digest, row.status, errorCode,
		); err != nil {
			t.Fatalf("insert playback media segment %d: %v", row.sequence, err)
		}
	}
	for _, artifact := range []struct {
		kind, container, codec, digest string
		sampleRate, channels           int
	}{
		{"asr_wav", "wav", "pcm_s16le", strings.Repeat("b", 64), 16000, 1},
		{"playback_mp4", "mp4", "h264", strings.Repeat("c", 64), 0, 0},
	} {
		id := newUUIDv7(t)
		if _, err := fixture.writer.Exec(`INSERT INTO media_artifacts(
			id, session_id, media_segment_id, kind, relative_path, container,
			codec, duration_ms, size_bytes, sample_rate, channels, sha256,
			source_sha256, status, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, 1000, 2048, ?, ?, ?, ?,
			'complete', 1, 1)`,
			id, sessionID, segmentIDs[0], artifact.kind,
			"private/source/"+id, artifact.container, artifact.codec,
			artifact.sampleRate, artifact.channels, artifact.digest, digest,
		); err != nil {
			t.Fatalf("insert playback media artifact %s: %v", artifact.kind, err)
		}
	}
	return segmentIDs
}
