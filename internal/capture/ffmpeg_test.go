package capture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiscoverFFmpegPrefersExplicitAndHashes(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	ffmpegPath := filepath.Join(directory, companionExecutable(directory, "ffmpeg"))
	probePath := filepath.Join(directory, companionExecutable(directory, "ffprobe"))
	ffmpegBody := []byte("trusted-ffmpeg")
	probeBody := []byte("trusted-ffprobe")
	if err := os.WriteFile(ffmpegPath, ffmpegBody, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(probePath, probeBody, 0o700); err != nil {
		t.Fatal(err)
	}

	tools, err := discoverFFmpeg(context.Background(), ffmpegDiscoveryOptions{
		ExplicitFFmpeg: ffmpegPath,
		ExplicitProbe:  probePath,
		LookPath: func(string) (string, error) {
			return "", errors.New("unused fallback")
		},
		RunVersion: fakeFFmpegVersion,
	})
	if err != nil {
		t.Fatalf("discover explicit tools: %v", err)
	}
	if tools.ffmpegPath != ffmpegPath || tools.ffprobePath != probePath {
		t.Fatal("explicit executable pair was not selected")
	}
	wantFFmpegHash := sha256.Sum256(ffmpegBody)
	wantProbeHash := sha256.Sum256(probeBody)
	if tools.FFmpeg.SHA256 != hex.EncodeToString(wantFFmpegHash[:]) || tools.FFprobe.SHA256 != hex.EncodeToString(wantProbeHash[:]) {
		t.Fatal("executable digest mismatch")
	}
	if !strings.HasPrefix(tools.FFmpeg.Version, "ffmpeg version ") || !strings.HasPrefix(tools.FFprobe.Version, "ffprobe version ") {
		t.Fatal("version metadata was not parsed")
	}
}

func TestDiscoverFFmpegFallsBackFromInvalidExplicitPair(t *testing.T) {
	t.Parallel()
	invalidDir := t.TempDir()
	bundledDir := t.TempDir()
	invalidFFmpeg := filepath.Join(invalidDir, companionExecutable(invalidDir, "ffmpeg"))
	invalidProbe := filepath.Join(invalidDir, companionExecutable(invalidDir, "ffprobe"))
	bundledFFmpeg := filepath.Join(bundledDir, companionExecutable(bundledDir, "ffmpeg"))
	bundledProbe := filepath.Join(bundledDir, companionExecutable(bundledDir, "ffprobe"))
	for _, path := range []string{invalidFFmpeg, invalidProbe, bundledFFmpeg, bundledProbe} {
		if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	tools, err := discoverFFmpeg(context.Background(), ffmpegDiscoveryOptions{
		ExplicitFFmpeg: invalidFFmpeg,
		ExplicitProbe:  invalidProbe,
		BundledDir:     bundledDir,
		LookPath: func(string) (string, error) {
			return "", errors.New("missing")
		},
		RunVersion: func(ctx context.Context, path string) ([]byte, error) {
			if filepath.Dir(path) == invalidDir {
				return []byte("not a trusted version response"), nil
			}
			return fakeFFmpegVersion(ctx, path)
		},
	})
	if err != nil {
		t.Fatalf("fall back to bundled pair: %v", err)
	}
	if tools.ffmpegPath != bundledFFmpeg || tools.ffprobePath != bundledProbe {
		t.Fatal("bundled pair was not selected after invalid explicit pair")
	}
}

func TestDiscoverFFmpegStableFailureClassification(t *testing.T) {
	t.Parallel()
	missingLookup := func(string) (string, error) { return "", errors.New("missing private path") }
	_, err := discoverFFmpeg(context.Background(), ffmpegDiscoveryOptions{LookPath: missingLookup})
	if !errors.Is(err, ErrFFmpegNotFound) || strings.Contains(err.Error(), "private") {
		t.Fatal("missing tools must return a stable redacted error")
	}

	directory := t.TempDir()
	ffmpegPath := filepath.Join(directory, companionExecutable(directory, "ffmpeg"))
	probePath := filepath.Join(directory, companionExecutable(directory, "ffprobe"))
	for _, path := range []string{ffmpegPath, probePath} {
		if err := os.WriteFile(path, []byte("invalid"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_, err = discoverFFmpeg(context.Background(), ffmpegDiscoveryOptions{
		ExplicitFFmpeg: ffmpegPath,
		ExplicitProbe:  probePath,
		LookPath:       missingLookup,
		RunVersion: func(context.Context, string) ([]byte, error) {
			return []byte("bad output containing https://secret.invalid/?token=secret"), nil
		},
	})
	if !errors.Is(err, ErrFFmpegInvalid) || strings.Contains(err.Error(), "secret") {
		t.Fatal("invalid tools must return a stable redacted error")
	}
}

func TestDiscoverFFmpegHonorsCancelledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := discoverFFmpeg(ctx, ffmpegDiscoveryOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled discovery returned %v", err)
	}
}

func TestDiscoverFFmpegRejectsRelativeConfiguredPaths(t *testing.T) {
	t.Parallel()
	for _, options := range []ffmpegDiscoveryOptions{
		{ExplicitFFmpeg: "relative/ffmpeg"},
		{ExplicitProbe: "relative/ffprobe"},
		{BundledDir: "relative/bundle"},
	} {
		_, err := discoverFFmpeg(context.Background(), options)
		if !errors.Is(err, ErrFFmpegInvalid) {
			t.Fatalf("relative configured path returned %v", err)
		}
	}
}

func fakeFFmpegVersion(_ context.Context, path string) ([]byte, error) {
	name := "ffmpeg"
	if strings.Contains(strings.ToLower(filepath.Base(path)), "ffprobe") {
		name = "ffprobe"
	}
	return []byte(name + " version 8.1.2-safe\nconfiguration: --enable-safe\n"), nil
}

func TestParseFFmpegVersionRedactsMetadata(t *testing.T) {
	t.Parallel()
	version, build, ok := parseFFmpegVersion([]byte(
		"ffmpeg version 8.1 https://secret.invalid/build?token=private\n"+
			"configuration: --prefix=C:\\Users\\private-user\\ffmpeg --extra-cflags=-I/private/build/include "+
			"--header Authorization: Bearer build-secret\n",
	), "ffmpeg")
	if !ok {
		t.Fatal("version metadata was rejected")
	}
	for _, forbidden := range []string{"secret.invalid", "private", "build-secret", "private-user", "/private/build"} {
		if strings.Contains(version, forbidden) || strings.Contains(build, forbidden) {
			t.Fatalf("version metadata retained %q", forbidden)
		}
	}
	if !strings.Contains(build, "--prefix=<redacted-path>") || !strings.Contains(build, "--extra-cflags=<redacted-path>") {
		t.Fatalf("version metadata did not retain redacted build switches: %q", build)
	}
}

func TestBuildFFmpegRecordingArgs(t *testing.T) {
	t.Parallel()
	const attemptID = "01900000-0000-7000-8000-000000000001"
	pattern, err := newFFmpegOutputPattern(t.TempDir(), time.Date(2026, 7, 17, 12, 34, 56, 123, time.FixedZone("test", 8*60*60)), attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(pattern, "segment-%06d-20260717T043456.000000123Z-"+attemptID+".mkv.partial") {
		t.Fatalf("unexpected safe output pattern %q", filepath.Base(pattern))
	}
	const streamURL = "https://stream.invalid/live.flv?token=top-secret&signature=hidden"
	args, err := buildFFmpegRecordingArgs(ffmpegRecordingSpec{
		InputURL: streamURL, OutputPattern: pattern, SegmentSeconds: 600,
	})
	if err != nil {
		t.Fatalf("build args: %v", err)
	}
	urlCount := 0
	for index, arg := range args {
		if arg != streamURL {
			continue
		}
		urlCount++
		if index == 0 || args[index-1] != "-i" {
			t.Fatal("stream URL appeared outside the dedicated input argument")
		}
	}
	if urlCount != 1 {
		t.Fatal("stream URL must occur exactly once")
	}
	for _, expected := range []string{"-progress", "pipe:1", "-rw_timeout", "15000000", "-c", "copy", "-segment_format", "matroska", "-segment_time", "600"} {
		if !containsArgument(args, expected) {
			t.Fatalf("missing required argument %q", expected)
		}
	}
	timeoutIndex, inputIndex := -1, -1
	for index, argument := range args {
		if argument == "-rw_timeout" {
			timeoutIndex = index
		}
		if argument == "-i" {
			inputIndex = index
		}
	}
	if timeoutIndex < 0 || inputIndex < 0 || timeoutIndex >= inputIndex || args[timeoutIndex+1] != "15000000" {
		t.Fatalf("input timeout is not scoped before -i: %v", args)
	}
}

func TestFFmpegRecordingSpecFormattingIsRedacted(t *testing.T) {
	t.Parallel()
	outputDirectory := filepath.Join(t.TempDir(), "sensitive-output-directory")
	spec := ffmpegRecordingSpec{
		InputURL:       "https://stream.invalid/live?token=private",
		OutputPattern:  filepath.Join(outputDirectory, "private-output-%06d.mkv.partial"),
		SegmentSeconds: 600,
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var logOutput bytes.Buffer
	slog.New(slog.NewJSONHandler(&logOutput, nil)).Info("recording spec", "spec", spec)
	for _, diagnostic := range []string{
		fmt.Sprint(spec), fmt.Sprintf("%+v", spec), fmt.Sprintf("%#v", spec),
		string(encoded), logOutput.String(),
	} {
		for _, forbidden := range []string{"stream.invalid", "private", "sensitive-output-directory"} {
			if strings.Contains(diagnostic, forbidden) {
				t.Fatalf("formatted recording spec retained %q", forbidden)
			}
		}
		if !strings.Contains(diagnostic, "redacted") {
			t.Fatalf("formatted recording spec lacks an explicit redaction marker: %s", diagnostic)
		}
	}
}

func TestBuildFFmpegRecordingArgsRejectsUnsafeInputs(t *testing.T) {
	t.Parallel()
	const attemptID = "01900000-0000-7000-8000-000000000002"
	pattern, err := newFFmpegOutputPattern(t.TempDir(), time.Now(), attemptID)
	if err != nil {
		t.Fatal(err)
	}
	tests := []ffmpegRecordingSpec{
		{InputURL: "ftp://stream.invalid/live", OutputPattern: pattern, SegmentSeconds: 600},
		{InputURL: "https://user:secret@stream.invalid/live", OutputPattern: pattern, SegmentSeconds: 600},
		{InputURL: "https://:443/live", OutputPattern: pattern, SegmentSeconds: 600},
		{InputURL: "https://stream.invalid/live#fragment", OutputPattern: pattern, SegmentSeconds: 600},
		{InputURL: "https://stream.invalid/live", OutputPattern: pattern, SegmentSeconds: 299},
		{InputURL: "https://stream.invalid/live", OutputPattern: pattern, SegmentSeconds: 1801},
		{InputURL: "https://stream.invalid/live", OutputPattern: filepath.Join(t.TempDir(), "unsafe.mkv.partial"), SegmentSeconds: 600},
	}
	for _, test := range tests {
		if _, err := buildFFmpegRecordingArgs(test); !errors.Is(err, ErrFFmpegArguments) {
			t.Fatalf("unsafe recording specification returned %v", err)
		}
	}
	if _, err := newFFmpegOutputPattern(filepath.Join(t.TempDir(), "media%name"), time.Now(), attemptID); !errors.Is(err, ErrFFmpegArguments) {
		t.Fatalf("percent-bearing media directory returned %v", err)
	}
	if _, err := newFFmpegOutputPattern(t.TempDir(), time.Now(), "not-an-attempt-id"); !errors.Is(err, ErrFFmpegArguments) {
		t.Fatalf("invalid attempt id returned %v", err)
	}
}

func TestReadFFmpegProgressBoundedParser(t *testing.T) {
	t.Parallel()
	input := strings.NewReader(strings.Join([]string{
		"frame=7",
		"fps=29.97",
		"total_size=1024",
		"out_time_us=1500000",
		"speed= 1.25x",
		"future_field=https://stream.invalid/live?token=secret",
		"progress=continue",
		"frame=N/A",
		"out_time=00:00:02.250000",
		"speed=N/A",
		"progress=end",
	}, "\n") + "\n")
	var snapshots []FFmpegProgress
	if err := readFFmpegProgress(context.Background(), input, func(progress FFmpegProgress) {
		snapshots = append(snapshots, progress)
	}); err != nil {
		t.Fatalf("parse progress: %v", err)
	}
	if len(snapshots) != 2 {
		t.Fatalf("got %d progress snapshots", len(snapshots))
	}
	if snapshots[0].Frame != 7 || snapshots[0].FPS != 29.97 || snapshots[0].TotalSize != 1024 || snapshots[0].OutTime != 1500*time.Millisecond || snapshots[0].Speed != 1.25 || snapshots[0].State != "continue" {
		t.Fatal("first progress snapshot mismatch")
	}
	if snapshots[1].OutTime != 2250*time.Millisecond || snapshots[1].State != "end" {
		t.Fatal("terminal progress snapshot mismatch")
	}
}

func TestReadFFmpegProgressAcceptsInitialNAClock(t *testing.T) {
	t.Parallel()
	input := strings.NewReader(strings.Join([]string{
		"frame=0", "fps=0.00", "bitrate=N/A", "total_size=N/A",
		"out_time_us=N/A", "out_time_ms=N/A", "out_time=N/A", "speed=N/A", "progress=end",
	}, "\n") + "\n")
	var snapshot FFmpegProgress
	if err := readFFmpegProgress(context.Background(), input, func(progress FFmpegProgress) { snapshot = progress }); err != nil {
		t.Fatalf("parse initial N/A progress: %v", err)
	}
	if snapshot.OutTime != 0 || snapshot.State != "end" {
		t.Fatalf("unexpected initial progress: %#v", snapshot)
	}
}

func TestReadFFmpegProgressRejectsMalformedOrOversizedLines(t *testing.T) {
	t.Parallel()
	for _, input := range []string{
		"frame=not-a-number\nprogress=continue\n",
		"speed=NaNx\nprogress=continue\n",
		"speed=+Infx\nprogress=continue\n",
		"out_time=00:00:NaN\nprogress=continue\n",
		"out_time=00:00:+Inf\nprogress=continue\n",
		"out_time=999999999999999:00:00\nprogress=continue\n",
		"out_time_us=9223372036854775807\nprogress=continue\n",
		"progress=unknown\n",
		strings.Repeat("x", ffmpegProgressLineMax+1) + "=value\n",
	} {
		if err := readFFmpegProgress(context.Background(), strings.NewReader(input), nil); !errors.Is(err, ErrFFmpegProgress) {
			t.Fatalf("malformed progress returned %v", err)
		}
	}
}

func TestBoundedTextBufferKeepsOnlyRedactedTail(t *testing.T) {
	t.Parallel()
	buffer := newBoundedTextBuffer(128)
	_, _ = buffer.Write([]byte("prefix https://stream.invalid/live?token=top-secret\nCookie: a=1; session=private-cookie\nAuthorization: Bearer bearer-secret\nlast-line"))
	snapshot := buffer.Snapshot()
	for _, forbidden := range []string{"stream.invalid", "top-secret", "private-cookie", "bearer-secret"} {
		if strings.Contains(snapshot, forbidden) {
			t.Fatalf("snapshot retained sensitive value %q", forbidden)
		}
	}
	if !strings.Contains(snapshot, "<redacted-stream-url>") || !strings.Contains(snapshot, "<redacted-secret>") || !strings.Contains(snapshot, "last-line") {
		t.Fatal("snapshot did not preserve the expected redacted tail")
	}

	oversized := newBoundedTextBuffer(32)
	_, _ = oversized.Write([]byte("https://stream.invalid/" + strings.Repeat("secret", 32)))
	if snapshot := oversized.Snapshot(); snapshot != "" {
		t.Fatal("a truncated mid-token tail must be discarded")
	}
}

func containsArgument(args []string, expected string) bool {
	for _, arg := range args {
		if arg == expected {
			return true
		}
	}
	return false
}
