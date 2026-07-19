package capture

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	ErrMediaContractInvalid  = errors.New("MEDIA_CONTRACT_INVALID")
	ErrSessionMediaNotFound  = errors.New("SESSION_MEDIA_NOT_FOUND")
	ErrSessionMediaConflict  = errors.New("SESSION_MEDIA_CONFLICT")
	ErrMediaSnapshotConflict = errors.New("MEDIA_SNAPSHOT_CONFLICT")
	ErrMediaPersistence      = errors.New("MEDIA_PERSISTENCE_FAILED")
)

const (
	maximumMediaAttempts      = 128
	maxMediaRelativePathBytes = 2048
	maxMediaSafeTokenBytes    = 128
	// json.Marshal can expand each accepted token byte to a six-byte escape.
	maxMediaAttemptsJSONBytes = maximumMediaAttempts * (3*maxMediaSafeTokenBytes*6 + 1024)
	maximumMediaArtifacts     = maximumMediaSegments * 2
)

type SessionMediaState string

const (
	SessionMediaOpen       SessionMediaState = "open"
	SessionMediaFinalizing SessionMediaState = "finalizing"
	SessionMediaCompleted  SessionMediaState = "completed"
	SessionMediaIncomplete SessionMediaState = "incomplete"
)

type MediaSegmentStatus string

const (
	MediaSegmentPartial   MediaSegmentStatus = "partial"
	MediaSegmentComplete  MediaSegmentStatus = "complete"
	MediaSegmentRecovered MediaSegmentStatus = "recovered"
	MediaSegmentCorrupt   MediaSegmentStatus = "corrupt"
	MediaSegmentMissing   MediaSegmentStatus = "missing"
)

type MediaArtifactKind string

const (
	MediaArtifactASRWAV      MediaArtifactKind = "asr_wav"
	MediaArtifactPlaybackMP4 MediaArtifactKind = "playback_mp4"
)

type MediaArtifactStatus string

const (
	MediaArtifactPending          MediaArtifactStatus = "pending"
	MediaArtifactPendingTranscode MediaArtifactStatus = "pending_transcode"
	MediaArtifactComplete         MediaArtifactStatus = "complete"
	MediaArtifactFailed           MediaArtifactStatus = "failed"
	MediaArtifactMissing          MediaArtifactStatus = "missing"
	MediaArtifactNotApplicable    MediaArtifactStatus = "not_applicable"
)

// MediaAttempt is deliberately URL-free and path-free. It is safe to persist
// in media.json after validateMediaAttempt accepts every free-form token.
type MediaAttempt struct {
	ID             string `json:"id"`
	Ordinal        int    `json:"ordinal"`
	StartedAt      int64  `json:"startedAt"`
	SegmentSeconds int    `json:"segmentSeconds"`
	Committed      bool   `json:"committed"`
	Clean          bool   `json:"clean"`
	VariantID      string `json:"variantId,omitempty"`
	Protocol       string `json:"protocol"`
	QualityKey     string `json:"qualityKey,omitempty"`
	Quality        string `json:"quality,omitempty"`
	Codec          string `json:"codec"`
	Bitrate        int64  `json:"bitrate,omitempty"`
}

func (attempt MediaAttempt) String() string {
	return fmt.Sprintf("MediaAttempt{id:%s ordinal:%d committed:%t clean:%t}",
		mediaCorrelationID(attempt.ID), attempt.Ordinal, attempt.Committed, attempt.Clean)
}

func (attempt MediaAttempt) GoString() string { return attempt.String() }

func (attempt MediaAttempt) MarshalJSON() ([]byte, error) {
	if err := validateMediaAttempt(attempt); err != nil {
		return nil, err
	}
	type wire MediaAttempt
	return json.Marshal(wire(attempt))
}

type SessionMedia struct {
	SessionID        string            `json:"sessionId"`
	RootID           *string           `json:"rootId,omitempty"`
	RelativePath     string            `json:"relativePath"`
	State            SessionMediaState `json:"state"`
	ManifestRevision int64             `json:"manifestRevision"`
	ManifestDirty    bool              `json:"-"`
	MediaEpochAt     *int64            `json:"mediaEpochAt,omitempty"`
	Attempts         []MediaAttempt    `json:"attempts"`
	CreatedAt        int64             `json:"createdAt"`
	UpdatedAt        int64             `json:"updatedAt"`
}

func (media SessionMedia) String() string {
	return fmt.Sprintf("SessionMedia{session:%s state:%s revision:%d path:<redacted>}",
		mediaCorrelationID(media.SessionID), media.State, media.ManifestRevision)
}

func (media SessionMedia) GoString() string { return media.String() }

