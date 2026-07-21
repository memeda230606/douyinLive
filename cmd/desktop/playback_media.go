package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jwwsjlm/douyinLive/v2/internal/playback"
)

const playbackMediaPathPrefix = "/playback/media/"

type playbackMediaContent struct {
	reader      io.ReadSeeker
	close       func() error
	modTime     time.Time
	contentType string
}

type playbackMediaOpenFunc func(context.Context, string) (playbackMediaContent, error)

func (a *DesktopApp) playbackMediaMiddleware(next http.Handler) http.Handler {
	return newPlaybackMediaHandler(a.openPlaybackMedia, next)
}

func (a *DesktopApp) openPlaybackMedia(ctx context.Context, artifactID string) (playbackMediaContent, error) {
	service, err := a.playbackService()
	if err != nil {
		return playbackMediaContent{}, err
	}
	file, err := service.OpenMedia(ctx, artifactID)
	if err != nil {
		return playbackMediaContent{}, err
	}
	return playbackMediaContent{
		reader: file, close: file.Close, modTime: file.ModTime(), contentType: file.ContentType(),
	}, nil
}

func newPlaybackMediaHandler(open playbackMediaOpenFunc, next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !strings.HasPrefix(request.URL.Path, playbackMediaPathPrefix) {
			next.ServeHTTP(response, request)
			return
		}
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			response.Header().Set("Allow", "GET, HEAD")
			http.Error(response, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		artifactID := strings.TrimPrefix(request.URL.Path, playbackMediaPathPrefix)
		if artifactID == "" || strings.Contains(artifactID, "/") || request.URL.RawQuery != "" {
			http.NotFound(response, request)
			return
		}
		content, err := open(request.Context(), artifactID)
		if err != nil {
			status := http.StatusConflict
			if errors.Is(err, playback.ErrMediaNotFound) || errors.Is(err, playback.ErrInvalidArgument) {
				status = http.StatusNotFound
			}
			http.Error(response, http.StatusText(status), status)
			return
		}
		if content.reader == nil || content.close == nil {
			http.Error(response, http.StatusText(http.StatusConflict), http.StatusConflict)
			return
		}
		defer content.close()
		response.Header().Set("Cache-Control", "no-store")
		response.Header().Set("Content-Type", content.contentType)
		response.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(response, request, "playback.mp4", content.modTime, content.reader)
	})
}
