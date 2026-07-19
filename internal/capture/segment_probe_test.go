package capture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validSegmentProbeJSON = `{
  "streams": [
    {
      "index": 2,
      "codec_name": "aac",
      "codec_type": "audio",
      "time_base": "1/48000",
      "start_pts": 24000,
      "duration_ts": 456000,
      "sample_rate": "48000",
      "channels": 2,
      "bit_rate": "128000"
    },
    {
      "index": 0,
      "codec_name": "h264",
      "codec_type": "video",
      "time_base": "1/90000",
      "start_pts": 90000,
      "duration_ts": 450000,
      "width": 1920,
      "height": 1080
    }
  ],
  "format": {
    "format_name": "matroska,webm",
    "start_time": "0.500000",
    "duration": "10.000000"
  }
}`

func TestFFprobeSegmentProberReturnsBoundedReadableMetadata(t *testing.T) {
	captured := make([]segmentProbeInvocation, 0, 2)
	prober, partialPath := newSegmentProbeTestProber(t, func(
		_ context.Context,
		invocation segmentProbeInvocation,
		stdout io.Writer,
		_ io.Writer,
	) error {
		captured = append(captured, invocation)
		output := validSegmentProbeJSON
		if invocation.phase == segmentProbePhaseActivity {
			output = validSegmentActivityProbeJSON(invocation.streamIndex)
		}
		_, err := io.WriteString(stdout, output)
		return err
	}, segmentProbeDependencies{})

	before := segmentProbeFileDigest(t, partialPath)
	result, err := prober.Probe(context.Background(), partialPath)
	if err != nil {
		t.Fatalf("probe readable partial: %v", err)
	}
	if result.Readability != SegmentReadabilityReadable || result.Container != "matroska,webm" {
		t.Fatalf("unexpected probe result: %#v", result)
	}
	if valueOf(result.DurationUs) != 10_000_000 ||
		valueOf(result.FirstTimestampUs) != 500_000 ||
		valueOf(result.LastTimestampUs) != 10_500_000 {
		t.Fatalf("unexpected top-level timing: %#v", result)
	}
	if len(result.Streams) != 2 || result.Streams[0].Index != 0 || result.Streams[1].Index != 2 {
		t.Fatalf("streams were not stable and sorted: %#v", result.Streams)
	}
	video := result.Streams[0]
	if video.Type != "video" || video.Codec != "h264" || video.Width != 1920 || video.Height != 1080 ||
		valueOf(video.StartTimestampUs) != 1_000_000 || valueOf(video.DurationUs) != 5_000_000 ||
		valueOf(video.EndTimestampUs) != 6_000_000 {
		t.Fatalf("unexpected video metadata: %#v", video)
	}
	audio := result.Streams[1]
	if audio.Type != "audio" || audio.Codec != "aac" || audio.SampleRate != 48_000 ||
		audio.Channels != 2 || audio.BitRate != 128_000 ||
		valueOf(audio.StartTimestampUs) != 500_000 || valueOf(audio.DurationUs) != 9_500_000 ||
		valueOf(audio.EndTimestampUs) != 10_000_000 {
		t.Fatalf("unexpected audio metadata: %#v", audio)
	}
	if after := segmentProbeFileDigest(t, partialPath); after != before {
		t.Fatal("successful probe changed the partial bytes")
	}
	if len(captured) != 2 {
		t.Fatalf("ffprobe invocations = %d, want metadata + activity", len(captured))
	}
	assertSegmentProbeInvocation(t, captured[0], partialPath)
	assertSegmentActivityInvocation(t, captured[1], partialPath, 0)
	for _, invocation := range captured {
		assertSegmentProbePrivateFormatting(t, prober, invocation, partialPath)
	}

	rendered, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if bytes.Contains(rendered, []byte(partialPath)) || bytes.Contains(rendered, []byte("https://")) {
		t.Fatalf("result JSON exposed private provenance: %s", rendered)
	}
}

