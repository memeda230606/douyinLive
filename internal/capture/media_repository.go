package capture

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"sort"
)

func (finalizer *sqliteSessionMediaFinalizer) AppendMediaAttempt(
	ctx context.Context,
	attempt MediaAttempt,
) error {
	return finalizer.persistMediaAttempt(ctx, attempt, true)
}

func (finalizer *sqliteSessionMediaFinalizer) UpdateMediaAttempt(
	ctx context.Context,
	attempt MediaAttempt,
) error {
	return finalizer.persistMediaAttempt(ctx, attempt, false)
}

func (finalizer *sqliteSessionMediaFinalizer) persistMediaAttempt(
	ctx context.Context,
	attempt MediaAttempt,
	appendOnly bool,
) error {
	if finalizer == nil || finalizer.repository == nil || ctx == nil ||
		validateMediaAttempt(attempt) != nil {
		return ErrMediaContractInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	finalizer.operationMu.Lock()
	defer finalizer.operationMu.Unlock()

	current, err := finalizer.repository.LoadSnapshot(ctx, finalizer.sessionID)
	if err != nil {
		return err
	}
	if current.Session.State != SessionMediaOpen {
		return ErrMediaSnapshotConflict
	}
	attempts := append([]MediaAttempt(nil), current.Session.Attempts...)
	matched := -1
	for index, existing := range attempts {
		if existing.ID != attempt.ID && existing.Ordinal != attempt.Ordinal {
			continue
		}
		if existing.ID != attempt.ID || existing.Ordinal != attempt.Ordinal || matched >= 0 {
			return ErrMediaSnapshotConflict
		}
		matched = index
	}

	if appendOnly {
		if attempt.Committed || attempt.Clean {
			return ErrMediaContractInvalid
		}
		if matched >= 0 {
			if attempts[matched] != attempt {
				return ErrMediaSnapshotConflict
			}
			_, _ = finalizer.repository.materializeMediaManifest(
				ctx, finalizer.root, finalizer.sessionID,
			)
			return nil
		}
		attempts = append(attempts, attempt)
	} else {
		if matched < 0 {
			return ErrMediaSnapshotConflict
		}
		existing := attempts[matched]
		if !sameMediaAttemptIdentity(existing, attempt) ||
			existing.Committed && !attempt.Committed || existing.Clean && !attempt.Clean {
			return ErrMediaSnapshotConflict
		}
		if existing == attempt {
			_, _ = finalizer.repository.materializeMediaManifest(
				ctx, finalizer.root, finalizer.sessionID,
			)
			return nil
		}
		attempts[matched] = attempt
	}

	if _, err := finalizer.repository.PersistMediaSnapshot(ctx, PersistMediaSnapshotInput{
		SessionID: finalizer.sessionID, ExpectedRevision: current.Session.ManifestRevision,
		State: current.Session.State, Attempts: attempts,
	}); err != nil {
		return err
	}
	// The SQLite CAS is the authoritative attempt journal. Manifest materialization
	// is a repairable projection: PersistMediaSnapshot deliberately leaves it
	// dirty until this succeeds. A projection failure after the commit must not
	// make the recorder forget an ordinal that is already durable in SQLite.
	_, _ = finalizer.repository.materializeMediaManifest(
		ctx, finalizer.root, finalizer.sessionID,
	)
	return nil
}

func sameMediaAttemptIdentity(left, right MediaAttempt) bool {
	left.Committed, left.Clean = false, false
	right.Committed, right.Clean = false, false
	return left == right
}

func validateMediaAttemptJournalTransition(durable, incoming []MediaAttempt) error {
	if len(incoming) < len(durable) {
		return ErrMediaSnapshotConflict
	}
	for index, attempt := range incoming {
		if attempt.Clean && !attempt.Committed {
			return ErrMediaContractInvalid
		}
		if index >= len(durable) {
			continue
		}
		existing := durable[index]
		if existing.Clean && !existing.Committed {
			return ErrMediaContractInvalid
		}
		if !sameMediaAttemptIdentity(existing, attempt) ||
			existing.Committed && !attempt.Committed ||
			existing.Clean && !attempt.Clean {
			return ErrMediaSnapshotConflict
		}
	}
	return nil
}

func (r *SQLiteRepository) OpenSessionMedia(ctx context.Context, input OpenSessionMediaInput) (MediaSnapshot, error) {
	if err := validateOpenSessionMediaInput(ctx, input); err != nil {
		return MediaSnapshot{}, err
	}
	createdAt := input.StartedAt
	if createdAt == 0 {
		createdAt = r.now().UTC().UnixMilli()
	}
	rootValue := nullableRootID(input.RootID)
	_, err := r.writer.ExecContext(ctx, `INSERT INTO session_media(
		session_id, root_id, relative_path, state, manifest_revision,
		manifest_dirty, attempts_json, created_at, updated_at
	) VALUES (?, ?, ?, 'open', 0, 1, '[]', ?, ?)
	ON CONFLICT(session_id) DO NOTHING`,
		input.SessionID, rootValue, input.RelativePath, createdAt, createdAt)
	if err != nil {
		return MediaSnapshot{}, ErrMediaPersistence
	}
	snapshot, err := r.LoadSnapshot(ctx, input.SessionID)
	if err != nil {
		return MediaSnapshot{}, err
	}
	if !sameOptionalString(snapshot.Session.RootID, input.RootID) || snapshot.Session.RelativePath != input.RelativePath {
		return MediaSnapshot{}, ErrSessionMediaConflict
	}
	return snapshot, nil
}

func (r *SQLiteRepository) PersistMediaSnapshot(ctx context.Context, input PersistMediaSnapshotInput) (MediaSnapshot, error) {
	validated, err := validatePersistMediaSnapshotInput(ctx, input)
	if err != nil {
		return MediaSnapshot{}, err
	}

	unlock := r.lockManifestSession(input.SessionID)
	current, err := r.LoadSnapshot(ctx, input.SessionID)
	if err != nil {
		unlock()
		return MediaSnapshot{}, err
	}
	if current.Session.ManifestRevision != input.ExpectedRevision {
		unlock()
		return MediaSnapshot{}, ErrMediaSnapshotConflict
	}

	tx, err := r.writer.BeginTx(ctx, nil)
	if err != nil {
		unlock()
		return MediaSnapshot{}, ErrMediaPersistence
	}
	defer tx.Rollback()

	var currentRevision, currentUpdatedAt int64
	var durableAttemptsJSON string
	if err := tx.QueryRowContext(ctx, `SELECT manifest_revision, updated_at, attempts_json
		FROM session_media WHERE session_id = ?`, input.SessionID).Scan(
		&currentRevision, &currentUpdatedAt, &durableAttemptsJSON,
	); err != nil {
		unlock()
		if errors.Is(err, sql.ErrNoRows) {
			return MediaSnapshot{}, ErrSessionMediaNotFound
		}
		return MediaSnapshot{}, ErrMediaPersistence
	}
	if currentRevision != input.ExpectedRevision {
		unlock()
		return MediaSnapshot{}, ErrMediaSnapshotConflict
	}
	durableAttempts, err := decodeMediaAttempts(durableAttemptsJSON)
	if err != nil {
		unlock()
		return MediaSnapshot{}, ErrMediaContractInvalid
	}
	if err := validateMediaAttemptJournalTransition(durableAttempts, validated.attempts); err != nil {
		unlock()
		return MediaSnapshot{}, err
	}
	updatedAt := input.UpdatedAt
	if updatedAt == 0 {
		updatedAt = r.now().UTC().UnixMilli()
	}
	if updatedAt <= currentUpdatedAt {
		updatedAt = currentUpdatedAt + 1
	}

	for _, segment := range validated.segments {
		if err := upsertMediaSegment(ctx, tx, input.SessionID, segment); err != nil {
			unlock()
			return MediaSnapshot{}, err
		}
	}
	for _, artifact := range validated.artifacts {
		if err := upsertMediaArtifact(ctx, tx, input.SessionID, artifact); err != nil {
			unlock()
			return MediaSnapshot{}, err
		}
	}
	if err := validateDurableMediaCardinality(ctx, tx, input.SessionID); err != nil {
		unlock()
		return MediaSnapshot{}, err
	}

	result, err := tx.ExecContext(ctx, `UPDATE session_media SET
		state = ?, manifest_revision = manifest_revision + 1, manifest_dirty = 1,
		media_epoch_at = CASE WHEN ? IS NULL THEN media_epoch_at ELSE ? END,
		attempts_json = ?, updated_at = ?
		WHERE session_id = ? AND manifest_revision = ?`,
		input.State, nullableInt64(input.MediaEpochAt), nullableInt64(input.MediaEpochAt),
		string(validated.attemptsJSON), updatedAt, input.SessionID, input.ExpectedRevision)
	if err != nil {
		unlock()
		return MediaSnapshot{}, ErrMediaPersistence
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		unlock()
		return MediaSnapshot{}, ErrMediaSnapshotConflict
	}

	mediaEpochChanged := input.MediaEpochAt != nil
	if mediaEpochChanged {
		result, err = tx.ExecContext(ctx, `UPDATE live_sessions SET
			media_epoch_at = ?, clock_source = 'media', manifest_dirty = 1,
			updated_at = CASE WHEN updated_at >= ? THEN updated_at + 1 ELSE ? END
			WHERE id = ?`, *input.MediaEpochAt, updatedAt, updatedAt, input.SessionID)
		if err != nil {
			unlock()
			return MediaSnapshot{}, ErrMediaPersistence
		}
		affected, err = result.RowsAffected()
		if err != nil || affected != 1 {
			unlock()
			return MediaSnapshot{}, ErrSessionMediaNotFound
		}
	}
	committed := committedMediaSnapshot(current, input, validated, updatedAt)
	if err := tx.Commit(); err != nil {
		resolved, resolveErr := r.resolveMediaSnapshotCommitOutcome(committed)
		unlock()
		if resolveErr == nil {
			if mediaEpochChanged {
				r.materializeCommittedMediaEpoch(input.SessionID)
			}
			return resolved, nil
		}
		return MediaSnapshot{}, ErrMediaPersistence
	}
	resolved := r.resolveMediaSnapshotAfterCommit(committed)
	unlock()

	if mediaEpochChanged {
		r.materializeCommittedMediaEpoch(input.SessionID)
	}
	return resolved, nil
}

func committedMediaSnapshot(
	current MediaSnapshot,
	input PersistMediaSnapshotInput,
	validated validatedMediaSnapshotInput,
	updatedAt int64,
) MediaSnapshot {
	committed := current
	committed.Session.State = input.State
	committed.Session.ManifestRevision = input.ExpectedRevision + 1
	committed.Session.ManifestDirty = true
	committed.Session.Attempts = append([]MediaAttempt(nil), validated.attempts...)
	committed.Session.UpdatedAt = updatedAt
	if current.Session.RootID != nil {
		value := *current.Session.RootID
		committed.Session.RootID = &value
	}
	if input.MediaEpochAt != nil {
		value := *input.MediaEpochAt
		committed.Session.MediaEpochAt = &value
	} else if current.Session.MediaEpochAt != nil {
		value := *current.Session.MediaEpochAt
		committed.Session.MediaEpochAt = &value
	}
	committed.Segments = mergeCommittedMediaSegments(current.Segments, validated.segments)
	committed.Artifacts = mergeCommittedMediaArtifacts(current.Artifacts, validated.artifacts)
	return committed
}

func mergeCommittedMediaSegments(current, updates []MediaSegment) []MediaSegment {
	merged := make([]MediaSegment, len(current))
	copy(merged, current)
	indices := make(map[string]int, len(merged))
	for index := range merged {
		indices[merged[index].ID] = index
	}
	for _, update := range updates {
		if index, ok := indices[update.ID]; ok {
			merged[index] = update
			continue
		}
		indices[update.ID] = len(merged)
		merged = append(merged, update)
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Sequence == merged[j].Sequence {
			return merged[i].ID < merged[j].ID
		}
		return merged[i].Sequence < merged[j].Sequence
	})
	return merged
}

