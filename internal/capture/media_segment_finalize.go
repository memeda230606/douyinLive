package capture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"time"
)

var (
	ErrMediaSegmentFinalize = errors.New("MEDIA_SEGMENT_FINALIZE_FAILED")
	ErrMediaHash            = errors.New("MEDIA_HASH_FAILED")
	errMediaFileChanged     = errors.New("MEDIA_FILE_CHANGED")
)

const mediaProbeVersion = "ffprobe_v1"

type mediaSegmentProcessor struct {
	prober *ffprobeSegmentProber
	newID  func() (string, error)
	verify func(context.Context) error
}

func (processor mediaSegmentProcessor) finalize(
	ctx context.Context,
	candidate mediaCandidate,
	existing *MediaSegment,
) (MediaSegment, []string, error) {
	if ctx == nil || processor.prober == nil || processor.newID == nil {
		return MediaSegment{}, nil, ErrMediaSegmentFinalize
	}
	if err := ctx.Err(); err != nil {
		return MediaSegment{}, nil, err
	}
	id, err := processor.segmentID(existing)
	if err != nil {
		return MediaSegment{}, nil, err
	}
	if existing != nil && existing.Status == MediaSegmentCorrupt &&
		existing.ErrorCode == "MEDIA_FINAL_CHANGED" {
		return *existing, []string{"MEDIA_FINAL_CHANGED"}, nil
	}
	probePath := candidate.PartialPath
	status := MediaSegmentComplete
	if candidate.AlreadyFinal {
		probePath = candidate.FinalPath
		status = MediaSegmentRecovered
	}
	if existing != nil && (existing.Status == MediaSegmentComplete || existing.Status == MediaSegmentRecovered) {
		status = existing.Status
	}
	baseline, hasBaseline, baselineErr := verifiedMediaSegmentBaseline(existing)
	if baselineErr != nil {
		return MediaSegment{}, nil, baselineErr
	}
	snapshot, snapshotErr := openMediaFileSnapshot(probePath)
	if snapshotErr != nil {
		if hasBaseline && errors.Is(snapshotErr, os.ErrNotExist) {
			missing := missingVerifiedMediaSegment(baseline)
			return missing, []string{"MEDIA_FINAL_MISSING"}, nil
		}
		if errors.Is(snapshotErr, errMediaFileChanged) {
			if hasBaseline {
				changed := changedVerifiedMediaSegment(baseline)
				return changed, []string{"MEDIA_FINAL_CHANGED"}, nil
			}
			return corruptMediaSegmentWithEvidence(
				candidate, id, "MEDIA_HASH_FAILED", 0, "",
			), []string{"MEDIA_HASH_FAILED"}, nil
		}
		return MediaSegment{}, nil, ErrMediaSegmentFinalize
	}
	defer snapshot.Close()
	preSize, preDigest, preHashErr := snapshot.hash(ctx, probePath)
	if preHashErr != nil {
		return MediaSegment{}, nil, ErrMediaSegmentFinalize
	}
	if hasBaseline && (preSize != baseline.SizeBytes || preDigest != baseline.SHA256) {
		changed := changedVerifiedMediaSegment(baseline)
		return changed, []string{"MEDIA_FINAL_CHANGED"}, nil
	}
	result, probeErr := processor.prober.Probe(ctx, probePath)
	// Probing can take seconds. Re-establish the root binding after it returns
	// so a drift during probing cannot be followed by a publish or cleanup.
	if processor.verify != nil {
		if err := processor.verify(ctx); err != nil {
			return MediaSegment{}, nil, err
		}
	}
	size, digest, evidenceErr := snapshot.hash(ctx, probePath)
	if evidenceErr != nil {
		if hasBaseline && (errors.Is(evidenceErr, os.ErrNotExist) ||
			errors.Is(evidenceErr, errMediaFileChanged)) {
			changed := changedVerifiedMediaSegment(baseline)
			return changed, []string{"MEDIA_FINAL_CHANGED"}, nil
		}
		if errors.Is(evidenceErr, os.ErrNotExist) || errors.Is(evidenceErr, errMediaFileChanged) {
			return corruptMediaSegmentWithEvidence(
				candidate, id, "MEDIA_FINAL_CHANGED", preSize, preDigest,
			), []string{"MEDIA_FINAL_CHANGED"}, nil
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return MediaSegment{}, nil, contextErr
		}
		return MediaSegment{}, nil, ErrMediaSegmentFinalize
	}
	if size != preSize || digest != preDigest {
		if hasBaseline {
			changed := changedVerifiedMediaSegment(baseline)
			return changed, []string{"MEDIA_FINAL_CHANGED"}, nil
		}
		return corruptMediaSegmentWithEvidence(
			candidate, id, "MEDIA_FINAL_CHANGED", preSize, preDigest,
		), []string{"MEDIA_FINAL_CHANGED"}, nil
	}
	if hasBaseline && (size != baseline.SizeBytes || digest != baseline.SHA256) {
		changed := changedVerifiedMediaSegment(baseline)
		return changed, []string{"MEDIA_FINAL_CHANGED"}, nil
	}
	if probeErr != nil {
		errorCode := mediaProbeErrorCode(probeErr)
		if hasBaseline {
			if transientMediaProbeFailure(probeErr) {
				return baseline, []string{errorCode}, nil
			}
			failed := baseline
			failed.Status = MediaSegmentCorrupt
			failed.ErrorCode = errorCode
			return failed, []string{errorCode}, nil
		}
		return corruptMediaSegmentWithEvidence(candidate, id, errorCode, size, digest),
			[]string{errorCode}, nil
	}
	warnings := make([]string, 0, 1)
	if candidate.AlreadyFinal {
		if candidate.PartialPath != "" {
			same, compareErr := sameMediaFileContent(ctx, candidate.PartialPath, candidate.FinalPath)
			switch {
			case compareErr != nil:
				return corruptMediaSegment(ctx, candidate, id, "MEDIA_TARGET_CONFLICT"),
					[]string{"MEDIA_DUPLICATE_CHECK_FAILED", "MEDIA_TARGET_CONFLICT"}, nil
			case !same:
				return corruptMediaSegment(ctx, candidate, id, "MEDIA_TARGET_CONFLICT"),
					[]string{"MEDIA_TARGET_CONFLICT"}, nil
			default:
				if err := os.Remove(candidate.PartialPath); err != nil && !errors.Is(err, os.ErrNotExist) {
					warnings = append(warnings, "MEDIA_DUPLICATE_CLEANUP_FAILED")
				}
			}
		}
	} else {
		if err := snapshot.Close(); err != nil {
			return MediaSegment{}, nil, ErrMediaSegmentFinalize
		}
		publishErr := publishMediaFile(candidate.PartialPath, candidate.FinalPath)
		if errors.Is(publishErr, ErrMediaFileConflict) {
			same, compareErr := sameMediaFileContent(ctx, candidate.PartialPath, candidate.FinalPath)
			if compareErr != nil || !same {
				return corruptMediaSegment(ctx, candidate, id, "MEDIA_TARGET_CONFLICT"),
					[]string{"MEDIA_TARGET_CONFLICT"}, nil
			}
			status = MediaSegmentRecovered
			if err := os.Remove(candidate.PartialPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				warnings = append(warnings, "MEDIA_DUPLICATE_CLEANUP_FAILED")
			}
			finalSize, finalDigest, hashErr := stableMediaFileEvidence(ctx, candidate.FinalPath)
			if hashErr != nil {
				return MediaSegment{}, nil, ErrMediaSegmentFinalize
			}
			if finalSize != size || finalDigest != digest {
				if hasBaseline {
					changed := changedVerifiedMediaSegment(baseline)
					return changed, []string{"MEDIA_FINAL_CHANGED"}, nil
				}
				return corruptMediaSegmentWithEvidence(
					candidate, id, "MEDIA_TARGET_CONFLICT", size, digest,
				), []string{"MEDIA_TARGET_CONFLICT"}, nil
			}
			size, digest = finalSize, finalDigest
		} else if publishErr != nil {
			return MediaSegment{}, nil, ErrMediaSegmentFinalize
		} else if err := snapshot.matchesStoredPath(candidate.FinalPath); err != nil {
			if hasBaseline {
				changed := changedVerifiedMediaSegment(baseline)
				return changed, []string{"MEDIA_FINAL_CHANGED"}, nil
			}
			return corruptMediaSegmentWithEvidence(
				candidate, id, "MEDIA_FINAL_CHANGED", size, digest,
			), []string{"MEDIA_FINAL_CHANGED"}, nil
		} else if finalSize, finalDigest, hashErr := stableMediaFileEvidence(
			ctx, candidate.FinalPath,
		); hashErr != nil || finalSize != size || finalDigest != digest {
			if hasBaseline {
				changed := changedVerifiedMediaSegment(baseline)
				return changed, []string{"MEDIA_FINAL_CHANGED"}, nil
			}
			return corruptMediaSegmentWithEvidence(
				candidate, id, "MEDIA_FINAL_CHANGED", size, digest,
			), []string{"MEDIA_FINAL_CHANGED"}, nil
		}
	}
	if candidate.AlreadyFinal {
		finalSize, finalDigest, finalErr := snapshot.hash(ctx, candidate.FinalPath)
		if finalErr != nil || finalSize != size || finalDigest != digest {
			if hasBaseline {
				changed := changedVerifiedMediaSegment(baseline)
				return changed, []string{"MEDIA_FINAL_CHANGED"}, nil
			}
			if contextErr := ctx.Err(); contextErr != nil {
				return MediaSegment{}, nil, contextErr
			}
			return corruptMediaSegmentWithEvidence(
				candidate, id, "MEDIA_FINAL_CHANGED", size, digest,
			), []string{"MEDIA_FINAL_CHANGED"}, nil
		}
	}
	if hasBaseline {
		preserved := baseline
		if preserved.Status == MediaSegmentMissing {
			preserved.Status = MediaSegmentRecovered
			preserved.ErrorCode = ""
		}
		return preserved, warnings, nil
	}
	segment, err := readableMediaSegment(candidate, id, result, status, size, digest)
	if err != nil {
		return MediaSegment{}, nil, err
	}
	return segment, warnings, nil
}

