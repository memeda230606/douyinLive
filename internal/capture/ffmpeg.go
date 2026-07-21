package capture

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrFFmpegNotFound         = errors.New("FFMPEG_NOT_FOUND")
	ErrFFmpegInvalid          = errors.New("FFMPEG_INVALID")
	ErrFFmpegArguments        = errors.New("FFMPEG_ARGS_INVALID")
	ErrFFmpegProgress         = errors.New("FFMPEG_PROGRESS_INVALID")
	ffmpegURLPattern          = regexp.MustCompile(`(?i)(?:https?|rtmp|rtmps)://[^\s"'<>]+`)
	ffmpegHeaderSecretPattern = regexp.MustCompile(`(?im)(?:cookie|authorization)\s*[:=][^\r\n]*`)
	ffmpegSecretPattern       = regexp.MustCompile(`(?i)(?:token|signature|a_bogus|mstoken)\s*[:=]\s*[^&\s,;]+`)
	ffmpegControlCharacters   = regexp.MustCompile(`[\x00-\x1f\x7f]`)
	ffmpegWindowsPathPattern  = regexp.MustCompile(`(?i)(?:[a-z]:[\\/]|\\\\)`)
)

const (
	ffmpegVersionTimeout   = 5 * time.Second
	ffmpegVersionOutputMax = 64 << 10
	ffmpegBuildSummaryMax  = 512
	ffmpegProgressLineMax  = 16 << 10
	ffmpegLogDefaultBytes  = 64 << 10
	ffmpegReadWriteTimeout = 15 * time.Second
)

type FFmpegBinaryInfo struct {
	Version      string `json:"version"`
	BuildSummary string `json:"build_summary,omitempty"`
	SHA256       string `json:"sha256"`
}

// ffmpegTools deliberately keeps executable paths in an internal-only type.
// Diagnostics and frontend contracts may use BinaryInfo but must never receive
// these paths or the stream URL passed to the process.
type ffmpegTools struct {
	ffmpegPath  string
	ffprobePath string
	FFmpeg      FFmpegBinaryInfo
	FFprobe     FFmpegBinaryInfo
}

type ffmpegDiscoveryOptions struct {
	ExplicitFFmpeg string
	ExplicitProbe  string
	BundledDir     string
	LookPath       func(string) (string, error)
	RunVersion     func(context.Context, string) ([]byte, error)
}

type ffmpegToolPair struct {
	ffmpeg string
	probe  string
}

func discoverFFmpeg(ctx context.Context, options ffmpegDiscoveryOptions) (ffmpegTools, error) {
	if ctx == nil {
		return ffmpegTools{}, ErrFFmpegInvalid
	}
	if err := ctx.Err(); err != nil {
		return ffmpegTools{}, err
	}
	for _, configuredPath := range []string{options.ExplicitFFmpeg, options.ExplicitProbe, options.BundledDir} {
		if configuredPath != "" && (!filepath.IsAbs(configuredPath) || ffmpegControlCharacters.MatchString(configuredPath)) {
			return ffmpegTools{}, ErrFFmpegInvalid
		}
	}
	if options.LookPath == nil {
		options.LookPath = exec.LookPath
	}
	if options.RunVersion == nil {
		options.RunVersion = runFFmpegVersion
	}

	pairs := ffmpegCandidatePairs(options)
	seen := make(map[string]struct{}, len(pairs))
	foundCandidate := false
	for _, pair := range pairs {
		if err := ctx.Err(); err != nil {
			return ffmpegTools{}, err
		}
		if pair.ffmpeg == "" || pair.probe == "" {
			continue
		}
		key := strings.ToLower(filepath.Clean(pair.ffmpeg) + "\x00" + filepath.Clean(pair.probe))
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		ffmpegRegular := regularFile(pair.ffmpeg)
		probeRegular := regularFile(pair.probe)
		foundCandidate = foundCandidate || ffmpegRegular || probeRegular
		if !ffmpegRegular || !probeRegular {
			continue
		}
		ffmpegInfo, err := inspectFFmpegBinary(ctx, pair.ffmpeg, "ffmpeg", options.RunVersion)
		if err != nil {
			if ctx.Err() != nil {
				return ffmpegTools{}, ctx.Err()
			}
			continue
		}
		probeInfo, err := inspectFFmpegBinary(ctx, pair.probe, "ffprobe", options.RunVersion)
		if err != nil {
			if ctx.Err() != nil {
				return ffmpegTools{}, ctx.Err()
			}
			continue
		}
		return ffmpegTools{
			ffmpegPath: pair.ffmpeg, ffprobePath: pair.probe,
			FFmpeg: ffmpegInfo, FFprobe: probeInfo,
		}, nil
	}
	if foundCandidate {
		return ffmpegTools{}, ErrFFmpegInvalid
	}
	return ffmpegTools{}, ErrFFmpegNotFound
}