type MediaSegment struct {
	ID                 string             `json:"id"`
	Sequence           int                `json:"sequence"`
	RelativePath       string             `json:"relativePath"`
	Container          string             `json:"container"`
	VideoCodec         string             `json:"videoCodec,omitempty"`
	AudioCodec         string             `json:"audioCodec,omitempty"`
	StartedAt          int64              `json:"startedAt"`
	EndedAt            int64              `json:"endedAt"`
	PTSStartMS         *int64             `json:"ptsStartMs,omitempty"`
	PTSEndMS           *int64             `json:"ptsEndMs,omitempty"`
	DurationMS         int64              `json:"durationMs"`
	SizeBytes          int64              `json:"sizeBytes"`
	SHA256             string             `json:"sha256,omitempty"`
	Status             MediaSegmentStatus `json:"status"`
	AttemptID          string             `json:"attemptId"`
	AttemptSequence    int                `json:"attemptSequence"`
	SourceRelativePath string             `json:"sourceRelativePath,omitempty"`
	ProbeVersion       string             `json:"probeVersion,omitempty"`
	ErrorCode          string             `json:"errorCode,omitempty"`
}

func (segment MediaSegment) String() string {
	return fmt.Sprintf("MediaSegment{id:%s sequence:%d status:%s paths:<redacted>}",
		mediaCorrelationID(segment.ID), segment.Sequence, segment.Status)
}

func (segment MediaSegment) GoString() string { return segment.String() }

type MediaArtifact struct {
	ID             string              `json:"id"`
	MediaSegmentID string              `json:"mediaSegmentId"`
	Kind           MediaArtifactKind   `json:"kind"`
	RelativePath   string              `json:"relativePath"`
	Container      string              `json:"container,omitempty"`
	Codec          string              `json:"codec,omitempty"`
	DurationMS     int64               `json:"durationMs"`
	SizeBytes      int64               `json:"sizeBytes"`
	SampleRate     int                 `json:"sampleRate,omitempty"`
	Channels       int                 `json:"channels,omitempty"`
	SHA256         string              `json:"sha256,omitempty"`
	SourceSHA256   string              `json:"sourceSha256,omitempty"`
	Status         MediaArtifactStatus `json:"status"`
	ErrorCode      string              `json:"errorCode,omitempty"`
	CreatedAt      int64               `json:"createdAt"`
	UpdatedAt      int64               `json:"updatedAt"`
}

func (artifact MediaArtifact) String() string {
	return fmt.Sprintf("MediaArtifact{id:%s kind:%s status:%s path:<redacted>}",
		mediaCorrelationID(artifact.ID), artifact.Kind, artifact.Status)
}

func (artifact MediaArtifact) GoString() string { return artifact.String() }

type MediaSnapshot struct {
	Session   SessionMedia    `json:"session"`
	Segments  []MediaSegment  `json:"segments"`
	Artifacts []MediaArtifact `json:"artifacts"`
}

func (snapshot MediaSnapshot) String() string {
	return fmt.Sprintf("MediaSnapshot{session:%s revision:%d segments:%d artifacts:%d}",
		mediaCorrelationID(snapshot.Session.SessionID), snapshot.Session.ManifestRevision,
		len(snapshot.Segments), len(snapshot.Artifacts))
}

func (snapshot MediaSnapshot) GoString() string { return snapshot.String() }

type MediaFinalizeResult struct {
	Snapshot     MediaSnapshot `json:"snapshot"`
	WarningCodes []string      `json:"warningCodes,omitempty"`
}

type SessionMediaFinalizer interface {
	Finalize(context.Context, []MediaAttempt) (MediaFinalizeResult, error)
}

// SessionMediaAttemptJournal durably records the URL-free recorder attempt
// lifecycle before a process can create media. Implementations must use a
// compare-and-swap update so a stale recorder cannot overwrite newer state.
type SessionMediaAttemptJournal interface {
	AppendMediaAttempt(context.Context, MediaAttempt) error
	UpdateMediaAttempt(context.Context, MediaAttempt) error
}

type OpenSessionMediaInput struct {
	SessionID    string
	RootID       *string
	RelativePath string
	StartedAt    int64
}

type PersistMediaSnapshotInput struct {
	SessionID        string
	ExpectedRevision int64
	State            SessionMediaState
	MediaEpochAt     *int64
	Attempts         []MediaAttempt
	Segments         []MediaSegment
	Artifacts        []MediaArtifact
	UpdatedAt        int64
}

func validSessionMediaState(state SessionMediaState) bool {
	switch state {
	case SessionMediaOpen, SessionMediaFinalizing, SessionMediaCompleted, SessionMediaIncomplete:
		return true
	default:
		return false
	}
}

func validMediaSegmentStatus(status MediaSegmentStatus) bool {
	switch status {
	case MediaSegmentPartial, MediaSegmentComplete, MediaSegmentRecovered, MediaSegmentCorrupt, MediaSegmentMissing:
		return true
	default:
		return false
	}
}

