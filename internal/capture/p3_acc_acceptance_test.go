//go:build p3accacceptance

package capture

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type p3ACCTestRecorderProcess struct {
	mu        sync.Mutex
	terminate int
	doneCh    chan struct{}
}

func (p *p3ACCTestRecorderProcess) writeQuit() error           { return nil }
func (p *p3ACCTestRecorderProcess) terminateTree() error       { return nil }
func (p *p3ACCTestRecorderProcess) wait(context.Context) error { return nil }
func (p *p3ACCTestRecorderProcess) done() <-chan struct{}      { return p.doneCh }
func (p *p3ACCTestRecorderProcess) close() error               { return nil }
func (p *p3ACCTestRecorderProcess) terminateProcess() error {
	p.mu.Lock()
	p.terminate++
	p.mu.Unlock()
	return nil
}

func (p *p3ACCTestRecorderProcess) terminateCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.terminate
}

func TestCrashP3AcceptanceCurrentRecorderRequiresExactFence(t *testing.T) {
	const (
		roomID      = "018f47a0-7c00-7000-8000-000000000201"
		sessionID   = "018f47a0-7c00-7000-8000-000000000202"
		operationID = "018f47a0-7c00-7000-8000-000000000203"
		attemptID   = "018f47a0-7c00-7000-8000-000000000204"
	)
	process := &p3ACCTestRecorderProcess{doneCh: make(chan struct{})}
	recorder := &FFmpegRecorder{
		current:  &recorderAttempt{id: attemptID, ordinal: 1, process: process},
		stopDone: make(chan struct{}),
	}
	runtime := &sessionRuntime{
		current: LiveSession{
			ID: sessionID, RoomConfigID: roomID, OperationID: operationID,
			Status: SessionRecording, RecordingStatus: RecordingActive,
		},
		operationID: operationID,
		recorder:    recorder,
	}
	coordinator := &Coordinator{runtimes: map[string]*coordinatorRuntimeEntry{
		roomID: {initialOperationID: operationID, ready: closedP3ACCChannel(), session: runtime},
	}}

	err := CrashP3AcceptanceCurrentRecorder(
		context.Background(), coordinator, roomID, sessionID, operationID,
		"018f47a0-7c00-7000-8000-000000000299",
	)
	if !errors.Is(err, ErrP3AcceptanceRecorderFence) || process.terminateCount() != 0 {
		t.Fatalf("wrong attempt result = %v, terminate count = %d", err, process.terminateCount())
	}
	if err := CrashP3AcceptanceCurrentRecorder(
		context.Background(), coordinator, roomID, sessionID, operationID, attemptID,
	); err != nil {
		t.Fatalf("exact fenced crash failed: %v", err)
	}
	if process.terminateCount() != 1 {
		t.Fatalf("terminate count = %d, want 1", process.terminateCount())
	}
}

func TestP3AcceptanceRecorderMatchRejectsExitedProcess(t *testing.T) {
	const (
		roomID      = "018f47a0-7c00-7000-8000-000000000221"
		sessionID   = "018f47a0-7c00-7000-8000-000000000222"
		operationID = "018f47a0-7c00-7000-8000-000000000223"
		attemptID   = "018f47a0-7c00-7000-8000-000000000224"
	)
	process := &p3ACCTestRecorderProcess{doneCh: make(chan struct{})}
	recorder := &FFmpegRecorder{
		current:  &recorderAttempt{id: attemptID, ordinal: 1, process: process},
		stopDone: make(chan struct{}),
	}
	runtime := &sessionRuntime{
		current: LiveSession{
			ID: sessionID, RoomConfigID: roomID, OperationID: operationID,
			Status: SessionRecording, RecordingStatus: RecordingActive,
		},
		operationID: operationID,
		recorder:    recorder,
	}
	coordinator := &Coordinator{runtimes: map[string]*coordinatorRuntimeEntry{
		roomID: {initialOperationID: operationID, ready: closedP3ACCChannel(), session: runtime},
	}}
	if !P3AcceptanceCurrentRecorderMatches(coordinator, roomID, sessionID, operationID, attemptID) ||
		P3AcceptanceCurrentRecorderMatches(coordinator, roomID, sessionID, operationID,
			"018f47a0-7c00-7000-8000-000000000229") {
		t.Fatal("exact recorder match did not enforce attempt fence")
	}
	close(process.doneCh)
	if P3AcceptanceCurrentRecorderMatches(coordinator, roomID, sessionID, operationID, attemptID) {
		t.Fatal("already-exited recorder matched")
	}
	if err := CrashP3AcceptanceCurrentRecorder(
		context.Background(), coordinator, roomID, sessionID, operationID, attemptID,
	); !errors.Is(err, ErrP3AcceptanceRecorderFence) || process.terminateCount() != 0 {
		t.Fatalf("already-exited crash result = %v, terminate count = %d", err, process.terminateCount())
	}
}

