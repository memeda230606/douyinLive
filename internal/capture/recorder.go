package capture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	douyinLive "github.com/jwwsjlm/douyinLive/v2"
)

var (
	ErrRecorderConfiguration     = errors.New("RECORDER_CONFIGURATION_INVALID")
	ErrRecorderCapacity          = errors.New("RECORDER_CAPACITY_EXCEEDED")
	ErrRecorderOutput            = errors.New("RECORDER_OUTPUT_UNAVAILABLE")
	ErrRecorderStart             = errors.New("RECORDER_START_FAILED")
	ErrRecorderStopped           = errors.New("RECORDER_STOPPED")
	ErrRecorderStop              = errors.New("RECORDER_STOP_FAILED")
	ErrRecorderProcessExited     = errors.New(RecorderProcessExitedErrorCode)
	ErrRecorderStreamExpired     = errors.New(RecorderStreamExpiredErrorCode)
	ErrRecorderNetworkFailure    = errors.New(RecorderNetworkFailureErrorCode)
	ErrRecorderUnsupportedInput  = errors.New(RecorderUnsupportedInputErrorCode)
	ErrRecorderLocalResource     = errors.New(RecorderLocalResourceErrorCode)
	ErrRecorderDependencyFailure = errors.New(RecorderDependencyFailureErrorCode)
)

const (
	defaultRecorderConcurrency      = 1
	defaultRecorderResolveSnapshots = 2
	defaultRecorderSegmentSeconds   = 600
	defaultRecorderGracefulTimeout  = 5 * time.Second
	defaultRecorderTerminateTimeout = 3 * time.Second
	defaultRecorderStartupWindow    = 8 * time.Second
	defaultRecorderEventBuffer      = 16
	recorderStderrBufferBytes       = 64 << 10
)

type RecorderEventKind string

const (
	RecorderEventProcessExited RecorderEventKind = "process_exited"
)

const (
	RecorderProcessExitedErrorCode     = "RECORDER_PROCESS_EXITED"
	RecorderStreamExpiredErrorCode     = "RECORDER_STREAM_EXPIRED"
	RecorderNetworkFailureErrorCode    = "RECORDER_NETWORK_FAILURE"
	RecorderUnsupportedInputErrorCode  = "RECORDER_UNSUPPORTED_INPUT"
	RecorderLocalResourceErrorCode     = "RECORDER_LOCAL_RESOURCE"
	RecorderDependencyFailureErrorCode = "RECORDER_DEPENDENCY_FAILURE"
)

// RecorderEvent is deliberately correlation-only. It contains neither stream
// metadata nor filesystem provenance, so it is safe to serialize and log.
type RecorderEvent struct {
	Kind       RecorderEventKind `json:"kind"`
	AttemptID  string            `json:"attemptId"`
	ErrorCode  string            `json:"errorCode"`
	OccurredAt int64             `json:"occurredAt"`
}

// FFmpegDependencyInfo is the path-free result of one dependency discovery.
type FFmpegDependencyInfo struct {
	FFmpeg  FFmpegBinaryInfo `json:"ffmpeg"`
	FFprobe FFmpegBinaryInfo `json:"ffprobe"`
}

type FFmpegRecorderFactoryOptions struct {
	DataRoot                string
	ExplicitFFmpeg          string
	ExplicitProbe           string
	BundledDir              string
	MaxConcurrentRecordings int
	Preference              douyinLive.StreamSelectionPreference
}

// String deliberately omits dependency and data-root paths as well as raw
// preference values.
func (FFmpegRecorderFactoryOptions) String() string {
	return "FFmpegRecorderFactoryOptions{paths:<redacted> preferences:<redacted>}"
}

func (options FFmpegRecorderFactoryOptions) GoString() string {
	return options.String()
}

func (options FFmpegRecorderFactoryOptions) MarshalJSON() ([]byte, error) {
	return []byte(`{"paths":"<redacted>","preferences":"<redacted>"}`), nil
}

func (options FFmpegRecorderFactoryOptions) LogValue() slog.Value {
	return slog.StringValue(options.String())
}

// NewFFmpegRecorderFactory discovers and verifies the FFmpeg pair exactly
// once. Executable paths remain inside the returned factory; only version and
// hashes are returned to callers.
func NewFFmpegRecorderFactory(
	ctx context.Context,
	options FFmpegRecorderFactoryOptions,
) (RecorderFactory, FFmpegDependencyInfo, error) {
	if err := validateRecorderFactoryOptions(ctx, options); err != nil {
		return nil, FFmpegDependencyInfo{}, err
	}
	tools, err := discoverFFmpeg(ctx, ffmpegDiscoveryOptions{
		ExplicitFFmpeg: options.ExplicitFFmpeg,
		ExplicitProbe:  options.ExplicitProbe,
		BundledDir:     options.BundledDir,
	})
	if err != nil {
		return nil, FFmpegDependencyInfo{}, err
	}
	return newFFmpegRecorderFactoryWithTools(options, tools, defaultRecorderDependencies())
}

