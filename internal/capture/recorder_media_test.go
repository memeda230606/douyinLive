package capture

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	douyinLive "github.com/jwwsjlm/douyinLive/v2"
	"github.com/jwwsjlm/douyinLive/v2/internal/storage"
)

type recorderMediaFinalizerStub struct {
	mu                 sync.Mutex
	attempts           []MediaAttempt
	journal            []MediaAttempt
	journalTransitions []MediaAttempt
	appendErr          error
	updateErr          error
	result             MediaFinalizeResult
	err                error
	started            chan struct{}
	unblock            <-chan struct{}
	once               sync.Once
	finalizeCalls      int
}

func (stub *recorderMediaFinalizerStub) Finalize(
	ctx context.Context,
	attempts []MediaAttempt,
) (MediaFinalizeResult, error) {
	stub.mu.Lock()
	stub.attempts = append([]MediaAttempt(nil), attempts...)
	stub.finalizeCalls++
	stub.mu.Unlock()
	if stub.started != nil {
		stub.once.Do(func() { close(stub.started) })
	}
	if stub.unblock != nil {
		select {
		case <-stub.unblock:
		case <-ctx.Done():
			return MediaFinalizeResult{}, ctx.Err()
		}
	}
	return stub.result, stub.err
}

func (stub *recorderMediaFinalizerStub) snapshot() []MediaAttempt {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	return append([]MediaAttempt(nil), stub.attempts...)
}

func (stub *recorderMediaFinalizerStub) AppendMediaAttempt(
	ctx context.Context,
	attempt MediaAttempt,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.appendErr != nil {
		return stub.appendErr
	}
	for _, existing := range stub.journal {
		if existing.ID == attempt.ID || existing.Ordinal == attempt.Ordinal {
			return ErrMediaSnapshotConflict
		}
	}
	stub.journal = append(stub.journal, attempt)
	stub.journalTransitions = append(stub.journalTransitions, attempt)
	return nil
}

func (stub *recorderMediaFinalizerStub) UpdateMediaAttempt(
	ctx context.Context,
	attempt MediaAttempt,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.updateErr != nil {
		return stub.updateErr
	}
	for index := range stub.journal {
		if stub.journal[index].ID != attempt.ID {
			continue
		}
		stub.journal[index] = attempt
		stub.journalTransitions = append(stub.journalTransitions, attempt)
		return nil
	}
	return ErrMediaSnapshotConflict
}

func (stub *recorderMediaFinalizerStub) journalSnapshot() []MediaAttempt {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	return append([]MediaAttempt(nil), stub.journal...)
}

func (stub *recorderMediaFinalizerStub) transitionSnapshot() []MediaAttempt {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	return append([]MediaAttempt(nil), stub.journalTransitions...)
}

func (stub *recorderMediaFinalizerStub) finalizeCallCount() int {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	return stub.finalizeCalls
}

type recorderMediaJournalFinalizer struct {
	journal SessionMediaAttemptJournal
}

func (finalizer *recorderMediaJournalFinalizer) AppendMediaAttempt(
	ctx context.Context,
	attempt MediaAttempt,
) error {
	return finalizer.journal.AppendMediaAttempt(ctx, attempt)
}

func (finalizer *recorderMediaJournalFinalizer) UpdateMediaAttempt(
	ctx context.Context,
	attempt MediaAttempt,
) error {
	return finalizer.journal.UpdateMediaAttempt(ctx, attempt)
}

func (*recorderMediaJournalFinalizer) Finalize(
	context.Context,
	[]MediaAttempt,
) (MediaFinalizeResult, error) {
	return MediaFinalizeResult{
		Snapshot: MediaSnapshot{Session: SessionMedia{State: SessionMediaCompleted}},
	}, nil
}

