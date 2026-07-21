package main

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	douyinLive "github.com/jwwsjlm/douyinLive/v2"
	application "github.com/jwwsjlm/douyinLive/v2/internal/app"
	"github.com/jwwsjlm/douyinLive/v2/internal/capture"
	"github.com/jwwsjlm/douyinLive/v2/internal/eventstore"
	"github.com/jwwsjlm/douyinLive/v2/internal/room"
	"github.com/jwwsjlm/douyinLive/v2/internal/settings"
	"github.com/jwwsjlm/douyinlive-proto/generated/new_douyin"
	"google.golang.org/protobuf/proto"
)

func TestDesktopAppLifecycleAndBootstrap(t *testing.T) {
	core := application.New(application.Options{Name: "测试桌面端", Version: "test"})
	desktop := newDesktopApp(core, application.InfrastructureOptions{DataRoot: t.TempDir()})
	desktop.emitEvent = func(context.Context, string, ...interface{}) {}

	desktop.startup(context.Background())
	if got := desktop.GetState(); got != application.StateRunning {
		t.Fatalf("GetState() = %q, want %q", got, application.StateRunning)
	}

	bootstrap := desktop.GetBootstrap()
	if bootstrap.Name != "测试桌面端" || bootstrap.Version != "test" {
		t.Fatalf("unexpected bootstrap: %#v", bootstrap)
	}
	if bootstrap.APIVersion != application.BootstrapAPIVersion {
		t.Fatalf("APIVersion = %q, want %q", bootstrap.APIVersion, application.BootstrapAPIVersion)
	}
	if !bootstrap.Data.Ready || bootstrap.Data.SchemaVersion != 6 {
		t.Fatalf("data infrastructure not ready: %#v", bootstrap.Data)
	}
	created, err := desktop.CreateRoom(room.CreateRoomInput{
		LiveID: "binding-room", Alias: "绑定测试", MonitorEnabled: false,
		RecordingProfile: room.RecordingProfile{Quality: room.QualityAuto, SegmentMinutes: 10},
	})
	if err != nil {
		t.Fatalf("CreateRoom() error = %v", err)
	}
	rooms, err := desktop.ListRooms()
	if err != nil || len(rooms) != 1 || rooms[0].ID != created.ID {
		t.Fatalf("ListRooms() = (%#v, %v)", rooms, err)
	}
	status, err := desktop.GetRoomStatus(created.ID)
	if err != nil || status.State != room.RuntimeStopped {
		t.Fatalf("GetRoomStatus() = (%#v, %v)", status, err)
	}
	gotSettings, err := desktop.GetSettings()
	if err != nil || gotSettings.Version != settings.SettingsVersion {
		t.Fatalf("GetSettings() = (%#v, %v)", gotSettings, err)
	}

	desktop.shutdown(context.Background())
	if got := desktop.GetState(); got != application.StateStopped {
		t.Fatalf("GetState() = %q, want %q", got, application.StateStopped)
	}
}

func TestDesktopBootstrapWaitsForStartupConclusion(t *testing.T) {
	desktop := newDesktopApp(
		application.New(application.Options{Name: "bootstrap-wait", Version: "test"}),
		application.InfrastructureOptions{},
	)
	desktop.beginStartup(context.Background())
	result := make(chan application.BootstrapDTO, 1)
	go func() { result <- desktop.GetBootstrap() }()
	select {
	case <-result:
		t.Fatal("GetBootstrap() returned before startup concluded")
	case <-time.After(50 * time.Millisecond):
	}
	desktop.finishStartup()
	select {
	case bootstrap := <-result:
		if bootstrap.Name != "bootstrap-wait" {
			t.Fatalf("GetBootstrap().Name = %q", bootstrap.Name)
		}
	case <-time.After(time.Second):
		t.Fatal("GetBootstrap() did not return after startup concluded")
	}
}

