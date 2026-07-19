package capture

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	douyinLive "github.com/jwwsjlm/douyinLive/v2"
)

var (
	ErrRecordingUnavailable  = errors.New("recording is unavailable")
	ErrCaptureFinalized      = errors.New("capture session is finalized")
	ErrCaptureSubscription   = errors.New("capture source subscription failed")
	ErrCaptureEventSink      = errors.New("capture event sink is unavailable")
	ErrCaptureCleanup        = errors.New("capture component cleanup failed")
	ErrCaptureCleanupPending = errors.New("capture component cleanup is still running")
)

type FinalizeReason string

const (
	FinalizeOffline  FinalizeReason = "offline"
	FinalizeStopped  FinalizeReason = "stopped"
	FinalizeShutdown FinalizeReason = "shutdown"
	FinalizeFailure  FinalizeReason = "failure"
)

type RecordingProfile struct {
	Quality        string
	SegmentMinutes int
	SaveDirectory  string
}

type OpenRequest struct {
	RoomConfigID   string
	OperationID    string
	PlatformRoomID string
	Title          string
	RecordEnabled  bool
	Profile        RecordingProfile
	StartedAt      time.Time
}

// CaptureSource is the existing DouyinLive surface needed by a capture
// session. Stream URLs and messages remain inside Go and are never serialized.
type CaptureSource interface {
	ResolveStreams() ([]douyinLive.ResolvedStream, error)
	SubscribeMessage(douyinLive.LiveMessageHandler) string
	Unsubscribe(string)
}

// Recorder represents an already-started recorder owned by one live session.
// RecorderFactory is responsible for resolving streams and starting it. Rebind
// refreshes or rebuilds the input for the same session. Both methods must
// observe ctx and return promptly when it is cancelled.
type Recorder interface {
	Rebind(context.Context, CaptureSource) error
	Stop(context.Context) error
}

// RecorderEventSource is an optional extension implemented by recorders that
// can report an asynchronous process exit. Stop must eventually close Events.
// IsCurrentEvent prevents a queued exit from an older bind from degrading a
// newly established recording attempt.
type RecorderEventSource interface {
	Events() <-chan RecorderEvent
	IsCurrentEvent(RecorderEvent) bool
}

type RecorderFactory func(context.Context, LiveSession, OpenRequest, CaptureSource) (Recorder, error)

// EventSink.Accept must be bounded and non-blocking. Implementations may spool
// internally, but callbacks from DouyinLive must never perform database work.
type EventSink interface {
	Accept(*douyinLive.LiveMessage)
	FlushAndClose(context.Context) error
}

type EventSinkFactory func(context.Context, LiveSession, OpenRequest) (EventSink, error)

type CoordinatorOptions struct {
	RecorderFactory  RecorderFactory
	EventSinkFactory EventSinkFactory
	Now              func() time.Time
	CommitTimeout    time.Duration
}

type Session interface {
	Snapshot() LiveSession
	Rebind(context.Context, string, CaptureSource) (LiveSession, error)
	Finalize(context.Context, string, FinalizeReason) (LiveSession, error)
}

type CaptureCoordinator interface {
	Open(context.Context, OpenRequest, CaptureSource) (Session, error)
}

type Coordinator struct {
	repository       SessionRepository
	recorderFactory  RecorderFactory
	eventSinkFactory EventSinkFactory
	now              func() time.Time
	commitTimeout    time.Duration
	registryMu       sync.Mutex
	runtimes         map[string]*coordinatorRuntimeEntry
}

