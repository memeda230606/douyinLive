package capture

import "testing"

func TestFixedSessionEnums(t *testing.T) {
	for _, status := range []SessionStatus{
		SessionStarting, SessionRecording, SessionFinalizing, SessionCompleted, SessionInterrupted, SessionFailed,
	} {
		if !validSessionStatus(status) {
			t.Fatalf("validSessionStatus(%q) = false", status)
		}
	}
	for _, status := range []RecordingStatus{
		RecordingPending, RecordingDisabled, RecordingStarting, RecordingActive, RecordingUnavailable,
		RecordingReconnecting, RecordingFinalizing, RecordingCompleted, RecordingIncomplete, RecordingFailed,
	} {
		if !validRecordingStatus(status) {
			t.Fatalf("validRecordingStatus(%q) = false", status)
		}
	}
	if validRecordingStatus("interrupted") {
		t.Fatal("recording status interrupted must not be accepted; use incomplete")
	}
}

func TestActiveSessionStatuses(t *testing.T) {
	for _, status := range []SessionStatus{SessionStarting, SessionRecording, SessionFinalizing} {
		if !activeSessionStatus(status) {
			t.Fatalf("activeSessionStatus(%q) = false", status)
		}
	}
	for _, status := range []SessionStatus{SessionCompleted, SessionInterrupted, SessionFailed} {
		if activeSessionStatus(status) {
			t.Fatalf("activeSessionStatus(%q) = true", status)
		}
	}
}
