package capture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	pathpkg "path"
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
	ErrRecorderMediaJournal      = errors.New("RECORDER_MEDIA_JOURNAL_FAILED")
	ErrRecorderMediaIncomplete   = errors.New("RECORDER_MEDIA_INCOMPLETE")
	ErrRecorderProcessExited     = errors.New(RecorderProcessExitedErrorCode)
	ErrRecorderStreamExpired     = errors.New(RecorderStreamExpiredErrorCode)
	ErrRecorderNetworkFailure    = errors.New(RecorderNetworkFailureErrorCode)
	ErrRecorderUnsupportedInput  = errors.New(RecorderUnsupportedInputErrorCode)
	ErrRecorderLocalResource     = errors.New(RecorderLocalResourceErrorCode)
	ErrRecorderDependencyFailure = errors.New(RecorderDependencyFailureErrorCode)
)

const (
	defaultRecorderConcurrency        = 1
	defaultRecorderResolveSnapshots   = 2
	defaultRecorderSegmentSeconds     = 600
	defaultRecorderGracefulTimeout    = 5 * time.Second
	defaultRecorderTerminateTimeout   = 3 * time.Second
	defaultRecorderStartupWindow      = 8 * time.Second
	defaultRecorderEventBuffer        = 16
	recorderStderrBufferBytes         = 64 << 10
	defaultRecorderCandidates         = 64
	maximumRecorderResolveCandidates  = 4096
	defaultMediaFinalizeTimeout       = 30 * time.Minute
	defaultMediaAttemptJournalTimeout = 10 * time.Second
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
	RecordingRoot           string
	ExplicitFFmpeg          string
	ExplicitProbe           string
	BundledDir              string
	MaxConcurrentRecordings int
	Preference              douyinLive.StreamSelectionPreference
	Repository              *SQLiteRepository
}

type FFmpegRecorderComponents struct {
	RecorderFactory RecorderFactory
	MediaRecoverer  SessionMediaRecoverer
	DependencyInfo  FFmpegDependencyInfo
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
	components, err := NewFFmpegRecorderComponents(ctx, options)
	return components.RecorderFactory, components.DependencyInfo, err
}

