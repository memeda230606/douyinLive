package main

import (
	"errors"

	"github.com/jwwsjlm/douyinLive/v2/internal/playback"
)

func (a *DesktopApp) GetPlaybackSession(sessionID string) (playback.SessionResult, error) {
	service, err := a.playbackService()
	if err != nil {
		return playback.SessionResult{}, err
	}
	return service.GetSession(a.application.Context(), sessionID)
}

func (a *DesktopApp) ListPlaybackSessions(
	filter playback.SessionFilter,
	page playback.PageRequest,
) (playback.SessionPage, error) {
	service, err := a.playbackService()
	if err != nil {
		return playback.SessionPage{}, err
	}
	return service.ListSessions(a.application.Context(), filter, page)
}

func (a *DesktopApp) ListPlaybackEvents(
	filter playback.EventFilter,
	page playback.PageRequest,
) (playback.EventPage, error) {
	service, err := a.playbackService()
	if err != nil {
		return playback.EventPage{}, err
	}
	return service.ListEvents(a.application.Context(), filter, page)
}

func (a *DesktopApp) ListPlaybackGaps(
	filter playback.GapFilter,
	page playback.PageRequest,
) (playback.GapPage, error) {
	service, err := a.playbackService()
	if err != nil {
		return playback.GapPage{}, err
	}
	return service.ListGaps(a.application.Context(), filter, page)
}

func (a *DesktopApp) ListPlaybackMedia(
	filter playback.MediaFilter,
	page playback.PageRequest,
) (playback.MediaPage, error) {
	service, err := a.playbackService()
	if err != nil {
		return playback.MediaPage{}, err
	}
	return service.ListMediaSegments(a.application.Context(), filter, page)
}

func (a *DesktopApp) LocatePlaybackMedia(
	request playback.MediaLocationRequest,
) (playback.MediaLocationResult, error) {
	service, err := a.playbackService()
	if err != nil {
		return playback.MediaLocationResult{}, err
	}
	return service.LocateMedia(a.application.Context(), request)
}

func (a *DesktopApp) playbackService() (*playback.Service, error) {
	service := a.application.PlaybackService()
	if service == nil {
		return nil, errors.New("PLAYBACK_SERVICE_UNAVAILABLE: 回放服务尚未就绪")
	}
	return service, nil
}
