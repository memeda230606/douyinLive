package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	douyinLive "github.com/jwwsjlm/douyinLive/v2"
)

type recorderTestSource struct {
	mu        sync.Mutex
	snapshots [][]douyinLive.ResolvedStream
	errors    []error
	calls     int
}

func (s *recorderTestSource) ResolveStreams() ([]douyinLive.ResolvedStream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.calls
	s.calls++
	var streams []douyinLive.ResolvedStream
	if index < len(s.snapshots) {
		streams = append([]douyinLive.ResolvedStream(nil), s.snapshots[index]...)
	}
	var err error
	if index < len(s.errors) {
		err = s.errors[index]
	}
	return streams, err
}

func (s *recorderTestSource) SubscribeMessage(douyinLive.LiveMessageHandler) string { return "unused" }
func (s *recorderTestSource) Unsubscribe(string)                                    {}

func (s *recorderTestSource) resolveCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type recorderTestProcess struct {
	mu sync.Mutex

	doneCh   chan struct{}
	waitGate <-chan struct{}
	waitErr  error

	quitExits      bool
	terminateExits bool
	treeExits      bool
	closeExits     bool

	quitErr      error
	terminateErr error
	treeErr      error
	closeErr     error

	actions    []string
	signalOnce sync.Once
	closeOnce  sync.Once
}

func newRecorderTestProcess() *recorderTestProcess {
	return &recorderTestProcess{doneCh: make(chan struct{}), closeExits: true}
}

func (p *recorderTestProcess) signal() {
	p.signalOnce.Do(func() { close(p.doneCh) })
}

func (p *recorderTestProcess) log(action string) {
	p.mu.Lock()
	p.actions = append(p.actions, action)
	p.mu.Unlock()
}

func (p *recorderTestProcess) writeQuit() error {
	p.log("q")
	if p.quitExits {
		p.signal()
	}
	return p.quitErr
}

func (p *recorderTestProcess) terminateProcess() error {
	p.log("terminate_process")
	if p.terminateExits {
		p.signal()
	}
	return p.terminateErr
}

func (p *recorderTestProcess) terminateTree() error {
	p.log("terminate_tree")
	if p.treeExits {
		p.signal()
	}
	return p.treeErr
}

func (p *recorderTestProcess) wait(ctx context.Context) error {
	select {
	case <-p.doneCh:
	case <-ctx.Done():
		return ctx.Err()
	}
	if p.waitGate != nil {
		select {
		case <-p.waitGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return p.waitErr
}

func (p *recorderTestProcess) done() <-chan struct{} { return p.doneCh }

func (p *recorderTestProcess) close() error {
	p.closeOnce.Do(func() {
		p.log("close")
		if p.closeExits {
			p.signal()
		}
	})
	return p.closeErr
}

func (p *recorderTestProcess) actionSnapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.actions...)
}

type recorderTestStartResult struct {
	process   recorderProcess
	stdout    string
	stderr    string
	rawStdout bool
	stdoutRC  io.ReadCloser
	stderrRC  io.ReadCloser
	err       error
}

type recorderTestStarter struct {
	mu      sync.Mutex
	results []recorderTestStartResult
	calls   []processConfig
}

func (s *recorderTestStarter) start(ctx context.Context, config processConfig) (recorderProcess, processStreams, error) {
	if err := ctx.Err(); err != nil {
		return nil, processStreams{}, err
	}
	s.mu.Lock()
	index := len(s.calls)
	copyConfig := processConfig{
		Path: config.Path, Args: append([]string(nil), config.Args...),
		Dir: config.Dir, Env: append([]string(nil), config.Env...),
		RecorderAttemptID:    config.RecorderAttemptID,
		RecorderJobNamespace: config.RecorderJobNamespace,
	}
	s.calls = append(s.calls, copyConfig)
	if index >= len(s.results) {
		s.mu.Unlock()
		return nil, processStreams{}, errors.New("unexpected recorder start")
	}
	result := s.results[index]
	s.mu.Unlock()
	if result.err != nil {
		return nil, processStreams{}, result.err
	}
	stdoutText := result.stdout
	if stdoutText == "" && !result.rawStdout {
		stdoutText = "progress=continue\n"
	}
	stdout := result.stdoutRC
	if stdout == nil {
		stdout = io.NopCloser(strings.NewReader(stdoutText))
	}
	stderr := result.stderrRC
	if stderr == nil {
		stderr = io.NopCloser(strings.NewReader(result.stderr))
	}
	return result.process, processStreams{
		Stdout: stdout,
		Stderr: stderr,
	}, nil
}

func (s *recorderTestStarter) configSnapshot() []processConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]processConfig, len(s.calls))
	for index, config := range s.calls {
		result[index] = processConfig{
			Path: config.Path, Args: append([]string(nil), config.Args...),
			Dir: config.Dir, Env: append([]string(nil), config.Env...),
			RecorderAttemptID:    config.RecorderAttemptID,
			RecorderJobNamespace: config.RecorderJobNamespace,
		}
	}
	return result
}

func recorderTestOptions(t *testing.T) recorderOptions {
	t.Helper()
	mediaDirectory := t.TempDir()
	return recorderOptions{
		tools: ffmpegTools{
			ffmpegPath:  filepath.Join(t.TempDir(), "ffmpeg.exe"),
			ffprobePath: filepath.Join(t.TempDir(), "ffprobe.exe"),
		},
		mediaDirectory: mediaDirectory, processNamespace: managedProcessTestNamespace,
		segmentSeconds: defaultRecorderSegmentSeconds,
	}
}

func recorderTestDependencies(starter *recorderTestStarter) recorderDependencies {
	return recorderDependencies{
		startProcess:        starter.start,
		now:                 func() time.Time { return time.Date(2026, 7, 17, 12, 0, 0, 123, time.UTC) },
		maxResolveSnapshots: 2,
		gracefulTimeout:     5 * time.Millisecond,
		terminateTimeout:    5 * time.Millisecond,
		startupWindow:       100 * time.Millisecond,
		eventBuffer:         4,
	}
}

func recorderTestCandidate(id, protocol, quality, codec, rawURL string, bitrate int64) douyinLive.ResolvedStream {
	return douyinLive.ResolvedStream{
		ID: id, Protocol: protocol, QualityKey: quality, Codec: codec,
		Bitrate: bitrate, URL: rawURL, SourcePath: "data.data.0.private_source_path",
	}
}

func recorderTestSession(t *testing.T, suffix string) LiveSession {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("new session UUIDv7: %v", err)
	}
	return LiveSession{
		ID: id.String(), DataPath: "rooms/room-" + suffix + "/sessions/2026/07/" + id.String(),
	}
}

func recorderInputURL(config processConfig) string {
	for index, argument := range config.Args {
		if argument == "-i" && index+1 < len(config.Args) {
			return config.Args[index+1]
		}
	}
	return ""
}

func waitForRecorderTest(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for recorder condition")
}

