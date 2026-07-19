package capture

import (
	"context"
	"errors"
	"fmt"
)

var ErrSessionProcessRecovery = errors.New("SESSION_PROCESS_RECOVERY_FAILED")

const (
	SessionProcessRecoveryCompletedErrorCode     = ""
	SessionProcessRecoveryConfigurationErrorCode = "SESSION_PROCESS_RECOVERY_CONFIGURATION_INVALID"
	SessionProcessRecoverySnapshotFailedCode     = "SESSION_PROCESS_RECOVERY_SNAPSHOT_FAILED"
	SessionProcessRecoveryProcessFailedCode      = "SESSION_PROCESS_RECOVERY_PROCESS_FAILED"
	SessionProcessRecoveryInterruptedCode        = "SESSION_PROCESS_RECOVERY_INTERRUPTED"
)

var (
	errSessionProcessRecoveryConfiguration = errors.New(
		SessionProcessRecoveryConfigurationErrorCode,
	)
	errSessionProcessRecoverySnapshot = errors.New(
		SessionProcessRecoverySnapshotFailedCode,
	)
	errSessionProcessRecoveryProcess = errors.New(
		SessionProcessRecoveryProcessFailedCode,
	)
	errSessionProcessRecoveryInspectionInvalid = errors.New(
		"SESSION_PROCESS_RECOVERY_INSPECTION_INVALID",
	)
)

type SessionProcessRecoveryState string

const (
	SessionProcessRecoveryClean     SessionProcessRecoveryState = "clean"
	SessionProcessRecoveryRecovered SessionProcessRecoveryState = "recovered"
	SessionProcessRecoveryFailed    SessionProcessRecoveryState = "failed"
)

// SessionProcessRecoveryResult deliberately carries no attempt ID, process ID,
// command line, filesystem path, stream URL, or native error text.
type SessionProcessRecoveryResult struct {
	AttemptsChecked     int                         `json:"attemptsChecked"`
	ProcessesFound      int                         `json:"processesFound"`
	ProcessesTerminated int                         `json:"processesTerminated"`
	State               SessionProcessRecoveryState `json:"state"`
	ErrorCode           string                      `json:"errorCode,omitempty"`
}

func (result SessionProcessRecoveryResult) String() string {
	return fmt.Sprintf(
		"SessionProcessRecoveryResult{state:%s checked:%d found:%d terminated:%d code:%s}",
		result.State, result.AttemptsChecked, result.ProcessesFound,
		result.ProcessesTerminated, result.ErrorCode,
	)
}

func (result SessionProcessRecoveryResult) GoString() string { return result.String() }

type SessionProcessRecoverer interface {
	RecoverSessionProcesses(
		context.Context,
		string,
	) (SessionProcessRecoveryResult, error)
}

type sessionMediaSnapshotLoader interface {
	LoadSnapshot(context.Context, string) (MediaSnapshot, error)
}

type sessionProcessInspector func(
	context.Context,
	string,
	string,
) (RecorderProcessRecoveryResult, error)

type durableSessionProcessRecoverer struct {
	loader         sessionMediaSnapshotLoader
	jobNamespace   string
	inspectProcess sessionProcessInspector
}

func NewSessionProcessRecoverer(
	repository *SQLiteRepository,
) (SessionProcessRecoverer, error) {
	if repository == nil {
		return nil, newSessionProcessRecoveryError(
			errSessionProcessRecoveryConfiguration,
		)
	}
	jobNamespace, valid := recorderJobNamespace(repository.dataRoot)
	if !valid {
		return nil, newSessionProcessRecoveryError(
			errSessionProcessRecoveryConfiguration,
		)
	}
	return newSessionProcessRecoverer(
		repository, jobNamespace, inspectRecorderAttemptProcess,
	)
}

func newSessionProcessRecoverer(
	loader sessionMediaSnapshotLoader,
	jobNamespace string,
	inspectProcess sessionProcessInspector,
) (SessionProcessRecoverer, error) {
	if loader == nil || !validRecorderJobNamespace(jobNamespace) || inspectProcess == nil {
		return nil, newSessionProcessRecoveryError(
			errSessionProcessRecoveryConfiguration,
		)
	}
	return &durableSessionProcessRecoverer{
		loader: loader, jobNamespace: jobNamespace,
		inspectProcess: inspectProcess,
	}, nil
}