func TestRecorderPersistsURLFreeCommittedCleanAttemptMetadata(t *testing.T) {
	process := newRecorderTestProcess()
	process.quitExits = true
	starter := &recorderTestStarter{results: []recorderTestStartResult{{process: process}}}
	dependencies := recorderTestDependencies(starter)
	startedAt := time.Date(2026, 7, 17, 12, 34, 56, 123456789, time.UTC)
	dependencies.now = func() time.Time { return startedAt }
	attemptID, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	dependencies.newAttemptID = func() (string, error) { return attemptID.String(), nil }
	finalizer := &recorderMediaFinalizerStub{result: MediaFinalizeResult{
		Snapshot: MediaSnapshot{Session: SessionMedia{State: SessionMediaCompleted}},
	}}
	options := recorderTestOptions(t)
	baseStartProcess := dependencies.startProcess
	dependencies.startProcess = func(
		ctx context.Context,
		config processConfig,
	) (recorderProcess, processStreams, error) {
		transitions := finalizer.transitionSnapshot()
		if len(transitions) != 1 || transitions[0].Committed || transitions[0].Clean {
			return nil, processStreams{}, errors.New("attempt was not durably journaled before process start")
		}
		return baseStartProcess(ctx, config)
	}
	options.mediaFinalizer = finalizer
	options.attemptJournal = finalizer
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{{
		ID: "stream-safe", Protocol: "flv", QualityKey: "hd", Quality: "超清",
		Codec: "h264", Bitrate: 2_500_000,
		URL:        "https://private.example.invalid/live.flv?token=secret",
		SourcePath: "data.data.0.private_source_path",
	}}}}
	recorder, err := newFFmpegRecorder(context.Background(), source, options, dependencies, nil)
	if err != nil {
		t.Fatalf("new recorder: %v", err)
	}
	if err := recorder.Stop(context.Background()); err != nil {
		t.Fatalf("stop recorder: %v", err)
	}
	attempts := finalizer.snapshot()
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(attempts))
	}
	journal := finalizer.journalSnapshot()
	if len(journal) != 1 || !journal[0].Committed || !journal[0].Clean {
		t.Fatalf("durable journal = %+v, want committed clean attempt", journal)
	}
	transitions := finalizer.transitionSnapshot()
	if len(transitions) != 3 ||
		transitions[0].Committed || transitions[0].Clean ||
		!transitions[1].Committed || transitions[1].Clean ||
		!transitions[2].Committed || !transitions[2].Clean {
		t.Fatalf("journal transitions = %+v, want open -> committed -> clean", transitions)
	}
	attempt := attempts[0]
	if attempt.ID != attemptID.String() || attempt.Ordinal != 1 ||
		attempt.StartedAt != startedAt.Truncate(time.Millisecond).UnixMilli() ||
		attempt.SegmentSeconds != defaultRecorderSegmentSeconds ||
		!attempt.Committed || !attempt.Clean || attempt.VariantID != "stream-safe" ||
		attempt.Protocol != "flv" || attempt.QualityKey != "hd" ||
		attempt.Quality != "超清" || attempt.Codec != "h264" || attempt.Bitrate != 2_500_000 {
		t.Fatalf("unexpected attempt: %+v", attempt)
	}
	payload, err := json.Marshal(attempts)
	if err != nil {
		t.Fatalf("marshal attempts: %v", err)
	}
	if strings.Contains(string(payload), "private.example.invalid") ||
		strings.Contains(string(payload), "token") || strings.Contains(string(payload), "private_source_path") {
		t.Fatalf("attempt metadata leaked private input: %s", payload)
	}
	configs := starter.configSnapshot()
	if len(configs) != 1 {
		t.Fatalf("start configs = %d, want 1", len(configs))
	}
	wantStamp := startedAt.Truncate(time.Millisecond).Format("20060102T150405.000000000Z")
	foundPattern := false
	for _, argument := range configs[0].Args {
		if strings.Contains(filepath.Base(argument), wantStamp+"-"+attemptID.String()+".mkv.partial") {
			foundPattern = true
		}
	}
	if !foundPattern {
		t.Fatalf("FFmpeg output pattern does not contain millisecond-aligned stamp %q", wantStamp)
	}
}

func TestRecorderMapsIncompleteMediaToStableStopError(t *testing.T) {
	process := newRecorderTestProcess()
	process.quitExits = true
	starter := &recorderTestStarter{results: []recorderTestStartResult{{process: process}}}
	options := recorderTestOptions(t)
	finalizer := &recorderMediaFinalizerStub{result: MediaFinalizeResult{
		Snapshot: MediaSnapshot{Session: SessionMedia{State: SessionMediaIncomplete}},
	}}
	options.mediaFinalizer = finalizer
	options.attemptJournal = finalizer
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{
		recorderTestCandidate("one", "flv", "hd", "h264", "https://incomplete.example.invalid/live.flv", 1),
	}}}
	recorder, err := newFFmpegRecorder(
		context.Background(), source, options, recorderTestDependencies(starter), nil,
	)
	if err != nil {
		t.Fatalf("new recorder: %v", err)
	}
	stopErr := recorder.Stop(context.Background())
	if !errors.Is(stopErr, ErrRecorderMediaIncomplete) || errors.Is(stopErr, ErrRecorderStop) {
		t.Fatalf("stop error = %v, want incomplete-only", stopErr)
	}
	if recorderStopCleanupError(stopErr) != nil {
		t.Fatalf("incomplete media was classified as cleanup failure: %v", stopErr)
	}
	if got := terminalRecordingStatus(RecordingActive, stopErr); got != RecordingIncomplete {
		t.Fatalf("terminal recording status = %s, want %s", got, RecordingIncomplete)
	}
}

func TestRecorderMapsMediaFinalizeFailureToCleanupError(t *testing.T) {
	process := newRecorderTestProcess()
	process.quitExits = true
	starter := &recorderTestStarter{results: []recorderTestStartResult{{process: process}}}
	privateErr := errors.New("private media finalize detail")
	finalizer := &recorderMediaFinalizerStub{err: privateErr}
	options := recorderTestOptions(t)
	options.mediaFinalizer = finalizer
	options.attemptJournal = finalizer
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{
		recorderTestCandidate("one", "flv", "hd", "h264", "https://finalize-error.example.invalid/live.flv", 1),
	}}}
	recorder, err := newFFmpegRecorder(
		context.Background(), source, options, recorderTestDependencies(starter), nil,
	)
	if err != nil {
		t.Fatalf("new recorder: %v", err)
	}
	stopErr := recorder.Stop(context.Background())
	if !errors.Is(stopErr, ErrRecorderStop) || recorderStopCleanupError(stopErr) == nil {
		t.Fatalf("media finalize failure = %v, want cleanup failure", stopErr)
	}
	if strings.Contains(stopErr.Error(), privateErr.Error()) {
		t.Fatalf("media finalize failure leaked private detail: %v", stopErr)
	}
}