func mergeCommittedMediaArtifacts(current, updates []MediaArtifact) []MediaArtifact {
	merged := make([]MediaArtifact, len(current))
	copy(merged, current)
	indices := make(map[string]int, len(merged))
	for index := range merged {
		indices[merged[index].ID] = index
	}
	for _, update := range updates {
		if index, ok := indices[update.ID]; ok {
			// SQLite deliberately preserves the original creation time on update.
			update.CreatedAt = merged[index].CreatedAt
			merged[index] = update
			continue
		}
		indices[update.ID] = len(merged)
		merged = append(merged, update)
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].MediaSegmentID == merged[j].MediaSegmentID {
			if merged[i].Kind == merged[j].Kind {
				return merged[i].ID < merged[j].ID
			}
			return merged[i].Kind < merged[j].Kind
		}
		return merged[i].MediaSegmentID < merged[j].MediaSegmentID
	})
	return merged
}

func (r *SQLiteRepository) resolveMediaSnapshotAfterCommit(committed MediaSnapshot) MediaSnapshot {
	ctx, cancel := context.WithTimeout(context.Background(), r.outcomeTimeout)
	defer cancel()
	loaded, err := r.loadMediaSnapshotCommitted(ctx, committed.Session.SessionID)
	if err != nil || loaded.Session.ManifestRevision != committed.Session.ManifestRevision {
		return committed
	}
	return loaded
}

