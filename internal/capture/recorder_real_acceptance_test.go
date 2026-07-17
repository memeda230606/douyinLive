//go:build p3acceptance

package capture

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	douyinLive "github.com/jwwsjlm/douyinLive/v2"
)

type realRecorderAcceptanceSource struct {
	streams []douyinLive.ResolvedStream
}

func (s *realRecorderAcceptanceSource) ResolveStreams() ([]douyinLive.ResolvedStream, error) {
	return append([]douyinLive.ResolvedStream(nil), s.streams...), nil
}

func (*realRecorderAcceptanceSource) SubscribeMessage(douyinLive.LiveMessageHandler) string {
	return "unused"
}

func (*realRecorderAcceptanceSource) Unsubscribe(string) {}

func TestP3AcceptanceRealFFmpegRecorderWritesReadablePartialAndStops(t *testing.T) {
	testCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	tools, err := discoverFFmpeg(testCtx, ffmpegDiscoveryOptions{})
	if errors.Is(err, ErrFFmpegNotFound) {
		t.Skip("FFmpeg/ffprobe are unavailable")
	}
	if err != nil {
		t.Fatalf("discover verified FFmpeg pair: %v", err)
	}

	workingDirectory := t.TempDir()
	sourcePath := filepath.Join(workingDirectory, "loop-source.ts")
	generate := exec.CommandContext(testCtx, tools.ffmpegPath,
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=25",
		"-f", "lavfi", "-i", "sine=frequency=880:sample_rate=48000",
		"-t", "2", "-c:v", "mpeg2video", "-q:v", "5",
		"-c:a", "mp2", "-f", "mpegts", sourcePath,
	)
	if err := generate.Run(); err != nil {
		t.Fatalf("generate local MPEG-TS fixture: %v", err)
	}
	payload, err := os.ReadFile(sourcePath)
	if err != nil || len(payload) == 0 {
		t.Fatalf("read generated MPEG-TS fixture: size=%d err=%v", len(payload), err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "video/mp2t")
		flusher, _ := writer.(http.Flusher)
		for {
			if _, writeErr := writer.Write(payload); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			select {
			case <-request.Context().Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
		}
	}))
	defer server.Close()
	const secret = "acceptance-stream-secret"
	source := &realRecorderAcceptanceSource{streams: []douyinLive.ResolvedStream{{
		ID: "acceptance", Protocol: "hls", QualityKey: "origin", Codec: "h264",
		URL: server.URL + "/live.ts?token=" + secret,
	}}}

	mediaDirectory := filepath.Join(workingDirectory, "media")
	if err := os.MkdirAll(mediaDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	dependencies := defaultRecorderDependencies()
	dependencies.startupWindow = 5 * time.Second
	var releases atomic.Int32
	recorder, err := newFFmpegRecorder(testCtx, source, recorderOptions{
		tools: tools, mediaDirectory: mediaDirectory,
		segmentSeconds: defaultRecorderSegmentSeconds,
	}, dependencies, func() { releases.Add(1) })
	if err != nil {
		t.Fatalf("start real FFmpeg recorder: %v", err)
	}
	for _, rendered := range []string{fmt.Sprint(recorder), fmt.Sprintf("%#v", recorder)} {
		if strings.Contains(rendered, secret) || strings.Contains(rendered, server.URL) || strings.Contains(rendered, workingDirectory) {
			t.Fatalf("recorder diagnostics expose private input or path")
		}
	}

	partialFiles := waitForRealRecorderPartials(t, mediaDirectory)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 12*time.Second)
	stopErr := recorder.Stop(stopCtx)
	stopCancel()
	if stopErr != nil {
		t.Fatalf("gracefully stop real FFmpeg recorder: %v", stopErr)
	}
	if releases.Load() != 1 {
		t.Fatalf("recorder capacity releases = %d, want 1", releases.Load())
	}
	if _, open := <-recorder.Events(); open {
		t.Fatal("recorder event stream remains open after graceful stop")
	}

	probe := exec.CommandContext(testCtx, tools.ffprobePath,
		"-v", "error", "-show_entries", "format=format_name,duration",
		"-of", "default=noprint_wrappers=1", partialFiles[0],
	)
	probeOutput, err := probe.Output()
	if err != nil {
		t.Fatalf("ffprobe real partial: %v", err)
	}
	if !strings.Contains(strings.ToLower(string(probeOutput)), "matroska") {
		t.Fatalf("real partial is not a readable Matroska container: %s", probeOutput)
	}
}

func waitForRealRecorderPartials(t *testing.T, mediaDirectory string) []string {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		matches, err := filepath.Glob(filepath.Join(mediaDirectory, ".attempt-*", "*.mkv.partial"))
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range matches {
			info, statErr := os.Stat(match)
			if statErr == nil && info.Size() > 0 {
				return matches
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("real FFmpeg recorder did not write a partial segment")
	return nil
}
