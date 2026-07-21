package app

import "github.com/jwwsjlm/douyinLive/v2/internal/playback"

func (a *Application) PlaybackService() *playback.Service {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.playback
}
