package settings

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jwwsjlm/douyinLive/v2/internal/room"
)

func TestSettingsDefaultsUpdateAndRestart(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config")
	defaultRecording := filepath.Join(root, "rooms")
	service, err := Open(config, root, defaultRecording)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defaults, err := service.GetSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Version != SettingsVersion || defaults.StorageRoot != root || defaults.RecordingDirectory != defaultRecording {
		t.Fatalf("unexpected defaults: %#v", defaults)
	}
	if defaults.DefaultQuality != room.QualityAuto || defaults.DefaultSegmentMinutes != 10 || defaults.MaxConcurrentRecordings != 1 {
		t.Fatalf("unexpected recording defaults: %#v", defaults)
	}

	customDirectory := filepath.Join(t.TempDir(), "recordings")
	updated, err := service.UpdateSettings(context.Background(), UpdateSettingsInput{
		RecordingDirectory: customDirectory, DefaultQuality: room.QualityHigh, DefaultSegmentMinutes: 15,
		MaxConcurrentRecordings: 2, MinimumFreeSpaceGiB: 20, SaveDisplayNames: false,
	})
	if err != nil {
		t.Fatalf("UpdateSettings() error = %v", err)
	}
	if updated.RecordingDirectory != customDirectory || updated.DefaultQuality != room.QualityHigh || updated.SaveDisplayNames {
		t.Fatalf("unexpected updated settings: %#v", updated)
	}
	if _, err := os.Stat(customDirectory); err != nil {
		t.Fatalf("recording directory not prepared: %v", err)
	}

	restarted, err := Open(config, root, defaultRecording)
	if err != nil {
		t.Fatalf("Open(restart) error = %v", err)
	}
	got, err := restarted.GetSettings(context.Background())
	if err != nil || got != updated {
		t.Fatalf("settings after restart = (%#v, %v), want %#v", got, err, updated)
	}
	data, err := os.ReadFile(filepath.Join(config, "settings.json"))
	if err != nil || len(data) == 0 {
		t.Fatalf("settings file = (%d bytes, %v)", len(data), err)
	}
}

func TestSettingsValidationKeepsPreviousValues(t *testing.T) {
	root := t.TempDir()
	service, err := Open(filepath.Join(root, "config"), root, filepath.Join(root, "rooms"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := service.GetSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.UpdateSettings(context.Background(), UpdateSettingsInput{
		RecordingDirectory: "relative", DefaultQuality: room.QualityAuto, DefaultSegmentMinutes: 10,
		MaxConcurrentRecordings: 1, MinimumFreeSpaceGiB: 10, SaveDisplayNames: true,
	})
	if ErrorCode(err) != "SETTINGS_INVALID" {
		t.Fatalf("UpdateSettings(relative) error = %v", err)
	}
	after, err := service.GetSettings(context.Background())
	if err != nil || after != before {
		t.Fatalf("settings changed after invalid update: before=%#v after=%#v err=%v", before, after, err)
	}
}
