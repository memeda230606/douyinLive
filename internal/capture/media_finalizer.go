package capture

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

var ErrMediaFinalize = errors.New("MEDIA_FINALIZE_FAILED")

type sessionMediaFinalizerOptions struct {
	Repository    *SQLiteRepository
	Tools         ffmpegTools
	Root          string
	RootID        *string
	SessionID     string
	RelativePath  string
	StartedAt     int64
	ProxyCapacity chan struct{}
	Prober        *ffprobeSegmentProber
	Dependencies  mediaFinalizerDependencies
}

func (sessionMediaFinalizerOptions) String() string {
	return "sessionMediaFinalizerOptions{paths:<redacted> identifiers:<redacted>}"
}

func (options sessionMediaFinalizerOptions) GoString() string { return options.String() }

func (options sessionMediaFinalizerOptions) LogValue() slog.Value {
	return slog.StringValue(options.String())
}

type mediaFinalizerDependencies struct {
	now              func() time.Time
	newID            func() (string, error)
	generateASR      func(context.Context, string, *mediaArtifactSource, string, mediaArtifactVerifyFunc) error
	generatePlayback func(context.Context, string, *mediaArtifactSource, string, mediaArtifactVerifyFunc) error
	inspectArtifact  func(context.Context, string, string, MediaArtifactKind) error
	hashFile         func(context.Context, string) (int64, string, error)
}

func defaultMediaFinalizerDependencies() mediaFinalizerDependencies {
	return mediaFinalizerDependencies{
		now: time.Now,
		newID: func() (string, error) {
			id, err := uuid.NewV7()
			return id.String(), err
		},
		generateASR:      generateASRAudio,
		generatePlayback: generatePlaybackMP4,
		inspectArtifact:  inspectMediaArtifact,
		hashFile:         hashMediaFile,
	}
}

func normalizeMediaFinalizerDependencies(dependencies mediaFinalizerDependencies) mediaFinalizerDependencies {
	defaults := defaultMediaFinalizerDependencies()
	if dependencies.now == nil {
		dependencies.now = defaults.now
	}
	if dependencies.newID == nil {
		dependencies.newID = defaults.newID
	}
	if dependencies.generateASR == nil {
		dependencies.generateASR = defaults.generateASR
	}
	if dependencies.generatePlayback == nil {
		dependencies.generatePlayback = defaults.generatePlayback
	}
	if dependencies.inspectArtifact == nil {
		dependencies.inspectArtifact = defaults.inspectArtifact
	}
	if dependencies.hashFile == nil {
		dependencies.hashFile = defaults.hashFile
	}
	return dependencies
}

type sqliteSessionMediaFinalizer struct {
	operationMu sync.Mutex

	repository       *SQLiteRepository
	tools            ffmpegTools
	root             string
	rootID           *string
	sessionDirectory string
	sessionID        string
	relativePath     string
	proxyCapacity    chan struct{}
	prober           *ffprobeSegmentProber
	dependencies     mediaFinalizerDependencies
}