func TestRecorderReleasesRecordingCapacityBeforeProxyFinalization(t *testing.T) {
	process := newRecorderTestProcess()
	process.quitExits = true
	starter := &recorderTestStarter{results: []recorderTestStartResult{{process: process}}}
	unblock := make(chan struct{})
	finalizer := &recorderMediaFinalizerStub{
		result:  MediaFinalizeResult{Snapshot: MediaSnapshot{Session: SessionMedia{State: SessionMediaCompleted}}},
		started: make(chan struct{}), unblock: unblock,
	}
	options := recorderTestOptions(t)
	options.mediaFinalizer = finalizer
	options.attemptJournal = finalizer
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{
		recorderTestCandidate("one", "flv", "hd", "h264", "https://capacity.example.invalid/live.flv", 1),
	}}}
	var releases atomic.Int32
	recorder, err := newFFmpegRecorder(
		context.Background(), source, options, recorderTestDependencies(starter),
		func() { releases.Add(1) },
	)
	if err != nil {
		t.Fatalf("new recorder: %v", err)
	}
	stopResult := make(chan error, 1)
	go func() { stopResult <- recorder.Stop(context.Background()) }()
	select {
	case <-finalizer.started:
	case <-time.After(time.Second):
		t.Fatal("media finalizer did not start")
	}
	if got := releases.Load(); got != 1 {
		t.Fatalf("capacity releases before finalization = %d, want 1", got)
	}
	select {
	case err := <-stopResult:
		t.Fatalf("Stop returned while proxy finalization was blocked: %v", err)
	default:
	}
	close(unblock)
	if err := <-stopResult; err != nil {
		t.Fatalf("Stop after finalization = %v", err)
	}
}

func TestRecorderAttemptJournalAppendFailurePreventsProcessLaunch(t *testing.T) {
	finalizer := &recorderMediaFinalizerStub{appendErr: errors.New("private sqlite failure")}
	options := recorderTestOptions(t)
	options.mediaFinalizer = finalizer
	options.attemptJournal = finalizer
	starter := &recorderTestStarter{}
	dependencies := recorderTestDependencies(starter)
	dependencies.maxResolveSnapshots = 1
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{
		recorderTestCandidate("one", "flv", "hd", "h264", "https://journal.example.invalid/live.flv", 1),
	}}}

	recorder, err := newFFmpegRecorder(context.Background(), source, options, dependencies, nil)
	if recorder != nil || !errors.Is(err, ErrRecordingUnavailable) ||
		!errors.Is(err, ErrRecorderMediaJournal) {
		t.Fatalf("new recorder = %v, %v; want journal fail-closed", recorder, err)
	}
	if got := len(starter.configSnapshot()); got != 0 {
		t.Fatalf("process starts after journal failure = %d, want 0", got)
	}
	if got := len(finalizer.journalSnapshot()); got != 0 {
		t.Fatalf("journal attempts after append failure = %d, want 0", got)
	}
	if got := finalizer.finalizeCallCount(); got != 0 {
		t.Fatalf("finalizer calls after constructor failure = %d, want 0", got)
	}
	entries, readErr := os.ReadDir(options.mediaDirectory)
	if readErr != nil {
		t.Fatalf("read media directory: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("media side effects before durable journal = %d entries, want 0", len(entries))
	}
}

func TestRecorderConstructorFailureLeavesDurableAttemptForRecoveryWithoutFinalizer(t *testing.T) {
	finalizer := &recorderMediaFinalizerStub{
		started: make(chan struct{}),
		result: MediaFinalizeResult{Snapshot: MediaSnapshot{
			Session: SessionMedia{State: SessionMediaCompleted},
		}},
	}
	options := recorderTestOptions(t)
	options.mediaFinalizer = finalizer
	options.attemptJournal = finalizer
	starter := &recorderTestStarter{results: []recorderTestStartResult{{
		err: errors.New("private process failure"),
	}}}
	dependencies := recorderTestDependencies(starter)
	dependencies.maxResolveSnapshots = 1
	dependencies.maxCandidates = 1
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{
		recorderTestCandidate("one", "flv", "hd", "h264", "https://recovery.example.invalid/live.flv", 1),
	}}}
	var releases atomic.Int32

	recorder, err := newFFmpegRecorder(
		context.Background(), source, options, dependencies,
		func() { releases.Add(1) },
	)
	if recorder != nil || !errors.Is(err, ErrRecorderStart) {
		t.Fatalf("new recorder = %v, %v; want process start failure", recorder, err)
	}
	journal := finalizer.journalSnapshot()
	if len(journal) != 1 || journal[0].Committed || journal[0].Clean {
		t.Fatalf("durable recovery journal = %+v, want one open attempt", journal)
	}
	select {
	case <-finalizer.started:
		t.Fatal("constructor failure launched an unowned media finalizer")
	case <-time.After(25 * time.Millisecond):
	}
	if got := finalizer.finalizeCallCount(); got != 0 {
		t.Fatalf("finalizer calls = %d, want 0", got)
	}
	if got := releases.Load(); got != 1 {
		t.Fatalf("capacity releases = %d, want 1", got)
	}
}