func TestDesktopBootstrapWaitersReleaseTogether(t *testing.T) {
	desktop := newDesktopApp(
		application.New(application.Options{Name: "bootstrap-concurrent", Version: "test"}),
		application.InfrastructureOptions{},
	)
	desktop.beginStartup(context.Background())
	const waiters = 8
	results := make(chan application.BootstrapDTO, waiters)
	for index := 0; index < waiters; index++ {
		go func() { results <- desktop.GetBootstrap() }()
	}
	desktop.finishStartup()
	for index := 0; index < waiters; index++ {
		select {
		case bootstrap := <-results:
			if bootstrap.Name != "bootstrap-concurrent" {
				t.Fatalf("waiter %d name = %q", index, bootstrap.Name)
			}
		case <-time.After(time.Second):
			t.Fatalf("waiter %d did not return", index)
		}
	}
}

func TestDesktopBootstrapStartupCancellationReleasesWait(t *testing.T) {
	desktop := newDesktopApp(
		application.New(application.Options{Name: "bootstrap-cancel", Version: "test"}),
		application.InfrastructureOptions{},
	)
	ctx, cancel := context.WithCancel(context.Background())
	desktop.beginStartup(ctx)
	result := make(chan application.BootstrapDTO, 1)
	go func() { result <- desktop.GetBootstrap() }()
	cancel()
	select {
	case <-result:
		t.Fatal("GetBootstrap() returned before the cancelled startup owner concluded")
	case <-time.After(50 * time.Millisecond):
	}
	desktop.finishStartup()
	select {
	case bootstrap := <-result:
		if bootstrap.State != application.StateRunning {
			t.Fatalf("cancelled startup bootstrap state = %q", bootstrap.State)
		}
	case <-time.After(time.Second):
		t.Fatal("GetBootstrap() did not return after the cancelled startup concluded")
	}
}

func TestDesktopBootstrapWithoutStartupDoesNotBlock(t *testing.T) {
	desktop := newDesktopApp(
		application.New(application.Options{Name: "bootstrap-unstarted", Version: "test"}),
		application.InfrastructureOptions{},
	)
	result := make(chan application.BootstrapDTO, 1)
	go func() { result <- desktop.GetBootstrap() }()
	select {
	case bootstrap := <-result:
		if bootstrap.Name != "bootstrap-unstarted" {
			t.Fatalf("GetBootstrap().Name = %q", bootstrap.Name)
		}
	case <-time.After(time.Second):
		t.Fatal("unstarted GetBootstrap() blocked")
	}
}