func ffmpegCandidatePairs(options ffmpegDiscoveryOptions) []ffmpegToolPair {
	pairs := make([]ffmpegToolPair, 0, 3)
	if options.ExplicitFFmpeg != "" || options.ExplicitProbe != "" {
		ffmpegPath := executablePath(options.ExplicitFFmpeg, "ffmpeg")
		probePath := executablePath(options.ExplicitProbe, "ffprobe")
		if probePath == "" && ffmpegPath != "" {
			probePath = filepath.Join(filepath.Dir(ffmpegPath), companionExecutable(ffmpegPath, "ffprobe"))
		}
		if ffmpegPath == "" && probePath != "" {
			ffmpegPath = filepath.Join(filepath.Dir(probePath), companionExecutable(probePath, "ffmpeg"))
		}
		pairs = append(pairs, ffmpegToolPair{ffmpeg: ffmpegPath, probe: probePath})
	}
	if options.BundledDir != "" {
		pairs = append(pairs, ffmpegToolPair{
			ffmpeg: filepath.Join(options.BundledDir, companionExecutable(options.BundledDir, "ffmpeg")),
			probe:  filepath.Join(options.BundledDir, companionExecutable(options.BundledDir, "ffprobe")),
		})
	}
	ffmpegPath, ffmpegErr := options.LookPath("ffmpeg")
	probePath, probeErr := options.LookPath("ffprobe")
	if ffmpegErr == nil || probeErr == nil {
		ffmpegPath = lockExecutablePath(ffmpegPath)
		probePath = lockExecutablePath(probePath)
		pairs = append(pairs, ffmpegToolPair{ffmpeg: ffmpegPath, probe: probePath})
	}
	return pairs
}

func lockExecutablePath(path string) string {
	if path == "" {
		return ""
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	return filepath.Clean(absolute)
}

func executablePath(path, name string) string {
	if path == "" {
		return ""
	}
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return filepath.Join(path, companionExecutable(path, name))
	}
	return path
}

func companionExecutable(reference, name string) string {
	if runtime.GOOS == "windows" || strings.EqualFold(filepath.Ext(reference), ".exe") || strings.EqualFold(filepath.Ext(filepath.Base(reference)), ".exe") {
		return name + ".exe"
	}
	return name
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func inspectFFmpegBinary(
	ctx context.Context,
	path string,
	toolName string,
	runVersion func(context.Context, string) ([]byte, error),
) (FFmpegBinaryInfo, error) {
	versionCtx, cancel := context.WithTimeout(ctx, ffmpegVersionTimeout)
	output, err := runVersion(versionCtx, path)
	cancel()
	if err != nil || len(output) == 0 || len(output) > ffmpegVersionOutputMax {
		return FFmpegBinaryInfo{}, ErrFFmpegInvalid
	}
	version, build, ok := parseFFmpegVersion(output, toolName)
	if !ok {
		return FFmpegBinaryInfo{}, ErrFFmpegInvalid
	}
	digest, err := hashExecutable(ctx, path)
	if err != nil {
		return FFmpegBinaryInfo{}, err
	}
	return FFmpegBinaryInfo{Version: version, BuildSummary: build, SHA256: digest}, nil
}

func runFFmpegVersion(ctx context.Context, path string) ([]byte, error) {
	var output prefixBuffer
	output.limit = ffmpegVersionOutputMax + 1
	command := exec.CommandContext(ctx, path, "-version")
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		return nil, ErrFFmpegInvalid
	}
	return output.Bytes(), nil
}

func parseFFmpegVersion(output []byte, toolName string) (string, string, bool) {
	lines := strings.Split(strings.ToValidUTF8(string(output), ""), "\n")
	version := ""
	build := ""
	for _, line := range lines {
		line = strings.TrimSpace(line)
		lower := strings.ToLower(line)
		if version == "" && strings.HasPrefix(lower, strings.ToLower(toolName)+" version ") {
			version = clipText(line, ffmpegBuildSummaryMax)
		}
		if build == "" && strings.HasPrefix(lower, "configuration:") {
			build = clipText(line, ffmpegBuildSummaryMax)
		}
	}
	return version, build, version != ""
}

func hashExecutable(ctx context.Context, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", ErrFFmpegInvalid
	}
	defer file.Close()
	hash := sha256.New()
	buffer := make([]byte, 128<<10)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			_, _ = hash.Write(buffer[:count])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", ErrFFmpegInvalid
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type prefixBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func (b *prefixBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	written := len(value)
	remaining := b.limit - len(b.data)
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		b.data = append(b.data, value...)
	}
	return written, nil
}

