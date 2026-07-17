package capture

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	douyinLive "github.com/jwwsjlm/douyinLive/v2"
)

type orderedSessionRepository struct {
	SessionRepository
	mu    sync.Mutex
	order []string
}

type markFinalizingFailureRepository struct {
	SessionRepository
	mu       sync.Mutex
	failures int
}

var errInjectedMarkFinalizing = errors.New("injected mark-finalizing transition failure")

func (r *markFinalizingFailureRepository) Transition(ctx context.Context, input TransitionSessionInput) (LiveSession, error) {
	if input.Status == SessionFinalizing {
		r.mu.Lock()
		if r.failures > 0 {
			r.failures--
			r.mu.Unlock()
			return LiveSession{}, errInjectedMarkFinalizing
		}
		r.mu.Unlock()
	}
	return r.SessionRepository.Transition(ctx, input)
}

type terminalTransitionAttempt struct {
	status          SessionStatus
	recordingStatus RecordingStatus
	endedAt         int64
}

type terminalTransitionFailureRepository struct {
	SessionRepository
	mu       sync.Mutex
	failures int
	attempts []terminalTransitionAttempt
}

var errInjectedTerminalTransition = errors.New("injected terminal transition failure")

func (r *terminalTransitionFailureRepository) Transition(ctx context.Context, input TransitionSessionInput) (LiveSession, error) {
	if input.Status == SessionCompleted || input.Status == SessionInterrupted || input.Status == SessionFailed {
		attempt := terminalTransitionAttempt{status: input.Status, recordingStatus: input.RecordingStatus}
		if input.EndedAt != nil {
			attempt.endedAt = input.EndedAt.UTC().UnixNano()
		}
		r.mu.Lock()
		r.attempts = append(r.attempts, attempt)
		if r.failures > 0 {
			r.failures--
			r.mu.Unlock()
			return LiveSession{}, errInjectedTerminalTransition
		}
		r.mu.Unlock()
	}
	return r.SessionRepository.Transition(ctx, input)
}

func (r *terminalTransitionFailureRepository) snapshotAttempts() []terminalTransitionAttempt {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]terminalTransitionAttempt(nil), r.attempts...)
}

type committedErrorRepository struct {
	SessionRepository
	failCreate     bool
	failActivation bool
}

func (r *committedErrorRepository) Create(ctx context.Context, input CreateSessionInput) (LiveSession, error) {
	session, err := r.SessionRepository.Create(ctx, input)
	if err == nil && r.failCreate {
		r.failCreate = false
		return session, errors.New("injected manifest materialization failure after create commit")
	}
	return session, err
}

func (r *committedErrorRepository) Transition(ctx context.Context, input TransitionSessionInput) (LiveSession, error) {
	session, err := r.SessionRepository.Transition(ctx, input)
	if err == nil && r.failActivation && input.Status == SessionRecording {
		r.failActivation = false
		return session, errors.New("injected manifest materialization failure after activation commit")
	}
	return session, err
}

func (r *orderedSessionRepository) Transition(ctx context.Context, input TransitionSessionInput) (LiveSession, error) {
	switch input.Status {
	case SessionFinalizing:
		r.append("repo.finalizing")
	case SessionCompleted, SessionInterrupted, SessionFailed:
		r.append("repo.terminal")
	case SessionRecording:
		if input.RecordingStatus == RecordingReconnecting {
			r.append("repo.reconnecting")
		} else if input.ExpectedRecordingStatus == RecordingReconnecting && input.RecordingStatus == RecordingActive {
			r.append("repo.recording")
		}
	}
	return r.SessionRepository.Transition(ctx, input)
}

func (r *orderedSessionRepository) append(value string) {
	r.mu.Lock()
	r.order = append(r.order, value)
	r.mu.Unlock()
}

func (r *orderedSessionRepository) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

type fakeCaptureSource struct {
	mu           sync.Mutex
	order        *[]string
	next         int
	handlers     map[string]douyinLive.LiveMessageHandler
	allHandlers  []douyinLive.LiveMessageHandler
	unsubscribed int
}

func newFakeCaptureSource(order *[]string) *fakeCaptureSource {
	return &fakeCaptureSource{order: order, handlers: make(map[string]douyinLive.LiveMessageHandler)}
}

func (s *fakeCaptureSource) ResolveStreams() ([]douyinLive.ResolvedStream, error) { return nil, nil }

func (s *fakeCaptureSource) SubscribeMessage(handler douyinLive.LiveMessageHandler) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	id := "subscription-" + time.Unix(int64(s.next), 0).UTC().Format("150405")
	s.handlers[id] = handler
	s.allHandlers = append(s.allHandlers, handler)
	if s.order != nil {
		*s.order = append(*s.order, "source.subscribe")
	}
	return id
}

func (s *fakeCaptureSource) Unsubscribe(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.handlers, id)
	s.unsubscribed++
	if s.order != nil {
		*s.order = append(*s.order, "source.unsubscribe")
	}
}

func (s *fakeCaptureSource) emitCurrent(message *douyinLive.LiveMessage) {
	s.mu.Lock()
	handlers := make([]douyinLive.LiveMessageHandler, 0, len(s.handlers))
	for _, handler := range s.handlers {
		handlers = append(handlers, handler)
	}
	s.mu.Unlock()
	for _, handler := range handlers {
		handler(message)
	}
}

func (s *fakeCaptureSource) emitStale(index int, message *douyinLive.LiveMessage) {
	s.mu.Lock()
	handler := s.allHandlers[index]
	s.mu.Unlock()
	handler(message)
}

type fakeEventSink struct {
	mu        sync.Mutex
	order     *[]string
	accepted  int
	flushes   int
	flushFunc func(context.Context) error
}