func TestDesktopBootstrapInitializationFailureReleasesWait(t *testing.T) {
	root := t.TempDir()
	invalidRoot := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(invalidRoot, []byte("fixture"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	desktop := newDesktopApp(
		application.New(application.Options{Name: "bootstrap-failure", Version: "test"}),
		application.InfrastructureOptions{DataRoot: invalidRoot},
	)
	desktop.emitEvent = func(context.Context, string, ...interface{}) {}
	desktop.startup(context.Background())
	result := make(chan application.BootstrapDTO, 1)
	go func() { result <- desktop.GetBootstrap() }()
	select {
	case bootstrap := <-result:
		if bootstrap.Data.Ready {
			t.Fatalf("failed bootstrap reported ready: %#v", bootstrap.Data)
		}
	case <-time.After(time.Second):
		t.Fatal("failed startup did not release GetBootstrap()")
	}
	desktop.shutdown(context.Background())
}

func TestDesktopBootstrapShutdownReleasesWait(t *testing.T) {
	desktop := newDesktopApp(
		application.New(application.Options{Name: "bootstrap-shutdown", Version: "test"}),
		application.InfrastructureOptions{},
	)
	desktop.beginStartup(context.Background())
	result := make(chan application.BootstrapDTO, 1)
	go func() { result <- desktop.GetBootstrap() }()
	desktop.shutdown(context.Background())
	select {
	case bootstrap := <-result:
		if bootstrap.State != application.StateStopped {
			t.Fatalf("shutdown bootstrap state = %q, want stopped", bootstrap.State)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not release GetBootstrap()")
	}
}

func TestDesktopBootstrapArmedBeforeStartupWaits(t *testing.T) {
	desktop := newDesktopApp(application.New(application.Options{Name: "bootstrap-armed"}), application.InfrastructureOptions{})
	desktop.armStartup()
	result := make(chan application.BootstrapDTO, 1)
	go func() { result <- desktop.GetBootstrap() }()
	select {
	case <-result:
		t.Fatal("pre-armed GetBootstrap() returned before startup")
	case <-time.After(50 * time.Millisecond):
	}
	desktop.beginStartup(context.Background())
	desktop.finishStartup()
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("pre-armed GetBootstrap() did not return after startup")
	}
}

func TestDesktopShutdownPermanentlyRejectsLateStartup(t *testing.T) {
	desktop := newDesktopApp(application.New(application.Options{Name: "late-startup"}), application.InfrastructureOptions{})
	desktop.armStartup()
	desktop.shutdown(context.Background())
	desktop.startup(context.Background())
	if state := desktop.GetState(); state != application.StateStopped {
		t.Fatalf("late startup state = %q, want stopped", state)
	}
	if desktop.acceptingEvents.Load() {
		t.Fatal("late startup reopened the event emitter")
	}
	if bootstrap := desktop.GetBootstrap(); bootstrap.State != application.StateStopped {
		t.Fatalf("late startup bootstrap state = %q", bootstrap.State)
	}
}

func TestDesktopDuplicateStartupCannotReleaseOwnerBarrier(t *testing.T) {
	desktop := newDesktopApp(application.New(application.Options{Name: "duplicate-startup"}), application.InfrastructureOptions{})
	if _, started := desktop.beginStartup(context.Background()); !started {
		t.Fatal("first startup was rejected")
	}
	if _, started := desktop.beginStartup(context.Background()); started {
		t.Fatal("duplicate startup was accepted")
	}
	result := make(chan application.BootstrapDTO, 1)
	go func() { result <- desktop.GetBootstrap() }()
	select {
	case <-result:
		t.Fatal("duplicate startup released the owner barrier")
	case <-time.After(50 * time.Millisecond):
	}
	desktop.finishStartup()
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("owner startup did not release the barrier")
	}
}

func TestDesktopAppEmitterFenceDropsEventsAfterShutdownStarts(t *testing.T) {
	var emitted atomic.Int32
	desktop := &DesktopApp{emitEvent: func(context.Context, string, ...interface{}) {
		emitted.Add(1)
	}}
	desktop.acceptingEvents.Store(true)
	desktop.emit(context.Background(), room.StatusEventName, room.RoomRuntimeStatus{})
	if got := emitted.Load(); got != 1 {
		t.Fatalf("active emitter calls = %d, want 1", got)
	}
	desktop.acceptingEvents.Store(false)
	desktop.emit(context.Background(), eventstore.LiveEventEventName, eventstore.LiveEventBatchDTO{})
	desktop.emit(context.Background(), capture.RecordingProgressEventName, capture.RecordingProgressDTO{})
	if got := emitted.Load(); got != 1 {
		t.Fatalf("post-shutdown emitter calls = %d, want 1", got)
	}
}

func TestDesktopUpdateSettingsAppliesToOpenEventSession(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	core := application.New(application.Options{Name: "privacy-runtime", Version: "test"})
	desktop := newDesktopApp(core, application.InfrastructureOptions{DataRoot: root})
	desktop.emitEvent = func(context.Context, string, ...interface{}) {}
	desktop.startup(ctx)
	t.Cleanup(func() { desktop.shutdown(context.Background()) })
	if core.EventStoreManager() == nil || core.Store() == nil {
		t.Fatal("desktop infrastructure did not initialize")
	}

	roomConfig, err := desktop.CreateRoom(room.CreateRoomInput{
		LiveID: "privacy-runtime-room", Alias: "privacy-runtime",
		RecordingProfile: room.RecordingProfile{Quality: room.QualityAuto, SegmentMinutes: 10},
	})
	if err != nil {
		t.Fatalf("CreateRoom() error = %v", err)
	}
	repository, err := capture.NewSQLiteRepository(core.Store().Writer(), core.Store().Reader(), root)
	if err != nil {
		t.Fatalf("NewSQLiteRepository() error = %v", err)
	}
	operationID, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7() error = %v", err)
	}
	session, err := repository.Create(ctx, capture.CreateSessionInput{
		RoomConfigID: roomConfig.ID, OperationID: operationID.String(),
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	sink, err := core.EventStoreManager().OpenSession(ctx, eventstore.SessionDescriptor{
		SessionID: session.ID, DataPath: session.DataPath,
		PlatformRoomID: session.PlatformRoomID,
		StartedAt:      time.UnixMilli(session.StartedAt).UTC(),
	})
	if err != nil {
		t.Fatalf("OpenSession() error = %v", err)
	}

	acceptChat := func(messageID uint64, nickname string, receivedAt time.Time) {
		t.Helper()
		payload, marshalErr := proto.Marshal(&new_douyin.Webcast_Im_ChatMessage{
			Common:  &new_douyin.Webcast_Im_Common{MsgId: messageID},
			User:    &new_douyin.Webcast_Data_User{WebcastUid: "runtime-user", Nickname: nickname},
			Content: "runtime-privacy-content",
		})
		if marshalErr != nil {
			t.Fatalf("proto.Marshal() error = %v", marshalErr)
		}
		sink.Accept(&douyinLive.LiveMessage{
			Raw:        &new_douyin.Webcast_Im_Message{Method: "WebcastChatMessage", Payload: payload},
			ReceivedAt: receivedAt,
		})
	}
	startedAt := time.UnixMilli(session.StartedAt).UTC()
	acceptChat(8101, "before-toggle", startedAt.Add(time.Second))
	deadline := time.Now().Add(3 * time.Second)
	for {
		var count int
		err := core.Store().Reader().QueryRow(`SELECT COUNT(*) FROM live_events
			WHERE session_id = ? AND event_role = 'source'`, session.ID).Scan(&count)
		if err == nil && count == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("first event was not persisted before settings update: count=%d err=%v", count, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	current, err := desktop.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings() error = %v", err)
	}
	updated, err := desktop.UpdateSettings(settings.UpdateSettingsInput{
		RecordingDirectory:      current.RecordingDirectory,
		DefaultQuality:          current.DefaultQuality,
		DefaultSegmentMinutes:   current.DefaultSegmentMinutes,
		MaxConcurrentRecordings: current.MaxConcurrentRecordings,
		MinimumFreeSpaceGiB:     current.MinimumFreeSpaceGiB,
		SaveDisplayNames:        false,
	})
	if err != nil || updated.SaveDisplayNames {
		t.Fatalf("UpdateSettings() = (%#v, %v)", updated, err)
	}
	acceptChat(8102, "after-toggle", startedAt.Add(2*time.Second))
	if err := sink.FlushAndClose(ctx); err != nil {
		t.Fatalf("FlushAndClose() error = %v", err)
	}

	rows, err := core.Store().Reader().Query(`SELECT ingest_sequence, COALESCE(display_name, '')
		FROM live_events WHERE session_id = ? AND event_role = 'source' ORDER BY ingest_sequence`, session.ID)
	if err != nil {
		t.Fatalf("query display names: %v", err)
	}
	defer rows.Close()
	var displayNames []string
	for rows.Next() {
		var sequence int64
		var displayName string
		if err := rows.Scan(&sequence, &displayName); err != nil {
			t.Fatalf("scan display name: %v", err)
		}
		displayNames = append(displayNames, displayName)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate display names: %v", err)
	}
	if len(displayNames) != 2 || displayNames[0] != "before-toggle" || displayNames[1] != "" {
		t.Fatalf("display names after runtime toggle = %#v", displayNames)
	}
}