func verifiedMediaSegmentBaseline(existing *MediaSegment) (MediaSegment, bool, error) {
	if existing == nil {
		return MediaSegment{}, false, nil
	}
	verified := existing.Status == MediaSegmentComplete || existing.Status == MediaSegmentRecovered ||
		existing.Status == MediaSegmentMissing && existing.ErrorCode == "MEDIA_FINAL_MISSING"
	if !verified {
		return MediaSegment{}, false, nil
	}
	if existing.SizeBytes <= 0 || existing.SHA256 == "" || !validMediaDigest(existing.SHA256) {
		return MediaSegment{}, false, ErrMediaSegmentFinalize
	}
	return *existing, true, nil
}

func changedVerifiedMediaSegment(existing MediaSegment) MediaSegment {
	existing.Status = MediaSegmentCorrupt
	existing.ErrorCode = "MEDIA_FINAL_CHANGED"
	return existing
}

func missingVerifiedMediaSegment(existing MediaSegment) MediaSegment {
	existing.Status = MediaSegmentMissing
	existing.ErrorCode = "MEDIA_FINAL_MISSING"
	return existing
}

func transientMediaProbeFailure(err error) bool {
	return errors.Is(err, ErrSegmentProbeTimeout) ||
		errors.Is(err, ErrSegmentProbeDependency) ||
		errors.Is(err, ErrSegmentProbeUnreadable)
}