func TestFFprobeSegmentProberAcceptsFinalMKVForIdempotentRecovery(t *testing.T) {
	prober, partialPath := newSegmentProbeTestProber(t, writeSegmentProbeJSON(validSegmentProbeJSON), segmentProbeDependencies{})
	finalPath := strings.TrimSuffix(partialPath, ".partial")
	if err := os.Rename(partialPath, finalPath); err != nil {
		t.Fatal(err)
	}
	before := segmentProbeFileDigest(t, finalPath)
	result, err := prober.Probe(context.Background(), finalPath)
	if err != nil || result.Readability != SegmentReadabilityReadable {
		t.Fatalf("probe final MKV: result=%#v err=%v", result, err)
	}
	if after := segmentProbeFileDigest(t, finalPath); after != before {
		t.Fatal("probe changed the final MKV bytes")
	}
}

func TestFFprobeSegmentProberRejectsDeclaredStreamsWithoutPackets(t *testing.T) {
	metadata := `{
		"streams":[{"index":0,"codec_type":"video","codec_name":"h264",
			"time_base":"1/1000","start_time":"N/A","duration":"N/A"}],
		"format":{"format_name":"matroska","start_time":"N/A","duration":"10.000000"}
	}`
	prober, partialPath := newSegmentProbeTestProber(t, func(
		_ context.Context,
		invocation segmentProbeInvocation,
		stdout io.Writer,
		_ io.Writer,
	) error {
		output := metadata
		if invocation.phase == segmentProbePhaseActivity {
			output = `{"packets":[]}`
		}
		_, err := io.WriteString(stdout, output)
		return err
	}, segmentProbeDependencies{})
	before := segmentProbeFileDigest(t, partialPath)
	result, err := prober.Probe(context.Background(), partialPath)
	if !errors.Is(err, ErrSegmentProbeUnreadable) || result.Readability != SegmentReadabilityUnreadable {
		t.Fatalf("declared zero-packet stream = (%#v, %v), want unreadable", result, err)
	}
	if after := segmentProbeFileDigest(t, partialPath); after != before {
		t.Fatal("zero-packet probe changed the partial bytes")
	}
}

func TestFFprobeSegmentProberRejectsZeroDurationActivity(t *testing.T) {
	metadata := `{
		"streams":[{"index":0,"codec_type":"video","codec_name":"h264",
			"time_base":"1/1000","start_time":"0","duration":"0"}],
		"format":{"format_name":"matroska","start_time":"0","duration":"0"}
	}`
	activity := `{
		"packets":[{"stream_index":0,"pts_time":"0","dts_time":"0","duration_time":"0"}]
	}`
	prober, partialPath := newSegmentProbeTestProber(t, func(
		_ context.Context,
		invocation segmentProbeInvocation,
		stdout io.Writer,
		_ io.Writer,
	) error {
		output := metadata
		if invocation.phase == segmentProbePhaseActivity {
			output = activity
		}
		_, err := io.WriteString(stdout, output)
		return err
	}, segmentProbeDependencies{})
	result, err := prober.Probe(context.Background(), partialPath)
	if !errors.Is(err, ErrSegmentProbeUnreadable) || result.Readability != SegmentReadabilityUnreadable {
		t.Fatalf("zero-duration packet = (%#v, %v), want unreadable", result, err)
	}
}

func TestFFprobeSegmentProberBoundsCombinedPhaseOutput(t *testing.T) {
	activity := validSegmentActivityProbeJSON(0)
	limit := len(validSegmentProbeJSON) + len(activity) - 1
	prober, partialPath := newSegmentProbeTestProber(
		t,
		writeSegmentProbeJSON(validSegmentProbeJSON),
		segmentProbeDependencies{outputBytes: limit},
	)
	result, err := prober.Probe(context.Background(), partialPath)
	if !errors.Is(err, ErrSegmentProbeOutput) || result.Readability != SegmentReadabilityUnreadable {
		t.Fatalf("combined output bound = (%#v, %v), want output error", result, err)
	}
}

