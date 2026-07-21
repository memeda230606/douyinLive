package playback

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
)

const (
	cursorKindMedia = "media"
)

var validMediaSegmentStatuses = map[string]struct{}{
	"partial": {}, "complete": {}, "recovered": {}, "corrupt": {}, "missing": {},
}

type normalizedMediaFilter struct {
	SessionID string   `json:"sessionId"`
	Statuses  []string `json:"statuses,omitempty"`
}

const mediaSegmentSelectSQL = `SELECT ms.id, ms.sequence, ms.container,
	ms.video_codec, ms.audio_codec, ms.started_at, ms.ended_at,
	ms.pts_start_ms, ms.pts_end_ms, ms.duration_ms, ms.size_bytes,
	ms.sha256, ms.status, ms.error_code, s.started_at, s.media_epoch_at
	FROM media_segments ms
	JOIN live_sessions s ON s.id = ms.session_id`

func (r *Repository) ListMediaSegments(
	ctx context.Context,
	filter MediaFilter,
	page PageRequest,
) (MediaPage, error) {
	if ctx == nil {
		return MediaPage{}, fmt.Errorf("%w: context", ErrInvalidArgument)
	}
	normalized, err := normalizeMediaFilter(filter)
	if err != nil {
		return MediaPage{}, err
	}
	limit, err := normalizeLimit(page.Limit)
	if err != nil {
		return MediaPage{}, err
	}
	filterHash, err := filterDigest(cursorKindMedia, normalized)
	if err != nil {
		return MediaPage{}, err
	}
	cursor, hasCursor, err := decodeCursor(page.Cursor, cursorKindMedia, filterHash)
	if err != nil {
		return MediaPage{}, err
	}

	query := mediaSegmentSelectSQL + ` WHERE ms.session_id = ?`
	args := []any{normalized.SessionID}
	query, args = appendStringSet(query, args, "ms.status", normalized.Statuses)
	if hasCursor {
		query += ` AND (ms.sequence > ? OR (ms.sequence = ? AND ms.id > ?))`
		args = append(args, cursor.Primary, cursor.Primary, cursor.ID)
	}
	query += ` ORDER BY ms.sequence, ms.id LIMIT ?`
	args = append(args, limit+1)

	rows, err := r.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return MediaPage{}, fmt.Errorf("list playback media: %w", err)
	}
	defer rows.Close()

	items := make([]MediaSegmentDTO, 0, limit+1)
	digests := make(map[string]string, limit+1)
	for rows.Next() {
		item, digest, err := scanMediaSegment(rows)
		if err != nil {
			return MediaPage{}, fmt.Errorf("scan playback media: %w", err)
		}
		items = append(items, item)
		digests[item.ID] = digest
	}
	if err := rows.Err(); err != nil {
		return MediaPage{}, fmt.Errorf("iterate playback media: %w", err)
	}

	result := MediaPage{Version: ContractVersion, Items: items}
	if len(items) > limit {
		last := items[limit-1]
		result.Items = items[:limit]
		result.NextCursor, err = encodeCursor(
			cursorKindMedia, filterHash, int64(last.Sequence), last.ID,
		)
		if err != nil {
			return MediaPage{}, err
		}
	}
	if err := r.attachMediaArtifacts(ctx, result.Items, digests); err != nil {
		return MediaPage{}, err
	}
	return result, nil
}

