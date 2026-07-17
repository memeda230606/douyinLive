package room

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	douyinLive "github.com/jwwsjlm/douyinLive/v2"
	"github.com/jwwsjlm/douyinLive/v2/internal/capture"
)

type fakeLiveClient struct {
	prepareErr error
	startErr   error
	offline    bool
	name       string
	title      string
	started    chan struct{}
	closed     chan struct{}
	startOnce  sync.Once
	closeOnce  sync.Once
}

func newFakeLiveClient() *fakeLiveClient {
	return &fakeLiveClient{started: make(chan struct{}), closed: make(chan struct{})}
}

func (c *fakeLiveClient) PrepareWebSocketContext() error { return c.prepareErr }
func (c *fakeLiveClient) IsKnownOfflineStatus() bool     { return c.offline }
func (c *fakeLiveClient) GetName() string                { return c.name }
func (c *fakeLiveClient) GetTitle() string               { return c.title }
func (c *fakeLiveClient) Start() error {
	c.startOnce.Do(func() { close(c.started) })
	<-c.closed
	return c.startErr
}
func (c *fakeLiveClient) Close()                                               { c.closeOnce.Do(func() { close(c.closed) }) }
func (c *fakeLiveClient) Dispose()                                             { c.Close() }
func (c *fakeLiveClient) ResolveStreams() ([]douyinLive.ResolvedStream, error) { return nil, nil }
func (c *fakeLiveClient) SubscribeMessage(douyinLive.LiveMessageHandler) string {
	return "fake-subscription"
}
func (c *fakeLiveClient) Unsubscribe(string) {}

type scriptedLiveFactory struct {
	mu      sync.Mutex
	clients []LiveClient
	calls   int
}

func (f *scriptedLiveFactory) create(context.Context, RoomConfig, string) (LiveClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.clients) == 0 {
		return nil, errors.New("scripted client queue is empty")
	}
	client := f.clients[0]
	f.clients = f.clients[1:]
	return client, nil
}

func (f *scriptedLiveFactory) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeFinalizeCall struct {
	operationID string
	reason      capture.FinalizeReason
}

type fakeCaptureCoordinator struct {
	mu                              sync.Mutex
	opens                           int
	rebinds                         int
	finalizes                       int
	session                         *fakeCaptureSession
	finalizeStarted                 chan struct{}
	finalizeRelease                 chan struct{}
	finalizeOnce                    sync.Once
	finalizeNonterminal             bool
	finalizeCancellationNonterminal bool
	finalizeFailures                int
	finalizeTerminalErr             error
	finalizeCalls                   []fakeFinalizeCall
}

func newFakeCaptureCoordinator() *fakeCaptureCoordinator { return &fakeCaptureCoordinator{} }

func (c *fakeCaptureCoordinator) Open(_ context.Context, request capture.OpenRequest, _ capture.CaptureSource) (capture.Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opens++
	recording := capture.RecordingDisabled
	if request.RecordEnabled {
		recording = capture.RecordingActive
	}
	current := capture.LiveSession{
		ID: newOperationID(), RoomConfigID: request.RoomConfigID, OperationID: request.OperationID,
		Title: request.Title, Status: capture.SessionRecording, RecordingStatus: recording,
		StartedAt: request.StartedAt.UTC().UnixMilli(), ClockSource: capture.ClockReceived,
	}
	session := &fakeCaptureSession{owner: c, current: current}
	c.session = session
	return session, nil
}

func (c *fakeCaptureCoordinator) counts() (int, int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.opens, c.rebinds, c.finalizes
}

func (c *fakeCaptureCoordinator) finalizeCallSnapshot() []fakeFinalizeCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]fakeFinalizeCall, len(c.finalizeCalls))
	copy(result, c.finalizeCalls)
	return result
}

type fakeCaptureSession struct {
	owner   *fakeCaptureCoordinator
	mu      sync.Mutex
	current capture.LiveSession
}

func (s *fakeCaptureSession) Snapshot() capture.LiveSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

func (s *fakeCaptureSession) Rebind(_ context.Context, operationID string, _ capture.CaptureSource) (capture.LiveSession, error) {
	s.owner.mu.Lock()
	s.owner.rebinds++
	s.owner.mu.Unlock()
	s.mu.Lock()
	s.current.OperationID = operationID
	current := s.current
	s.mu.Unlock()
	return current, nil
}

func (s *fakeCaptureSession) Finalize(ctx context.Context, operationID string, reason capture.FinalizeReason) (capture.LiveSession, error) {
	s.owner.mu.Lock()
	s.owner.finalizes++
	s.owner.finalizeCalls = append(s.owner.finalizeCalls, fakeFinalizeCall{operationID: operationID, reason: reason})
	started, release := s.owner.finalizeStarted, s.owner.finalizeRelease
	s.owner.mu.Unlock()
	if started != nil {
		s.owner.finalizeOnce.Do(func() { close(started) })
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			s.owner.mu.Lock()
			nonterminal := s.owner.finalizeCancellationNonterminal
			s.owner.mu.Unlock()
			s.mu.Lock()
			s.current.OperationID = operationID
			if nonterminal {
				s.current.Status = capture.SessionFinalizing
				s.current.RecordingStatus = capture.RecordingFinalizing
			} else {
				s.current.Status = capture.SessionInterrupted
				s.current.RecordingStatus = capture.RecordingIncomplete
			}
			current := s.current
			s.mu.Unlock()
			return current, ctx.Err()
		}
	}
	s.owner.mu.Lock()
	nonterminal := s.owner.finalizeNonterminal
	if !nonterminal && s.owner.finalizeFailures > 0 {
		s.owner.finalizeFailures--
		nonterminal = true
	}
	terminalErr := s.owner.finalizeTerminalErr
	s.owner.mu.Unlock()
	if nonterminal {
		s.mu.Lock()
		s.current.OperationID = operationID
		s.current.Status = capture.SessionFinalizing
		s.current.RecordingStatus = capture.RecordingFinalizing
		current := s.current
		s.mu.Unlock()
		return current, errors.New("injected terminal CAS failure")
	}
	s.mu.Lock()
	s.current.OperationID = operationID
	s.current.Status = capture.SessionCompleted
	if s.current.RecordingStatus == capture.RecordingActive {
		s.current.RecordingStatus = capture.RecordingCompleted
	}
	current := s.current
	s.mu.Unlock()
	return current, terminalErr
}

