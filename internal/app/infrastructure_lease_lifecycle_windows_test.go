//go:build windows

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jwwsjlm/douyinLive/v2/internal/capture"
)

func TestApplicationInfrastructureLeaseRejectsBeforePersistentOpen(t *testing.T) {
	root := filepath.Join(t.TempDir(), "fresh-data")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	blocker, err := acquireApplicationInstanceLease(root)
	if err != nil {
		t.Fatalf("acquire blocking lease: %v", err)
	}
	t.Cleanup(func() { _ = blocker.Close() })

	application := New(Options{Name: "lease-blocked", Version: "test"})
	application.Startup(context.Background())
	err = application.InitializeInfrastructure(
		context.Background(),
		InfrastructureOptions{DataRoot: root},
	)
	if !errors.Is(err, ErrApplicationInstanceActive) || strings.Contains(err.Error(), root) {
		t.Fatalf("blocked InitializeInfrastructure() error = %v", err)
	}
	if application.Store() != nil || application.MonitorManager() != nil ||
		application.CaptureCoordinator() != nil || application.Bootstrap().Data.Ready {
		t.Fatal("blocked application published infrastructure")
	}
	var files []string
	if walkErr := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			files = append(files, filepath.Base(path))
		}
		return nil
	}); walkErr != nil || len(files) != 0 {
		t.Fatalf("blocked instance created persistent files = %v, err = %v", files, walkErr)
	}
	if err := application.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown blocked application: %v", err)
	}
}

func TestApplicationInfrastructureLeaseRejectsSecondInstanceUntilShutdown(t *testing.T) {
	root := filepath.Join(t.TempDir(), "app-data")
	first := New(Options{Name: "lease-first", Version: "test"})
	first.Startup(context.Background())
	first.newRecorderFactory = unavailableLeaseTestRecorderFactory
	if err := first.InitializeInfrastructure(
		context.Background(),
		InfrastructureOptions{DataRoot: root},
	); err != nil {
		t.Fatalf("initialize first application: %v", err)
	}
	firstOwned := true
	t.Cleanup(func() {
		if firstOwned {
			_ = first.Shutdown(context.Background())
		}
	})

	second := New(Options{Name: "lease-second", Version: "test"})
	second.Startup(context.Background())
	recorderDiscoveryCalls := 0
	second.newRecorderFactory = func(
		context.Context,
		capture.FFmpegRecorderFactoryOptions,
	) (capture.RecorderFactory, capture.FFmpegDependencyInfo, error) {
		recorderDiscoveryCalls++
		return unavailableLeaseTestRecorderFactory(context.Background(), capture.FFmpegRecorderFactoryOptions{})
	}
	err := second.InitializeInfrastructure(
		context.Background(),
		InfrastructureOptions{DataRoot: root},
	)
	if !errors.Is(err, ErrApplicationInstanceActive) || recorderDiscoveryCalls != 0 {
		t.Fatalf(
			"initialize duplicate application = err:%v discovery:%d",
			err,
			recorderDiscoveryCalls,
		)
	}
	if second.Store() != nil || second.MonitorManager() != nil || second.CaptureCoordinator() != nil {
		t.Fatal("duplicate application published uncommitted infrastructure")
	}

	if err := first.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown first application: %v", err)
	}
	firstOwned = false
	if err := second.InitializeInfrastructure(
		context.Background(),
		InfrastructureOptions{DataRoot: root},
	); err != nil {
		t.Fatalf("initialize second application after release: %v", err)
	}
	if recorderDiscoveryCalls != 1 {
		t.Fatalf("recorder discovery calls after release = %d, want 1", recorderDiscoveryCalls)
	}
	if err := second.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown second application: %v", err)
	}
}

