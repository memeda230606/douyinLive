//go:build !windows

package capture

import (
	"context"
	"sync"
)

type portableProcessJob struct {
	mu      sync.Mutex
	command processCommand
	closed  bool
}

func (c *execProcessCommand) configure() error {
	return nil
}

func (c *execProcessCommand) resume() error {
	return nil
}

func newPlatformProcessJob(jobNamespace, attemptID string) (processJob, error) {
	if attemptID == "" {
		if jobNamespace != "" {
			return nil, errManagedProcessConfiguration
		}
	} else {
		if _, valid := recorderAttemptJobName(jobNamespace, attemptID); !valid {
			return nil, errManagedProcessConfiguration
		}
	}
	return &portableProcessJob{}, nil
}

func recoverPlatformRecorderAttemptProcess(ctx context.Context, _ string) (RecorderProcessRecoveryResult, error) {
	if ctx == nil {
		return recorderProcessRecoveryFailure(false, false, RecorderProcessRecoveryContextErrorCode, errRecorderProcessRecoveryContext)
	}
	if ctx.Err() != nil {
		return recorderProcessRecoveryFailure(false, false, RecorderProcessRecoveryInterruptedErrorCode, errRecorderProcessRecoveryInterrupted)
	}
	return recorderProcessRecoveryFailure(false, false, RecorderProcessRecoveryOpenErrorCode, errRecorderProcessRecoveryOpen)
}

func (j *portableProcessJob) assign(command processCommand) error {
	if command == nil {
		return errManagedProcessIsolation
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return errManagedProcessIsolation
	}
	j.command = command
	return nil
}

func (j *portableProcessJob) terminate(uint32) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || j.command == nil {
		return nil
	}
	return maskedProcessError(j.command.kill(), errManagedProcessControl)
}

func (j *portableProcessJob) close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	// The private CommandContext cancellation in managedProcess.close is the
	// non-Windows fallback. A native process-tree primitive is only guaranteed
	// by the Windows Job implementation used by this application.
	return nil
}