func TestMonitorManagerWaitsGoesLiveAndStopsPersistently(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := openTestStore(t)
	defer store.Close()
	service, err := NewService(store.Writer(), store.Reader(), newMemoryCredentialStore())
	if err != nil {
		t.Fatal(err)
	}
	config, err := service.CreateRoom(ctx, CreateRoomInput{LiveID: "monitor-flow", Alias: "监控流程"})
	if err != nil {
		t.Fatal(err)
	}
	offline := newFakeLiveClient()
	offline.offline = true
	offline.name = "脱敏主播"
	online := newFakeLiveClient()
	online.name = "脱敏主播"
	online.title = "测试直播"
	factory := &scriptedLiveFactory{clients: []LiveClient{offline, online}}
	events := make(chan RoomRuntimeStatus, 32)
	manager, err := NewMonitorManager(ctx, service, nil, MonitorOptions{
		Coordinator:  newFakeCaptureCoordinator(),
		PollInterval: 5 * time.Millisecond, ReconnectDelay: 5 * time.Millisecond,
		Factory: factory.create, Publisher: func(status RoomRuntimeStatus) { events <- status },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.StartMonitoring(context.Background(), config.ID); err != nil {
		t.Fatalf("StartMonitoring() error = %v", err)
	}
	waitForRuntimeState(t, events, RuntimeWaiting, "ROOM_OFFLINE")
	live := waitForRuntimeState(t, events, RuntimeLive, "")
	if live.LiveName != "脱敏主播" || live.Title != "测试直播" || live.OperationID == "" {
		t.Fatalf("unexpected live status: %#v", live)
	}
	select {
	case <-online.started:
	case <-time.After(time.Second):
		t.Fatal("live client Start() was not called")
	}
	serialized, err := json.Marshal(live)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(serialized)), "cookie") {
		t.Fatalf("status event exposes credential fields: %s", serialized)
	}
	if err := manager.StopMonitoring(context.Background(), config.ID); err != nil {
		t.Fatalf("StopMonitoring() error = %v", err)
	}
	stopped := waitForRuntimeState(t, events, RuntimeStopped, "")
	if stopped.Message != "已停止监控" {
		t.Fatalf("unexpected stopped status: %#v", stopped)
	}
	persisted, err := service.GetRoom(context.Background(), config.ID)
	if err != nil || persisted.MonitorEnabled {
		t.Fatalf("monitor preference after stop = (%t, %v)", persisted.MonitorEnabled, err)
	}
}