func (r *SQLiteRepository) resolveMediaSnapshotCommitOutcome(
	committed MediaSnapshot,
) (MediaSnapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r.outcomeTimeout)
	defer cancel()
	loaded, err := r.loadMediaSnapshotCommitted(ctx, committed.Session.SessionID)
	if err != nil || !reflect.DeepEqual(loaded, committed) {
		return MediaSnapshot{}, ErrMediaPersistence
	}
	return loaded, nil
}

func (r *SQLiteRepository) loadMediaSnapshotCommitted(
	ctx context.Context,
	sessionID string,
) (MediaSnapshot, error) {
	if r.loadMediaAfterCommit != nil {
		return r.loadMediaAfterCommit(ctx, sessionID)
	}
	return r.LoadSnapshot(ctx, sessionID)
}

func (r *SQLiteRepository) materializeCommittedMediaEpoch(sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), r.outcomeTimeout)
	defer cancel()
	liveSession, err := r.loadSession(ctx, sessionID)
	if err != nil {
		return
	}
	_, _ = r.materialize(ctx, liveSession)
}

func (r *SQLiteRepository) LoadSnapshot(ctx context.Context, sessionID string) (MediaSnapshot, error) {
	if err := requireContext(ctx); err != nil {
		return MediaSnapshot{}, err
	}
	if validateUUIDv7("session id", sessionID) != nil {
		return MediaSnapshot{}, ErrSessionMediaNotFound
	}
	var snapshot MediaSnapshot
	var rootID sql.NullString
	var dirty int
	var epoch sql.NullInt64
	var attemptsJSON string
	err := r.reader.QueryRowContext(ctx, `SELECT session_id, root_id, relative_path,
		state, manifest_revision, manifest_dirty, media_epoch_at, attempts_json,
		created_at, updated_at FROM session_media WHERE session_id = ?`, sessionID).Scan(
		&snapshot.Session.SessionID, &rootID, &snapshot.Session.RelativePath,
		&snapshot.Session.State, &snapshot.Session.ManifestRevision, &dirty, &epoch,
		&attemptsJSON, &snapshot.Session.CreatedAt, &snapshot.Session.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return MediaSnapshot{}, ErrSessionMediaNotFound
	}
	if err != nil {
		return MediaSnapshot{}, ErrMediaPersistence
	}
	if rootID.Valid {
		value := rootID.String
		snapshot.Session.RootID = &value
	}
	snapshot.Session.ManifestDirty = dirty == 1
	if epoch.Valid {
		value := epoch.Int64
		snapshot.Session.MediaEpochAt = &value
	}
	snapshot.Session.Attempts, err = decodeMediaAttempts(attemptsJSON)
	if err != nil || !validSessionMediaState(snapshot.Session.State) ||
		!validMediaRelativePath(snapshot.Session.RelativePath) {
		return MediaSnapshot{}, ErrMediaContractInvalid
	}

	snapshot.Segments, err = r.loadMediaSegments(ctx, sessionID)
	if err != nil {
		return MediaSnapshot{}, err
	}
	snapshot.Artifacts, err = r.loadMediaArtifacts(ctx, sessionID)
	if err != nil {
		return MediaSnapshot{}, err
	}
	return snapshot, nil
}

