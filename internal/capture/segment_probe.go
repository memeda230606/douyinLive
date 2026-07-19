package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	ErrSegmentProbeConfiguration = errors.New("SEGMENT_PROBE_CONFIGURATION_INVALID")
	ErrSegmentProbeDependency    = errors.New("SEGMENT_PROBE_DEPENDENCY_FAILURE")
	ErrSegmentProbeInput         = errors.New("SEGMENT_PROBE_INPUT_INVALID")
	ErrSegmentProbeTimeout       = errors.New("SEGMENT_PROBE_TIMEOUT")
	ErrSegmentProbeOutput        = errors.New("SEGMENT_PROBE_OUTPUT_INVALID")
	ErrSegmentProbeUnreadable    = errors.New("SEGMENT_PROBE_UNREADABLE")
	ErrSegmentProbeUnsupported   = errors.New("SEGMENT_PROBE_UNSUPPORTED")
)

const (
	defaultSegmentProbeTimeout     = 10 * time.Second
	defaultSegmentProbeOutputBytes = 1 << 20
	defaultSegmentProbeStderrBytes = 64 << 10
	maximumSegmentProbeStreams     = 256
	maximumSegmentProbeScalarBytes = 64
	maximumSegmentProbeTokenBytes  = 128
)

const segmentProbeShowEntries = "format=format_name,start_time,duration:" +
	"stream=index,codec_type,codec_name,time_base,start_pts,start_time,duration_ts,duration," +
	"width,height,sample_rate,channels,bit_rate"

const segmentActivityProbeShowEntries = "packet=stream_index,pts_time,dts_time,duration_time"

type SegmentReadability string

const (
	SegmentReadabilityReadable   SegmentReadability = "readable"
	SegmentReadabilityUnreadable SegmentReadability = "unreadable"
)

// SegmentProbeStream contains only bounded media facts. It deliberately has
// no source or filesystem field, so it is safe to persist in media.json.
type SegmentProbeStream struct {
	Index            int    `json:"index"`
	Type             string `json:"type"`
	Codec            string `json:"codec"`
	TimeBase         string `json:"timeBase,omitempty"`
	StartTimestampUs *int64 `json:"startTimestampUs,omitempty"`
	DurationUs       *int64 `json:"durationUs,omitempty"`
	EndTimestampUs   *int64 `json:"endTimestampUs,omitempty"`
	Width            int    `json:"width,omitempty"`
	Height           int    `json:"height,omitempty"`
	SampleRate       int64  `json:"sampleRate,omitempty"`
	Channels         int    `json:"channels,omitempty"`
	BitRate          int64  `json:"bitRate,omitempty"`
}

// SegmentProbeResult is path-free and URL-free. Nil clock fields mean that
// ffprobe did not publish that optional piece of container timing metadata.
type SegmentProbeResult struct {
	Readability      SegmentReadability   `json:"readability"`
	Container        string               `json:"container,omitempty"`
	DurationUs       *int64               `json:"durationUs,omitempty"`
	FirstTimestampUs *int64               `json:"firstTimestampUs,omitempty"`
	LastTimestampUs  *int64               `json:"lastTimestampUs,omitempty"`
	Streams          []SegmentProbeStream `json:"streams,omitempty"`
}

func (result SegmentProbeResult) String() string {
	return "SegmentProbeResult{readability:" + string(result.Readability) +
		" streams:" + strconv.Itoa(len(result.Streams)) + "}"
}

func (result SegmentProbeResult) GoString() string {
	return result.String()
}

func (result SegmentProbeResult) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("readability", string(result.Readability)),
		slog.Int("streams", len(result.Streams)),
	)
}

type segmentProbePhase string

const (
	segmentProbePhaseMetadata segmentProbePhase = "metadata"
	segmentProbePhaseActivity segmentProbePhase = "activity"
)

type segmentProbeInvocation struct {
	executablePath string
	partialPath    string
	args           []string
	phase          segmentProbePhase
	streamIndex    int
}

