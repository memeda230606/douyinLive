package main

import (
	"context"
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
	if !bootstrap.Data.Ready || bootstrap.Data.SchemaVersion != 3 {
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

func TestDesktopUpdateSettingsAppliesToOpenEventSession(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	core := application.New(application.Options{Name: "privacy-runtime", Version: "test"})
	desktop := newDesktopApp(core, application.InfrastructureOptions{DataRoot: root})
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