func TestRecorderRetriesRankedCandidatesAndRefreshesSnapshot(t *testing.T) {
	const secret = "private-query-do-not-log"
	firstSnapshot := []douyinLive.ResolvedStream{
		recorderTestCandidate("h265-flv", "flv", "origin", "h265", "https://retry.example.invalid/h265.flv?token="+secret, 9_000_000),
		recorderTestCandidate("h264-hls", "hls", "origin", "h264", "https://retry.example.invalid/h264.m3u8?token="+secret, 8_000_000),
		recorderTestCandidate("h264-flv", "flv", "origin", "h264", "https://retry.example.invalid/h264.flv?token="+secret, 7_000_000),
	}
	refreshedURL := "https://retry.example.invalid/h264.flv?token=refreshed-" + secret
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{
		firstSnapshot,
		{recorderTestCandidate("refreshed", "flv", "origin", "h264", refreshedURL, 6_000_000)},
	}}
	process := newRecorderTestProcess()
	process.quitExits = true
	starter := &recorderTestStarter{results: []recorderTestStartResult{
		{err: errors.New("start failed with " + secret)},
		{err: errors.New("start failed with " + secret)},
		{err: errors.New("start failed with " + secret)},
		{process: process, stderr: "input https://retry.example.invalid/private?token=" + secret},
	}}
	var releases atomic.Int32
	recorder, err := newFFmpegRecorder(context.Background(), source, recorderTestOptions(t), recorderTestDependencies(starter), func() {
		releases.Add(1)
	})
	if err != nil {
		t.Fatalf("new recorder: %v", err)
	}
	configs := starter.configSnapshot()
	if len(configs) != 4 {
		t.Fatalf("process starts = %d, want 4", len(configs))
	}
	wantURLs := []string{firstSnapshot[2].URL, firstSnapshot[1].URL, firstSnapshot[0].URL, refreshedURL}
	for index, want := range wantURLs {
		if got := recorderInputURL(configs[index]); got != want {
			t.Fatalf("start %d input = %q, want %q", index, got, want)
		}
	}
	if source.resolveCalls() != 2 {
		t.Fatalf("resolve calls = %d, want 2", source.resolveCalls())
	}
	rendered := fmt.Sprint(recorder)
	for _, forbidden := range []string{secret, "retry.example.invalid", configs[0].Path, configs[0].Dir} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("recorder String exposes %q: %s", forbidden, rendered)
		}
	}
	if err := recorder.Stop(context.Background()); err != nil {
		t.Fatalf("stop recorder: %v", err)
	}
	waitForRecorderTest(t, func() bool {
		return containsRecorderAction(process.actionSnapshot(), "close")
	})
	if releases.Load() != 1 {
		t.Fatalf("capacity releases = %d, want 1", releases.Load())
	}
}

func TestRecorderAllCandidatesFailWithBoundedSnapshotsAndRedactedError(t *testing.T) {
	const secret = "all-failed-secret-query"
	snapshot := []douyinLive.ResolvedStream{
		recorderTestCandidate("h264", "flv", "hd", "h264", "https://failure.example.invalid/a.flv?token="+secret, 2),
		recorderTestCandidate("h265", "flv", "hd", "h265", "https://failure.example.invalid/b.flv?token="+secret, 3),
	}
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{snapshot, snapshot, snapshot}}
	results := make([]recorderTestStartResult, 6)
	for index := range results {
		results[index].err = errors.New("unsafe start detail: https://failure.example.invalid/?token=" + secret)
	}
	starter := &recorderTestStarter{results: results}
	dependencies := recorderTestDependencies(starter)
	dependencies.maxResolveSnapshots = 3
	var releases atomic.Int32
	recorder, err := newFFmpegRecorder(context.Background(), source, recorderTestOptions(t), dependencies, func() {
		releases.Add(1)
	})
	if recorder != nil {
		t.Fatal("failed startup returned a recorder")
	}
	if !errors.Is(err, ErrRecorderStart) || !errors.Is(err, ErrRecordingUnavailable) {
		t.Fatalf("error = %v, want stable unavailable/start errors", err)
	}
	for _, forbidden := range []string{secret, "failure.example.invalid", "private_source_path"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("startup error exposes %q: %v", forbidden, err)
		}
	}
	if source.resolveCalls() != 3 || len(starter.configSnapshot()) != 2 {
		t.Fatalf("resolve/start counts = %d/%d, want 3/2 unique URLs", source.resolveCalls(), len(starter.configSnapshot()))
	}
	if releases.Load() != 1 {
		t.Fatalf("capacity releases = %d, want 1", releases.Load())
	}
}

func TestRecorderUnexpectedExitEmitsSafeUUIDv7AndStopIsIdempotent(t *testing.T) {
	const secret = "unexpected-exit-secret"
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{
		recorderTestCandidate("one", "flv", "hd", "h264", "https://exit.example.invalid/live.flv?token="+secret, 1),
	}}}
	process := newRecorderTestProcess()
	process.waitErr = errors.New("wait failed with https://exit.example.invalid/?token=" + secret)
	starter := &recorderTestStarter{results: []recorderTestStartResult{{process: process}}}
	var releases atomic.Int32
	recorder, err := newFFmpegRecorder(context.Background(), source, recorderTestOptions(t), recorderTestDependencies(starter), func() {
		releases.Add(1)
	})
	if err != nil {
		t.Fatalf("new recorder: %v", err)
	}
	process.signal()
	var event RecorderEvent
	select {
	case event = <-recorder.Events():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for exit event")
	}
	parsed, parseErr := uuid.Parse(event.AttemptID)
	if parseErr != nil || parsed.Version() != 7 {
		t.Fatalf("attempt ID = %q, want UUIDv7", event.AttemptID)
	}
	configs := starter.configSnapshot()
	if len(configs) != 1 || configs[0].RecorderAttemptID != event.AttemptID {
		t.Fatalf("managed process attempt correlation = %#v, event = %q", configs, event.AttemptID)
	}
	if event.Kind != RecorderEventProcessExited || event.ErrorCode != RecorderProcessExitedErrorCode {
		t.Fatalf("unexpected event: %#v", event)
	}
	encoded, marshalErr := json.Marshal(event)
	if marshalErr != nil {
		t.Fatalf("marshal event: %v", marshalErr)
	}
	rendered := fmt.Sprint(event)
	for _, forbidden := range []string{secret, "exit.example.invalid", "private_source_path"} {
		if strings.Contains(string(encoded), forbidden) || strings.Contains(rendered, forbidden) {
			t.Fatalf("exit event exposes %q: JSON=%s String=%s", forbidden, encoded, rendered)
		}
	}
	firstErr := recorder.Stop(context.Background())
	if !errors.Is(firstErr, ErrRecorderMediaIncomplete) || errors.Is(firstErr, ErrRecorderStop) {
		t.Fatalf("stop after unexpected exit = %v, want incomplete-only", firstErr)
	}
	secondErr := recorder.Stop(context.Background())
	if !errors.Is(secondErr, ErrRecorderMediaIncomplete) || errors.Is(secondErr, ErrRecorderStop) ||
		secondErr.Error() != firstErr.Error() {
		t.Fatalf("idempotent stop error = %v, first = %v", secondErr, firstErr)
	}
	if releases.Load() != 1 {
		t.Fatalf("capacity releases = %d, want 1", releases.Load())
	}
	if _, open := <-recorder.Events(); open {
		t.Fatal("event stream remains open after Stop")
	}
}

