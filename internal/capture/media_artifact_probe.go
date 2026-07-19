package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultMediaArtifactProbeTimeout     = 10 * time.Second
	defaultMediaArtifactProbeOutputBytes = 1 << 20
	defaultMediaArtifactProbeStderrBytes = 64 << 10
	mediaArtifactProbeShowEntries        = "format=format_name:stream=index,codec_type,codec_name,sample_rate,channels"
)

type mediaArtifactProbePhase string

const (
	mediaArtifactProbePhaseMetadata mediaArtifactProbePhase = "metadata"
	mediaArtifactProbePhaseActivity mediaArtifactProbePhase = "activity"
)

type mediaArtifactProbeInvocation struct {
	executablePath string
	artifactPath   string
	args           []string
	phase          mediaArtifactProbePhase
	streamIndex    int
}

func (invocation mediaArtifactProbeInvocation) String() string {
	return "mediaArtifactProbeInvocation{phase:" + string(invocation.phase) +
		" paths:<redacted> args:" + strconv.Itoa(len(invocation.args)) + "}"
}

func (invocation mediaArtifactProbeInvocation) GoString() string {
	return invocation.String()
}

type mediaArtifactProbeRun func(
	context.Context,
	mediaArtifactProbeInvocation,
	io.Writer,
	io.Writer,
) error

type mediaArtifactProbeDependencies struct {
	run         mediaArtifactProbeRun
	timeout     time.Duration
	outputBytes int
	stderrBytes int
}

func inspectMediaArtifact(
	ctx context.Context,
	ffprobePath string,
	artifactPath string,
	kind MediaArtifactKind,
) error {
	return inspectMediaArtifactWithDependencies(
		ctx,
		ffprobePath,
		artifactPath,
		kind,
		mediaArtifactProbeDependencies{},
	)
}