func NewCoordinator(repository SessionRepository, options CoordinatorOptions) (*Coordinator, error) {
	if repository == nil {
		return nil, errors.New("capture coordinator repository is nil")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.CommitTimeout <= 0 {
		options.CommitTimeout = time.Second
	}
	if options.RecorderFactory == nil {
		options.RecorderFactory = func(context.Context, LiveSession, OpenRequest, CaptureSource) (Recorder, error) {
			return nil, ErrRecordingUnavailable
		}
	}
	if options.EventSinkFactory == nil {
		return nil, errors.New("capture coordinator event sink factory is nil")
	}
	return &Coordinator{
		repository: repository, recorderFactory: options.RecorderFactory,
		eventSinkFactory: options.EventSinkFactory, now: options.Now,
		commitTimeout: options.CommitTimeout,
	}, nil
}

type coordinatorRuntimeEntry struct {
	initialOperationID string
	ready              chan struct{}
	session            *sessionRuntime
	err                error
}

func (c *Coordinator) Open(ctx context.Context, request OpenRequest, source CaptureSource) (Session, error) {
	if err := coordinatorContext(ctx); err != nil {
		return nil, err
	}
	if source == nil {
		return nil, errors.New("capture source is nil")
	}
	entry, owner, err := c.reserveOpen(request.RoomConfigID, request.OperationID)
	if err != nil {
		return nil, err
	}
	if !owner {
		return awaitRuntime(ctx, entry)
	}

	runtime, openErr := c.openReserved(ctx, request, source)
	c.finishOpen(request.RoomConfigID, entry, runtime, openErr)
	if openErr != nil {
		return nil, openErr
	}
	return runtime, nil
}

func (c *Coordinator) reserveOpen(roomConfigID, operationID string) (*coordinatorRuntimeEntry, bool, error) {
	c.registryMu.Lock()
	defer c.registryMu.Unlock()
	if c.runtimes == nil {
		c.runtimes = make(map[string]*coordinatorRuntimeEntry)
	}
	if existing := c.runtimes[roomConfigID]; existing != nil {
		if existing.initialOperationID != operationID {
			return nil, false, ErrActiveSessionExists
		}
		return existing, false, nil
	}
	entry := &coordinatorRuntimeEntry{initialOperationID: operationID, ready: make(chan struct{})}
	c.runtimes[roomConfigID] = entry
	return entry, true, nil
}

func awaitRuntime(ctx context.Context, entry *coordinatorRuntimeEntry) (Session, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-entry.ready:
		if entry.session != nil {
			return entry.session, nil
		}
		if entry.err != nil {
			return nil, entry.err
		}
		return nil, errors.New("capture runtime open finished without a result")
	}
}

func (c *Coordinator) finishOpen(roomConfigID string, entry *coordinatorRuntimeEntry, runtime *sessionRuntime, err error) {
	c.registryMu.Lock()
	entry.session = runtime
	entry.err = err
	if err != nil && c.runtimes[roomConfigID] == entry {
		delete(c.runtimes, roomConfigID)
	}
	close(entry.ready)
	c.registryMu.Unlock()
}

func (c *Coordinator) unregisterRuntime(runtime *sessionRuntime, roomConfigID string) {
	c.registryMu.Lock()
	if entry := c.runtimes[roomConfigID]; entry != nil && entry.session == runtime {
		delete(c.runtimes, roomConfigID)
	}
	c.registryMu.Unlock()
}