func TestMonitorManagerRoomNotFoundWaitsForExplicitRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := openTestStore(t)
	defer store.Close()
	service, err := NewService(store.Writer(), store.Reader(), newMemoryCredentialStore())
	if err != nil {
		t.Fatal(err)
	}
	config, err := service.CreateRoom(ctx, CreateRoomInput{LiveID: "missing-flow"})
	if err != nil {
		t.Fatal(err)
	}
	missing := newFakeLiveClient()
	missing.prepareErr = douyinLive.ErrRoomNotFound
	offline := newFakeLiveClient()
	offline.offline = true
	factory := &scriptedLiveFactory{clients: []LiveClient{missing, offline}}
	events := make(chan RoomRuntimeStatus, 32)
	manager, err := NewMonitorManager(ctx, service, nil, MonitorOptions{
		Coordinator:  newFakeCaptureCoordinator(),
		PollInterval: 5 * time.Millisecond, Factory: factory.create,
		Publisher: func(status RoomRuntimeStatus) { events <- status },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.StartMonitoring(context.Background(), config.ID); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeState(t, events, RuntimeError, "ROOM_NOT_FOUND")
	time.Sleep(25 * time.Millisecond)
	if got := factory.callCount(); got != 1 {
		t.Fatalf("factory calls during non-retryable error = %d, want 1", got)
	}
	if err := manager.StartMonitoring(context.Background(), config.ID); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeState(t, events, RuntimeWaiting, "ROOM_OFFLINE")
	if got := factory.callCount(); got != 2 {
		t.Fatalf("factory calls after explicit retry = %d, want 2", got)
	}
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMonitorManagerEnforcesConfiguredLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := openTestStore(t)
	defer store.Close()
	service, err := NewService(store.Writer(), store.Reader(), newMemoryCredentialStore())
	if err != nil {
		t.Fatal(err)
	}
	first, _ := service.CreateRoom(ctx, CreateRoomInput{LiveID: "limit-one"})
	second, _ := service.CreateRoom(ctx, CreateRoomInput{LiveID: "limit-two"})
	client := newFakeLiveClient()
	client.offline = true
	manager, err := NewMonitorManager(ctx, service, nil, MonitorOptions{
		Coordinator: newFakeCaptureCoordinator(),
		MaxRooms:    1, PollInterval: time.Hour,
		Factory: func(context.Context, RoomConfig, string) (LiveClient, error) { return client, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.StartMonitoring(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := manager.StartMonitoring(ctx, second.ID); ErrorCode(err) != "MONITOR_LIMIT_REACHED" {
		t.Fatalf("second StartMonitoring() error = %v", err)
	}
	secondConfig, err := service.GetRoom(ctx, second.ID)
	if err != nil || secondConfig.MonitorEnabled {
		t.Fatalf("limit rollback preference = (%t, %v)", secondConfig.MonitorEnabled, err)
	}
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMonitorManagerResumesPersistedEnabledRoomsAfterRestart(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	defer store.Close()
	service, err := NewService(store.Writer(), store.Reader(), newMemoryCredentialStore())
	if err != nil {
		t.Fatal(err)
	}
	config, err := service.CreateRoom(ctx, CreateRoomInput{LiveID: "restart-monitor", MonitorEnabled: true})
	if err != nil {
		t.Fatal(err)
	}

	for attempt := 0; attempt < 2; attempt++ {
		root, cancel := context.WithCancel(context.Background())
		offline := newFakeLiveClient()
		offline.offline = true
		factory := &scriptedLiveFactory{clients: []LiveClient{offline}}
		events := make(chan RoomRuntimeStatus, 16)
		manager, err := NewMonitorManager(root, service, nil, MonitorOptions{
			Coordinator:  newFakeCaptureCoordinator(),
			PollInterval: time.Hour, Factory: factory.create,
			Publisher: func(status RoomRuntimeStatus) { events <- status },
		})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if err := manager.StartEnabled(ctx); err != nil {
			cancel()
			t.Fatal(err)
		}
		waitForRuntimeState(t, events, RuntimeWaiting, "ROOM_OFFLINE")
		if got := factory.callCount(); got != 1 {
			t.Fatalf("attempt %d factory calls = %d, want 1", attempt, got)
		}
		if err := manager.Shutdown(context.Background()); err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
	}

	persisted, err := service.GetRoom(ctx, config.ID)
	if err != nil || !persisted.MonitorEnabled {
		t.Fatalf("persisted monitor preference = (%t, %v), want true", persisted.MonitorEnabled, err)
	}
}

func TestMonitorManagerContainsPublisherPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := openTestStore(t)
	defer store.Close()
	service, _ := NewService(store.Writer(), store.Reader(), newMemoryCredentialStore())
	config, _ := service.CreateRoom(ctx, CreateRoomInput{LiveID: "publisher-panic"})
	manager, err := NewMonitorManager(ctx, service, nil, MonitorOptions{
		Coordinator: newFakeCaptureCoordinator(), Publisher: func(RoomRuntimeStatus) { panic("test") }})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.StartMonitoring(ctx, config.ID); err != nil {
		t.Fatalf("publisher panic escaped StartMonitoring(): %v", err)
	}
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMonitorManagerRebindsSameSessionAcrossOuterReconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := openTestStore(t)
	defer store.Close()
	service, _ := NewService(store.Writer(), store.Reader(), newMemoryCredentialStore())
	config, _ := service.CreateRoom(ctx, CreateRoomInput{LiveID: "same-session", RecordEnabled: true})
	first := newFakeLiveClient()
	second := newFakeLiveClient()
	factory := &scriptedLiveFactory{clients: []LiveClient{first, second}}
	coordinator := newFakeCaptureCoordinator()
	events := make(chan RoomRuntimeStatus, 64)
	manager, err := NewMonitorManager(ctx, service, nil, MonitorOptions{
		Coordinator: coordinator, PollInterval: 5 * time.Millisecond, ReconnectDelay: 5 * time.Millisecond,
		Factory: factory.create, Publisher: func(status RoomRuntimeStatus) { events <- status },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.StartMonitoring(ctx, config.ID); err != nil {
		t.Fatal(err)
	}
	firstRecording := waitForRuntimeState(t, events, RuntimeRecording, "")
	first.Close()
	waitForRuntimeState(t, events, RuntimeReconnecting, "ROOM_CONNECTION_INTERRUPTED")
	secondRecording := waitForRuntimeState(t, events, RuntimeRecording, "")
	if firstRecording.SessionID == "" || secondRecording.SessionID != firstRecording.SessionID {
		t.Fatalf("session changed across reconnect: first=%#v second=%#v", firstRecording, secondRecording)
	}
	opens, rebinds, _ := coordinator.counts()
	if opens != 1 || rebinds != 1 {
		t.Fatalf("capture calls = open:%d rebind:%d, want 1/1", opens, rebinds)
	}
	if err := manager.StopMonitoring(context.Background(), config.ID); err != nil {
		t.Fatal(err)
	}
}

func TestMonitorManagerRequiresTwoReliableOfflineConfirmations(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := openTestStore(t)
	defer store.Close()
	service, _ := NewService(store.Writer(), store.Reader(), newMemoryCredentialStore())
	config, _ := service.CreateRoom(ctx, CreateRoomInput{LiveID: "offline-confirm", RecordEnabled: true})
	online := newFakeLiveClient()
	firstOffline := newFakeLiveClient()
	firstOffline.offline = true
	secondOffline := newFakeLiveClient()
	secondOffline.offline = true
	factory := &scriptedLiveFactory{clients: []LiveClient{online, firstOffline, secondOffline}}
	coordinator := newFakeCaptureCoordinator()
	events := make(chan RoomRuntimeStatus, 64)
	manager, err := NewMonitorManager(ctx, service, nil, MonitorOptions{
		Coordinator: coordinator, PollInterval: 5 * time.Millisecond, ReconnectDelay: 5 * time.Millisecond,
		Factory: factory.create, Publisher: func(status RoomRuntimeStatus) { events <- status },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.StartMonitoring(ctx, config.ID); err != nil {
		t.Fatal(err)
	}
	recording := waitForRuntimeState(t, events, RuntimeRecording, "")
	online.Close()
	waitForRuntimeState(t, events, RuntimeReconnecting, "ROOM_CONNECTION_INTERRUPTED")
	firstConfirmation := waitForRuntimeState(t, events, RuntimeReconnecting, "ROOM_OFFLINE_CONFIRMING")
	if firstConfirmation.SessionID != recording.SessionID {
		t.Fatalf("first offline confirmation changed session: %#v", firstConfirmation)
	}
	_, _, finalizes := coordinator.counts()
	if finalizes != 0 {
		t.Fatalf("finalized after one reliable offline confirmation: %d", finalizes)
	}
	finalizing := waitForRuntimeState(t, events, RuntimeFinalizing, "")
	if finalizing.SessionID != recording.SessionID {
		t.Fatalf("finalizing session = %#v, want %s", finalizing, recording.SessionID)
	}
	if finalizing.RecordingStatus != capture.RecordingFinalizing {
		t.Fatalf("finalizing recording status = %q, want %q", finalizing.RecordingStatus, capture.RecordingFinalizing)
	}
	waiting := waitForRuntimeState(t, events, RuntimeWaiting, "ROOM_OFFLINE")
	if waiting.OperationID != finalizing.OperationID {
		t.Fatalf("waiting operation ID = %q, want finalize operation ID %q", waiting.OperationID, finalizing.OperationID)
	}
	_, _, finalizes = coordinator.counts()
	if finalizes != 1 {
		t.Fatalf("finalize calls after second confirmation = %d, want 1", finalizes)
	}
	if err := manager.StopMonitoring(context.Background(), config.ID); err != nil {
		t.Fatal(err)
	}
}

func TestMonitorManagerKeepsFinalizingWorkerExclusive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := openTestStore(t)
	defer store.Close()
	service, _ := NewService(store.Writer(), store.Reader(), newMemoryCredentialStore())
	config, _ := service.CreateRoom(ctx, CreateRoomInput{LiveID: "finalizing-exclusive", RecordEnabled: true})
	online := newFakeLiveClient()
	coordinator := newFakeCaptureCoordinator()
	coordinator.finalizeStarted = make(chan struct{})
	coordinator.finalizeRelease = make(chan struct{})
	events := make(chan RoomRuntimeStatus, 64)
	manager, err := NewMonitorManager(ctx, service, nil, MonitorOptions{
		Coordinator: coordinator, FinalizeTimeout: time.Second,
		Factory:   func(context.Context, RoomConfig, string) (LiveClient, error) { return online, nil },
		Publisher: func(status RoomRuntimeStatus) { events <- status },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.StartMonitoring(ctx, config.ID); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeState(t, events, RuntimeRecording, "")
	stopResult := make(chan error, 1)
	go func() { stopResult <- manager.StopMonitoring(context.Background(), config.ID) }()
	finalizing := waitForRuntimeState(t, events, RuntimeFinalizing, "")
	if finalizing.RecordingStatus != capture.RecordingFinalizing {
		t.Fatalf("finalizing recording status = %q", finalizing.RecordingStatus)
	}
	select {
	case <-coordinator.finalizeStarted:
	case <-time.After(time.Second):
		t.Fatal("Finalize() did not start")
	}
	if err := manager.StartMonitoring(context.Background(), config.ID); ErrorCode(err) != "CAPTURE_FINALIZING" {
		t.Fatalf("StartMonitoring() during finalizing error = %v", err)
	}
	close(coordinator.finalizeRelease)
	select {
	case err := <-stopResult:
		if err != nil {
			t.Fatalf("StopMonitoring() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("StopMonitoring() did not finish")
	}
}
func TestMonitorManagerRejectsRestartWhileSessionlessWorkerIsStopping(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := openTestStore(t)
	defer store.Close()
	service, _ := NewService(store.Writer(), store.Reader(), newMemoryCredentialStore())
	config, _ := service.CreateRoom(ctx, CreateRoomInput{LiveID: "sessionless-stop"})
	factoryStarted := make(chan struct{})
	factoryRelease := make(chan struct{})
	manager, err := NewMonitorManager(ctx, service, nil, MonitorOptions{
		Coordinator: newFakeCaptureCoordinator(),
		Factory: func(context.Context, RoomConfig, string) (LiveClient, error) {
			close(factoryStarted)
			<-factoryRelease
			client := newFakeLiveClient()
			client.offline = true
			return client, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.StartMonitoring(ctx, config.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-factoryStarted:
	case <-time.After(time.Second):
		t.Fatal("factory did not start")
	}
	stopResult := make(chan error, 1)
	go func() { stopResult <- manager.StopMonitoring(context.Background(), config.ID) }()
	deadline := time.Now().Add(time.Second)
	for {
		manager.mu.RLock()
		worker := manager.workers[config.ID]
		manager.mu.RUnlock()
		if worker != nil && worker.isStopping() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not enter stopping state")
		}
		time.Sleep(time.Millisecond)
	}
	if err := manager.StartMonitoring(context.Background(), config.ID); ErrorCode(err) != "CAPTURE_FINALIZING" {
		t.Fatalf("StartMonitoring() during sessionless stop error = %v", err)
	}
	close(factoryRelease)
	select {
	case err := <-stopResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("sessionless stop did not finish")
	}
}

func TestMonitorManagerFactoryFailureKeepsActiveSessionReconnecting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := openTestStore(t)
	defer store.Close()
	service, _ := NewService(store.Writer(), store.Reader(), newMemoryCredentialStore())
	config, _ := service.CreateRoom(ctx, CreateRoomInput{LiveID: "factory-reconnect", RecordEnabled: true})
	first, second := newFakeLiveClient(), newFakeLiveClient()
	var mu sync.Mutex
	calls := 0
	factory := func(context.Context, RoomConfig, string) (LiveClient, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		switch calls {
		case 1:
			return first, nil
		case 2:
			return nil, errors.New("injected factory failure")
		default:
			return second, nil
		}
	}
	coordinator := newFakeCaptureCoordinator()
	events := make(chan RoomRuntimeStatus, 64)
	manager, err := NewMonitorManager(ctx, service, nil, MonitorOptions{
		Coordinator: coordinator, ReconnectDelay: 5 * time.Millisecond,
		Factory: factory, Publisher: func(status RoomRuntimeStatus) { events <- status },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.StartMonitoring(ctx, config.ID); err != nil {
		t.Fatal(err)
	}
	recording := waitForRuntimeState(t, events, RuntimeRecording, "")
	first.Close()
	waitForRuntimeState(t, events, RuntimeReconnecting, "ROOM_CONNECTION_INTERRUPTED")
	failed := waitForRuntimeState(t, events, RuntimeReconnecting, "ROOM_CHECK_FAILED")
	if failed.SessionID != recording.SessionID {
		t.Fatalf("factory failure lost active session: %#v", failed)
	}
	rebound := waitForRuntimeState(t, events, RuntimeRecording, "")
	if rebound.SessionID != recording.SessionID {
		t.Fatalf("factory recovery changed session: %#v", rebound)
	}
	if err := manager.StopMonitoring(context.Background(), config.ID); err != nil {
		t.Fatal(err)
	}
}

func TestMonitorManagerDoesNotEnterWaitingWhenFinalizeIsNonterminal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := openTestStore(t)
	defer store.Close()
	service, _ := NewService(store.Writer(), store.Reader(), newMemoryCredentialStore())
	config, _ := service.CreateRoom(ctx, CreateRoomInput{LiveID: "finalize-cas", RecordEnabled: true})
	online := newFakeLiveClient()
	firstOffline := newFakeLiveClient()
	firstOffline.offline = true
	secondOffline := newFakeLiveClient()
	secondOffline.offline = true
	factory := &scriptedLiveFactory{clients: []LiveClient{online, firstOffline, secondOffline}}
	coordinator := newFakeCaptureCoordinator()
	coordinator.finalizeNonterminal = true
	events := make(chan RoomRuntimeStatus, 64)
	manager, err := NewMonitorManager(ctx, service, nil, MonitorOptions{
		Coordinator: coordinator, PollInterval: 5 * time.Millisecond, ReconnectDelay: 5 * time.Millisecond,
		Factory: factory.create, Publisher: func(status RoomRuntimeStatus) { events <- status },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.StartMonitoring(ctx, config.ID); err != nil {
		t.Fatal(err)
	}
	recording := waitForRuntimeState(t, events, RuntimeRecording, "")
	online.Close()
	waitForRuntimeState(t, events, RuntimeReconnecting, "ROOM_CONNECTION_INTERRUPTED")
	waitForRuntimeState(t, events, RuntimeReconnecting, "ROOM_OFFLINE_CONFIRMING")
	failed := waitForRuntimeState(t, events, RuntimeFinalizing, "CAPTURE_FINALIZE_FAILED")
	if failed.SessionID != recording.SessionID || failed.RecordingStatus != capture.RecordingFinalizing {
		t.Fatalf("nonterminal finalization status = %#v", failed)
	}
	status, err := manager.GetRoomStatus(context.Background(), config.ID)
	if err != nil || status.State != RuntimeFinalizing {
		t.Fatalf("GetRoomStatus() after terminal CAS failure = (%#v, %v)", status, err)
	}
	if err := manager.StartMonitoring(context.Background(), config.ID); ErrorCode(err) != "CAPTURE_FINALIZING" {
		t.Fatalf("restart after terminal CAS failure error = %v", err)
	}
	if err := manager.StopMonitoring(context.Background(), config.ID); err == nil {
		t.Fatal("StopMonitoring() error = nil, want retained finalization failure")
	}
	manager.mu.RLock()
	placeholder := manager.workers[config.ID]
	manager.mu.RUnlock()
	if placeholder == nil || !placeholder.doneClosed() || placeholder.sessionValue() == nil {
		t.Fatalf("nonterminal worker was not retained as a completed placeholder: %#v", placeholder)
	}
	status, err = manager.GetRoomStatus(context.Background(), config.ID)
	if err != nil || status.State != RuntimeFinalizing || status.ErrorCode != "CAPTURE_FINALIZE_FAILED" {
		t.Fatalf("retained placeholder status = (%#v, %v)", status, err)
	}
	if err := manager.StartMonitoring(context.Background(), config.ID); ErrorCode(err) != "CAPTURE_FINALIZING" {
		t.Fatalf("restart with retained placeholder error = %v", err)
	}

	coordinator.mu.Lock()
	coordinator.finalizeNonterminal = false
	coordinator.mu.Unlock()
	if err := manager.StopMonitoring(context.Background(), config.ID); err != nil {
		t.Fatalf("retry StopMonitoring() error = %v", err)
	}
	manager.mu.RLock()
	retained := manager.workers[config.ID]
	manager.mu.RUnlock()
	if retained != nil {
		t.Fatalf("terminal retry retained worker: %#v", retained.snapshot())
	}
	status, err = manager.GetRoomStatus(context.Background(), config.ID)
	if err != nil || status.State != RuntimeStopped {
		t.Fatalf("status after terminal retry = (%#v, %v)", status, err)
	}
	if err := manager.StartMonitoring(context.Background(), config.ID); err != nil {
		t.Fatalf("restart after terminal retry error = %v", err)
	}
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatalf("cleanup Shutdown() error = %v", err)
	}
}

func TestMonitorManagerShutdownRejectsStartsAndWaitsIdempotently(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := openTestStore(t)
	defer store.Close()
	service, _ := NewService(store.Writer(), store.Reader(), newMemoryCredentialStore())
	active, _ := service.CreateRoom(ctx, CreateRoomInput{LiveID: "shutdown-active"})
	inactive, _ := service.CreateRoom(ctx, CreateRoomInput{LiveID: "shutdown-rejected"})
	factoryStarted := make(chan struct{})
	factoryRelease := make(chan struct{})
	manager, err := NewMonitorManager(ctx, service, nil, MonitorOptions{
		Coordinator: newFakeCaptureCoordinator(),
		Factory: func(context.Context, RoomConfig, string) (LiveClient, error) {
			close(factoryStarted)
			<-factoryRelease
			client := newFakeLiveClient()
			client.offline = true
			return client, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.StartMonitoring(ctx, active.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-factoryStarted:
	case <-time.After(time.Second):
		t.Fatal("factory did not start")
	}
	firstResult := make(chan error, 1)
	go func() { firstResult <- manager.Shutdown(context.Background()) }()
	deadline := time.Now().Add(time.Second)
	for {
		manager.mu.RLock()
		shuttingDown := manager.shuttingDown
		manager.mu.RUnlock()
		if shuttingDown {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("manager did not mark shutting down")
		}
		time.Sleep(time.Millisecond)
	}
	if err := manager.StartMonitoring(context.Background(), inactive.ID); ErrorCode(err) != "MONITOR_MANAGER_SHUTTING_DOWN" {
		t.Fatalf("StartMonitoring() during shutdown error = %v", err)
	}
	persisted, err := service.GetRoom(context.Background(), inactive.ID)
	if err != nil || persisted.MonitorEnabled {
		t.Fatalf("rejected start changed persistence = (%t, %v)", persisted.MonitorEnabled, err)
	}
	secondResult := make(chan error, 1)
	go func() { secondResult <- manager.Shutdown(context.Background()) }()
	select {
	case err := <-secondResult:
		t.Fatalf("concurrent Shutdown() returned before first completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(factoryRelease)
	for index, result := range []<-chan error{firstResult, secondResult} {
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("Shutdown result %d = %v", index, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("Shutdown result %d timed out", index)
		}
	}
	manager.mu.RLock()
	closed := manager.closed
	manager.mu.RUnlock()
	if !closed {
		t.Fatal("manager did not mark closed")
	}
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatalf("repeated Shutdown() error = %v", err)
	}
	if err := manager.StartMonitoring(context.Background(), inactive.ID); ErrorCode(err) != "MONITOR_MANAGER_SHUTTING_DOWN" {
		t.Fatalf("StartMonitoring() after shutdown error = %v", err)
	}
}

func TestMonitorManagerSerializesConcurrentStartAndStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := openTestStore(t)
	defer store.Close()
	service, _ := NewService(store.Writer(), store.Reader(), newMemoryCredentialStore())
	config, _ := service.CreateRoom(ctx, CreateRoomInput{LiveID: "start-stop-race"})
	manager, err := NewMonitorManager(ctx, service, nil, MonitorOptions{
		Coordinator: newFakeCaptureCoordinator(), PollInterval: time.Hour,
		Factory: func(context.Context, RoomConfig, string) (LiveClient, error) {
			client := newFakeLiveClient()
			client.offline = true
			return client, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	operation := manager.roomOperation(config.ID)
	operation.Lock()
	startBarrier := make(chan struct{})
	ready := make(chan struct{}, 2)
	type operationResult struct {
		name string
		err  error
	}
	results := make(chan operationResult, 2)
	go func() {
		ready <- struct{}{}
		<-startBarrier
		results <- operationResult{name: "start", err: manager.StartMonitoring(context.Background(), config.ID)}
	}()
	go func() {
		ready <- struct{}{}
		<-startBarrier
		results <- operationResult{name: "stop", err: manager.StopMonitoring(context.Background(), config.ID)}
	}()
	<-ready
	<-ready
	close(startBarrier)
	operation.Unlock()
	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent operations = (%s:%v, %s:%v)", first.name, first.err, second.name, second.err)
	}
	persisted, err := service.GetRoom(context.Background(), config.ID)
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.RLock()
	worker := manager.workers[config.ID]
	manager.mu.RUnlock()
	if second.name == "start" {
		if !persisted.MonitorEnabled || worker == nil {
			t.Fatalf("last start left inconsistent state: enabled=%t worker=%v", persisted.MonitorEnabled, worker != nil)
		}
	} else if persisted.MonitorEnabled || worker != nil {
		t.Fatalf("last stop left inconsistent state: enabled=%t worker=%v", persisted.MonitorEnabled, worker != nil)
	}
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMonitorManagerShutdownWaitsForInflightSetMonitorEnabled(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	defer store.Close()
	service, _ := NewService(store.Writer(), store.Reader(), newMemoryCredentialStore())
	config, _ := service.CreateRoom(ctx, CreateRoomInput{LiveID: "shutdown-inflight-set"})
	manager, err := NewMonitorManager(ctx, service, nil, MonitorOptions{
		Coordinator: newFakeCaptureCoordinator(),
		Factory: func(context.Context, RoomConfig, string) (LiveClient, error) {
			client := newFakeLiveClient()
			client.offline = true
			return client, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var barrierOnce sync.Once
	manager.beforeSetMonitorEnabled = func(id string, enabled bool) {
		if id == config.ID && enabled {
			barrierOnce.Do(func() {
				close(entered)
				<-release
			})
		}
	}

	startResult := make(chan error, 1)
	go func() { startResult <- manager.StartMonitoring(context.Background(), config.ID) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("StartMonitoring() did not enter SetMonitorEnabled barrier")
	}

	shutdownResult := make(chan error, 1)
	go func() { shutdownResult <- manager.Shutdown(context.Background()) }()
	deadline := time.Now().Add(time.Second)
	for {
		manager.mu.RLock()
		shuttingDown := manager.shuttingDown
		manager.mu.RUnlock()
		if shuttingDown {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("manager did not mark shutting down")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-shutdownResult:
		t.Fatalf("Shutdown() returned before in-flight persistence drained: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-startResult:
		if ErrorCode(err) != "MONITOR_MANAGER_SHUTTING_DOWN" {
			t.Fatalf("StartMonitoring() after shutdown gate error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("StartMonitoring() did not finish after releasing barrier")
	}
	select {
	case err := <-shutdownResult:
		if err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown() did not finish after in-flight operation drained")
	}
	persisted, err := service.GetRoom(context.Background(), config.ID)
	if err != nil || persisted.MonitorEnabled {
		t.Fatalf("shutdown race left monitor enabled = (%t, %v)", persisted.MonitorEnabled, err)
	}
	manager.mu.RLock()
	worker := manager.workers[config.ID]
	inflight := manager.inflightRoomOperations
	manager.mu.RUnlock()
	if worker != nil || inflight != 0 {
		t.Fatalf("shutdown left worker/in-flight operation: worker=%v inflight=%d", worker != nil, inflight)
	}
}

func TestMonitorManagerTerminalFinalizeErrorDoesNotPolluteLaterStop(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	defer store.Close()
	service, _ := NewService(store.Writer(), store.Reader(), newMemoryCredentialStore())
	config, _ := service.CreateRoom(ctx, CreateRoomInput{LiveID: "terminal-finalize-error", RecordEnabled: true})
	online := newFakeLiveClient()
	firstOffline := newFakeLiveClient()
	firstOffline.offline = true
	secondOffline := newFakeLiveClient()
	secondOffline.offline = true
	coordinator := newFakeCaptureCoordinator()
	coordinator.finalizeTerminalErr = errors.New("injected terminal finalize warning")
	events := make(chan RoomRuntimeStatus, 64)
	manager, err := NewMonitorManager(ctx, service, nil, MonitorOptions{
		Coordinator: coordinator, PollInterval: 5 * time.Millisecond, ReconnectDelay: 5 * time.Millisecond,
		Factory:   (&scriptedLiveFactory{clients: []LiveClient{online, firstOffline, secondOffline}}).create,
		Publisher: func(status RoomRuntimeStatus) { events <- status },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.StartMonitoring(ctx, config.ID); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeState(t, events, RuntimeRecording, "")
	online.Close()
	waitForRuntimeState(t, events, RuntimeReconnecting, "ROOM_CONNECTION_INTERRUPTED")
	waitForRuntimeState(t, events, RuntimeReconnecting, "ROOM_OFFLINE_CONFIRMING")
	waitForRuntimeState(t, events, RuntimeWaiting, "ROOM_OFFLINE")
	if err := manager.StopMonitoring(context.Background(), config.ID); err != nil {
		t.Fatalf("StopMonitoring() inherited terminal finalize error: %v", err)
	}
	_, _, finalizes := coordinator.counts()
	if finalizes != 1 {
		t.Fatalf("Finalize() calls = %d, want 1", finalizes)
	}
}

func TestMonitorManagerAutomaticallyRetriesNonterminalFinalize(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	defer store.Close()
	service, _ := NewService(store.Writer(), store.Reader(), newMemoryCredentialStore())
	config, _ := service.CreateRoom(ctx, CreateRoomInput{LiveID: "finalize-auto-retry", RecordEnabled: true})
	online := newFakeLiveClient()
	firstOffline := newFakeLiveClient()
	firstOffline.offline = true
	secondOffline := newFakeLiveClient()
	secondOffline.offline = true
	coordinator := newFakeCaptureCoordinator()
	coordinator.finalizeFailures = 1
	events := make(chan RoomRuntimeStatus, 64)
	manager, err := NewMonitorManager(ctx, service, nil, MonitorOptions{
		Coordinator: coordinator, PollInterval: 5 * time.Millisecond, ReconnectDelay: 5 * time.Millisecond,
		Factory:   (&scriptedLiveFactory{clients: []LiveClient{online, firstOffline, secondOffline}}).create,
		Publisher: func(status RoomRuntimeStatus) { events <- status },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.StartMonitoring(ctx, config.ID); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeState(t, events, RuntimeRecording, "")
	online.Close()
	waitForRuntimeState(t, events, RuntimeReconnecting, "ROOM_CONNECTION_INTERRUPTED")
	waitForRuntimeState(t, events, RuntimeReconnecting, "ROOM_OFFLINE_CONFIRMING")
	retrying := waitForRuntimeState(t, events, RuntimeFinalizing, "CAPTURE_FINALIZE_FAILED")
	if retrying.Message != "场次收尾失败，将自动重试" || retrying.RetryAt == 0 {
		t.Fatalf("automatic retry status = %#v", retrying)
	}
	waitForRuntimeState(t, events, RuntimeWaiting, "ROOM_OFFLINE")
	_, _, finalizes := coordinator.counts()
	if finalizes != 2 {
		t.Fatalf("Finalize() calls = %d, want 2", finalizes)
	}
	persisted, err := service.GetRoom(context.Background(), config.ID)
	if err != nil || !persisted.MonitorEnabled {
		t.Fatalf("automatic retry changed enabled preference = (%t, %v)", persisted.MonitorEnabled, err)
	}
	if err := manager.StopMonitoring(context.Background(), config.ID); err != nil {
		t.Fatal(err)
	}
}

func TestMonitorManagerShutdownTimeoutContinuesSharedCleanup(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	defer store.Close()
	service, _ := NewService(store.Writer(), store.Reader(), newMemoryCredentialStore())
	activeConfig, _ := service.CreateRoom(ctx, CreateRoomInput{LiveID: "shutdown-timeout-active"})
	blockedConfig, _ := service.CreateRoom(ctx, CreateRoomInput{LiveID: "shutdown-timeout-blocked"})
	active := newFakeLiveClient()
	manager, err := NewMonitorManager(ctx, service, nil, MonitorOptions{
		Coordinator: newFakeCaptureCoordinator(),
		Factory: func(_ context.Context, config RoomConfig, _ string) (LiveClient, error) {
			if config.ID == activeConfig.ID {
				return active, nil
			}
			client := newFakeLiveClient()
			client.offline = true
			return client, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.StartMonitoring(ctx, activeConfig.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-active.started:
	case <-time.After(time.Second):
		t.Fatal("active worker did not start")
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var barrierOnce sync.Once
	manager.beforeSetMonitorEnabled = func(id string, enabled bool) {
		if id == blockedConfig.ID && enabled {
			barrierOnce.Do(func() {
				close(entered)
				<-release
			})
		}
	}
	startResult := make(chan error, 1)
	go func() { startResult <- manager.StartMonitoring(context.Background(), blockedConfig.ID) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("blocked start did not enter persistence barrier")
	}

	firstCtx, firstCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer firstCancel()
	if err := manager.Shutdown(firstCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Shutdown() error = %v, want deadline exceeded", err)
	}
	if manager.ShutdownComplete() {
		t.Fatal("manager reported complete while an operation was still in flight")
	}
	select {
	case <-active.closed:
		t.Fatal("timed-out shutdown stopped worker before operation drain")
	default:
	}
	if _, err := service.GetRoom(context.Background(), activeConfig.ID); err != nil {
		t.Fatalf("database became unavailable after incomplete shutdown: %v", err)
	}

	close(release)
	select {
	case err := <-startResult:
		if ErrorCode(err) != "MONITOR_MANAGER_SHUTTING_DOWN" {
			t.Fatalf("blocked StartMonitoring() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked StartMonitoring() did not finish")
	}
	secondCtx, secondCancel := context.WithTimeout(context.Background(), time.Second)
	defer secondCancel()
	if err := manager.Shutdown(secondCtx); err != nil {
		t.Fatalf("second Shutdown() error = %v", err)
	}
	if !manager.ShutdownComplete() {
		t.Fatal("manager did not report completed shared shutdown")
	}
	select {
	case <-active.closed:
	default:
		t.Fatal("shared cleanup did not stop the active worker")
	}
	persisted, err := service.GetRoom(context.Background(), blockedConfig.ID)
	if err != nil || persisted.MonitorEnabled {
		t.Fatalf("shutdown continuation left blocked room enabled = (%t, %v)", persisted.MonitorEnabled, err)
	}
}

func TestMonitorManagerTerminalRetryClearsHistoricalFinalizeErrors(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		shutdown bool
	}{
		{name: "stop"},
		{name: "shutdown", shutdown: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			store := openTestStore(t)
			defer store.Close()
			service, _ := NewService(store.Writer(), store.Reader(), newMemoryCredentialStore())
			config, _ := service.CreateRoom(ctx, CreateRoomInput{LiveID: "clear-finalize-errors-" + testCase.name, RecordEnabled: true})
			online := newFakeLiveClient()
			firstOffline := newFakeLiveClient()
			firstOffline.offline = true
			secondOffline := newFakeLiveClient()
			secondOffline.offline = true
			coordinator := newFakeCaptureCoordinator()
			coordinator.finalizeFailures = maxAutomaticFinalizeAttempts
			events := make(chan RoomRuntimeStatus, 64)
			manager, err := NewMonitorManager(ctx, service, nil, MonitorOptions{
				Coordinator: coordinator, PollInterval: 5 * time.Millisecond, ReconnectDelay: 5 * time.Millisecond,
				Factory:   (&scriptedLiveFactory{clients: []LiveClient{online, firstOffline, secondOffline}}).create,
				Publisher: func(status RoomRuntimeStatus) { events <- status },
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.StartMonitoring(ctx, config.ID); err != nil {
				t.Fatal(err)
			}
			waitForRuntimeState(t, events, RuntimeRecording, "")
			online.Close()
			waitForRuntimeState(t, events, RuntimeReconnecting, "ROOM_CONNECTION_INTERRUPTED")
			waitForRuntimeState(t, events, RuntimeReconnecting, "ROOM_OFFLINE_CONFIRMING")
			waitForRuntimeStatus(t, events, func(status RoomRuntimeStatus) bool {
				return status.State == RuntimeFinalizing && status.ErrorCode == "CAPTURE_FINALIZE_FAILED" &&
					status.Message == "场次收尾失败，需要恢复" && status.RetryAt == 0
			})
			if testCase.shutdown {
				err = manager.Shutdown(context.Background())
			} else {
				err = manager.StopMonitoring(context.Background(), config.ID)
			}
			if err != nil {
				t.Fatalf("terminal fourth Finalize() returned historical error: %v", err)
			}
			_, _, finalizes := coordinator.counts()
			if finalizes != maxAutomaticFinalizeAttempts+1 {
				t.Fatalf("Finalize() calls = %d, want %d", finalizes, maxAutomaticFinalizeAttempts+1)
			}
		})
	}
}

func TestMonitorManagerRetriesFinalizationAfterWorkerBecomesDone(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		shutdown bool
	}{
		{name: "detach"},
		{name: "shutdown", shutdown: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			store := openTestStore(t)
			defer store.Close()
			service, _ := NewService(store.Writer(), store.Reader(), newMemoryCredentialStore())
			config, _ := service.CreateRoom(ctx, CreateRoomInput{LiveID: "done-retry-" + testCase.name, RecordEnabled: true})
			online := newFakeLiveClient()
			coordinator := newFakeCaptureCoordinator()
			coordinator.finalizeFailures = 1
			events := make(chan RoomRuntimeStatus, 16)
			manager, err := NewMonitorManager(ctx, service, nil, MonitorOptions{
				Coordinator: coordinator,
				Factory:     func(context.Context, RoomConfig, string) (LiveClient, error) { return online, nil },
				Publisher:   func(status RoomRuntimeStatus) { events <- status },
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.StartMonitoring(ctx, config.ID); err != nil {
				t.Fatal(err)
			}
			waitForRuntimeState(t, events, RuntimeRecording, "")
			if testCase.shutdown {
				err = manager.Shutdown(context.Background())
			} else {
				err = manager.StopMonitoring(context.Background(), config.ID)
			}
			if err != nil {
				t.Fatalf("operation did not retry done-worker finalization: %v", err)
			}
			_, _, finalizes := coordinator.counts()
			if finalizes != 2 {
				t.Fatalf("Finalize() calls = %d, want 2", finalizes)
			}
			manager.mu.RLock()
			worker := manager.workers[config.ID]
			manager.mu.RUnlock()
			if worker != nil {
				t.Fatal("terminal done-worker retry retained the worker")
			}
		})
	}
}

func TestMonitorManagerTerminalFinalizeWinsOverLaterStopIntent(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	defer store.Close()
	service, _ := NewService(store.Writer(), store.Reader(), newMemoryCredentialStore())
	config, _ := service.CreateRoom(ctx, CreateRoomInput{LiveID: "terminal-finalize-wins", RecordEnabled: true})
	online := newFakeLiveClient()
	firstOffline := newFakeLiveClient()
	firstOffline.offline = true
	secondOffline := newFakeLiveClient()
	secondOffline.offline = true
	coordinator := newFakeCaptureCoordinator()
	coordinator.finalizeStarted = make(chan struct{})
	coordinator.finalizeRelease = make(chan struct{})
	manager, err := NewMonitorManager(ctx, service, nil, MonitorOptions{
		Coordinator: coordinator, PollInterval: 5 * time.Millisecond, ReconnectDelay: 5 * time.Millisecond,
		Factory: (&scriptedLiveFactory{clients: []LiveClient{online, firstOffline, secondOffline}}).create,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.StartMonitoring(ctx, config.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-online.started:
	case <-time.After(time.Second):
		t.Fatal("online client did not start")
	}
	online.Close()
	select {
	case <-coordinator.finalizeStarted:
	case <-time.After(time.Second):
		t.Fatal("automatic Finalize() did not start")
	}
	firstCall := waitForFinalizeCalls(t, coordinator, 1, time.Second)[0]
	intentEntered := make(chan struct{})
	intentRelease := make(chan struct{})
	var intentOnce sync.Once
	manager.afterStopIntent = func() {
		intentOnce.Do(func() {
			close(intentEntered)
			<-intentRelease
		})
	}
	stopResult := make(chan error, 1)
	go func() { stopResult <- manager.StopMonitoring(context.Background(), config.ID) }()
	select {
	case <-intentEntered:
	case <-time.After(time.Second):
		t.Fatal("stop intent was not recorded")
	}
	manager.mu.RLock()
	worker := manager.workers[config.ID]
	manager.mu.RUnlock()
	worker.mu.RLock()
	pendingOperation := worker.stopOperation
	statusOperation := worker.status.OperationID
	inProgress := worker.finalizeInProgress
	worker.mu.RUnlock()
	if !inProgress || pendingOperation == "" || statusOperation != firstCall.operationID {
		t.Fatalf("stop/finalize arbitration = inProgress:%t pending:%q status:%q first:%q", inProgress, pendingOperation, statusOperation, firstCall.operationID)
	}
	close(coordinator.finalizeRelease)
	close(intentRelease)
	select {
	case err := <-stopResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("StopMonitoring() did not finish")
	}
	calls := coordinator.finalizeCallSnapshot()
	if len(calls) != 1 || calls[0].reason != capture.FinalizeOffline || calls[0].operationID != firstCall.operationID {
		t.Fatalf("terminal automatic finalization was replayed: %#v", calls)
	}
}

func TestMonitorManagerStopCancelsAutomaticFinalizeAndOwnsRetry(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	defer store.Close()
	service, _ := NewService(store.Writer(), store.Reader(), newMemoryCredentialStore())
	config, _ := service.CreateRoom(ctx, CreateRoomInput{LiveID: "cancel-auto-finalize", RecordEnabled: true})
	online := newFakeLiveClient()
	firstOffline := newFakeLiveClient()
	firstOffline.offline = true
	secondOffline := newFakeLiveClient()
	secondOffline.offline = true
	coordinator := newFakeCaptureCoordinator()
	coordinator.finalizeStarted = make(chan struct{})
	coordinator.finalizeRelease = make(chan struct{})
	coordinator.finalizeCancellationNonterminal = true
	manager, err := NewMonitorManager(ctx, service, nil, MonitorOptions{
		Coordinator: coordinator, FinalizeTimeout: 2 * time.Second,
		PollInterval: 5 * time.Millisecond, ReconnectDelay: 5 * time.Millisecond,
		Factory: (&scriptedLiveFactory{clients: []LiveClient{online, firstOffline, secondOffline}}).create,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.StartMonitoring(ctx, config.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-online.started:
	case <-time.After(time.Second):
		t.Fatal("online client did not start")
	}
	online.Close()
	select {
	case <-coordinator.finalizeStarted:
	case <-time.After(time.Second):
		t.Fatal("automatic Finalize() did not start")
	}
	firstCall := waitForFinalizeCalls(t, coordinator, 1, time.Second)[0]
	intentEntered := make(chan struct{})
	intentRelease := make(chan struct{})
	var intentOnce sync.Once
	manager.afterStopIntent = func() {
		intentOnce.Do(func() {
			close(intentEntered)
			<-intentRelease
		})
	}
	stopResult := make(chan error, 1)
	startedAt := time.Now()
	go func() { stopResult <- manager.StopMonitoring(context.Background(), config.ID) }()
	select {
	case <-intentEntered:
	case <-time.After(time.Second):
		t.Fatal("stop intent was not recorded")
	}
	manager.mu.RLock()
	worker := manager.workers[config.ID]
	manager.mu.RUnlock()
	worker.mu.RLock()
	pendingOperation := worker.stopOperation
	statusOperation := worker.status.OperationID
	inProgress := worker.finalizeInProgress
	worker.mu.RUnlock()
	if !inProgress || pendingOperation == "" || statusOperation != firstCall.operationID {
		t.Fatalf("stop/finalize arbitration = inProgress:%t pending:%q status:%q first:%q", inProgress, pendingOperation, statusOperation, firstCall.operationID)
	}
	close(intentRelease)
	calls := waitForFinalizeCalls(t, coordinator, 2, 500*time.Millisecond)
	if elapsed := time.Since(startedAt); elapsed >= time.Second {
		t.Fatalf("stop waited for automatic Finalize timeout before retry: %s", elapsed)
	}
	if calls[0].reason != capture.FinalizeOffline || calls[1].reason != capture.FinalizeStopped || calls[1].operationID != pendingOperation {
		t.Fatalf("stop retry did not own reason/operation: %#v pending=%q", calls, pendingOperation)
	}
	close(coordinator.finalizeRelease)
	select {
	case err := <-stopResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("StopMonitoring() did not finish after retry release")
	}
}

func waitForRuntimeState(t *testing.T, events <-chan RoomRuntimeStatus, state RuntimeState, code string) RoomRuntimeStatus {
	t.Helper()
	return waitForRuntimeStatus(t, events, func(status RoomRuntimeStatus) bool {
		return status.State == state && status.ErrorCode == code
	})
}

func waitForRuntimeStatus(t *testing.T, events <-chan RoomRuntimeStatus, match func(RoomRuntimeStatus) bool) RoomRuntimeStatus {
	t.Helper()
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case status := <-events:
			if match(status) {
				return status
			}
		case <-timeout.C:
			t.Fatal("timed out waiting for matching runtime status")
		}
	}
}

func waitForFinalizeCalls(t *testing.T, coordinator *fakeCaptureCoordinator, count int, timeout time.Duration) []fakeFinalizeCall {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		calls := coordinator.finalizeCallSnapshot()
		if len(calls) >= count {
			return calls
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d Finalize() calls; got %d", count, len(calls))
		}
		time.Sleep(time.Millisecond)
	}
}
