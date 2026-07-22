package app

import (
	"context"
	"testing"
)

func TestApplicationLifecycleIsIdempotent(t *testing.T) {
	application := New(Options{Name: "test", Version: "1.2.3"})
	if got := application.State(); got != StateCreated {
		t.Fatalf("initial state = %q, want %q", got, StateCreated)
	}
	application.Startup(context.Background())
	application.Startup(context.Background())
	if got := application.State(); got != StateRunning {
		t.Fatalf("running state = %q, want %q", got, StateRunning)
	}
	if err := application.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if err := application.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown() error = %v", err)
	}
	if got := application.State(); got != StateStopped {
		t.Fatalf("stopped state = %q, want %q", got, StateStopped)
	}
}

func TestApplicationBootstrapIsSanitizedAndVersioned(t *testing.T) {
	application := New(Options{Name: "桌面端", Version: "test"})
	application.Startup(context.Background())
	got := application.Bootstrap()
	if got.APIVersion != BootstrapAPIVersion {
		t.Fatalf("APIVersion = %q, want %q", got.APIVersion, BootstrapAPIVersion)
	}
	if got.Name != "桌面端" || got.Version != "test" || got.State != StateRunning {
		t.Fatalf("unexpected bootstrap: %#v", got)
	}
	if got.Build.ProductVersion != "test" || got.Build.DatabaseSchemaVersion != 6 ||
		got.Build.SettingsSchemaVersion != 2 || got.Build.FFmpegSHA256 == "" {
		t.Fatalf("unexpected build metadata: %#v", got.Build)
	}
	if len(got.Capabilities) != 7 {
		t.Fatalf("capabilities = %d, want 7", len(got.Capabilities))
	}
	if !got.Capabilities[0].Available || got.Capabilities[0].ID != "overview" {
		t.Fatalf("unexpected initial capability: %#v", got.Capabilities[0])
	}
	for _, capability := range got.Capabilities {
		if capability.ID == "" || capability.Label == "" {
			t.Fatalf("empty capability field: %#v", capability)
		}
		if capability.ID == "realtime" && capability.Available {
			t.Fatalf("realtime capability became available before infrastructure: %#v", capability)
		}
	}
}

func TestShutdownRejectsCancelledContext(t *testing.T) {
	application := New(Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := application.Shutdown(ctx); err == nil {
		t.Fatal("Shutdown() error = nil, want cancelled context error")
	}
}
