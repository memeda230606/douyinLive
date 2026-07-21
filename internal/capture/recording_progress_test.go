package capture

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

type fakeProgressRecorder struct {
	*fakeRecorder
	progressEvents chan recorderProgressSample
	mu             sync.Mutex
	attemptID      string
	ordinal        int
}

func newFakeProgressRecorder(attemptID string, ordinal int) *fakeProgressRecorder {
	return &fakeProgressRecorder{
		fakeRecorder:   &fakeRecorder{},
		progressEvents: make(chan recorderProgressSample, 16),
		attemptID:      attemptID,
		ordinal:        ordinal,
	}
}

func (r *fakeProgressRecorder) Progress() <-chan recorderProgressSample {
	return r.progressEvents
}

func (r *fakeProgressRecorder) IsCurrentProgress(progress recorderProgressSample) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return progress.attemptID == r.attemptID && progress.ordinal == r.ordinal
}

func (r *fakeProgressRecorder) setCurrentProgressAttempt(attemptID string, ordinal int) {
	r.mu.Lock()
	r.attemptID = attemptID
	r.ordinal = ordinal
	r.mu.Unlock()
}

func TestRecordingProgressDTOJSONWhitelist(t *testing.T) {
	dto := RecordingProgressDTO{
		RoomID: "room", SessionID: "session", OperationID: "operation",
		State: RecordingActive, ElapsedMS: 1234, BytesWritten: 5678,
		BytesAvailable: true, SegmentCount: 2, Frame: 99, FPS: 29.97, Speed: 1.25,
		RestartCount: 3, UpdatedAt: 4567,
	}
	encoded, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	gotKeys := make([]string, 0, len(decoded))
	for key := range decoded {
		gotKeys = append(gotKeys, key)
	}
	sort.Strings(gotKeys)
	wantKeys := []string{
		"bytesAvailable", "bytesWritten", "elapsedMs", "fps", "frame", "operationId", "restartCount",
		"roomId", "segmentCount", "sessionId", "speed", "state", "updatedAt",
	}
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("JSON keys = %v, want %v", gotKeys, wantKeys)
	}
	serialized := string(encoded)
	for _, forbidden := range []string{
		"attemptId", "path", "url", "error", "native", "cookie", "stream",
	} {
		if strings.Contains(strings.ToLower(serialized), strings.ToLower(forbidden)) {
			t.Fatalf("serialized progress exposed forbidden field %q: %s", forbidden, serialized)
		}
	}
	if RecordingProgressEventName != "recording:progress" || !validRecordingProgressDTO(dto) {
		t.Fatal("recording progress public contract is invalid")
	}
	invalid := dto
	invalid.Speed = math.Inf(1)
	if validRecordingProgressDTO(invalid) {
		t.Fatal("infinite speed accepted")
	}
	invalid = dto
	invalid.ElapsedMS = -1
	if validRecordingProgressDTO(invalid) {
		t.Fatal("negative elapsed time accepted")
	}
}

func TestRecordingProgressDTOJavaScriptSafeIntegerBounds(t *testing.T) {
	maximum := maximumJavaScriptSafeInteger
	restartMaximum := int(^uint(0) >> 1)
	if strconv.IntSize >= 64 {
		restartMaximum = int(maximum)
	}
	dto := RecordingProgressDTO{
		RoomID: "room", SessionID: "session", OperationID: "operation",
		State:     RecordingActive,
		ElapsedMS: maximum, BytesWritten: maximum, SegmentCount: maximum,
		Frame: maximum, RestartCount: restartMaximum, UpdatedAt: maximum,
	}
	if !validRecordingProgressDTO(dto) {
		t.Fatalf("maximum JavaScript-safe DTO rejected: %#v", dto)
	}

	aboveMaximum := maximum + 1
	for _, testCase := range []struct {
		name   string
		assign func(*RecordingProgressDTO)
	}{
		{name: "elapsedMs", assign: func(value *RecordingProgressDTO) { value.ElapsedMS = aboveMaximum }},
		{name: "bytesWritten", assign: func(value *RecordingProgressDTO) { value.BytesWritten = aboveMaximum }},
		{name: "segmentCount", assign: func(value *RecordingProgressDTO) { value.SegmentCount = aboveMaximum }},
		{name: "frame", assign: func(value *RecordingProgressDTO) { value.Frame = aboveMaximum }},
		{name: "updatedAt", assign: func(value *RecordingProgressDTO) { value.UpdatedAt = aboveMaximum }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			invalid := dto
			testCase.assign(&invalid)
			if validRecordingProgressDTO(invalid) {
				t.Fatalf("DTO above JavaScript-safe %s bound accepted: %#v", testCase.name, invalid)
			}
		})
	}
	if strconv.IntSize >= 64 {
		invalid := dto
		invalid.RestartCount = int(aboveMaximum)
		if validRecordingProgressDTO(invalid) {
			t.Fatalf("DTO above JavaScript-safe restartCount bound accepted: %#v", invalid)
		}
		if got := javascriptSafeProgressInt(int(aboveMaximum)); int64(got) != maximum {
			t.Fatalf("javascriptSafeProgressInt() = %d, want %d", got, maximum)
		}
	}
	if got := javascriptSafeProgressInt64(aboveMaximum); got != maximum {
		t.Fatalf("javascriptSafeProgressInt64() = %d, want %d", got, maximum)
	}
}