func TestCrashP3AcceptanceCurrentRecorderRejectsNonProductionRecorder(t *testing.T) {
	const roomID = "018f47a0-7c00-7000-8000-000000000211"
	runtime := &sessionRuntime{
		current: LiveSession{
			ID: "018f47a0-7c00-7000-8000-000000000212", RoomConfigID: roomID,
			OperationID: "018f47a0-7c00-7000-8000-000000000213",
			Status:      SessionRecording, RecordingStatus: RecordingActive,
		},
		operationID: "018f47a0-7c00-7000-8000-000000000213",
		recorder:    nil,
	}
	coordinator := &Coordinator{runtimes: map[string]*coordinatorRuntimeEntry{
		roomID: {ready: closedP3ACCChannel(), session: runtime},
	}}
	if err := CrashP3AcceptanceCurrentRecorder(
		context.Background(), coordinator, roomID, runtime.current.ID,
		runtime.current.OperationID, "018f47a0-7c00-7000-8000-000000000214",
	); !errors.Is(err, ErrP3AcceptanceRecorderUnavailable) {
		t.Fatalf("non-production recorder error = %v", err)
	}
}

func TestP3AcceptanceRecorderMediaActivityRequiresExactLivePhysicalAttempt(t *testing.T) {
	const (
		roomID      = "018f47a0-7c00-7000-8000-000000000231"
		sessionID   = "018f47a0-7c00-7000-8000-000000000232"
		operationID = "018f47a0-7c00-7000-8000-000000000233"
		attemptID   = "018f47a0-7c00-7000-8000-000000000234"
	)
	mediaDirectory := t.TempDir()
	attempt := MediaAttempt{
		ID: attemptID, Ordinal: 1, StartedAt: time.Now().UTC().Truncate(time.Millisecond).UnixMilli(),
		SegmentSeconds: 300, Committed: true, Protocol: "flv", Codec: "h264",
	}
	attemptDirectory := filepath.Join(mediaDirectory, ".attempt-"+attemptID)
	if err := os.Mkdir(attemptDirectory, 0o700); err != nil {
		t.Fatalf("create attempt directory: %v", err)
	}
	segmentPath := filepath.Join(attemptDirectory, mediaAttemptSegmentName(1, attempt))
	if err := os.WriteFile(segmentPath, nil, 0o600); err != nil {
		t.Fatalf("create segment: %v", err)
	}
	process := &p3ACCTestRecorderProcess{doneCh: make(chan struct{})}
	recorder := &FFmpegRecorder{
		options:  recorderOptions{mediaDirectory: mediaDirectory},
		attempts: []MediaAttempt{attempt},
		current: &recorderAttempt{
			id: attemptID, ordinal: 1, mediaIndex: 0, process: process,
		},
		stopDone: make(chan struct{}),
	}
	runtime := &sessionRuntime{
		current: LiveSession{
			ID: sessionID, RoomConfigID: roomID, OperationID: operationID,
			Status: SessionRecording, RecordingStatus: RecordingActive,
		},
		operationID: operationID,
		recorder:    recorder,
	}
	coordinator := &Coordinator{runtimes: map[string]*coordinatorRuntimeEntry{
		roomID: {initialOperationID: operationID, ready: closedP3ACCChannel(), session: runtime},
	}}

	activity, complete := P3AcceptanceCurrentRecorderMediaActivity(
		coordinator, roomID, sessionID, operationID, attemptID,
	)
	if !complete || activity.FileCount != 1 || activity.TotalBytes != 0 {
		t.Fatalf("zero-length physical baseline = (%#v, %t)", activity, complete)
	}
	if err := os.WriteFile(segmentPath, make([]byte, 128), 0o600); err != nil {
		t.Fatalf("grow segment: %v", err)
	}
	activity, complete = P3AcceptanceCurrentRecorderMediaActivity(
		coordinator, roomID, sessionID, operationID, attemptID,
	)
	if !complete || activity.FileCount != 1 || activity.TotalBytes != 128 {
		t.Fatalf("physical activity = (%#v, %t)", activity, complete)
	}

	unexpected := filepath.Join(attemptDirectory, "unexpected.txt")
	if err := os.WriteFile(unexpected, []byte("x"), 0o600); err != nil {
		t.Fatalf("write unexpected entry: %v", err)
	}
	if _, complete := P3AcceptanceCurrentRecorderMediaActivity(
		coordinator, roomID, sessionID, operationID, attemptID,
	); complete {
		t.Fatal("unexpected attempt entry was accepted")
	}
	if err := os.Remove(unexpected); err != nil {
		t.Fatalf("remove unexpected entry: %v", err)
	}
	close(process.doneCh)
	if _, complete := P3AcceptanceCurrentRecorderMediaActivity(
		coordinator, roomID, sessionID, operationID, attemptID,
	); complete {
		t.Fatal("exited recorder returned physical activity")
	}
}

func closedP3ACCChannel() chan struct{} {
	result := make(chan struct{})
	close(result)
	return result
}