type recorderProcess interface {
	writeQuit() error
	terminateProcess() error
	terminateTree() error
	wait(context.Context) error
	done() <-chan struct{}
	close() error
}

type recorderDependencies struct {
	resolveStreams func(CaptureSource) ([]douyinLive.ResolvedStream, error)
	startProcess   func(context.Context, processConfig) (recorderProcess, processStreams, error)
	now            func() time.Time
	newAttemptID   func() (string, error)

	maxResolveSnapshots int
	gracefulTimeout     time.Duration
	terminateTimeout    time.Duration
	startupWindow       time.Duration
	eventBuffer         int
}

func defaultRecorderDependencies() recorderDependencies {
	return recorderDependencies{
		resolveStreams: func(source CaptureSource) ([]douyinLive.ResolvedStream, error) {
			// CaptureSource has no context-aware resolver. Calling synchronously
			// avoids abandoning an uninterruptible resolver goroutine.
			return source.ResolveStreams()
		},
		startProcess: func(ctx context.Context, config processConfig) (recorderProcess, processStreams, error) {
			return startManagedProcess(ctx, config)
		},
		now: time.Now,
		newAttemptID: func() (string, error) {
			id, err := uuid.NewV7()
			return id.String(), err
		},
		maxResolveSnapshots: defaultRecorderResolveSnapshots,
		gracefulTimeout:     defaultRecorderGracefulTimeout,
		terminateTimeout:    defaultRecorderTerminateTimeout,
		startupWindow:       defaultRecorderStartupWindow,
		eventBuffer:         defaultRecorderEventBuffer,
	}
}

func normalizeRecorderDependencies(dependencies recorderDependencies) (recorderDependencies, error) {
	defaults := defaultRecorderDependencies()
	if dependencies.resolveStreams == nil {
		dependencies.resolveStreams = defaults.resolveStreams
	}
	if dependencies.startProcess == nil {
		dependencies.startProcess = defaults.startProcess
	}
	if dependencies.now == nil {
		dependencies.now = defaults.now
	}
	if dependencies.newAttemptID == nil {
		dependencies.newAttemptID = defaults.newAttemptID
	}
	if dependencies.maxResolveSnapshots == 0 {
		dependencies.maxResolveSnapshots = defaults.maxResolveSnapshots
	}
	if dependencies.gracefulTimeout == 0 {
		dependencies.gracefulTimeout = defaults.gracefulTimeout
	}
	if dependencies.terminateTimeout == 0 {
		dependencies.terminateTimeout = defaults.terminateTimeout
	}
	if dependencies.startupWindow == 0 {
		dependencies.startupWindow = defaults.startupWindow
	}
	if dependencies.eventBuffer == 0 {
		dependencies.eventBuffer = defaults.eventBuffer
	}
	if dependencies.maxResolveSnapshots < 1 || dependencies.gracefulTimeout < 0 ||
		dependencies.terminateTimeout < 0 || dependencies.startupWindow < 0 || dependencies.eventBuffer < 1 {
		return recorderDependencies{}, ErrRecorderConfiguration
	}
	return dependencies, nil
}