func newSQLiteSessionMediaFinalizer(
	ctx context.Context,
	options sessionMediaFinalizerOptions,
) (*sqliteSessionMediaFinalizer, error) {
	if ctx == nil || options.Repository == nil ||
		validateUUIDv7("media finalizer session", options.SessionID) != nil ||
		!validMediaRelativePath(options.RelativePath) || options.StartedAt < 0 ||
		!validMediaExecutable(options.Tools.ffmpegPath) {
		return nil, ErrMediaFinalize
	}
	if options.RootID != nil && validateUUIDv7("media finalizer root", *options.RootID) != nil {
		return nil, ErrMediaFinalize
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := options.Repository.resolveSessionMediaRoot(ctx, options.Root, options.RootID)
	if err != nil {
		return nil, ErrMediaFinalize
	}
	sessionDirectory, err := secureMediaSessionDirectory(root, options.RelativePath)
	if err != nil {
		return nil, ErrMediaFinalize
	}
	prober := options.Prober
	if prober == nil {
		prober, err = newFFprobeSegmentProber(options.Tools)
		if err != nil {
			return nil, ErrMediaFinalize
		}
	}
	proxyCapacity := options.ProxyCapacity
	if proxyCapacity == nil {
		proxyCapacity = make(chan struct{}, 1)
	}
	if cap(proxyCapacity) < 1 {
		return nil, ErrMediaFinalize
	}
	if _, err := options.Repository.OpenSessionMedia(ctx, OpenSessionMediaInput{
		SessionID: options.SessionID, RootID: options.RootID,
		RelativePath: options.RelativePath, StartedAt: options.StartedAt,
	}); err != nil {
		return nil, err
	}
	if _, err := options.Repository.materializeMediaManifest(ctx, root, options.SessionID); err != nil {
		return nil, err
	}
	var rootID *string
	if options.RootID != nil {
		value := *options.RootID
		rootID = &value
	}
	return &sqliteSessionMediaFinalizer{
		repository: options.Repository, tools: options.Tools, root: root, rootID: rootID,
		sessionDirectory: sessionDirectory, sessionID: options.SessionID,
		relativePath: options.RelativePath, proxyCapacity: proxyCapacity,
		prober: prober, dependencies: normalizeMediaFinalizerDependencies(options.Dependencies),
	}, nil
}

func (r *SQLiteRepository) resolveSessionMediaRoot(
	ctx context.Context,
	root string,
	rootID *string,
) (string, error) {
	if r == nil || ctx == nil {
		return "", ErrMediaFinalize
	}
	canonicalRoot, err := canonicalMediaRoot(root)
	if err != nil {
		return "", ErrMediaFinalize
	}
	if rootID == nil {
		canonicalDataRoot, err := canonicalMediaRoot(r.dataRoot)
		if err != nil || !sameRecorderDirectory(canonicalRoot, canonicalDataRoot) {
			return "", ErrMediaFinalize
		}
		return canonicalDataRoot, nil
	}
	if validateUUIDv7("session media root", *rootID) != nil {
		return "", ErrMediaFinalize
	}
	registered, err := r.verifyRecordingRootBinding(ctx, canonicalRoot, *rootID)
	if err != nil || registered.ID != *rootID ||
		!sameRecorderDirectory(registered.absolutePath, canonicalRoot) {
		return "", ErrMediaFinalize
	}
	return registered.absolutePath, nil
}

func (finalizer *sqliteSessionMediaFinalizer) String() string {
	if finalizer == nil {
		return "sqliteSessionMediaFinalizer{state:nil}"
	}
	return "sqliteSessionMediaFinalizer{state:ready paths:<redacted>}"
}

func (finalizer *sqliteSessionMediaFinalizer) GoString() string { return finalizer.String() }

func (finalizer *sqliteSessionMediaFinalizer) LogValue() slog.Value {
	return slog.StringValue(finalizer.String())
}

// verifyBoundRoot re-establishes the durable root binding immediately before
// each phase that can observe or mutate media. A finalizer may live for hours,
// so constructor-time verification alone is not a safety boundary.
func (finalizer *sqliteSessionMediaFinalizer) verifyBoundRoot(ctx context.Context) error {
	if finalizer == nil || ctx == nil || finalizer.repository == nil {
		return ErrMediaFinalize
	}
	root, err := finalizer.repository.resolveSessionMediaRoot(ctx, finalizer.root, finalizer.rootID)
	if err != nil || !sameRecorderDirectory(root, finalizer.root) {
		return ErrMediaFinalize
	}
	directory, err := secureMediaSessionDirectory(root, finalizer.relativePath)
	if err != nil || !sameRecorderDirectory(directory, finalizer.sessionDirectory) {
		return ErrMediaFinalize
	}
	return nil
}

func (finalizer *sqliteSessionMediaFinalizer) Finalize(
	ctx context.Context,
	attempts []MediaAttempt,
) (MediaFinalizeResult, error) {
	if finalizer == nil || ctx == nil {
		return MediaFinalizeResult{}, ErrMediaFinalize
	}
	if err := ctx.Err(); err != nil {
		return MediaFinalizeResult{}, err
	}
	incomingAttempts, _, err := normalizeMediaAttempts(attempts)
	if err != nil || len(incomingAttempts) > maximumMediaAttempts {
		return MediaFinalizeResult{}, ErrMediaFinalize
	}
	finalizer.operationMu.Lock()
	defer finalizer.operationMu.Unlock()
	if err := finalizer.verifyBoundRoot(ctx); err != nil {
		return MediaFinalizeResult{}, err
	}

	current, err := finalizer.repository.LoadSnapshot(ctx, finalizer.sessionID)
	if err != nil {
		return MediaFinalizeResult{}, err
	}
	normalizedAttempts, err := mergeMediaAttemptJournal(current.Session.Attempts, incomingAttempts)
	if err != nil {
		return MediaFinalizeResult{}, err
	}
	auditedCurrentSegments, initialSegmentWarnings, err := finalizer.auditMediaSegments(
		ctx, current.Segments,
	)
	if err != nil {
		return MediaFinalizeResult{}, err
	}
	auditedCurrentArtifacts, initialArtifactWarnings, err := finalizer.auditMediaArtifacts(
		ctx, current.Artifacts,
	)
	if err != nil {
		return MediaFinalizeResult{}, err
	}
	snapshotIntact := reflect.DeepEqual(auditedCurrentSegments, current.Segments) &&
		reflect.DeepEqual(auditedCurrentArtifacts, current.Artifacts)
	current.Segments = auditedCurrentSegments
	current.Artifacts = auditedCurrentArtifacts
	warnings := append([]string(nil), initialSegmentWarnings...)
	warnings = append(warnings, initialArtifactWarnings...)
	if current.Session.State == SessionMediaCompleted &&
		reflect.DeepEqual(current.Session.Attempts, normalizedAttempts) &&
		!mediaSnapshotHasRetryableArtifacts(current) && snapshotIntact {
		materialized, materializeErr := finalizer.repository.materializeMediaManifest(
			ctx, finalizer.root, finalizer.sessionID,
		)
		return MediaFinalizeResult{Snapshot: materialized}, materializeErr
	}
	candidates, err := discoverMediaCandidates(
		finalizer.sessionDirectory, finalizer.relativePath, normalizedAttempts,
	)
	if err != nil {
		return MediaFinalizeResult{}, err
	}
	if err := finalizer.verifyBoundRoot(ctx); err != nil {
		return MediaFinalizeResult{}, err
	}
	processedSegments := make([]MediaSegment, 0, len(candidates)+len(current.Segments))
	matchedSegmentIDs := make(map[string]struct{}, len(candidates))
	processor := mediaSegmentProcessor{
		prober: finalizer.prober,
		newID:  finalizer.dependencies.newID,
		verify: finalizer.verifyBoundRoot,
	}
	for _, candidate := range candidates {
		existing, findErr := findExistingMediaSegment(current.Segments, candidate)
		if findErr != nil {
			return MediaFinalizeResult{}, findErr
		}
		if existing != nil {
			matchedSegmentIDs[existing.ID] = struct{}{}
		}
		segment, segmentWarnings, finalizeErr := processor.finalize(ctx, candidate, existing)
		if finalizeErr != nil {
			return MediaFinalizeResult{}, finalizeErr
		}
		processedSegments = append(processedSegments, segment)
		warnings = append(warnings, segmentWarnings...)
	}
	missingSegments, missingWarnings := reconcileMissingVerifiedMediaSegments(
		current.Segments, matchedSegmentIDs,
	)
	processedSegments = append(processedSegments, missingSegments...)
	warnings = append(warnings, missingWarnings...)
	mergedSegments := mergeMediaSegments(current.Segments, processedSegments)
	plannedArtifacts, err := finalizer.planArtifacts(ctx, current.Artifacts, mergedSegments)
	if err != nil {
		return MediaFinalizeResult{}, err
	}
	for _, artifact := range plannedArtifacts {
		if artifact.ErrorCode == "MEDIA_ARTIFACT_CHANGED" || artifact.ErrorCode == "MEDIA_ARTIFACT_CONFLICT" {
			warnings = append(warnings, artifact.ErrorCode)
		}
	}
	if len(mergedSegments) == 0 {
		warnings = append(warnings, "MEDIA_SEGMENTS_MISSING")
	}
	mediaEpoch := mediaEpochFromSegments(mergedSegments)
	if err := finalizer.verifyBoundRoot(ctx); err != nil {
		return MediaFinalizeResult{}, err
	}
	coreSnapshot, err := finalizer.repository.PersistMediaSnapshot(ctx, PersistMediaSnapshotInput{
		SessionID: finalizer.sessionID, ExpectedRevision: current.Session.ManifestRevision,
		State: SessionMediaFinalizing, MediaEpochAt: mediaEpoch, Attempts: normalizedAttempts,
		Segments: processedSegments, Artifacts: plannedArtifacts,
		UpdatedAt: finalizer.dependencies.now().UTC().UnixMilli(),
	})
	if err != nil {
		return MediaFinalizeResult{}, err
	}
	coreSnapshot, err = finalizer.repository.materializeMediaManifest(ctx, finalizer.root, finalizer.sessionID)
	if err != nil {
		return MediaFinalizeResult{Snapshot: coreSnapshot, WarningCodes: stableMediaWarnings(warnings)}, err
	}

	updatedArtifacts, artifactWarnings, err := finalizer.generatePendingArtifacts(ctx, coreSnapshot)
	warnings = append(warnings, artifactWarnings...)
	if err != nil {
		return MediaFinalizeResult{Snapshot: coreSnapshot, WarningCodes: stableMediaWarnings(warnings)}, err
	}
	if err := finalizer.verifyBoundRoot(ctx); err != nil {
		return MediaFinalizeResult{Snapshot: coreSnapshot, WarningCodes: stableMediaWarnings(warnings)}, err
	}
	auditedSegments, segmentAuditWarnings, err := finalizer.auditMediaSegments(ctx, coreSnapshot.Segments)
	warnings = append(warnings, segmentAuditWarnings...)
	if err != nil {
		return MediaFinalizeResult{Snapshot: coreSnapshot, WarningCodes: stableMediaWarnings(warnings)}, err
	}
	auditedArtifacts, artifactAuditWarnings, err := finalizer.auditMediaArtifacts(ctx, updatedArtifacts)
	warnings = append(warnings, artifactAuditWarnings...)
	if err != nil {
		return MediaFinalizeResult{Snapshot: coreSnapshot, WarningCodes: stableMediaWarnings(warnings)}, err
	}
	state := completedMediaState(auditedSegments)
	if err := finalizer.verifyBoundRoot(ctx); err != nil {
		return MediaFinalizeResult{Snapshot: coreSnapshot, WarningCodes: stableMediaWarnings(warnings)}, err
	}
	completedSnapshot, err := finalizer.repository.PersistMediaSnapshot(ctx, PersistMediaSnapshotInput{
		SessionID: finalizer.sessionID, ExpectedRevision: coreSnapshot.Session.ManifestRevision,
		State: state, Attempts: normalizedAttempts, Segments: auditedSegments, Artifacts: auditedArtifacts,
		UpdatedAt: finalizer.dependencies.now().UTC().UnixMilli(),
	})
	if err != nil {
		return MediaFinalizeResult{Snapshot: coreSnapshot, WarningCodes: stableMediaWarnings(warnings)}, err
	}
	completedSnapshot, err = finalizer.repository.materializeMediaManifest(ctx, finalizer.root, finalizer.sessionID)
	return MediaFinalizeResult{
		Snapshot: completedSnapshot, WarningCodes: stableMediaWarnings(warnings),
	}, err
}

func mergeMediaAttemptJournal(durable, incoming []MediaAttempt) ([]MediaAttempt, error) {
	durableAttempts, _, err := normalizeMediaAttempts(durable)
	if err != nil {
		return nil, ErrMediaContractInvalid
	}
	incomingAttempts, _, err := normalizeMediaAttempts(incoming)
	if err != nil {
		return nil, ErrMediaContractInvalid
	}
	merged := append([]MediaAttempt(nil), durableAttempts...)
	for _, attempt := range durableAttempts {
		if attempt.Clean && !attempt.Committed {
			return nil, ErrMediaContractInvalid
		}
	}
	for _, attempt := range incomingAttempts {
		if attempt.Clean && !attempt.Committed {
			return nil, ErrMediaContractInvalid
		}
		matched := -1
		for index, existing := range merged {
			if existing.ID != attempt.ID && existing.Ordinal != attempt.Ordinal {
				continue
			}
			if existing.ID != attempt.ID || existing.Ordinal != attempt.Ordinal || matched >= 0 {
				return nil, ErrMediaSnapshotConflict
			}
			matched = index
		}
		if matched < 0 {
			merged = append(merged, attempt)
			continue
		}
		existing := merged[matched]
		if !sameMediaAttemptIdentity(existing, attempt) ||
			existing.Committed && !attempt.Committed || existing.Clean && !attempt.Clean {
			return nil, ErrMediaSnapshotConflict
		}
		merged[matched] = attempt
	}
	canonical, _, err := normalizeMediaAttempts(merged)
	if err != nil {
		return nil, ErrMediaContractInvalid
	}
	return canonical, nil
}

func findExistingMediaSegment(segments []MediaSegment, candidate mediaCandidate) (*MediaSegment, error) {
	var matched *MediaSegment
	for index := range segments {
		segment := &segments[index]
		if segment.Sequence != candidate.Sequence && segment.SourceRelativePath != candidate.SourceRelativePath {
			continue
		}
		if segment.Sequence != candidate.Sequence || segment.SourceRelativePath != candidate.SourceRelativePath ||
			segment.AttemptID != candidate.Attempt.ID || segment.AttemptSequence != candidate.AttemptSequence {
			return nil, ErrMediaSnapshotConflict
		}
		if matched != nil {
			return nil, ErrMediaSnapshotConflict
		}
		matched = segment
	}
	return matched, nil
}

func mergeMediaSegments(existing, updates []MediaSegment) []MediaSegment {
	merged := make(map[string]MediaSegment, len(existing)+len(updates))
	for _, segment := range existing {
		merged[segment.ID] = segment
	}
	for _, segment := range updates {
		merged[segment.ID] = segment
	}
	result := make([]MediaSegment, 0, len(merged))
	for _, segment := range merged {
		result = append(result, segment)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Sequence < result[right].Sequence })
	return result
}

