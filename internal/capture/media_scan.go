package capture

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	ErrMediaScanInvalid  = errors.New("MEDIA_SCAN_INVALID")
	ErrMediaScanLimit    = errors.New("MEDIA_SCAN_LIMIT")
	ErrMediaScanConflict = errors.New("MEDIA_SCAN_CONFLICT")
	ErrMediaScanIO       = errors.New("MEDIA_SCAN_IO")
)

const (
	maximumMediaSegments          = 4096
	maximumMediaDirectoryEntries  = maximumMediaSegments * 2
	mediaAttemptTimestampLayout   = "20060102T150405.000000000Z"
	mediaFinalTimestampLayout     = "20060102T150405.000Z"
	mediaSegmentFilenameIndexSize = 6
)

type mediaCandidate struct {
	Attempt            MediaAttempt
	Sequence           int
	AttemptSequence    int
	WallStartedAt      int64
	SourceRelativePath string
	FinalRelativePath  string
	PartialPath        string
	FinalPath          string
	AlreadyFinal       bool
}

func (candidate mediaCandidate) String() string {
	return "mediaCandidate{sequence:" + strconv.Itoa(candidate.Sequence) +
		" attemptSequence:" + strconv.Itoa(candidate.AttemptSequence) + " paths:<redacted>}"
}

func (candidate mediaCandidate) GoString() string { return candidate.String() }

func (candidate mediaCandidate) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int("sequence", candidate.Sequence),
		slog.Int("attempt_sequence", candidate.AttemptSequence),
		slog.String("paths", "<redacted>"),
	)
}

type mediaSlot struct {
	attemptID       string
	attemptSequence int
}

func discoverMediaCandidates(
	sessionDirectory, sessionRelativePath string,
	attempts []MediaAttempt,
) ([]mediaCandidate, error) {
	if !validMediaAbsolutePath(sessionDirectory) || !validMediaRelativePath(sessionRelativePath) {
		return nil, ErrMediaScanInvalid
	}
	info, err := os.Lstat(sessionDirectory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrMediaScanInvalid
	}
	normalizedAttempts, err := normalizeMediaScanAttempts(attempts)
	if err != nil {
		return nil, err
	}
	mediaDirectory := filepath.Join(sessionDirectory, "media")
	if err := ensureMediaScanDirectory(mediaDirectory); err != nil {
		return nil, err
	}

	candidates := make([]mediaCandidate, 0)
	sequenceIndexes := make(map[int]int)
	slotIndexes := make(map[mediaSlot]int)
	finalNames, err := readMediaDirectoryNames(mediaDirectory, maximumMediaDirectoryEntries)
	if err != nil {
		return nil, err
	}
	for _, name := range finalNames {
		sequence, startedAt, ok := parseMediaFinalSegmentName(name)
		if !ok {
			continue
		}
		attempt, attemptSequence, matched := matchFinalMediaAttempt(normalizedAttempts, startedAt)
		if !matched {
			continue
		}
		if len(candidates) >= maximumMediaSegments {
			return nil, ErrMediaScanLimit
		}
		if _, exists := sequenceIndexes[sequence]; exists {
			return nil, ErrMediaScanConflict
		}
		slot := mediaSlot{attemptID: attempt.ID, attemptSequence: attemptSequence}
		if _, exists := slotIndexes[slot]; exists {
			return nil, ErrMediaScanConflict
		}
		finalRelativePath, err := joinMediaRelativePath(sessionRelativePath, "media", name)
		if err != nil {
			return nil, ErrMediaScanInvalid
		}
		sourceName := mediaAttemptSegmentName(attemptSequence, attempt)
		sourceRelativePath, err := joinMediaRelativePath(
			sessionRelativePath, "media", ".attempt-"+attempt.ID, sourceName,
		)
		if err != nil {
			return nil, ErrMediaScanInvalid
		}
		candidate := mediaCandidate{
			Attempt: attempt, Sequence: sequence, AttemptSequence: attemptSequence,
			WallStartedAt: startedAt, SourceRelativePath: sourceRelativePath,
			FinalRelativePath: finalRelativePath,
			FinalPath:         filepath.Join(mediaDirectory, name), AlreadyFinal: true,
		}
		sequenceIndexes[sequence] = len(candidates)
		slotIndexes[slot] = len(candidates)
		candidates = append(candidates, candidate)
	}

	nextSequence := 1
	for _, attempt := range normalizedAttempts {
		attemptDirectory := filepath.Join(mediaDirectory, ".attempt-"+attempt.ID)
		info, statErr := os.Lstat(attemptDirectory)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return nil, ErrMediaScanIO
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, ErrMediaScanInvalid
		}
		names, readErr := readMediaDirectoryNames(attemptDirectory, maximumMediaSegments)
		if readErr != nil {
			return nil, readErr
		}
		attemptSequences := make(map[int]struct{}, len(names))
		for _, name := range names {
			attemptSequence, ok := parseMediaAttemptSegmentName(name, attempt)
			if !ok {
				continue
			}
			if _, exists := attemptSequences[attemptSequence]; exists {
				return nil, ErrMediaScanConflict
			}
			attemptSequences[attemptSequence] = struct{}{}
			slot := mediaSlot{attemptID: attempt.ID, attemptSequence: attemptSequence}
			if existingIndex, exists := slotIndexes[slot]; exists {
				candidates[existingIndex].PartialPath = filepath.Join(attemptDirectory, name)
				continue
			}
			if len(candidates) >= maximumMediaSegments {
				return nil, ErrMediaScanLimit
			}
			for {
				if _, occupied := sequenceIndexes[nextSequence]; !occupied {
					break
				}
				nextSequence++
				if nextSequence > maximumMediaSegments {
					return nil, ErrMediaScanLimit
				}
			}
			wallStartedAt, ok := mediaAttemptSegmentWallStart(attempt, attemptSequence)
			if !ok {
				return nil, ErrMediaScanInvalid
			}
			finalName := mediaFinalSegmentName(nextSequence, wallStartedAt)
			finalRelativePath, err := joinMediaRelativePath(sessionRelativePath, "media", finalName)
			if err != nil {
				return nil, ErrMediaScanInvalid
			}
			sourceRelativePath, err := joinMediaRelativePath(
				sessionRelativePath, "media", ".attempt-"+attempt.ID, name,
			)
			if err != nil {
				return nil, ErrMediaScanInvalid
			}
			candidate := mediaCandidate{
				Attempt: attempt, Sequence: nextSequence, AttemptSequence: attemptSequence,
				WallStartedAt: wallStartedAt, SourceRelativePath: sourceRelativePath,
				FinalRelativePath: finalRelativePath,
				PartialPath:       filepath.Join(attemptDirectory, name),
				FinalPath:         filepath.Join(mediaDirectory, finalName),
			}
			sequenceIndexes[nextSequence] = len(candidates)
			slotIndexes[slot] = len(candidates)
			candidates = append(candidates, candidate)
			nextSequence++
		}
	}
	sort.Slice(candidates, func(left, right int) bool {
		return candidates[left].Sequence < candidates[right].Sequence
	})
	return candidates, nil
}