func TestRecorderAttemptCommitJournalFailureStopsProcessAndPreservesOpenAttempt(t *testing.T) {
	process := newRecorderTestProcess()
	process.quitExits = true
	starter := &recorderTestStarter{results: []recorderTestStartResult{{process: process}}}
	finalizer := &recorderMediaFinalizerStub{updateErr: errors.New("private sqlite failure")}
	options := recorderTestOptions(t)
	options.mediaFinalizer = finalizer
	options.attemptJournal = finalizer
	dependencies := recorderTestDependencies(starter)
	dependencies.maxResolveSnapshots = 1
	dependencies.maxCandidates = 1
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{
		recorderTestCandidate("one", "flv", "hd", "h264", "https://commit.example.invalid/live.flv", 1),
	}}}

	recorder, err := newFFmpegRecorder(context.Background(), source, options, dependencies, nil)
	if recorder != nil || !errors.Is(err, ErrRecorderMediaJournal) {
		t.Fatalf("new recorder = %v, %v; want commit journal failure", recorder, err)
	}
	journal := finalizer.journalSnapshot()
	if len(journal) != 1 || journal[0].Committed || journal[0].Clean {
		t.Fatalf("journal after failed commit = %+v, want durable open attempt", journal)
	}
	if got := process.actionSnapshot(); len(got) == 0 || got[0] != "q" {
		t.Fatalf("process shutdown actions = %v, want graceful stop first", got)
	}
	if got := finalizer.finalizeCallCount(); got != 0 {
		t.Fatalf("finalizer calls after constructor failure = %d, want 0", got)
	}
}

func TestRecorderRanksBeforeCandidateCapSoSixtyFifthValidCandidateIsAttempted(t *testing.T) {
	streams := make([]douyinLive.ResolvedStream, 65)
	for index := 0; index < 64; index++ {
		streams[index] = recorderTestCandidate(
			"invalid", "flv", "hd", "h264", "not-a-stream-url", int64(index+1),
		)
	}
	validURL := "https://rank-before-cap.example.invalid/live.flv"
	streams[64] = recorderTestCandidate("valid", "flv", "hd", "h264", validURL, 1)
	process := newRecorderTestProcess()
	process.quitExits = true
	starter := &recorderTestStarter{results: []recorderTestStartResult{{process: process}}}
	dependencies := recorderTestDependencies(starter)
	dependencies.maxResolveSnapshots = 1
	dependencies.maxCandidates = 1
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{streams}}

	recorder, err := newFFmpegRecorder(
		context.Background(), source, recorderTestOptions(t), dependencies, nil,
	)
	if err != nil {
		t.Fatalf("new recorder: %v", err)
	}
	if err := recorder.Stop(context.Background()); err != nil {
		t.Fatalf("stop recorder: %v", err)
	}
	configs := starter.configSnapshot()
	if len(configs) != 1 || recorderInputURL(configs[0]) != validURL {
		t.Fatalf("attempted configs = %+v, want only 65th valid candidate", configs)
	}
}

func TestRecorderCapsCandidatesPerResolverSnapshot(t *testing.T) {
	const capCandidates = 3
	streams := make([]douyinLive.ResolvedStream, 10)
	results := make([]recorderTestStartResult, capCandidates)
	for index := range streams {
		streams[index] = recorderTestCandidate(
			string(rune('a'+index)), "flv", "hd", "h264",
			"https://bounded.example.invalid/live-"+string(rune('a'+index))+".flv", int64(index+1),
		)
	}
	for index := range results {
		results[index].err = errors.New("private start failure")
	}
	starter := &recorderTestStarter{results: results}
	dependencies := recorderTestDependencies(starter)
	dependencies.maxResolveSnapshots = 1
	dependencies.maxCandidates = capCandidates
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{streams}}
	if recorder, err := newFFmpegRecorder(
		context.Background(), source, recorderTestOptions(t), dependencies, nil,
	); recorder != nil || !errors.Is(err, ErrRecorderStart) {
		t.Fatalf("new recorder = %v, %v; want bounded start failure", recorder, err)
	}
	if got := len(starter.configSnapshot()); got != capCandidates {
		t.Fatalf("candidate starts = %d, want cap %d", got, capCandidates)
	}
}