func TestSessionRuntimeRecordingProgressSaturatesJavaScriptSafeIntegers(t *testing.T) {
	maximum := maximumJavaScriptSafeInteger
	published := make(chan RecordingProgressDTO, 1)
	coordinator := &Coordinator{
		now: func() time.Time { return time.UnixMilli(maximum + 100).UTC() },
		progressDispatcher: newRecordingProgressDispatcher(func(progress RecordingProgressDTO) {
			published <- progress
		}),
	}
	attemptID := newProgressTestID(t)
	recorder := newFakeProgressRecorder(attemptID, 1)
	runtime := &sessionRuntime{
		coordinator: coordinator,
		current: LiveSession{
			ID: newProgressTestID(t), RoomConfigID: newProgressTestID(t),
			OperationID: newProgressTestID(t), Status: SessionRecording,
			RecordingStatus: RecordingActive,
		},
		recorder: recorder,
	}
	if strconv.IntSize >= 64 {
		runtime.progressRestartCount = int(maximum + 100)
	}
	internalMaximum := int64(^uint64(0) >> 1)
	runtime.handleRecorderProgress(0, recorder, recorder, recorderProgressSample{
		attemptID: attemptID, ordinal: 1,
		elapsedMS: internalMaximum, bytesWritten: internalMaximum,
		bytesAvailable: true,
		segmentCount:   internalMaximum, frame: internalMaximum,
		fps: 1, speed: 1, updatedAt: internalMaximum,
	})
	got := waitProgressDTO(t, published)
	if got.ElapsedMS != maximum || got.BytesWritten != maximum ||
		got.SegmentCount != maximum || got.Frame != maximum || got.UpdatedAt != maximum {
		t.Fatalf("saturated progress = %#v, want integer metrics capped at %d", got, maximum)
	}
	if strconv.IntSize >= 64 && int64(got.RestartCount) != maximum {
		t.Fatalf("saturated restartCount = %d, want %d", got.RestartCount, maximum)
	}
	if !validRecordingProgressDTO(got) {
		t.Fatalf("saturated progress is not a valid public DTO: %#v", got)
	}
}

