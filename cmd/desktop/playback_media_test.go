package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jwwsjlm/douyinLive/v2/internal/playback"
)

type closeTrackingReadSeeker struct {
	*bytes.Reader
	closed bool
}

func (reader *closeTrackingReadSeeker) Close() error {
	reader.closed = true
	return nil
}

func TestPlaybackMediaHandlerServesBoundedRangeAndCloses(t *testing.T) {
	payload := []byte("0123456789")
	opened := &closeTrackingReadSeeker{Reader: bytes.NewReader(payload)}
	handler := newPlaybackMediaHandler(func(context.Context, string) (playbackMediaContent, error) {
		return playbackMediaContent{
			reader: opened, close: opened.Close,
			modTime: time.Unix(100, 0), contentType: "video/mp4",
		}, nil
	}, http.NotFoundHandler())
	request := httptest.NewRequest(http.MethodGet, playbackMediaPathPrefix+"019aa000-0000-7000-8000-000000000001", nil)
	request.Header.Set("Range", "bytes=2-5")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusPartialContent || response.Body.String() != "2345" {
		t.Fatalf("range response = %d %q", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Range") != "bytes 2-5/10" ||
		response.Header().Get("Cache-Control") != "no-store" || !opened.closed {
		t.Fatalf("range headers/close = %#v closed=%v", response.Header(), opened.closed)
	}
}

func TestPlaybackMediaHandlerRejectsInvalidRequestsWithoutLeaks(t *testing.T) {
	openCalls := 0
	handler := newPlaybackMediaHandler(func(context.Context, string) (playbackMediaContent, error) {
		openCalls++
		return playbackMediaContent{}, playback.ErrMediaUnavailable
	}, http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusTeapot)
	}))

	for _, test := range []struct {
		method, target string
		status         int
	}{
		{http.MethodGet, "/index.html", http.StatusTeapot},
		{http.MethodPost, playbackMediaPathPrefix + "id", http.StatusMethodNotAllowed},
		{http.MethodGet, playbackMediaPathPrefix + "nested/id", http.StatusNotFound},
		{http.MethodGet, playbackMediaPathPrefix + "id?token=secret", http.StatusNotFound},
		{http.MethodGet, playbackMediaPathPrefix + "id", http.StatusConflict},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(test.method, test.target, nil))
		if response.Code != test.status {
			t.Fatalf("%s %s status = %d, want %d", test.method, test.target, response.Code, test.status)
		}
		if bytes.Contains(response.Body.Bytes(), []byte("secret")) || bytes.Contains(response.Body.Bytes(), []byte("playback media")) {
			t.Fatalf("response leaked internal detail: %q", response.Body.String())
		}
	}
	if openCalls != 1 {
		t.Fatalf("open calls = %d, want 1", openCalls)
	}

	notFoundHandler := newPlaybackMediaHandler(func(context.Context, string) (playbackMediaContent, error) {
		return playbackMediaContent{}, errors.Join(playback.ErrMediaNotFound, io.EOF)
	}, http.NotFoundHandler())
	response := httptest.NewRecorder()
	notFoundHandler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, playbackMediaPathPrefix+"id", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("not found status = %d", response.Code)
	}
}
