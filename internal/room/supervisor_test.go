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
func (c *fakeLiveClient) Close()   { c.closeOnce.Do(func() { close(c.closed) }) }
func (c *fakeLiveClient) Dispose() { c.Close() }

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
		MaxRooms: 1, PollInterval: time.Hour,
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
	manager, err := NewMonitorManager(ctx, service, nil, MonitorOptions{Publisher: func(RoomRuntimeStatus) { panic("test") }})
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

func waitForRuntimeState(t *testing.T, events <-chan RoomRuntimeStatus, state RuntimeState, code string) RoomRuntimeStatus {
	t.Helper()
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case status := <-events:
			if status.State == state && status.ErrorCode == code {
				return status
			}
		case <-timeout.C:
			t.Fatalf("timed out waiting for state=%s code=%s", state, code)
		}
	}
}
