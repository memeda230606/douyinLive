package main

import (
	"context"
	"testing"

	application "github.com/jwwsjlm/douyinLive/v2/internal/app"
	"github.com/jwwsjlm/douyinLive/v2/internal/room"
	"github.com/jwwsjlm/douyinLive/v2/internal/settings"
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
	if !bootstrap.Data.Ready || bootstrap.Data.SchemaVersion != 2 {
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