func reconcileMissingVerifiedMediaSegments(
	existing []MediaSegment,
	matchedIDs map[string]struct{},
) ([]MediaSegment, []string) {
	updates := make([]MediaSegment, 0)
	warnings := make([]string, 0)
	for _, segment := range existing {
		if _, matched := matchedIDs[segment.ID]; matched {
			continue
		}
		if segment.Status != MediaSegmentComplete && segment.Status != MediaSegmentRecovered {
			continue
		}
		updates = append(updates, missingVerifiedMediaSegment(segment))
		warnings = append(warnings, "MEDIA_FINAL_MISSING")
	}
	return updates, warnings
}

func (finalizer *sqliteSessionMediaFinalizer) auditMediaSegments(
	ctx context.Context,
	segments []MediaSegment,
) ([]MediaSegment, []string, error) {
	audited := append([]MediaSegment(nil), segments...)
	warnings := make([]string, 0)
	for index := range audited {
		segment := &audited[index]
		if segment.Status != MediaSegmentComplete && segment.Status != MediaSegmentRecovered {
			continue
		}
		absolutePath, err := mediaAbsolutePath(finalizer.root, segment.RelativePath)
		if err != nil {
			return nil, warnings, ErrMediaFinalize
		}
		size, digest, evidenceErr := stableMediaFileEvidence(ctx, absolutePath)
		switch {
		case errors.Is(evidenceErr, os.ErrNotExist):
			*segment = missingVerifiedMediaSegment(*segment)
			warnings = append(warnings, "MEDIA_FINAL_MISSING")
		case errors.Is(evidenceErr, errMediaFileChanged):
			*segment = changedVerifiedMediaSegment(*segment)
			warnings = append(warnings, "MEDIA_FINAL_CHANGED")
		case evidenceErr != nil:
			if contextErr := ctx.Err(); contextErr != nil {
				return nil, warnings, contextErr
			}
			return nil, warnings, ErrMediaFinalize
		case size != segment.SizeBytes || digest != segment.SHA256:
			*segment = changedVerifiedMediaSegment(*segment)
			warnings = append(warnings, "MEDIA_FINAL_CHANGED")
		}
	}
	return audited, warnings, nil
}