func validMediaArtifactKind(kind MediaArtifactKind) bool {
	return kind == MediaArtifactASRWAV || kind == MediaArtifactPlaybackMP4
}

func validMediaArtifactStatus(status MediaArtifactStatus) bool {
	switch status {
	case MediaArtifactPending, MediaArtifactComplete, MediaArtifactFailed,
		MediaArtifactMissing, MediaArtifactNotApplicable, MediaArtifactPendingTranscode:
		return true
	default:
		return false
	}
}

func normalizeMediaAttempts(attempts []MediaAttempt) ([]MediaAttempt, []byte, error) {
	if len(attempts) > maximumMediaAttempts {
		return nil, nil, ErrMediaContractInvalid
	}
	normalized := make([]MediaAttempt, len(attempts))
	copy(normalized, attempts)
	sort.Slice(normalized, func(i, j int) bool {
		if normalized[i].Ordinal == normalized[j].Ordinal {
			return normalized[i].ID < normalized[j].ID
		}
		return normalized[i].Ordinal < normalized[j].Ordinal
	})
	seenIDs := make(map[string]struct{}, len(normalized))
	seenOrdinals := make(map[int]struct{}, len(normalized))
	for _, attempt := range normalized {
		if err := validateMediaAttempt(attempt); err != nil {
			return nil, nil, err
		}
		if _, exists := seenIDs[attempt.ID]; exists {
			return nil, nil, ErrMediaContractInvalid
		}
		if _, exists := seenOrdinals[attempt.Ordinal]; exists {
			return nil, nil, ErrMediaContractInvalid
		}
		seenIDs[attempt.ID] = struct{}{}
		seenOrdinals[attempt.Ordinal] = struct{}{}
	}
	payload, err := json.Marshal(normalized)
	if err != nil || len(payload) > maxMediaAttemptsJSONBytes {
		return nil, nil, ErrMediaContractInvalid
	}
	return normalized, payload, nil
}

func decodeMediaAttempts(payload string) ([]MediaAttempt, error) {
	if len(payload) > maxMediaAttemptsJSONBytes {
		return nil, ErrMediaContractInvalid
	}
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.DisallowUnknownFields()
	var attempts []MediaAttempt
	if err := decoder.Decode(&attempts); err != nil {
		return nil, ErrMediaContractInvalid
	}
	if decoder.More() {
		return nil, ErrMediaContractInvalid
	}
	normalized, encoded, err := normalizeMediaAttempts(attempts)
	if err != nil || string(encoded) != payload {
		return nil, ErrMediaContractInvalid
	}
	return normalized, nil
}

func validateMediaAttempt(attempt MediaAttempt) error {
	if validateUUIDv7("media attempt id", attempt.ID) != nil || attempt.Ordinal < 1 ||
		attempt.StartedAt < 0 || attempt.SegmentSeconds < 300 || attempt.SegmentSeconds > 1800 ||
		attempt.Bitrate < 0 || !validMediaProtocol(attempt.Protocol) || !validMediaCodec(attempt.Codec) ||
		!validMediaSafeToken(attempt.VariantID, true) || !validMediaSafeToken(attempt.QualityKey, true) ||
		!validMediaSafeToken(attempt.Quality, true) {
		return ErrMediaContractInvalid
	}
	return nil
}

func validMediaProtocol(value string) bool {
	return value == "flv" || value == "hls" || value == "unknown"
}

func validMediaCodec(value string) bool {
	return value == "h264" || value == "h265" || value == "unknown"
}

func validMediaSafeToken(value string, allowEmpty bool) bool {
	if value == "" {
		return allowEmpty
	}
	if len(value) > maxMediaSafeTokenBytes || !utf8.ValidString(value) || strings.Contains(value, "://") ||
		strings.ContainsAny(value, "?&=\\/\r\n\t") {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validMediaRelativePath(value string) bool {
	if value == "" || len(value) > maxMediaRelativePathBytes || !utf8.ValidString(value) || strings.Contains(value, `\`) ||
		strings.ContainsAny(value, "%:") ||
		strings.ContainsAny(value, "?\r\n\t") || path.IsAbs(value) {
		return false
	}
	cleaned := path.Clean(value)
	if cleaned != value || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return validMediaPlatformRelativePath(value)
}

func validMediaErrorCode(value string) bool {
	if value == "" {
		return true
	}
	if len(value) > 96 {
		return false
	}
	for _, character := range value {
		if (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}

func validMediaDigest(value string) bool {
	if value == "" {
		return true
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && strings.ToLower(value) == value
}

func mediaCorrelationID(value string) string {
	if validateUUIDv7("media correlation id", value) == nil {
		return value
	}
	return "invalid"
}