func TestFFprobeSegmentProberRejectsUnsafeInputsWithoutRunning(t *testing.T) {
	runs := 0
	prober, partialPath := newSegmentProbeTestProber(t, func(
		context.Context,
		segmentProbeInvocation,
		io.Writer,
		io.Writer,
	) error {
		runs++
		return nil
	}, segmentProbeDependencies{})

	directory := t.TempDir()
	wrongSuffix := filepath.Join(directory, "segment.ts.partial")
	if err := os.WriteFile(wrongSuffix, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	emptyPath := filepath.Join(directory, "empty.mkv.partial")
	if err := os.WriteFile(emptyPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		path string
		want error
	}{
		{name: "relative", path: "segment.mkv.partial", want: ErrSegmentProbeInput},
		{name: "missing", path: filepath.Join(directory, "missing.mkv.partial"), want: ErrSegmentProbeInput},
		{name: "directory", path: directory, want: ErrSegmentProbeInput},
		{name: "wrong_suffix", path: wrongSuffix, want: ErrSegmentProbeInput},
		{name: "unclean", path: filepath.Dir(partialPath) + string(os.PathSeparator) + "." + string(os.PathSeparator) + filepath.Base(partialPath), want: ErrSegmentProbeInput},
		{name: "empty", path: emptyPath, want: ErrSegmentProbeUnreadable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := prober.Probe(context.Background(), test.path)
			if !errors.Is(err, test.want) || result.Readability != SegmentReadabilityUnreadable {
				t.Fatalf("result=%#v err=%v, want %v", result, err, test.want)
			}
		})
	}
	if runs != 0 {
		t.Fatalf("unsafe inputs started ffprobe %d times", runs)
	}
}

func TestFFprobeSegmentProberRejectsSymlink(t *testing.T) {
	prober, partialPath := newSegmentProbeTestProber(t, writeSegmentProbeJSON(validSegmentProbeJSON), segmentProbeDependencies{})
	symlinkPath := filepath.Join(filepath.Dir(partialPath), "symlink.mkv.partial")
	if err := os.Symlink(partialPath, symlinkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	result, err := prober.Probe(context.Background(), symlinkPath)
	if !errors.Is(err, ErrSegmentProbeInput) || result.Readability != SegmentReadabilityUnreadable {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestFFprobeSegmentProberFailureKeepsPartialByteExact(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		stderr     string
		runErr     error
		outputSize int
		want       error
	}{
		{name: "invalid_json", output: `{`, want: ErrSegmentProbeOutput},
		{name: "trailing_json", output: validSegmentProbeJSON + `{}`, want: ErrSegmentProbeOutput},
		{name: "output_limit", output: strings.Repeat("x", 257), outputSize: 256, want: ErrSegmentProbeOutput},
		{name: "unreadable", stderr: "invalid data found when processing input", runErr: errors.New("exit"), want: ErrSegmentProbeUnreadable},
		{name: "unsupported", stderr: strings.Repeat("noise ", 30) + "unsupported codec", runErr: errors.New("exit"), want: ErrSegmentProbeUnsupported},
		{name: "dependency", runErr: os.ErrNotExist, want: ErrSegmentProbeDependency},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dependencies := segmentProbeDependencies{}
			if test.outputSize != 0 {
				dependencies.outputBytes = test.outputSize
			}
			prober, partialPath := newSegmentProbeTestProber(t, func(
				_ context.Context,
				_ segmentProbeInvocation,
				stdout io.Writer,
				stderr io.Writer,
			) error {
				_, _ = io.WriteString(stdout, test.output)
				_, _ = io.WriteString(stderr, test.stderr)
				return test.runErr
			}, dependencies)
			before := segmentProbeFileDigest(t, partialPath)
			result, err := prober.Probe(context.Background(), partialPath)
			if !errors.Is(err, test.want) || result.Readability != SegmentReadabilityUnreadable {
				t.Fatalf("result=%#v err=%v, want %v", result, err, test.want)
			}
			if err.Error() != test.want.Error() {
				t.Fatalf("unstable or detailed error %q, want %q", err, test.want)
			}
			if after := segmentProbeFileDigest(t, partialPath); after != before {
				t.Fatal("failed probe changed the partial bytes")
			}
		})
	}
}

