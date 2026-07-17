// Package capture owns live-session contracts and persistence orchestration.
package capture

import (
	"context"
	"errors"
	"time"
)

const SessionManifestSchemaVersion = 1

type SessionStatus string

const (
	SessionStarting    SessionStatus = "starting"
	SessionRecording   SessionStatus = "recording"
	SessionFinalizing  SessionStatus = "finalizing"
	SessionCompleted   SessionStatus = "completed"
	SessionInterrupted SessionStatus = "interrupted"
	SessionFailed      SessionStatus = "failed"
)

type RecordingStatus string

const (
	RecordingPending      RecordingStatus = "pending"
	RecordingDisabled     RecordingStatus = "disabled"
	RecordingStarting     RecordingStatus = "starting"
	RecordingActive       RecordingStatus = "recording"
	RecordingUnavailable  RecordingStatus = "unavailable"
	RecordingReconnecting RecordingStatus = "reconnecting"
	RecordingFinalizing   RecordingStatus = "finalizing"
	RecordingCompleted    RecordingStatus = "completed"
	RecordingIncomplete   RecordingStatus = "incomplete"
	RecordingFailed       RecordingStatus = "failed"
)

type ClockSource string

const (
	ClockMedia      ClockSource = "media"
	ClockReceived   ClockSource = "received"
	ClockCalibrated ClockSource = "calibrated"
)

var (
	ErrSessionNotFound            = errors.New("live session not found")
	ErrActiveSessionExists        = errors.New("active live session already exists")
	ErrOperationConflict          = errors.New("session operation conflicts with existing data")
	ErrStaleTransition            = errors.New("stale live session transition")
	ErrManifestRepairRequired     = errors.New("capture session manifest requires repair")
	ErrManifestHealthReportFailed = errors.New("capture manifest health report failed")
)

const (
	ManifestRepairRequiredErrorCode   = "CAPTURE_MANIFEST_REPAIR_REQUIRED"
	ManifestRepairClearedErrorCode    = "CAPTURE_MANIFEST_REPAIR_CLEARED"
	ManifestRepairIncompleteErrorCode = "CAPTURE_MANIFEST_REPAIR_INCOMPLETE"
)

type ManifestHealthState string

const (
	ManifestHealthRepairRequired ManifestHealthState = "repair_required"
	ManifestHealthRepairCleared  ManifestHealthState = "repair_cleared"
)

// ManifestHealthEvent is intentionally sanitized: it contains no filesystem
// path and no underlying error text.
type ManifestHealthEvent struct {
	SessionID   string
	State       ManifestHealthState
	ErrorCode   string
	Outstanding int
}

type ManifestHealthReporter interface {
	ReportManifestHealth(ManifestHealthEvent) error
}

type ManifestHealthReporterFunc func(ManifestHealthEvent) error

func (reporter ManifestHealthReporterFunc) ReportManifestHealth(event ManifestHealthEvent) error {
	if reporter == nil {
		return nil
	}
	return reporter(event)
}

type ManifestHealthBatchReporter interface {
	ManifestHealthReporter
	BeginManifestHealthBatch() error
	EndManifestHealthBatch() error
}

type SQLiteRepositoryOptions struct {
	ManifestHealthReporter ManifestHealthReporter
}

type ManifestRepairReport struct {
	Scanned  int
	Repaired int
	Failed   int
}

type LiveSession struct {
	ID              string          `json:"id"`
	RoomConfigID    string          `json:"roomConfigId"`
	OperationID     string          `json:"operationId"`
	PlatformRoomID  string          `json:"platformRoomId,omitempty"`
	Title           string          `json:"title"`
	Status          SessionStatus   `json:"status"`
	RecordingStatus RecordingStatus `json:"recordingStatus"`
	ManifestDirty   bool            `json:"-"`
	StartedAt       int64           `json:"startedAt"`
	EndedAt         *int64          `json:"endedAt,omitempty"`
	MediaEpochAt    *int64          `json:"mediaEpochAt,omitempty"`
	CaptureOffsetMS int64           `json:"captureOffsetMs"`
	ClockSource     ClockSource     `json:"clockSource"`
	IntegrityScore  float64         `json:"integrityScore"`
	DataPath        string          `json:"dataPath"`
	SchemaVersion   int             `json:"schemaVersion"`
	CreatedAt       int64           `json:"createdAt"`
	UpdatedAt       int64           `json:"updatedAt"`
}

type CreateSessionInput struct {
	RoomConfigID   string
	OperationID    string
	PlatformRoomID string
	Title          string
	Recording      RecordingStatus
	StartedAt      time.Time
}

type TransitionSessionInput struct {
	ID                      string
	ExpectedStatus          SessionStatus
	ExpectedRecordingStatus RecordingStatus
	ExpectedOperationID     string
	Status                  SessionStatus
	RecordingStatus         RecordingStatus
	NextOperationID         string
	EndedAt                 *time.Time
	MediaEpochAt            *time.Time
	CaptureOffsetMS         *int64
	ClockSource             *ClockSource
	IntegrityScore          *float64
}

type SessionRepository interface {
	Create(context.Context, CreateSessionInput) (LiveSession, error)
	Get(context.Context, string) (LiveSession, error)
	ActiveForRoom(context.Context, string) (LiveSession, bool, error)
	Transition(context.Context, TransitionSessionInput) (LiveSession, error)
	ListRecoverable(context.Context) ([]LiveSession, error)
}

type ManifestRepairer interface {
	RepairManifests(context.Context) (ManifestRepairReport, error)
}

func validSessionStatus(status SessionStatus) bool {
	switch status {
	case SessionStarting, SessionRecording, SessionFinalizing, SessionCompleted, SessionInterrupted, SessionFailed:
		return true
	default:
		return false
	}
}

func activeSessionStatus(status SessionStatus) bool {
	return status == SessionStarting || status == SessionRecording || status == SessionFinalizing
}

func validRecordingStatus(status RecordingStatus) bool {
	switch status {
	case RecordingPending, RecordingDisabled, RecordingStarting, RecordingActive,
		RecordingUnavailable, RecordingReconnecting, RecordingFinalizing,
		RecordingCompleted, RecordingIncomplete, RecordingFailed:
		return true
	default:
		return false
	}
}

func validClockSource(source ClockSource) bool {
	return source == ClockMedia || source == ClockReceived || source == ClockCalibrated
}