func TestRecorderUnexpectedWindowsRelayResetEmitsNetworkFailure(t *testing.T) {
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{
		recorderTestCandidate("one", "flv", "hd", "h264", "https://reset.example.invalid/live.flv", 1),
	}}}
	process := newRecorderTestProcess()
	starter := &recorderTestStarter{results: []recorderTestStartResult{{
		process: process,
		stderr:  "[tcp @ 0000000000000000] Error number -10054 occurred",
	}}}
	recorder, err := newFFmpegRecorder(
		context.Background(), source, recorderTestOptions(t), recorderTestDependencies(starter), nil,
	)
	if err != nil {
		t.Fatalf("new recorder: %v", err)
	}
	process.signal()
	select {
	case event := <-recorder.Events():
		if event.Kind != RecorderEventProcessExited || event.ErrorCode != RecorderNetworkFailureErrorCode {
			t.Fatalf("unexpected event: %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for network exit event")
	}
	if stopErr := recorder.Stop(context.Background()); !errors.Is(stopErr, ErrRecorderMediaIncomplete) ||
		errors.Is(stopErr, ErrRecorderStop) {
		t.Fatalf("stop after network exit = %v, want incomplete-only", stopErr)
	}
}

func TestClassifyRecorderExitWindowsNetworkErrors(t *testing.T) {
	tests := []struct {
		name    string
		summary string
		want    string
	}{
		{
			name:    "winsock_connection_reset",
			summary: "[tcp @ 0000000000000000] Error number -10054 occurred",
			want:    RecorderNetworkFailureErrorCode,
		},
		{
			name:    "ffmpeg_windows_connect_timeout",
			summary: "[tcp @ 0000000000000000] Connection failed: Error number -138 occurred",
			want:    RecorderNetworkFailureErrorCode,
		},
		{
			name:    "socket_resource_exhaustion_is_not_network_fault",
			summary: "[tcp @ 0000000000000000] Error number -10055 occurred",
			want:    RecorderProcessExitedErrorCode,
		},
		{
			name:    "unknown_numeric_error_is_not_network_fault",
			summary: "Error number -4242 occurred",
			want:    RecorderProcessExitedErrorCode,
		},
		{
			name:    "http_status_keeps_stream_expired_precedence",
			summary: "Server returned 403 Forbidden; Error number -10054 occurred",
			want:    RecorderStreamExpiredErrorCode,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyRecorderExit(test.summary); got != test.want {
				t.Fatalf("classifyRecorderExit() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRecorderRebindRefreshesSameSourceAndStaleAttemptCannotWin(t *testing.T) {
	oldWaitGate := make(chan struct{})
	oldProcess := newRecorderTestProcess()
	oldProcess.quitExits = true
	oldProcess.waitGate = oldWaitGate
	newProcess := newRecorderTestProcess()
	latestProcess := newRecorderTestProcess()
	latestProcess.quitExits = true
	starter := &recorderTestStarter{results: []recorderTestStartResult{
		{process: oldProcess}, {process: newProcess}, {process: latestProcess},
	}}
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{
		{recorderTestCandidate("old", "flv", "hd", "h264", "https://same.example.invalid/old.flv?token=old", 1)},
		{recorderTestCandidate("refreshed", "flv", "hd", "h264", "https://same.example.invalid/refreshed.flv?token=new", 1)},
		{recorderTestCandidate("latest", "flv", "hd", "h264", "https://same.example.invalid/latest.flv?token=latest", 1)},
	}}
	var releases atomic.Int32
	recorder, err := newFFmpegRecorder(context.Background(), source, recorderTestOptions(t), recorderTestDependencies(starter), func() {
		releases.Add(1)
	})
	if err != nil {
		t.Fatalf("new recorder: %v", err)
	}
	rebindResult := make(chan error, 1)
	go func() { rebindResult <- recorder.Rebind(context.Background(), source) }()
	waitForRecorderTest(t, func() bool {
		return containsRecorderAction(oldProcess.actionSnapshot(), "q")
	})
	select {
	case err := <-rebindResult:
		t.Fatalf("Rebind returned before old watcher finished: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(oldWaitGate)
	if err := <-rebindResult; err != nil {
		t.Fatalf("same-source refresh rebind: %v", err)
	}
	configs := starter.configSnapshot()
	if len(configs) != 2 || !strings.Contains(recorderInputURL(configs[1]), "/refreshed.flv") {
		t.Fatalf("same-source rebind did not refresh URL: %+v", configs)
	}
	select {
	case event := <-recorder.Events():
		t.Fatalf("stale attempt emitted event: %#v", event)
	case <-time.After(20 * time.Millisecond):
	}

	newProcess.signal()
	waitForRecorderTest(t, func() bool { return len(recorder.events) == 1 })
	if err := recorder.Rebind(context.Background(), source); err != nil {
		t.Fatalf("rebind after queued exit: %v", err)
	}
	queued := <-recorder.Events()
	if queued.Kind != RecorderEventProcessExited || recorder.IsCurrentEvent(queued) {
		t.Fatalf("queued old event remained current after new bind: %#v", queued)
	}
	configs = starter.configSnapshot()
	if len(configs) != 3 || !strings.Contains(recorderInputURL(configs[2]), "/latest.flv") {
		t.Fatalf("latest same-source URL was not resolved: %+v", configs)
	}
	if err := recorder.Stop(context.Background()); !errors.Is(err, ErrRecorderMediaIncomplete) ||
		errors.Is(err, ErrRecorderStop) || recorderStopCleanupError(err) != nil {
		t.Fatalf("stop after historical unexpected exit = %v, want incomplete-only", err)
	}
	if releases.Load() != 1 {
		t.Fatalf("capacity releases = %d, want 1", releases.Load())
	}
}

func TestRecorderStopEscalatesInOrderAndOnlyOnce(t *testing.T) {
	process := newRecorderTestProcess()
	starter := &recorderTestStarter{results: []recorderTestStartResult{{process: process}}}
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{
		recorderTestCandidate("one", "flv", "hd", "h264", "https://stop.example.invalid/live.flv", 1),
	}}}
	dependencies := recorderTestDependencies(starter)
	dependencies.gracefulTimeout = 2 * time.Millisecond
	dependencies.terminateTimeout = 2 * time.Millisecond
	var releases atomic.Int32
	recorder, err := newFFmpegRecorder(context.Background(), source, recorderTestOptions(t), dependencies, func() {
		releases.Add(1)
	})
	if err != nil {
		t.Fatalf("new recorder: %v", err)
	}
	firstErr := recorder.Stop(context.Background())
	if !errors.Is(firstErr, ErrRecorderMediaIncomplete) || errors.Is(firstErr, ErrRecorderStop) {
		t.Fatalf("successful forced stop = %v, want incomplete-only", firstErr)
	}
	waitForRecorderTest(t, func() bool { return len(process.actionSnapshot()) >= 4 })
	wantActions := []string{"q", "terminate_tree", "terminate_process", "close"}
	if got := process.actionSnapshot(); !reflect.DeepEqual(got, wantActions) {
		t.Fatalf("stop actions = %v, want %v", got, wantActions)
	}
	secondErr := recorder.Stop(context.Background())
	if !errors.Is(secondErr, ErrRecorderMediaIncomplete) || errors.Is(secondErr, ErrRecorderStop) ||
		secondErr.Error() != firstErr.Error() {
		t.Fatalf("second stop = %v, first = %v", secondErr, firstErr)
	}
	if got := process.actionSnapshot(); !reflect.DeepEqual(got, wantActions) {
		t.Fatalf("idempotent stop repeated controls: %v", got)
	}
	if releases.Load() != 1 {
		t.Fatalf("capacity releases = %d, want 1", releases.Load())
	}
}

func TestRecorderStopProcessControlFailureRemainsHard(t *testing.T) {
	privateErr := errors.New("private process control detail")
	process := newRecorderTestProcess()
	process.treeErr = privateErr
	process.terminateErr = privateErr
	process.closeErr = privateErr
	starter := &recorderTestStarter{results: []recorderTestStartResult{{process: process}}}
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{
		recorderTestCandidate("one", "flv", "hd", "h264", "https://stop-hard.example.invalid/live.flv", 1),
	}}}
	dependencies := recorderTestDependencies(starter)
	dependencies.gracefulTimeout = 2 * time.Millisecond
	dependencies.terminateTimeout = 2 * time.Millisecond
	recorder, err := newFFmpegRecorder(
		context.Background(), source, recorderTestOptions(t), dependencies, nil,
	)
	if err != nil {
		t.Fatalf("new recorder: %v", err)
	}
	stopErr := recorder.Stop(context.Background())
	if !errors.Is(stopErr, ErrRecorderStop) || !errors.Is(stopErr, ErrRecorderMediaIncomplete) ||
		recorderStopCleanupError(stopErr) == nil {
		t.Fatalf("process control failure = %v, want hard quality+cleanup error", stopErr)
	}
	if strings.Contains(stopErr.Error(), privateErr.Error()) {
		t.Fatalf("process control failure leaked private detail: %v", stopErr)
	}
}

func TestFFmpegRecorderFactoryBoundsLifetimeAndReturnsRedactedDependencyInfo(t *testing.T) {
	const secretURL = "https://dependency.example.invalid/?token=secret"
	secretPath := filepath.Join(t.TempDir(), "private-ffmpeg.exe")
	probePath := filepath.Join(t.TempDir(), "private-ffprobe.exe")
	dataRoot := t.TempDir()
	options := FFmpegRecorderFactoryOptions{
		DataRoot: dataRoot, MaxConcurrentRecordings: 1,
		Preference: douyinLive.StreamSelectionPreference{},
	}
	tools := ffmpegTools{
		ffmpegPath: secretPath, ffprobePath: probePath,
		FFmpeg: FFmpegBinaryInfo{
			Version:      "ffmpeg version 8.1 " + secretURL,
			BuildSummary: "configuration: --enable-safe " + secretURL,
			SHA256:       strings.Repeat("a", 64),
		},
		FFprobe: FFmpegBinaryInfo{Version: "ffprobe version 8.1", SHA256: strings.Repeat("b", 64)},
	}
	firstProcess := newRecorderTestProcess()
	firstProcess.quitExits = true
	secondProcess := newRecorderTestProcess()
	secondProcess.quitExits = true
	starter := &recorderTestStarter{results: []recorderTestStartResult{{process: firstProcess}, {process: secondProcess}}}
	dependencies := recorderTestDependencies(starter)
	dependencies.maxResolveSnapshots = 1
	factory, info, err := newFFmpegRecorderFactoryWithTools(options, tools, dependencies)
	if err != nil {
		t.Fatalf("new recorder factory: %v", err)
	}
	encodedInfo, marshalErr := json.Marshal(info)
	if marshalErr != nil {
		t.Fatalf("marshal dependency info: %v", marshalErr)
	}
	for _, forbidden := range []string{secretURL, "dependency.example.invalid", secretPath, "private_source_path"} {
		if strings.Contains(string(encodedInfo), forbidden) || strings.Contains(fmt.Sprint(options), forbidden) {
			t.Fatalf("dependency surface exposes %q: info=%s options=%s", forbidden, encodedInfo, fmt.Sprint(options))
		}
	}
	if !strings.Contains(info.FFmpeg.BuildSummary, "configuration: --enable-safe") ||
		info.FFmpeg.SHA256 != strings.Repeat("a", 64) {
		t.Fatalf("unexpected safe dependency info: %#v", info.FFmpeg)
	}

	firstSource := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{
		recorderTestCandidate("origin", "flv", "origin", "h264", "https://factory.example.invalid/origin.flv", 2),
		recorderTestCandidate("hd", "flv", "hd", "h264", "https://factory.example.invalid/hd.flv", 1),
	}}}
	request := OpenRequest{Profile: RecordingProfile{
		Quality: "high", SegmentMinutes: 10,
	}}
	firstRecorder, err := factory(context.Background(), recorderTestSession(t, "first"), request, firstSource)
	if err != nil {
		t.Fatalf("start first recorder: %v", err)
	}
	secondSource := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{
		recorderTestCandidate("second", "flv", "hd", "h264", "https://factory.example.invalid/second.flv", 1),
	}}}
	if recorder, capacityErr := factory(context.Background(), recorderTestSession(t, "capacity"), request, secondSource); recorder != nil ||
		!errors.Is(capacityErr, ErrRecorderCapacity) || !errors.Is(capacityErr, ErrRecordingUnavailable) {
		t.Fatalf("capacity result = recorder:%v error:%v", recorder, capacityErr)
	}
	configs := starter.configSnapshot()
	if len(configs) != 1 || !strings.Contains(recorderInputURL(configs[0]), "/hd.flv") {
		t.Fatalf("profile quality was not applied: %+v", configs)
	}
	if !validRecorderJobNamespace(configs[0].RecorderJobNamespace) {
		t.Fatal("factory omitted recorder Job namespace")
	}
	if !pathWithinRecorderRoot(dataRoot, configs[0].Dir) {
		t.Fatalf("media directory escaped canonical data root: %q", configs[0].Dir)
	}
	if err := firstRecorder.Stop(context.Background()); err != nil {
		t.Fatalf("stop first recorder: %v", err)
	}
	if err := firstRecorder.Stop(context.Background()); err != nil {
		t.Fatalf("repeat stop first recorder: %v", err)
	}
	secondRecorder, err := factory(context.Background(), recorderTestSession(t, "second"), request, secondSource)
	if err != nil {
		t.Fatalf("capacity was not released: %v", err)
	}
	if err := secondRecorder.Stop(context.Background()); err != nil {
		t.Fatalf("stop second recorder: %v", err)
	}
}

