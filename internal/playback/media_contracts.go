package playback

type MediaFilter struct {
	SessionID string
	Statuses  []string
}

// MediaArtifactDTO excludes relative paths, content hashes and source hashes.
// A later media endpoint resolves the opaque artifact ID only after repeating
// the filesystem identity and containment checks.
type MediaArtifactDTO struct {
	ID             string `json:"id"`
	MediaSegmentID string `json:"mediaSegmentId"`
	Kind           string `json:"kind"`
	Container      string `json:"container"`
	Codec          string `json:"codec"`
	DurationMS     int64  `json:"durationMs"`
	SizeBytes      int64  `json:"sizeBytes"`
	SampleRate     int    `json:"sampleRate"`
	Channels       int    `json:"channels"`
	Status         string `json:"status"`
	ErrorCode      string `json:"errorCode,omitempty"`
	DirectPlayback bool   `json:"directPlayback"`
}

// MediaSegmentDTO is a path-free projection of a durable media segment.
type MediaSegmentDTO struct {
	ID                 string             `json:"id"`
	Sequence           int                `json:"sequence"`
	Container          string             `json:"container"`
	VideoCodec         string             `json:"videoCodec,omitempty"`
	AudioCodec         string             `json:"audioCodec,omitempty"`
	StartedAt          int64              `json:"startedAt"`
	EndedAt            int64              `json:"endedAt"`
	PTSStartMS         *int64             `json:"ptsStartMs,omitempty"`
	PTSEndMS           *int64             `json:"ptsEndMs,omitempty"`
	DurationMS         int64              `json:"durationMs"`
	SizeBytes          int64              `json:"sizeBytes"`
	Status             string             `json:"status"`
	ErrorCode          string             `json:"errorCode,omitempty"`
	TimelineStartMS    int64              `json:"timelineStartMs"`
	TimelineEndMS      int64              `json:"timelineEndMs"`
	Artifacts          []MediaArtifactDTO `json:"artifacts"`
	PlaybackArtifactID string             `json:"playbackArtifactId,omitempty"`
}

type MediaPage struct {
	Version    int               `json:"version"`
	Items      []MediaSegmentDTO `json:"items"`
	NextCursor string            `json:"nextCursor,omitempty"`
}

type MediaLocationState string

const (
	MediaLocationPlaybackMP4 MediaLocationState = "playback_mp4"
	MediaLocationSourceMKV   MediaLocationState = "source_mkv"
	MediaLocationGap         MediaLocationState = "gap"
)

type MediaLocationRequest struct {
	SessionID       string
	SessionOffsetMS int64
}

type MediaLocationResult struct {
	Version            int                `json:"version"`
	SessionID          string             `json:"sessionId"`
	RequestedOffsetMS  int64              `json:"requestedOffsetMs"`
	AdjustedOffsetMS   int64              `json:"adjustedOffsetMs"`
	State              MediaLocationState `json:"state"`
	ReasonCode         string             `json:"reasonCode,omitempty"`
	Segment            *MediaSegmentDTO   `json:"segment,omitempty"`
	SegmentPlaybackMS  *int64             `json:"segmentPlaybackMs,omitempty"`
	PlaybackArtifactID string             `json:"playbackArtifactId,omitempty"`
}