func (finalizer *sqliteSessionMediaFinalizer) auditMediaArtifacts(
	ctx context.Context,
	artifacts []MediaArtifact,
) ([]MediaArtifact, []string, error) {
	audited := append([]MediaArtifact(nil), artifacts...)
	warnings := make([]string, 0)
	for index := range audited {
		artifact := &audited[index]
		if artifact.Status != MediaArtifactComplete {
			continue
		}
		absolutePath, err := mediaAbsolutePath(finalizer.root, artifact.RelativePath)
		if err != nil {
			return nil, warnings, ErrMediaFinalize
		}
		size, digest, evidenceErr := stableMediaFileEvidence(ctx, absolutePath)
		switch {
		case errors.Is(evidenceErr, os.ErrNotExist):
			artifact.Status = MediaArtifactMissing
			artifact.ErrorCode = "MEDIA_ARTIFACT_MISSING"
			warnings = append(warnings, artifact.ErrorCode)
		case errors.Is(evidenceErr, errMediaFileChanged):
			artifact.Status = MediaArtifactFailed
			artifact.ErrorCode = "MEDIA_ARTIFACT_CHANGED"
			warnings = append(warnings, artifact.ErrorCode)
		case evidenceErr != nil:
			if contextErr := ctx.Err(); contextErr != nil {
				return nil, warnings, contextErr
			}
			return nil, warnings, ErrMediaFinalize
		case size != artifact.SizeBytes || digest != artifact.SHA256:
			artifact.Status = MediaArtifactFailed
			artifact.ErrorCode = "MEDIA_ARTIFACT_CHANGED"
			warnings = append(warnings, artifact.ErrorCode)
		}
	}
	return audited, warnings, nil
}