func (invocation segmentProbeInvocation) String() string {
	return "segmentProbeInvocation{phase:" + string(invocation.phase) +
		" paths:<redacted> args:" + strconv.Itoa(len(invocation.args)) + "}"
}

func (invocation segmentProbeInvocation) GoString() string {
	return invocation.String()
}

func (invocation segmentProbeInvocation) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Phase string `json:"phase"`
		Paths string `json:"paths"`
		Args  int    `json:"args_count"`
	}{Phase: string(invocation.phase), Paths: "<redacted>", Args: len(invocation.args)})
}

func (invocation segmentProbeInvocation) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("phase", string(invocation.phase)),
		slog.String("paths", "<redacted>"),
		slog.Int("args_count", len(invocation.args)),
	)
}

type segmentProbeRun func(
	context.Context,
	segmentProbeInvocation,
	io.Writer,
	io.Writer,
) error

type segmentProbeDependencies struct {
	run         segmentProbeRun
	timeout     time.Duration
	outputBytes int
	stderrBytes int
}

type ffprobeSegmentProber struct {
	ffprobePath  string
	dependencies segmentProbeDependencies
}

func (prober *ffprobeSegmentProber) String() string {
	if prober == nil {
		return "ffprobeSegmentProber{state:nil}"
	}
	return "ffprobeSegmentProber{path:<redacted>}"
}

func (prober *ffprobeSegmentProber) GoString() string {
	return prober.String()
}

func (prober *ffprobeSegmentProber) MarshalJSON() ([]byte, error) {
	return []byte(`{"path":"<redacted>"}`), nil
}

func (prober *ffprobeSegmentProber) LogValue() slog.Value {
	return slog.StringValue(prober.String())
}

func newFFprobeSegmentProber(tools ffmpegTools) (*ffprobeSegmentProber, error) {
	return newFFprobeSegmentProberWithDependencies(tools, segmentProbeDependencies{})
}

func newFFprobeSegmentProberWithDependencies(
	tools ffmpegTools,
	dependencies segmentProbeDependencies,
) (*ffprobeSegmentProber, error) {
	if !validSegmentProbeExecutablePath(tools.ffprobePath) || !regularFile(tools.ffprobePath) {
		return nil, ErrSegmentProbeDependency
	}
	normalized, err := normalizeSegmentProbeDependencies(dependencies)
	if err != nil {
		return nil, err
	}
	return &ffprobeSegmentProber{
		ffprobePath:  filepath.Clean(tools.ffprobePath),
		dependencies: normalized,
	}, nil
}

func normalizeSegmentProbeDependencies(dependencies segmentProbeDependencies) (segmentProbeDependencies, error) {
	if dependencies.run == nil {
		dependencies.run = runSegmentProbeCommand
	}
	if dependencies.timeout == 0 {
		dependencies.timeout = defaultSegmentProbeTimeout
	}
	if dependencies.outputBytes == 0 {
		dependencies.outputBytes = defaultSegmentProbeOutputBytes
	}
	if dependencies.stderrBytes == 0 {
		dependencies.stderrBytes = defaultSegmentProbeStderrBytes
	}
	if dependencies.timeout < 0 || dependencies.outputBytes < 0 || dependencies.stderrBytes < 0 {
		return segmentProbeDependencies{}, ErrSegmentProbeConfiguration
	}
	return dependencies, nil
}