func TestFFprobeSegmentProberRejectsInvalidOrUnsafeMetadata(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   error
	}{
		{
			name: "non_matroska",
			output: `{"streams":[{"index":0,"codec_type":"video","codec_name":"h264"}],` +
				`"format":{"format_name":"mov,mp4","start_time":"0","duration":"1"}}`,
			want: ErrSegmentProbeUnsupported,
		},
		{
			name: "no_media_stream",
			output: `{"streams":[{"index":0,"codec_type":"subtitle","codec_name":"subrip"}],` +
				`"format":{"format_name":"matroska","start_time":"0","duration":"1"}}`,
			want: ErrSegmentProbeUnreadable,
		},
		{
			name: "duplicate_index",
			output: `{"streams":[{"index":0,"codec_type":"video","codec_name":"h264"},` +
				`{"index":0,"codec_type":"audio","codec_name":"aac"}],` +
				`"format":{"format_name":"matroska","start_time":"0","duration":"1"}}`,
			want: ErrSegmentProbeOutput,
		},
		{
			name: "metadata_url",
			output: `{"streams":[{"index":0,"codec_type":"video","codec_name":"https://private.invalid"}],` +
				`"format":{"format_name":"matroska","start_time":"0","duration":"1"}}`,
			want: ErrSegmentProbeOutput,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prober, partialPath := newSegmentProbeTestProber(t, writeSegmentProbeJSON(test.output), segmentProbeDependencies{})
			before := segmentProbeFileDigest(t, partialPath)
			result, err := prober.Probe(context.Background(), partialPath)
			if !errors.Is(err, test.want) || result.Readability != SegmentReadabilityUnreadable {
				t.Fatalf("result=%#v err=%v, want %v", result, err, test.want)
			}
			if strings.Contains(err.Error(), "private.invalid") {
				t.Fatal("error exposed metadata URL")
			}
			if after := segmentProbeFileDigest(t, partialPath); after != before {
				t.Fatal("failed probe changed the partial bytes")
			}
		})
	}
}

func TestFFprobeSegmentProberRejectsInvalidTimes(t *testing.T) {
	tests := []struct {
		name           string
		formatStart    string
		formatDuration string
		streamStart    string
		streamDuration string
	}{
		{name: "negative_format_start", formatStart: "-0.1", formatDuration: "1", streamStart: "0", streamDuration: "1"},
		{name: "negative_zero", formatStart: "-0.0", formatDuration: "1", streamStart: "0", streamDuration: "1"},
		{name: "nan_duration", formatStart: "0", formatDuration: "NaN", streamStart: "0", streamDuration: "1"},
		{name: "infinite_stream", formatStart: "0", formatDuration: "1", streamStart: "0", streamDuration: "Inf"},
		{name: "negative_stream", formatStart: "0", formatDuration: "1", streamStart: "-1", streamDuration: "1"},
		{name: "negative_zero_pts", formatStart: "0", formatDuration: "1", streamStart: "-0", streamDuration: "1"},
		{name: "overflow", formatStart: "0", formatDuration: "9223372036854.775808", streamStart: "0", streamDuration: "1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output := fmt.Sprintf(
				`{"streams":[{"index":0,"codec_type":"video","codec_name":"h264",`+
					`"start_time":%q,"duration":%q}],`+
					`"format":{"format_name":"matroska","start_time":%q,"duration":%q}}`,
				test.streamStart, test.streamDuration, test.formatStart, test.formatDuration,
			)
			prober, partialPath := newSegmentProbeTestProber(t, writeSegmentProbeJSON(output), segmentProbeDependencies{})
			before := segmentProbeFileDigest(t, partialPath)
			result, err := prober.Probe(context.Background(), partialPath)
			if !errors.Is(err, ErrSegmentProbeOutput) || result.Readability != SegmentReadabilityUnreadable {
				t.Fatalf("result=%#v err=%v", result, err)
			}
			if after := segmentProbeFileDigest(t, partialPath); after != before {
				t.Fatal("invalid-time probe changed the partial bytes")
			}
		})
	}
}