func normalizeMediaScanAttempts(attempts []MediaAttempt) ([]MediaAttempt, error) {
	if len(attempts) > maximumMediaAttempts {
		return nil, ErrMediaScanLimit
	}
	normalized := append([]MediaAttempt(nil), attempts...)
	sort.Slice(normalized, func(left, right int) bool {
		if normalized[left].Ordinal == normalized[right].Ordinal {
			return normalized[left].ID < normalized[right].ID
		}
		return normalized[left].Ordinal < normalized[right].Ordinal
	})
	ids := make(map[string]struct{}, len(normalized))
	ordinals := make(map[int]struct{}, len(normalized))
	for _, attempt := range normalized {
		if !validRecorderAttemptID(attempt.ID) || attempt.Ordinal < 1 || attempt.Ordinal > maximumMediaAttempts ||
			attempt.StartedAt <= 0 || attempt.SegmentSeconds < 300 || attempt.SegmentSeconds > 1800 {
			return nil, ErrMediaScanInvalid
		}
		if _, exists := ids[attempt.ID]; exists {
			return nil, ErrMediaScanConflict
		}
		if _, exists := ordinals[attempt.Ordinal]; exists {
			return nil, ErrMediaScanConflict
		}
		ids[attempt.ID] = struct{}{}
		ordinals[attempt.Ordinal] = struct{}{}
	}
	return normalized, nil
}

func readMediaDirectoryNames(directory string, limit int) ([]string, error) {
	if limit < 1 {
		return nil, ErrMediaScanInvalid
	}
	handle, err := os.Open(directory)
	if err != nil {
		return nil, ErrMediaScanIO
	}
	defer handle.Close()
	names := make([]string, 0, min(limit, 256))
	for {
		entries, readErr := handle.ReadDir(256)
		for _, entry := range entries {
			if len(names) >= limit {
				return nil, ErrMediaScanLimit
			}
			names = append(names, entry.Name())
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, ErrMediaScanIO
		}
	}
	sort.Strings(names)
	return names, nil
}

