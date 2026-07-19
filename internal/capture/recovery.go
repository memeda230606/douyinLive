package capture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

var (
	ErrStartupRecoveryConfiguration = errors.New("STARTUP_RECOVERY_CONFIGURATION_INVALID")
	ErrStartupRecoveryIncomplete    = errors.New("STARTUP_RECOVERY_INCOMPLETE")
	ErrStartupProcessRecovery       = errors.New("STARTUP_PROCESS_RECOVERY_FAILED")
	ErrStartupEventRecoveryDeferred = errors.New("STARTUP_EVENT_RECOVERY_DEFERRED")
)

const (
	StartupRecoveryCompletedErrorCode    = ""
	StartupRecoveryMediaMissingCode      = "MEDIA_RECOVERY_SNAPSHOT_MISSING"
	StartupRecoveryMediaIncompleteCode   = "MEDIA_RECOVERY_INCOMPLETE"
	StartupRecoveryProcessFailedCode     = "SESSION_PROCESS_RECOVERY_FAILED"
	StartupRecoveryProcessTerminatedCode = "SESSION_PROCESS_RECOVERY_TERMINATED"
	StartupRecoveryEventFailedCode       = "EVENT_RECOVERY_INCOMPLETE"
	StartupRecoveryClockWarningCode      = "STARTUP_RECOVERY_CLOCK_UNCERTAIN"
	StartupRecoverySessionFailedCode     = "SESSION_RECOVERY_INCOMPLETE"
	defaultStartupRecoveryPageSize       = 128
)

type SessionEventRecoverer interface {
	RecoverAndCloseEvents(context.Context, LiveSession, time.Time) (time.Time, error)
}

type SessionEventRecoveryFunc func(
	context.Context,
	LiveSession,
	time.Time,
) (time.Time, error)

func (function SessionEventRecoveryFunc) RecoverAndCloseEvents(
	ctx context.Context,
	session LiveSession,
	minimumCutoff time.Time,
) (time.Time, error) {
	if function == nil {
		return time.Time{}, ErrStartupRecoveryConfiguration
	}
	return function(ctx, session, minimumCutoff)
}

type StartupRecoveryState string

const (
	StartupRecoverySessionCompleted StartupRecoveryState = "completed"
	StartupRecoverySessionFailed    StartupRecoveryState = "failed"
)

type StartupRecoveryEvent struct {
	SessionID    string
	RoomConfigID string
	State        StartupRecoveryState
	ErrorCode    string
	WarningCodes []string
	CutoffAtMS   int64
}

func (event StartupRecoveryEvent) String() string {
	return fmt.Sprintf(
		"StartupRecoveryEvent{session:%s room:%s state:%s code:%s warnings:%d cutoff:%d}",
		mediaCorrelationID(event.SessionID), mediaCorrelationID(event.RoomConfigID),
		event.State, event.ErrorCode, len(event.WarningCodes), event.CutoffAtMS,
	)
}

func (event StartupRecoveryEvent) GoString() string { return event.String() }

type StartupRecoveryReporter interface {
	ReportStartupRecovery(StartupRecoveryEvent)
}

type StartupRecoveryReporterFunc func(StartupRecoveryEvent)

func (function StartupRecoveryReporterFunc) ReportStartupRecovery(event StartupRecoveryEvent) {
	if function != nil {
		function(event)
	}
}

type StartupRecoveryOptions struct {
	Repository       RecoveryRepository
	ProcessRecoverer SessionProcessRecoverer
	MediaRecoverer   SessionMediaRecoverer
	EventRecoverer   SessionEventRecoverer
	Reporter         StartupRecoveryReporter
	Now              func() time.Time
	NewID            func() (string, error)
	PageSize         int
}

type StartupRecoveryReport struct {
	ScanCutoffMS int64
	Scanned      int
	Recovered    int
	Failed       int
	Warnings     int
}

func (report StartupRecoveryReport) String() string {
	return fmt.Sprintf(
		"StartupRecoveryReport{cutoff:%d scanned:%d recovered:%d failed:%d warnings:%d}",
		report.ScanCutoffMS, report.Scanned, report.Recovered,
		report.Failed, report.Warnings,
	)
}