func (processor mediaSegmentProcessor) segmentID(existing *MediaSegment) (string, error) {
	if existing != nil {
		if validateUUIDv7("existing media segment", existing.ID) != nil {
			return "", ErrMediaSegmentFinalize
		}
		return existing.ID, nil
	}
	id, err := processor.newID()
	if err != nil || validateUUIDv7("new media segment", id) != nil {
		return "", ErrMediaSegmentFinalize
	}
	return id, nil
}

func readableMediaSegment(
	candidate mediaCandidate,
	id string,
	probe SegmentProbeResult,
	status MediaSegmentStatus,
	size int64,
	digest string,
) (MediaSegment, error) {
	if probe.Readability != SegmentReadabilityReadable || size <= 0 || !validMediaDigest(digest) {
		return MediaSegment{}, ErrMediaSegmentFinalize
	}
	duration := mediaMicrosecondsToMilliseconds(probe.DurationUs)
	startedAt := candidate.WallStartedAt
	if duration > math.MaxInt64-startedAt {
		return MediaSegment{}, ErrMediaSegmentFinalize
	}
	videoCodec, audioCodec := mediaProbeCodecs(probe)
	return MediaSegment{
		ID: id, Sequence: candidate.Sequence, RelativePath: candidate.FinalRelativePath,
		Container: "mkv", VideoCodec: videoCodec, AudioCodec: audioCodec,
		StartedAt: startedAt, EndedAt: startedAt + duration,
		PTSStartMS: mediaMicrosecondsPointerToMilliseconds(probe.FirstTimestampUs),
		PTSEndMS:   mediaMicrosecondsPointerToMilliseconds(probe.LastTimestampUs),
		DurationMS: duration, SizeBytes: size, SHA256: digest, Status: status,
		AttemptID: candidate.Attempt.ID, AttemptSequence: candidate.AttemptSequence,
		SourceRelativePath: candidate.SourceRelativePath, ProbeVersion: mediaProbeVersion,
	}, nil
}