func ensureMediaScanDirectory(directory string) error {
	info, err := os.Lstat(directory)
	if err != nil {
		return ErrMediaScanIO
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrMediaScanInvalid
	}
	return nil
}

func parseMediaAttemptSegmentName(name string, attempt MediaAttempt) (int, bool) {
	prefix := "segment-"
	suffix := "-" + strings.ToLower(attempt.ID) + ".mkv.partial"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(strings.ToLower(name), suffix) {
		return 0, false
	}
	content := name[len(prefix) : len(name)-len(suffix)]
	if len(content) != mediaSegmentFilenameIndexSize+1+len(mediaAttemptTimestampLayout) ||
		content[mediaSegmentFilenameIndexSize] != '-' {
		return 0, false
	}
	sequence, ok := parseMediaFilenameSequence(content[:mediaSegmentFilenameIndexSize])
	if !ok {
		return 0, false
	}
	stamp := content[mediaSegmentFilenameIndexSize+1:]
	startedAt, err := time.Parse(mediaAttemptTimestampLayout, stamp)
	if err != nil || startedAt.UTC().UnixMilli() != attempt.StartedAt {
		return 0, false
	}
	return sequence, true
}

func parseMediaFinalSegmentName(name string) (int, int64, bool) {
	const prefix = "segment-"
	const suffix = ".mkv"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(strings.ToLower(name), suffix) {
		return 0, 0, false
	}
	content := name[len(prefix) : len(name)-len(suffix)]
	if len(content) != mediaSegmentFilenameIndexSize+1+len(mediaFinalTimestampLayout) ||
		content[mediaSegmentFilenameIndexSize] != '-' {
		return 0, 0, false
	}
	sequence, ok := parseMediaFilenameSequence(content[:mediaSegmentFilenameIndexSize])
	if !ok || sequence > maximumMediaSegments {
		return 0, 0, false
	}
	startedAt, err := time.Parse(mediaFinalTimestampLayout, content[mediaSegmentFilenameIndexSize+1:])
	if err != nil {
		return 0, 0, false
	}
	return sequence, startedAt.UTC().UnixMilli(), true
}

func parseMediaFilenameSequence(value string) (int, bool) {
	if len(value) != mediaSegmentFilenameIndexSize || value[0] == '0' && value == "000000" {
		return 0, false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, false
		}
	}
	sequence, err := strconv.Atoi(value)
	return sequence, err == nil && sequence > 0
}

func mediaAttemptSegmentName(sequence int, attempt MediaAttempt) string {
	startedAt := time.UnixMilli(attempt.StartedAt).UTC()
	return fmt.Sprintf("segment-%06d-%s-%s.mkv.partial",
		sequence, startedAt.Format(mediaAttemptTimestampLayout), strings.ToLower(attempt.ID))
}

func mediaFinalSegmentName(sequence int, startedAt int64) string {
	return fmt.Sprintf("segment-%06d-%s.mkv",
		sequence, time.UnixMilli(startedAt).UTC().Format(mediaFinalTimestampLayout))
}

func mediaAttemptSegmentWallStart(attempt MediaAttempt, attemptSequence int) (int64, bool) {
	if attemptSequence < 1 || attempt.SegmentSeconds < 1 {
		return 0, false
	}
	deltaSegments := int64(attemptSequence - 1)
	deltaMillis := int64(attempt.SegmentSeconds) * int64(time.Second/time.Millisecond)
	if deltaSegments > 0 && deltaMillis > math.MaxInt64/deltaSegments {
		return 0, false
	}
	deltaMillis *= deltaSegments
	if attempt.StartedAt > math.MaxInt64-deltaMillis {
		return 0, false
	}
	return attempt.StartedAt + deltaMillis, true
}

func matchFinalMediaAttempt(attempts []MediaAttempt, startedAt int64) (MediaAttempt, int, bool) {
	var matched MediaAttempt
	matchedSequence := 0
	matches := 0
	for _, attempt := range attempts {
		delta := startedAt - attempt.StartedAt
		segmentMillis := int64(attempt.SegmentSeconds) * int64(time.Second/time.Millisecond)
		if delta < 0 || segmentMillis <= 0 || delta%segmentMillis != 0 {
			continue
		}
		sequence := int(delta/segmentMillis) + 1
		if sequence < 1 || sequence > maximumMediaSegments {
			continue
		}
		matched, matchedSequence, matches = attempt, sequence, matches+1
	}
	return matched, matchedSequence, matches == 1
}
