package main

import (
	"strings"
	"testing"

	application "github.com/jwwsjlm/douyinLive/v2/internal/app"
	"github.com/jwwsjlm/douyinLive/v2/internal/playback"
)

func TestPlaybackFacadeFailsClosedBeforeInfrastructure(t *testing.T) {
	facade := NewDesktopApp(application.New(application.Options{}))
	if _, err := facade.ListPlaybackSessions(
		playback.SessionFilter{}, playback.PageRequest{},
	); err == nil || !strings.Contains(err.Error(), "PLAYBACK_SERVICE_UNAVAILABLE") {
		t.Fatalf("ListPlaybackSessions() error = %v", err)
	}
	if _, err := facade.LocatePlaybackMedia(playback.MediaLocationRequest{}); err == nil || !strings.Contains(err.Error(), "PLAYBACK_SERVICE_UNAVAILABLE") {
		t.Fatalf("LocatePlaybackMedia() error = %v", err)
	}
}