func corruptMediaSegment(
	ctx context.Context,
	candidate mediaCandidate,
	id, errorCode string,
) MediaSegment {
	path := candidate.PartialPath
	if path == "" {
		path = candidate.FinalPath
	}
	size, digest, _ := hashMediaFile(ctx, path)
	return corruptMediaSegmentWithEvidence(candidate, id, errorCode, size, digest)
}

func corruptMediaSegmentWithEvidence(
	candidate mediaCandidate,
	id, errorCode string,
	size int64,
	digest string,
) MediaSegment {
	return MediaSegment{
		ID: id, Sequence: candidate.Sequence, RelativePath: candidate.FinalRelativePath,
		Container: "mkv", StartedAt: candidate.WallStartedAt, EndedAt: candidate.WallStartedAt,
		SizeBytes: size, SHA256: digest, Status: MediaSegmentCorrupt,
		AttemptID: candidate.Attempt.ID, AttemptSequence: candidate.AttemptSequence,
		SourceRelativePath: candidate.SourceRelativePath, ProbeVersion: mediaProbeVersion,
		ErrorCode: errorCode,
	}
}

func mediaProbeErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrSegmentProbeTimeout):
		return "MEDIA_PROBE_TIMEOUT"
	case errors.Is(err, ErrSegmentProbeDependency):
		return "MEDIA_PROBE_DEPENDENCY"
	case errors.Is(err, ErrSegmentProbeUnsupported):
		return "MEDIA_PROBE_UNSUPPORTED"
	case errors.Is(err, ErrSegmentProbeInput):
		return "MEDIA_PROBE_INPUT"
	case errors.Is(err, ErrSegmentProbeUnreadable):
		return "MEDIA_PROBE_UNREADABLE"
	default:
		return "MEDIA_PROBE_FAILED"
	}
}

func mediaProbeCodecs(result SegmentProbeResult) (string, string) {
	videoCodec := ""
	audioCodec := ""
	for _, stream := range result.Streams {
		switch stream.Type {
		case "video":
			if videoCodec == "" {
				videoCodec = stream.Codec
			}
		case "audio":
			if audioCodec == "" {
				audioCodec = stream.Codec
			}
		}
	}
	return videoCodec, audioCodec
}

func mediaMicrosecondsToMilliseconds(value *int64) int64 {
	if value == nil || *value <= 0 {
		return 0
	}
	return *value / int64(time.Millisecond/time.Microsecond)
}

func mediaMicrosecondsPointerToMilliseconds(value *int64) *int64 {
	if value == nil {
		return nil
	}
	converted := mediaMicrosecondsToMilliseconds(value)
	return &converted
}

type mediaFileSnapshot struct {
	file *os.File
	info os.FileInfo
}