func TestRecorderDurableAttemptAndPartialSurviveStoreReopenWithoutStop(t *testing.T) {
	ctx := context.Background()
	repository, store, layout, roomID, now := openRepository(t)
	session, err := repository.Create(ctx, CreateSessionInput{
		RoomConfigID: roomID, OperationID: newV7(t), Recording: RecordingPending,
		StartedAt: now,
	})
	if err != nil {
		store.Close()
		t.Fatalf("create session: %v", err)
	}
	process := newRecorderTestProcess()
	starter := &recorderTestStarter{results: []recorderTestStartResult{{process: process}}}
	dependencies := recorderTestDependencies(starter)
	dependencies.maxResolveSnapshots = 1
	var partialPath string
	baseStartProcess := dependencies.startProcess
	dependencies.startProcess = func(
		startCtx context.Context,
		config processConfig,
	) (recorderProcess, processStreams, error) {
		for _, argument := range config.Args {
			if !strings.HasSuffix(argument, ".mkv.partial") || !strings.Contains(argument, "%06d") {
				continue
			}
			partialPath = strings.Replace(argument, "%06d", "000001", 1)
			if writeErr := os.WriteFile(partialPath, []byte("recoverable partial"), 0o600); writeErr != nil {
				return nil, processStreams{}, writeErr
			}
			break
		}
		if partialPath == "" {
			return nil, processStreams{}, errors.New("missing FFmpeg segment pattern")
		}
		return baseStartProcess(startCtx, config)
	}
	toolDirectory := t.TempDir()
	tools := ffmpegTools{
		ffmpegPath: filepath.Join(toolDirectory, "ffmpeg.exe"), ffprobePath: filepath.Join(toolDirectory, "ffprobe.exe"),
	}
	for _, toolPath := range []string{tools.ffmpegPath, tools.ffprobePath} {
		if err := os.WriteFile(toolPath, []byte("verified tool"), 0o600); err != nil {
			store.Close()
			t.Fatalf("write fake tool: %v", err)
		}
	}

	factory, _, err := newFFmpegRecorderFactoryWithTools(FFmpegRecorderFactoryOptions{
		DataRoot: layout.Root, Repository: repository, MaxConcurrentRecordings: 1,
	}, tools, dependencies)
	if err != nil {
		store.Close()
		t.Fatalf("new recorder factory: %v", err)
	}
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{
		recorderTestCandidate("one", "flv", "hd", "h264", "https://crash.example.invalid/live.flv", 1),
	}}}
	recorderValue, err := factory(ctx, session, OpenRequest{}, source)
	if err != nil {
		store.Close()
		t.Fatalf("start recorder: %v", err)
	}
	recorder, ok := recorderValue.(*FFmpegRecorder)
	if !ok {
		store.Close()
		t.Fatalf("recorder type = %T, want *FFmpegRecorder", recorderValue)
	}
	beforeCrash, err := repository.LoadSnapshot(ctx, session.ID)
	if err != nil {
		store.Close()
		t.Fatalf("load attempt journal before crash: %v", err)
	}
	if len(beforeCrash.Session.Attempts) != 1 ||
		!beforeCrash.Session.Attempts[0].Committed || beforeCrash.Session.Attempts[0].Clean {
		store.Close()
		t.Fatalf("attempt journal before crash = %+v", beforeCrash.Session.Attempts)
	}
	process.signal()
	select {
	case event := <-recorder.Events():
		if event.Kind != RecorderEventProcessExited {
			store.Close()
			t.Fatalf("recorder event = %+v, want process exit", event)
		}
	case <-time.After(time.Second):
		store.Close()
		t.Fatal("recorder watcher did not observe simulated hard crash")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close crashed process store: %v", err)
	}

	reopenedStore, err := storage.Open(ctx, layout, storage.OpenOptions{})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopenedStore.Close()
	reopenedRepository, err := newSQLiteRepository(
		reopenedStore.Writer(), reopenedStore.Reader(), layout.Root, func() time.Time { return now },
	)
	if err != nil {
		t.Fatalf("open repository after crash: %v", err)
	}
	afterCrash, err := reopenedRepository.LoadSnapshot(ctx, session.ID)
	if err != nil {
		t.Fatalf("load durable attempt after reopen: %v", err)
	}
	if len(afterCrash.Session.Attempts) != 1 ||
		!afterCrash.Session.Attempts[0].Committed || afterCrash.Session.Attempts[0].Clean {
		t.Fatalf("attempt journal after reopen = %+v", afterCrash.Session.Attempts)
	}
	if info, statErr := os.Stat(partialPath); statErr != nil || info.Size() == 0 {
		t.Fatalf("recoverable partial after reopen: size=%d err=%v", func() int64 {
			if info == nil {
				return 0
			}
			return info.Size()
		}(), statErr)
	}
	sessionDirectory, err := secureMediaSessionDirectory(layout.Root, afterCrash.Session.RelativePath)
	if err != nil {
		t.Fatalf("resolve reopened session directory: %v", err)
	}
	candidates, err := discoverMediaCandidates(
		sessionDirectory, afterCrash.Session.RelativePath, afterCrash.Session.Attempts,
	)
	if err != nil || len(candidates) != 1 || candidates[0].PartialPath != partialPath {
		t.Fatalf("recovery candidates = %+v, err=%v", candidates, err)
	}
}

