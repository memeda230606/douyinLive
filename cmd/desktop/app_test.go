package main

import (
	"context"
	"testing"

	application "github.com/jwwsjlm/douyinLive/v2/internal/app"
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
	if !bootstrap.Data.Ready || bootstrap.Data.SchemaVersion != 1 {
		t.Fatalf("data infrastructure not ready: %#v", bootstrap.Data)
	}

	desktop.shutdown(context.Background())
	if got := desktop.GetState(); got != application.StateStopped {
		t.Fatalf("GetState() = %q, want %q", got, application.StateStopped)
	}
}