func TestRecordingProgressDispatcherIsBoundedPanicSafeAndIdle(t *testing.T) {
	base := RecordingProgressDTO{
		RoomID: "room", SessionID: "session", OperationID: "operation",
		State: RecordingActive, UpdatedAt: 1,
	}
	entered := make(chan struct{}, 1)
	recovered := make(chan RecordingProgressDTO, 1)
	var calls atomic.Int32
	dispatcher := newRecordingProgressDispatcher(func(progress RecordingProgressDTO) {
		if calls.Add(1) == 1 {
			entered <- struct{}{}
			panic("publisher panic")
		}
		recovered <- progress
	})
	dispatcher.publish(base)
	waitProgressSignal(t, entered, "panic publisher was not invoked")
	waitProgressDispatcherIdle(t, dispatcher)
	base.RestartCount = 1
	base.UpdatedAt++
	dispatcher.publish(base)
	if got := waitProgressDTO(t, recovered); got.RestartCount != 1 {
		t.Fatalf("post-panic DTO = %#v", got)
	}
	waitProgressDispatcherIdle(t, dispatcher)

	blocked := make(chan struct{})
	firstEntered := make(chan struct{}, 1)
	completed := make(chan RecordingProgressDTO, 4)
	var active atomic.Int32
	var maximumActive atomic.Int32
	blockingDispatcher := newRecordingProgressDispatcher(func(progress RecordingProgressDTO) {
		current := active.Add(1)
		for {
			maximum := maximumActive.Load()
			if current <= maximum || maximumActive.CompareAndSwap(maximum, current) {
				break
			}
		}
		select {
		case firstEntered <- struct{}{}:
		default:
		}
		<-blocked
		completed <- progress
		active.Add(-1)
	})
	blockingDispatcher.publish(base)
	waitProgressSignal(t, firstEntered, "blocking publisher was not invoked")
	started := time.Now()
	for index := 2; index <= 100; index++ {
		next := base
		next.RestartCount = index
		next.UpdatedAt = int64(index)
		blockingDispatcher.publish(next)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("bounded publish took %s", elapsed)
	}
	if maximumActive.Load() != 1 {
		t.Fatalf("maximum concurrent publisher calls = %d", maximumActive.Load())
	}
	close(blocked)
	first := waitProgressDTO(t, completed)
	if first.RestartCount != 1 {
		t.Fatalf("first DTO restart count = %d, want 1", first.RestartCount)
	}
	waitProgressDispatcherIdle(t, blockingDispatcher)
	latest := base
	latest.RestartCount = 100
	latest.UpdatedAt = 100
	blockingDispatcher.publish(latest)
	if got := waitProgressDTO(t, completed); got.RestartCount != 100 {
		t.Fatalf("post-release DTO restart count = %d, want 100", got.RestartCount)
	}
	waitProgressDispatcherIdle(t, blockingDispatcher)
}