func (r *SQLiteRepository) ClearDirty(ctx context.Context, sessionID string, revision int64) (bool, error) {
	if err := requireContext(ctx); err != nil {
		return false, err
	}
	if validateUUIDv7("session id", sessionID) != nil || revision < 0 {
		return false, ErrMediaContractInvalid
	}
	result, err := r.writer.ExecContext(ctx, `UPDATE session_media SET manifest_dirty = 0
		WHERE session_id = ? AND manifest_revision = ? AND manifest_dirty = 1`, sessionID, revision)
	if err != nil {
		return false, ErrMediaPersistence
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, ErrMediaPersistence
	}
	return affected == 1, nil
}

type validatedMediaSnapshotInput struct {
	attemptsJSON []byte
	attempts     []MediaAttempt
	segments     []MediaSegment
	artifacts    []MediaArtifact
}

func validateOpenSessionMediaInput(ctx context.Context, input OpenSessionMediaInput) error {
	if err := requireContext(ctx); err != nil {
		return err
	}
	if validateUUIDv7("session id", input.SessionID) != nil || !validMediaRelativePath(input.RelativePath) || input.StartedAt < 0 {
		return ErrMediaContractInvalid
	}
	if input.RootID != nil && validateUUIDv7("recording root id", *input.RootID) != nil {
		return ErrMediaContractInvalid
	}
	return nil
}