func (finalizer *sqliteSessionMediaFinalizer) planArtifacts(
	ctx context.Context,
	existing []MediaArtifact,
	segments []MediaSegment,
) ([]MediaArtifact, error) {
	byKey := make(map[string]MediaArtifact, len(existing))
	for _, artifact := range existing {
		byKey[mediaArtifactKey(artifact.MediaSegmentID, artifact.Kind)] = artifact
	}
	planned := make([]MediaArtifact, 0, len(segments)*2)
	for _, segment := range segments {
		if segment.Status != MediaSegmentComplete && segment.Status != MediaSegmentRecovered {
			continue
		}
		for _, kind := range []MediaArtifactKind{MediaArtifactASRWAV, MediaArtifactPlaybackMP4} {
			existingArtifact, exists := byKey[mediaArtifactKey(segment.ID, kind)]
			artifact, err := finalizer.planArtifact(ctx, segment, kind, existingArtifact, exists)
			if err != nil {
				return nil, err
			}
			planned = append(planned, artifact)
		}
	}
	return planned, nil
}

func (finalizer *sqliteSessionMediaFinalizer) planArtifact(
	ctx context.Context,
	segment MediaSegment,
	kind MediaArtifactKind,
	existing MediaArtifact,
	hasExisting bool,
) (MediaArtifact, error) {
	id := existing.ID
	createdAt := existing.CreatedAt
	now := finalizer.dependencies.now().UTC().UnixMilli()
	if !hasExisting {
		var err error
		id, err = finalizer.dependencies.newID()
		if err != nil || validateUUIDv7("media artifact", id) != nil {
			return MediaArtifact{}, ErrMediaFinalize
		}
		createdAt = now
	}
	name := fmt.Sprintf("asr-%06d.wav", segment.Sequence)
	directory := "audio"
	container := "wav"
	codec := "pcm_s16le"
	status := MediaArtifactPending
	errorCode := ""
	if kind == MediaArtifactPlaybackMP4 {
		name = fmt.Sprintf("playback-%06d.mp4", segment.Sequence)
		directory, container, codec = "media", "mp4", segment.VideoCodec
		if segment.VideoCodec == "" {
			status, errorCode = MediaArtifactNotApplicable, "MEDIA_VIDEO_STREAM_MISSING"
		} else if !playbackCopyCompatible(segment.VideoCodec, segment.AudioCodec) {
			status, errorCode = MediaArtifactPendingTranscode, "MEDIA_TRANSCODE_REQUIRED"
		}
	} else if segment.AudioCodec == "" {
		status, errorCode = MediaArtifactNotApplicable, "MEDIA_AUDIO_STREAM_MISSING"
	}
	relativePath, err := joinMediaRelativePath(finalizer.relativePath, directory, name)
	if err != nil {
		return MediaArtifact{}, ErrMediaFinalize
	}
	planned := MediaArtifact{
		ID: id, MediaSegmentID: segment.ID, Kind: kind, RelativePath: relativePath,
		Container: container, Codec: codec, DurationMS: segment.DurationMS,
		SourceSHA256: segment.SHA256, Status: status, ErrorCode: errorCode,
		CreatedAt: createdAt, UpdatedAt: now,
	}
	sameBinding := hasExisting && existing.SourceSHA256 == segment.SHA256 &&
		existing.RelativePath == relativePath
	if sameBinding && terminalMediaArtifactFailure(existing) {
		return existing, nil
	}
	if sameBinding && (existing.Status == MediaArtifactComplete ||
		existing.Status == MediaArtifactMissing && existing.SizeBytes > 0 && existing.SHA256 != "") {
		absolutePath, pathErr := mediaAbsolutePath(finalizer.root, relativePath)
		if pathErr != nil {
			return MediaArtifact{}, ErrMediaFinalize
		}
		size, digest, evidenceErr := stableMediaFileEvidence(ctx, absolutePath)
		switch {
		case errors.Is(evidenceErr, os.ErrNotExist):
			planned.SizeBytes, planned.SHA256 = existing.SizeBytes, existing.SHA256
			planned.SampleRate, planned.Channels = existing.SampleRate, existing.Channels
			return planned, nil
		case errors.Is(evidenceErr, errMediaFileChanged):
			changed := changedMediaArtifact(existing)
			return changed, nil
		case evidenceErr != nil:
			if contextErr := ctx.Err(); contextErr != nil {
				return MediaArtifact{}, contextErr
			}
			return MediaArtifact{}, ErrMediaFinalize
		case size != existing.SizeBytes || digest != existing.SHA256:
			changed := changedMediaArtifact(existing)
			return changed, nil
		default:
			restored := existing
			restored.Status, restored.ErrorCode = MediaArtifactComplete, ""
			return restored, nil
		}
	}
	if sameBinding && retryableMediaArtifact(existing) {
		adopted, ok, adoptErr := finalizer.adoptPublishedArtifact(ctx, planned)
		if adoptErr != nil {
			return MediaArtifact{}, adoptErr
		}
		if ok {
			return adopted, nil
		}
		absolutePath, pathErr := mediaAbsolutePath(finalizer.root, relativePath)
		if pathErr != nil {
			return MediaArtifact{}, ErrMediaFinalize
		}
		info, statErr := os.Lstat(absolutePath)
		if statErr == nil {
			if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				return MediaArtifact{}, ErrMediaFileConflict
			}
			planned.Status, planned.ErrorCode = MediaArtifactFailed, "MEDIA_ARTIFACT_CONFLICT"
			planned.SizeBytes, planned.SHA256 = existing.SizeBytes, existing.SHA256
			return planned, nil
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return MediaArtifact{}, ErrMediaFinalize
		}
	}
	return planned, nil
}