func openMediaFileSnapshot(path string) (*mediaFileSnapshot, error) {
	if !validMediaAbsolutePath(path) {
		return nil, ErrMediaHash
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	reparse, reparseErr := mediaPathIsReparsePoint(path, pathInfo)
	if reparseErr != nil {
		return nil, errMediaFileChanged
	}
	if !pathInfo.Mode().IsRegular() || reparse || pathInfo.Size() <= 0 {
		return nil, errMediaFileChanged
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !sameMediaFileVersion(pathInfo, info) {
		file.Close()
		return nil, errMediaFileChanged
	}
	return &mediaFileSnapshot{file: file, info: info}, nil
}

func (snapshot *mediaFileSnapshot) Close() error {
	if snapshot == nil || snapshot.file == nil {
		return nil
	}
	return snapshot.file.Close()
}

func (snapshot *mediaFileSnapshot) matchesPath(path string) error {
	if snapshot == nil || snapshot.file == nil || !validMediaAbsolutePath(path) {
		return ErrMediaHash
	}
	handleInfo, err := snapshot.file.Stat()
	if err != nil {
		return ErrMediaHash
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return err
	}
	reparse, reparseErr := mediaPathIsReparsePoint(path, pathInfo)
	if reparseErr != nil {
		return errMediaFileChanged
	}
	if !pathInfo.Mode().IsRegular() || reparse || pathInfo.Size() <= 0 ||
		!sameMediaFileVersion(snapshot.info, handleInfo) ||
		!sameMediaFileVersion(snapshot.info, pathInfo) {
		return errMediaFileChanged
	}
	return nil
}

func (snapshot *mediaFileSnapshot) matchesStoredPath(path string) error {
	if snapshot == nil || snapshot.info == nil || !validMediaAbsolutePath(path) {
		return ErrMediaHash
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return err
	}
	reparse, reparseErr := mediaPathIsReparsePoint(path, pathInfo)
	if reparseErr != nil {
		return errMediaFileChanged
	}
	if !pathInfo.Mode().IsRegular() || reparse || pathInfo.Size() <= 0 ||
		!sameMediaFileVersion(snapshot.info, pathInfo) {
		return errMediaFileChanged
	}
	return nil
}

func sameMediaFileVersion(left, right os.FileInfo) bool {
	return left != nil && right != nil && os.SameFile(left, right) &&
		left.Size() == right.Size() && left.Mode() == right.Mode() &&
		left.ModTime().Equal(right.ModTime())
}

func (snapshot *mediaFileSnapshot) hash(ctx context.Context, path string) (int64, string, error) {
	if ctx == nil || snapshot == nil || snapshot.file == nil {
		return 0, "", ErrMediaHash
	}
	if err := snapshot.matchesPath(path); err != nil {
		return 0, "", err
	}
	if _, err := snapshot.file.Seek(0, io.SeekStart); err != nil {
		return 0, "", ErrMediaHash
	}
	hash := sha256.New()
	buffer := make([]byte, 128<<10)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, "", err
		}
		count, readErr := snapshot.file.Read(buffer)
		if count > 0 {
			size += int64(count)
			_, _ = hash.Write(buffer[:count])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return 0, "", ErrMediaHash
		}
	}
	if size <= 0 {
		return 0, "", ErrMediaHash
	}
	if err := snapshot.matchesPath(path); err != nil {
		return 0, "", err
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

func stableMediaFileEvidence(ctx context.Context, path string) (int64, string, error) {
	snapshot, err := openMediaFileSnapshot(path)
	if err != nil {
		return 0, "", err
	}
	defer snapshot.Close()
	return snapshot.hash(ctx, path)
}

func hashMediaFile(ctx context.Context, path string) (int64, string, error) {
	if ctx == nil {
		return 0, "", ErrMediaHash
	}
	size, digest, err := stableMediaFileEvidence(ctx, path)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return 0, "", contextErr
		}
		return 0, "", ErrMediaHash
	}
	return size, digest, nil
}

func sameMediaFileContent(ctx context.Context, left, right string) (bool, error) {
	leftSize, leftDigest, err := stableMediaFileEvidence(ctx, left)
	if err != nil {
		return false, err
	}
	rightSize, rightDigest, err := stableMediaFileEvidence(ctx, right)
	if err != nil {
		return false, err
	}
	return leftSize == rightSize && leftDigest == rightDigest, nil
}