func validatePersistMediaSnapshotInput(ctx context.Context, input PersistMediaSnapshotInput) (validatedMediaSnapshotInput, error) {
	if err := requireContext(ctx); err != nil {
		return validatedMediaSnapshotInput{}, err
	}
	if validateUUIDv7("session id", input.SessionID) != nil || input.ExpectedRevision < 0 ||
		!validSessionMediaState(input.State) || input.UpdatedAt < 0 ||
		(input.MediaEpochAt != nil && *input.MediaEpochAt < 0) ||
		len(input.Segments) > maximumMediaSegments || len(input.Artifacts) > maximumMediaArtifacts {
		return validatedMediaSnapshotInput{}, ErrMediaContractInvalid
	}
	attempts, attemptsJSON, err := normalizeMediaAttempts(input.Attempts)
	if err != nil {
		return validatedMediaSnapshotInput{}, err
	}
	if len(attempts) != len(input.Attempts) {
		return validatedMediaSnapshotInput{}, ErrMediaContractInvalid
	}
	for index := range attempts {
		if attempts[index] != input.Attempts[index] {
			return validatedMediaSnapshotInput{}, ErrMediaContractInvalid
		}
	}
	segments := append([]MediaSegment(nil), input.Segments...)
	sort.Slice(segments, func(i, j int) bool {
		if segments[i].Sequence == segments[j].Sequence {
			return segments[i].ID < segments[j].ID
		}
		return segments[i].Sequence < segments[j].Sequence
	})
	segmentIDs := make(map[string]struct{}, len(segments))
	segmentSequences := make(map[int]struct{}, len(segments))
	segmentPaths := make(map[string]struct{}, len(segments))
	sourcePaths := make(map[string]struct{}, len(segments))
	for _, segment := range segments {
		if err := validateMediaSegment(segment); err != nil {
			return validatedMediaSnapshotInput{}, err
		}
		if _, exists := segmentIDs[segment.ID]; exists {
			return validatedMediaSnapshotInput{}, ErrMediaContractInvalid
		}
		if _, exists := segmentSequences[segment.Sequence]; exists {
			return validatedMediaSnapshotInput{}, ErrMediaContractInvalid
		}
		pathKey := mediaPlatformRelativePathKey(segment.RelativePath)
		if _, exists := segmentPaths[pathKey]; exists {
			return validatedMediaSnapshotInput{}, ErrMediaContractInvalid
		}
		sourceKey := mediaPlatformRelativePathKey(segment.SourceRelativePath)
		if _, exists := sourcePaths[sourceKey]; exists {
			return validatedMediaSnapshotInput{}, ErrMediaContractInvalid
		}
		segmentIDs[segment.ID] = struct{}{}
		segmentSequences[segment.Sequence] = struct{}{}
		segmentPaths[pathKey] = struct{}{}
		sourcePaths[sourceKey] = struct{}{}
	}
	artifacts := append([]MediaArtifact(nil), input.Artifacts...)
	sort.Slice(artifacts, func(i, j int) bool {
		if artifacts[i].MediaSegmentID == artifacts[j].MediaSegmentID {
			if artifacts[i].Kind == artifacts[j].Kind {
				return artifacts[i].ID < artifacts[j].ID
			}
			return artifacts[i].Kind < artifacts[j].Kind
		}
		return artifacts[i].MediaSegmentID < artifacts[j].MediaSegmentID
	})
	artifactIDs := make(map[string]struct{}, len(artifacts))
	artifactKinds := make(map[string]struct{}, len(artifacts))
	artifactPaths := make(map[string]struct{}, len(artifacts))
	for _, artifact := range artifacts {
		if err := validateMediaArtifact(artifact); err != nil {
			return validatedMediaSnapshotInput{}, err
		}
		if _, exists := artifactIDs[artifact.ID]; exists {
			return validatedMediaSnapshotInput{}, ErrMediaContractInvalid
		}
		key := artifact.MediaSegmentID + "\x00" + string(artifact.Kind)
		if _, exists := artifactKinds[key]; exists {
			return validatedMediaSnapshotInput{}, ErrMediaContractInvalid
		}
		pathKey := mediaPlatformRelativePathKey(artifact.RelativePath)
		if _, exists := artifactPaths[pathKey]; exists {
			return validatedMediaSnapshotInput{}, ErrMediaContractInvalid
		}
		artifactIDs[artifact.ID] = struct{}{}
		artifactKinds[key] = struct{}{}
		artifactPaths[pathKey] = struct{}{}
	}
	return validatedMediaSnapshotInput{
		attemptsJSON: attemptsJSON, attempts: attempts, segments: segments, artifacts: artifacts,
	}, nil
}

