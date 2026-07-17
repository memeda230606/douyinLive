package eventstore

import "time"

// ContractVersion identifies the durable event/spool contract implemented by
// this package. It is intentionally independent from the SQLite schema version.
const ContractVersion = 1

type EventKind string

const (
	EventChat    EventKind = "chat"
	EventGift    EventKind = "gift"
	EventLike    EventKind = "like"
	EventMember  EventKind = "member"
	EventFollow  EventKind = "follow"
	EventSystem  EventKind = "system"
	EventUnknown EventKind = "unknown"
)

// EventRole distinguishes a platform source event from a durable aggregate
// derived from source events, such as a finalized gift combo.
type EventRole string

const (
	EventRoleSource    EventRole = "source"
	EventRoleAggregate EventRole = "aggregate"
)

type ParseStatus string

const (
	ParseParsed  ParseStatus = "parsed"
	ParseUnknown ParseStatus = "unknown"
	ParseFailed  ParseStatus = "failed"
)

type ComboStatus string

const (
	ComboOpen   ComboStatus = "open"
	ComboClosed ComboStatus = "closed"
)

type CheckpointState string

const (
	CheckpointOpen     CheckpointState = "open"
	CheckpointClosing  CheckpointState = "closing"
	CheckpointClosed   CheckpointState = "closed"
	CheckpointDegraded CheckpointState = "degraded"
)

// RawRef points into a session-relative raw binpack. Absolute paths never cross
// this boundary or enter SQLite.
type RawRef struct {
	File   string `json:"file"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	CRC32C uint32 `json:"crc32c"`
}

// SpoolPosition is the durable byte position immediately after a WAL record.
type SpoolPosition struct {
	File   string `json:"file"`
	Offset int64  `json:"offset"`
}

// IngestEnvelope is copied in the live callback. Payload owns its backing
// storage and may safely outlive the upstream LiveMessage.
type IngestEnvelope struct {
	SessionID       string    `json:"session_id"`
	EventID         string    `json:"event_id"`
	Sequence        int64     `json:"sequence"`
	Method          string    `json:"method"`
	PlatformRoomID  string    `json:"platform_room_id,omitempty"`
	ReceivedAt      time.Time `json:"received_at"`
	SessionOffsetMS int64     `json:"session_offset_ms"`
	Payload         []byte    `json:"payload,omitempty"`
}

// Event is the normalized, privacy-filtered row written to live_events.
type Event struct {
	ID                string
	SessionID         string
	IngestSequence    int64
	Role              EventRole
	Method            string
	Kind              EventKind
	PlatformMessageID string
	DedupeKey         string
	MessageCreateAt   *time.Time
	ReceivedAt        time.Time
	SessionOffsetMS   int64
	ClockConfidence   float64
	UserHash          string
	DisplayName       string
	Content           string
	NumericValue      *float64
	NormalizedJSON    string
	Raw               RawRef
	ParseStatus       ParseStatus
	ParseErrorCode    string
	NormalizerVersion string
}

// LiveEvent is the explicit pipeline name retained alongside the concise Event.
type LiveEvent = Event

// GiftComboState is the durable fold state for one platform gift combo. Closed
// rows are terminal and must never be reopened.
type GiftComboState struct {
	SessionID         string
	ComboKey          string
	Status            ComboStatus
	UserHash          string
	GiftID            string
	GiftName          string
	TotalCount        int64
	TotalValue        *float64
	FirstSequence     int64
	LastSequence      int64
	StartedAt         time.Time
	UpdatedAt         time.Time
	ClosedAt          *time.Time
	AggregateEventID  string
	NormalizerVersion string
}

// GiftCombo is the pipeline-facing alias for the persisted combo state.
type GiftCombo = GiftComboState

// CaptureGap is a normalized availability gap. DedupeKey makes replay
// idempotent while retaining a stable event-persistence diagnostic.
type CaptureGap struct {
	ID             string
	SessionID      string
	MediaSegmentID string
	Kind           string
	StartedAt      time.Time
	EndedAt        *time.Time
	StartOffsetMS  int64
	EndOffsetMS    *int64
	Severity       string
	Recovered      bool
	ReasonCode     string
	DetailsJSON    string
	DedupeKey      string
}

// Checkpoint is the last fully committed sequence and its durable spool/raw
// byte positions. It advances in the same SQLite transaction as Batch.
type Checkpoint struct {
	SessionID         string
	CommittedSequence int64
	State             CheckpointState
	PrivacyKeyID      string
	Spool             SpoolPosition
	Raw               SpoolPosition
	UpdatedAt         time.Time
}

// Batch is one ordered atomic SQLite commit. PreviousSequence is the compare-
// and-swap predecessor; a replay may supply the already committed checkpoint.
type Batch struct {
	SessionID        string
	PreviousSequence int64
	Events           []Event
	GiftCombos       []GiftComboState
	Gaps             []CaptureGap
	Checkpoint       Checkpoint
}

// SpoolRecord is the versioned WAL representation shared by the spool and
// recovery pipeline. Event/Combo/Gaps may be omitted before normalization.
type SpoolRecord struct {
	Version  int             `json:"version"`
	Envelope IngestEnvelope  `json:"envelope"`
	Raw      RawRef          `json:"raw"`
	Event    *Event          `json:"event,omitempty"`
	Combo    *GiftComboState `json:"combo,omitempty"`
	Gaps     []CaptureGap    `json:"gaps,omitempty"`
}