func (s *fakeEventSink) Accept(*douyinLive.LiveMessage) {
	s.mu.Lock()
	s.accepted++
	s.mu.Unlock()
}

func newTestCoordinator(repository SessionRepository, options CoordinatorOptions) (*Coordinator, error) {
	if options.EventSinkFactory == nil {
		options.EventSinkFactory = func(context.Context, LiveSession, OpenRequest) (EventSink, error) {
			return &fakeEventSink{}, nil
		}
	}
	return NewCoordinator(repository, options)
}

func (s *fakeEventSink) FlushAndClose(ctx context.Context) error {
	s.mu.Lock()
	s.flushes++
	if s.order != nil {
		*s.order = append(*s.order, "sink.flush")
	}
	flushFunc := s.flushFunc
	s.mu.Unlock()
	if flushFunc != nil {
		return flushFunc(ctx)
	}
	return nil
}

type fakeRecorder struct {
	mu         sync.Mutex
	order      *[]string
	rebinds    int
	stops      int
	rebindFunc func(context.Context, CaptureSource) error
	stopFunc   func(context.Context) error
}

func (r *fakeRecorder) Rebind(ctx context.Context, source CaptureSource) error {
	r.mu.Lock()
	r.rebinds++
	if r.order != nil {
		*r.order = append(*r.order, "recorder.rebind")
	}
	rebindFunc := r.rebindFunc
	r.mu.Unlock()
	if rebindFunc != nil {
		return rebindFunc(ctx, source)
	}
	return nil
}

func (r *fakeRecorder) Stop(ctx context.Context) error {
	r.mu.Lock()
	r.stops++
	if r.order != nil {
		*r.order = append(*r.order, "recorder.stop")
	}
	stopFunc := r.stopFunc
	r.mu.Unlock()
	if stopFunc != nil {
		return stopFunc(ctx)
	}
	return nil
}

