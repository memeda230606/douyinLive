package capture

import (
	"context"
	"math"
	"time"
)

const (
	RecordingProgressEventName         = "recording:progress"
	recordingProgressInterval          = time.Second
	maximumJavaScriptSafeInteger int64 = 1<<53 - 1
)

// RecordingProgressDTO is the fixed, privacy-safe desktop progress contract.
// A zero value is reported when FFmpeg has not made a metric available yet.
type RecordingProgressDTO struct {
	RoomID       string          `json:"roomId"`
	SessionID    string          `json:"sessionId"`
	OperationID  string          `json:"operationId"`
	State        RecordingStatus `json:"state"`
	ElapsedMS    int64           `json:"elapsedMs"`
	BytesWritten int64           `json:"bytesWritten"`
	SegmentCount int64           `json:"segmentCount"`
	Frame        int64           `json:"frame"`
	FPS          float64         `json:"fps"`
	Speed        float64         `json:"speed"`
	RestartCount int             `json:"restartCount"`
	UpdatedAt    int64           `json:"updatedAt"`
}

type RecordingProgressPublisher func(RecordingProgressDTO)

// recorderProgressSample stays inside capture. attemptID and ordinal are used
// only for stale-attempt fencing and monotonic aggregation and never cross the
// public DTO boundary.
type recorderProgressSample struct {
	attemptID    string
	ordinal      int
	elapsedMS    int64
	bytesWritten int64
	segmentCount int64
	frame        int64
	fps          float64
	speed        float64
	updatedAt    int64
}

type recorderProgressSource interface {
	Progress() <-chan recorderProgressSample
	IsCurrentProgress(recorderProgressSample) bool
}

// recordingProgressDispatcher is deliberately shared by one Coordinator. A
// blocked callback can occupy at most one goroutine. While that callback owns
// the slot, later best-effort UI snapshots are dropped without blocking.
// No idle worker exists, so Coordinator construction and teardown cannot leak
// a permanent dispatcher goroutine.
type recordingProgressDispatcher struct {
	publisher RecordingProgressPublisher
	slot      chan struct{}
}

func newRecordingProgressDispatcher(publisher RecordingProgressPublisher) *recordingProgressDispatcher {
	if publisher == nil {
		return nil
	}
	return &recordingProgressDispatcher{
		publisher: publisher,
		slot:      make(chan struct{}, 1),
	}
}

func (d *recordingProgressDispatcher) publish(progress RecordingProgressDTO) {
	if d == nil || !validRecordingProgressDTO(progress) {
		return
	}
	select {
	case d.slot <- struct{}{}:
	default:
		return
	}
	go func() {
		defer func() { <-d.slot }()
		callRecordingProgressPublisher(d.publisher, progress)
	}()
}

func callRecordingProgressPublisher(publisher RecordingProgressPublisher, progress RecordingProgressDTO) {
	defer func() {
		_ = recover()
	}()
	publisher(progress)
}

func validRecordingProgressDTO(progress RecordingProgressDTO) bool {
	if progress.RoomID == "" || progress.SessionID == "" || progress.OperationID == "" ||
		(progress.State != RecordingActive && progress.State != RecordingReconnecting) ||
		progress.ElapsedMS < 0 || progress.BytesWritten < 0 || progress.SegmentCount < 0 ||
		progress.Frame < 0 || progress.RestartCount < 0 || progress.UpdatedAt < 0 ||
		progress.ElapsedMS > maximumJavaScriptSafeInteger ||
		progress.BytesWritten > maximumJavaScriptSafeInteger ||
		progress.SegmentCount > maximumJavaScriptSafeInteger ||
		progress.Frame > maximumJavaScriptSafeInteger ||
		int64(progress.RestartCount) > maximumJavaScriptSafeInteger ||
		progress.UpdatedAt > maximumJavaScriptSafeInteger ||
		progress.FPS < 0 || progress.Speed < 0 ||
		math.IsNaN(progress.FPS) || math.IsInf(progress.FPS, 0) ||
		math.IsNaN(progress.Speed) || math.IsInf(progress.Speed, 0) {
		return false
	}
	return true
}

func (s *sessionRuntime) startRecorderProgress(recorder Recorder) {
	if s == nil || s.coordinator == nil || s.coordinator.progressDispatcher == nil {
		return
	}
	source, ok := recorder.(recorderProgressSource)
	if !ok {
		return
	}
	progressEvents := source.Progress()
	if progressEvents == nil {
		return
	}
	watchCtx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	current := s.current
	if s.finalizing || s.finalized || !sameRecorderInstance(s.recorder, recorder) ||
		current.Status != SessionRecording ||
		(current.RecordingStatus != RecordingActive && current.RecordingStatus != RecordingReconnecting) {
		s.mu.Unlock()
		cancel()
		return
	}
	s.cancelRecorderProgressLocked()
	generation := s.recorderProgressGeneration
	s.recorderProgressCancel = cancel
	s.mu.Unlock()

	go func() {
		for {
			select {
			case <-watchCtx.Done():
				return
			case progress, open := <-progressEvents:
				if !open {
					return
				}
				s.handleRecorderProgress(generation, recorder, source, progress)
			}
		}
	}()
}

// cancelRecorderProgressLocked invalidates a watcher before changing the
// session operation, recorder ownership, or terminal state. Callers hold mu.
func (s *sessionRuntime) cancelRecorderProgressLocked() {
	s.recorderProgressGeneration++
	if s.recorderProgressCancel != nil {
		s.recorderProgressCancel()
		s.recorderProgressCancel = nil
	}
}