func TestRecorderFactoryValidatesSegmentBeforeExternalRootAndMediaDatabaseSideEffects(t *testing.T) {
	repository, store, layout, _, _ := openRepository(t)
	defer store.Close()
	externalRoot := t.TempDir()
	starter := &recorderTestStarter{}
	dependencies := recorderTestDependencies(starter)
	factory, _, err := newFFmpegRecorderFactoryWithTools(FFmpegRecorderFactoryOptions{
		DataRoot: layout.Root, RecordingRoot: externalRoot,
		Repository: repository, MaxConcurrentRecordings: 1,
	}, ffmpegTools{
		ffmpegPath:  filepath.Join(t.TempDir(), "ffmpeg.exe"),
		ffprobePath: filepath.Join(t.TempDir(), "ffprobe.exe"),
	}, dependencies)
	if err != nil {
		t.Fatalf("new recorder factory: %v", err)
	}
	session := recorderTestSession(t, "invalid-segment-external-root")
	source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{
		recorderTestCandidate("one", "flv", "hd", "h264", "https://segment.example.invalid/live.flv", 1),
	}}}

	recorder, openErr := factory(context.Background(), session, OpenRequest{
		Profile: RecordingProfile{SegmentMinutes: 4},
	}, source)
	if recorder != nil || !errors.Is(openErr, ErrRecorderConfiguration) {
		t.Fatalf("invalid segment result = recorder:%v error:%v", recorder, openErr)
	}
	if got := len(starter.configSnapshot()); got != 0 {
		t.Fatalf("process starts before segment validation = %d, want 0", got)
	}
	var rootCount, mediaCount int
	if err := store.Reader().QueryRowContext(
		context.Background(), "SELECT COUNT(*) FROM recording_roots",
	).Scan(&rootCount); err != nil {
		t.Fatalf("count recording roots: %v", err)
	}
	if err := store.Reader().QueryRowContext(
		context.Background(), "SELECT COUNT(*) FROM session_media WHERE session_id = ?", session.ID,
	).Scan(&mediaCount); err != nil {
		t.Fatalf("count session media: %v", err)
	}
	if rootCount != 0 || mediaCount != 0 {
		t.Fatalf("invalid segment database side effects = roots:%d media:%d, want zero", rootCount, mediaCount)
	}
	entries, err := os.ReadDir(externalRoot)
	if err != nil {
		t.Fatalf("read external recording root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("invalid segment external-root side effects = %d entries, want 0", len(entries))
	}
}