func (r *Repository) LocateMedia(
	ctx context.Context,
	request MediaLocationRequest,
) (MediaLocationResult, error) {
	if ctx == nil {
		return MediaLocationResult{}, fmt.Errorf("%w: context", ErrInvalidArgument)
	}
	if !isUUIDv7(request.SessionID) || request.SessionOffsetMS < 0 {
		return MediaLocationResult{}, fmt.Errorf("%w: media location", ErrInvalidArgument)
	}

	var sessionStartedAt, captureOffsetMS int64
	err := r.reader.QueryRowContext(ctx,
		`SELECT started_at, capture_offset_ms FROM live_sessions WHERE id = ?`,
		request.SessionID,
	).Scan(&sessionStartedAt, &captureOffsetMS)
	if errors.Is(err, sql.ErrNoRows) {
		return MediaLocationResult{}, ErrSessionNotFound
	}
	if err != nil {
		return MediaLocationResult{}, fmt.Errorf("load playback media session: %w", err)
	}
	adjustedOffsetMS, ok := checkedAddInt64(request.SessionOffsetMS, captureOffsetMS)
	if !ok {
		return MediaLocationResult{}, fmt.Errorf("%w: adjusted media offset", ErrInvalidArgument)
	}
	targetWallMS, ok := checkedAddInt64(sessionStartedAt, adjustedOffsetMS)
	if !ok {
		return MediaLocationResult{}, fmt.Errorf("%w: media wall clock", ErrInvalidArgument)
	}

	result := MediaLocationResult{
		Version: ContractVersion, SessionID: request.SessionID,
		RequestedOffsetMS: request.SessionOffsetMS, AdjustedOffsetMS: adjustedOffsetMS,
		State: MediaLocationGap, ReasonCode: "MEDIA_TIMELINE_GAP",
	}
	query := mediaSegmentSelectSQL +
		` WHERE ms.session_id = ? AND ms.started_at <= ? AND ms.ended_at > ?` +
		` ORDER BY CASE ms.status
			WHEN 'complete' THEN 0 WHEN 'recovered' THEN 1
			WHEN 'partial' THEN 2 WHEN 'corrupt' THEN 3 ELSE 4
		END, ms.sequence, ms.id LIMIT 1`
	row := r.reader.QueryRowContext(ctx, query, request.SessionID, targetWallMS, targetWallMS)
	segment, segmentDigest, err := scanMediaSegment(row)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return MediaLocationResult{}, fmt.Errorf("locate playback media: %w", err)
	}
	located := []MediaSegmentDTO{segment}
	if err := r.attachMediaArtifacts(
		ctx, located, map[string]string{segment.ID: segmentDigest},
	); err != nil {
		return MediaLocationResult{}, err
	}
	segment = located[0]
	result.Segment = &segment

	segmentOffset, ok := checkedSubInt64(adjustedOffsetMS, segment.TimelineStartMS)
	if !ok || segmentOffset < 0 {
		result.ReasonCode = "MEDIA_TIMELINE_INVALID"
		return result, nil
	}
	result.SegmentPlaybackMS = &segmentOffset
	if (segment.Status != "complete" && segment.Status != "recovered") ||
		len(segmentDigest) != 64 || segment.SizeBytes <= 0 {
		if segment.ErrorCode != "" {
			result.ReasonCode = segment.ErrorCode
		} else {
			result.ReasonCode = "MEDIA_SEGMENT_" + strings.ToUpper(segment.Status)
		}
		return result, nil
	}
	if segment.PlaybackArtifactID != "" {
		result.State = MediaLocationPlaybackMP4
		result.ReasonCode = ""
		result.PlaybackArtifactID = segment.PlaybackArtifactID
		return result, nil
	}
	if segment.Container == "mkv" {
		result.State = MediaLocationSourceMKV
		result.ReasonCode = "PLAYBACK_MP4_UNAVAILABLE"
		return result, nil
	}
	result.ReasonCode = "MEDIA_CONTAINER_UNSUPPORTED"
	return result, nil
}

func scanMediaSegment(source scanner) (MediaSegmentDTO, string, error) {
	var item MediaSegmentDTO
	var videoCodec, audioCodec, digest, errorCode sql.NullString
	var ptsStart, ptsEnd, mediaEpoch sql.NullInt64
	var sessionStartedAt int64
	if err := source.Scan(
		&item.ID, &item.Sequence, &item.Container, &videoCodec, &audioCodec,
		&item.StartedAt, &item.EndedAt, &ptsStart, &ptsEnd, &item.DurationMS,
		&item.SizeBytes, &digest, &item.Status, &errorCode,
		&sessionStartedAt, &mediaEpoch,
	); err != nil {
		return MediaSegmentDTO{}, "", err
	}
	item.VideoCodec = videoCodec.String
	item.AudioCodec = audioCodec.String
	item.ErrorCode = errorCode.String
	item.PTSStartMS = nullableInt64(ptsStart)
	item.PTSEndMS = nullableInt64(ptsEnd)
	item.Artifacts = make([]MediaArtifactDTO, 0)

	startWallMS := item.StartedAt
	endWallMS := item.EndedAt
	if mediaEpoch.Valid && ptsStart.Valid {
		var ok bool
		startWallMS, ok = checkedAddInt64(mediaEpoch.Int64, ptsStart.Int64)
		if !ok {
			return MediaSegmentDTO{}, "", errors.New("media timeline start overflow")
		}
	}
	if mediaEpoch.Valid && ptsEnd.Valid {
		var ok bool
		endWallMS, ok = checkedAddInt64(mediaEpoch.Int64, ptsEnd.Int64)
		if !ok {
			return MediaSegmentDTO{}, "", errors.New("media timeline end overflow")
		}
	}
	var ok bool
	item.TimelineStartMS, ok = checkedSubInt64(startWallMS, sessionStartedAt)
	if !ok {
		return MediaSegmentDTO{}, "", errors.New("media timeline start overflow")
	}
	item.TimelineEndMS, ok = checkedSubInt64(endWallMS, sessionStartedAt)
	if !ok || item.TimelineEndMS < item.TimelineStartMS {
		return MediaSegmentDTO{}, "", errors.New("media timeline range invalid")
	}
	return item, digest.String, nil
}