func (s *sessionRuntime) handleRecorderProgress(
	generation uint64,
	recorder Recorder,
	source recorderProgressSource,
	progress recorderProgressSample,
) {
	if !validRecorderProgressSample(progress) || !source.IsCurrentProgress(progress) {
		return
	}

	s.mu.Lock()
	current := s.current
	if generation != s.recorderProgressGeneration || s.finalizing || s.finalized ||
		!sameRecorderInstance(s.recorder, recorder) || current.Status != SessionRecording ||
		(current.RecordingStatus != RecordingActive && current.RecordingStatus != RecordingReconnecting) ||
		current.ID == "" || current.RoomConfigID == "" || current.OperationID == "" {
		s.mu.Unlock()
		return
	}

	if s.progressAttemptID == "" {
		s.progressAttemptID = progress.attemptID
		s.progressAttemptOrdinal = progress.ordinal
	} else if s.progressAttemptID != progress.attemptID {
		if progress.ordinal <= s.progressAttemptOrdinal {
			s.mu.Unlock()
			return
		}
		s.progressElapsedBase = saturatingProgressAdd(
			s.progressElapsedBase, s.progressAttemptElapsed,
		)
		s.progressBytesBase = saturatingProgressAdd(
			s.progressBytesBase, s.progressAttemptBytes,
		)
		s.progressSegmentBase = saturatingProgressAdd(
			s.progressSegmentBase, s.progressAttemptSegments,
		)
		s.progressRestartCount += progress.ordinal - s.progressAttemptOrdinal
		s.progressAttemptID = progress.attemptID
		s.progressAttemptOrdinal = progress.ordinal
		s.progressAttemptElapsed = 0
		s.progressAttemptBytes = 0
		s.progressAttemptSegments = 0
	} else if progress.ordinal != s.progressAttemptOrdinal {
		s.mu.Unlock()
		return
	}

	s.progressAttemptElapsed = maxProgressInt64(s.progressAttemptElapsed, progress.elapsedMS)
	s.progressAttemptBytes = maxProgressInt64(s.progressAttemptBytes, progress.bytesWritten)
	s.progressAttemptSegments = maxProgressInt64(s.progressAttemptSegments, progress.segmentCount)
	s.recordProgressRestartCountLocked(s.recoveryAttempts)

	now := s.coordinator.now().UTC()
	if now.UnixMilli() < 0 {
		now = time.Unix(0, 0).UTC()
	}
	if !s.progressLastPublishedAt.IsZero() {
		if now.Before(s.progressLastPublishedAt) {
			now = s.progressLastPublishedAt
		}
		if now.Sub(s.progressLastPublishedAt) < recordingProgressInterval {
			s.mu.Unlock()
			return
		}
	}
	s.progressLastPublishedAt = now
	dto := RecordingProgressDTO{
		RoomID:       current.RoomConfigID,
		SessionID:    current.ID,
		OperationID:  current.OperationID,
		State:        current.RecordingStatus,
		ElapsedMS:    javascriptSafeProgressInt64(saturatingProgressAdd(s.progressElapsedBase, s.progressAttemptElapsed)),
		BytesWritten: javascriptSafeProgressInt64(saturatingProgressAdd(s.progressBytesBase, s.progressAttemptBytes)),
		SegmentCount: javascriptSafeProgressInt64(saturatingProgressAdd(s.progressSegmentBase, s.progressAttemptSegments)),
		Frame:        javascriptSafeProgressInt64(progress.frame),
		FPS:          progress.fps,
		Speed:        progress.speed,
		RestartCount: javascriptSafeProgressInt(s.progressRestartCount),
		UpdatedAt:    javascriptSafeProgressInt64(now.UnixMilli()),
	}
	s.mu.Unlock()
	s.coordinator.progressDispatcher.publish(dto)
}

func validRecorderProgressSample(progress recorderProgressSample) bool {
	return validRecorderAttemptID(progress.attemptID) && progress.ordinal > 0 && progress.ordinal <= maximumMediaAttempts &&
		progress.elapsedMS >= 0 && progress.bytesWritten >= 0 && progress.segmentCount >= 0 &&
		progress.frame >= 0 && progress.fps >= 0 && progress.speed >= 0 && progress.updatedAt >= 0 &&
		!math.IsNaN(progress.fps) && !math.IsInf(progress.fps, 0) &&
		!math.IsNaN(progress.speed) && !math.IsInf(progress.speed, 0)
}

func (s *sessionRuntime) recordProgressRestartCountLocked(value int) {
	if value > s.progressRestartCount {
		s.progressRestartCount = value
	}
}

func maxProgressInt64(left, right int64) int64 {
	if right > left {
		return right
	}
	return left
}

func javascriptSafeProgressInt64(value int64) int64 {
	if value < 0 {
		return 0
	}
	if value > maximumJavaScriptSafeInteger {
		return maximumJavaScriptSafeInteger
	}
	return value
}

func javascriptSafeProgressInt(value int) int {
	if value < 0 {
		return 0
	}
	maximum := maximumJavaScriptSafeInteger
	if int64(value) > maximum {
		return int(maximum)
	}
	return value
}

func saturatingProgressAdd(left, right int64) int64 {
	const maximum = int64(^uint64(0) >> 1)
	if right > maximum-left {
		return maximum
	}
	return left + right
}