func TestSessionRuntimeRecordingProgressFencingRateAndMonotonicity(t *testing.T) {
	start := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	var nowMS atomic.Int64
	nowMS.Store(start.UnixMilli())
	published := make(chan RecordingProgressDTO, 16)
	coordinator := &Coordinator{
		now: func() time.Time { return time.UnixMilli(nowMS.Load()).UTC() },
		progressDispatcher: newRecordingProgressDispatcher(func(progress RecordingProgressDTO) {
			published <- progress
		}),
	}
	roomID := newProgressTestID(t)
	sessionID := newProgressTestID(t)
	operationID := newProgressTestID(t)
	firstAttempt := newProgressTestID(t)
	recorder := newFakeProgressRecorder(firstAttempt, 1)
	runtime := &sessionRuntime{
		coordinator: coordinator,
		current: LiveSession{
			ID: sessionID, RoomConfigID: roomID, OperationID: operationID,
			Status: SessionRecording, RecordingStatus: RecordingActive,
		},
		operationID: operationID,
		recorder:    recorder,
	}
	runtime.startRecorderProgress(recorder)
	t.Cleanup(func() {
		runtime.mu.Lock()
		runtime.cancelRecorderProgressLocked()
		runtime.mu.Unlock()
	})

	recorder.progressEvents <- progressSample(firstAttempt, 1, 1000, 2000, 1, 10)
	first := waitProgressDTO(t, published)
	if first.RoomID != roomID || first.SessionID != sessionID || first.OperationID != operationID ||
		first.State != RecordingActive || first.ElapsedMS != 1000 || first.BytesWritten != 2000 || !first.BytesAvailable ||
		first.SegmentCount != 1 || first.RestartCount != 0 || first.UpdatedAt != start.UnixMilli() {
		t.Fatalf("first progress = %#v", first)
	}

	nowMS.Add(500)
	recorder.progressEvents <- progressSample(firstAttempt, 1, 2000, 3000, 1, 20)
	assertNoProgressDTO(t, published)
	nowMS.Add(500)
	recorder.progressEvents <- progressSample(firstAttempt, 1, 2500, 3500, 2, 25)
	second := waitProgressDTO(t, published)
	if second.ElapsedMS != 2500 || second.BytesWritten != 3500 || second.SegmentCount != 2 ||
		second.UpdatedAt-first.UpdatedAt != int64(time.Second/time.Millisecond) {
		t.Fatalf("one-hertz progress = %#v", second)
	}

	secondAttempt := newProgressTestID(t)
	recorder.setCurrentProgressAttempt(secondAttempt, 2)
	nowMS.Add(1000)
	recorder.progressEvents <- progressSample(firstAttempt, 1, 9000, 9000, 9, 90)
	assertNoProgressDTO(t, published)
	recorder.progressEvents <- progressSample(secondAttempt, 2, 500, 700, 1, 5)
	third := waitProgressDTO(t, published)
	if third.ElapsedMS != 3000 || third.BytesWritten != 4200 || third.SegmentCount != 3 ||
		third.RestartCount != 1 {
		t.Fatalf("cross-attempt progress = %#v", third)
	}

	oldGeneration := func() uint64 {
		runtime.mu.Lock()
		defer runtime.mu.Unlock()
		generation := runtime.recorderProgressGeneration
		runtime.cancelRecorderProgressLocked()
		runtime.current.OperationID = newProgressTestID(t)
		runtime.operationID = runtime.current.OperationID
		return generation
	}()
	thirdAttempt := newProgressTestID(t)
	recorder.setCurrentProgressAttempt(thirdAttempt, 3)
	nowMS.Add(1000)
	stale := progressSample(thirdAttempt, 3, 250, 300, 1, 3)
	runtime.handleRecorderProgress(oldGeneration, recorder, recorder, stale)
	assertNoProgressDTO(t, published)
	runtime.startRecorderProgress(recorder)
	runtime.mu.Lock()
	currentGeneration := runtime.recorderProgressGeneration
	wantOperationID := runtime.current.OperationID
	runtime.mu.Unlock()
	runtime.handleRecorderProgress(currentGeneration, recorder, recorder, stale)
	fourth := waitProgressDTO(t, published)
	if fourth.OperationID != wantOperationID || fourth.ElapsedMS != 3250 ||
		fourth.BytesWritten != 4500 || fourth.SegmentCount != 4 || fourth.RestartCount != 2 {
		t.Fatalf("new-operation progress = %#v", fourth)
	}
}

func TestSessionRecordingProgressKeepsUnavailableBytesExplicitAcrossAttempts(t *testing.T) {
	start := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	var nowMS atomic.Int64
	nowMS.Store(start.UnixMilli())
	published := make(chan RecordingProgressDTO, 4)
	coordinator := &Coordinator{
		now: func() time.Time { return time.UnixMilli(nowMS.Load()).UTC() },
		progressDispatcher: newRecordingProgressDispatcher(func(progress RecordingProgressDTO) {
			published <- progress
		}),
	}
	firstAttempt := newProgressTestID(t)
	recorder := newFakeProgressRecorder(firstAttempt, 1)
	runtime := &sessionRuntime{
		coordinator: coordinator,
		current: LiveSession{
			ID: newProgressTestID(t), RoomConfigID: newProgressTestID(t), OperationID: newProgressTestID(t),
			Status: SessionRecording, RecordingStatus: RecordingActive,
		},
		recorder: recorder,
	}
	unavailable := progressSample(firstAttempt, 1, 1000, 0, 1, 10)
	unavailable.bytesAvailable = false
	runtime.handleRecorderProgress(0, recorder, recorder, unavailable)
	if got := waitProgressDTO(t, published); got.BytesAvailable || got.BytesWritten != 0 {
		t.Fatalf("unavailable bytes were presented as available: %#v", got)
	}
	nowMS.Add(int64(time.Second / time.Millisecond))
	runtime.handleRecorderProgress(0, recorder, recorder, progressSample(firstAttempt, 1, 2000, 0, 1, 20))
	if got := waitProgressDTO(t, published); !got.BytesAvailable || got.BytesWritten != 0 {
		t.Fatalf("numeric zero did not restore current-attempt availability: %#v", got)
	}
	nowMS.Add(int64(time.Second / time.Millisecond))
	laterUnavailable := progressSample(firstAttempt, 1, 3000, 0, 2, 30)
	laterUnavailable.bytesAvailable = false
	runtime.handleRecorderProgress(0, recorder, recorder, laterUnavailable)
	if got := waitProgressDTO(t, published); got.BytesAvailable || got.ElapsedMS != 3000 || got.SegmentCount != 2 || got.Frame != 30 {
		t.Fatalf("latest N/A sample did not override prior availability while media advanced: %#v", got)
	}
	nowMS.Add(int64(time.Second / time.Millisecond))
	secondAttempt := newProgressTestID(t)
	recorder.setCurrentProgressAttempt(secondAttempt, 2)
	runtime.handleRecorderProgress(0, recorder, recorder, progressSample(secondAttempt, 2, 500, 700, 1, 5))
	if got := waitProgressDTO(t, published); got.BytesAvailable || got.BytesWritten != 700 {
		t.Fatalf("unknown prior attempt bytes were presented as a complete total: %#v", got)
	}
}

