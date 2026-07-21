package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jwwsjlm/douyinLive/v2/internal/playback"
)

func TestInitializeInfrastructureProvidesPlaybackService(t *testing.T) {
	application := New(Options{Name: "test", Version: "test"})
	if err := application.InitializeInfrastructure(
		context.Background(),
		InfrastructureOptions{
			DataRoot:           filepath.Join(t.TempDir(), "playback-app-data"),
			DisableDiagnostics: true,
		},
	); err != nil {
		t.Fatalf("InitializeInfrastructure() error = %v", err)
	}
	service := application.PlaybackService()
	if service == nil {
		t.Fatal("PlaybackService() is nil")
	}
	page, err := service.ListSessions(
		context.Background(), playback.SessionFilter{}, playback.PageRequest{},
	)
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	if page.Version != playback.ContractVersion || len(page.Items) != 0 {
		t.Fatalf("initial playback page = %+v", page)
	}
	sessionsAvailable := false
	for _, capability := range application.Bootstrap().Capabilities {
		if capability.ID == "sessions" {
			sessionsAvailable = capability.Available
		}
	}
	if !sessionsAvailable {
		t.Fatal("sessions capability is unavailable")
	}
	application.Startup(context.Background())
	if err := application.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if application.PlaybackService() != nil {
		t.Fatal("PlaybackService() remained available after shutdown")
	}
}