func (c *Coordinator) openReserved(ctx context.Context, request OpenRequest, source CaptureSource) (*sessionRuntime, error) {
	initialRecording := RecordingDisabled
	if request.RecordEnabled {
		initialRecording = RecordingPending
	}
	created, err := c.repository.Create(ctx, CreateSessionInput{
		RoomConfigID: request.RoomConfigID, OperationID: request.OperationID,
		PlatformRoomID: request.PlatformRoomID, Title: request.Title,
		Recording: initialRecording, StartedAt: request.StartedAt,
	})
	if created.ID != "" {
		var stateErr error
		switch created.Status {
		case SessionStarting:
		case SessionRecording, SessionFinalizing:
			stateErr = ErrActiveSessionExists
		case SessionCompleted, SessionInterrupted, SessionFailed:
			stateErr = ErrCaptureFinalized
		default:
			stateErr = fmt.Errorf("cannot open capture session in status %q", created.Status)
		}
		if stateErr != nil {
			return nil, errors.Join(stateErr, err)
		}
	}
	if err != nil {
		if created.ID != "" {
			c.failOpen(ctx, created, err)
		}
		return nil, err
	}
	if created.ID == "" {
		return nil, errors.New("capture repository created an empty session")
	}

	sink, err := c.eventSinkFactory(ctx, created, request)
	if err != nil || sink == nil {
		factoryErr := err
		if factoryErr == nil {
			factoryErr = errors.New("event sink factory returned nil")
		}
		c.failOpen(ctx, created, factoryErr)
		publicErr := error(ErrCaptureEventSink)
		if ctx.Err() != nil {
			publicErr = errors.Join(publicErr, ctx.Err())
		}
		return nil, publicErr
	}
	runtime := &sessionRuntime{
		coordinator: c, current: created, operationID: request.OperationID,
		source: source, sink: sink,
	}
	subscriptionID := runtime.subscribe(source, request.OperationID)
	if subscriptionID == "" {
		_ = boundedCall(ctx, sink.FlushAndClose)
		c.failOpen(ctx, created, ErrCaptureSubscription)
		return nil, ErrCaptureSubscription
	}
	runtime.subscriptionID = subscriptionID

	targetRecording := RecordingDisabled
	if request.RecordEnabled {
		recorder, recorderErr := c.recorderFactory(ctx, created, request, source)
		if recorderErr != nil || recorder == nil {
			if recorder != nil {
				_ = stopOwnedRecorder(recorder)
			}
			targetRecording = RecordingUnavailable
		} else {
			runtime.recorder = recorder
			targetRecording = RecordingActive
		}
	}
	active, err := c.repository.Transition(ctx, TransitionSessionInput{
		ID: created.ID, ExpectedStatus: SessionStarting,
		ExpectedRecordingStatus: initialRecording, ExpectedOperationID: request.OperationID,
		Status: SessionRecording, RecordingStatus: targetRecording,
	})
	if err != nil {
		runtime.stopAcceptingMessages(source, subscriptionID)
		if runtime.recorder != nil {
			_ = stopOwnedRecorder(runtime.recorder)
		}
		_ = boundedCall(ctx, sink.FlushAndClose)
		failedSession := created
		if active.ID != "" {
			failedSession = active
		}
		c.failOpen(ctx, failedSession, err)
		return nil, fmt.Errorf("activate capture session: %w", err)
	}
	runtime.mu.Lock()
	runtime.current = active
	runtime.mu.Unlock()
	if runtime.recorder != nil {
		runtime.startRecorderEvents(runtime.recorder)
	}
	return runtime, nil
}

func (c *Coordinator) failOpen(ctx context.Context, session LiveSession, cause error) {
	commitCtx, cancel := c.commitContext(ctx)
	defer cancel()
	endedAt := c.now().UTC()
	_, _ = c.repository.Transition(commitCtx, TransitionSessionInput{
		ID: session.ID, ExpectedStatus: session.Status,
		ExpectedRecordingStatus: session.RecordingStatus, ExpectedOperationID: session.OperationID,
		Status: SessionFailed, RecordingStatus: failureRecordingStatus(session.RecordingStatus), EndedAt: &endedAt,
	})
	_ = cause
}

func (c *Coordinator) commitContext(parent context.Context) (context.Context, context.CancelFunc) {
	base := context.Background()
	if parent != nil {
		base = context.WithoutCancel(parent)
	}
	return context.WithTimeout(base, c.commitTimeout)
}

type sessionRuntime struct {
	coordinator               *Coordinator
	operationMu               sync.Mutex
	mu                        sync.Mutex
	acceptWG                  sync.WaitGroup
	current                   LiveSession
	operationID               string
	source                    CaptureSource
	subscriptionID            string
	recorder                  Recorder
	recorderEventsCancel      context.CancelFunc
	sink                      EventSink
	finalizing                bool
	finalized                 bool
	finalizeErr               error
	cleanupErr                error
	cleanupPublicErr          error
	finalizeOriginalRecording RecordingStatus
	finalizeOriginalSet       bool
	finalizeReason            FinalizeReason
	finalizeReasonSet         bool
	terminalStatus            SessionStatus
	terminalRecordingStatus   RecordingStatus
	terminalEndedAt           time.Time
	terminalTargetSet         bool
}

