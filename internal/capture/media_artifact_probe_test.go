package capture

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

const (
	validASRArtifactProbeJSON      = `{"streams":[{"index":0,"codec_name":"pcm_s16le","codec_type":"audio","sample_rate":"16000","channels":1}],"format":{"format_name":"wav"}}`
	validPlaybackArtifactProbeJSON = `{"streams":[{"index":0,"codec_name":"h264","codec_type":"video"},{"index":1,"codec_name":"aac","codec_type":"audio","sample_rate":"48000","channels":2}],"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2"}}`
	validArtifactActivityProbeJSON = `{"packets":[{"stream_index":0,"pts_time":"0.000000","dts_time":"0.000000","duration_time":"0.064000"}]}`
)

func TestDecodeMediaArtifactProbeOutputAcceptsRequiredProfiles(t *testing.T) {
	for _, test := range []struct {
		name    string
		kind    MediaArtifactKind
		payload string
	}{
		{name: "asr", kind: MediaArtifactASRWAV, payload: validASRArtifactProbeJSON},
		{name: "playback with audio", kind: MediaArtifactPlaybackMP4, payload: validPlaybackArtifactProbeJSON},
		{name: "playback without audio", kind: MediaArtifactPlaybackMP4,
			payload: `{"streams":[{"index":0,"codec_name":"h264","codec_type":"video"}],"format":{"format_name":"mov,mp4"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := decodeMediaArtifactProbeOutput([]byte(test.payload), test.kind); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDecodeMediaArtifactProbeOutputRejectsWrongProfiles(t *testing.T) {
	tests := []struct {
		name    string
		kind    MediaArtifactKind
		payload string
	}{
		{name: "wav rate", kind: MediaArtifactASRWAV,
			payload: `{"streams":[{"index":0,"codec_name":"pcm_s16le","codec_type":"audio","sample_rate":"48000","channels":1}],"format":{"format_name":"wav"}}`},
		{name: "wav channels", kind: MediaArtifactASRWAV,
			payload: `{"streams":[{"index":0,"codec_name":"pcm_s16le","codec_type":"audio","sample_rate":"16000","channels":2}],"format":{"format_name":"wav"}}`},
		{name: "wav codec", kind: MediaArtifactASRWAV,
			payload: `{"streams":[{"index":0,"codec_name":"pcm_s24le","codec_type":"audio","sample_rate":"16000","channels":1}],"format":{"format_name":"wav"}}`},
		{name: "mp4 video codec", kind: MediaArtifactPlaybackMP4,
			payload: `{"streams":[{"index":0,"codec_name":"hevc","codec_type":"video"}],"format":{"format_name":"mov,mp4"}}`},
		{name: "mp4 audio codec", kind: MediaArtifactPlaybackMP4,
			payload: `{"streams":[{"index":0,"codec_name":"h264","codec_type":"video"},{"index":1,"codec_name":"opus","codec_type":"audio"}],"format":{"format_name":"mov,mp4"}}`},
		{name: "mp4 missing video", kind: MediaArtifactPlaybackMP4,
			payload: `{"streams":[{"index":0,"codec_name":"aac","codec_type":"audio"}],"format":{"format_name":"mov,mp4"}}`},
		{name: "wrong container", kind: MediaArtifactPlaybackMP4,
			payload: `{"streams":[{"index":0,"codec_name":"h264","codec_type":"video"}],"format":{"format_name":"matroska"}}`},
		{name: "duplicate index", kind: MediaArtifactPlaybackMP4,
			payload: `{"streams":[{"index":0,"codec_name":"h264","codec_type":"video"},{"index":0,"codec_name":"aac","codec_type":"audio"}],"format":{"format_name":"mov,mp4"}}`},
		{name: "trailing json", kind: MediaArtifactASRWAV, payload: validASRArtifactProbeJSON + `{}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := decodeMediaArtifactProbeOutput([]byte(test.payload), test.kind); !errors.Is(err, ErrMediaArtifactFailed) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestInspectMediaArtifactUsesBoundedSafeProbe(t *testing.T) {
	directory := t.TempDir()
	ffprobePath := filepath.Join(directory, "ffprobe-private.exe")
	artifactPath := filepath.Join(directory, "proxy.wav")
	for _, path := range []string{ffprobePath, artifactPath} {
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	wantArgs := []string{
		"-v", "error", "-show_entries", mediaArtifactProbeShowEntries,
		"-of", "json", "-i", artifactPath,
	}
	wantActivityArgs := []string{
		"-v", "error", "-select_streams", "0", "-show_packets",
		"-show_entries", segmentActivityProbeShowEntries,
		"-read_intervals", "%+#1", "-of", "json", "-i", artifactPath,
	}
	var phases []mediaArtifactProbePhase
	dependencies := mediaArtifactProbeDependencies{
		timeout: time.Second,
		run: func(
			_ context.Context,
			invocation mediaArtifactProbeInvocation,
			stdout io.Writer,
			_ io.Writer,
		) error {
			if invocation.executablePath != ffprobePath || invocation.artifactPath != artifactPath {
				t.Fatalf("unsafe probe invocation: %#v", invocation)
			}
			phases = append(phases, invocation.phase)
			switch invocation.phase {
			case mediaArtifactProbePhaseMetadata:
				if !reflect.DeepEqual(invocation.args, wantArgs) {
					t.Fatalf("unsafe metadata invocation: %#v", invocation)
				}
				_, err := io.WriteString(stdout, validASRArtifactProbeJSON)
				return err
			case mediaArtifactProbePhaseActivity:
				if invocation.streamIndex != 0 || !reflect.DeepEqual(invocation.args, wantActivityArgs) {
					t.Fatalf("unsafe activity invocation: %#v", invocation)
				}
				_, err := io.WriteString(stdout, validArtifactActivityProbeJSON)
				return err
			default:
				t.Fatalf("unexpected phase: %#v", invocation)
				return nil
			}
		},
	}
	if err := inspectMediaArtifactWithDependencies(
		context.Background(), ffprobePath, artifactPath, MediaArtifactASRWAV, dependencies,
	); err != nil {
		t.Fatal(err)
	}
	if want := []mediaArtifactProbePhase{mediaArtifactProbePhaseMetadata, mediaArtifactProbePhaseActivity}; !reflect.DeepEqual(phases, want) {
		t.Fatalf("probe phases = %v, want %v", phases, want)
	}

	dependencies.outputBytes = 8
	if err := inspectMediaArtifactWithDependencies(
		context.Background(), ffprobePath, artifactPath, MediaArtifactASRWAV, dependencies,
	); !errors.Is(err, ErrMediaArtifactFailed) {
		t.Fatalf("overflow error = %v", err)
	}
}

func TestInspectMediaArtifactBoundsRuntimeAndPropagatesCallerCancellation(t *testing.T) {
	directory := t.TempDir()
	ffprobePath := filepath.Join(directory, "ffprobe-private.exe")
	artifactPath := filepath.Join(directory, "proxy.wav")
	for _, path := range []string{ffprobePath, artifactPath} {
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dependencies := mediaArtifactProbeDependencies{
		timeout: 10 * time.Millisecond,
		run: func(ctx context.Context, _ mediaArtifactProbeInvocation, _, _ io.Writer) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	if err := inspectMediaArtifactWithDependencies(
		context.Background(), ffprobePath, artifactPath, MediaArtifactASRWAV, dependencies,
	); !errors.Is(err, ErrMediaArtifactFailed) {
		t.Fatalf("timeout error = %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := inspectMediaArtifactWithDependencies(
		canceled, ffprobePath, artifactPath, MediaArtifactASRWAV, dependencies,
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation error = %v", err)
	}
}

func TestDecodeMediaArtifactActivityProbeRequiresMatchingPositivePacket(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantErr bool
	}{
		{name: "positive duration", payload: validArtifactActivityProbeJSON},
		{name: "positive timestamp", payload: `{"packets":[{"stream_index":0,"pts_time":"0.125000","duration_time":"0"}]}`},
		{name: "zero packets", payload: `{"packets":[]}`, wantErr: true},
		{name: "wrong stream", payload: `{"packets":[{"stream_index":1,"pts_time":"0","duration_time":"0.064"}]}`, wantErr: true},
		{name: "zero activity", payload: `{"packets":[{"stream_index":0,"pts_time":"0","dts_time":"0","duration_time":"0"}]}`, wantErr: true},
		{name: "multiple packets", payload: `{"packets":[{"stream_index":0,"duration_time":"0.064"},{"stream_index":0,"duration_time":"0.064"}]}`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := decodeMediaArtifactActivityProbeOutput([]byte(test.payload), 0)
			if test.wantErr && !errors.Is(err, ErrMediaArtifactFailed) {
				t.Fatalf("error = %v, want ErrMediaArtifactFailed", err)
			}
			if !test.wantErr && err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInspectMediaArtifactSelectsProfileStreamAndSharesOutputBudget(t *testing.T) {
	directory := t.TempDir()
	ffprobePath := filepath.Join(directory, "ffprobe-private.exe")
	artifactPath := filepath.Join(directory, "proxy.mp4")
	for _, path := range []string{ffprobePath, artifactPath} {
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	metadata := `{"streams":[{"index":2,"codec_name":"aac","codec_type":"audio","sample_rate":"48000","channels":2},{"index":7,"codec_name":"h264","codec_type":"video"}],"format":{"format_name":"mov,mp4"}}`
	activity := `{"packets":[{"stream_index":7,"pts_time":"0","duration_time":"0.040000"}]}`
	var calls int
	dependencies := mediaArtifactProbeDependencies{
		timeout:     time.Second,
		outputBytes: len(metadata) + len(activity) - 1,
		run: func(_ context.Context, invocation mediaArtifactProbeInvocation, stdout, _ io.Writer) error {
			calls++
			if invocation.phase == mediaArtifactProbePhaseMetadata {
				_, err := io.WriteString(stdout, metadata)
				return err
			}
			if invocation.phase != mediaArtifactProbePhaseActivity || invocation.streamIndex != 7 {
				t.Fatalf("activity did not select video stream: %#v", invocation)
			}
			_, err := io.WriteString(stdout, activity)
			return err
		},
	}
	if err := inspectMediaArtifactWithDependencies(
		context.Background(), ffprobePath, artifactPath, MediaArtifactPlaybackMP4, dependencies,
	); !errors.Is(err, ErrMediaArtifactFailed) {
		t.Fatalf("shared output overflow error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("probe calls = %d, want both bounded phases", calls)
	}
	dependencies.outputBytes++
	if err := inspectMediaArtifactWithDependencies(
		context.Background(), ffprobePath, artifactPath, MediaArtifactPlaybackMP4, dependencies,
	); err != nil {
		t.Fatalf("exact shared output budget failed: %v", err)
	}
}

func TestP3MediaRealArtifactProbeRejectsEmptyWAVAndAcceptsActivity(t *testing.T) {
	if os.Getenv("P3MEDIA_REAL_FFPROBE") != "1" {
		t.Skip("set P3MEDIA_REAL_FFPROBE=1 to run real artifact activity probes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tools, err := discoverFFmpeg(ctx, ffmpegDiscoveryOptions{})
	if errors.Is(err, ErrFFmpegNotFound) {
		t.Skip("FFmpeg/ffprobe are unavailable")
	}
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	emptyWAV := filepath.Join(directory, "empty.wav")
	generateEmpty := exec.CommandContext(ctx, tools.ffmpegPath,
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "anullsrc=r=16000:cl=mono",
		"-frames:a", "0", "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", emptyWAV,
	)
	if output, generateErr := generateEmpty.CombinedOutput(); generateErr != nil {
		t.Fatalf("generate empty WAV: %v (%s)", generateErr, redactFFmpegText(string(output)))
	}
	before := segmentProbeFileDigest(t, emptyWAV)
	if err := inspectMediaArtifact(ctx, tools.ffprobePath, emptyWAV, MediaArtifactASRWAV); !errors.Is(err, ErrMediaArtifactFailed) {
		t.Fatalf("empty WAV inspection error = %v", err)
	}
	if after := segmentProbeFileDigest(t, emptyWAV); after != before {
		t.Fatal("empty WAV probe changed the file")
	}

	validWAV := filepath.Join(directory, "active.wav")
	generateWAV := exec.CommandContext(ctx, tools.ffmpegPath,
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=880:sample_rate=16000",
		"-t", "0.2", "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", validWAV,
	)
	if output, generateErr := generateWAV.CombinedOutput(); generateErr != nil {
		t.Fatalf("generate active WAV: %v (%s)", generateErr, redactFFmpegText(string(output)))
	}
	if err := inspectMediaArtifact(ctx, tools.ffprobePath, validWAV, MediaArtifactASRWAV); err != nil {
		t.Fatalf("active WAV inspection: %v", err)
	}

	validMP4 := filepath.Join(directory, "active.mp4")
	generateMP4 := exec.CommandContext(ctx, tools.ffmpegPath,
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=25",
		"-t", "0.2", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-an", validMP4,
	)
	if output, generateErr := generateMP4.CombinedOutput(); generateErr != nil {
		t.Fatalf("generate active MP4: %v (%s)", generateErr, redactFFmpegText(string(output)))
	}
	if err := inspectMediaArtifact(ctx, tools.ffprobePath, validMP4, MediaArtifactPlaybackMP4); err != nil {
		t.Fatalf("active MP4 inspection: %v", err)
	}
}

func TestP3MediaRealArtifactFinalizerRejectsZeroPacketAudio(t *testing.T) {
	if os.Getenv("P3MEDIA_REAL_FFPROBE") != "1" {
		t.Skip("set P3MEDIA_REAL_FFPROBE=1 to run the real zero-packet audio finalizer regression")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tools, err := discoverFFmpeg(ctx, ffmpegDiscoveryOptions{})
	if errors.Is(err, ErrFFmpegNotFound) {
		t.Skip("FFmpeg/ffprobe are unavailable")
	}
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	emptyAudioInput := filepath.Join(root, "empty-input.wav")
	generateEmptyAudio := exec.CommandContext(ctx, tools.ffmpegPath,
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "anullsrc=r=16000:cl=mono",
		"-frames:a", "0", "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", emptyAudioInput,
	)
	if output, generateErr := generateEmptyAudio.CombinedOutput(); generateErr != nil {
		t.Fatalf("generate empty audio input: %v (%s)", generateErr, redactFFmpegText(string(output)))
	}
	sourcePath := filepath.Join(root, "video-with-empty-audio.mkv")
	generateSource := exec.CommandContext(ctx, tools.ffmpegPath,
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=25",
		"-i", emptyAudioInput,
		"-t", "0.2", "-map", "0:v:0", "-map", "1:a:0",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "copy",
		"-f", "matroska", sourcePath,
	)
	if output, generateErr := generateSource.CombinedOutput(); generateErr != nil {
		t.Fatalf("generate video plus zero-packet audio source: %v (%s)",
			generateErr, redactFFmpegText(string(output)))
	}
	prober, err := newFFprobeSegmentProber(tools)
	if err != nil {
		t.Fatal(err)
	}
	probeResult, err := prober.Probe(ctx, sourcePath)
	if err != nil || probeResult.Readability != SegmentReadabilityReadable {
		t.Fatalf("video activity source = (%#v, %v)", probeResult, err)
	}
	hasAudio := false
	hasVideo := false
	for _, stream := range probeResult.Streams {
		hasAudio = hasAudio || stream.Type == "audio"
		hasVideo = hasVideo || stream.Type == "video"
	}
	if !hasAudio || !hasVideo {
		t.Fatalf("source streams = %#v, want declared video and audio", probeResult.Streams)
	}

	segmentID := newV7(t)
	artifactPath := filepath.Join(root, "generated-empty.wav")
	finalizer := sqliteSessionMediaFinalizer{
		tools: tools, root: root, proxyCapacity: make(chan struct{}, 1),
		dependencies: defaultMediaFinalizerDependencies(),
	}
	artifacts, warnings, err := finalizer.generatePendingArtifacts(ctx, MediaSnapshot{
		Segments: []MediaSegment{{
			ID: segmentID, RelativePath: "video-with-empty-audio.mkv",
			VideoCodec: "h264", AudioCodec: "pcm_s16le", Status: MediaSegmentComplete,
		}},
		Artifacts: []MediaArtifact{{
			ID: newV7(t), MediaSegmentID: segmentID, Kind: MediaArtifactASRWAV,
			RelativePath: "generated-empty.wav", Status: MediaArtifactPending,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || artifacts[0].Status == MediaArtifactComplete ||
		artifacts[0].Status != MediaArtifactFailed || artifacts[0].ErrorCode != "MEDIA_ARTIFACT_FAILED" ||
		!containsMediaWarning(warnings, "MEDIA_ARTIFACT_FAILED") {
		t.Fatalf("zero-packet audio artifact reached complete: artifacts=%#v warnings=%v", artifacts, warnings)
	}
	info, err := os.Stat(artifactPath)
	if err != nil || info.Size() <= 0 || info.Size() >= 1024 {
		t.Fatalf("real empty WAV was not preserved: info=%v err=%v", info, err)
	}
	if err := inspectMediaArtifact(ctx, tools.ffprobePath, artifactPath, MediaArtifactASRWAV); !errors.Is(err, ErrMediaArtifactFailed) {
		t.Fatalf("generated empty WAV inspection error = %v", err)
	}
}
