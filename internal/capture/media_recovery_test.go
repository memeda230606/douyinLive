package capture

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestScanRecoveryMediaSourcesAuditsOrphansAndUsesLatestSourceWrite(t *testing.T) {
	sessionDirectory := filepath.Join(t.TempDir(), "session")
	mediaDirectory := filepath.Join(sessionDirectory, "media")
	if err := os.MkdirAll(mediaDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	attempt := MediaAttempt{
		ID: newV7(t), Ordinal: 1, StartedAt: startedAt.UnixMilli(),
		SegmentSeconds: 600, Committed: true, Protocol: "flv", Codec: "h264",
	}
	knownDirectory := filepath.Join(mediaDirectory, ".attempt-"+attempt.ID)
	if err := os.Mkdir(knownDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	knownPath := filepath.Join(knownDirectory, mediaAttemptSegmentName(1, attempt))
	writeRecoveryTestFileAt(t, knownPath, startedAt.Add(time.Second))

	orphanAttempt := attempt
	orphanAttempt.ID = newV7(t)
	orphanAttempt.Ordinal = 2
	orphanAttempt.StartedAt = startedAt.Add(time.Minute).UnixMilli()
	orphanDirectory := filepath.Join(mediaDirectory, ".attempt-"+orphanAttempt.ID)
	if err := os.Mkdir(orphanDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	orphanPartial := filepath.Join(
		orphanDirectory, mediaAttemptSegmentName(1, orphanAttempt),
	)
	writeRecoveryTestFileAt(t, orphanPartial, startedAt.Add(2*time.Second))

	orphanFinal := filepath.Join(
		mediaDirectory, mediaFinalSegmentName(7, startedAt.Add(3*time.Second).UnixMilli()),
	)
	writeRecoveryTestFileAt(t, orphanFinal, startedAt.Add(3*time.Second))

	scan, err := scanRecoveryMediaSources(
		context.Background(), sessionDirectory, []MediaAttempt{attempt},
	)
	if err != nil {
		t.Fatal(err)
	}
	if scan.lastWriteAt.UnixMilli() != startedAt.Add(3*time.Second).UnixMilli() {
		t.Fatalf("last write = %v", scan.lastWriteAt)
	}
	if scan.orphanFiles != 2 ||
		!containsMediaWarning(scan.warningCodes, MediaRecoveryOrphanAttemptWarning) ||
		!containsMediaWarning(scan.warningCodes, MediaRecoveryOrphanSegmentWarning) {
		t.Fatalf("unexpected orphan audit: %+v", scan)
	}
	for _, path := range []string{knownPath, orphanPartial, orphanFinal} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("recovery scan changed %s: %v", filepath.Base(path), err)
		}
	}
}

func TestMediaSegmentProcessorRecoveryMarksReadablePartialRecovered(t *testing.T) {
	fixture := newMediaFinalizerFixture(t, writeSegmentProbeJSON(validSegmentProbeJSON))
	t.Cleanup(fixture.close)
	candidates, err := discoverMediaCandidates(
		fixture.sessionDirectory, fixture.session.DataPath, []MediaAttempt{fixture.attempt},
	)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("discover candidates = %d, %v", len(candidates), err)
	}
	segment, warnings, err := (mediaSegmentProcessor{
		prober:     fixture.prober,
		newID:      func() (string, error) { return newV7(t), nil },
		recovering: true,
	}).finalize(context.Background(), candidates[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	if segment.Status != MediaSegmentRecovered || len(warnings) != 0 {
		t.Fatalf("recovered partial = %#v warnings=%v", segment, warnings)
	}
	if _, err := os.Stat(fixture.partialPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial was not atomically promoted: %v", err)
	}
}

func TestSQLiteSessionMediaRecovererClampsFutureSourceAndEnablesRecoveryMode(t *testing.T) {
	fixture := newMediaFinalizerFixture(t, writeSegmentProbeJSON(validSegmentProbeJSON))
	finalizer := fixture.open(t)
	initialAttempt := fixture.attempt
	initialAttempt.Committed = false
	initialAttempt.Clean = false
	if err := finalizer.AppendMediaAttempt(context.Background(), initialAttempt); err != nil {
		t.Fatal(err)
	}
	recoveryAttempt := fixture.attempt
	recoveryAttempt.Clean = false
	if err := finalizer.UpdateMediaAttempt(context.Background(), recoveryAttempt); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.repository.LoadSnapshot(context.Background(), fixture.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	scanCutoff := time.UnixMilli(fixture.session.StartedAt).UTC().Add(10 * time.Second)
	writeTime := scanCutoff.Add(time.Minute)
	if err := os.Chtimes(fixture.partialPath, writeTime, writeTime); err != nil {
		t.Fatal(err)
	}
	jobNamespace, valid := recorderJobNamespace(fixture.root)
	if !valid {
		t.Fatal("derive media recovery Job namespace")
	}
	var inspectedNamespace string
	var observedOptions sessionMediaFinalizerOptions
	var inspectedAttemptID string
	recoverer := &sqliteSessionMediaRecoverer{
		repository:    fixture.repository,
		tools:         fixture.tools,
		dataRoot:      fixture.root,
		jobNamespace:  jobNamespace,
		proxyCapacity: make(chan struct{}, 1),
		newFinalizer: func(
			_ context.Context,
			options sessionMediaFinalizerOptions,
		) (SessionMediaFinalizer, error) {
			observedOptions = options
			return recoveryFinalizerStub{result: MediaFinalizeResult{Snapshot: snapshot}}, nil
		},
		inspectProcess: func(
			_ context.Context,
			processNamespace string,
			attemptID string,
		) (RecorderProcessRecoveryResult, error) {
			inspectedNamespace = processNamespace
			inspectedAttemptID = attemptID
			return RecorderProcessRecoveryResult{Found: true, Terminated: true, Status: RecorderProcessRecoveryTerminated}, nil
		},
	}
	result, err := recoverer.RecoverSessionMedia(
		context.Background(), fixture.session, scanCutoff,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.CutoffAt.Equal(scanCutoff) ||
		!containsMediaWarning(result.WarningCodes, MediaRecoveryClockWarning) {
		t.Fatalf("future file cutoff was not bounded: %+v", result)
	}
	if inspectedNamespace != jobNamespace || inspectedAttemptID != recoveryAttempt.ID ||
		result.ProcessesFound != 1 ||
		result.ProcessesTerminated != 1 ||
		!containsMediaWarning(result.WarningCodes, MediaRecoveryProcessTerminatedWarning) {
		t.Fatalf("durable attempt process was not recovered first: %+v id=%q", result, inspectedAttemptID)
	}
	if !observedOptions.Recovering || observedOptions.SessionID != fixture.session.ID ||
		observedOptions.RootID != nil || observedOptions.RelativePath != fixture.session.DataPath {
		t.Fatalf("unexpected finalizer options: %+v", observedOptions)
	}
}

func TestSQLiteSessionMediaRecovererRejectsMissingSnapshotWithoutCreatingIt(t *testing.T) {
	fixture := newMediaFinalizerFixture(t, writeSegmentProbeJSON(validSegmentProbeJSON))
	t.Cleanup(fixture.close)
	jobNamespace, valid := recorderJobNamespace(fixture.root)
	if !valid {
		t.Fatal("derive media recovery Job namespace")
	}
	recoverer := &sqliteSessionMediaRecoverer{
		repository:    fixture.repository,
		tools:         fixture.tools,
		dataRoot:      fixture.root,
		jobNamespace:  jobNamespace,
		proxyCapacity: make(chan struct{}, 1),
		newFinalizer: func(context.Context, sessionMediaFinalizerOptions) (SessionMediaFinalizer, error) {
			t.Fatal("missing snapshot must not construct a finalizer")
			return nil, nil
		},
		inspectProcess: func(context.Context, string, string) (RecorderProcessRecoveryResult, error) {
			t.Fatal("missing snapshot must not inspect a process")
			return RecorderProcessRecoveryResult{}, nil
		},
	}
	_, err := recoverer.RecoverSessionMedia(
		context.Background(), fixture.session,
		time.UnixMilli(fixture.session.StartedAt).Add(time.Minute),
	)
	if !errors.Is(err, ErrSessionMediaNotFound) {
		t.Fatalf("RecoverSessionMedia() error = %v, want %v", err, ErrSessionMediaNotFound)
	}
}

func TestSQLiteSessionMediaRecovererFailsClosedOnMalformedProcessInspection(t *testing.T) {
	fixture := newMediaFinalizerFixture(t, writeSegmentProbeJSON(validSegmentProbeJSON))
	finalizer := fixture.open(t)
	attempt := fixture.attempt
	attempt.Committed = false
	attempt.Clean = false
	if err := finalizer.AppendMediaAttempt(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	jobNamespace, valid := recorderJobNamespace(fixture.root)
	if !valid {
		t.Fatal("derive media recovery Job namespace")
	}
	finalizerCalls := 0
	recoverer := &sqliteSessionMediaRecoverer{
		repository:    fixture.repository,
		tools:         fixture.tools,
		dataRoot:      fixture.root,
		jobNamespace:  jobNamespace,
		proxyCapacity: make(chan struct{}, 1),
		newFinalizer: func(
			context.Context,
			sessionMediaFinalizerOptions,
		) (SessionMediaFinalizer, error) {
			finalizerCalls++
			return recoveryFinalizerStub{}, nil
		},
		inspectProcess: func(
			_ context.Context,
			processNamespace string,
			attemptID string,
		) (RecorderProcessRecoveryResult, error) {
			if processNamespace != jobNamespace || attemptID != attempt.ID {
				t.Fatalf("process identity = namespace:%q attempt:%q", processNamespace, attemptID)
			}
			return RecorderProcessRecoveryResult{
				Terminated: true,
				Status:     RecorderProcessRecoveryClean,
			}, nil
		},
	}
	result, err := recoverer.RecoverSessionMedia(
		context.Background(), fixture.session,
		time.UnixMilli(fixture.session.StartedAt).Add(time.Minute),
	)
	if !errors.Is(err, ErrMediaRecovery) {
		t.Fatalf("RecoverSessionMedia error = %v", err)
	}
	if !containsMediaWarning(result.WarningCodes, MediaRecoveryProcessFailedWarning) ||
		result.ProcessesTerminated != 1 {
		t.Fatalf("malformed inspection result = %+v", result)
	}
	if finalizerCalls != 0 {
		t.Fatalf("finalizer calls = %d, want fail-closed 0", finalizerCalls)
	}
}

type recoveryFinalizerStub struct {
	result MediaFinalizeResult
	err    error
}

func (stub recoveryFinalizerStub) Finalize(
	context.Context,
	[]MediaAttempt,
) (MediaFinalizeResult, error) {
	return stub.result, stub.err
}

func writeRecoveryTestFileAt(t *testing.T, path string, modified time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte("recovery-media"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}