func (finalizer *sqliteSessionMediaFinalizer) adoptPublishedArtifact(
	ctx context.Context,
	artifact MediaArtifact,
) (MediaArtifact, bool, error) {
	absolutePath, err := mediaAbsolutePath(finalizer.root, artifact.RelativePath)
	if err != nil {
		return MediaArtifact{}, false, ErrMediaFinalize
	}
	snapshot, err := openMediaFileSnapshot(absolutePath)
	if errors.Is(err, os.ErrNotExist) {
		return artifact, false, nil
	}
	if err != nil {
		return artifact, false, nil
	}
	defer snapshot.Close()
	preSize, preDigest, err := snapshot.hash(ctx, absolutePath)
	if err != nil {
		return artifact, false, nil
	}
	if err := finalizer.dependencies.inspectArtifact(
		ctx, finalizer.tools.ffprobePath, absolutePath, artifact.Kind,
	); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return MediaArtifact{}, false, contextErr
		}
		return artifact, false, nil
	}
	postSize, postDigest, err := snapshot.hash(ctx, absolutePath)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return MediaArtifact{}, false, contextErr
		}
		return artifact, false, nil
	}
	if preSize != postSize || preDigest != postDigest {
		return artifact, false, nil
	}
	artifact.Status, artifact.ErrorCode = MediaArtifactComplete, ""
	artifact.SizeBytes, artifact.SHA256 = preSize, preDigest
	if artifact.Kind == MediaArtifactASRWAV {
		artifact.SampleRate, artifact.Channels = 16_000, 1
	}
	return artifact, true, nil
}