func TestSessionRecordingProgressMakesKnownHistoryUnknownAfterUnavailableAttempt(t *testing.T) {
	start := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	var nowMS atomic.Int64
	nowMS.Store(start.UnixMilli())
	published := make(chan RecordingProgressDTO, 4)
	coordinator := &Coordinator{
		now: func() time.Time { return time.UnixMilli(nowMS.Load()).UTC() },
		progressDispatcher: newRecordingProgressDispatcher(func(progress RecordingProgressDTO) {
			published <- progress
		}),
	}
	firstAttempt := newProgressTestID(t)
	recorder := newFakeProgressRecorder(firstAttempt, 1)
	runtime := &sessionRuntime{
		coordinator: coordinator,
		current: LiveSession{
			ID: newProgressTestID(t), RoomConfigID: newProgressTestID(t), OperationID: newProgressTestID(t),
			Status: SessionRecording, RecordingStatus: RecordingActive,
		},
		recorder: recorder,
	}
	runtime.handleRecorderProgress(0, recorder, recorder, progressSample(firstAttempt, 1, 1000, 100, 1, 10))
	if got := waitProgressDTO(t, published); !got.BytesAvailable || got.BytesWritten != 100 {
		t.Fatalf("known first attempt was not available: %#v", got)
	}
	nowMS.Add(int64(time.Second / time.Millisecond))
	secondAttempt := newProgressTestID(t)
	recorder.setCurrentProgressAttempt(secondAttempt, 2)
	unavailable := progressSample(secondAttempt, 2, 500, 0, 1, 5)
	unavailable.bytesAvailable = false
	runtime.handleRecorderProgress(0, recorder, recorder, unavailable)
	if got := waitProgressDTO(t, published); got.BytesAvailable || got.BytesWritten != 100 {
		t.Fatalf("unknown current attempt did not hide known history: %#v", got)
	}
	nowMS.Add(int64(time.Second / time.Millisecond))
	thirdAttempt := newProgressTestID(t)
	recorder.setCurrentProgressAttempt(thirdAttempt, 3)
	runtime.handleRecorderProgress(0, recorder, recorder, progressSample(thirdAttempt, 3, 250, 200, 1, 3))
	if got := waitProgressDTO(t, published); got.BytesAvailable || got.BytesWritten != 300 {
		t.Fatalf("unavailable historical attempt was forgotten: %#v", got)
	}
}

func TestFFmpegRecorderProgressSourceAndSegmentSemantics(t *testing.T) {
	unavailable := recorderProgressFromFFmpeg(t, strings.Join([]string{
		"frame=0", "fps=0.00", "total_size=N/A", "out_time_us=0", "speed=0x", "progress=continue",
	}, "\n")+"\n")
	if unavailable.bytesAvailable || unavailable.segmentCount != 0 || unavailable.elapsedMS != 0 || unavailable.bytesWritten != 0 {
		t.Fatalf("unavailable-size progress = %#v", unavailable)
	}
	numericZero := recorderProgressFromFFmpeg(t, strings.Join([]string{
		"frame=0", "fps=0.00", "total_size=0", "out_time_us=0", "speed=0x", "progress=continue",
	}, "\n")+"\n")
	if !numericZero.bytesAvailable || numericZero.bytesWritten != 0 {
		t.Fatalf("numeric zero size was not preserved as available: %#v", numericZero)
	}
	active := recorderProgressFromFFmpeg(t, strings.Join([]string{
		"frame=18000", "fps=30.0", "total_size=8192", "out_time_us=600000000", "speed=1.5x", "progress=continue",
	}, "\n")+"\n")
	if !active.bytesAvailable || active.elapsedMS != 600000 || active.bytesWritten != 8192 || active.segmentCount != 3 ||
		active.frame != 18000 || active.fps != 30 || active.speed != 1.5 {
		t.Fatalf("active progress = %#v", active)
	}
}