func validateMediaSegment(segment MediaSegment) error {
	if validateUUIDv7("media segment id", segment.ID) != nil || segment.Sequence < 1 ||
		!validMediaRelativePath(segment.RelativePath) || !validMediaRelativePath(segment.SourceRelativePath) ||
		!validMediaSafeToken(segment.Container, false) || !validMediaSafeToken(segment.VideoCodec, true) ||
		!validMediaSafeToken(segment.AudioCodec, true) || segment.StartedAt < 0 || segment.EndedAt < segment.StartedAt ||
		segment.DurationMS < 0 || segment.SizeBytes < 0 || !validMediaDigest(segment.SHA256) ||
		!validMediaSegmentStatus(segment.Status) || validateUUIDv7("media attempt id", segment.AttemptID) != nil ||
		segment.AttemptSequence < 1 || !validMediaSafeToken(segment.ProbeVersion, true) ||
		!validMediaErrorCode(segment.ErrorCode) {
		return ErrMediaContractInvalid
	}
	if segment.PTSStartMS != nil && *segment.PTSStartMS < 0 || segment.PTSEndMS != nil && *segment.PTSEndMS < 0 {
		return ErrMediaContractInvalid
	}
	return nil
}

func validateMediaArtifact(artifact MediaArtifact) error {
	if validateUUIDv7("media artifact id", artifact.ID) != nil ||
		validateUUIDv7("media segment id", artifact.MediaSegmentID) != nil ||
		!validMediaArtifactKind(artifact.Kind) || !validMediaRelativePath(artifact.RelativePath) ||
		!validMediaSafeToken(artifact.Container, true) || !validMediaSafeToken(artifact.Codec, true) ||
		artifact.DurationMS < 0 || artifact.SizeBytes < 0 || artifact.SampleRate < 0 || artifact.Channels < 0 ||
		!validMediaDigest(artifact.SHA256) || !validMediaDigest(artifact.SourceSHA256) ||
		!validMediaArtifactStatus(artifact.Status) || !validMediaErrorCode(artifact.ErrorCode) ||
		artifact.CreatedAt < 0 || artifact.UpdatedAt < artifact.CreatedAt {
		return ErrMediaContractInvalid
	}
	return nil
}