func TestFFprobeSegmentProberTimeoutAndCallerCancellation(t *testing.T) {
	prober, partialPath := newSegmentProbeTestProber(t, func(
		ctx context.Context,
		_ segmentProbeInvocation,
		_ io.Writer,
		_ io.Writer,
	) error {
		<-ctx.Done()
		return ctx.Err()
	}, segmentProbeDependencies{timeout: 10 * time.Millisecond})
	before := segmentProbeFileDigest(t, partialPath)
	result, err := prober.Probe(context.Background(), partialPath)
	if !errors.Is(err, ErrSegmentProbeTimeout) || result.Readability != SegmentReadabilityUnreadable {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if after := segmentProbeFileDigest(t, partialPath); after != before {
		t.Fatal("timed-out probe changed the partial bytes")
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	result, err = prober.Probe(canceled, partialPath)
	if !errors.Is(err, context.Canceled) || result.Readability != SegmentReadabilityUnreadable {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestP3MediaRealFFprobeSegment(t *testing.T) {
	if os.Getenv("P3MEDIA_REAL_FFPROBE") != "1" {
		t.Skip("set P3MEDIA_REAL_FFPROBE=1 to run the real FFmpeg/ffprobe probe")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tools, err := discoverFFmpeg(ctx, ffmpegDiscoveryOptions{})
	if errors.Is(err, ErrFFmpegNotFound) {
		t.Skip("FFmpeg/ffprobe are unavailable")
	}
	if err != nil {
		t.Fatalf("discover verified FFmpeg pair: %v", err)
	}
	partialPath := filepath.Join(t.TempDir(), "real-segment.mkv.partial")
	generate := exec.CommandContext(ctx, tools.ffmpegPath,
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=25",
		"-f", "lavfi", "-i", "sine=frequency=880:sample_rate=48000",
		"-t", "1", "-c:v", "mpeg2video", "-q:v", "5",
		"-c:a", "mp2", "-f", "matroska", partialPath,
	)
	if output, generateErr := generate.CombinedOutput(); generateErr != nil {
		t.Fatalf("generate real Matroska segment: %v (%s)", generateErr, redactFFmpegText(string(output)))
	}
	before := segmentProbeFileDigest(t, partialPath)
	prober, err := newFFprobeSegmentProber(tools)
	if err != nil {
		t.Fatal(err)
	}
	result, err := prober.Probe(ctx, partialPath)
	if err != nil {
		t.Fatalf("probe real Matroska segment: %v", err)
	}
	if result.Readability != SegmentReadabilityReadable ||
		!segmentProbeContainerIsMatroska(result.Container) || len(result.Streams) < 2 ||
		result.DurationUs == nil || result.FirstTimestampUs == nil || result.LastTimestampUs == nil {
		t.Fatalf("incomplete real probe result: %#v", result)
	}
	if after := segmentProbeFileDigest(t, partialPath); after != before {
		t.Fatal("real probe changed the partial bytes")
	}
}

func TestP3MediaRealFFprobeRejectsEmptyMatroska(t *testing.T) {
	if os.Getenv("P3MEDIA_REAL_FFPROBE") != "1" {
		t.Skip("set P3MEDIA_REAL_FFPROBE=1 to run the real empty Matroska probe")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tools, err := discoverFFmpeg(ctx, ffmpegDiscoveryOptions{})
	if errors.Is(err, ErrFFmpegNotFound) {
		t.Skip("FFmpeg/ffprobe are unavailable")
	}
	if err != nil {
		t.Fatalf("discover verified FFmpeg pair: %v", err)
	}
	partialPath := filepath.Join(t.TempDir(), "empty-segment.mkv.partial")
	generate := exec.CommandContext(ctx, tools.ffmpegPath,
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=25",
		"-frames:v", "0", "-c:v", "mpeg2video", "-f", "matroska", partialPath,
	)
	if output, generateErr := generate.CombinedOutput(); generateErr != nil {
		t.Skipf("FFmpeg cannot produce an empty Matroska candidate: %v (%s)",
			generateErr, redactFFmpegText(string(output)))
	}
	info, err := os.Stat(partialPath)
	if err != nil || info.Size() == 0 {
		t.Skipf("FFmpeg did not produce a non-empty empty-Matroska candidate: %v", err)
	}
	metadataInvocation, err := newSegmentProbeInvocation(tools.ffprobePath, partialPath)
	if err != nil {
		t.Fatal(err)
	}
	metadataStdout := newSegmentProbeOutputBuffer(defaultSegmentProbeOutputBytes)
	metadataStderr := newBoundedTextBuffer(defaultSegmentProbeStderrBytes)
	if err := runSegmentProbeCommand(
		ctx, metadataInvocation, metadataStdout, metadataStderr,
	); err != nil {
		t.Skipf("FFmpeg 8.1.2 empty Matroska is not metadata-readable: %s",
			redactFFmpegText(metadataStderr.Snapshot()))
	}
	metadata, err := decodeSegmentProbeOutput(metadataStdout.Bytes())
	if err != nil {
		t.Skipf("empty Matroska has no declared A/V stream: %v", err)
	}
	if _, ok := firstSegmentActivityStream(metadata.Streams); !ok {
		t.Skip("empty Matroska has no declared A/V stream")
	}
	before := segmentProbeFileDigest(t, partialPath)
	prober, err := newFFprobeSegmentProber(tools)
	if err != nil {
		t.Fatal(err)
	}
	result, err := prober.Probe(ctx, partialPath)
	if !errors.Is(err, ErrSegmentProbeUnreadable) || result.Readability != SegmentReadabilityUnreadable {
		t.Fatalf("real empty Matroska = (%#v, %v), want unreadable", result, err)
	}
	if after := segmentProbeFileDigest(t, partialPath); after != before {
		t.Fatal("real empty Matroska probe changed the file")
	}
}

func TestSegmentProbeRunFailureClassificationIsStable(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		stderr string
		want   error
	}{
		{name: "deadline", err: context.DeadlineExceeded, want: ErrSegmentProbeTimeout},
		{name: "dependency", err: os.ErrNotExist, want: ErrSegmentProbeDependency},
		{name: "missing_input", err: errors.New("exit"), stderr: "The system cannot find the file specified", want: ErrSegmentProbeInput},
		{name: "permission", err: errors.New("exit"), stderr: "Permission denied", want: ErrSegmentProbeInput},
		{name: "unsupported", err: errors.New("exit"), stderr: "feature is not implemented", want: ErrSegmentProbeUnsupported},
		{name: "corrupt", err: errors.New("exit"), stderr: "invalid data", want: ErrSegmentProbeUnreadable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifySegmentProbeRunFailure(test.err, test.stderr); got != test.want {
				t.Fatalf("got %v, want %v", got, test.want)
			}
		})
	}
}

func TestNewFFprobeSegmentProberValidatesDependencyAndLimits(t *testing.T) {
	if _, err := newFFprobeSegmentProber(ffmpegTools{}); !errors.Is(err, ErrSegmentProbeDependency) {
		t.Fatalf("missing dependency error = %v", err)
	}
	directory := t.TempDir()
	if _, err := newFFprobeSegmentProber(ffmpegTools{ffprobePath: directory}); !errors.Is(err, ErrSegmentProbeDependency) {
		t.Fatalf("directory dependency error = %v", err)
	}
	probePath := filepath.Join(directory, "ffprobe-private.exe")
	if err := os.WriteFile(probePath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dependencies := range []segmentProbeDependencies{
		{timeout: -time.Second},
		{outputBytes: -1},
		{stderrBytes: -1},
	} {
		if _, err := newFFprobeSegmentProberWithDependencies(
			ffmpegTools{ffprobePath: probePath}, dependencies,
		); !errors.Is(err, ErrSegmentProbeConfiguration) {
			t.Fatalf("invalid dependency limits error = %v", err)
		}
	}
}

func TestSegmentProbeOutputBufferIsBoundedAndDrains(t *testing.T) {
	buffer := newSegmentProbeOutputBuffer(4)
	if count, err := buffer.Write([]byte("abcd")); err != nil || count != 4 || buffer.Overflowed() {
		t.Fatalf("exact write count=%d err=%v overflow=%v", count, err, buffer.Overflowed())
	}
	if count, err := buffer.Write([]byte("efgh")); err != nil || count != 4 || !buffer.Overflowed() {
		t.Fatalf("overflow write count=%d err=%v overflow=%v", count, err, buffer.Overflowed())
	}
	if got := string(buffer.Bytes()); got != "abcd" {
		t.Fatalf("bounded prefix = %q", got)
	}
}

func TestRunSegmentProbeCommandDrainsStdoutAndStderr(t *testing.T) {
	if os.Getenv("GO_SEGMENT_PROBE_HELPER") == "1" {
		return
	}
	t.Setenv("GO_SEGMENT_PROBE_HELPER", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	stdout := newSegmentProbeOutputBuffer(1024)
	stderr := newBoundedTextBuffer(1024)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = runSegmentProbeCommand(ctx, segmentProbeInvocation{
		executablePath: executable,
		args:           []string{"-test.run=^TestSegmentProbeCommandHelperProcess$"},
	}, stdout, stderr)
	if err != nil {
		t.Fatalf("run helper probe: %v", err)
	}
	if !stdout.Overflowed() || len(stdout.Bytes()) != 1024 {
		t.Fatalf("stdout was not fully drained into a bounded writer: overflow=%v bytes=%d", stdout.Overflowed(), len(stdout.Bytes()))
	}
	if snapshot := stderr.Snapshot(); !strings.Contains(snapshot, "stderr-tail") {
		t.Fatalf("stderr was not fully drained: %q", snapshot)
	}
}

func TestSegmentProbeCommandHelperProcess(t *testing.T) {
	if os.Getenv("GO_SEGMENT_PROBE_HELPER") != "1" {
		return
	}
	_, _ = fmt.Fprint(os.Stdout, strings.Repeat("stdout-block ", 32<<10), "stdout-tail")
	_, _ = fmt.Fprint(os.Stderr, strings.Repeat("stderr-block ", 32<<10), "stderr-tail")
	os.Exit(0)
}

func newSegmentProbeTestProber(
	t *testing.T,
	run segmentProbeRun,
	dependencies segmentProbeDependencies,
) (*ffprobeSegmentProber, string) {
	t.Helper()
	directory := t.TempDir()
	probePath := filepath.Join(directory, "ffprobe-private-secret.exe")
	if err := os.WriteFile(probePath, []byte("verified-tool"), 0o600); err != nil {
		t.Fatal(err)
	}
	partialPath := filepath.Join(directory, "segment-private-token.mkv.partial")
	if err := os.WriteFile(partialPath, []byte("immutable-partial-media"), 0o600); err != nil {
		t.Fatal(err)
	}
	dependencies.run = run
	prober, err := newFFprobeSegmentProberWithDependencies(
		ffmpegTools{ffprobePath: probePath}, dependencies,
	)
	if err != nil {
		t.Fatal(err)
	}
	return prober, partialPath
}

func validSegmentActivityProbeJSON(streamIndex int) string {
	return fmt.Sprintf(
		`{"packets":[{"stream_index":%d,"pts_time":"0.500000",`+
			`"dts_time":"0.500000","duration_time":"0.040000"}]}`,
		streamIndex,
	)
}

func writeSegmentProbeJSON(output string) segmentProbeRun {
	metadataOutput := output
	return func(
		_ context.Context,
		invocation segmentProbeInvocation,
		stdout io.Writer,
		_ io.Writer,
	) error {
		phaseOutput := metadataOutput
		if invocation.phase == segmentProbePhaseActivity {
			phaseOutput = validSegmentActivityProbeJSON(invocation.streamIndex)
		}
		_, err := io.WriteString(stdout, phaseOutput)
		return err
	}
}

func segmentProbeFileDigest(t *testing.T, path string) [sha256.Size]byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(content)
}

func assertSegmentProbeInvocation(t *testing.T, invocation segmentProbeInvocation, partialPath string) {
	t.Helper()
	if invocation.phase != segmentProbePhaseMetadata || invocation.partialPath != partialPath ||
		len(invocation.args) < 2 ||
		invocation.args[len(invocation.args)-2] != "-i" ||
		invocation.args[len(invocation.args)-1] != partialPath {
		t.Fatalf("input path was not one explicit argument: %#v", invocation)
	}
	for _, argument := range invocation.args {
		if strings.EqualFold(argument, "cmd") || strings.EqualFold(argument, "cmd.exe") ||
			strings.EqualFold(argument, "/c") {
			t.Fatalf("shell argument found: %#v", invocation)
		}
	}
}

func assertSegmentActivityInvocation(
	t *testing.T,
	invocation segmentProbeInvocation,
	partialPath string,
	streamIndex int,
) {
	t.Helper()
	assertExplicitSegmentProbeInput(t, invocation, partialPath)
	if invocation.phase != segmentProbePhaseActivity || invocation.streamIndex != streamIndex {
		t.Fatalf("unexpected activity invocation: %#v", invocation)
	}
	wantStream := fmt.Sprint(streamIndex)
	selected := false
	bounded := false
	showPackets := false
	for index, argument := range invocation.args {
		if argument == "-select_streams" && index+1 < len(invocation.args) && invocation.args[index+1] == wantStream {
			selected = true
		}
		if argument == "-read_intervals" && index+1 < len(invocation.args) && invocation.args[index+1] == "%+#1" {
			bounded = true
		}
		if argument == "-show_packets" {
			showPackets = true
		}
		if argument == "-count_packets" || argument == "-count_frames" {
			t.Fatalf("unbounded counting argument found: %#v", invocation)
		}
	}
	if !selected || !bounded || !showPackets {
		t.Fatalf("activity invocation is not packet-bounded: %#v", invocation)
	}
}

func assertExplicitSegmentProbeInput(t *testing.T, invocation segmentProbeInvocation, partialPath string) {
	t.Helper()
	if invocation.partialPath != partialPath || len(invocation.args) < 2 ||
		invocation.args[len(invocation.args)-2] != "-i" || invocation.args[len(invocation.args)-1] != partialPath {
		t.Fatalf("input path was not one explicit argument: %#v", invocation)
	}
}

func assertSegmentProbePrivateFormatting(
	t *testing.T,
	prober *ffprobeSegmentProber,
	invocation segmentProbeInvocation,
	secretPath string,
) {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	logger.Info("probe", "prober", prober, "invocation", invocation)
	invocationJSON, err := json.Marshal(invocation)
	if err != nil {
		t.Fatal(err)
	}
	proberJSON, err := json.Marshal(prober)
	if err != nil {
		t.Fatal(err)
	}
	rendered := strings.Join([]string{
		fmt.Sprint(invocation), fmt.Sprintf("%#v", invocation),
		fmt.Sprint(prober), fmt.Sprintf("%#v", prober),
		string(invocationJSON), string(proberJSON), logs.String(),
	}, "\n")
	for _, secret := range []string{secretPath, invocation.executablePath, "private-token", "private-secret"} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("private path leaked through formatting: %s", rendered)
		}
	}
	if !strings.Contains(rendered, "redacted") {
		t.Fatalf("formatting did not identify redaction: %s", rendered)
	}
}

func valueOf(value *int64) int64 {
	if value == nil {
		return -1
	}
	return *value
}