func inspectMediaArtifactWithDependencies(
	ctx context.Context,
	ffprobePath string,
	artifactPath string,
	kind MediaArtifactKind,
	dependencies mediaArtifactProbeDependencies,
) error {
	if ctx == nil || !validMediaArtifactKind(kind) ||
		!validMediaExecutable(ffprobePath) || !validMediaInput(artifactPath) {
		return ErrMediaArtifactFailed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dependencies, err := normalizeMediaArtifactProbeDependencies(dependencies)
	if err != nil {
		return err
	}
	metadataInvocation, err := newMediaArtifactProbeInvocation(ffprobePath, artifactPath)
	if err != nil {
		return err
	}
	metadataStdout := newSegmentProbeOutputBuffer(dependencies.outputBytes)
	stderr := newBoundedTextBuffer(dependencies.stderrBytes)
	probeCtx, cancel := context.WithTimeout(ctx, dependencies.timeout)
	defer cancel()
	runErr := dependencies.run(probeCtx, metadataInvocation, metadataStdout, stderr)
	if err := mediaArtifactProbeExecutionError(ctx, probeCtx, metadataStdout, stderr, runErr); err != nil {
		return err
	}
	metadataOutput := metadataStdout.Bytes()
	streamIndex, err := decodeMediaArtifactProbeMetadata(metadataOutput, kind)
	if err != nil {
		return err
	}
	activityInvocation, err := newMediaArtifactActivityProbeInvocation(
		ffprobePath, artifactPath, streamIndex,
	)
	if err != nil {
		return err
	}
	remainingOutputBytes := dependencies.outputBytes - len(metadataOutput)
	if remainingOutputBytes < 1 {
		return ErrMediaArtifactFailed
	}
	activityStdout := newSegmentProbeOutputBuffer(remainingOutputBytes)
	runErr = dependencies.run(probeCtx, activityInvocation, activityStdout, stderr)
	if err := mediaArtifactProbeExecutionError(ctx, probeCtx, activityStdout, stderr, runErr); err != nil {
		return err
	}
	return decodeMediaArtifactActivityProbeOutput(activityStdout.Bytes(), streamIndex)
}

func mediaArtifactProbeExecutionError(
	ctx context.Context,
	probeCtx context.Context,
	stdout *segmentProbeOutputBuffer,
	stderr *boundedTextBuffer,
	runErr error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if errors.Is(probeCtx.Err(), context.DeadlineExceeded) || stdout.Overflowed() || runErr != nil {
		_ = stderr.Snapshot()
		return ErrMediaArtifactFailed
	}
	return nil
}

func normalizeMediaArtifactProbeDependencies(
	dependencies mediaArtifactProbeDependencies,
) (mediaArtifactProbeDependencies, error) {
	if dependencies.run == nil {
		dependencies.run = runMediaArtifactProbeCommand
	}
	if dependencies.timeout == 0 {
		dependencies.timeout = defaultMediaArtifactProbeTimeout
	}
	if dependencies.outputBytes == 0 {
		dependencies.outputBytes = defaultMediaArtifactProbeOutputBytes
	}
	if dependencies.stderrBytes == 0 {
		dependencies.stderrBytes = defaultMediaArtifactProbeStderrBytes
	}
	if dependencies.timeout < 0 || dependencies.outputBytes < 0 || dependencies.stderrBytes < 0 {
		return mediaArtifactProbeDependencies{}, ErrMediaArtifactFailed
	}
	return dependencies, nil
}

func newMediaArtifactProbeInvocation(
	ffprobePath string,
	artifactPath string,
) (mediaArtifactProbeInvocation, error) {
	if !validMediaExecutable(ffprobePath) || !validMediaInput(artifactPath) {
		return mediaArtifactProbeInvocation{}, ErrMediaArtifactFailed
	}
	return mediaArtifactProbeInvocation{
		executablePath: filepath.Clean(ffprobePath),
		artifactPath:   filepath.Clean(artifactPath),
		args: []string{
			"-v", "error",
			"-show_entries", mediaArtifactProbeShowEntries,
			"-of", "json",
			"-i", filepath.Clean(artifactPath),
		},
		phase: mediaArtifactProbePhaseMetadata,
	}, nil
}

func newMediaArtifactActivityProbeInvocation(
	ffprobePath string,
	artifactPath string,
	streamIndex int,
) (mediaArtifactProbeInvocation, error) {
	if !validMediaExecutable(ffprobePath) || !validMediaInput(artifactPath) || streamIndex < 0 {
		return mediaArtifactProbeInvocation{}, ErrMediaArtifactFailed
	}
	return mediaArtifactProbeInvocation{
		executablePath: filepath.Clean(ffprobePath),
		artifactPath:   filepath.Clean(artifactPath),
		args: []string{
			"-v", "error",
			"-select_streams", strconv.Itoa(streamIndex),
			"-show_packets",
			"-show_entries", segmentActivityProbeShowEntries,
			"-read_intervals", "%+#1",
			"-of", "json",
			"-i", filepath.Clean(artifactPath),
		},
		phase:       mediaArtifactProbePhaseActivity,
		streamIndex: streamIndex,
	}, nil
}

func runMediaArtifactProbeCommand(
	ctx context.Context,
	invocation mediaArtifactProbeInvocation,
	stdout io.Writer,
	stderr io.Writer,
) error {
	if ctx == nil || invocation.executablePath == "" || stdout == nil || stderr == nil {
		return ErrMediaArtifactFailed
	}
	command := exec.CommandContext(ctx, invocation.executablePath, invocation.args...)
	configureMediaCommand(command)
	command.Stdin = nil
	command.Stdout = stdout
	command.Stderr = stderr
	return command.Run()
}

type rawMediaArtifactProbeOutput struct {
	Streams []rawMediaArtifactProbeStream `json:"streams"`
	Format  rawMediaArtifactProbeFormat   `json:"format"`
}

type rawMediaArtifactProbeFormat struct {
	FormatName string `json:"format_name"`
}

type rawMediaArtifactProbeStream struct {
	Index      int                `json:"index"`
	CodecType  string             `json:"codec_type"`
	CodecName  string             `json:"codec_name"`
	SampleRate segmentProbeScalar `json:"sample_rate"`
	Channels   int                `json:"channels"`
}

func decodeMediaArtifactProbeOutput(output []byte, kind MediaArtifactKind) error {
	_, err := decodeMediaArtifactProbeMetadata(output, kind)
	return err
}

func decodeMediaArtifactProbeMetadata(
	output []byte,
	kind MediaArtifactKind,
) (int, error) {
	if !validMediaArtifactKind(kind) || len(bytes.TrimSpace(output)) == 0 {
		return 0, ErrMediaArtifactFailed
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	var raw rawMediaArtifactProbeOutput
	if err := decoder.Decode(&raw); err != nil {
		return 0, ErrMediaArtifactFailed
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return 0, ErrMediaArtifactFailed
	}
	if len(raw.Streams) == 0 || len(raw.Streams) > maximumSegmentProbeStreams {
		return 0, ErrMediaArtifactFailed
	}
	container, err := sanitizeSegmentProbeToken(raw.Format.FormatName, true)
	if err != nil || container == "" {
		return 0, ErrMediaArtifactFailed
	}
	switch kind {
	case MediaArtifactASRWAV:
		if !mediaArtifactContainerContains(container, "wav") {
			return 0, ErrMediaArtifactFailed
		}
	case MediaArtifactPlaybackMP4:
		if !mediaArtifactContainerContains(container, "mov") &&
			!mediaArtifactContainerContains(container, "mp4") {
			return 0, ErrMediaArtifactFailed
		}
	default:
		return 0, ErrMediaArtifactFailed
	}

	seenIndexes := make(map[int]struct{}, len(raw.Streams))
	audioStreams := 0
	videoStreams := 0
	selectedStreamIndex := -1
	for _, rawStream := range raw.Streams {
		if rawStream.Index < 0 || rawStream.Channels < 0 {
			return 0, ErrMediaArtifactFailed
		}
		if _, exists := seenIndexes[rawStream.Index]; exists {
			return 0, ErrMediaArtifactFailed
		}
		seenIndexes[rawStream.Index] = struct{}{}
		streamType, typeErr := sanitizeSegmentProbeToken(rawStream.CodecType, false)
		codec, codecErr := sanitizeSegmentProbeToken(rawStream.CodecName, false)
		if typeErr != nil || codecErr != nil || streamType == "" || codec == "" {
			return 0, ErrMediaArtifactFailed
		}
		switch streamType {
		case "audio":
			audioStreams++
			if kind == MediaArtifactASRWAV {
				sampleRate, rateErr := parseSegmentProbeNonNegativeInteger(rawStream.SampleRate)
				if rateErr != nil || sampleRate == nil || *sampleRate != 16_000 ||
					rawStream.Channels != 1 || codec != "pcm_s16le" {
					return 0, ErrMediaArtifactFailed
				}
				selectedStreamIndex = rawStream.Index
			} else if codec != "aac" {
				return 0, ErrMediaArtifactFailed
			}
		case "video":
			videoStreams++
			if kind != MediaArtifactPlaybackMP4 || codec != "h264" {
				return 0, ErrMediaArtifactFailed
			}
			selectedStreamIndex = rawStream.Index
		default:
			return 0, ErrMediaArtifactFailed
		}
	}
	if kind == MediaArtifactASRWAV {
		if audioStreams != 1 || videoStreams != 0 || selectedStreamIndex < 0 {
			return 0, ErrMediaArtifactFailed
		}
		return selectedStreamIndex, nil
	}
	if videoStreams != 1 || audioStreams > 1 || selectedStreamIndex < 0 {
		return 0, ErrMediaArtifactFailed
	}
	return selectedStreamIndex, nil
}

func decodeMediaArtifactActivityProbeOutput(output []byte, streamIndex int) error {
	activity, err := decodeSegmentActivityProbeOutput(output, streamIndex)
	if err != nil {
		return ErrMediaArtifactFailed
	}
	hasPositiveTimestamp := activity.TimestampUs != nil && *activity.TimestampUs > 0
	hasPositiveDuration := activity.DurationUs != nil && *activity.DurationUs > 0
	hasPositiveRange := activity.TimestampUs != nil && activity.EndUs != nil &&
		*activity.EndUs > *activity.TimestampUs
	if !hasPositiveTimestamp && !hasPositiveDuration && !hasPositiveRange {
		return ErrMediaArtifactFailed
	}
	return nil
}

func mediaArtifactContainerContains(container string, want string) bool {
	for _, candidate := range strings.Split(container, ",") {
		if candidate == want {
			return true
		}
	}
	return false
}
