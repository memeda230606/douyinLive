//go:build p3acceptance

package capture

import (
	"context"
	"encoding/json"
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
	"github.com/jwwsjlm/douyinLive/v2/internal/storage"
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

func TestP3AcceptanceRealMediaFinalizationProducesRawManifestWAVAndMP4(t *testing.T) {
	testCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tools, err := discoverFFmpeg(testCtx, ffmpegDiscoveryOptions{})
	if errors.Is(err, ErrFFmpegNotFound) {
		t.Skip("FFmpeg/ffprobe are unavailable")
	}
	if err != nil {
		t.Fatalf("discover verified FFmpeg pair: %v", err)
	}

	repository, store, layout, roomID, now := openRepository(t)
	defer store.Close()
	session, err := repository.Create(testCtx, CreateSessionInput{
		RoomConfigID: roomID, OperationID: newV7(t), Recording: RecordingPending,
		StartedAt: now,
	})
	if err != nil {
		t.Fatalf("create acceptance session: %v", err)
	}
	attempt := MediaAttempt{
		ID: newV7(t), Ordinal: 1, StartedAt: now.Add(time.Second).Truncate(time.Millisecond).UnixMilli(),
		SegmentSeconds: defaultRecorderSegmentSeconds, Committed: true, Clean: true,
		VariantID: "acceptance", Protocol: "flv", QualityKey: "origin",
		Quality: "origin", Codec: "h264", Bitrate: 1_000_000,
	}
	sessionDirectory, err := secureMediaSessionDirectory(layout.Root, session.DataPath)
	if err != nil {
		t.Fatal(err)
	}
	attemptDirectory := filepath.Join(sessionDirectory, "media", ".attempt-"+attempt.ID)
	if err := os.MkdirAll(attemptDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	partialPath := filepath.Join(attemptDirectory, mediaAttemptSegmentName(1, attempt))
	generate := exec.CommandContext(testCtx, tools.ffmpegPath,
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=25",
		"-f", "lavfi", "-i", "sine=frequency=880:sample_rate=48000",
		"-t", "2", "-shortest", "-c:v", "libx264", "-preset", "ultrafast",
		"-pix_fmt", "yuv420p", "-c:a", "aac", "-f", "matroska", partialPath,
	)
	configureMediaCommand(generate)
	if output, err := generate.CombinedOutput(); err != nil {
		t.Fatalf("generate real Matroska fixture: %v: %s", err, output)
	}

	finalizer, err := newSQLiteSessionMediaFinalizer(testCtx, sessionMediaFinalizerOptions{
		Repository: repository, Tools: tools, Root: layout.Root,
		SessionID: session.ID, RelativePath: session.DataPath, StartedAt: session.StartedAt,
		ProxyCapacity: make(chan struct{}, 1),
	})
	if err != nil {
		t.Fatalf("open real media finalizer: %v", err)
	}
	result, err := finalizer.Finalize(testCtx, []MediaAttempt{attempt})
	if err != nil {
		t.Fatalf("finalize real media: %v", err)
	}
	if result.Snapshot.Session.State != SessionMediaCompleted ||
		result.Snapshot.Session.ManifestDirty || len(result.Snapshot.Segments) != 1 ||
		len(result.Snapshot.Artifacts) != 2 {
		t.Fatalf("unexpected real finalization snapshot: %v", result.Snapshot)
	}
	segment := result.Snapshot.Segments[0]
	if segment.Status != MediaSegmentComplete || segment.Container != "mkv" ||
		segment.VideoCodec != "h264" || segment.AudioCodec != "aac" ||
		segment.SizeBytes == 0 || len(segment.SHA256) != 64 {
		t.Fatalf("unexpected real raw segment: %v", segment)
	}
	if _, err := os.Stat(partialPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("real partial was not promoted: %v", err)
	}
	rawPath, err := mediaAbsolutePath(layout.Root, segment.RelativePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rawPath); err != nil {
		t.Fatalf("final raw Matroska missing: %v", err)
	}
	kinds := make(map[MediaArtifactKind]bool, 2)
	for _, artifact := range result.Snapshot.Artifacts {
		if artifact.Status != MediaArtifactComplete || artifact.SizeBytes == 0 ||
			len(artifact.SHA256) != 64 || artifact.SourceSHA256 != segment.SHA256 {
			t.Fatalf("unexpected real artifact: %v", artifact)
		}
		artifactPath, pathErr := mediaAbsolutePath(layout.Root, artifact.RelativePath)
		if pathErr != nil {
			t.Fatal(pathErr)
		}
		if inspectErr := inspectMediaArtifact(testCtx, tools.ffprobePath, artifactPath, artifact.Kind); inspectErr != nil {
			t.Fatalf("inspect real %s artifact: %v", artifact.Kind, inspectErr)
		}
		kinds[artifact.Kind] = true
	}
	if !kinds[MediaArtifactASRWAV] || !kinds[MediaArtifactPlaybackMP4] {
		t.Fatalf("real artifact kinds = %v", kinds)
	}
	manifestPath := filepath.Join(sessionDirectory, "manifests", "media.json")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil || len(manifest) == 0 {
		t.Fatalf("real media manifest is not durable: size=%d err=%v", len(manifest), err)
	}
	var decodedManifest mediaManifest
	if err := json.Unmarshal(manifest, &decodedManifest); err != nil ||
		decodedManifest.Session.State != SessionMediaCompleted {
		t.Fatalf("real media manifest is invalid or incomplete: %v", err)
	}
	for _, secret := range []string{layout.Root, tools.ffmpegPath, tools.ffprobePath, "https://"} {
		if strings.Contains(string(manifest), secret) {
			t.Fatal("real media manifest exposed a protected value")
		}
	}
}

func TestP3AcceptanceProductionRecorderFactoryPersistsInternalAndExternalMedia(t *testing.T) {
	testCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tools, err := discoverFFmpeg(testCtx, ffmpegDiscoveryOptions{})
	if errors.Is(err, ErrFFmpegNotFound) {
		t.Skip("FFmpeg/ffprobe are unavailable")
	}
	if err != nil {
		t.Fatalf("discover verified FFmpeg pair: %v", err)
	}

	fixtureDirectory := t.TempDir()
	fixturePath := filepath.Join(fixtureDirectory, "factory-loop-source.ts")
	generate := exec.CommandContext(testCtx, tools.ffmpegPath,
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=25",
		"-f", "lavfi", "-i", "sine=frequency=880:sample_rate=48000",
		"-t", "2", "-shortest", "-c:v", "libx264", "-preset", "ultrafast",
		"-pix_fmt", "yuv420p", "-c:a", "aac", "-f", "mpegts", fixturePath,
	)
	configureMediaCommand(generate)
	if output, runErr := generate.CombinedOutput(); runErr != nil {
		t.Fatalf("generate factory MPEG-TS fixture: %v: %s", runErr, output)
	}
	payload, err := os.ReadFile(fixturePath)
	if err != nil || len(payload) == 0 {
		t.Fatalf("read factory MPEG-TS fixture: size=%d err=%v", len(payload), err)
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
			case <-time.After(200 * time.Millisecond):
			}
		}
	}))
	defer server.Close()

	shortDataRoot, err := os.MkdirTemp("", "p3-factory-e2e-")
	if err != nil {
		t.Fatalf("create short data root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortDataRoot) })
	layout, err := storage.PrepareLayout(shortDataRoot)
	if err != nil {
		t.Fatalf("prepare short data layout: %v", err)
	}
	store, err := storage.Open(testCtx, layout, storage.OpenOptions{})
	if err != nil {
		t.Fatalf("open short data store: %v", err)
	}
	defer store.Close()
	now := time.Date(2026, 7, 17, 8, 30, 0, 0, time.UTC)
	repository, err := newSQLiteRepository(
		store.Writer(), store.Reader(), layout.Root, func() time.Time { return now },
	)
	if err != nil {
		t.Fatalf("open short data repository: %v", err)
	}
	roomID := insertRoom(t, store, "factory-internal")
	externalRoot, err := os.MkdirTemp("", "p3-factory-recordings-")
	if err != nil {
		t.Fatalf("create short external root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(externalRoot) })
	testCases := []struct {
		name          string
		recordingRoot string
		wantRoot      string
		external      bool
		roomID        string
	}{
		{name: "internal", recordingRoot: layout.RoomsDir, wantRoot: layout.Root, roomID: roomID},
		{name: "external", recordingRoot: externalRoot, wantRoot: externalRoot, roomID: insertRoom(t, store, "factory-external"), external: true},
	}
	for index, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			session, createErr := repository.Create(testCtx, CreateSessionInput{
				RoomConfigID: testCase.roomID, OperationID: newV7(t), Recording: RecordingPending,
				StartedAt: now.Add(time.Duration(index) * time.Second),
			})
			if createErr != nil {
				t.Fatalf("create factory session: %v", createErr)
			}
			dependencies := defaultRecorderDependencies()
			dependencies.startupWindow = 5 * time.Second
			factory, _, factoryErr := newFFmpegRecorderFactoryWithTools(FFmpegRecorderFactoryOptions{
				DataRoot: layout.Root, RecordingRoot: testCase.recordingRoot,
				Repository: repository, MaxConcurrentRecordings: 1,
			}, tools, dependencies)
			if factoryErr != nil {
				t.Fatalf("new production recorder factory: %v", factoryErr)
			}
			const secret = "factory-e2e-stream-secret"
			source := &realRecorderAcceptanceSource{streams: []douyinLive.ResolvedStream{{
				ID: "factory-e2e", Protocol: "hls", QualityKey: "origin", Quality: "原画",
				Codec: "h264", Bitrate: 1_000_000,
				URL: server.URL + "/live.ts?token=" + secret,
			}}}
			recorder, startErr := factory(testCtx, session, OpenRequest{
				Profile: RecordingProfile{SegmentMinutes: 5},
			}, source)
			if startErr != nil {
				t.Fatalf("start production recorder: %v", startErr)
			}
			relativePath := session.DataPath
			if testCase.external {
				var ok bool
				relativePath, ok = strings.CutPrefix(relativePath, "rooms/")
				if !ok {
					t.Fatalf("session path lacks rooms prefix: %q", session.DataPath)
				}
			}
			sessionDirectory, pathErr := secureMediaSessionDirectory(testCase.wantRoot, relativePath)
			if pathErr != nil {
				t.Fatalf("resolve factory media directory: %v", pathErr)
			}
			waitForRealRecorderPartials(t, filepath.Join(sessionDirectory, "media"))
			stopCtx, stopCancel := context.WithTimeout(testCtx, 45*time.Second)
			stopErr := recorder.Stop(stopCtx)
			stopCancel()
			if stopErr != nil {
				t.Fatalf("stop production recorder: %v", stopErr)
			}

			snapshot, loadErr := repository.LoadSnapshot(testCtx, session.ID)
			if loadErr != nil {
				t.Fatalf("load production media snapshot: %v", loadErr)
			}
			if snapshot.Session.State != SessionMediaCompleted || snapshot.Session.ManifestDirty ||
				len(snapshot.Session.Attempts) != 1 || !snapshot.Session.Attempts[0].Committed ||
				!snapshot.Session.Attempts[0].Clean || len(snapshot.Segments) == 0 ||
				len(snapshot.Artifacts) != 2 {
				t.Fatalf("production media snapshot = %v", snapshot)
			}
			if testCase.external && snapshot.Session.RootID == nil {
				t.Fatal("external production snapshot lacks registered root")
			}
			if !testCase.external && snapshot.Session.RootID != nil {
				t.Fatalf("internal production snapshot has root ID %q", *snapshot.Session.RootID)
			}
			kinds := make(map[MediaArtifactKind]bool, 2)
			for _, artifact := range snapshot.Artifacts {
				if artifact.Status != MediaArtifactComplete || artifact.SizeBytes == 0 ||
					len(artifact.SHA256) != 64 {
					t.Fatalf("production artifact = %v", artifact)
				}
				artifactPath, pathErr := mediaAbsolutePath(testCase.wantRoot, artifact.RelativePath)
				if pathErr != nil {
					t.Fatal(pathErr)
				}
				if inspectErr := inspectMediaArtifact(testCtx, tools.ffprobePath, artifactPath, artifact.Kind); inspectErr != nil {
					t.Fatalf("inspect production %s artifact: %v", artifact.Kind, inspectErr)
				}
				kinds[artifact.Kind] = true
			}
			if !kinds[MediaArtifactASRWAV] || !kinds[MediaArtifactPlaybackMP4] {
				t.Fatalf("production artifact kinds = %v", kinds)
			}
			manifest, readErr := os.ReadFile(filepath.Join(sessionDirectory, "manifests", "media.json"))
			if readErr != nil || len(manifest) == 0 {
				t.Fatalf("read production media manifest: size=%d err=%v", len(manifest), readErr)
			}
			var decoded mediaManifest
			if decodeErr := json.Unmarshal(manifest, &decoded); decodeErr != nil ||
				decoded.Session.State != SessionMediaCompleted || len(decoded.Artifacts) != 2 ||
				len(decoded.Attempts) != 1 ||
				!decoded.Attempts[0].Committed || !decoded.Attempts[0].Clean {
				t.Fatalf("production media manifest invalid: state=%s artifacts=%d err=%v",
					decoded.Session.State, len(decoded.Artifacts), decodeErr)
			}
			for _, forbidden := range []string{secret, server.URL, tools.ffmpegPath, tools.ffprobePath} {
				if strings.Contains(string(manifest), forbidden) {
					t.Fatalf("production media manifest leaked protected input")
				}
			}
		})
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