func TestFFmpegRecorderFactoryRejectsUnsafeSessionAndSegmentWithoutLeakingCapacity(t *testing.T) {
	dataRoot := t.TempDir()
	process := newRecorderTestProcess()
	process.quitExits = true
	starter := &recorderTestStarter{results: []recorderTestStartResult{{process: process}}}
	dependencies := recorderTestDependencies(starter)
	dependencies.maxResolveSnapshots = 1
	factory, _, err := newFFmpegRecorderFactoryWithTools(FFmpegRecorderFactoryOptions{
		DataRoot: dataRoot, MaxConcurrentRecordings: 1,
	}, ffmpegTools{
		ffmpegPath:  filepath.Join(t.TempDir(), "ffmpeg.exe"),
		ffprobePath: filepath.Join(t.TempDir(), "ffprobe.exe"),
	}, dependencies)
	if err != nil {
		t.Fatalf("new recorder factory: %v", err)
	}
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{
		recorderTestCandidate("valid", "flv", "hd", "h264", "https://valid.example.invalid/live.flv", 1),
	}}}
	unsafeSession := recorderTestSession(t, "unsafe")
	unsafeSession.DataPath = "../https://secret.example.invalid/?token=private"
	if recorder, unsafeErr := factory(context.Background(), unsafeSession, OpenRequest{}, source); recorder != nil || !errors.Is(unsafeErr, ErrRecorderConfiguration) {
		t.Fatalf("unsafe session result = recorder:%v error:%v", recorder, unsafeErr)
	} else if strings.Contains(unsafeErr.Error(), "secret.example.invalid") || strings.Contains(unsafeErr.Error(), "token=private") {
		t.Fatalf("unsafe path error leaked input: %v", unsafeErr)
	}
	if recorder, segmentErr := factory(context.Background(), recorderTestSession(t, "segment"), OpenRequest{
		Profile: RecordingProfile{SegmentMinutes: 4},
	}, source); recorder != nil || !errors.Is(segmentErr, ErrRecorderConfiguration) {
		t.Fatalf("invalid segment result = recorder:%v error:%v", recorder, segmentErr)
	}
	validRecorder, err := factory(context.Background(), recorderTestSession(t, "valid"), OpenRequest{}, source)
	if err != nil {
		t.Fatalf("rejected calls leaked factory capacity: %v", err)
	}
	if err := validRecorder.Stop(context.Background()); err != nil {
		t.Fatalf("stop valid recorder: %v", err)
	}
}

