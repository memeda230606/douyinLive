package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jwwsjlm/douyinLive/v2/internal/room"
)

func TestApplicationShutdownTimeoutKeepsResourcesUntilSharedCleanupCompletes(t *testing.T) {
	application := New(Options{Name: "shutdown-test", Version: "test"})
	application.Startup(context.Background())
	if err := application.InitializeInfrastructure(context.Background(), InfrastructureOptions{DataRoot: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	service := application.RoomService()
	config, err := service.CreateRoom(context.Background(), room.CreateRoomInput{LiveID: "app-shared-cleanup"})
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := application.Context()
	application.mu.RLock()
	generation := application.lifecycleGeneration
	application.mu.RUnlock()

	cleanupEntered := make(chan struct{})
	cleanupRelease := make(chan struct{})
	var cleanupOnce sync.Once
	application.beforeShutdownCleanup = func() {
		cleanupOnce.Do(func() {
			close(cleanupEntered)
			<-cleanupRelease
		})
	}
	firstCtx, firstCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer firstCancel()
	if err := application.Shutdown(firstCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Shutdown() error = %v, want deadline exceeded", err)
	}
	select {
	case <-cleanupEntered:
	case <-time.After(time.Second):
		t.Fatal("shared cleanup did not start")
	}
	if state := application.State(); state != StateStopping {
		t.Fatalf("state after timed-out wait = %q, want %q", state, StateStopping)
	}
	if application.Store() != nil || application.RoomService() != nil || application.MonitorManager() != nil || application.Bootstrap().Data.Ready {
		t.Fatalf("stopping application still exposed public infrastructure: %#v", application.Bootstrap())
	}
	if _, err := service.GetRoom(context.Background(), config.ID); err != nil {
		t.Fatalf("owned database closed after caller timeout: %v", err)
	}
	select {
	case <-lifecycle.Done():
		t.Fatal("lifecycle was cancelled before monitor cleanup completed")
	default:
	}
	if application.Context().Done() != lifecycle.Done() {
		t.Fatal("Context() stopped exposing the owned lifecycle during cleanup")
	}
	application.Startup(context.Background())
	application.mu.RLock()
	generationAfterStartup := application.lifecycleGeneration
	application.mu.RUnlock()
	if application.State() != StateStopping || generationAfterStartup != generation+1 {
		t.Fatalf("Startup reset an incomplete cleanup: state=%s generation=%d want=%d", application.State(), generationAfterStartup, generation+1)
	}

	type shutdownResult struct {
		caller int
		err    error
	}
	results := make(chan shutdownResult, 2)
	for caller := 0; caller < 2; caller++ {
		go func(caller int) {
			results <- shutdownResult{caller: caller, err: application.Shutdown(context.Background())}
		}(caller)
	}
	select {
	case result := <-results:
		t.Fatalf("concurrent Shutdown(%d) returned before shared cleanup release: %v", result.caller, result.err)
	case <-time.After(20 * time.Millisecond):
	}
	close(cleanupRelease)
	for range 2 {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatalf("concurrent Shutdown(%d) error = %v", result.caller, result.err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent Shutdown() did not observe shared completion")
		}
	}
	if state := application.State(); state != StateStopped {
		t.Fatalf("completed state = %q, want %q", state, StateStopped)
	}
	select {
	case <-lifecycle.Done():
	default:
		t.Fatal("lifecycle was not cancelled after monitor cleanup")
	}
	select {
	case <-application.Context().Done():
	default:
		t.Fatal("stopped Context() was not pre-cancelled")
	}
	if _, err := service.GetRoom(context.Background(), config.ID); err == nil {
		t.Fatal("captured room service still accessed the database after shared cleanup")
	}

	application.mu.Lock()
	application.beforeShutdownCleanup = nil
	application.mu.Unlock()
	application.Startup(context.Background())
	if state := application.State(); state != StateRunning {
		t.Fatalf("Startup after completed cleanup state = %q, want %q", state, StateRunning)
	}
	select {
	case <-application.Context().Done():
		t.Fatal("new lifecycle generation inherited cancellation")
	default:
	}
	if err := application.Shutdown(context.Background()); err != nil {
		t.Fatalf("new lifecycle Shutdown() error = %v", err)
	}
}

func TestApplicationCancelledFirstShutdownStillOwnsCleanup(t *testing.T) {
	application := New(Options{Name: "cancelled-shutdown", Version: "test"})
	application.Startup(context.Background())
	if err := application.InitializeInfrastructure(context.Background(), InfrastructureOptions{DataRoot: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	service := application.RoomService()
	config, err := service.CreateRoom(context.Background(), room.CreateRoomInput{LiveID: "cancelled-first-cleanup"})
	if err != nil {
		t.Fatal(err)
	}
	cleanupEntered := make(chan struct{})
	cleanupRelease := make(chan struct{})
	application.beforeShutdownCleanup = func() {
		close(cleanupEntered)
		<-cleanupRelease
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := application.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled first Shutdown() error = %v, want context canceled", err)
	}
	select {
	case <-cleanupEntered:
	case <-time.After(time.Second):
		t.Fatal("cancelled first Shutdown() did not start shared cleanup")
	}
	if _, err := service.GetRoom(context.Background(), config.ID); err != nil {
		t.Fatalf("cancelled caller caused premature database close: %v", err)
	}
	close(cleanupRelease)
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer waitCancel()
	if err := application.Shutdown(waitCtx); err != nil {
		t.Fatalf("fresh Shutdown() wait error = %v", err)
	}
	if application.State() != StateStopped {
		t.Fatalf("state after shared cleanup = %q", application.State())
	}
	if _, err := service.GetRoom(context.Background(), config.ID); err == nil {
		t.Fatal("database remained open after shared cleanup completed")
	}
}