func (s *sessionRuntime) Snapshot() LiveSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

func (s *sessionRuntime) startRecorderEvents(recorder Recorder) {
	eventSource, ok := recorder.(RecorderEventSource)
	if !ok {
		return
	}
	events := eventSource.Events()
	if events == nil {
		return
	}
	watchCtx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	if s.finalizing || s.finalized || !sameRecorderInstance(s.recorder, recorder) {
		s.mu.Unlock()
		cancel()
		return
	}
	if s.recorderEventsCancel != nil {
		s.recorderEventsCancel()
	}
	s.recorderEventsCancel = cancel
	s.mu.Unlock()

	go func() {
		for {
			select {
			case <-watchCtx.Done():
				return
			case event, open := <-events:
				if !open {
					return
				}
				retryDelay := 25 * time.Millisecond
				for !s.handleRecorderEvent(recorder, eventSource, event) {
					timer := time.NewTimer(retryDelay)
					select {
					case <-watchCtx.Done():
						if !timer.Stop() {
							<-timer.C
						}
						return
					case <-timer.C:
					}
					if retryDelay < time.Second {
						retryDelay *= 2
						if retryDelay > time.Second {
							retryDelay = time.Second
						}
					}
				}
			}
		}
	}()
}

func (s *sessionRuntime) handleRecorderEvent(
	recorder Recorder,
	eventSource RecorderEventSource,
	event RecorderEvent,
) bool {
	if event.Kind != RecorderEventProcessExited {
		return true
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()

	if !eventSource.IsCurrentEvent(event) {
		return true
	}
	s.mu.Lock()
	current := s.current
	active := !s.finalizing && !s.finalized &&
		sameRecorderInstance(s.recorder, recorder) &&
		current.Status == SessionRecording &&
		(current.RecordingStatus == RecordingActive || current.RecordingStatus == RecordingReconnecting)
	s.mu.Unlock()
	if !active {
		return true
	}

	commitCtx, cancel := s.coordinator.commitContext(context.Background())
	next, transitionErr := s.coordinator.repository.Transition(commitCtx, TransitionSessionInput{
		ID: current.ID, ExpectedStatus: current.Status,
		ExpectedRecordingStatus: current.RecordingStatus, ExpectedOperationID: current.OperationID,
		Status: current.Status, RecordingStatus: RecordingUnavailable,
	})
	cancel()

	// A process exit must never disappear behind a transient SQLite/CAS or
	// manifest error. Keep the recorder and event correlation alive until the
	// repository confirms the unavailable state; the watcher retries while
	// Rebind/Finalize remain able to supersede it through operationMu.
	transitionConfirmed := next.ID != "" && next.RecordingStatus == RecordingUnavailable
	if transitionErr != nil && !transitionConfirmed {
		return false
	}
	if !transitionConfirmed {
		return false
	}
	s.mu.Lock()
	s.current = next
	if sameRecorderInstance(s.recorder, recorder) {
		s.cancelRecorderEventsLocked()
	}
	s.mu.Unlock()

	stopCtx, stopCancel := s.coordinator.commitContext(context.Background())
	_ = boundedCall(stopCtx, recorder.Stop)
	stopCancel()
	return true
}

func (s *sessionRuntime) cancelRecorderEventsLocked() {
	if s.recorderEventsCancel != nil {
		s.recorderEventsCancel()
		s.recorderEventsCancel = nil
	}
}

func (s *sessionRuntime) Rebind(ctx context.Context, operationID string, source CaptureSource) (LiveSession, error) {
	if err := coordinatorContext(ctx); err != nil {
		return LiveSession{}, err
	}
	if source == nil {
		return LiveSession{}, errors.New("capture source is nil")
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()

	s.mu.Lock()
	if s.finalized || s.finalizing {
		current := s.current
		s.mu.Unlock()
		return current, ErrCaptureFinalized
	}
	if operationID == s.operationID {
		current := s.current
		s.mu.Unlock()
		return current, nil
	}
	current := s.current
	s.mu.Unlock()

	previousRecording := current.RecordingStatus
	needsRecorderRebind := previousRecording == RecordingActive || previousRecording == RecordingReconnecting
	transitionRecording := previousRecording
	if needsRecorderRebind {
		transitionRecording = RecordingReconnecting
	}
	reconnecting, err := s.coordinator.repository.Transition(ctx, TransitionSessionInput{
		ID: current.ID, ExpectedStatus: current.Status,
		ExpectedRecordingStatus: current.RecordingStatus, ExpectedOperationID: current.OperationID,
		Status: current.Status, RecordingStatus: transitionRecording,
		NextOperationID: operationID,
	})
	if err != nil {
		return current, err
	}
	s.mu.Lock()
	s.current = reconnecting
	s.operationID = operationID
	recorder := s.recorder
	s.mu.Unlock()

	newSubscriptionID := s.subscribe(source, operationID)
	if newSubscriptionID == "" {
		return reconnecting, ErrCaptureSubscription
	}
	targetRecording := transitionRecording
	if needsRecorderRebind {
		if recorder == nil || boundedCall(ctx, func(callCtx context.Context) error {
			return recorder.Rebind(callCtx, source)
		}) != nil {
			targetRecording = RecordingUnavailable
			if recorder != nil {
				// Stop owns shared asynchronous cleanup. Keep the stopped/stopping
				// recorder attached so Finalize can wait for that owner.
				_ = boundedCall(ctx, recorder.Stop)
			}
		} else {
			targetRecording = RecordingActive
		}
	}
	next, err := s.coordinator.repository.Transition(ctx, TransitionSessionInput{
		ID: reconnecting.ID, ExpectedStatus: reconnecting.Status,
		ExpectedRecordingStatus: reconnecting.RecordingStatus, ExpectedOperationID: operationID,
		Status: reconnecting.Status, RecordingStatus: targetRecording,
	})
	if err != nil {
		source.Unsubscribe(newSubscriptionID)
		return reconnecting, err
	}
	s.mu.Lock()
	oldSource, oldSubscriptionID := s.source, s.subscriptionID
	s.current = next
	s.source = source
	s.subscriptionID = newSubscriptionID
	s.recorder = recorder
	if recorder == nil || targetRecording == RecordingUnavailable {
		s.cancelRecorderEventsLocked()
	}
	s.mu.Unlock()
	if oldSource != nil && oldSubscriptionID != "" {
		oldSource.Unsubscribe(oldSubscriptionID)
	}
	return next, nil
}

func (s *sessionRuntime) Finalize(ctx context.Context, operationID string, reason FinalizeReason) (LiveSession, error) {
	if ctx == nil {
		return LiveSession{}, errors.New("capture finalize context is nil")
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()

	s.mu.Lock()
	if s.finalized {
		current, err := s.current, s.finalizeErr
		s.mu.Unlock()
		return current, err
	}
	current := s.current
	if !s.finalizeOriginalSet {
		s.finalizeOriginalRecording = current.RecordingStatus
		s.finalizeOriginalSet = true
	}
	if !s.finalizeReasonSet {
		s.finalizeReason = reason
		s.finalizeReasonSet = true
	}
	originalRecording := s.finalizeOriginalRecording
	effectiveReason := s.finalizeReason
	componentErr := s.cleanupErr
	publicComponentErr := s.cleanupPublicErr
	targetStatus := s.terminalStatus
	targetRecording := s.terminalRecordingStatus
	targetEndedAt := s.terminalEndedAt
	targetSet := s.terminalTargetSet
	s.mu.Unlock()

	markCommitted := current.Status == SessionFinalizing && current.OperationID == operationID
	var materializationErr error
	if !markCommitted {
		transitionCtx, cancel := s.coordinator.commitContext(ctx)
		finalizing, err := s.coordinator.repository.Transition(transitionCtx, TransitionSessionInput{
			ID: current.ID, ExpectedStatus: current.Status,
			ExpectedRecordingStatus: current.RecordingStatus, ExpectedOperationID: current.OperationID,
			Status: SessionFinalizing, RecordingStatus: RecordingFinalizing,
			NextOperationID: operationID,
		})
		cancel()
		if err != nil {
			materializationErr = fmt.Errorf("mark capture session finalizing: %w", err)
		}
		if finalizing.ID != "" {
			current = finalizing
			markCommitted = true
		}
	}

	s.mu.Lock()
	if markCommitted {
		s.current = current
		s.operationID = operationID
	}
	s.finalizing = true
	recorder, source, subscriptionID, sink := s.recorder, s.source, s.subscriptionID, s.sink
	s.mu.Unlock()

	if !targetSet {
		var recorderErr, sinkErr error
		if recorder != nil {
			recorderErr = boundedCall(ctx, recorder.Stop)
			if recorderErr != nil && ctx.Err() != nil {
				// Recorder.Stop owns a shared asynchronous completion. A caller
				// deadline only stops this wait; it must not orphan media proxy/DB
				// work or turn the session terminal before that work is observed.
				pendingErr := errors.Join(ErrCaptureCleanupPending, ctx.Err())
				if materializationErr != nil {
					pendingErr = errors.Join(materializationErr, pendingErr)
				}
				s.mu.Lock()
				s.finalizing = true
				s.finalizeErr = pendingErr
				result := s.current
				s.mu.Unlock()
				return result, pendingErr
			}
		}
		if source != nil && subscriptionID != "" {
			source.Unsubscribe(subscriptionID)
		}
		s.acceptWG.Wait()
		if sink != nil {
			sinkErr = boundedCall(ctx, sink.FlushAndClose)
			if sinkErr != nil && ctx.Err() != nil {
				// SessionSink uses the same shared-completion shape as Recorder:
				// its caller deadline does not stop the SQLite spool finalizer.
				pendingErr := errors.Join(ErrCaptureCleanupPending, ctx.Err())
				if materializationErr != nil {
					pendingErr = errors.Join(materializationErr, pendingErr)
				}
				s.mu.Lock()
				if s.source == source && s.subscriptionID == subscriptionID {
					// Message acceptance has converged; only the sink remains owned.
					s.source = nil
					s.subscriptionID = ""
				}
				s.finalizing = true
				s.finalizeErr = pendingErr
				result := s.current
				s.mu.Unlock()
				return result, pendingErr
			}
		}
		componentErr = errors.Join(componentErr, recorderErr, sinkErr, ctx.Err())
		if recorderErr != nil || sinkErr != nil {
			publicComponentErr = errors.Join(publicComponentErr, ErrCaptureCleanup)
		}
		publicComponentErr = errors.Join(publicComponentErr, ctx.Err())

		targetStatus = SessionCompleted
		if effectiveReason == FinalizeFailure {
			targetStatus = SessionFailed
			targetRecording = failureRecordingStatus(originalRecording)
		} else {
			if componentErr != nil {
				targetStatus = SessionInterrupted
			}
			targetRecording = terminalRecordingStatus(originalRecording, componentErr)
		}
		targetEndedAt = s.coordinator.now().UTC()
		targetSet = true

		s.mu.Lock()
		s.recorder = nil
		s.cancelRecorderEventsLocked()
		s.source = nil
		s.subscriptionID = ""
		s.sink = nil
		s.cleanupErr = componentErr
		s.cleanupPublicErr = publicComponentErr
		s.terminalStatus = targetStatus
		s.terminalRecordingStatus = targetRecording
		s.terminalEndedAt = targetEndedAt
		s.terminalTargetSet = true
		s.mu.Unlock()
	}

	if !markCommitted {
		finalErr := errors.Join(materializationErr, publicComponentErr)
		s.mu.Lock()
		s.finalizing = true
		s.finalizeErr = finalErr
		result := s.current
		s.mu.Unlock()
		return result, finalErr
	}

	endedAt := targetEndedAt
	commitCtx, cancel := s.coordinator.commitContext(ctx)
	terminal, transitionErr := s.coordinator.repository.Transition(commitCtx, TransitionSessionInput{
		ID: current.ID, ExpectedStatus: SessionFinalizing,
		ExpectedRecordingStatus: RecordingFinalizing, ExpectedOperationID: operationID,
		Status: targetStatus, RecordingStatus: targetRecording, EndedAt: &endedAt,
	})
	cancel()
	finalErr := errors.Join(materializationErr, publicComponentErr, transitionErr)

	s.mu.Lock()
	terminalCommitted := transitionErr == nil || terminal.ID != ""
	s.finalizing = !terminalCommitted
	if terminalCommitted {
		s.current = terminal
		s.finalized = true
	}
	s.finalizeErr = finalErr
	result := s.current
	s.mu.Unlock()
	if terminalCommitted {
		s.coordinator.unregisterRuntime(s, result.RoomConfigID)
	}
	return result, finalErr
}

func (s *sessionRuntime) subscribe(source CaptureSource, operationID string) string {
	return source.SubscribeMessage(func(message *douyinLive.LiveMessage) {
		s.mu.Lock()
		if s.finalizing || s.finalized || s.operationID != operationID || s.sink == nil {
			s.mu.Unlock()
			return
		}
		sink := s.sink
		s.acceptWG.Add(1)
		s.mu.Unlock()

		defer s.acceptWG.Done()
		sink.Accept(message)
	})
}

func (s *sessionRuntime) stopAcceptingMessages(source CaptureSource, subscriptionID string) {
	s.mu.Lock()
	s.finalizing = true
	s.mu.Unlock()
	if source != nil && subscriptionID != "" {
		source.Unsubscribe(subscriptionID)
	}
	s.acceptWG.Wait()
}

func sameRecorderInstance(left, right Recorder) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftType := reflect.TypeOf(left)
	if leftType != reflect.TypeOf(right) || !leftType.Comparable() {
		return false
	}
	return reflect.ValueOf(left).Interface() == reflect.ValueOf(right).Interface()
}

func failureRecordingStatus(original RecordingStatus) RecordingStatus {
	switch original {
	case RecordingDisabled:
		return RecordingDisabled
	case RecordingUnavailable:
		return RecordingUnavailable
	default:
		return RecordingFailed
	}
}
func terminalRecordingStatus(original RecordingStatus, finalizationErr error) RecordingStatus {
	switch original {
	case RecordingDisabled:
		return RecordingDisabled
	case RecordingUnavailable:
		return RecordingUnavailable
	case RecordingActive:
		if finalizationErr == nil {
			return RecordingCompleted
		}
		return RecordingIncomplete
	case RecordingReconnecting:
		return RecordingIncomplete
	default:
		if finalizationErr == nil {
			return RecordingCompleted
		}
		return RecordingIncomplete
	}
}

func boundedCall(ctx context.Context, call func(context.Context) error) error {
	if call == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("bounded call context is nil")
	}
	result := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				result <- fmt.Errorf("capture component panic: %v", recovered)
			}
		}()
		result <- call(ctx)
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func stopOwnedRecorder(recorder Recorder) error {
	if recorder == nil {
		return nil
	}
	// Open has not published a session owner yet. Keep the registry reservation
	// until Stop's shared completion finishes, even if the opening caller left.
	return boundedCall(context.Background(), recorder.Stop)
}

func coordinatorContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("capture coordinator context is nil")
	}
	return ctx.Err()
}