// NewFFmpegRecorderComponents discovers the trusted FFmpeg pair once and
// shares that immutable dependency identity with live recording and startup
// media recovery.
func NewFFmpegRecorderComponents(
	ctx context.Context,
	options FFmpegRecorderFactoryOptions,
) (FFmpegRecorderComponents, error) {
	var components FFmpegRecorderComponents
	if err := validateRecorderFactoryOptions(ctx, options); err != nil {
		return components, err
	}
	tools, err := discoverFFmpeg(ctx, ffmpegDiscoveryOptions{
		ExplicitFFmpeg: options.ExplicitFFmpeg,
		ExplicitProbe:  options.ExplicitProbe,
		BundledDir:     options.BundledDir,
	})
	if err != nil {
		return components, err
	}
	factory, info, err := newFFmpegRecorderFactoryWithTools(
		options, tools, defaultRecorderDependencies(),
	)
	if err != nil {
		return components, err
	}
	components.RecorderFactory = factory
	components.DependencyInfo = info
	if options.Repository != nil {
		recoverer, recoveryErr := newSQLiteSessionMediaRecoverer(options, tools)
		if recoveryErr != nil {
			return FFmpegRecorderComponents{}, recoveryErr
		}
		components.MediaRecoverer = recoverer
	}
	return components, nil
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
	maxCandidates       int
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
		maxCandidates:       defaultRecorderCandidates,
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
	if dependencies.maxCandidates == 0 {
		dependencies.maxCandidates = defaults.maxCandidates
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
		dependencies.maxCandidates < 1 || dependencies.maxCandidates > defaultRecorderCandidates ||
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
		!validRecorderFactoryPath(options.RecordingRoot, false, true) ||
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
	processNamespace, valid := recorderJobNamespace(dataRoot)
	if !valid {
		return nil, FFmpegDependencyInfo{}, ErrRecorderConfiguration
	}
	if options.Repository != nil {
		repositoryNamespace, repositoryValid := recorderJobNamespace(options.Repository.dataRoot)
		if !repositoryValid || repositoryNamespace != processNamespace {
			return nil, FFmpegDependencyInfo{}, ErrRecorderConfiguration
		}
	}
	recordingRoot := options.RecordingRoot
	if recordingRoot == "" {
		recordingRoot = filepath.Join(dataRoot, "rooms")
	}
	capacity := make(chan struct{}, maximum)
	proxyCapacity := make(chan struct{}, 1)
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
		segmentSeconds, segmentErr := recorderSegmentSeconds(request.Profile.SegmentMinutes)
		if segmentErr != nil {
			return nil, segmentErr
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

		var mediaFinalizer SessionMediaFinalizer
		var attemptJournal SessionMediaAttemptJournal
		var mediaDirectory string
		var mediaErr error
		if options.Repository == nil {
			mediaDirectory, mediaErr = recorderMediaDirectory(dataRoot, session)
			if mediaErr == nil && request.Profile.SaveDirectory != "" &&
				!sameRecorderDirectory(request.Profile.SaveDirectory, mediaDirectory) {
				mediaErr = ErrRecorderConfiguration
			}
		} else {
			mediaDirectory, mediaFinalizer, mediaErr = prepareRecorderSessionMedia(ctx, recorderSessionMediaOptions{
				Repository: options.Repository, Tools: tools, DataRoot: dataRoot,
				RecordingRoot: recordingRoot, SaveDirectory: request.Profile.SaveDirectory,
				Session: session, ProxyCapacity: proxyCapacity,
			})
			if mediaErr == nil {
				var ok bool
				attemptJournal, ok = mediaFinalizer.(SessionMediaAttemptJournal)
				if !ok {
					mediaErr = ErrRecorderConfiguration
				}
			}
		}
		if mediaErr != nil {
			release()
			return nil, errors.Join(ErrRecordingUnavailable, mediaErr)
		}
		preference := options.Preference
		preference.QualityKey = request.Profile.Quality
		recorder, recorderErr := newFFmpegRecorder(ctx, source, recorderOptions{
			tools: tools, mediaDirectory: mediaDirectory,
			processNamespace: processNamespace,
			preference:       preference, segmentSeconds: segmentSeconds,
			mediaFinalizer: mediaFinalizer, attemptJournal: attemptJournal,
		}, normalizedDependencies, release)
		if recorderErr != nil {
			return nil, recorderErr
		}
		return recorder, nil
	})
	return factory, info, nil
}

type recorderSessionMediaOptions struct {
	Repository    *SQLiteRepository
	Tools         ffmpegTools
	DataRoot      string
	RecordingRoot string
	SaveDirectory string
	Session       LiveSession
	ProxyCapacity chan struct{}
}