func TestSessionProgressDoesNotWaitForBlockedPublisher(t *testing.T) {
	blocked := make(chan struct{})
	entered := make(chan struct{}, 1)
	coordinator := &Coordinator{
		now: time.Now,
		progressDispatcher: newRecordingProgressDispatcher(func(RecordingProgressDTO) {
			entered <- struct{}{}
			<-blocked
		}),
	}
	attemptID := newProgressTestID(t)
	recorder := newFakeProgressRecorder(attemptID, 1)
	runtime := &sessionRuntime{
		coordinator: coordinator,
		current: LiveSession{
			ID: newProgressTestID(t), RoomConfigID: newProgressTestID(t),
			OperationID: newProgressTestID(t), Status: SessionRecording,
			RecordingStatus: RecordingActive,
		},
		recorder: recorder,
	}
	runtime.startRecorderProgress(recorder)
	runtime.mu.Lock()
	generation := runtime.recorderProgressGeneration
	runtime.mu.Unlock()
	started := time.Now()
	runtime.handleRecorderProgress(
		generation, recorder, recorder,
		progressSample(attemptID, 1, 1, 1, 1, 1),
	)
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("session progress waited for publisher for %s", elapsed)
	}
	waitProgressSignal(t, entered, "publisher did not receive progress")
	close(blocked)
	waitProgressDispatcherIdle(t, coordinator.progressDispatcher)
	runtime.mu.Lock()
	runtime.cancelRecorderProgressLocked()
	runtime.mu.Unlock()
}