func TestRecorderCallsContextlessResolverSynchronouslyWithoutAbandoningGoroutine(t *testing.T) {
	entered := make(chan struct{})
	releaseResolver := make(chan struct{})
	starter := &recorderTestStarter{}
	dependencies := recorderTestDependencies(starter)
	dependencies.maxResolveSnapshots = 1
	dependencies.resolveStreams = func(CaptureSource) ([]douyinLive.ResolvedStream, error) {
		close(entered)
		<-releaseResolver
		return nil, errors.New("resolver detail must remain private")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		recorder *FFmpegRecorder
		err      error
	}
	resultCh := make(chan result, 1)
	var releases atomic.Int32
	go func() {
		recorder, err := newFFmpegRecorder(ctx, &recorderTestSource{}, recorderTestOptions(t), dependencies, func() {
			releases.Add(1)
		})
		resultCh <- result{recorder: recorder, err: err}
	}()
	<-entered
	cancel()
	select {
	case <-resultCh:
		t.Fatal("recorder abandoned a still-running contextless resolver")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseResolver)
	resultValue := <-resultCh
	if resultValue.recorder != nil || !errors.Is(resultValue.err, context.Canceled) {
		t.Fatalf("cancelled resolver result = recorder:%v error:%v", resultValue.recorder, resultValue.err)
	}
	if len(starter.configSnapshot()) != 0 || releases.Load() != 1 {
		t.Fatalf("cancelled resolver starts/releases = %d/%d, want 0/1", len(starter.configSnapshot()), releases.Load())
	}
}

func TestRecorderRejectsUnsafeAttemptIDWithoutLeakingIt(t *testing.T) {
	const secretAttempt = "https://attempt.example.invalid/?token=secret-attempt"
	starter := &recorderTestStarter{}
	dependencies := recorderTestDependencies(starter)
	dependencies.maxResolveSnapshots = 1
	dependencies.newAttemptID = func() (string, error) { return secretAttempt, nil }
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{
		recorderTestCandidate("one", "flv", "hd", "h264", "https://input.example.invalid/live.flv?token=private", 1),
	}}}
	recorder, err := newFFmpegRecorder(context.Background(), source, recorderTestOptions(t), dependencies, nil)
	if recorder != nil || !errors.Is(err, ErrRecorderStart) {
		t.Fatalf("unsafe attempt result = recorder:%v error:%v", recorder, err)
	}
	if strings.Contains(err.Error(), secretAttempt) || strings.Contains(err.Error(), "secret-attempt") {
		t.Fatalf("unsafe attempt ID leaked: %v", err)
	}
	if len(starter.configSnapshot()) != 0 {
		t.Fatalf("unsafe attempt ID started %d processes", len(starter.configSnapshot()))
	}
}

func TestRecorderStartupEarlyExitClassificationControlsFallback(t *testing.T) {
	tests := []struct {
		name    string
		stderr  string
		wantErr error
		retry   bool
	}{
		{name: "expired_403", stderr: "Server returned 403 Forbidden", wantErr: ErrRecorderStreamExpired, retry: true},
		{name: "network", stderr: "Connection reset by peer", wantErr: ErrRecorderNetworkFailure, retry: true},
		{name: "unsupported", stderr: "Invalid data found when processing input", wantErr: ErrRecorderUnsupportedInput, retry: true},
		{name: "generic", stderr: "encoder exited without detail", wantErr: ErrRecorderProcessExited, retry: true},
		{name: "local_resource", stderr: "No space left on device", wantErr: ErrRecorderLocalResource},
		{name: "dependency", stderr: "error while loading shared libraries: codec.dll", wantErr: ErrRecorderDependencyFailure},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			first := newRecorderTestProcess()
			first.signal()
			second := newRecorderTestProcess()
			second.quitExits = true
			starter := &recorderTestStarter{results: []recorderTestStartResult{
				{process: first, rawStdout: true, stderr: test.stderr},
				{process: second},
			}}
			source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{
				recorderTestCandidate("first", "flv", "hd", "h264", "https://classification.example.invalid/first.flv?signature=one", 2),
				recorderTestCandidate("second", "flv", "hd", "h264", "https://classification.example.invalid/second.flv?signature=two", 1),
			}}}
			dependencies := recorderTestDependencies(starter)
			dependencies.maxResolveSnapshots = 1
			recorder, err := newFFmpegRecorder(context.Background(), source, recorderTestOptions(t), dependencies, nil)
			if test.retry {
				if err != nil || recorder == nil {
					t.Fatalf("retryable early exit did not fall back: recorder=%v error=%v", recorder, err)
				}
				if got := len(starter.configSnapshot()); got != 2 {
					t.Fatalf("start count = %d, want 2", got)
				}
				if err := recorder.Stop(context.Background()); err != nil {
					t.Fatalf("stop fallback recorder: %v", err)
				}
				return
			}
			if recorder != nil || !errors.Is(err, test.wantErr) || !errors.Is(err, ErrRecordingUnavailable) {
				t.Fatalf("fail-fast result = recorder:%v error:%v, want %v", recorder, err, test.wantErr)
			}
			if len(starter.configSnapshot()) != 1 {
				t.Fatalf("fail-fast category started fallback: %d", len(starter.configSnapshot()))
			}
			if strings.Contains(err.Error(), test.stderr) {
				t.Fatalf("stable startup error exposed stderr: %v", err)
			}
		})
	}
}