func (recoverer *durableSessionProcessRecoverer) RecoverSessionProcesses(
	ctx context.Context,
	sessionID string,
) (SessionProcessRecoveryResult, error) {
	result := failedSessionProcessRecoveryResult(
		SessionProcessRecoveryConfigurationErrorCode,
	)
	if recoverer == nil || recoverer.loader == nil ||
		!validRecorderJobNamespace(recoverer.jobNamespace) ||
		recoverer.inspectProcess == nil || ctx == nil ||
		validateUUIDv7("session process recovery session", sessionID) != nil {
		return result, newSessionProcessRecoveryError(
			errSessionProcessRecoveryConfiguration,
		)
	}
	if err := ctx.Err(); err != nil {
		return interruptedSessionProcessRecoveryResult(result, err)
	}

	snapshot, err := recoverer.loader.LoadSnapshot(ctx, sessionID)
	if err != nil {
		if interrupted := sessionProcessRecoveryInterruption(ctx, err); interrupted != nil {
			return interruptedSessionProcessRecoveryResult(result, interrupted)
		}
		if errors.Is(err, ErrSessionMediaNotFound) {
			return cleanSessionProcessRecoveryResult(), nil
		}
		result.ErrorCode = SessionProcessRecoverySnapshotFailedCode
		return result, newSessionProcessRecoveryError(err, errSessionProcessRecoverySnapshot)
	}
	if err := ctx.Err(); err != nil {
		return interruptedSessionProcessRecoveryResult(result, err)
	}
	if snapshot.Session.SessionID != sessionID {
		result.ErrorCode = SessionProcessRecoverySnapshotFailedCode
		return result, newSessionProcessRecoveryError(
			errSessionProcessRecoverySnapshot,
		)
	}
	attempts, _, err := normalizeMediaAttempts(snapshot.Session.Attempts)
	if err != nil {
		result.ErrorCode = SessionProcessRecoverySnapshotFailedCode
		return result, newSessionProcessRecoveryError(
			err, errSessionProcessRecoverySnapshot,
		)
	}
	result = cleanSessionProcessRecoveryResult()
	for _, attempt := range attempts {
		if err := ctx.Err(); err != nil {
			return interruptedSessionProcessRecoveryResult(result, err)
		}
		process, inspectErr := recoverer.inspectProcess(
			ctx, recoverer.jobNamespace, attempt.ID,
		)
		result.AttemptsChecked++
		if process.Found {
			result.ProcessesFound++
		}
		if process.Terminated {
			result.ProcessesTerminated++
		}
		if inspectErr != nil || !validSessionProcessInspection(process) {
			result.State = SessionProcessRecoveryFailed
			result.ErrorCode = SessionProcessRecoveryProcessFailedCode
			if interrupted := sessionProcessRecoveryInterruption(ctx, inspectErr); interrupted != nil {
				return interruptedSessionProcessRecoveryResult(result, interrupted)
			}
			if inspectErr == nil {
				inspectErr = errSessionProcessRecoveryInspectionInvalid
			}
			return result, newSessionProcessRecoveryError(
				inspectErr, errSessionProcessRecoveryProcess,
			)
		}
		if process.Found {
			result.State = SessionProcessRecoveryRecovered
		}
	}
	return result, nil
}

func validSessionProcessInspection(result RecorderProcessRecoveryResult) bool {
	if result.ErrorCode != "" {
		return false
	}
	switch result.Status {
	case RecorderProcessRecoveryClean:
		return !result.Terminated
	case RecorderProcessRecoveryTerminated:
		return result.Found && result.Terminated
	default:
		return false
	}
}

func cleanSessionProcessRecoveryResult() SessionProcessRecoveryResult {
	return SessionProcessRecoveryResult{
		State: SessionProcessRecoveryClean,
	}
}

func failedSessionProcessRecoveryResult(errorCode string) SessionProcessRecoveryResult {
	return SessionProcessRecoveryResult{
		State: SessionProcessRecoveryFailed, ErrorCode: errorCode,
	}
}

func interruptedSessionProcessRecoveryResult(
	result SessionProcessRecoveryResult,
	cause error,
) (SessionProcessRecoveryResult, error) {
	result.State = SessionProcessRecoveryFailed
	result.ErrorCode = SessionProcessRecoveryInterruptedCode
	return result, newSessionProcessRecoveryError(cause)
}

func sessionProcessRecoveryInterruption(ctx context.Context, err error) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	default:
		return nil
	}
}

type sessionProcessRecoveryError struct {
	causes []error
}

func newSessionProcessRecoveryError(causes ...error) error {
	return &sessionProcessRecoveryError{causes: causes}
}

func (*sessionProcessRecoveryError) Error() string {
	return ErrSessionProcessRecovery.Error()
}

func (err *sessionProcessRecoveryError) GoString() string {
	return err.Error()
}

func (err *sessionProcessRecoveryError) Is(target error) bool {
	if target == ErrSessionProcessRecovery {
		return true
	}
	for _, cause := range err.causes {
		if cause != nil && errors.Is(cause, target) {
			return true
		}
	}
	return false
}