// Probe reads one recorder work or finalized segment without changing it.
// Final renaming and manifest updates belong to the caller and must happen
// only after a readable result is returned.
func (prober *ffprobeSegmentProber) Probe(ctx context.Context, partialPath string) (SegmentProbeResult, error) {
	unreadable := SegmentProbeResult{Readability: SegmentReadabilityUnreadable}
	if prober == nil || ctx == nil || prober.dependencies.run == nil {
		return unreadable, ErrSegmentProbeConfiguration
	}
	if err := ctx.Err(); err != nil {
		return unreadable, err
	}
	if err := validateSegmentPartialPath(partialPath); err != nil {
		return unreadable, err
	}

	invocation, err := newSegmentProbeInvocation(prober.ffprobePath, partialPath)
	if err != nil {
		return unreadable, err
	}
	metadataStdout := newSegmentProbeOutputBuffer(prober.dependencies.outputBytes)
	stderr := newBoundedTextBuffer(prober.dependencies.stderrBytes)
	probeCtx, cancel := context.WithTimeout(ctx, prober.dependencies.timeout)
	defer cancel()
	runErr := prober.dependencies.run(probeCtx, invocation, metadataStdout, stderr)
	if err := segmentProbeExecutionError(ctx, probeCtx, metadataStdout, stderr, runErr); err != nil {
		return unreadable, err
	}
	metadataOutput := metadataStdout.Bytes()
	result, err := decodeSegmentProbeOutput(metadataOutput)
	if err != nil {
		return unreadable, err
	}
	streamIndex, ok := firstSegmentActivityStream(result.Streams)
	if !ok {
		return unreadable, ErrSegmentProbeUnreadable
	}
	activityInvocation, err := newSegmentActivityProbeInvocation(
		prober.ffprobePath, partialPath, streamIndex,
	)
	if err != nil {
		return unreadable, err
	}
	remainingOutputBytes := prober.dependencies.outputBytes - len(metadataOutput)
	if remainingOutputBytes < 1 {
		return unreadable, ErrSegmentProbeOutput
	}
	activityStdout := newSegmentProbeOutputBuffer(remainingOutputBytes)
	runErr = prober.dependencies.run(probeCtx, activityInvocation, activityStdout, stderr)
	if err := segmentProbeExecutionError(ctx, probeCtx, activityStdout, stderr, runErr); err != nil {
		return unreadable, err
	}
	activity, err := decodeSegmentActivityProbeOutput(activityStdout.Bytes(), streamIndex)
	if err != nil {
		return unreadable, err
	}
	result, err = applySegmentMediaActivity(result, activity)
	if err != nil {
		return unreadable, err
	}
	return result, nil
}

func segmentProbeExecutionError(
	ctx context.Context,
	probeCtx context.Context,
	stdout *segmentProbeOutputBuffer,
	stderr *boundedTextBuffer,
	runErr error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
		return ErrSegmentProbeTimeout
	}
	if stdout.Overflowed() {
		return ErrSegmentProbeOutput
	}
	if runErr != nil {
		return classifySegmentProbeRunFailure(runErr, stderr.Snapshot())
	}
	return nil
}

func validateSegmentPartialPath(partialPath string) error {
	if partialPath == "" || !filepath.IsAbs(partialPath) ||
		ffmpegControlCharacters.MatchString(partialPath) ||
		filepath.Clean(partialPath) != partialPath ||
		!validSegmentProbeSuffix(filepath.Base(partialPath)) {
		return ErrSegmentProbeInput
	}
	info, err := os.Lstat(partialPath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return ErrSegmentProbeInput
	}
	if info.Size() == 0 {
		return ErrSegmentProbeUnreadable
	}
	return nil
}

func validSegmentProbeSuffix(name string) bool {
	name = strings.ToLower(name)
	return strings.HasSuffix(name, ".mkv.partial") || strings.HasSuffix(name, ".mkv")
}

func newSegmentProbeInvocation(ffprobePath, partialPath string) (segmentProbeInvocation, error) {
	if !validSegmentProbeExecutablePath(ffprobePath) ||
		partialPath == "" || !filepath.IsAbs(partialPath) ||
		ffmpegControlCharacters.MatchString(partialPath) {
		return segmentProbeInvocation{}, ErrSegmentProbeConfiguration
	}
	args := []string{
		"-v", "error",
		"-show_format",
		"-show_streams",
		"-show_entries", segmentProbeShowEntries,
		"-of", "json",
		"-i", partialPath,
	}
	return segmentProbeInvocation{
		executablePath: filepath.Clean(ffprobePath),
		partialPath:    partialPath,
		args:           args,
		phase:          segmentProbePhaseMetadata,
	}, nil
}