func (r *Repository) attachMediaArtifacts(
	ctx context.Context,
	items []MediaSegmentDTO,
	segmentDigests map[string]string,
) error {
	if len(items) == 0 {
		return nil
	}
	placeholders := make([]string, len(items))
	args := make([]any, len(items))
	indexByID := make(map[string]int, len(items))
	for index := range items {
		placeholders[index] = "?"
		args[index] = items[index].ID
		indexByID[items[index].ID] = index
	}
	query := `SELECT id, media_segment_id, kind, container, codec, duration_ms,
		size_bytes, sample_rate, channels, sha256, source_sha256, status, error_code
		FROM media_artifacts WHERE media_segment_id IN (` +
		strings.Join(placeholders, ",") + `)
		ORDER BY media_segment_id, kind, id`
	rows, err := r.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("list playback media artifacts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item MediaArtifactDTO
		var digest, sourceDigest string
		var errorCode sql.NullString
		if err := rows.Scan(
			&item.ID, &item.MediaSegmentID, &item.Kind, &item.Container,
			&item.Codec, &item.DurationMS, &item.SizeBytes, &item.SampleRate,
			&item.Channels, &digest, &sourceDigest, &item.Status, &errorCode,
		); err != nil {
			return fmt.Errorf("scan playback media artifact: %w", err)
		}
		item.ErrorCode = errorCode.String
		item.DirectPlayback = item.Kind == "playback_mp4" && item.Status == "complete" &&
			item.Container == "mp4" && item.Codec == "h264" && item.SizeBytes > 0 &&
			len(segmentDigests[item.MediaSegmentID]) == 64 &&
			len(digest) == 64 && sourceDigest == segmentDigests[item.MediaSegmentID]
		itemIndex, exists := indexByID[item.MediaSegmentID]
		if !exists {
			return errors.New("playback artifact references unexpected segment")
		}
		items[itemIndex].Artifacts = append(items[itemIndex].Artifacts, item)
		if item.DirectPlayback {
			items[itemIndex].PlaybackArtifactID = item.ID
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate playback media artifacts: %w", err)
	}
	return nil
}

func normalizeMediaFilter(filter MediaFilter) (normalizedMediaFilter, error) {
	if !isUUIDv7(filter.SessionID) {
		return normalizedMediaFilter{}, fmt.Errorf("%w: session id", ErrInvalidArgument)
	}
	statuses, err := normalizeSet(
		filter.Statuses, validMediaSegmentStatuses, "media status",
	)
	if err != nil {
		return normalizedMediaFilter{}, err
	}
	return normalizedMediaFilter{SessionID: filter.SessionID, Statuses: statuses}, nil
}

func checkedAddInt64(left, right int64) (int64, bool) {
	if right > 0 && left > math.MaxInt64-right {
		return 0, false
	}
	if right < 0 && left < math.MinInt64-right {
		return 0, false
	}
	return left + right, true
}

func checkedSubInt64(left, right int64) (int64, bool) {
	if right > 0 && left < math.MinInt64+right {
		return 0, false
	}
	if right < 0 && left > math.MaxInt64+right {
		return 0, false
	}
	return left - right, true
}