func prepareRecorderSessionMedia(
	ctx context.Context,
	options recorderSessionMediaOptions,
) (string, SessionMediaFinalizer, error) {
	if ctx == nil || options.Repository == nil || options.ProxyCapacity == nil ||
		!validRecorderFactoryPath(options.DataRoot, true, true) ||
		!validRecorderFactoryPath(options.RecordingRoot, true, true) ||
		(options.SaveDirectory != "" && !validRecorderFactoryPath(options.SaveDirectory, true, true)) ||
		validateUUIDv7("recorder media session", options.Session.ID) != nil ||
		!validMediaRelativePath(options.Session.DataPath) {
		return "", nil, ErrRecorderConfiguration
	}
	effectiveRoot := options.RecordingRoot
	if options.SaveDirectory != "" {
		effectiveRoot = options.SaveDirectory
	}
	internalRoomsRoot := filepath.Join(filepath.Clean(options.DataRoot), "rooms")
	logicalRoot := filepath.Clean(options.DataRoot)
	relativePath := options.Session.DataPath
	var rootID *string
	if !sameRecorderDirectory(effectiveRoot, internalRoomsRoot) {
		registered, err := options.Repository.RegisterRecordingRoot(ctx, effectiveRoot)
		if err != nil {
			return "", nil, err
		}
		logicalRoot = registered.absolutePath
		trimmed, ok := strings.CutPrefix(options.Session.DataPath, "rooms/")
		if !ok || !validMediaRelativePath(trimmed) || pathpkg.Clean(trimmed) != trimmed {
			return "", nil, ErrRecorderConfiguration
		}
		relativePath = trimmed
		id := registered.ID
		rootID = &id
	}
	finalizer, err := newSQLiteSessionMediaFinalizer(ctx, sessionMediaFinalizerOptions{
		Repository: options.Repository, Tools: options.Tools, Root: logicalRoot,
		RootID: rootID, SessionID: options.Session.ID, RelativePath: relativePath,
		StartedAt: options.Session.StartedAt, ProxyCapacity: options.ProxyCapacity,
	})
	if err != nil {
		return "", nil, err
	}
	sessionDirectory, err := secureMediaSessionDirectory(logicalRoot, relativePath)
	if err != nil {
		return "", nil, err
	}
	return filepath.Join(sessionDirectory, "media"), finalizer, nil
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
	tools            ffmpegTools
	mediaDirectory   string
	processNamespace string
	preference       douyinLive.StreamSelectionPreference
	segmentSeconds   int
	mediaFinalizer   SessionMediaFinalizer
	attemptJournal   SessionMediaAttemptJournal
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
	progress     chan recorderProgressSample
	attempts     []MediaAttempt
	nextOrdinal  int

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
	ordinal int
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
	mediaIndex   int    // protected by FFmpegRecorder.mu
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
		!validRecorderJobNamespace(options.processNamespace) ||
		options.segmentSeconds < 300 || options.segmentSeconds > 1800 ||
		(options.mediaFinalizer == nil) != (options.attemptJournal == nil) {
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
		source:       source,
		events:       make(chan RecorderEvent, normalizedDependencies.eventBuffer),
		progress:     make(chan recorderProgressSample, 1),
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
			go recorder.completeFailedStart(attempt)
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

func (r *FFmpegRecorder) Progress() <-chan recorderProgressSample {
	if r == nil {
		return nil
	}
	return r.progress
}

func (r *FFmpegRecorder) IsCurrentProgress(progress recorderProgressSample) bool {
	if r == nil || !validRecorderAttemptID(progress.attemptID) || progress.ordinal < 1 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.stopping && !r.stopped && r.current != nil &&
		r.current.id == progress.attemptID &&
		r.current.ordinal == progress.ordinal
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
		journalErr := r.markAttemptClean(previous, graceful)
		shutdownErr = errors.Join(shutdownErr, journalErr)
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
		journalErr := r.markAttemptClean(attempt, graceful)
		if journalErr != nil {
			shutdownErr = errors.Join(shutdownErr, ErrRecorderStop, journalErr)
		}
	}
	if !graceful || previouslyUnclean {
		shutdownErr = errors.Join(shutdownErr, ErrRecorderStop)
	}
	shutdownContextErr := ctx.Err()
	if shutdownContextErr != nil {
		shutdownErr = errors.Join(shutdownErr, ErrRecorderStop, shutdownContextErr)
	}

	go func() {
		if attempt != nil && !recorderAttemptFinished(attempt) {
			<-attempt.finished
		}
		r.completeStop(shutdownErr)
	}()
	r.operationMu.Unlock()
	select {
	case <-r.stopDone:
		r.mu.Lock()
		err := r.stopErr
		r.mu.Unlock()
		return err
	case <-ctx.Done():
		if shutdownContextErr != nil {
			return shutdownErr
		}
		return errors.Join(shutdownErr, ErrRecorderStop, ctx.Err())
	}
}

func (r *FFmpegRecorder) completeStop(shutdownErr error) {
	// Recording capacity covers the FFmpeg process and its drains, not the
	// potentially long proxy generation phase. Proxy work has its own limit.
	r.release()
	finalizeErr := r.finalizeMedia()
	if finalizeErr != nil {
		shutdownErr = errors.Join(shutdownErr, ErrRecorderStop, finalizeErr)
	}
	r.finishStop(shutdownErr)
}

func (r *FFmpegRecorder) completeFailedStart(attempt *recorderAttempt) {
	if attempt != nil && !recorderAttemptFinished(attempt) {
		<-attempt.finished
	}
	// A failed constructor has no returned owner that can await database or
	// proxy work. Leave the durable open attempt for P3-RCV and only converge
	// the process/capacity lifecycle here.
	r.release()
}

func (r *FFmpegRecorder) finalizeMedia() error {
	if r == nil || r.options.mediaFinalizer == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultMediaFinalizeTimeout)
	defer cancel()
	r.mu.Lock()
	attempts := append([]MediaAttempt(nil), r.attempts...)
	r.mu.Unlock()
	result, err := r.options.mediaFinalizer.Finalize(ctx, attempts)
	if err != nil {
		return err
	}
	if result.Snapshot.Session.State == SessionMediaIncomplete {
		return ErrRecorderMediaIncomplete
	}
	return nil
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
		if len(candidates) > maximumRecorderResolveCandidates {
			lastCandidateErr = ErrRecorderStart
			continue
		}
		ranked, rankErr := douyinLive.RankResolvedStreams(candidates, r.options.preference)
		if rankErr != nil {
			continue
		}
		if len(ranked) > r.dependencies.maxCandidates {
			ranked = ranked[:r.dependencies.maxCandidates]
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
				case errors.Is(startErr, ErrRecorderMediaJournal):
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
			if attempt.mediaIndex < 0 || attempt.mediaIndex >= len(r.attempts) {
				cause = ErrRecorderStart
			} else {
				updated := r.attempts[attempt.mediaIndex]
				updated.Committed = true
				// Keep the in-memory lifecycle monotonic while the watcher is
				// blocked on r.mu; a journal failure fails this candidate closed.
				r.attempts[attempt.mediaIndex] = updated
				if err := r.updateMediaAttemptLocked(ctx, updated); err != nil {
					cause = errors.Join(ErrRecorderStart, ErrRecorderMediaJournal, err)
				} else {
					r.source = source
					r.lastUnexpectedAttemptID = ""
					attempt.starting = false
				}
			}
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
	startedAt := r.dependencies.now().UTC().Truncate(time.Millisecond)
	attemptID, err := r.dependencies.newAttemptID()
	if err != nil || !validRecorderAttemptID(attemptID) {
		return nil, ErrRecorderStart
	}
	r.mu.Lock()
	if r.nextOrdinal >= maximumMediaAttempts {
		r.mu.Unlock()
		return nil, ErrRecorderStart
	}
	r.nextOrdinal++
	ordinal := r.nextOrdinal
	r.mu.Unlock()
	mediaAttempt := recorderMediaAttempt(
		candidate, attemptID, ordinal, startedAt, r.options.segmentSeconds,
	)
	mediaIndex, err := r.appendMediaAttempt(ctx, mediaAttempt)
	if err != nil {
		return nil, errors.Join(ErrRecorderStart, ErrRecorderMediaJournal, err)
	}
	attemptDirectory := filepath.Join(r.options.mediaDirectory, ".attempt-"+attemptID)
	if err := os.Mkdir(attemptDirectory, 0o700); err != nil {
		return nil, ErrRecorderOutput
	}
	outputPattern, err := newFFmpegOutputPattern(attemptDirectory, startedAt, attemptID)
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
		RecorderJobNamespace: r.options.processNamespace,
		RecorderAttemptID:    attemptID,
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
		id: attemptID, ordinal: ordinal, process: process, streams: streams,
		starting: true, stderrBuffer: newBoundedTextBuffer(recorderStderrBufferBytes),
		mediaIndex: mediaIndex,
		progress:   make(chan struct{}), startupEnd: make(chan struct{}),
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

	graceful, shutdownErr := r.shutdownAttempt(ctx, attempt)
	journalErr := r.markAttemptClean(attempt, graceful)
	if !recorderAttemptFinished(attempt) {
		r.rememberPendingAttempt(attempt)
	}
	result := errors.Join(cause, journalErr)
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

func recorderMediaAttempt(
	candidate douyinLive.ResolvedStream,
	attemptID string,
	ordinal int,
	startedAt time.Time,
	segmentSeconds int,
) MediaAttempt {
	protocol := strings.ToLower(strings.TrimSpace(candidate.Protocol))
	if !validMediaProtocol(protocol) {
		protocol = "unknown"
	}
	codec := strings.ToLower(strings.TrimSpace(candidate.Codec))
	if !validMediaCodec(codec) {
		codec = "unknown"
	}
	bitrate := candidate.Bitrate
	if bitrate < 0 {
		bitrate = 0
	}
	return MediaAttempt{
		ID: attemptID, Ordinal: ordinal, StartedAt: startedAt.UTC().UnixMilli(),
		SegmentSeconds: segmentSeconds, VariantID: safeMediaAttemptToken(candidate.ID),
		Protocol: protocol, QualityKey: safeMediaAttemptToken(candidate.QualityKey),
		Quality: safeMediaAttemptToken(candidate.Quality), Codec: codec, Bitrate: bitrate,
	}
}

func safeMediaAttemptToken(value string) string {
	value = strings.TrimSpace(value)
	if !validMediaSafeToken(value, true) {
		return ""
	}
	return value
}

func (r *FFmpegRecorder) appendMediaAttempt(ctx context.Context, attempt MediaAttempt) (int, error) {
	if r == nil || ctx == nil {
		return -1, ErrRecorderMediaJournal
	}
	if r.options.attemptJournal != nil {
		if err := r.options.attemptJournal.AppendMediaAttempt(ctx, attempt); err != nil {
			return -1, err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	index := len(r.attempts)
	r.attempts = append(r.attempts, attempt)
	return index, nil
}

// updateMediaAttemptLocked serializes the durable transition with watcher
// state publication. Callers must hold r.mu.
func (r *FFmpegRecorder) updateMediaAttemptLocked(ctx context.Context, attempt MediaAttempt) error {
	if r.options.attemptJournal == nil {
		return nil
	}
	return r.options.attemptJournal.UpdateMediaAttempt(ctx, attempt)
}

func (r *FFmpegRecorder) markAttemptClean(attempt *recorderAttempt, clean bool) error {
	if r == nil || attempt == nil || !clean {
		return nil
	}
	journalCtx, cancel := context.WithTimeout(context.Background(), defaultMediaAttemptJournalTimeout)
	defer cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	if attempt.mediaIndex >= 0 && attempt.mediaIndex < len(r.attempts) {
		updated := r.attempts[attempt.mediaIndex]
		updated.Clean = true
		r.attempts[attempt.mediaIndex] = updated
		if err := r.updateMediaAttemptLocked(journalCtx, updated); err != nil {
			return errors.Join(ErrRecorderMediaJournal, err)
		}
		return nil
	}
	return ErrRecorderMediaJournal
}

func (r *FFmpegRecorder) startAttemptDrains(attempt *recorderAttempt) {
	if attempt.streams.Stdout != nil {
		attempt.drainWG.Add(1)
		go func(reader io.ReadCloser) {
			defer attempt.drainWG.Done()
			defer reader.Close()
			_ = readFFmpegProgress(r.lifecycleCtx, reader, func(ffmpegProgress FFmpegProgress) {
				switch ffmpegProgress.State {
				case "continue":
					attempt.progressOnce.Do(func() { close(attempt.progress) })
					elapsedMS := ffmpegProgress.OutTime.Milliseconds()
					segmentCount := int64(0)
					if ffmpegProgress.Frame > 0 || elapsedMS > 0 || ffmpegProgress.TotalSize > 0 {
						segmentDurationMS := int64(r.options.segmentSeconds) * 1000
						segmentCount = 1 + elapsedMS/segmentDurationMS
					}
					// SegmentCount means the number of attempts' segments that
					// have become active according to muxer media time. It is
					// zero until explicit media activity, and is derived without
					// scanning or exposing the output directory.
					r.enqueueLatestProgress(recorderProgressSample{
						attemptID: attempt.id, ordinal: attempt.ordinal,
						elapsedMS: elapsedMS, bytesWritten: ffmpegProgress.TotalSize,
						segmentCount: segmentCount,
						frame:        ffmpegProgress.Frame, fps: ffmpegProgress.FPS,
						speed:     ffmpegProgress.Speed,
						updatedAt: nonNegativeRecorderUnixMilli(r.dependencies.now()),
					})
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
	close(attempt.finished)
	r.mu.Unlock()
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

func (r *FFmpegRecorder) enqueueLatestProgress(progress recorderProgressSample) {
	select {
	case r.progress <- progress:
		return
	default:
	}
	select {
	case <-r.progress:
	default:
	}
	select {
	case r.progress <- progress:
	default:
	}
}

func nonNegativeRecorderUnixMilli(value time.Time) int64 {
	milliseconds := value.UTC().UnixMilli()
	if milliseconds < 0 {
		return 0
	}
	return milliseconds
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