func changedMediaArtifact(existing MediaArtifact) MediaArtifact {
	existing.Status = MediaArtifactFailed
	existing.ErrorCode = "MEDIA_ARTIFACT_CHANGED"
	return existing
}

func terminalMediaArtifactFailure(artifact MediaArtifact) bool {
	return artifact.Status == MediaArtifactFailed &&
		(artifact.ErrorCode == "MEDIA_ARTIFACT_CHANGED" || artifact.ErrorCode == "MEDIA_ARTIFACT_CONFLICT")
}

func retryableMediaArtifact(artifact MediaArtifact) bool {
	return retryableMediaArtifactStatus(artifact.Status) && !terminalMediaArtifactFailure(artifact)
}

func retryableMediaArtifactStatus(status MediaArtifactStatus) bool {
	switch status {
	case MediaArtifactPending, MediaArtifactFailed, MediaArtifactMissing:
		return true
	default:
		return false
	}
}

func (finalizer *sqliteSessionMediaFinalizer) generatePendingArtifacts(
	ctx context.Context,
	snapshot MediaSnapshot,
) ([]MediaArtifact, []string, error) {
	segments := make(map[string]MediaSegment, len(snapshot.Segments))
	for _, segment := range snapshot.Segments {
		segments[segment.ID] = segment
	}
	artifacts := append([]MediaArtifact(nil), snapshot.Artifacts...)
	warnings := make([]string, 0)
	for index := range artifacts {
		artifact := &artifacts[index]
		if artifact.Status != MediaArtifactPending {
			continue
		}
		segment, exists := segments[artifact.MediaSegmentID]
		if !exists {
			return nil, warnings, ErrMediaSnapshotConflict
		}
		if err := finalizer.verifyBoundRoot(ctx); err != nil {
			return nil, warnings, err
		}
		if err := finalizer.acquireProxy(ctx); err != nil {
			return nil, warnings, err
		}
		generatedSize, generatedDigest, generationErr := finalizer.generateArtifact(ctx, segment, artifact)
		finalizer.releaseProxy()
		if ctx.Err() != nil {
			return nil, warnings, ctx.Err()
		}
		if err := finalizer.verifyBoundRoot(ctx); err != nil {
			return nil, warnings, err
		}
		now := finalizer.dependencies.now().UTC().UnixMilli()
		if generationErr != nil {
			if errors.Is(generationErr, ErrMediaFinalize) {
				return nil, warnings, generationErr
			}
			if generatedSize > 0 && generatedDigest != "" {
				artifact.SizeBytes, artifact.SHA256 = generatedSize, generatedDigest
			}
			artifact.Status = MediaArtifactFailed
			artifact.ErrorCode = "MEDIA_ARTIFACT_FAILED"
			if errors.Is(generationErr, ErrMediaFileConflict) {
				artifact.ErrorCode = "MEDIA_ARTIFACT_CONFLICT"
			}
			artifact.UpdatedAt = now
			warnings = append(warnings, artifact.ErrorCode)
			continue
		}
		absolutePath, err := mediaAbsolutePath(finalizer.root, artifact.RelativePath)
		if err != nil {
			return nil, warnings, ErrMediaFinalize
		}
		observedSize, observedDigest, err := finalizer.dependencies.hashFile(ctx, absolutePath)
		if err != nil {
			artifact.Status, artifact.ErrorCode, artifact.UpdatedAt = MediaArtifactFailed, "MEDIA_ARTIFACT_HASH_FAILED", now
			artifact.SizeBytes, artifact.SHA256 = generatedSize, generatedDigest
			warnings = append(warnings, artifact.ErrorCode)
			continue
		}
		if observedSize != generatedSize || observedDigest != generatedDigest {
			artifact.Status, artifact.ErrorCode, artifact.UpdatedAt = MediaArtifactFailed, "MEDIA_ARTIFACT_CHANGED", now
			artifact.SizeBytes, artifact.SHA256 = generatedSize, generatedDigest
			warnings = append(warnings, artifact.ErrorCode)
			continue
		}
		artifact.Status, artifact.ErrorCode = MediaArtifactComplete, ""
		artifact.SizeBytes, artifact.SHA256, artifact.UpdatedAt = generatedSize, generatedDigest, now
		if artifact.Kind == MediaArtifactASRWAV {
			artifact.SampleRate, artifact.Channels = 16_000, 1
		}
	}
	return artifacts, warnings, nil
}

