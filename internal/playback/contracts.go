package playback

import "errors"

const (
	ContractVersion = 1
	CursorVersion   = 1
	MaxPageSize     = 100
	DefaultPageSize = 50
)

var (
	ErrInvalidArgument  = errors.New("playback query argument is invalid")
	ErrInvalidCursor    = errors.New("playback query cursor is invalid")
	ErrSessionNotFound  = errors.New("playback session not found")
	ErrMediaNotFound    = errors.New("playback media not found")
	ErrMediaUnavailable = errors.New("playback media is unavailable")
)

type PageRequest struct {
	Limit  int
	Cursor string
}

type SessionFilter struct {
	RoomConfigID string
	Statuses     []string
	StartedAtMin *int64
	StartedAtMax *int64
}

type EventFilter struct {
	SessionID string
	Kinds     []string
	Roles     []string
	OffsetMin *int64
	OffsetMax *int64
}

type GapFilter struct {
	SessionID string
	Kinds     []string
	Recovered *bool
	OffsetMin *int64
	OffsetMax *int64
}

// SessionDTO is the playback-facing allowlist. It excludes live IDs,
// platform room IDs, operation IDs, filesystem paths, manifest journals and
// recording-root identity.
type SessionDTO struct {
	ID                string  `json:"id"`
	RoomConfigID      string  `json:"roomConfigId"`
	RoomAlias         string  `json:"roomAlias"`
	Title             string  `json:"title"`
	Status            string  `json:"status"`
	RecordingStatus   string  `json:"recordingStatus"`
	StartedAt         int64   `json:"startedAt"`
	EndedAt           *int64  `json:"endedAt,omitempty"`
	MediaEpochAt      *int64  `json:"mediaEpochAt,omitempty"`
	CaptureOffsetMS   int64   `json:"captureOffsetMs"`
	ClockSource       string  `json:"clockSource"`
	IntegrityScore    float64 `json:"integrityScore"`
	SessionMediaState string  `json:"sessionMediaState,omitempty"`
}

type SessionResult struct {
	Version int        `json:"version"`
	Session SessionDTO `json:"session"`
}

type SessionPage struct {
	Version    int          `json:"version"`
	Items      []SessionDTO `json:"items"`
	NextCursor string       `json:"nextCursor,omitempty"`
}

// EventDTO intentionally excludes method names, platform message IDs,
// dedupe keys, user hashes, normalized JSON and raw payload references.
type EventDTO struct {
	ID              string   `json:"id"`
	IngestSequence  int64    `json:"ingestSequence"`
	Role            string   `json:"role"`
	Kind            string   `json:"kind"`
	ReceivedAt      int64    `json:"receivedAt"`
	SessionOffsetMS int64    `json:"sessionOffsetMs"`
	ClockConfidence float64  `json:"clockConfidence"`
	DisplayName     string   `json:"displayName,omitempty"`
	Content         string   `json:"content,omitempty"`
	NumericValue    *float64 `json:"numericValue,omitempty"`
	ParseStatus     string   `json:"parseStatus"`
}

type EventPage struct {
	Version    int        `json:"version"`
	Items      []EventDTO `json:"items"`
	NextCursor string     `json:"nextCursor,omitempty"`
}

// GapDTO excludes details_json and durable dedupe keys. Those fields are
// recovery evidence and may contain implementation or diagnostic material
// that is not part of the playback contract.
type GapDTO struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	StartedAt     int64  `json:"startedAt"`
	EndedAt       *int64 `json:"endedAt,omitempty"`
	StartOffsetMS int64  `json:"startOffsetMs"`
	EndOffsetMS   *int64 `json:"endOffsetMs,omitempty"`
	Severity      string `json:"severity"`
	Recovered     bool   `json:"recovered"`
	ReasonCode    string `json:"reasonCode"`
}

type GapPage struct {
	Version    int      `json:"version"`
	Items      []GapDTO `json:"items"`
	NextCursor string   `json:"nextCursor,omitempty"`
}