func TestFinalizeCleanupTimeoutCancelsRecordingProgressWatcherAndRetryConverges(t *testing.T) {
	for _, phase := range []string{"recorder_stop", "sink_flush"} {
		t.Run(phase, func(t *testing.T) {
			repository, store, _, roomID, now := openRepository(t)
			defer store.Close()
			cleanupGate := make(chan struct{})
			recorder := newFakeProgressRecorder(newProgressTestID(t), 1)
			sink := &fakeEventSink{}
			switch phase {
			case "recorder_stop":
				recorder.stopFunc = func(ctx context.Context) error {
					select {
					case <-cleanupGate:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}
			case "sink_flush":
				sink.flushFunc = func(ctx context.Context) error {
					select {
					case <-cleanupGate:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}
			default:
				t.Fatalf("unknown cleanup phase %q", phase)
			}
			coordinator, err := newTestCoordinator(repository, CoordinatorOptions{
				Now: func() time.Time { return now },
				RecorderFactory: func(context.Context, LiveSession, OpenRequest, CaptureSource) (Recorder, error) {
					return recorder, nil
				},
				EventSinkFactory: func(context.Context, LiveSession, OpenRequest) (EventSink, error) {
					return sink, nil
				},
				ProgressPublisher: func(RecordingProgressDTO) {},
			})
			if err != nil {
				t.Fatal(err)
			}
			session, err := coordinator.Open(context.Background(), OpenRequest{
				RoomConfigID: roomID, OperationID: newProgressTestID(t),
				RecordEnabled: true, StartedAt: now,
			}, newFakeCaptureSource(nil))
			if err != nil {
				t.Fatal(err)
			}
			runtime := session.(*sessionRuntime)
			runtime.mu.Lock()
			generationBefore := runtime.recorderProgressGeneration
			watchingBefore := runtime.recorderProgressCancel != nil
			runtime.mu.Unlock()
			if !watchingBefore {
				t.Fatal("recording progress watcher was not started")
			}

			finalizeOperationID := newProgressTestID(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			pending, finalizeErr := session.Finalize(ctx, finalizeOperationID, FinalizeShutdown)
			cancel()
			if !errors.Is(finalizeErr, context.DeadlineExceeded) ||
				!errors.Is(finalizeErr, ErrCaptureCleanupPending) {
				t.Fatalf("Finalize() error = %v, want cleanup pending deadline", finalizeErr)
			}
			if pending.Status != SessionFinalizing || pending.RecordingStatus != RecordingFinalizing {
				t.Fatalf("pending finalization = %#v", pending)
			}
			runtime.mu.Lock()
			watchingAfter := runtime.recorderProgressCancel != nil
			generationAfter := runtime.recorderProgressGeneration
			ownsFinalization := runtime.finalizing && !runtime.finalized
			runtime.mu.Unlock()
			if watchingAfter || generationAfter <= generationBefore || !ownsFinalization {
				t.Fatalf("progress watcher after timeout = watching:%t generation:%d->%d finalizing:%t",
					watchingAfter, generationBefore, generationAfter, ownsFinalization)
			}

			close(cleanupGate)
			terminal, err := session.Finalize(context.Background(), finalizeOperationID, FinalizeShutdown)
			if err != nil {
				t.Fatalf("Finalize() retry error = %v", err)
			}
			if terminal.Status != SessionCompleted || terminal.RecordingStatus != RecordingCompleted {
				t.Fatalf("Finalize() retry = %#v, want completed recording", terminal)
			}
		})
	}
}

func recorderProgressFromFFmpeg(t *testing.T, input string) recorderProgressSample {
	t.Helper()
	attemptID := newProgressTestID(t)
	fixedNow := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	recorder := &FFmpegRecorder{
		options:      recorderOptions{segmentSeconds: 300},
		dependencies: recorderDependencies{now: func() time.Time { return fixedNow }},
		lifecycleCtx: context.Background(),
		progress:     make(chan recorderProgressSample, 1),
	}
	attempt := &recorderAttempt{
		id: attemptID, ordinal: 1,
		streams:      processStreams{Stdout: io.NopCloser(strings.NewReader(input))},
		stderrBuffer: newBoundedTextBuffer(recorderStderrBufferBytes),
		progress:     make(chan struct{}),
		startupEnd:   make(chan struct{}),
		finished:     make(chan struct{}),
	}
	recorder.current = attempt
	recorder.startAttemptDrains(attempt)
	attempt.drainWG.Wait()
	select {
	case progress := <-recorder.Progress():
		if !recorder.IsCurrentProgress(progress) || progress.attemptID != attemptID ||
			progress.ordinal != 1 || progress.updatedAt != fixedNow.UnixMilli() {
			t.Fatalf("recorder progress identity = %#v", progress)
		}
		return progress
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for recorder progress")
		return recorderProgressSample{}
	}
}

func progressSample(
	attemptID string,
	ordinal int,
	elapsedMS int64,
	bytesWritten int64,
	segmentCount int64,
	frame int64,
) recorderProgressSample {
	return recorderProgressSample{
		attemptID: attemptID, ordinal: ordinal,
		elapsedMS: elapsedMS, bytesWritten: bytesWritten, segmentCount: segmentCount,
		bytesAvailable: true,
		frame:          frame, fps: 30, speed: 1, updatedAt: 1,
	}
}

func newProgressTestID(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7() error = %v", err)
	}
	return id.String()
}

func waitProgressDTO(t *testing.T, channel <-chan RecordingProgressDTO) RecordingProgressDTO {
	t.Helper()
	select {
	case progress := <-channel:
		return progress
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for recording progress")
		return RecordingProgressDTO{}
	}
}

func assertNoProgressDTO(t *testing.T, channel <-chan RecordingProgressDTO) {
	t.Helper()
	select {
	case progress := <-channel:
		t.Fatalf("unexpected recording progress: %#v", progress)
	case <-time.After(75 * time.Millisecond):
	}
}

func waitProgressSignal(t *testing.T, channel <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(2 * time.Second):
		t.Fatal(message)
	}
}

func waitProgressDispatcherIdle(t *testing.T, dispatcher *recordingProgressDispatcher) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if len(dispatcher.slot) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("recording progress dispatcher did not become idle")
		}
		time.Sleep(time.Millisecond)
	}
}