func upsertMediaSegment(ctx context.Context, tx *sql.Tx, sessionID string, segment MediaSegment) error {
	result, err := tx.ExecContext(ctx, `INSERT INTO media_segments(
		id, session_id, sequence, relative_path, container, video_codec, audio_codec,
		started_at, ended_at, pts_start_ms, pts_end_ms, duration_ms, size_bytes,
		sha256, status, attempt_id, attempt_sequence, source_relative_path,
		probe_version, error_code
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		container = excluded.container, video_codec = excluded.video_codec,
		audio_codec = excluded.audio_codec, started_at = excluded.started_at,
		ended_at = excluded.ended_at, pts_start_ms = excluded.pts_start_ms,
		pts_end_ms = excluded.pts_end_ms, duration_ms = excluded.duration_ms,
		size_bytes = excluded.size_bytes, sha256 = excluded.sha256,
		status = excluded.status, probe_version = excluded.probe_version,
		error_code = excluded.error_code
	WHERE media_segments.session_id = excluded.session_id
		AND media_segments.sequence = excluded.sequence
		AND media_segments.relative_path = excluded.relative_path
		AND media_segments.attempt_id = excluded.attempt_id
		AND media_segments.attempt_sequence = excluded.attempt_sequence
		AND media_segments.source_relative_path = excluded.source_relative_path`,
		segment.ID, sessionID, segment.Sequence, segment.RelativePath, segment.Container,
		nullableText(segment.VideoCodec), nullableText(segment.AudioCodec), segment.StartedAt,
		segment.EndedAt, nullableInt64(segment.PTSStartMS), nullableInt64(segment.PTSEndMS),
		segment.DurationMS, segment.SizeBytes, nullableText(segment.SHA256), segment.Status,
		segment.AttemptID, segment.AttemptSequence, segment.SourceRelativePath,
		segment.ProbeVersion, nullableText(segment.ErrorCode))
	if err != nil {
		return ErrMediaSnapshotConflict
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return ErrMediaSnapshotConflict
	}
	return nil
}

func upsertMediaArtifact(ctx context.Context, tx *sql.Tx, sessionID string, artifact MediaArtifact) error {
	result, err := tx.ExecContext(ctx, `INSERT INTO media_artifacts(
		id, session_id, media_segment_id, kind, relative_path, container, codec,
		duration_ms, size_bytes, sample_rate, channels, sha256, source_sha256,
		status, error_code, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		container = excluded.container, codec = excluded.codec,
		duration_ms = excluded.duration_ms, size_bytes = excluded.size_bytes,
		sample_rate = excluded.sample_rate, channels = excluded.channels,
		sha256 = excluded.sha256, source_sha256 = excluded.source_sha256,
		status = excluded.status, error_code = excluded.error_code,
		updated_at = excluded.updated_at
	WHERE media_artifacts.session_id = excluded.session_id
		AND media_artifacts.media_segment_id = excluded.media_segment_id
		AND media_artifacts.kind = excluded.kind
		AND media_artifacts.relative_path = excluded.relative_path`,
		artifact.ID, sessionID, artifact.MediaSegmentID, artifact.Kind, artifact.RelativePath,
		artifact.Container, artifact.Codec, artifact.DurationMS, artifact.SizeBytes,
		artifact.SampleRate, artifact.Channels, artifact.SHA256, artifact.SourceSHA256,
		artifact.Status, nullableText(artifact.ErrorCode), artifact.CreatedAt, artifact.UpdatedAt)
	if err != nil {
		return ErrMediaSnapshotConflict
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return ErrMediaSnapshotConflict
	}
	return nil
}