func (b *prefixBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.data)
}

type ffmpegRecordingSpec struct {
	InputURL       string
	OutputPattern  string
	SegmentSeconds int
}

func (spec ffmpegRecordingSpec) String() string {
	return "ffmpegRecordingSpec{InputURL:<redacted> OutputPattern:<redacted> SegmentSeconds:" + strconv.Itoa(spec.SegmentSeconds) + "}"
}

func (spec ffmpegRecordingSpec) GoString() string {
	return spec.String()
}

func (spec ffmpegRecordingSpec) MarshalJSON() ([]byte, error) {
	return []byte(
		`{"inputUrl":"<redacted>","outputPattern":"<redacted>","segmentSeconds":` +
			strconv.Itoa(spec.SegmentSeconds) + `}`,
	), nil
}

func (spec ffmpegRecordingSpec) LogValue() slog.Value {
	return slog.StringValue(spec.String())
}

func newFFmpegOutputPattern(mediaDir string, startedAt time.Time, attemptID string) (string, error) {
	if mediaDir == "" || !filepath.IsAbs(mediaDir) || strings.Contains(mediaDir, "%") ||
		ffmpegControlCharacters.MatchString(mediaDir) || !validRecorderAttemptID(attemptID) {
		return "", ErrFFmpegArguments
	}
	stamp := startedAt.UTC().Format("20060102T150405.000000000Z")
	return filepath.Join(filepath.Clean(mediaDir), "segment-%06d-"+stamp+"-"+strings.ToLower(attemptID)+".mkv.partial"), nil
}

func buildFFmpegRecordingArgs(spec ffmpegRecordingSpec) ([]string, error) {
	if spec.SegmentSeconds < 300 || spec.SegmentSeconds > 1800 {
		return nil, ErrFFmpegArguments
	}
	input, err := url.Parse(spec.InputURL)
	if err != nil || (input.Scheme != "http" && input.Scheme != "https") || input.Hostname() == "" || input.User != nil || input.Fragment != "" || ffmpegControlCharacters.MatchString(spec.InputURL) {
		return nil, ErrFFmpegArguments
	}
	pattern := filepath.Clean(spec.OutputPattern)
	base := filepath.Base(pattern)
	remainingPercent := strings.Replace(pattern, "%06d", "", 1)
	if !filepath.IsAbs(pattern) || !strings.HasSuffix(strings.ToLower(base), ".mkv.partial") || strings.Count(pattern, "%06d") != 1 || strings.Contains(remainingPercent, "%") || ffmpegControlCharacters.MatchString(pattern) {
		return nil, ErrFFmpegArguments
	}
	return []string{
		"-hide_banner",
		"-loglevel", "warning",
		"-nostats",
		"-progress", "pipe:1",
		// FFmpeg protocol timeout values are expressed in microseconds.
		"-rw_timeout", strconv.FormatInt(int64(ffmpegReadWriteTimeout/time.Microsecond), 10),
		"-i", spec.InputURL,
		"-map", "0:v?",
		"-map", "0:a?",
		"-c", "copy",
		"-f", "segment",
		"-segment_format", "matroska",
		"-segment_time", strconv.Itoa(spec.SegmentSeconds),
		"-segment_start_number", "1",
		"-reset_timestamps", "1",
		pattern,
	}, nil
}

type FFmpegProgress struct {
	Frame              int64
	FPS                float64
	TotalSize          int64
	TotalSizeAvailable bool
	OutTime            time.Duration
	Speed              float64
	State              string
}

func readFFmpegProgress(ctx context.Context, reader io.Reader, accept func(FFmpegProgress)) error {
	if ctx == nil || reader == nil {
		return ErrFFmpegProgress
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 1024), ffmpegProgressLineMax)
	current := FFmpegProgress{}
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Text()
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			return ErrFFmpegProgress
		}
		var err error
		switch key {
		case "frame":
			current.Frame, err = parseNonNegativeInt(value)
		case "fps":
			current.FPS, err = parseNonNegativeFloat(value)
		case "total_size":
			current.TotalSizeAvailable = strings.TrimSpace(value) != "N/A"
			current.TotalSize, err = parseNonNegativeInt(value)
		case "out_time_us", "out_time_ms":
			var micros int64
			micros, err = parseNonNegativeInt(value)
			const maximumDuration = time.Duration(1<<63 - 1)
			if err == nil && micros > int64(maximumDuration/time.Microsecond) {
				err = ErrFFmpegProgress
			}
			if err == nil {
				current.OutTime = time.Duration(micros) * time.Microsecond
			}
		case "out_time":
			current.OutTime, err = parseFFmpegClock(value)
		case "speed":
			current.Speed, err = parseNonNegativeFloat(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(value), "x")))
		case "progress":
			if value != "continue" && value != "end" {
				return ErrFFmpegProgress
			}
			current.State = value
			if accept != nil {
				accept(current)
			}
			current = FFmpegProgress{}
		default:
			// New FFmpeg versions may add fields. The bounded scanner protects
			// memory, so unknown keys are safe to ignore.
		}
		if err != nil {
			return ErrFFmpegProgress
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if scanner.Err() != nil {
		return ErrFFmpegProgress
	}
	return nil
}