func (report StartupRecoveryReport) GoString() string { return report.String() }

type startupRecoveryCoordinator struct {
	repository       RecoveryRepository
	processRecoverer SessionProcessRecoverer
	mediaRecoverer   SessionMediaRecoverer
	eventRecoverer   SessionEventRecoverer
	reporter         StartupRecoveryReporter
	now              func() time.Time
	newID            func() (string, error)
	pageSize         int
}

func RecoverStartupSessions(
	ctx context.Context,
	options StartupRecoveryOptions,
) (StartupRecoveryReport, error) {
	coordinator, err := newStartupRecoveryCoordinator(options)
	if err != nil {
		return StartupRecoveryReport{}, err
	}
	return coordinator.recover(ctx)
}

func newStartupRecoveryCoordinator(
	options StartupRecoveryOptions,
) (*startupRecoveryCoordinator, error) {
	if options.Repository == nil || options.ProcessRecoverer == nil ||
		options.EventRecoverer == nil {
		return nil, ErrStartupRecoveryConfiguration
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.NewID == nil {
		options.NewID = func() (string, error) {
			id, err := uuid.NewV7()
			return id.String(), err
		}
	}
	if options.PageSize == 0 {
		options.PageSize = defaultStartupRecoveryPageSize
	}
	if options.PageSize < 1 || options.PageSize > maximumRecoverablePageSize {
		return nil, ErrStartupRecoveryConfiguration
	}
	return &startupRecoveryCoordinator{
		repository:       options.Repository,
		processRecoverer: options.ProcessRecoverer,
		mediaRecoverer:   options.MediaRecoverer,
		eventRecoverer:   options.EventRecoverer,
		reporter:         options.Reporter,
		now:              options.Now,
		newID:            options.NewID,
		pageSize:         options.PageSize,
	}, nil
}

func (coordinator *startupRecoveryCoordinator) recover(
	ctx context.Context,
) (StartupRecoveryReport, error) {
	var report StartupRecoveryReport
	if ctx == nil {
		return report, ErrStartupRecoveryConfiguration
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	scanCutoff := coordinator.now().UTC()
	if scanCutoff.IsZero() || scanCutoff.UnixMilli() <= 0 {
		return report, ErrStartupRecoveryConfiguration
	}
	report.ScanCutoffMS = scanCutoff.UnixMilli()
	afterID := ""
	processRecoveryFailed := false
	eventRecoveryDeferred := false
	for {
		page, err := coordinator.repository.ListRecoverablePage(ctx, RecoverablePageQuery{
			ScanCutoffMS: report.ScanCutoffMS,
			AfterID:      afterID,
			Limit:        coordinator.pageSize,
		})
		if err != nil {
			if interrupted := startupRecoveryInterruption(ctx, err); interrupted != nil {
				return report, interrupted
			}
			return report, startupRecoveryFailure(err, true, eventRecoveryDeferred)
		}
		if !validStartupRecoveryPage(page, afterID, coordinator.pageSize) {
			return report, startupRecoveryFailure(nil, true, eventRecoveryDeferred)
		}
		for _, session := range page.Sessions {
			if err := ctx.Err(); err != nil {
				return report, err
			}
			report.Scanned++
			warnings, cutoff, recoveryErr := coordinator.recoverSession(
				ctx, session, scanCutoff,
			)
			if interrupted := startupRecoveryInterruption(ctx, recoveryErr); interrupted != nil {
				return report, interrupted
			}
			report.Warnings += len(warnings)
			if recoveryErr != nil {
				report.Failed++
				if errors.Is(recoveryErr, ErrStartupProcessRecovery) {
					processRecoveryFailed = true
				}
				if errors.Is(recoveryErr, ErrStartupEventRecoveryDeferred) {
					eventRecoveryDeferred = true
				}
				coordinator.report(StartupRecoveryEvent{
					SessionID: session.ID, RoomConfigID: session.RoomConfigID,
					State:        StartupRecoverySessionFailed,
					ErrorCode:    startupRecoveryErrorCode(recoveryErr),
					WarningCodes: warnings, CutoffAtMS: cutoff.UnixMilli(),
				})
				continue
			}
			report.Recovered++
			coordinator.report(StartupRecoveryEvent{
				SessionID: session.ID, RoomConfigID: session.RoomConfigID,
				State:        StartupRecoverySessionCompleted,
				ErrorCode:    StartupRecoveryCompletedErrorCode,
				WarningCodes: warnings, CutoffAtMS: cutoff.UnixMilli(),
			})
		}
		if page.NextID == "" {
			break
		}
		afterID = page.NextID
	}
	if report.Failed != 0 {
		return report, startupRecoveryFailure(
			nil, processRecoveryFailed, eventRecoveryDeferred,
		)
	}
	return report, nil
}

func startupRecoveryFailure(cause error, processRecoveryFailed, eventRecoveryDeferred bool) error {
	causes := []error{ErrStartupRecoveryIncomplete}
	if processRecoveryFailed {
		causes = append(causes, ErrStartupProcessRecovery)
	}
	if eventRecoveryDeferred {
		causes = append(causes, ErrStartupEventRecoveryDeferred)
	}
	if cause != nil {
		causes = append(causes, cause)
	}
	return errors.Join(causes...)
}

func startupRecoveryInterruption(ctx context.Context, _ error) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func validStartupRecoveryPage(
	page RecoverableSessionPage,
	afterID string,
	limit int,
) bool {
	if limit < 1 || len(page.Sessions) > limit {
		return false
	}
	previousID := afterID
	for _, session := range page.Sessions {
		if validateUUIDv7("startup recovery page session", session.ID) != nil ||
			(previousID != "" && session.ID <= previousID) {
			return false
		}
		previousID = session.ID
	}
	if page.NextID == "" {
		return len(page.Sessions) < limit
	}
	if len(page.Sessions) != limit ||
		validateUUIDv7("startup recovery page cursor", page.NextID) != nil {
		return false
	}
	return page.NextID == page.Sessions[len(page.Sessions)-1].ID
}

func (coordinator *startupRecoveryCoordinator) recoverSession(
	ctx context.Context,
	session LiveSession,
	scanCutoff time.Time,
) ([]string, time.Time, error) {
	if validateUUIDv7("startup recovery session", session.ID) != nil ||
		!activeSessionStatus(session.Status) || session.CreatedAt > scanCutoff.UnixMilli() ||
		session.StartedAt <= 0 || session.StartedAt > scanCutoff.UnixMilli() {
		err := error(ErrStartupRecoveryIncomplete)
		if session.RecordingStatus != RecordingDisabled {
			err = errors.Join(err, ErrStartupProcessRecovery)
		}
		return nil, time.Time{}, err
	}
	cutoff := time.UnixMilli(session.StartedAt).UTC()
	warnings := make([]string, 0, 8)
	var processResult SessionProcessRecoveryResult
	if session.RecordingStatus != RecordingDisabled {
		var processErr error
		processResult, processErr = coordinator.processRecoverer.RecoverSessionProcesses(
			ctx, session.ID,
		)
		if interrupted := startupRecoveryInterruption(ctx, processErr); interrupted != nil {
			return stableMediaWarnings(warnings), cutoff, interrupted
		}
		if processResult.ProcessesTerminated > 0 {
			warnings = append(warnings, StartupRecoveryProcessTerminatedCode)
		}
		if processErr != nil {
			if processResult.ErrorCode != "" {
				warnings = append(warnings, processResult.ErrorCode)
			} else {
				warnings = append(warnings, StartupRecoveryProcessFailedCode)
			}
			return stableMediaWarnings(warnings), cutoff,
				errors.Join(ErrStartupProcessRecovery, processErr)
		}
	}
	mediaRecovered := session.RecordingStatus == RecordingDisabled
	mediaOrphans := 0
	if session.RecordingStatus != RecordingDisabled {
		if coordinator.mediaRecoverer == nil {
			warnings = append(warnings, MediaRecoveryFailedWarning)
		} else {
			media, err := coordinator.mediaRecoverer.RecoverSessionMedia(ctx, session, scanCutoff)
			if interrupted := startupRecoveryInterruption(ctx, err); interrupted != nil {
				return stableMediaWarnings(warnings), cutoff, interrupted
			}
			if media.CutoffAt.After(cutoff) {
				cutoff = media.CutoffAt.UTC()
			}
			warnings = append(warnings, media.WarningCodes...)
			mediaOrphans = media.OrphanFiles
			switch {
			case err == nil:
				mediaRecovered = completedStartupMediaRecovery(media.Snapshot)
				if !mediaRecovered {
					warnings = append(warnings, StartupRecoveryMediaIncompleteCode)
				}
			case errors.Is(err, ErrSessionMediaNotFound):
				warnings = append(warnings, StartupRecoveryMediaMissingCode)
			default:
				warnings = append(warnings, MediaRecoveryFailedWarning)
			}
		}
	}
	eventRecovered := true
	effectiveCutoff, eventErr := coordinator.eventRecoverer.RecoverAndCloseEvents(
		ctx, session, cutoff,
	)
	effectiveCutoff = effectiveCutoff.UTC()
	validEffectiveCutoff := !effectiveCutoff.IsZero() && !effectiveCutoff.Before(cutoff)
	if validEffectiveCutoff && eventErr != nil && !errors.Is(eventErr, ErrStartupEventRecoveryDeferred) {
		cutoff = effectiveCutoff
	}
	if interrupted := startupRecoveryInterruption(ctx, eventErr); interrupted != nil {
		return stableMediaWarnings(warnings), cutoff, interrupted
	}
	switch {
	case errors.Is(eventErr, ErrStartupEventRecoveryDeferred):
		warnings = append(warnings, StartupRecoveryEventFailedCode)
		return stableMediaWarnings(warnings), cutoff, ErrStartupEventRecoveryDeferred
	case eventErr != nil:
		eventRecovered = false
		warnings = append(warnings, StartupRecoveryEventFailedCode)
	default:
		if !validEffectiveCutoff {
			eventRecovered = false
			warnings = append(warnings, StartupRecoveryEventFailedCode)
		} else {
			cutoff = effectiveCutoff
		}
	}
	clockUncertain := cutoff.After(scanCutoff) ||
		containsStartupRecoveryWarning(warnings, MediaRecoveryClockWarning)
	if clockUncertain {
		warnings = append(warnings, StartupRecoveryClockWarningCode)
	}
	warnings = stableMediaWarnings(warnings)

	recoveryOperationID, err := coordinator.nextID("startup recovery operation")
	if err != nil {
		return warnings, cutoff, err
	}
	gapID, err := coordinator.nextID("startup recovery gap")
	if err != nil {
		return warnings, cutoff, err
	}
	endedAt := cutoff.UnixMilli()
	startedAt := endedAt
	kind := "message_disconnect"
	if session.RecordingStatus != RecordingDisabled {
		kind = "process_crash"
	}
	severity := "warning"
	fullyRecovered := mediaRecovered && eventRecovered
	if !fullyRecovered {
		severity = "error"
	}
	details, _ := json.Marshal(struct {
		Source              string `json:"source"`
		Warnings            int    `json:"warnings"`
		OrphanFiles         int    `json:"orphanFiles"`
		MediaRecovered      bool   `json:"mediaRecovered"`
		EventRecovered      bool   `json:"eventRecovered"`
		AttemptsChecked     int    `json:"attemptsChecked"`
		ProcessesFound      int    `json:"processesFound"`
		ProcessesTerminated int    `json:"processesTerminated"`
	}{
		Source: "startup_recovery", Warnings: len(warnings),
		OrphanFiles: mediaOrphans, MediaRecovered: mediaRecovered,
		EventRecovered: eventRecovered, AttemptsChecked: processResult.AttemptsChecked,
		ProcessesFound:      processResult.ProcessesFound,
		ProcessesTerminated: processResult.ProcessesTerminated,
	})
	integrity := session.IntegrityScore
	if integrity <= 0 || integrity > 1 {
		integrity = 1
	}
	if fullyRecovered {
		integrity = min(integrity, 0.9)
	} else {
		integrity = min(integrity, 0.5)
	}
	gaps := []RecoveryGapInput{{
		ID: gapID, Kind: kind, StartedAtMS: startedAt, EndedAtMS: &endedAt,
		Severity: severity, Recovered: fullyRecovered,
		ReasonCode:  "STARTUP_RECOVERY_INTERRUPTED",
		DetailsJSON: string(details),
		DedupeKey:   "startup-recovery:" + session.ID + ":" + kind,
	}}
	if !eventRecovered {
		eventGapID, idErr := coordinator.nextID("startup event recovery gap")
		if idErr != nil {
			return warnings, cutoff, idErr
		}
		gaps = append(gaps, RecoveryGapInput{
			ID: eventGapID, Kind: "event_persistence", StartedAtMS: endedAt, EndedAtMS: &endedAt,
			Severity: "error", Recovered: false,
			ReasonCode: "STARTUP_EVENT_RECOVERY_INCOMPLETE", DetailsJSON: string(details),
			DedupeKey: "startup-recovery:" + session.ID + ":event-persistence",
		})
	}
	if clockUncertain {
		clockGapID, idErr := coordinator.nextID("startup clock uncertainty gap")
		if idErr != nil {
			return warnings, cutoff, idErr
		}
		gaps = append(gaps, RecoveryGapInput{
			ID: clockGapID, Kind: "clock_uncertain", StartedAtMS: endedAt, EndedAtMS: &endedAt,
			Severity: "warning", Recovered: false,
			ReasonCode: "STARTUP_CLOCK_UNCERTAIN", DetailsJSON: string(details),
			DedupeKey: "startup-recovery:" + session.ID + ":clock-uncertain",
		})
	}
	_, err = coordinator.repository.RecoverAndClose(ctx, RecoverAndCloseInput{
		SessionID:               session.ID,
		ExpectedStatus:          session.Status,
		ExpectedRecordingStatus: session.RecordingStatus,
		ExpectedOperationID:     session.OperationID,
		RecoveryOperationID:     recoveryOperationID,
		ScanCutoffMS:            scanCutoff.UnixMilli(),
		EndedAtMS:               endedAt,
		IntegrityScore:          integrity,
		Gaps:                    gaps,
	})
	if err != nil {
		return warnings, cutoff, errors.Join(ErrStartupRecoveryIncomplete, err)
	}
	return warnings, cutoff, nil
}

func completedStartupMediaRecovery(snapshot MediaSnapshot) bool {
	if snapshot.Session.SessionID == "" || snapshot.Session.State != SessionMediaCompleted ||
		len(snapshot.Segments) == 0 {
		return false
	}
	for _, segment := range snapshot.Segments {
		if segment.Status != MediaSegmentComplete && segment.Status != MediaSegmentRecovered {
			return false
		}
	}
	return true
}

func containsStartupRecoveryWarning(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (coordinator *startupRecoveryCoordinator) nextID(label string) (string, error) {
	id, err := coordinator.newID()
	if err != nil || validateUUIDv7(label, id) != nil {
		return "", ErrStartupRecoveryIncomplete
	}
	return id, nil
}

func (coordinator *startupRecoveryCoordinator) report(event StartupRecoveryEvent) {
	event.SessionID = mediaCorrelationID(event.SessionID)
	event.RoomConfigID = mediaCorrelationID(event.RoomConfigID)
	event.WarningCodes = stableMediaWarnings(event.WarningCodes)
	if coordinator.reporter != nil {
		coordinator.reporter.ReportStartupRecovery(event)
	}
}

func startupRecoveryErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrStartupProcessRecovery):
		return StartupRecoveryProcessFailedCode
	case errors.Is(err, ErrMediaRecovery):
		return MediaRecoveryFailedWarning
	case errors.Is(err, ErrStaleRecovery), errors.Is(err, ErrRecoveryGapConflict),
		errors.Is(err, ErrRecoveryPersistence), errors.Is(err, ErrRecoveryContractInvalid):
		return StartupRecoverySessionFailedCode
	default:
		return StartupRecoveryEventFailedCode
	}
}