func TestRecorderProgressEndWithoutStderrFallsBackAndPreservesAttemptDirectory(t *testing.T) {
	first := newRecorderTestProcess()
	first.quitExits = true
	second := newRecorderTestProcess()
	second.quitExits = true
	starter := &recorderTestStarter{results: []recorderTestStartResult{
		{process: first, stdout: "progress=end\n"},
		{process: second},
	}}
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{
		recorderTestCandidate("ended", "flv", "hd", "h264", "https://end.example.invalid/ended.flv", 2),
		recorderTestCandidate("fallback", "flv", "hd", "h264", "https://end.example.invalid/fallback.flv", 1),
	}}}
	dependencies := recorderTestDependencies(starter)
	dependencies.maxResolveSnapshots = 1
	recorder, err := newFFmpegRecorder(context.Background(), source, recorderTestOptions(t), dependencies, nil)
	if err != nil {
		t.Fatalf("progress=end fallback: %v", err)
	}
	configs := starter.configSnapshot()
	if len(configs) != 2 || configs[0].Dir == configs[1].Dir ||
		configs[0].Args[len(configs[0].Args)-1] == configs[1].Args[len(configs[1].Args)-1] {
		t.Fatalf("attempt outputs are not unique: %+v", configs)
	}
	if info, statErr := os.Stat(configs[0].Dir); statErr != nil || !info.IsDir() {
		t.Fatalf("failed attempt directory was not preserved: %v", statErr)
	}
	if err := recorder.Stop(context.Background()); err != nil {
		t.Fatalf("stop fallback recorder: %v", err)
	}

	only := newRecorderTestProcess()
	only.quitExits = true
	onlyStarter := &recorderTestStarter{results: []recorderTestStartResult{{process: only, stdout: "progress=end\n"}}}
	onlyDependencies := recorderTestDependencies(onlyStarter)
	onlyDependencies.maxResolveSnapshots = 1
	onlySource := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{
		recorderTestCandidate("only", "flv", "hd", "h264", "https://end.example.invalid/only.flv", 1),
	}}}
	if got, onlyErr := newFFmpegRecorder(context.Background(), onlySource, recorderTestOptions(t), onlyDependencies, nil); got != nil ||
		!errors.Is(onlyErr, ErrRecorderStreamExpired) {
		t.Fatalf("single progress=end result = recorder:%v error:%v", got, onlyErr)
	}
}

func TestRecorderStartupTimeoutAfterMalformedProgressAndLate403FallsBack(t *testing.T) {
	first := newRecorderTestProcess()
	stderrReader, stderrWriter := io.Pipe()
	second := newRecorderTestProcess()
	second.quitExits = true
	starter := &recorderTestStarter{results: []recorderTestStartResult{
		{process: first, stdout: "malformed-progress\nprogress=endless\n", stderrRC: stderrReader},
		{process: second},
	}}
	go func() {
		time.Sleep(25 * time.Millisecond)
		_, _ = io.WriteString(stderrWriter, "Server returned 403 Forbidden")
		_ = stderrWriter.Close()
		first.signal()
	}()
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{
		recorderTestCandidate("slow", "flv", "hd", "h264", "https://timeout.example.invalid/slow.flv", 2),
		recorderTestCandidate("fallback", "flv", "hd", "h264", "https://timeout.example.invalid/fallback.flv", 1),
	}}}
	dependencies := recorderTestDependencies(starter)
	dependencies.maxResolveSnapshots = 1
	dependencies.startupWindow = 2 * time.Millisecond
	dependencies.gracefulTimeout = 100 * time.Millisecond
	recorder, err := newFFmpegRecorder(context.Background(), source, recorderTestOptions(t), dependencies, nil)
	if err != nil || recorder == nil {
		t.Fatalf("startup timeout did not fall back: recorder=%v error=%v", recorder, err)
	}
	if len(starter.configSnapshot()) != 2 {
		t.Fatalf("startup timeout starts = %d, want 2", len(starter.configSnapshot()))
	}
	if err := recorder.Stop(context.Background()); err != nil {
		t.Fatalf("stop timeout fallback: %v", err)
	}
}

type recorderSignalReader struct {
	process *recorderTestProcess
	read    bool
}

func (r *recorderSignalReader) Read(buffer []byte) (int, error) {
	if r.read {
		return 0, io.EOF
	}
	r.read = true
	value := []byte("progress=continue\n")
	n := copy(buffer, value)
	r.process.signal()
	return n, nil
}

func (*recorderSignalReader) Close() error { return nil }

func TestRecorderContinueThenImmediateExitCannotCommitFalseActive(t *testing.T) {
	for iteration := 0; iteration < 50; iteration++ {
		first := newRecorderTestProcess()
		second := newRecorderTestProcess()
		second.quitExits = true
		starter := &recorderTestStarter{results: []recorderTestStartResult{
			{process: first, stdoutRC: &recorderSignalReader{process: first}},
			{process: second},
		}}
		source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{
			recorderTestCandidate("racy", "flv", "hd", "h264", "https://race.example.invalid/racy.flv", 2),
			recorderTestCandidate("fallback", "flv", "hd", "h264", "https://race.example.invalid/fallback.flv", 1),
		}}}
		dependencies := recorderTestDependencies(starter)
		dependencies.maxResolveSnapshots = 1
		recorder, err := newFFmpegRecorder(context.Background(), source, recorderTestOptions(t), dependencies, nil)
		if err != nil || recorder == nil {
			t.Fatalf("iteration %d false-active fallback failed: recorder=%v error=%v", iteration, recorder, err)
		}
		recorder.mu.Lock()
		active := recorder.current != nil && !recorder.current.starting
		recorder.mu.Unlock()
		if !active || len(starter.configSnapshot()) != 2 {
			t.Fatalf("iteration %d committed false active state", iteration)
		}
		if err := recorder.Stop(context.Background()); err != nil {
			t.Fatalf("iteration %d stop: %v", iteration, err)
		}
	}
}