func validateRecorderFactoryOptions(ctx context.Context, options FFmpegRecorderFactoryOptions) error {
	if ctx == nil {
		return ErrRecorderConfiguration
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validRecorderFactoryPath(options.DataRoot, true, true) ||
		!validRecorderFactoryPath(options.ExplicitFFmpeg, false, false) ||
		!validRecorderFactoryPath(options.ExplicitProbe, false, false) ||
		!validRecorderFactoryPath(options.BundledDir, false, false) ||
		options.MaxConcurrentRecordings < 0 || options.MaxConcurrentRecordings > 4 {
		return ErrRecorderConfiguration
	}
	return nil
}
func validRecorderFactoryPath(value string, required, rejectPercent bool) bool {
	if value == "" {
		return !required
	}
	if !filepath.IsAbs(value) || ffmpegControlCharacters.MatchString(value) {
		return false
	}
	if rejectPercent && strings.Contains(value, "%") {
		return false
	}
	return true
}

func newFFmpegRecorderFactoryWithTools(
	options FFmpegRecorderFactoryOptions,
	tools ffmpegTools,
	dependencies recorderDependencies,
) (RecorderFactory, FFmpegDependencyInfo, error) {
	if err := validateRecorderFactoryOptions(context.Background(), options); err != nil {
		return nil, FFmpegDependencyInfo{}, err
	}
	if !validRecorderFactoryPath(tools.ffmpegPath, true, false) ||
		!validRecorderFactoryPath(tools.ffprobePath, true, false) {
		return nil, FFmpegDependencyInfo{}, ErrFFmpegInvalid
	}
	normalizedDependencies, err := normalizeRecorderDependencies(dependencies)
	if err != nil {
		return nil, FFmpegDependencyInfo{}, err
	}
	maximum := options.MaxConcurrentRecordings
	if maximum == 0 {
		maximum = defaultRecorderConcurrency
	}
	dataRoot := filepath.Clean(options.DataRoot)
	capacity := make(chan struct{}, maximum)
	info := FFmpegDependencyInfo{
		FFmpeg:  safeFFmpegBinaryInfo(tools.FFmpeg),
		FFprobe: safeFFmpegBinaryInfo(tools.FFprobe),
	}

	factory := RecorderFactory(func(
		ctx context.Context,
		session LiveSession,
		request OpenRequest,
		source CaptureSource,
	) (Recorder, error) {
		if ctx == nil || source == nil {
			return nil, ErrRecorderConfiguration
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		select {
		case capacity <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			return nil, errors.Join(ErrRecordingUnavailable, ErrRecorderCapacity)
		}
		release := func() { <-capacity }

		mediaDirectory, err := recorderMediaDirectory(dataRoot, session)
		if err != nil {
			release()
			return nil, errors.Join(ErrRecordingUnavailable, err)
		}
		if request.Profile.SaveDirectory != "" && !sameRecorderDirectory(request.Profile.SaveDirectory, mediaDirectory) {
			release()
			return nil, ErrRecorderConfiguration
		}

		segmentSeconds, err := recorderSegmentSeconds(request.Profile.SegmentMinutes)
		if err != nil {
			release()
			return nil, err
		}
		preference := options.Preference
		preference.QualityKey = request.Profile.Quality
		recorder, err := newFFmpegRecorder(ctx, source, recorderOptions{
			tools: tools, mediaDirectory: mediaDirectory,
			preference: preference, segmentSeconds: segmentSeconds,
		}, normalizedDependencies, release)
		if err != nil {
			return nil, err
		}
		return recorder, nil
	})
	return factory, info, nil
}
func sameRecorderDirectory(requested, canonical string) bool {
	if requested == "" || canonical == "" || !filepath.IsAbs(requested) || !filepath.IsAbs(canonical) {
		return false
	}
	requested = filepath.Clean(requested)
	canonical = filepath.Clean(canonical)
	if recorderPathsEqual(requested, canonical) {
		return true
	}
	requestedEvaluated, requestedErr := filepath.EvalSymlinks(requested)
	canonicalEvaluated, canonicalErr := filepath.EvalSymlinks(canonical)
	return requestedErr == nil && canonicalErr == nil &&
		recorderPathsEqual(filepath.Clean(requestedEvaluated), filepath.Clean(canonicalEvaluated))
}

func recorderPathsEqual(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func safeFFmpegBinaryInfo(info FFmpegBinaryInfo) FFmpegBinaryInfo {
	digest := info.SHA256
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 {
		digest = ""
	}
	return FFmpegBinaryInfo{
		Version:      clipText(redactFFmpegText(info.Version), ffmpegBuildSummaryMax),
		SHA256:       digest,
		BuildSummary: clipText(redactFFmpegText(info.BuildSummary), ffmpegBuildSummaryMax),
	}
}

func recorderMediaDirectory(dataRoot string, session LiveSession) (string, error) {
	if err := validateUUIDv7("session id", session.ID); err != nil {
		return "", ErrRecorderConfiguration
	}
	manifestPath, err := secureManifestPath(dataRoot, session.DataPath)
	if err != nil {
		return "", ErrRecorderConfiguration
	}
	mediaDirectory := filepath.Join(filepath.Dir(manifestPath), "media")
	if err := os.MkdirAll(mediaDirectory, 0o700); err != nil {
		return "", ErrRecorderOutput
	}
	return mediaDirectory, nil
}

func recorderSegmentSeconds(minutes int) (int, error) {
	if minutes == 0 {
		return defaultRecorderSegmentSeconds, nil
	}
	if minutes < 5 || minutes > 30 {
		return 0, ErrRecorderConfiguration
	}
	return minutes * 60, nil
}

type recorderOptions struct {
	tools          ffmpegTools
	mediaDirectory string
	preference     douyinLive.StreamSelectionPreference
	segmentSeconds int
}

func (recorderOptions) String() string {
	return "recorderOptions{paths:<redacted> preferences:<redacted>}"
}

func (options recorderOptions) GoString() string {
	return options.String()
}

func (options recorderOptions) LogValue() slog.Value {
	return slog.StringValue(options.String())
}

type FFmpegRecorder struct {
	operationMu sync.Mutex
	mu          sync.Mutex

	options      recorderOptions
	dependencies recorderDependencies
	source       CaptureSource
	current      *recorderAttempt
	pending      *recorderAttempt
	events       chan RecorderEvent

	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc

	stopping                bool
	stopped                 bool
	hadUncleanExit          bool
	lastUnexpectedAttemptID string
	stopErr                 error
	eventsClosed            bool
	stopDone                chan struct{}

	releaseCapacity func()
	releaseOnce     sync.Once
	finishStopOnce  sync.Once
}

type recorderAttempt struct {
	id      string
	process recorderProcess
	streams processStreams

	expected     bool // protected by FFmpegRecorder.mu
	starting     bool // protected by FFmpegRecorder.mu
	stderrBuffer *boundedTextBuffer
	progress     chan struct{}
	progressOnce sync.Once
	startupEnd   chan struct{}
	endOnce      sync.Once
	startupEnded bool // written by the serialized bind operation
	drainWG      sync.WaitGroup
	finished     chan struct{}
	exitCode     string // protected by FFmpegRecorder.mu
}

func newFFmpegRecorder(
	ctx context.Context,
	source CaptureSource,
	options recorderOptions,
	dependencies recorderDependencies,
	releaseCapacity func(),
) (*FFmpegRecorder, error) {
	if ctx == nil || source == nil || options.tools.ffmpegPath == "" ||
		options.mediaDirectory == "" || !filepath.IsAbs(options.mediaDirectory) ||
		options.segmentSeconds < 300 || options.segmentSeconds > 1800 {
		if releaseCapacity != nil {
			releaseCapacity()
		}
		return nil, ErrRecorderConfiguration
	}
	if err := ctx.Err(); err != nil {
		if releaseCapacity != nil {
			releaseCapacity()
		}
		return nil, err
	}
	normalizedDependencies, err := normalizeRecorderDependencies(dependencies)
	if err != nil {
		if releaseCapacity != nil {
			releaseCapacity()
		}
		return nil, err
	}
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	recorder := &FFmpegRecorder{
		options: options, dependencies: normalizedDependencies,
		source: source, events: make(chan RecorderEvent, normalizedDependencies.eventBuffer),
		stopDone:     make(chan struct{}),
		lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel,
		releaseCapacity: releaseCapacity,
	}
	if err := recorder.bindLocked(ctx, source); err != nil {
		lifecycleCancel()
		recorder.mu.Lock()
		attempt := recorder.pending
		if attempt == nil && recorder.current != nil {
			attempt = recorder.detachCurrentLocked()
		}
		recorder.closeEventsLocked()
		recorder.mu.Unlock()
		if attempt != nil && !recorderAttemptFinished(attempt) {
			go func() {
				<-attempt.finished
				recorder.release()
			}()
		} else {
			recorder.release()
		}
		return nil, err
	}
	return recorder, nil
}

func (r *FFmpegRecorder) Events() <-chan RecorderEvent {
	if r == nil {
		closed := make(chan RecorderEvent)
		close(closed)
		return closed
	}
	return r.events
}

// IsCurrentEvent lets a coordinator discard a queued exit after a newer bind
// has already established another attempt.
func (r *FFmpegRecorder) IsCurrentEvent(event RecorderEvent) bool {
	if r == nil || event.Kind != RecorderEventProcessExited || !validRecorderAttemptID(event.AttemptID) {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.stopping && !r.stopped && r.current == nil &&
		r.lastUnexpectedAttemptID == event.AttemptID
}

func (r *FFmpegRecorder) String() string {
	if r == nil {
		return "FFmpegRecorder{state:nil}"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.stopped:
		return "FFmpegRecorder{state:stopped}"
	case r.stopping:
		return "FFmpegRecorder{state:stopping}"
	case r.current == nil:
		return "FFmpegRecorder{state:unavailable}"
	default:
		return "FFmpegRecorder{state:recording}"
	}
}

func (r *FFmpegRecorder) GoString() string {
	return r.String()
}

func (r *FFmpegRecorder) LogValue() slog.Value {
	return slog.StringValue(r.String())
}

func (r *FFmpegRecorder) Rebind(ctx context.Context, source CaptureSource) error {
	if r == nil || ctx == nil || source == nil {
		return ErrRecorderConfiguration
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.operationMu.Lock()
	defer r.operationMu.Unlock()
	if err := r.awaitPendingAttempt(ctx); err != nil {
		return err
	}

	r.mu.Lock()
	if r.stopped || r.stopping {
		r.mu.Unlock()
		return ErrRecorderStopped
	}
	previous := r.detachCurrentLocked()
	r.mu.Unlock()

	if previous != nil {
		graceful, shutdownErr := r.shutdownAttempt(ctx, previous)
		if !graceful {
			r.mu.Lock()
			r.hadUncleanExit = true
			r.mu.Unlock()
		}
		if shutdownErr != nil {
			if !recorderAttemptFinished(previous) {
				r.rememberPendingAttempt(previous)
			}
			return shutdownErr
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.bindLocked(ctx, source)
}

func (r *FFmpegRecorder) Stop(ctx context.Context) error {
	if r == nil || ctx == nil {
		return ErrRecorderConfiguration
	}
	r.operationMu.Lock()
	r.mu.Lock()
	if r.stopped {
		err := r.stopErr
		r.mu.Unlock()
		r.operationMu.Unlock()
		return err
	}
	if r.stopping {
		done := r.stopDone
		r.mu.Unlock()
		r.operationMu.Unlock()
		select {
		case <-done:
			r.mu.Lock()
			err := r.stopErr
			r.mu.Unlock()
			return err
		case <-ctx.Done():
			return errors.Join(ErrRecorderStop, ctx.Err())
		}
	}
	r.stopping = true
	attempt := r.detachCurrentLocked()
	if attempt == nil && r.pending != nil {
		attempt = r.pending
		r.pending = nil
	}
	previouslyUnclean := r.hadUncleanExit
	r.mu.Unlock()

	graceful := true
	var shutdownErr error
	if attempt != nil {
		graceful, shutdownErr = r.shutdownAttempt(ctx, attempt)
	}
	if !graceful || previouslyUnclean {
		shutdownErr = errors.Join(shutdownErr, ErrRecorderStop)
	}

	if attempt == nil || recorderAttemptFinished(attempt) {
		r.finishStop(shutdownErr)
	} else {
		go func() {
			<-attempt.finished
			r.finishStop(shutdownErr)
		}()
	}
	r.operationMu.Unlock()
	return shutdownErr
}

func (r *FFmpegRecorder) finishStop(err error) {
	r.finishStopOnce.Do(func() {
		r.lifecycleCancel()
		r.mu.Lock()
		r.stopping = false
		r.stopped = true
		r.stopErr = err
		r.closeEventsLocked()
		close(r.stopDone)
		r.mu.Unlock()
		r.release()
	})
}

func recorderAttemptFinished(attempt *recorderAttempt) bool {
	if attempt == nil {
		return true
	}
	select {
	case <-attempt.finished:
		return true
	default:
		return false
	}
}

func (r *FFmpegRecorder) rememberPendingAttempt(attempt *recorderAttempt) {
	if attempt == nil || recorderAttemptFinished(attempt) {
		return
	}
	r.mu.Lock()
	if r.pending == nil {
		r.pending = attempt
	}
	r.mu.Unlock()
}

func (r *FFmpegRecorder) awaitPendingAttempt(ctx context.Context) error {
	r.mu.Lock()
	attempt := r.pending
	r.mu.Unlock()
	if attempt == nil {
		return nil
	}
	if err := waitRecorderAttemptFinished(ctx, attempt); err != nil {
		return err
	}
	r.mu.Lock()
	if r.pending == attempt {
		r.pending = nil
	}
	r.mu.Unlock()
	return nil
}

func (r *FFmpegRecorder) bindLocked(ctx context.Context, source CaptureSource) error {
	var lastCandidateErr error
	attemptedURLs := make(map[[sha256.Size]byte]struct{})
	for snapshot := 0; snapshot < r.dependencies.maxResolveSnapshots; snapshot++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		candidates, resolveErr := r.dependencies.resolveStreams(source)
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(candidates) == 0 {
			_ = resolveErr
			continue
		}
		ranked, rankErr := douyinLive.RankResolvedStreams(candidates, r.options.preference)
		if rankErr != nil {
			continue
		}
		for _, candidate := range ranked {
			urlDigest := sha256.Sum256([]byte(candidate.URL))
			if _, attempted := attemptedURLs[urlDigest]; attempted {
				continue
			}
			attemptedURLs[urlDigest] = struct{}{}
			if err := ctx.Err(); err != nil {
				return err
			}
			attempt, startErr := r.startCandidate(ctx, candidate)
			if startErr == nil {
				startErr = r.commitCandidate(ctx, source, attempt)
			}
			if startErr != nil {
				if err := ctx.Err(); err != nil {
					return err
				}
				if errors.Is(startErr, ErrRecorderOutput) {
					return errors.Join(ErrRecordingUnavailable, ErrRecorderOutput)
				}
				if errors.Is(startErr, ErrRecorderStopped) {
					return ErrRecorderStopped
				}
				if errors.Is(startErr, ErrRecorderStop) {
					return startErr
				}
				switch {
				case errors.Is(startErr, ErrRecorderLocalResource),
					errors.Is(startErr, ErrRecorderDependencyFailure):
					return errors.Join(ErrRecordingUnavailable, startErr)
				case errors.Is(startErr, ErrRecorderStreamExpired),
					errors.Is(startErr, ErrRecorderNetworkFailure),
					errors.Is(startErr, ErrRecorderUnsupportedInput),
					errors.Is(startErr, ErrRecorderProcessExited):
					lastCandidateErr = startErr
				default:
					lastCandidateErr = startErr
				}
				continue
			}
			return nil
		}
	}
	return errors.Join(ErrRecordingUnavailable, ErrRecorderStart, lastCandidateErr)
}

func (r *FFmpegRecorder) commitCandidate(ctx context.Context, source CaptureSource, attempt *recorderAttempt) error {
	var cause error
	classifyExit := false
	r.mu.Lock()
	switch {
	case ctx.Err() != nil:
		cause = ctx.Err()
	case r.current != attempt:
		cause = ErrRecorderStart
		classifyExit = true
	case r.stopped || r.stopping:
		cause = ErrRecorderStopped
	default:
		select {
		case <-attempt.process.done():
			cause = ErrRecorderStart
			classifyExit = true
		default:
			r.source = source
			r.lastUnexpectedAttemptID = ""
			attempt.starting = false
		}
	}
	if cause != nil && r.current == attempt {
		attempt.expected = true
		r.current = nil
	}
	r.mu.Unlock()
	if cause != nil {
		return r.failStartupAttempt(ctx, attempt, cause, classifyExit)
	}
	return nil
}

func (r *FFmpegRecorder) startCandidate(ctx context.Context, candidate douyinLive.ResolvedStream) (*recorderAttempt, error) {
	attemptID, err := r.dependencies.newAttemptID()
	if err != nil || !validRecorderAttemptID(attemptID) {
		return nil, ErrRecorderStart
	}
	attemptDirectory := filepath.Join(r.options.mediaDirectory, ".attempt-"+attemptID)
	if err := os.Mkdir(attemptDirectory, 0o700); err != nil {
		return nil, ErrRecorderOutput
	}
	outputPattern, err := newFFmpegOutputPattern(attemptDirectory, r.dependencies.now(), attemptID)
	if err != nil {
		return nil, ErrRecorderOutput
	}
	args, err := buildFFmpegRecordingArgs(ffmpegRecordingSpec{
		InputURL: candidate.URL, OutputPattern: outputPattern,
		SegmentSeconds: r.options.segmentSeconds,
	})
	if err != nil {
		return nil, ErrRecorderStart
	}
	process, streams, err := r.dependencies.startProcess(ctx, processConfig{
		Path: r.options.tools.ffmpegPath, Args: args, Dir: attemptDirectory,
	})
	if err != nil || process == nil {
		if streams.Stdout != nil {
			_ = streams.Stdout.Close()
		}
		if streams.Stderr != nil {
			_ = streams.Stderr.Close()
		}
		if process != nil {
			_ = process.terminateTree()
			_ = process.close()
		}
		return nil, ErrRecorderStart
	}
	attempt := &recorderAttempt{
		id: attemptID, process: process, streams: streams,
		starting: true, stderrBuffer: newBoundedTextBuffer(recorderStderrBufferBytes),
		progress: make(chan struct{}), startupEnd: make(chan struct{}),
		finished: make(chan struct{}),
	}
	r.startAttemptDrains(attempt)
	r.mu.Lock()
	if r.stopped || r.stopping {
		attempt.expected = true
		attempt.starting = false
		r.mu.Unlock()
		go r.watchAttempt(attempt)
		return nil, r.failStartupAttempt(ctx, attempt, ErrRecorderStopped, false)
	}
	r.current = attempt
	r.mu.Unlock()
	go r.watchAttempt(attempt)
	if err := r.observeAttemptStartup(ctx, attempt); err != nil {
		return nil, err
	}
	return attempt, nil
}

func (r *FFmpegRecorder) observeAttemptStartup(ctx context.Context, attempt *recorderAttempt) error {
	timer := time.NewTimer(r.dependencies.startupWindow)
	defer timer.Stop()

	var cause error
	classifyExit := false
	select {
	case <-attempt.progress:
	case <-attempt.startupEnd:
		cause = ErrRecorderStart
		classifyExit = true
		attempt.startupEnded = true
	case <-timer.C:
		cause = ErrRecorderStart
	case <-attempt.process.done():
		cause = ErrRecorderStart
		classifyExit = true
	case <-ctx.Done():
		cause = ctx.Err()
	}
	if cause != nil {
		return r.failStartupAttempt(ctx, attempt, cause, classifyExit)
	}

	r.mu.Lock()
	switch {
	case r.current != attempt:
		cause = ErrRecorderStart
		classifyExit = true
	case r.stopped || r.stopping:
		cause = ErrRecorderStopped
		attempt.expected = true
		r.current = nil
	default:
		select {
		case <-attempt.process.done():
			cause = ErrRecorderStart
			classifyExit = true
			attempt.expected = true
			r.current = nil
		default:
			// bindLocked commits source, correlation state, and readiness atomically.
		}
	}
	r.mu.Unlock()
	if cause != nil {
		return r.failStartupAttempt(ctx, attempt, cause, classifyExit)
	}
	return nil
}

func (r *FFmpegRecorder) failStartupAttempt(ctx context.Context, attempt *recorderAttempt, cause error, classifyExit bool) error {
	r.mu.Lock()
	attempt.expected = true
	attempt.starting = false
	if r.current == attempt {
		r.current = nil
	}
	r.mu.Unlock()

	_, shutdownErr := r.shutdownAttempt(ctx, attempt)
	if !recorderAttemptFinished(attempt) {
		r.rememberPendingAttempt(attempt)
	}
	result := cause
	if classifyExit && errors.Is(cause, ErrRecorderStart) && recorderAttemptFinished(attempt) {
		r.mu.Lock()
		exitCode := attempt.exitCode
		r.mu.Unlock()
		if exitCode == RecorderProcessExitedErrorCode && attempt.startupEnded {
			exitCode = RecorderStreamExpiredErrorCode
		}
		result = errors.Join(result, recorderErrorForExitCode(exitCode))
	}
	if shutdownErr != nil && ctx.Err() == nil {
		return errors.Join(result, shutdownErr)
	}
	return result
}

func validRecorderAttemptID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.Version() == 7
}

func (r *FFmpegRecorder) startAttemptDrains(attempt *recorderAttempt) {
	if attempt.streams.Stdout != nil {
		attempt.drainWG.Add(1)
		go func(reader io.ReadCloser) {
			defer attempt.drainWG.Done()
			defer reader.Close()
			_ = readFFmpegProgress(r.lifecycleCtx, reader, func(progress FFmpegProgress) {
				switch progress.State {
				case "continue":
					attempt.progressOnce.Do(func() { close(attempt.progress) })
				case "end":
					attempt.endOnce.Do(func() { close(attempt.startupEnd) })
				}
			})
			if r.lifecycleCtx.Err() == nil {
				_, _ = io.Copy(io.Discard, reader)
			}
		}(attempt.streams.Stdout)
	}
	if attempt.streams.Stderr != nil {
		attempt.drainWG.Add(1)
		go func(reader io.ReadCloser) {
			defer attempt.drainWG.Done()
			defer reader.Close()
			_, _ = io.Copy(attempt.stderrBuffer, reader)
		}(attempt.streams.Stderr)
	}
}

func (r *FFmpegRecorder) watchAttempt(attempt *recorderAttempt) {
	_ = attempt.process.wait(context.Background())
	if attempt.streams.Stdout != nil {
		_ = attempt.streams.Stdout.Close()
	}
	if attempt.streams.Stderr != nil {
		_ = attempt.streams.Stderr.Close()
	}
	attempt.drainWG.Wait()
	_ = attempt.process.close()
	stderrSummary := ""
	if attempt.stderrBuffer != nil {
		stderrSummary = attempt.stderrBuffer.Snapshot()
	}

	r.mu.Lock()
	attempt.exitCode = classifyRecorderExit(stderrSummary)
	isCurrent := r.current == attempt
	unexpected := isCurrent && !attempt.expected && !attempt.starting && !r.stopping && !r.stopped
	if isCurrent {
		r.current = nil
	}
	if unexpected {
		r.hadUncleanExit = true
		r.lastUnexpectedAttemptID = attempt.id
		event := RecorderEvent{
			Kind: RecorderEventProcessExited, AttemptID: attempt.id,
			ErrorCode:  attempt.exitCode,
			OccurredAt: r.dependencies.now().UTC().UnixMilli(),
		}
		r.enqueueLatestEventLocked(event)
	}
	r.mu.Unlock()
	close(attempt.finished)
}

func classifyRecorderExit(summary string) string {
	normalized := strings.ToLower(summary)
	switch {
	case containsRecorderExitMarker(normalized,
		"http error 401", "http error 403", "http error 404", "http error 410",
		"server returned 401", "server returned 403", "server returned 404", "server returned 410",
		"end of file"):
		return RecorderStreamExpiredErrorCode
	case containsRecorderExitMarker(normalized,
		"connection reset", "connection timed out", "network timeout", "operation timed out"):
		return RecorderNetworkFailureErrorCode
	case containsRecorderExitMarker(normalized, "invalid data", "unsupported"):
		return RecorderUnsupportedInputErrorCode
	case containsRecorderExitMarker(normalized, "no space left on device", "permission denied"):
		return RecorderLocalResourceErrorCode
	case containsRecorderExitMarker(normalized,
		".dll", "shared library", "shared libraries", "error while loading",
		"failed to load library", "could not load library"):
		return RecorderDependencyFailureErrorCode
	default:
		return RecorderProcessExitedErrorCode
	}
}

func recorderErrorForExitCode(code string) error {
	switch code {
	case RecorderStreamExpiredErrorCode:
		return ErrRecorderStreamExpired
	case RecorderNetworkFailureErrorCode:
		return ErrRecorderNetworkFailure
	case RecorderUnsupportedInputErrorCode:
		return ErrRecorderUnsupportedInput
	case RecorderLocalResourceErrorCode:
		return ErrRecorderLocalResource
	case RecorderDependencyFailureErrorCode:
		return ErrRecorderDependencyFailure
	default:
		return ErrRecorderProcessExited
	}
}

func containsRecorderExitMarker(value string, markers ...string) bool {
	for _, marker := range markers {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

// enqueueLatestEventLocked keeps the channel bounded while ensuring a newly
// observed exit is not silently discarded. When full, the oldest queued exit
// is coalesced away and the latest correlation event is retained.
func (r *FFmpegRecorder) enqueueLatestEventLocked(event RecorderEvent) {
	select {
	case r.events <- event:
		return
	default:
	}
	select {
	case <-r.events:
	default:
	}
	r.events <- event
}

func (r *FFmpegRecorder) detachCurrentLocked() *recorderAttempt {
	attempt := r.current
	if attempt != nil {
		attempt.expected = true
		r.current = nil
	}
	return attempt
}

func (r *FFmpegRecorder) shutdownAttempt(ctx context.Context, attempt *recorderAttempt) (bool, error) {
	if attempt == nil {
		return true, nil
	}
	quitErr := attempt.process.writeQuit()
	if waitRecorderStage(ctx, attempt.process.done(), r.dependencies.gracefulTimeout) {
		waitErr := attempt.process.wait(context.Background())
		if err := waitRecorderAttemptFinished(ctx, attempt); err != nil {
			return false, err
		}
		return quitErr == nil && waitErr == nil, nil
	}
	treeErr := attempt.process.terminateTree()
	if waitRecorderStage(ctx, attempt.process.done(), r.dependencies.terminateTimeout) {
		_ = attempt.process.wait(context.Background())
		if err := waitRecorderAttemptFinished(ctx, attempt); err != nil {
			return false, err
		}
		if treeErr != nil {
			return false, ErrRecorderStop
		}
		return false, nil
	}
	processErr := attempt.process.terminateProcess()
	closeErr := attempt.process.close()
	finishedErr := waitRecorderAttemptFinished(ctx, attempt)
	if finishedErr != nil {
		return false, finishedErr
	}
	if processErr != nil || treeErr != nil || closeErr != nil {
		return false, ErrRecorderStop
	}
	return false, nil
}

func waitRecorderAttemptFinished(ctx context.Context, attempt *recorderAttempt) error {
	if attempt == nil {
		return nil
	}
	select {
	case <-attempt.finished:
		return nil
	default:
	}
	select {
	case <-attempt.finished:
		return nil
	case <-ctx.Done():
		return errors.Join(ErrRecorderStop, ctx.Err())
	}
}

func waitRecorderStage(ctx context.Context, done <-chan struct{}, timeout time.Duration) bool {
	select {
	case <-done:
		return true
	default:
	}
	if ctx.Err() != nil {
		return false
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

func (r *FFmpegRecorder) closeEventsLocked() {
	if !r.eventsClosed {
		close(r.events)
		r.eventsClosed = true
	}
}

func (r *FFmpegRecorder) release() {
	r.releaseOnce.Do(func() {
		if r.releaseCapacity != nil {
			r.releaseCapacity()
		}
	})
}