func parseNonNegativeInt(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "N/A" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, ErrFFmpegProgress
	}
	return parsed, nil
}

func parseNonNegativeFloat(value string) (float64, error) {
	value = strings.TrimSpace(value)
	if value == "N/A" {
		return 0, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed < 0 || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, ErrFFmpegProgress
	}
	return parsed, nil
}

func parseFFmpegClock(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "N/A" {
		return 0, nil
	}
	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		return 0, ErrFFmpegProgress
	}
	hours, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	if err != nil || hours < 0 {
		return 0, ErrFFmpegProgress
	}
	minutes, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil || minutes < 0 || minutes > 59 {
		return 0, ErrFFmpegProgress
	}
	seconds, err := parseNonNegativeFloat(parts[2])
	if err != nil || seconds >= 60 {
		return 0, ErrFFmpegProgress
	}
	const maximumDuration = time.Duration(1<<63 - 1)
	if hours > int64(maximumDuration/time.Hour) {
		return 0, ErrFFmpegProgress
	}
	result := time.Duration(hours) * time.Hour
	minutesDuration := time.Duration(minutes) * time.Minute
	if minutesDuration > maximumDuration-result {
		return 0, ErrFFmpegProgress
	}
	result += minutesDuration
	secondsDuration := time.Duration(seconds * float64(time.Second))
	if secondsDuration > maximumDuration-result {
		return 0, ErrFFmpegProgress
	}
	return result + secondsDuration, nil
}

type boundedTextBuffer struct {
	mu        sync.Mutex
	capacity  int
	data      []byte
	truncated bool
}

func newBoundedTextBuffer(capacity int) *boundedTextBuffer {
	if capacity <= 0 {
		capacity = ffmpegLogDefaultBytes
	}
	return &boundedTextBuffer{capacity: capacity, data: make([]byte, 0, capacity)}
}

func (b *boundedTextBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	written := len(value)
	if len(value) >= b.capacity {
		b.data = append(b.data[:0], value[len(value)-b.capacity:]...)
		b.truncated = true
		return written, nil
	}
	overflow := len(b.data) + len(value) - b.capacity
	if overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
		b.truncated = true
	}
	b.data = append(b.data, value...)
	return written, nil
}

func (b *boundedTextBuffer) Snapshot() string {
	b.mu.Lock()
	value := bytes.Clone(b.data)
	truncated := b.truncated
	b.mu.Unlock()
	if truncated {
		// A tail buffer may begin in the middle of a URL or secret token.
		// Drop that incomplete token before applying normal redaction.
		if boundary := bytes.IndexAny(value, " \t\r\n"); boundary >= 0 {
			value = value[boundary:]
		} else {
			value = nil
		}
	}
	return redactFFmpegText(string(value))
}

func redactFFmpegText(value string) string {
	value = strings.ToValidUTF8(value, "")
	value = ffmpegURLPattern.ReplaceAllString(value, "<redacted-stream-url>")
	value = ffmpegHeaderSecretPattern.ReplaceAllString(value, "<redacted-secret>")
	value = ffmpegSecretPattern.ReplaceAllString(value, "<redacted-secret>")
	value = redactFFmpegPathTokens(value)
	return value
}

func redactFFmpegPathTokens(value string) string {
	fields := strings.Fields(value)
	for index, token := range fields {
		prefix := ""
		candidate := token
		if separator := strings.Index(candidate, "="); separator >= 0 {
			prefix = candidate[:separator+1]
			candidate = candidate[separator+1:]
		}
		candidate = strings.Trim(candidate, `"'`)
		for _, includePrefix := range []string{"-I", "-L"} {
			if strings.HasPrefix(candidate, includePrefix) {
				candidate = candidate[len(includePrefix):]
				break
			}
		}
		if strings.HasPrefix(candidate, "/") || ffmpegWindowsPathPattern.MatchString(candidate) {
			if prefix == "" {
				fields[index] = "<redacted-path>"
			} else {
				fields[index] = prefix + "<redacted-path>"
			}
		}
	}
	return strings.Join(fields, " ")
}

func clipText(value string, limit int) string {
	value = redactFFmpegText(value)
	value = ffmpegControlCharacters.ReplaceAllString(value, " ")
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