func (finalizer *sqliteSessionMediaFinalizer) generateArtifact(
	ctx context.Context,
	segment MediaSegment,
	artifact *MediaArtifact,
) (int64, string, error) {
	source, err := mediaAbsolutePath(finalizer.root, segment.RelativePath)
	if err != nil {
		return 0, "", ErrMediaFinalize
	}
	target, err := mediaAbsolutePath(finalizer.root, artifact.RelativePath)
	if err != nil {
		return 0, "", ErrMediaFinalize
	}
	if err := finalizer.verifyBoundRoot(ctx); err != nil {
		return 0, "", err
	}
	sourceInput, err := newMediaArtifactSource(source, segment.SizeBytes, segment.SHA256)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return 0, "", contextErr
		}
		return 0, "", ErrMediaFileConflict
	}
	defer sourceInput.Close()
	verify := func(verifyCtx context.Context) error {
		if err := sourceInput.verify(verifyCtx); err != nil {
			return err
		}
		return finalizer.verifyBoundRoot(verifyCtx)
	}
	var generationErr error
	switch artifact.Kind {
	case MediaArtifactASRWAV:
		generationErr = finalizer.dependencies.generateASR(
			ctx, finalizer.tools.ffmpegPath, sourceInput, target, verify,
		)
	case MediaArtifactPlaybackMP4:
		generationErr = finalizer.dependencies.generatePlayback(
			ctx, finalizer.tools.ffmpegPath, sourceInput, target, verify,
		)
	default:
		return 0, "", ErrMediaFinalize
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return 0, "", contextErr
	}
	if err := finalizer.verifyBoundRoot(ctx); err != nil {
		return 0, "", err
	}
	if generationErr != nil {
		return 0, "", generationErr
	}
	if err := sourceInput.verify(ctx); err != nil {
		return 0, "", err
	}
	snapshot, err := openMediaFileSnapshot(target)
	if err != nil {
		return 0, "", ErrMediaArtifactFailed
	}
	defer snapshot.Close()
	preSize, preDigest, err := snapshot.hash(ctx, target)
	if err != nil {
		return 0, "", ErrMediaArtifactFailed
	}
	if err := finalizer.dependencies.inspectArtifact(
		ctx, finalizer.tools.ffprobePath, target, artifact.Kind,
	); err != nil {
		return preSize, preDigest, err
	}
	postSize, postDigest, err := snapshot.hash(ctx, target)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return preSize, preDigest, contextErr
		}
		return preSize, preDigest, ErrMediaFileConflict
	}
	if postSize != preSize || postDigest != preDigest {
		return preSize, preDigest, ErrMediaFileConflict
	}
	return preSize, preDigest, nil
}

func (finalizer *sqliteSessionMediaFinalizer) acquireProxy(ctx context.Context) error {
	select {
	case finalizer.proxyCapacity <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (finalizer *sqliteSessionMediaFinalizer) releaseProxy() { <-finalizer.proxyCapacity }

func mediaArtifactKey(segmentID string, kind MediaArtifactKind) string {
	return segmentID + "\x00" + string(kind)
}

func mediaEpochFromSegments(segments []MediaSegment) *int64 {
	for _, segment := range segments {
		if segment.Status != MediaSegmentComplete && segment.Status != MediaSegmentRecovered {
			continue
		}
		epoch := segment.StartedAt
		if segment.PTSStartMS != nil && *segment.PTSStartMS <= epoch {
			epoch -= *segment.PTSStartMS
		}
		return &epoch
	}
	return nil
}

func completedMediaState(segments []MediaSegment) SessionMediaState {
	if len(segments) == 0 {
		return SessionMediaIncomplete
	}
	for _, segment := range segments {
		if segment.Status != MediaSegmentComplete && segment.Status != MediaSegmentRecovered {
			return SessionMediaIncomplete
		}
	}
	return SessionMediaCompleted
}

func mediaSnapshotHasRetryableArtifacts(snapshot MediaSnapshot) bool {
	for _, artifact := range snapshot.Artifacts {
		if retryableMediaArtifact(artifact) {
			return true
		}
	}
	return false
}

func stableMediaWarnings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !validMediaErrorCode(value) {
			value = "MEDIA_FINALIZE_FAILED"
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