func newSegmentActivityProbeInvocation(
	ffprobePath, partialPath string,
	streamIndex int,
) (segmentProbeInvocation, error) {
	if !validSegmentProbeExecutablePath(ffprobePath) ||
		partialPath == "" || !filepath.IsAbs(partialPath) ||
		ffmpegControlCharacters.MatchString(partialPath) || streamIndex < 0 {
		return segmentProbeInvocation{}, ErrSegmentProbeConfiguration
	}
	args := []string{
		"-v", "error",
		"-select_streams", strconv.Itoa(streamIndex),
		"-show_packets",
		"-show_entries", segmentActivityProbeShowEntries,
		"-read_intervals", "%+#1",
		"-of", "json",
		"-i", partialPath,
	}
	return segmentProbeInvocation{
		executablePath: filepath.Clean(ffprobePath),
		partialPath:    partialPath,
		args:           args,
		phase:          segmentProbePhaseActivity,
		streamIndex:    streamIndex,
	}, nil
}

func runSegmentProbeCommand(
	ctx context.Context,
	invocation segmentProbeInvocation,
	stdout io.Writer,
	stderr io.Writer,
) error {
	if ctx == nil || invocation.executablePath == "" || stdout == nil || stderr == nil {
		return ErrSegmentProbeConfiguration
	}
	command := exec.CommandContext(ctx, invocation.executablePath, invocation.args...)
	configureMediaCommand(command)
	command.Stdout = stdout
	command.Stderr = stderr
	// os/exec owns the pipe-copy goroutines for non-*os.File writers. Run does
	// not return until Wait has joined those copies, so both bounded writers
	// are fully drained and no probe-owned goroutine survives this call.
	return command.Run()
}

func classifySegmentProbeRunFailure(runErr error, stderr string) error {
	if errors.Is(runErr, context.DeadlineExceeded) {
		return ErrSegmentProbeTimeout
	}
	if errors.Is(runErr, os.ErrNotExist) || errors.Is(runErr, exec.ErrNotFound) {
		return ErrSegmentProbeDependency
	}
	normalized := strings.ToLower(stderr)
	switch {
	case containsSegmentProbeMarker(normalized,
		"no such file", "cannot find the file", "the system cannot find",
		"permission denied", "access is denied"):
		return ErrSegmentProbeInput
	case containsSegmentProbeMarker(normalized,
		"unsupported", "not implemented", "unknown codec"):
		return ErrSegmentProbeUnsupported
	default:
		return ErrSegmentProbeUnreadable
	}
}

func validSegmentProbeExecutablePath(value string) bool {
	return value != "" && filepath.IsAbs(value) &&
		filepath.Clean(value) == value && !ffmpegControlCharacters.MatchString(value)
}