func TestSQLiteAttemptJournalKeepsCommittedCASWhenManifestProjectionFails(t *testing.T) {
	ctx := context.Background()
	repository, store, layout, roomID, now := openRepository(t)
	defer store.Close()
	session, err := repository.Create(ctx, CreateSessionInput{
		RoomConfigID: roomID, OperationID: newV7(t), Recording: RecordingPending,
		StartedAt: now,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	toolDirectory := t.TempDir()
	tools := ffmpegTools{
		ffmpegPath: filepath.Join(toolDirectory, "ffmpeg.exe"), ffprobePath: filepath.Join(toolDirectory, "ffprobe.exe"),
	}
	for _, toolPath := range []string{tools.ffmpegPath, tools.ffprobePath} {
		if err := os.WriteFile(toolPath, []byte("verified tool"), 0o600); err != nil {
			t.Fatalf("write fake tool: %v", err)
		}
	}
	externalParent := t.TempDir()
	externalRoot := filepath.Join(externalParent, "recordings")
	if err := os.Mkdir(externalRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	_, finalizer, err := prepareRecorderSessionMedia(ctx, recorderSessionMediaOptions{
		Repository: repository, Tools: tools, DataRoot: layout.Root,
		RecordingRoot: externalRoot, Session: session, ProxyCapacity: make(chan struct{}, 1),
	})
	if err != nil {
		t.Fatalf("prepare external session media: %v", err)
	}
	journal, ok := finalizer.(SessionMediaAttemptJournal)
	if !ok {
		t.Fatalf("finalizer type %T does not implement attempt journal", finalizer)
	}
	movedRoot := filepath.Join(externalParent, "recordings-unavailable")
	if err := os.Rename(externalRoot, movedRoot); err != nil {
		t.Fatalf("make manifest projection unavailable: %v", err)
	}
	attempt := MediaAttempt{
		ID: newV7(t), Ordinal: 1, StartedAt: now.UnixMilli(),
		SegmentSeconds: defaultRecorderSegmentSeconds,
		VariantID:      "projection", Protocol: "flv", QualityKey: "hd",
		Quality: "hd", Codec: "h264", Bitrate: 1,
	}
	if err := journal.AppendMediaAttempt(ctx, attempt); err != nil {
		t.Fatalf("append durable attempt after projection failure: %v", err)
	}
	attempt.Committed = true
	if err := journal.UpdateMediaAttempt(ctx, attempt); err != nil {
		t.Fatalf("commit durable attempt after projection failure: %v", err)
	}
	attempt.Clean = true
	if err := journal.UpdateMediaAttempt(ctx, attempt); err != nil {
		t.Fatalf("clean durable attempt after projection failure: %v", err)
	}
	if err := journal.UpdateMediaAttempt(ctx, attempt); err != nil {
		t.Fatalf("idempotent durable attempt update after projection failure: %v", err)
	}
	snapshot, err := repository.LoadSnapshot(ctx, session.ID)
	if err != nil {
		t.Fatalf("load durable attempt journal: %v", err)
	}
	if len(snapshot.Session.Attempts) != 1 || snapshot.Session.Attempts[0] != attempt ||
		!snapshot.Session.ManifestDirty || snapshot.Session.ManifestRevision != 3 {
		t.Fatalf("durable journal after projection failure = %v", snapshot.Session)
	}
}

func TestPersistMediaSnapshotPostCommitReadUsesIndependentContext(t *testing.T) {
	repository, store, _, roomID, now := openRepository(t)
	defer store.Close()
	session, err := repository.Create(context.Background(), CreateSessionInput{
		RoomConfigID: roomID, OperationID: newV7(t), Recording: RecordingPending,
		StartedAt: now,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	opened, err := repository.OpenSessionMedia(context.Background(), OpenSessionMediaInput{
		SessionID: session.ID, RelativePath: session.DataPath, StartedAt: now.UnixMilli(),
	})
	if err != nil {
		t.Fatalf("open session media: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	repository.loadMediaAfterCommit = func(
		loadCtx context.Context,
		sessionID string,
	) (MediaSnapshot, error) {
		cancel()
		if err := loadCtx.Err(); err != nil {
			return MediaSnapshot{}, errors.New("postcommit load inherited caller cancellation")
		}
		return repository.LoadSnapshot(loadCtx, sessionID)
	}
	attempt := MediaAttempt{
		ID: newV7(t), Ordinal: 1, StartedAt: now.UnixMilli(),
		SegmentSeconds: defaultRecorderSegmentSeconds,
		VariantID:      "independent", Protocol: "flv", QualityKey: "hd",
		Quality: "hd", Codec: "h264", Bitrate: 1,
	}
	persisted, err := repository.PersistMediaSnapshot(ctx, PersistMediaSnapshotInput{
		SessionID: session.ID, ExpectedRevision: opened.Session.ManifestRevision,
		State: SessionMediaOpen, Attempts: []MediaAttempt{attempt},
	})
	if err != nil {
		t.Fatalf("persist after caller cancellation at postcommit load: %v", err)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("caller context = %v, want canceled by postcommit hook", ctx.Err())
	}
	if persisted.Session.ManifestRevision != 1 ||
		len(persisted.Session.Attempts) != 1 || persisted.Session.Attempts[0] != attempt {
		t.Fatalf("committed snapshot = %+v", persisted.Session)
	}
}

func TestRecorderAttemptJournalPostCommitReadFailureCannotReverseDurableState(t *testing.T) {
	tests := []struct {
		name      string
		faultCall int
		fault     error
	}{
		{name: "append load failure", faultCall: 1, fault: ErrMediaPersistence},
		{name: "append load canceled", faultCall: 1, fault: context.Canceled},
		{name: "committed load failure", faultCall: 2, fault: ErrMediaPersistence},
		{name: "committed load canceled", faultCall: 2, fault: context.Canceled},
		{name: "clean load failure", faultCall: 3, fault: ErrMediaPersistence},
		{name: "clean load canceled", faultCall: 3, fault: context.Canceled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository, store, layout, roomID, now := openRepository(t)
			defer store.Close()
			session, err := repository.Create(context.Background(), CreateSessionInput{
				RoomConfigID: roomID, OperationID: newV7(t), Recording: RecordingPending,
				StartedAt: now,
			})
			if err != nil {
				t.Fatalf("create session: %v", err)
			}
			tools := ffmpegTools{
				ffmpegPath:  filepath.Join(t.TempDir(), "ffmpeg.exe"),
				ffprobePath: filepath.Join(t.TempDir(), "ffprobe.exe"),
			}
			for _, toolPath := range []string{tools.ffmpegPath, tools.ffprobePath} {
				if err := os.WriteFile(toolPath, []byte("verified tool"), 0o600); err != nil {
					t.Fatalf("write fake tool: %v", err)
				}
			}
			mediaDirectory, mediaFinalizer, err := prepareRecorderSessionMedia(
				context.Background(),
				recorderSessionMediaOptions{
					Repository: repository, Tools: tools, DataRoot: layout.Root,
					RecordingRoot: layout.RoomsDir, Session: session,
					ProxyCapacity: make(chan struct{}, 1),
				},
			)
			if err != nil {
				t.Fatalf("prepare session media: %v", err)
			}
			journal, ok := mediaFinalizer.(SessionMediaAttemptJournal)
			if !ok {
				t.Fatalf("finalizer type %T does not implement journal", mediaFinalizer)
			}

			var observations []MediaSnapshot
			postCommitCalls := 0
			repository.loadMediaAfterCommit = func(
				loadCtx context.Context,
				sessionID string,
			) (MediaSnapshot, error) {
				postCommitCalls++
				observed, loadErr := repository.LoadSnapshot(loadCtx, sessionID)
				if loadErr != nil {
					return MediaSnapshot{}, loadErr
				}
				observations = append(observations, observed)
				if postCommitCalls == test.faultCall {
					return MediaSnapshot{}, test.fault
				}
				return observed, nil
			}

			process := newRecorderTestProcess()
			process.quitExits = true
			starter := &recorderTestStarter{results: []recorderTestStartResult{{process: process}}}
			dependencies := recorderTestDependencies(starter)
			dependencies.maxResolveSnapshots = 1
			options := recorderTestOptions(t)
			options.mediaDirectory = mediaDirectory
			journalFinalizer := &recorderMediaJournalFinalizer{journal: journal}
			options.mediaFinalizer = journalFinalizer
			options.attemptJournal = journalFinalizer
			source := &recorderTestSource{snapshots: [][]douyinLive.ResolvedStream{{
				recorderTestCandidate("one", "flv", "hd", "h264", "https://postcommit.example.invalid/live.flv", 1),
			}}}
			recorder, err := newFFmpegRecorder(
				context.Background(), source, options, dependencies, nil,
			)
			if err != nil {
				t.Fatalf("new recorder after committed postcommit-read fault: %v", err)
			}
			if actions := process.actionSnapshot(); len(actions) != 0 {
				t.Fatalf("healthy process killed after committed journal CAS: %v", actions)
			}
			if err := recorder.Stop(context.Background()); err != nil {
				t.Fatalf("Stop after committed postcommit-read fault: %v", err)
			}
			if actions := process.actionSnapshot(); len(actions) != 2 || actions[0] != "q" || actions[1] != "close" {
				t.Fatalf("process stop actions = %v, want graceful q then handle close", actions)
			}
			if postCommitCalls != 3 || len(observations) != 3 {
				t.Fatalf("postcommit observations = calls:%d snapshots:%d, want 3", postCommitCalls, len(observations))
			}
			for index, observed := range observations {
				attempts := observed.Session.Attempts
				wantCommitted := index >= 1
				wantClean := index >= 2
				if observed.Session.ManifestRevision != int64(index+1) || len(attempts) != 1 ||
					attempts[0].Committed != wantCommitted || attempts[0].Clean != wantClean {
					t.Fatalf("journal transition %d = session:%+v attempts:%+v", index, observed.Session, attempts)
				}
			}
			persisted, err := repository.LoadSnapshot(context.Background(), session.ID)
			if err != nil {
				t.Fatalf("load final durable journal: %v", err)
			}
			if persisted.Session.ManifestRevision != 3 || len(persisted.Session.Attempts) != 1 ||
				!persisted.Session.Attempts[0].Committed || !persisted.Session.Attempts[0].Clean {
				t.Fatalf("final durable journal = %+v", persisted.Session)
			}
		})
	}
}

func TestPrepareRecorderSessionMediaWiresInternalGlobalAndPerRoomRoots(t *testing.T) {
	repository, store, layout, roomID, now := openRepository(t)
	defer store.Close()
	toolDirectory := t.TempDir()
	tools := ffmpegTools{
		ffmpegPath:  filepath.Join(toolDirectory, "ffmpeg.exe"),
		ffprobePath: filepath.Join(toolDirectory, "ffprobe.exe"),
	}
	for _, path := range []string{tools.ffmpegPath, tools.ffprobePath} {
		if err := os.WriteFile(path, []byte("verified tool"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	externalGlobal := filepath.Join(t.TempDir(), "global-recordings")
	externalRoom := filepath.Join(t.TempDir(), "room-recordings")
	for _, path := range []string{externalGlobal, externalRoom} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name          string
		recordingRoot string
		saveDirectory string
		wantRoot      string
		wantExternal  bool
	}{
		{name: "internal", recordingRoot: layout.RoomsDir, wantRoot: layout.Root},
		{name: "external global", recordingRoot: externalGlobal, wantRoot: externalGlobal, wantExternal: true},
		{name: "per-room override", recordingRoot: layout.RoomsDir, saveDirectory: externalRoom, wantRoot: externalRoom, wantExternal: true},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operationID := newV7(t)
			session, err := repository.Create(context.Background(), CreateSessionInput{
				RoomConfigID: roomID, OperationID: operationID, Recording: RecordingPending,
				StartedAt: now.Add(time.Duration(index) * time.Second),
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				endedAt := now.Add(time.Hour + time.Duration(index)*time.Second)
				if _, err := repository.Transition(context.Background(), TransitionSessionInput{
					ID: session.ID, ExpectedStatus: SessionStarting,
					ExpectedRecordingStatus: RecordingPending, ExpectedOperationID: operationID,
					Status: SessionInterrupted, RecordingStatus: RecordingIncomplete,
					EndedAt: &endedAt,
				}); err != nil {
					t.Errorf("close fixture session: %v", err)
				}
			})
			mediaDirectory, finalizer, err := prepareRecorderSessionMedia(
				context.Background(), recorderSessionMediaOptions{
					Repository: repository, Tools: tools, DataRoot: layout.Root,
					RecordingRoot: test.recordingRoot, SaveDirectory: test.saveDirectory,
					Session: session, ProxyCapacity: make(chan struct{}, 1),
				},
			)
			if err != nil || finalizer == nil {
				t.Fatalf("prepare recorder session media = (%q, %v, %v)", mediaDirectory, finalizer, err)
			}
			wantRelative := session.DataPath
			if test.wantExternal {
				var ok bool
				wantRelative, ok = strings.CutPrefix(wantRelative, "rooms/")
				if !ok {
					t.Fatalf("session data path %q lacks rooms prefix", session.DataPath)
				}
			}
			wantDirectory := filepath.Join(test.wantRoot, filepath.FromSlash(wantRelative), "media")
			if !sameRecorderDirectory(mediaDirectory, wantDirectory) {
				t.Fatalf("media directory = %q, want %q", mediaDirectory, wantDirectory)
			}
			snapshot, err := repository.LoadSnapshot(context.Background(), session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Session.RelativePath != wantRelative {
				t.Fatalf("relative path = %q, want %q", snapshot.Session.RelativePath, wantRelative)
			}
			if test.wantExternal {
				if snapshot.Session.RootID == nil {
					t.Fatal("external root ID was not persisted")
				}
				marker, err := readRecordingRootMarker(filepath.Join(test.wantRoot, recordingRootMarkerName))
				if err != nil || marker.RootID != *snapshot.Session.RootID {
					t.Fatalf("external marker does not match snapshot root: %+v, %v", marker, err)
				}
			} else if snapshot.Session.RootID != nil {
				t.Fatalf("internal session unexpectedly has root ID %q", *snapshot.Session.RootID)
			}
		})
	}
}