func TestCoordinatorOpenRebindFinalizeKeepsOneSessionAndOrdersCleanup(t *testing.T) {
	repository, store, _, roomID, now := openRepository(t)
	defer store.Close()
	order := make([]string, 0, 16)
	orderedRepository := &orderedSessionRepository{SessionRepository: repository, order: order}
	sink := &fakeEventSink{order: &orderedRepository.order}
	recorder := &fakeRecorder{order: &orderedRepository.order}
	coordinator, err := newTestCoordinator(orderedRepository, CoordinatorOptions{
		Now: func() time.Time { return now },
		EventSinkFactory: func(context.Context, LiveSession, OpenRequest) (EventSink, error) {
			orderedRepository.append("sink.open")
			return sink, nil
		},
		RecorderFactory: func(context.Context, LiveSession, OpenRequest, CaptureSource) (Recorder, error) {
			orderedRepository.append("recorder.start")
			return recorder, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	firstSource := newFakeCaptureSource(&orderedRepository.order)
	operationID := newV7(t)
	session, err := coordinator.Open(context.Background(), OpenRequest{
		RoomConfigID: roomID, OperationID: operationID, Title: "测试场次",
		RecordEnabled: true, StartedAt: now,
	}, firstSource)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	opened := session.Snapshot()
	if opened.Status != SessionRecording || opened.RecordingStatus != RecordingActive {
		t.Fatalf("opened session = %+v", opened)
	}
	firstSource.emitCurrent(&douyinLive.LiveMessage{ReceivedAt: now})

	secondSource := newFakeCaptureSource(&orderedRepository.order)
	rebindOperation := newV7(t)
	rebound, err := session.Rebind(context.Background(), rebindOperation, secondSource)
	if err != nil {
		t.Fatalf("Rebind() error = %v", err)
	}
	if rebound.ID != opened.ID || rebound.OperationID != rebindOperation {
		t.Fatalf("Rebind() created or selected another session: %+v", rebound)
	}
	if rebound.RecordingStatus != RecordingActive || recorder.rebinds != 1 {
		t.Fatalf("Rebind() recording = %s, recorder calls = %d", rebound.RecordingStatus, recorder.rebinds)
	}
	firstSource.emitStale(0, &douyinLive.LiveMessage{ReceivedAt: now.Add(time.Second)})
	secondSource.emitCurrent(&douyinLive.LiveMessage{ReceivedAt: now.Add(2 * time.Second)})
	if sink.accepted != 2 {
		t.Fatalf("accepted messages = %d, want 2 (old operation must be stale)", sink.accepted)
	}

	finalOperation := newV7(t)
	finalized, err := session.Finalize(context.Background(), finalOperation, FinalizeOffline)
	if err != nil {
		t.Fatalf("Finalize() error = %v", err)
	}
	if finalized.Status != SessionCompleted || finalized.RecordingStatus != RecordingCompleted || finalized.ID != opened.ID {
		t.Fatalf("finalized session = %+v", finalized)
	}
	secondSource.emitStale(0, &douyinLive.LiveMessage{ReceivedAt: now.Add(3 * time.Second)})
	if sink.accepted != 2 {
		t.Fatalf("message accepted after finalize: %d", sink.accepted)
	}
	if _, err := session.Finalize(context.Background(), newV7(t), FinalizeShutdown); err != nil {
		t.Fatalf("idempotent Finalize() error = %v", err)
	}
	if recorder.stops != 1 || sink.flushes != 1 || secondSource.unsubscribed != 1 {
		t.Fatalf("cleanup counts = recorder:%d sink:%d unsubscribe:%d", recorder.stops, sink.flushes, secondSource.unsubscribed)
	}
	wantRebind := []string{"repo.reconnecting", "source.subscribe", "recorder.rebind", "repo.recording", "source.unsubscribe"}
	if !containsOrderedSubsequence(orderedRepository.snapshot(), wantRebind) {
		t.Fatalf("rebind order = %v, want subsequence %v", orderedRepository.snapshot(), wantRebind)
	}
	wantTail := []string{"repo.finalizing", "recorder.stop", "source.unsubscribe", "sink.flush", "repo.terminal"}
	if !containsOrderedSubsequence(orderedRepository.snapshot(), wantTail) {
		t.Fatalf("cleanup order = %v, want subsequence %v", orderedRepository.snapshot(), wantTail)
	}
}

func TestCoordinatorRecorderUnavailableDoesNotFailLiveSession(t *testing.T) {
	repository, store, _, roomID, now := openRepository(t)
	defer store.Close()
	factoryCalls := 0
	coordinator, err := newTestCoordinator(repository, CoordinatorOptions{
		Now: func() time.Time { return now },
		RecorderFactory: func(context.Context, LiveSession, OpenRequest, CaptureSource) (Recorder, error) {
			factoryCalls++
			return nil, ErrRecordingUnavailable
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := coordinator.Open(context.Background(), OpenRequest{
		RoomConfigID: roomID, OperationID: newV7(t), RecordEnabled: true, StartedAt: now,
	}, newFakeCaptureSource(nil))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if got := session.Snapshot(); got.Status != SessionRecording || got.RecordingStatus != RecordingUnavailable {
		t.Fatalf("unavailable recorder session = %+v", got)
	}
	rebound, err := session.Rebind(context.Background(), newV7(t), newFakeCaptureSource(nil))
	if err != nil {
		t.Fatalf("unavailable Rebind() error = %v", err)
	}
	if rebound.RecordingStatus != RecordingUnavailable || factoryCalls != 1 {
		t.Fatalf("unavailable Rebind() = %s, factory calls = %d", rebound.RecordingStatus, factoryCalls)
	}
	finalized, err := session.Finalize(context.Background(), newV7(t), FinalizeOffline)
	if err != nil {
		t.Fatal(err)
	}
	if finalized.Status != SessionCompleted || finalized.RecordingStatus != RecordingUnavailable {
		t.Fatalf("unavailable finalization = %+v", finalized)
	}
}

func TestCoordinatorRecordDisabledNeverConstructsRecorder(t *testing.T) {
	repository, store, _, roomID, now := openRepository(t)
	defer store.Close()
	factoryCalls := 0
	coordinator, err := newTestCoordinator(repository, CoordinatorOptions{
		Now: func() time.Time { return now },
		RecorderFactory: func(context.Context, LiveSession, OpenRequest, CaptureSource) (Recorder, error) {
			factoryCalls++
			return &fakeRecorder{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := coordinator.Open(context.Background(), OpenRequest{
		RoomConfigID: roomID, OperationID: newV7(t), RecordEnabled: false, StartedAt: now,
	}, newFakeCaptureSource(nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := session.Snapshot(); got.RecordingStatus != RecordingDisabled || factoryCalls != 0 {
		t.Fatalf("disabled recording = %s, factory calls = %d", got.RecordingStatus, factoryCalls)
	}
	if _, err := session.Rebind(context.Background(), newV7(t), newFakeCaptureSource(nil)); err != nil {
		t.Fatal(err)
	}
	if factoryCalls != 0 {
		t.Fatalf("disabled Rebind constructed recorder %d times", factoryCalls)
	}
}

func TestCoordinatorRecorderRebindFailureDegradesWithoutEndingSession(t *testing.T) {
	repository, store, _, roomID, now := openRepository(t)
	defer store.Close()
	recorder := &fakeRecorder{rebindFunc: func(context.Context, CaptureSource) error {
		return errors.New("injected stream refresh failure")
	}}
	coordinator, err := newTestCoordinator(repository, CoordinatorOptions{
		Now: func() time.Time { return now },
		RecorderFactory: func(context.Context, LiveSession, OpenRequest, CaptureSource) (Recorder, error) {
			return recorder, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := coordinator.Open(context.Background(), OpenRequest{
		RoomConfigID: roomID, OperationID: newV7(t), RecordEnabled: true, StartedAt: now,
	}, newFakeCaptureSource(nil))
	if err != nil {
		t.Fatal(err)
	}
	opened := session.Snapshot()
	rebound, err := session.Rebind(context.Background(), newV7(t), newFakeCaptureSource(nil))
	if err != nil {
		t.Fatalf("Rebind() error = %v, recorder failure must degrade", err)
	}
	if rebound.ID != opened.ID || rebound.Status != SessionRecording || rebound.RecordingStatus != RecordingUnavailable {
		t.Fatalf("degraded Rebind() = %+v", rebound)
	}
	if recorder.rebinds != 1 || recorder.stops != 1 {
		t.Fatalf("recorder calls = rebind:%d stop:%d", recorder.rebinds, recorder.stops)
	}
}

func TestCoordinatorFinalizeTimeoutIsBoundedAndMarksIncomplete(t *testing.T) {
	repository, store, _, roomID, now := openRepository(t)
	defer store.Close()
	recorder := &fakeRecorder{stopFunc: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	coordinator, err := newTestCoordinator(repository, CoordinatorOptions{
		Now: func() time.Time { return now }, CommitTimeout: time.Second,
		RecorderFactory: func(context.Context, LiveSession, OpenRequest, CaptureSource) (Recorder, error) {
			return recorder, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := coordinator.Open(context.Background(), OpenRequest{
		RoomConfigID: roomID, OperationID: newV7(t), RecordEnabled: true, StartedAt: now,
	}, newFakeCaptureSource(nil))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	finalized, err := session.Finalize(ctx, newV7(t), FinalizeShutdown)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Finalize() error = %v, want deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("Finalize() elapsed = %v, want bounded return", elapsed)
	}
	if finalized.Status != SessionInterrupted || finalized.RecordingStatus != RecordingIncomplete {
		t.Fatalf("timed out finalization = %+v", finalized)
	}
}

func TestCoordinatorFinalizeFailureMarksSessionAndRecordingFailed(t *testing.T) {
	repository, store, _, roomID, now := openRepository(t)
	defer store.Close()
	coordinator, err := newTestCoordinator(repository, CoordinatorOptions{
		Now: func() time.Time { return now },
		RecorderFactory: func(context.Context, LiveSession, OpenRequest, CaptureSource) (Recorder, error) {
			return &fakeRecorder{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := coordinator.Open(context.Background(), OpenRequest{
		RoomConfigID: roomID, OperationID: newV7(t), RecordEnabled: true, StartedAt: now,
	}, newFakeCaptureSource(nil))
	if err != nil {
		t.Fatal(err)
	}
	finalized, err := session.Finalize(context.Background(), newV7(t), FinalizeFailure)
	if err != nil {
		t.Fatal(err)
	}
	if finalized.Status != SessionFailed || finalized.RecordingStatus != RecordingFailed {
		t.Fatalf("failure finalization = %+v", finalized)
	}
}

func TestCoordinatorCleansCommittedRowsWhenManifestMaterializationReportsError(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		failCreate     bool
		failActivation bool
	}{
		{name: "create", failCreate: true},
		{name: "activation", failActivation: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			repository, store, _, roomID, now := openRepository(t)
			defer store.Close()
			faulty := &committedErrorRepository{
				SessionRepository: repository,
				failCreate:        testCase.failCreate, failActivation: testCase.failActivation,
			}
			coordinator, err := newTestCoordinator(faulty, CoordinatorOptions{Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := coordinator.Open(context.Background(), OpenRequest{
				RoomConfigID: roomID, OperationID: newV7(t), RecordEnabled: false, StartedAt: now,
			}, newFakeCaptureSource(nil)); err == nil {
				t.Fatal("Open() error = nil, want injected post-commit error")
			}
			if active, found, err := repository.ActiveForRoom(context.Background(), roomID); err != nil || found {
				t.Fatalf("ActiveForRoom() after cleanup = (%+v, %v, %v)", active, found, err)
			}
			if _, err := repository.Create(context.Background(), CreateSessionInput{
				RoomConfigID: roomID, OperationID: newV7(t), Recording: RecordingDisabled, StartedAt: now,
			}); err != nil {
				t.Fatalf("new session remained blocked after cleanup: %v", err)
			}
		})
	}
}

func TestCoordinatorCancelledFinalizeStillPersistsInterruptedTerminalState(t *testing.T) {
	repository, store, _, roomID, now := openRepository(t)
	defer store.Close()
	coordinator, err := newTestCoordinator(repository, CoordinatorOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	session, err := coordinator.Open(context.Background(), OpenRequest{
		RoomConfigID: roomID, OperationID: newV7(t), RecordEnabled: false, StartedAt: now,
	}, newFakeCaptureSource(nil))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	finalized, err := session.Finalize(ctx, newV7(t), FinalizeShutdown)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Finalize() error = %v, want canceled", err)
	}
	if finalized.Status != SessionInterrupted || finalized.RecordingStatus != RecordingDisabled {
		t.Fatalf("cancelled finalization = %+v", finalized)
	}
}

func TestCoordinatorOpenSameInitialOperationReusesRuntimeAfterRebind(t *testing.T) {
	repository, store, _, roomID, now := openRepository(t)
	defer store.Close()
	sink := &fakeEventSink{}
	recorder := &fakeRecorder{}
	sinkCalls, recorderCalls := 0, 0
	coordinator, err := newTestCoordinator(repository, CoordinatorOptions{
		Now: func() time.Time { return now },
		EventSinkFactory: func(context.Context, LiveSession, OpenRequest) (EventSink, error) {
			sinkCalls++
			return sink, nil
		},
		RecorderFactory: func(context.Context, LiveSession, OpenRequest, CaptureSource) (Recorder, error) {
			recorderCalls++
			return recorder, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	initialOperationID := newV7(t)
	request := OpenRequest{
		RoomConfigID: roomID, OperationID: initialOperationID,
		RecordEnabled: true, StartedAt: now,
	}
	firstSource := newFakeCaptureSource(nil)
	first, err := coordinator.Open(context.Background(), request, firstSource)
	if err != nil {
		t.Fatal(err)
	}
	retrySource := newFakeCaptureSource(nil)
	second, err := coordinator.Open(context.Background(), request, retrySource)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatal("serial same-operation Open returned a different runtime")
	}
	if sinkCalls != 1 || recorderCalls != 1 || retrySource.next != 0 {
		t.Fatalf("serial reuse side effects = sink:%d recorder:%d retry subscriptions:%d", sinkCalls, recorderCalls, retrySource.next)
	}

	if _, err := first.Rebind(context.Background(), newV7(t), newFakeCaptureSource(nil)); err != nil {
		t.Fatal(err)
	}
	postRebindSource := newFakeCaptureSource(nil)
	third, err := coordinator.Open(context.Background(), request, postRebindSource)
	if err != nil {
		t.Fatal(err)
	}
	if third != first {
		t.Fatal("same initial operation after Rebind returned a different runtime")
	}
	if sinkCalls != 1 || recorderCalls != 1 || postRebindSource.next != 0 {
		t.Fatalf("post-Rebind reuse side effects = sink:%d recorder:%d subscriptions:%d", sinkCalls, recorderCalls, postRebindSource.next)
	}
	if _, err := first.Finalize(context.Background(), newV7(t), FinalizeShutdown); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorConcurrentSameOperationOpenRunsFactoriesOnce(t *testing.T) {
	repository, store, _, roomID, now := openRepository(t)
	defer store.Close()
	sink := &fakeEventSink{}
	recorder := &fakeRecorder{}
	var countsMu sync.Mutex
	sinkCalls, recorderCalls := 0, 0
	coordinator, err := newTestCoordinator(repository, CoordinatorOptions{
		Now: func() time.Time { return now },
		EventSinkFactory: func(context.Context, LiveSession, OpenRequest) (EventSink, error) {
			countsMu.Lock()
			sinkCalls++
			countsMu.Unlock()
			return sink, nil
		},
		RecorderFactory: func(context.Context, LiveSession, OpenRequest, CaptureSource) (Recorder, error) {
			countsMu.Lock()
			recorderCalls++
			countsMu.Unlock()
			return recorder, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := OpenRequest{
		RoomConfigID: roomID, OperationID: newV7(t),
		RecordEnabled: true, StartedAt: now,
	}
	const callers = 24
	start := make(chan struct{})
	sessions := make([]Session, callers)
	sources := make([]*fakeCaptureSource, callers)
	errs := make([]error, callers)
	var wait sync.WaitGroup
	for index := 0; index < callers; index++ {
		index := index
		sources[index] = newFakeCaptureSource(nil)
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			sessions[index], errs[index] = coordinator.Open(context.Background(), request, sources[index])
		}()
	}
	close(start)
	wait.Wait()
	for index := range sessions {
		if errs[index] != nil {
			t.Fatalf("Open[%d]() error = %v", index, errs[index])
		}
		if sessions[index] != sessions[0] {
			t.Fatalf("Open[%d]() returned a different runtime", index)
		}
	}
	countsMu.Lock()
	gotSinkCalls, gotRecorderCalls := sinkCalls, recorderCalls
	countsMu.Unlock()
	subscriptions := 0
	for _, source := range sources {
		source.mu.Lock()
		subscriptions += source.next
		source.mu.Unlock()
	}
	if gotSinkCalls != 1 || gotRecorderCalls != 1 || subscriptions != 1 {
		t.Fatalf("concurrent side effects = sink:%d recorder:%d subscriptions:%d", gotSinkCalls, gotRecorderCalls, subscriptions)
	}
	if _, err := sessions[0].Finalize(context.Background(), newV7(t), FinalizeShutdown); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorRejectsUnregisteredActiveAndTerminalReplay(t *testing.T) {
	t.Run("active", func(t *testing.T) {
		repository, store, _, roomID, now := openRepository(t)
		defer store.Close()
		owner, err := newTestCoordinator(repository, CoordinatorOptions{
			Now: func() time.Time { return now },
			RecorderFactory: func(context.Context, LiveSession, OpenRequest, CaptureSource) (Recorder, error) {
				return &fakeRecorder{}, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		request := OpenRequest{
			RoomConfigID: roomID, OperationID: newV7(t),
			RecordEnabled: true, StartedAt: now,
		}
		session, err := owner.Open(context.Background(), request, newFakeCaptureSource(nil))
		if err != nil {
			t.Fatal(err)
		}
		sinkCalls, recorderCalls := 0, 0
		orphan, err := newTestCoordinator(repository, CoordinatorOptions{
			Now: func() time.Time { return now },
			EventSinkFactory: func(context.Context, LiveSession, OpenRequest) (EventSink, error) {
				sinkCalls++
				return &fakeEventSink{}, nil
			},
			RecorderFactory: func(context.Context, LiveSession, OpenRequest, CaptureSource) (Recorder, error) {
				recorderCalls++
				return &fakeRecorder{}, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		replaySource := newFakeCaptureSource(nil)
		if _, err := orphan.Open(context.Background(), request, replaySource); !errors.Is(err, ErrActiveSessionExists) {
			t.Fatalf("orphan active replay error = %v, want ErrActiveSessionExists", err)
		}
		if sinkCalls != 0 || recorderCalls != 0 || replaySource.next != 0 {
			t.Fatalf("orphan active replay side effects = sink:%d recorder:%d subscriptions:%d", sinkCalls, recorderCalls, replaySource.next)
		}
		if _, err := session.Finalize(context.Background(), newV7(t), FinalizeShutdown); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("terminal", func(t *testing.T) {
		repository, store, _, roomID, now := openRepository(t)
		defer store.Close()
		owner, err := newTestCoordinator(repository, CoordinatorOptions{Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		request := OpenRequest{
			RoomConfigID: roomID, OperationID: newV7(t),
			RecordEnabled: false, StartedAt: now,
		}
		session, err := owner.Open(context.Background(), request, newFakeCaptureSource(nil))
		if err != nil {
			t.Fatal(err)
		}
		finalOperationID := newV7(t)
		completed, err := session.Finalize(context.Background(), finalOperationID, FinalizeShutdown)
		if err != nil {
			t.Fatal(err)
		}
		sinkCalls := 0
		replay, err := newTestCoordinator(repository, CoordinatorOptions{
			Now: func() time.Time { return now },
			EventSinkFactory: func(context.Context, LiveSession, OpenRequest) (EventSink, error) {
				sinkCalls++
				return &fakeEventSink{}, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		replaySource := newFakeCaptureSource(nil)
		replayRequest := request
		replayRequest.OperationID = finalOperationID
		if _, err := replay.Open(context.Background(), replayRequest, replaySource); !errors.Is(err, ErrCaptureFinalized) {
			t.Fatalf("terminal replay error = %v, want ErrCaptureFinalized", err)
		}
		if sinkCalls != 0 || replaySource.next != 0 {
			t.Fatalf("terminal replay side effects = sink:%d subscriptions:%d", sinkCalls, replaySource.next)
		}
		persisted, err := repository.Get(context.Background(), completed.ID)
		if err != nil {
			t.Fatal(err)
		}
		if persisted.Status != SessionCompleted || persisted.RecordingStatus != RecordingDisabled {
			t.Fatalf("terminal replay changed persisted session: %+v", persisted)
		}
	})
}

func TestCoordinatorMarkFinalizingFailureStillCleansAndCanRetryTerminal(t *testing.T) {
	repository, store, _, roomID, now := openRepository(t)
	defer store.Close()
	faulty := &markFinalizingFailureRepository{SessionRepository: repository, failures: 1}
	recorder := &fakeRecorder{}
	sink := &fakeEventSink{}
	coordinator, err := newTestCoordinator(faulty, CoordinatorOptions{
		Now: func() time.Time { return now },
		EventSinkFactory: func(context.Context, LiveSession, OpenRequest) (EventSink, error) {
			return sink, nil
		},
		RecorderFactory: func(context.Context, LiveSession, OpenRequest, CaptureSource) (Recorder, error) {
			return recorder, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := OpenRequest{
		RoomConfigID: roomID, OperationID: newV7(t),
		RecordEnabled: true, StartedAt: now,
	}
	source := newFakeCaptureSource(nil)
	session, err := coordinator.Open(context.Background(), request, source)
	if err != nil {
		t.Fatal(err)
	}
	finalOperationID := newV7(t)
	pending, err := session.Finalize(context.Background(), finalOperationID, FinalizeShutdown)
	if !errors.Is(err, errInjectedMarkFinalizing) {
		t.Fatalf("first Finalize() error = %v, want injected mark failure", err)
	}
	if pending.Status != SessionRecording || pending.RecordingStatus != RecordingActive {
		t.Fatalf("failed mark changed runtime snapshot: %+v", pending)
	}
	if recorder.stops != 1 || sink.flushes != 1 || source.unsubscribed != 1 {
		t.Fatalf("cleanup after failed mark = recorder:%d sink:%d unsubscribe:%d", recorder.stops, sink.flushes, source.unsubscribed)
	}
	active, found, err := repository.ActiveForRoom(context.Background(), roomID)
	if err != nil || !found || active.Status != SessionRecording {
		t.Fatalf("persisted row after failed mark = (%+v, %v, %v)", active, found, err)
	}
	retrySource := newFakeCaptureSource(nil)
	reused, err := coordinator.Open(context.Background(), request, retrySource)
	if err != nil {
		t.Fatal(err)
	}
	if reused != session || retrySource.next != 0 {
		t.Fatalf("failed-mark runtime was not retained: same=%v subscriptions=%d", reused == session, retrySource.next)
	}
	acceptedBefore := sink.accepted
	source.emitStale(0, &douyinLive.LiveMessage{ReceivedAt: now.Add(time.Second)})
	if sink.accepted != acceptedBefore {
		t.Fatalf("failed-mark runtime accepted a message after cleanup: before=%d after=%d", acceptedBefore, sink.accepted)
	}
	if _, err := session.Rebind(context.Background(), newV7(t), newFakeCaptureSource(nil)); !errors.Is(err, ErrCaptureFinalized) {
		t.Fatalf("Rebind() after failed mark error = %v, want ErrCaptureFinalized", err)
	}
	if _, err := coordinator.Open(context.Background(), OpenRequest{
		RoomConfigID: roomID, OperationID: newV7(t), RecordEnabled: true, StartedAt: now,
	}, newFakeCaptureSource(nil)); !errors.Is(err, ErrActiveSessionExists) {
		t.Fatalf("different operation during pending terminal error = %v", err)
	}

	completed, err := session.Finalize(context.Background(), finalOperationID, FinalizeShutdown)
	if err != nil {
		t.Fatalf("retry Finalize() error = %v", err)
	}
	if completed.Status != SessionCompleted || completed.RecordingStatus != RecordingCompleted {
		t.Fatalf("retry finalization = %+v", completed)
	}
	if recorder.stops != 1 || sink.flushes != 1 || source.unsubscribed != 1 {
		t.Fatalf("retry duplicated cleanup = recorder:%d sink:%d unsubscribe:%d", recorder.stops, sink.flushes, source.unsubscribed)
	}
	if active, found, err := repository.ActiveForRoom(context.Background(), roomID); err != nil || found {
		t.Fatalf("active row after retry terminal = (%+v, %v, %v)", active, found, err)
	}
}
func TestCoordinatorFinalizeRetryUsesFrozenTerminalTarget(t *testing.T) {
	testCases := []struct {
		name              string
		recordEnabled     bool
		recorderMode      string
		reason            FinalizeReason
		wantStatus        SessionStatus
		wantRecording     RecordingStatus
		wantCleanupError  bool
		wantContextExpiry bool
	}{
		{name: "disabled", reason: FinalizeShutdown, wantStatus: SessionCompleted, wantRecording: RecordingDisabled},
		{name: "unavailable", recordEnabled: true, recorderMode: "unavailable", reason: FinalizeShutdown, wantStatus: SessionCompleted, wantRecording: RecordingUnavailable},
		{name: "timeout", recordEnabled: true, recorderMode: "timeout", reason: FinalizeShutdown, wantStatus: SessionInterrupted, wantRecording: RecordingIncomplete, wantCleanupError: true, wantContextExpiry: true},
		{name: "failure_active", recordEnabled: true, recorderMode: "active", reason: FinalizeFailure, wantStatus: SessionFailed, wantRecording: RecordingFailed},
		{name: "failure_disabled", reason: FinalizeFailure, wantStatus: SessionFailed, wantRecording: RecordingDisabled},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			repository, store, _, roomID, now := openRepository(t)
			defer store.Close()
			faulty := &terminalTransitionFailureRepository{SessionRepository: repository, failures: 1}
			options := CoordinatorOptions{Now: func() time.Time { return now }}
			switch testCase.recorderMode {
			case "unavailable":
				options.RecorderFactory = func(context.Context, LiveSession, OpenRequest, CaptureSource) (Recorder, error) {
					return nil, ErrRecordingUnavailable
				}
			case "timeout":
				options.RecorderFactory = func(context.Context, LiveSession, OpenRequest, CaptureSource) (Recorder, error) {
					return &fakeRecorder{stopFunc: func(ctx context.Context) error {
						<-ctx.Done()
						return errors.New("sensitive recorder timeout detail")
					}}, nil
				}
			case "active":
				options.RecorderFactory = func(context.Context, LiveSession, OpenRequest, CaptureSource) (Recorder, error) {
					return &fakeRecorder{}, nil
				}
			}
			coordinator, err := newTestCoordinator(faulty, options)
			if err != nil {
				t.Fatal(err)
			}
			session, err := coordinator.Open(context.Background(), OpenRequest{
				RoomConfigID: roomID, OperationID: newV7(t),
				RecordEnabled: testCase.recordEnabled, StartedAt: now,
			}, newFakeCaptureSource(nil))
			if err != nil {
				t.Fatal(err)
			}
			finalOperationID := newV7(t)
			var firstCtx context.Context = context.Background()
			cancel := func() {}
			if testCase.wantContextExpiry {
				firstCtx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
			}
			pending, firstErr := session.Finalize(firstCtx, finalOperationID, testCase.reason)
			cancel()
			if !errors.Is(firstErr, errInjectedTerminalTransition) {
				t.Fatalf("first Finalize() error = %v, want terminal transition failure", firstErr)
			}
			if testCase.wantContextExpiry && !errors.Is(firstErr, context.DeadlineExceeded) {
				t.Fatalf("first Finalize() error = %v, want deadline", firstErr)
			}
			if pending.Status != SessionFinalizing || pending.RecordingStatus != RecordingFinalizing {
				t.Fatalf("pending terminal retry session = %+v", pending)
			}

			completed, retryErr := session.Finalize(context.Background(), finalOperationID, FinalizeShutdown)
			if testCase.wantCleanupError {
				if !errors.Is(retryErr, ErrCaptureCleanup) || strings.Contains(retryErr.Error(), "sensitive recorder") {
					t.Fatalf("retry cleanup error = %v, want masked ErrCaptureCleanup", retryErr)
				}
			} else if retryErr != nil {
				t.Fatalf("retry Finalize() error = %v", retryErr)
			}
			if completed.Status != testCase.wantStatus || completed.RecordingStatus != testCase.wantRecording {
				t.Fatalf("retry finalization = %+v, want status=%s recording=%s", completed, testCase.wantStatus, testCase.wantRecording)
			}
			attempts := faulty.snapshotAttempts()
			if len(attempts) != 2 {
				t.Fatalf("terminal transition attempts = %v, want 2", attempts)
			}
			if attempts[0] != attempts[1] || attempts[0].status != testCase.wantStatus || attempts[0].recordingStatus != testCase.wantRecording || attempts[0].endedAt == 0 {
				t.Fatalf("terminal target was not frozen: %v", attempts)
			}
		})
	}
}

func TestCoordinatorMasksEventSinkFactoryErrorAndPreservesDisabledRecording(t *testing.T) {
	repository, store, _, roomID, now := openRepository(t)
	defer store.Close()
	secret := errors.New("sensitive sink setup credential")
	coordinator, err := newTestCoordinator(repository, CoordinatorOptions{
		Now: func() time.Time { return now },
		EventSinkFactory: func(context.Context, LiveSession, OpenRequest) (EventSink, error) {
			return nil, secret
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	operationID := newV7(t)
	_, err = coordinator.Open(context.Background(), OpenRequest{
		RoomConfigID: roomID, OperationID: operationID,
		RecordEnabled: false, StartedAt: now,
	}, newFakeCaptureSource(nil))
	if !errors.Is(err, ErrCaptureEventSink) || errors.Is(err, secret) || strings.Contains(err.Error(), secret.Error()) {
		t.Fatalf("Open() error = %v, want masked ErrCaptureEventSink", err)
	}
	persisted, err := repository.Create(context.Background(), CreateSessionInput{
		RoomConfigID: roomID, OperationID: operationID,
		Recording: RecordingDisabled, StartedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != SessionFailed || persisted.RecordingStatus != RecordingDisabled {
		t.Fatalf("failed disabled Open persisted = %+v", persisted)
	}
}

func TestCoordinatorMasksFinalizeComponentErrors(t *testing.T) {
	repository, store, _, roomID, now := openRepository(t)
	defer store.Close()
	recorderSecret := errors.New("sensitive recorder process command")
	sinkSecret := errors.New("sensitive sink spool path")
	recorder := &fakeRecorder{stopFunc: func(context.Context) error { return recorderSecret }}
	sink := &fakeEventSink{flushFunc: func(context.Context) error { return sinkSecret }}
	coordinator, err := newTestCoordinator(repository, CoordinatorOptions{
		Now: func() time.Time { return now },
		EventSinkFactory: func(context.Context, LiveSession, OpenRequest) (EventSink, error) {
			return sink, nil
		},
		RecorderFactory: func(context.Context, LiveSession, OpenRequest, CaptureSource) (Recorder, error) {
			return recorder, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := coordinator.Open(context.Background(), OpenRequest{
		RoomConfigID: roomID, OperationID: newV7(t),
		RecordEnabled: true, StartedAt: now,
	}, newFakeCaptureSource(nil))
	if err != nil {
		t.Fatal(err)
	}
	finalized, err := session.Finalize(context.Background(), newV7(t), FinalizeShutdown)
	if !errors.Is(err, ErrCaptureCleanup) || errors.Is(err, recorderSecret) || errors.Is(err, sinkSecret) {
		t.Fatalf("Finalize() error = %v, want masked ErrCaptureCleanup", err)
	}
	if strings.Contains(err.Error(), recorderSecret.Error()) || strings.Contains(err.Error(), sinkSecret.Error()) {
		t.Fatalf("Finalize() leaked component detail: %v", err)
	}
	if finalized.Status != SessionInterrupted || finalized.RecordingStatus != RecordingIncomplete {
		t.Fatalf("component failure terminal state = %+v", finalized)
	}
}

func TestNewCoordinatorRequiresEventSinkFactory(t *testing.T) {
	repository, store, _, _, _ := openRepository(t)
	defer store.Close()

	coordinator, err := NewCoordinator(repository, CoordinatorOptions{})
	if err == nil || coordinator != nil {
		t.Fatalf("NewCoordinator() = (%v, %v), want configuration error", coordinator, err)
	}
	if !strings.Contains(err.Error(), "event sink factory") {
		t.Fatalf("NewCoordinator() error = %v, want event sink factory detail", err)
	}
}

type blockingEventSink struct {
	entered      chan struct{}
	release      chan struct{}
	flushEntered chan struct{}
	flushOnce    sync.Once
}

func (s *blockingEventSink) Accept(*douyinLive.LiveMessage) {
	close(s.entered)
	<-s.release
}

func (s *blockingEventSink) FlushAndClose(context.Context) error {
	if s.flushEntered != nil {
		s.flushOnce.Do(func() {
			close(s.flushEntered)
		})
	}
	return nil
}

func TestEventSinkAcceptDoesNotHoldSessionStateLock(t *testing.T) {
	repository, store, _, roomID, now := openRepository(t)
	defer store.Close()
	sink := &blockingEventSink{entered: make(chan struct{}), release: make(chan struct{})}
	coordinator, err := NewCoordinator(repository, CoordinatorOptions{
		Now: func() time.Time { return now },
		EventSinkFactory: func(context.Context, LiveSession, OpenRequest) (EventSink, error) {
			return sink, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	source := newFakeCaptureSource(nil)
	session, err := coordinator.Open(context.Background(), OpenRequest{
		RoomConfigID: roomID, OperationID: newV7(t), StartedAt: now,
	}, source)
	if err != nil {
		t.Fatal(err)
	}

	go source.emitCurrent(&douyinLive.LiveMessage{ReceivedAt: now})
	select {
	case <-sink.entered:
	case <-time.After(time.Second):
		t.Fatal("event sink was not invoked")
	}

	snapshotDone := make(chan struct{})
	go func() {
		_ = session.Snapshot()
		close(snapshotDone)
	}()
	select {
	case <-snapshotDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("EventSink.Accept held session state lock")
	}

	close(sink.release)
	if _, err := session.Finalize(context.Background(), newV7(t), FinalizeShutdown); err != nil {
		t.Fatal(err)
	}
}

func TestFinalizeWaitsForInflightEventAcceptBeforeSinkClose(t *testing.T) {
	repository, store, _, roomID, now := openRepository(t)
	defer store.Close()
	sink := &blockingEventSink{
		entered:      make(chan struct{}),
		release:      make(chan struct{}),
		flushEntered: make(chan struct{}),
	}
	coordinator, err := NewCoordinator(repository, CoordinatorOptions{
		Now: func() time.Time { return now },
		EventSinkFactory: func(context.Context, LiveSession, OpenRequest) (EventSink, error) {
			return sink, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	source := newFakeCaptureSource(nil)
	session, err := coordinator.Open(context.Background(), OpenRequest{
		RoomConfigID: roomID, OperationID: newV7(t), StartedAt: now,
	}, source)
	if err != nil {
		t.Fatal(err)
	}

	emitDone := make(chan struct{})
	go func() {
		source.emitCurrent(&douyinLive.LiveMessage{ReceivedAt: now})
		close(emitDone)
	}()
	select {
	case <-sink.entered:
	case <-time.After(time.Second):
		t.Fatal("event sink was not invoked")
	}

	finalizeDone := make(chan error, 1)
	go func() {
		_, finalizeErr := session.Finalize(context.Background(), newV7(t), FinalizeShutdown)
		finalizeDone <- finalizeErr
	}()
	unsubscribeDeadline := time.Now().Add(time.Second)
	for {
		source.mu.Lock()
		unsubscribed := source.unsubscribed > 0
		source.mu.Unlock()
		if unsubscribed {
			break
		}
		if time.Now().After(unsubscribeDeadline) {
			t.Fatal("Finalize did not unsubscribe before waiting for callbacks")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-sink.flushEntered:
		t.Fatal("sink closed before the in-flight Accept returned")
	case <-time.After(100 * time.Millisecond):
	}

	close(sink.release)
	select {
	case <-sink.flushEntered:
	case <-time.After(time.Second):
		t.Fatal("sink was not closed after the in-flight Accept returned")
	}
	select {
	case err := <-finalizeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Finalize did not complete")
	}
	select {
	case <-emitDone:
	case <-time.After(time.Second):
		t.Fatal("message callback did not complete")
	}
}
func containsOrderedSubsequence(values, wanted []string) bool {
	if len(wanted) == 0 {
		return true
	}
	index := 0
	for _, value := range values {
		if value == wanted[index] {
			index++
			if index == len(wanted) {
				return true
			}
		}
	}
	return false
}