func TestApplicationUncommittedLeaseSurvivesSupersedingShutdownUntilCleanup(t *testing.T) {
	root := filepath.Join(t.TempDir(), "superseded-data")
	application := New(Options{Name: "lease-superseded", Version: "test"})
	application.Startup(context.Background())
	application.newRecorderFactory = unavailableLeaseTestRecorderFactory
	commitReady := make(chan struct{})
	commitRelease := make(chan struct{})
	defer func() {
		select {
		case <-commitRelease:
		default:
			close(commitRelease)
		}
	}()
	application.beforeInfrastructureCommit = func() {
		close(commitReady)
		<-commitRelease
	}
	initResult := make(chan error, 1)
	go func() {
		initResult <- application.InitializeInfrastructure(
			context.Background(),
			InfrastructureOptions{DataRoot: root},
		)
	}()
	select {
	case <-commitReady:
	case err := <-initResult:
		t.Fatalf("initialization returned before commit barrier: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("initialization did not reach commit barrier")
	}
	contender, err := acquireApplicationInstanceLease(root)
	if contender != nil || !errors.Is(err, ErrApplicationInstanceActive) {
		t.Fatalf("lease at commit barrier = (%T, %v), want active", contender, err)
	}
	if err := application.Shutdown(context.Background()); err != nil {
		t.Fatalf("superseding Shutdown() error = %v", err)
	}
	contender, err = acquireApplicationInstanceLease(root)
	if contender != nil || !errors.Is(err, ErrApplicationInstanceActive) {
		t.Fatalf("lease before uncommitted cleanup = (%T, %v), want active", contender, err)
	}
	close(commitRelease)
	select {
	case err := <-initResult:
		if !errors.Is(err, ErrInfrastructureSuperseded) {
			t.Fatalf("superseded initialization error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("superseded initialization did not clean up")
	}
	reacquired, err := acquireApplicationInstanceLease(root)
	if err != nil {
		t.Fatalf("reacquire after superseded cleanup: %v", err)
	}
	if err := reacquired.Close(); err != nil {
		t.Fatalf("close reacquired lease: %v", err)
	}
}

func TestApplicationLeaseRemainsHeldWhileShutdownCallerTimesOut(t *testing.T) {
	root := filepath.Join(t.TempDir(), "shutdown-data")
	application := New(Options{Name: "lease-shutdown", Version: "test"})
	application.Startup(context.Background())
	application.newRecorderFactory = unavailableLeaseTestRecorderFactory
	if err := application.InitializeInfrastructure(
		context.Background(),
		InfrastructureOptions{DataRoot: root},
	); err != nil {
		t.Fatalf("initialize application: %v", err)
	}
	cleanupEntered := make(chan struct{})
	cleanupRelease := make(chan struct{})
	defer func() {
		select {
		case <-cleanupRelease:
		default:
			close(cleanupRelease)
		}
	}()
	application.beforeShutdownCleanup = func() {
		close(cleanupEntered)
		<-cleanupRelease
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := application.Shutdown(shutdownCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed-out Shutdown() error = %v", err)
	}
	select {
	case <-cleanupEntered:
	case <-time.After(time.Second):
		t.Fatal("shared shutdown cleanup did not start")
	}
	contender, err := acquireApplicationInstanceLease(root)
	if contender != nil || !errors.Is(err, ErrApplicationInstanceActive) {
		t.Fatalf("lease during shared cleanup = (%T, %v), want active", contender, err)
	}
	close(cleanupRelease)
	if err := application.Shutdown(context.Background()); err != nil {
		t.Fatalf("wait for shared shutdown: %v", err)
	}
	reacquired, err := acquireApplicationInstanceLease(root)
	if err != nil {
		t.Fatalf("reacquire after shared shutdown: %v", err)
	}
	_ = reacquired.Close()
}

func unavailableLeaseTestRecorderFactory(
	context.Context,
	capture.FFmpegRecorderFactoryOptions,
) (capture.RecorderFactory, capture.FFmpegDependencyInfo, error) {
	return nil, capture.FFmpegDependencyInfo{}, errors.New("recording dependency unavailable for lease test")
}