func TestRecorderDuplicateAttemptIDCannotReuseOutputDirectory(t *testing.T) {
	fixed, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("new UUIDv7: %v", err)
	}
	process := newRecorderTestProcess()
	process.quitExits = true
	starter := &recorderTestStarter{results: []recorderTestStartResult{{process: process}}}
	dependencies := recorderTestDependencies(starter)
	dependencies.maxResolveSnapshots = 1
	dependencies.newAttemptID = func() (string, error) { return fixed.String(), nil }
	options := recorderTestOptions(t)
	candidate := recorderTestCandidate("same", "flv", "hd", "h264", "https://collision.example.invalid/live.flv", 1)
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{candidate}, {candidate}}}
	first, err := newFFmpegRecorder(context.Background(), source, options, dependencies, nil)
	if err != nil {
		t.Fatalf("first recorder: %v", err)
	}
	attemptDirectory := filepath.Join(options.mediaDirectory, ".attempt-"+fixed.String())
	sentinel := filepath.Join(attemptDirectory, "existing.mkv")
	if err := os.WriteFile(sentinel, []byte("unchanged"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	if err := first.Stop(context.Background()); err != nil {
		t.Fatalf("stop first recorder: %v", err)
	}
	second, secondErr := newFFmpegRecorder(context.Background(), source, options, dependencies, nil)
	if second != nil || !errors.Is(secondErr, ErrRecorderOutput) || !errors.Is(secondErr, ErrRecordingUnavailable) {
		t.Fatalf("duplicate attempt result = recorder:%v error:%v", second, secondErr)
	}
	content, readErr := os.ReadFile(sentinel)
	if readErr != nil || string(content) != "unchanged" || len(starter.configSnapshot()) != 1 {
		t.Fatalf("duplicate attempt changed existing output: content=%q error=%v starts=%d", content, readErr, len(starter.configSnapshot()))
	}
}

func TestRecorderStopTimeoutDefersCapacityReleaseUntilWatcherFinishes(t *testing.T) {
	process := newRecorderTestProcess()
	process.closeExits = false
	starter := &recorderTestStarter{results: []recorderTestStartResult{{process: process}}}
	dependencies := recorderTestDependencies(starter)
	dependencies.maxResolveSnapshots = 1
	dependencies.gracefulTimeout = 50 * time.Millisecond
	dependencies.terminateTimeout = 50 * time.Millisecond
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{
		recorderTestCandidate("one", "flv", "hd", "h264", "https://stop-timeout.example.invalid/live.flv", 1),
	}}}
	var releases atomic.Int32
	recorder, err := newFFmpegRecorder(context.Background(), source, recorderTestOptions(t), dependencies, func() { releases.Add(1) })
	if err != nil {
		t.Fatalf("new recorder: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	firstErr := recorder.Stop(ctx)
	if !errors.Is(firstErr, ErrRecorderStop) || !errors.Is(firstErr, context.DeadlineExceeded) {
		t.Fatalf("timeout stop = %v", firstErr)
	}
	if releases.Load() != 0 {
		t.Fatal("capacity released before watcher finished")
	}
	secondResult := make(chan error, 1)
	go func() { secondResult <- recorder.Stop(context.Background()) }()
	select {
	case err := <-secondResult:
		t.Fatalf("second Stop returned before watcher finished: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	process.signal()
	if err := <-secondResult; !errors.Is(err, ErrRecorderStop) ||
		!errors.Is(err, context.DeadlineExceeded) || recorderStopCleanupError(err) == nil {
		t.Fatalf("second Stop = %v, want persistent cleanup timeout", err)
	}
	waitForRecorderTest(t, func() bool { return releases.Load() == 1 })
}

func TestRecorderGracefulQuitWithNonzeroWaitIsUnclean(t *testing.T) {
	process := newRecorderTestProcess()
	process.quitExits = true
	process.waitErr = errors.New("private process exit detail")
	starter := &recorderTestStarter{results: []recorderTestStartResult{{process: process}}}
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{
		recorderTestCandidate("one", "flv", "hd", "h264", "https://nonzero.example.invalid/live.flv", 1),
	}}}
	recorder, err := newFFmpegRecorder(context.Background(), source, recorderTestOptions(t), recorderTestDependencies(starter), nil)
	if err != nil {
		t.Fatalf("new recorder: %v", err)
	}
	stopErr := recorder.Stop(context.Background())
	if !errors.Is(stopErr, ErrRecorderMediaIncomplete) || errors.Is(stopErr, ErrRecorderStop) ||
		recorderStopCleanupError(stopErr) != nil {
		t.Fatalf("nonzero graceful stop = %v, want incomplete-only", stopErr)
	}
	want := []string{"q", "close"}
	if got := process.actionSnapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("nonzero graceful actions = %v, want %v", got, want)
	}
}

func TestRecorderEventBufferCoalescesOldestForLatest(t *testing.T) {
	first, _ := uuid.NewV7()
	second, _ := uuid.NewV7()
	recorder := &FFmpegRecorder{events: make(chan RecorderEvent, 1)}
	recorder.mu.Lock()
	recorder.enqueueLatestEventLocked(RecorderEvent{Kind: RecorderEventProcessExited, AttemptID: first.String()})
	recorder.enqueueLatestEventLocked(RecorderEvent{Kind: RecorderEventProcessExited, AttemptID: second.String()})
	recorder.lastUnexpectedAttemptID = second.String()
	recorder.mu.Unlock()
	got := <-recorder.Events()
	if got.AttemptID != second.String() || !recorder.IsCurrentEvent(got) {
		t.Fatalf("coalesced event = %#v, want latest/current", got)
	}
}

func TestRecorderFactoryOptionsArePrivateAcrossFormattingJSONAndSlog(t *testing.T) {
	secret := "recorder-private-token"
	options := FFmpegRecorderFactoryOptions{
		DataRoot:       filepath.Join(t.TempDir(), secret),
		ExplicitFFmpeg: filepath.Join(t.TempDir(), secret+"-ffmpeg.exe"),
		ExplicitProbe:  filepath.Join(t.TempDir(), secret+"-ffprobe.exe"),
		BundledDir:     filepath.Join(t.TempDir(), secret+"-bundle"),
		Preference: douyinLive.StreamSelectionPreference{
			QualityKey: secret, Protocol: secret,
		},
	}
	encoded, err := json.Marshal(options)
	if err != nil {
		t.Fatalf("marshal options: %v", err)
	}
	var logOutput bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logOutput, nil))
	logger.Info("options", "value", options)
	rendered := strings.Join([]string{
		fmt.Sprint(options), fmt.Sprintf("%+v", options), fmt.Sprintf("%#v", options), string(encoded), logOutput.String(),
	}, "\n")
	if strings.Contains(rendered, secret) || strings.Contains(rendered, options.DataRoot) {
		t.Fatalf("private factory options leaked: %s", rendered)
	}
}