// validateDurableMediaCardinality observes the transaction after all upserts,
// so COUNT(*) is the distinct durable union of existing IDs and this CAS input.
// Any failure rolls the transaction back before its revision becomes visible.
func validateDurableMediaCardinality(ctx context.Context, tx *sql.Tx, sessionID string) error {
	if ctx == nil || tx == nil {
		return ErrMediaPersistence
	}
	var segmentCount, artifactCount int64
	err := tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM media_segments WHERE session_id = ?),
		(SELECT COUNT(*) FROM media_artifacts WHERE session_id = ?)`,
		sessionID, sessionID).Scan(&segmentCount, &artifactCount)
	if err != nil {
		return ErrMediaPersistence
	}
	if segmentCount > maximumMediaSegments || artifactCount > maximumMediaArtifacts {
		return ErrMediaContractInvalid
	}
	return nil
}

func (r *SQLiteRepository) loadMediaSegments(ctx context.Context, sessionID string) ([]MediaSegment, error) {
	rows, err := r.reader.QueryContext(ctx, `SELECT id, sequence, relative_path, container,
		video_codec, audio_codec, started_at, ended_at, pts_start_ms, pts_end_ms,
		duration_ms, size_bytes, sha256, status, attempt_id, attempt_sequence,
		source_relative_path, probe_version, error_code
		FROM media_segments WHERE session_id = ? ORDER BY sequence ASC, id ASC
		LIMIT ?`, sessionID, maximumMediaSegments+1)
	if err != nil {
		return nil, ErrMediaPersistence
	}
	defer rows.Close()
	segments := make([]MediaSegment, 0, maximumMediaSegments+1)
	for rows.Next() {
		var segment MediaSegment
		var video, audio, digest, errorCode sql.NullString
		var ptsStart, ptsEnd sql.NullInt64
		var attemptSequence sql.NullInt64
		if err := rows.Scan(&segment.ID, &segment.Sequence, &segment.RelativePath, &segment.Container,
			&video, &audio, &segment.StartedAt, &segment.EndedAt, &ptsStart, &ptsEnd,
			&segment.DurationMS, &segment.SizeBytes, &digest, &segment.Status, &segment.AttemptID,
			&attemptSequence, &segment.SourceRelativePath, &segment.ProbeVersion, &errorCode); err != nil {
			return nil, ErrMediaPersistence
		}
		segment.VideoCodec, segment.AudioCodec, segment.SHA256, segment.ErrorCode = video.String, audio.String, digest.String, errorCode.String
		if ptsStart.Valid {
			value := ptsStart.Int64
			segment.PTSStartMS = &value
		}
		if ptsEnd.Valid {
			value := ptsEnd.Int64
			segment.PTSEndMS = &value
		}
		if attemptSequence.Valid {
			segment.AttemptSequence = int(attemptSequence.Int64)
		}
		if err := validateMediaSegment(segment); err != nil {
			return nil, ErrMediaContractInvalid
		}
		segments = append(segments, segment)
		if len(segments) > maximumMediaSegments {
			return nil, ErrMediaContractInvalid
		}
	}
	if rows.Err() != nil {
		return nil, ErrMediaPersistence
	}
	return segments, nil
}

func (r *SQLiteRepository) loadMediaArtifacts(ctx context.Context, sessionID string) ([]MediaArtifact, error) {
	rows, err := r.reader.QueryContext(ctx, `SELECT id, media_segment_id, kind, relative_path,
		container, codec, duration_ms, size_bytes, sample_rate, channels, sha256,
		source_sha256, status, error_code, created_at, updated_at
		FROM media_artifacts WHERE session_id = ?
		ORDER BY media_segment_id ASC, kind ASC, id ASC
		LIMIT ?`, sessionID, maximumMediaArtifacts+1)
	if err != nil {
		return nil, ErrMediaPersistence
	}
	defer rows.Close()
	artifacts := make([]MediaArtifact, 0, maximumMediaArtifacts+1)
	for rows.Next() {
		var artifact MediaArtifact
		var errorCode sql.NullString
		if err := rows.Scan(&artifact.ID, &artifact.MediaSegmentID, &artifact.Kind,
			&artifact.RelativePath, &artifact.Container, &artifact.Codec, &artifact.DurationMS,
			&artifact.SizeBytes, &artifact.SampleRate, &artifact.Channels, &artifact.SHA256,
			&artifact.SourceSHA256, &artifact.Status, &errorCode, &artifact.CreatedAt,
			&artifact.UpdatedAt); err != nil {
			return nil, ErrMediaPersistence
		}
		artifact.ErrorCode = errorCode.String
		if err := validateMediaArtifact(artifact); err != nil {
			return nil, ErrMediaContractInvalid
		}
		artifacts = append(artifacts, artifact)
		if len(artifacts) > maximumMediaArtifacts {
			return nil, ErrMediaContractInvalid
		}
	}
	if rows.Err() != nil {
		return nil, ErrMediaPersistence
	}
	return artifacts, nil
}

func nullableRootID(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func sameOptionalString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