func containsSegmentProbeMarker(value string, markers ...string) bool {
	for _, marker := range markers {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

type segmentProbeOutputBuffer struct {
	limit    int
	data     []byte
	overflow bool
}

func newSegmentProbeOutputBuffer(limit int) *segmentProbeOutputBuffer {
	return &segmentProbeOutputBuffer{limit: limit, data: make([]byte, 0, limit)}
}

func (buffer *segmentProbeOutputBuffer) Write(value []byte) (int, error) {
	written := len(value)
	remaining := buffer.limit - len(buffer.data)
	if remaining > 0 {
		if len(value) > remaining {
			buffer.data = append(buffer.data, value[:remaining]...)
		} else {
			buffer.data = append(buffer.data, value...)
		}
	}
	if len(value) > remaining {
		buffer.overflow = true
	}
	// Discard overflow while reporting a complete write so os/exec continues
	// draining stdout and the child cannot block on a full pipe.
	return written, nil
}

func (buffer *segmentProbeOutputBuffer) Bytes() []byte {
	return bytes.Clone(buffer.data)
}

func (buffer *segmentProbeOutputBuffer) Overflowed() bool {
	return buffer.overflow
}

type segmentProbeScalar string

func (scalar *segmentProbeScalar) UnmarshalJSON(value []byte) error {
	if bytes.Equal(value, []byte("null")) {
		*scalar = ""
		return nil
	}
	var decoded string
	if len(value) > 0 && value[0] == '"' {
		if err := json.Unmarshal(value, &decoded); err != nil {
			return ErrSegmentProbeOutput
		}
	} else {
		decoded = string(value)
	}
	if len(decoded) > maximumSegmentProbeScalarBytes ||
		ffmpegControlCharacters.MatchString(decoded) {
		return ErrSegmentProbeOutput
	}
	*scalar = segmentProbeScalar(decoded)
	return nil
}

type rawSegmentProbeOutput struct {
	Streams []rawSegmentProbeStream `json:"streams"`
	Format  rawSegmentProbeFormat   `json:"format"`
}

type rawSegmentProbeFormat struct {
	FormatName string             `json:"format_name"`
	StartTime  segmentProbeScalar `json:"start_time"`
	Duration   segmentProbeScalar `json:"duration"`
}

type rawSegmentProbeStream struct {
	Index      int                `json:"index"`
	CodecType  string             `json:"codec_type"`
	CodecName  string             `json:"codec_name"`
	TimeBase   string             `json:"time_base"`
	StartPTS   segmentProbeScalar `json:"start_pts"`
	StartTime  segmentProbeScalar `json:"start_time"`
	DurationTS segmentProbeScalar `json:"duration_ts"`
	Duration   segmentProbeScalar `json:"duration"`
	Width      int                `json:"width"`
	Height     int                `json:"height"`
	SampleRate segmentProbeScalar `json:"sample_rate"`
	Channels   int                `json:"channels"`
	BitRate    segmentProbeScalar `json:"bit_rate"`
}

func decodeSegmentProbeOutput(output []byte) (SegmentProbeResult, error) {
	unreadable := SegmentProbeResult{Readability: SegmentReadabilityUnreadable}
	if len(bytes.TrimSpace(output)) == 0 {
		return unreadable, ErrSegmentProbeOutput
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	var raw rawSegmentProbeOutput
	if err := decoder.Decode(&raw); err != nil {
		return unreadable, ErrSegmentProbeOutput
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return unreadable, ErrSegmentProbeOutput
	}
	if len(raw.Streams) > maximumSegmentProbeStreams {
		return unreadable, ErrSegmentProbeOutput
	}
	container, err := sanitizeSegmentProbeToken(raw.Format.FormatName, true)
	if err != nil || container == "" {
		return unreadable, ErrSegmentProbeOutput
	}
	if !segmentProbeContainerIsMatroska(container) {
		return unreadable, ErrSegmentProbeUnsupported
	}

	formatStart, err := parseSegmentProbeTime(raw.Format.StartTime)
	if err != nil {
		return unreadable, err
	}
	formatDuration, err := parseSegmentProbeTime(raw.Format.Duration)
	if err != nil {
		return unreadable, err
	}
	formatEnd, err := addSegmentProbeTimes(formatStart, formatDuration)
	if err != nil {
		return unreadable, err
	}

	result := SegmentProbeResult{
		Readability: SegmentReadabilityUnreadable,
		Container:   container,
		DurationUs:  cloneInt64Pointer(formatDuration),
		Streams:     make([]SegmentProbeStream, 0, len(raw.Streams)),
	}
	first := cloneInt64Pointer(formatStart)
	last := cloneInt64Pointer(formatEnd)
	hasMedia := false
	seenIndexes := make(map[int]struct{}, len(raw.Streams))
	for _, rawStream := range raw.Streams {
		stream, streamErr := decodeSegmentProbeStream(rawStream)
		if streamErr != nil {
			return unreadable, streamErr
		}
		if _, exists := seenIndexes[stream.Index]; exists {
			return unreadable, ErrSegmentProbeOutput
		}
		seenIndexes[stream.Index] = struct{}{}
		if stream.Type == "audio" || stream.Type == "video" {
			hasMedia = true
		}
		mergeSegmentProbeMinimum(&first, stream.StartTimestampUs)
		mergeSegmentProbeMaximum(&last, stream.EndTimestampUs)
		result.Streams = append(result.Streams, stream)
	}
	if !hasMedia {
		return unreadable, ErrSegmentProbeUnreadable
	}
	sort.Slice(result.Streams, func(left, right int) bool {
		return result.Streams[left].Index < result.Streams[right].Index
	})
	result.FirstTimestampUs = first
	result.LastTimestampUs = last
	if first != nil && last != nil {
		if *last < *first {
			return unreadable, ErrSegmentProbeOutput
		}
		rangeDuration := *last - *first
		if result.DurationUs == nil || rangeDuration > *result.DurationUs {
			result.DurationUs = int64Pointer(rangeDuration)
		}
	}
	return result, nil
}

func firstSegmentActivityStream(streams []SegmentProbeStream) (int, bool) {
	for _, stream := range streams {
		if stream.Type == "video" || stream.Type == "audio" {
			return stream.Index, true
		}
	}
	return 0, false
}

type rawSegmentActivityProbeOutput struct {
	Packets []rawSegmentActivityProbePacket `json:"packets"`
}

type rawSegmentActivityProbePacket struct {
	StreamIndex  *int               `json:"stream_index"`
	PTSTime      segmentProbeScalar `json:"pts_time"`
	DTSTime      segmentProbeScalar `json:"dts_time"`
	DurationTime segmentProbeScalar `json:"duration_time"`
}

type segmentProbeActivity struct {
	TimestampUs *int64
	DurationUs  *int64
	EndUs       *int64
}

func decodeSegmentActivityProbeOutput(
	output []byte,
	streamIndex int,
) (segmentProbeActivity, error) {
	if streamIndex < 0 || len(bytes.TrimSpace(output)) == 0 {
		return segmentProbeActivity{}, ErrSegmentProbeOutput
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	var raw rawSegmentActivityProbeOutput
	if err := decoder.Decode(&raw); err != nil {
		return segmentProbeActivity{}, ErrSegmentProbeOutput
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return segmentProbeActivity{}, ErrSegmentProbeOutput
	}
	if len(raw.Packets) == 0 {
		return segmentProbeActivity{}, ErrSegmentProbeUnreadable
	}
	if len(raw.Packets) != 1 || raw.Packets[0].StreamIndex == nil ||
		*raw.Packets[0].StreamIndex != streamIndex {
		return segmentProbeActivity{}, ErrSegmentProbeOutput
	}
	packet := raw.Packets[0]
	timestamp, err := parseSegmentProbeTime(packet.PTSTime)
	if err != nil {
		return segmentProbeActivity{}, err
	}
	if timestamp == nil {
		timestamp, err = parseSegmentProbeTime(packet.DTSTime)
		if err != nil {
			return segmentProbeActivity{}, err
		}
	}
	duration, err := parseSegmentProbeTime(packet.DurationTime)
	if err != nil {
		return segmentProbeActivity{}, err
	}
	end, err := addSegmentProbeTimes(timestamp, duration)
	if err != nil {
		return segmentProbeActivity{}, err
	}
	return segmentProbeActivity{
		TimestampUs: timestamp,
		DurationUs:  duration,
		EndUs:       end,
	}, nil
}

func applySegmentMediaActivity(
	result SegmentProbeResult,
	activity segmentProbeActivity,
) (SegmentProbeResult, error) {
	mergeSegmentProbeMinimum(&result.FirstTimestampUs, activity.TimestampUs)
	mergeSegmentProbeMaximum(&result.LastTimestampUs, activity.EndUs)
	if activity.DurationUs != nil && *activity.DurationUs > 0 &&
		(result.DurationUs == nil || *activity.DurationUs > *result.DurationUs) {
		result.DurationUs = cloneInt64Pointer(activity.DurationUs)
	}
	if result.FirstTimestampUs != nil && result.LastTimestampUs != nil {
		if *result.LastTimestampUs < *result.FirstTimestampUs {
			return SegmentProbeResult{}, ErrSegmentProbeOutput
		}
		rangeDuration := *result.LastTimestampUs - *result.FirstTimestampUs
		if rangeDuration > 0 && (result.DurationUs == nil || rangeDuration > *result.DurationUs) {
			result.DurationUs = int64Pointer(rangeDuration)
		}
	}
	hasPositiveDuration := result.DurationUs != nil && *result.DurationUs > 0
	hasPositiveRange := result.FirstTimestampUs != nil && result.LastTimestampUs != nil &&
		*result.LastTimestampUs > *result.FirstTimestampUs
	if !hasPositiveDuration && !hasPositiveRange {
		return SegmentProbeResult{}, ErrSegmentProbeUnreadable
	}
	result.Readability = SegmentReadabilityReadable
	return result, nil
}

func decodeSegmentProbeStream(raw rawSegmentProbeStream) (SegmentProbeStream, error) {
	if raw.Index < 0 || raw.Width < 0 || raw.Height < 0 || raw.Channels < 0 {
		return SegmentProbeStream{}, ErrSegmentProbeOutput
	}
	kind, err := sanitizeSegmentProbeToken(raw.CodecType, false)
	if err != nil {
		return SegmentProbeStream{}, err
	}
	if kind == "" {
		kind = "unknown"
	}
	codec, err := sanitizeSegmentProbeToken(raw.CodecName, false)
	if err != nil {
		return SegmentProbeStream{}, err
	}
	if codec == "" {
		codec = "unknown"
	}
	timeBase, timeBaseValue, err := parseSegmentProbeTimeBase(raw.TimeBase)
	if err != nil {
		return SegmentProbeStream{}, err
	}
	start, err := parseSegmentProbeScaledTime(raw.StartPTS, timeBaseValue)
	if err != nil {
		return SegmentProbeStream{}, err
	}
	if start == nil {
		start, err = parseSegmentProbeTime(raw.StartTime)
		if err != nil {
			return SegmentProbeStream{}, err
		}
	}
	duration, err := parseSegmentProbeScaledTime(raw.DurationTS, timeBaseValue)
	if err != nil {
		return SegmentProbeStream{}, err
	}
	if duration == nil {
		duration, err = parseSegmentProbeTime(raw.Duration)
		if err != nil {
			return SegmentProbeStream{}, err
		}
	}
	end, err := addSegmentProbeTimes(start, duration)
	if err != nil {
		return SegmentProbeStream{}, err
	}
	sampleRate, err := parseSegmentProbeNonNegativeInteger(raw.SampleRate)
	if err != nil {
		return SegmentProbeStream{}, err
	}
	bitRate, err := parseSegmentProbeNonNegativeInteger(raw.BitRate)
	if err != nil {
		return SegmentProbeStream{}, err
	}
	return SegmentProbeStream{
		Index: raw.Index, Type: kind, Codec: codec, TimeBase: timeBase,
		StartTimestampUs: start, DurationUs: duration, EndTimestampUs: end,
		Width: raw.Width, Height: raw.Height, Channels: raw.Channels,
		SampleRate: valueOrZero(sampleRate), BitRate: valueOrZero(bitRate),
	}, nil
}

func sanitizeSegmentProbeToken(value string, allowComma bool) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) > maximumSegmentProbeTokenBytes {
		return "", ErrSegmentProbeOutput
	}
	for _, character := range value {
		valid := character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9' ||
			character == '_' || character == '-' || character == '.' || character == '+'
		if allowComma && character == ',' {
			valid = true
		}
		if !valid {
			return "", ErrSegmentProbeOutput
		}
	}
	return value, nil
}

func segmentProbeContainerIsMatroska(container string) bool {
	for _, name := range strings.Split(container, ",") {
		if strings.TrimSpace(name) == "matroska" {
			return true
		}
	}
	return false
}

func parseSegmentProbeTimeBase(value string) (string, *big.Rat, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "N/A" {
		return "", nil, nil
	}
	if len(value) > maximumSegmentProbeScalarBytes ||
		strings.Count(value, "/") != 1 {
		return "", nil, ErrSegmentProbeOutput
	}
	rational, ok := new(big.Rat).SetString(value)
	if !ok || rational.Sign() <= 0 {
		return "", nil, ErrSegmentProbeOutput
	}
	return value, rational, nil
}

func parseSegmentProbeTime(value segmentProbeScalar) (*int64, error) {
	rational, present, err := parseSegmentProbeNonNegativeRational(string(value))
	if err != nil || !present {
		return nil, err
	}
	return segmentProbeRationalMicros(rational)
}

func parseSegmentProbeScaledTime(value segmentProbeScalar, scale *big.Rat) (*int64, error) {
	integer, err := parseSegmentProbeNonNegativeInteger(value)
	if err != nil || integer == nil {
		return nil, err
	}
	if scale == nil {
		return nil, ErrSegmentProbeOutput
	}
	rational := new(big.Rat).SetInt64(*integer)
	rational.Mul(rational, scale)
	return segmentProbeRationalMicros(rational)
}

func parseSegmentProbeNonNegativeInteger(value segmentProbeScalar) (*int64, error) {
	text := strings.TrimSpace(string(value))
	if text == "" || text == "N/A" {
		return nil, nil
	}
	if len(text) > maximumSegmentProbeScalarBytes || strings.HasPrefix(text, "-") {
		return nil, ErrSegmentProbeOutput
	}
	integer, ok := new(big.Int).SetString(text, 10)
	if !ok || integer.Sign() < 0 || !integer.IsInt64() {
		return nil, ErrSegmentProbeOutput
	}
	result := integer.Int64()
	return &result, nil
}

func parseSegmentProbeNonNegativeRational(value string) (*big.Rat, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "N/A" {
		return nil, false, nil
	}
	if len(value) > maximumSegmentProbeScalarBytes {
		return nil, false, ErrSegmentProbeOutput
	}
	// big.Rat rejects NaN and infinities. The explicit sign check also rejects
	// negative zero spellings before they can be normalized to zero.
	if strings.HasPrefix(value, "-") {
		return nil, false, ErrSegmentProbeOutput
	}
	rational, ok := new(big.Rat).SetString(value)
	if !ok || rational.Sign() < 0 {
		return nil, false, ErrSegmentProbeOutput
	}
	return rational, true, nil
}

func segmentProbeRationalMicros(value *big.Rat) (*int64, error) {
	if value == nil || value.Sign() < 0 {
		return nil, ErrSegmentProbeOutput
	}
	scaled := new(big.Rat).Mul(new(big.Rat).Set(value), big.NewRat(1_000_000, 1))
	quotient := new(big.Int)
	remainder := new(big.Int)
	quotient.QuoRem(scaled.Num(), scaled.Denom(), remainder)
	// Round sub-microsecond positive values to the nearest microsecond.
	doubledRemainder := new(big.Int).Lsh(new(big.Int).Abs(remainder), 1)
	if doubledRemainder.Cmp(scaled.Denom()) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if quotient.Sign() < 0 || !quotient.IsInt64() {
		return nil, ErrSegmentProbeOutput
	}
	result := quotient.Int64()
	return &result, nil
}

func addSegmentProbeTimes(start, duration *int64) (*int64, error) {
	if start == nil || duration == nil {
		return nil, nil
	}
	if *start < 0 || *duration < 0 || *start > int64(^uint64(0)>>1)-*duration {
		return nil, ErrSegmentProbeOutput
	}
	return int64Pointer(*start + *duration), nil
}

func mergeSegmentProbeMinimum(current **int64, candidate *int64) {
	if candidate == nil {
		return
	}
	if *current == nil || *candidate < **current {
		*current = cloneInt64Pointer(candidate)
	}
}

func mergeSegmentProbeMaximum(current **int64, candidate *int64) {
	if candidate == nil {
		return
	}
	if *current == nil || *candidate > **current {
		*current = cloneInt64Pointer(candidate)
	}
}

func int64Pointer(value int64) *int64 {
	return &value
}

func cloneInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	return int64Pointer(*value)
}

func valueOrZero(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}
