package capture

import (
	"context"
	"errors"
)

var (
	ErrRecoveryContractInvalid = errors.New("RECOVERY_CONTRACT_INVALID")
	ErrRecoveryPersistence     = errors.New("RECOVERY_PERSISTENCE_FAILED")
	ErrRecoveryGapConflict     = errors.New("RECOVERY_GAP_CONFLICT")
	ErrStaleRecovery           = errors.New("RECOVERY_STALE")
)

const (
	maximumRecoverablePageSize = 128
	maximumRecoveryGaps        = 32
	maximumRecoveryDetailsJSON = 64 << 10
)

type RecoverablePageQuery struct {
	ScanCutoffMS int64
	AfterID      string
	Limit        int
}

type RecoverableSessionPage struct {
	Sessions []LiveSession
	NextID   string
}

type RecoveryGapInput struct {
	ID             string
	MediaSegmentID string
	Kind           string
	StartedAtMS    int64
	EndedAtMS      *int64
	Severity       string
	Recovered      bool
	ReasonCode     string
	DetailsJSON    string
	DedupeKey      string
}

type RecoverAndCloseInput struct {
	SessionID               string
	ExpectedStatus          SessionStatus
	ExpectedRecordingStatus RecordingStatus
	ExpectedOperationID     string
	RecoveryOperationID     string
	ScanCutoffMS            int64
	EndedAtMS               int64
	IntegrityScore          float64
	Gaps                    []RecoveryGapInput
}

// RecoveryRepository is deliberately separate from SessionRepository. Live
// coordination does not need startup-recovery paging or terminal repair
// primitives, and keeping the interfaces separate avoids widening every live
// coordinator test double.
type RecoveryRepository interface {
	ListRecoverablePage(context.Context, RecoverablePageQuery) (RecoverableSessionPage, error)
	RecoverAndClose(context.Context, RecoverAndCloseInput) (LiveSession, error)
}
