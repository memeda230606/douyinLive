//go:build !windows

package capture

import (
	"context"
	"errors"
	"testing"
)

func TestPortableRecorderAttemptRecoveryFailsClosedWithoutDurableProcessEvidence(t *testing.T) {
	result, err := recoverPlatformRecorderAttemptProcess(
		context.Background(),
		recorderAttemptJobNamePrefix+"0123456789abcdef0123456789abcdef.0198f6e4-4d00-7000-8000-000000000001",
	)
	if result.Status != RecorderProcessRecoveryFailed || result.Found ||
		result.Terminated || result.ErrorCode != RecorderProcessRecoveryOpenErrorCode ||
		!errors.Is(err, errRecorderProcessRecoveryOpen) {
		t.Fatalf("portable recovery result = (%#v, %v)", result, err)
	}
}

func TestPortableRecorderAttemptRecoveryPreservesInterruption(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := recoverPlatformRecorderAttemptProcess(ctx, "opaque-job-name")
	if result.Status != RecorderProcessRecoveryFailed ||
		result.ErrorCode != RecorderProcessRecoveryInterruptedErrorCode ||
		!errors.Is(err, errRecorderProcessRecoveryInterrupted) {
		t.Fatalf("portable interrupted result = (%#v, %v)", result, err)
	}
}