func TestRecorderFactoryRejectsInvalidBoundsAndPathsBeforeAllocation(t *testing.T) {
	absoluteRoot := t.TempDir()
	tests := []struct {
		name    string
		options FFmpegRecorderFactoryOptions
	}{
		{name: "max_five", options: FFmpegRecorderFactoryOptions{DataRoot: absoluteRoot, MaxConcurrentRecordings: 5}},
		{name: "max_integer", options: FFmpegRecorderFactoryOptions{DataRoot: absoluteRoot, MaxConcurrentRecordings: int(^uint(0) >> 1)}},
		{name: "relative_ffmpeg", options: FFmpegRecorderFactoryOptions{DataRoot: absoluteRoot, ExplicitFFmpeg: "relative/ffmpeg"}},
		{name: "relative_ffprobe", options: FFmpegRecorderFactoryOptions{DataRoot: absoluteRoot, ExplicitProbe: "relative/ffprobe"}},
		{name: "relative_bundle", options: FFmpegRecorderFactoryOptions{DataRoot: absoluteRoot, BundledDir: "relative/bundle"}},
		{name: "percent_data_root", options: FFmpegRecorderFactoryOptions{DataRoot: filepath.Join(absoluteRoot, "%private%")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateRecorderFactoryOptions(context.Background(), test.options); !errors.Is(err, ErrRecorderConfiguration) {
				t.Fatalf("validation error = %v", err)
			}
		})
	}
	options := FFmpegRecorderFactoryOptions{DataRoot: absoluteRoot}
	if factory, _, err := newFFmpegRecorderFactoryWithTools(options, ffmpegTools{
		ffmpegPath: "relative-ffmpeg", ffprobePath: filepath.Join(t.TempDir(), "ffprobe.exe"),
	}, recorderDependencies{}); factory != nil || !errors.Is(err, ErrFFmpegInvalid) {
		t.Fatalf("relative discovered tool result = factory:%v error:%v", factory, err)
	}
}

func TestRecorderFactoryRejectsCustomSaveDirectoryWithoutCreatingItOrLeakingCapacity(t *testing.T) {
	dataRoot := t.TempDir()
	process := newRecorderTestProcess()
	process.quitExits = true
	starter := &recorderTestStarter{results: []recorderTestStartResult{{process: process}}}
	dependencies := recorderTestDependencies(starter)
	dependencies.maxResolveSnapshots = 1
	factory, _, err := newFFmpegRecorderFactoryWithTools(FFmpegRecorderFactoryOptions{
		DataRoot: dataRoot, MaxConcurrentRecordings: 1,
	}, ffmpegTools{
		ffmpegPath:  filepath.Join(t.TempDir(), "ffmpeg.exe"),
		ffprobePath: filepath.Join(t.TempDir(), "ffprobe.exe"),
	}, dependencies)
	if err != nil {
		t.Fatalf("new factory: %v", err)
	}
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{
		recorderTestCandidate("valid", "flv", "hd", "h264", "https://save-directory.example.invalid/live.flv", 1),
	}}}
	customDirectory := filepath.Join(t.TempDir(), "must-not-be-created")
	request := OpenRequest{Profile: RecordingProfile{SaveDirectory: customDirectory}}
	if recorder, openErr := factory(context.Background(), recorderTestSession(t, "custom-save"), request, source); recorder != nil ||
		!errors.Is(openErr, ErrRecorderConfiguration) {
		t.Fatalf("custom save result = recorder:%v error:%v", recorder, openErr)
	}
	if _, statErr := os.Stat(customDirectory); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("custom save directory was created: %v", statErr)
	}
	recorder, err := factory(context.Background(), recorderTestSession(t, "after-custom-save"), OpenRequest{}, source)
	if err != nil {
		t.Fatalf("custom save rejection leaked capacity: %v", err)
	}
	if err := recorder.Stop(context.Background()); err != nil {
		t.Fatalf("stop valid recorder: %v", err)
	}
}

func TestRecorderConstructorFailureDefersReleaseUntilPendingWatcherFinishes(t *testing.T) {
	process := newRecorderTestProcess()
	waitGate := make(chan struct{})
	process.waitGate = waitGate
	stdoutReader, stdoutWriter := io.Pipe()
	defer stdoutWriter.Close()
	starter := &recorderTestStarter{results: []recorderTestStartResult{{
		process: process, rawStdout: true, stdoutRC: stdoutReader,
	}}}
	dependencies := recorderTestDependencies(starter)
	dependencies.maxResolveSnapshots = 1
	dependencies.startupWindow = time.Second
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{
		recorderTestCandidate("pending", "flv", "hd", "h264", "https://pending.example.invalid/live.flv", 1),
	}}}
	ctx, cancel := context.WithCancel(context.Background())
	type constructorResult struct {
		recorder *FFmpegRecorder
		err      error
	}
	resultCh := make(chan constructorResult, 1)
	var releases atomic.Int32
	options := recorderTestOptions(t)
	go func() {
		recorder, err := newFFmpegRecorder(ctx, source, options, dependencies, func() { releases.Add(1) })
		resultCh <- constructorResult{recorder: recorder, err: err}
	}()
	waitForRecorderTest(t, func() bool { return len(starter.configSnapshot()) == 1 })
	cancel()
	result := <-resultCh
	if result.recorder != nil || !errors.Is(result.err, context.Canceled) {
		t.Fatalf("cancelled constructor result = recorder:%v error:%v", result.recorder, result.err)
	}
	if releases.Load() != 0 {
		t.Fatal("constructor failure released capacity before pending watcher")
	}
	close(waitGate)
	waitForRecorderTest(t, func() bool { return releases.Load() == 1 })
	if releases.Load() != 1 {
		t.Fatalf("capacity releases = %d, want exactly 1", releases.Load())
	}
}

func TestRecorderSegmentSeconds(t *testing.T) {
	tests := []struct {
		minutes int
		want    int
		valid   bool
	}{
		{minutes: 0, want: 600, valid: true},
		{minutes: 5, want: 300, valid: true},
		{minutes: 30, want: 1800, valid: true},
		{minutes: 4},
		{minutes: 31},
		{minutes: -1},
	}
	for _, test := range tests {
		t.Run(fmt.Sprintf("minutes_%d", test.minutes), func(t *testing.T) {
			got, err := recorderSegmentSeconds(test.minutes)
			if test.valid {
				if err != nil || got != test.want {
					t.Fatalf("segment seconds = %d, %v; want %d", got, err, test.want)
				}
			} else if !errors.Is(err, ErrRecorderConfiguration) {
				t.Fatalf("invalid segment error = %v", err)
			}
		})
	}
}

func containsRecorderAction(actions []string, expected string) bool {
	for _, action := range actions {
		if action == expected {
			return true
		}
	}
	return false
}

func pathWithinRecorderRoot(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
