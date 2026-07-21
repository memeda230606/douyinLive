//go:build p3accacceptance

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jwwsjlm/douyinLive/v2/internal/capture"
	"github.com/jwwsjlm/douyinLive/v2/internal/eventstore"
	"github.com/jwwsjlm/douyinLive/v2/internal/room"
	"github.com/jwwsjlm/douyinLive/v2/internal/settings"
)

const (
	p3ACCTestRoomID      = "018f47a0-7c00-7000-8000-000000000101"
	p3ACCTestSessionID   = "018f47a0-7c00-7000-8000-000000000102"
	p3ACCTestOperationID = "018f47a0-7c00-7000-8000-000000000103"
	p3ACCTestAttemptID   = "018f47a0-7c00-7000-8000-000000000104"
)

func TestLoadP3ACCAcceptancePathsPinsDataRootAndResult(t *testing.T) {
	clearP3ACCBootstrap()
	t.Cleanup(clearP3ACCBootstrap)
	root := t.TempDir()
	result := filepath.Join(root, "evidence", p3ACCAcceptanceResultName)
	t.Setenv("P3ACC_ROOT", root)
	t.Setenv("P3ACC_RESULT_PATH", result)
	t.Setenv("P3ACC_LIVE_URL", "p3-acc-private-fixture")
	if err := os.WriteFile(
		filepath.Join(root, p3ACCAcceptanceSentinelName),
		[]byte(p3ACCAcceptanceSentinelContent), 0o600,
	); err != nil {
		t.Fatalf("write controller sentinel: %v", err)
	}

	paths, err := loadP3ACCAcceptancePaths()
	if err != nil {
		t.Fatalf("loadP3ACCAcceptancePaths() error = %v", err)
	}
	if paths.Root != filepath.Clean(root) || paths.DataRoot != filepath.Join(filepath.Clean(root), "data") ||
		paths.ResultPath != filepath.Clean(result) {
		t.Fatalf("unexpected paths: %#v", paths)
	}
	options, err := desktopInfrastructureOptions()
	if err != nil {
		t.Fatalf("desktopInfrastructureOptions() error = %v", err)
	}
	if options.DataRoot != paths.DataRoot || !options.DisableDiagnostics {
		t.Fatalf("DataRoot = %q, want pinned acceptance data root", options.DataRoot)
	}
	if _, exists := os.LookupEnv("P3ACC_LIVE_URL"); exists {
		t.Fatal("P3ACC_LIVE_URL remains in process environment after infrastructure options")
	}
	for _, name := range []string{"P3ACC_ROOT", "P3ACC_RESULT_PATH"} {
		if _, exists := os.LookupEnv(name); exists {
			t.Fatalf("%s remains in process environment after infrastructure options", name)
		}
	}

	if _, err := parseP3ACCAcceptancePaths(root, filepath.Join(filepath.Dir(root), p3ACCAcceptanceResultName)); err == nil {
		t.Fatal("outside result path accepted")
	}
	if _, err := parseP3ACCAcceptancePaths(root, filepath.Join(root, "data", p3ACCAcceptanceResultName)); err == nil {
		t.Fatal("result path inside mutable application data accepted")
	}
	if _, err := parseP3ACCAcceptancePaths(root, filepath.Join(root, "result.json")); err == nil {
		t.Fatal("unexpected result filename accepted")
	}
}

func TestP3ACCFreshLayoutRejectsPreexistingDataRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, p3ACCAcceptanceSentinelName),
		[]byte(p3ACCAcceptanceSentinelContent), 0o600); err != nil {
		t.Fatalf("write controller sentinel: %v", err)
	}
	paths, err := parseP3ACCAcceptancePaths(
		root, filepath.Join(root, "evidence", p3ACCAcceptanceResultName),
	)
	if err != nil {
		t.Fatalf("parse acceptance paths: %v", err)
	}
	if err := os.MkdirAll(paths.DataRoot, 0o700); err != nil {
		t.Fatalf("create preexisting data root: %v", err)
	}
	if err := validateP3ACCAcceptanceFreshLayout(paths); err == nil {
		t.Fatal("preexisting data root was accepted before infrastructure initialization")
	}
}

func TestP3ACCPrivacySettingsInputPreservesPolicyAndDisablesNames(t *testing.T) {
	dataRoot := t.TempDir()
	current := settings.AppSettings{
		DefaultQuality: room.QualityHigh, DefaultSegmentMinutes: 15,
		MaxConcurrentRecordings: 2, MinimumFreeSpaceGiB: 7, SaveDisplayNames: true,
	}
	input := p3ACCPrivacySettingsInput(current, dataRoot)
	if input.SaveDisplayNames {
		t.Fatal("SaveDisplayNames remains enabled")
	}
	if input.RecordingDirectory != filepath.Join(dataRoot, "rooms") ||
		input.DefaultQuality != current.DefaultQuality ||
		input.DefaultSegmentMinutes != current.DefaultSegmentMinutes ||
		input.MaxConcurrentRecordings != current.MaxConcurrentRecordings ||
		input.MinimumFreeSpaceGiB != current.MinimumFreeSpaceGiB {
		t.Fatalf("privacy input changed unrelated policy: %#v", input)
	}
}

func TestP3ACCAcceptanceObserverScopesAndBoundsRuntimeEvents(t *testing.T) {
	app := &DesktopApp{}
	state := &p3ACCAcceptanceState{
		roomID: p3ACCTestRoomID, observedEventIDs: make(map[string]struct{}),
	}
	p3ACCAcceptanceRegistry.Lock()
	p3ACCAcceptanceRegistry.states[app] = state
	p3ACCAcceptanceRegistry.Unlock()
	t.Cleanup(func() {
		p3ACCAcceptanceRegistry.Lock()
		delete(p3ACCAcceptanceRegistry.states, app)
		p3ACCAcceptanceRegistry.Unlock()
	})

	app.observeAcceptanceEvent(room.StatusEventName, room.RoomRuntimeStatus{
		RoomID: "018f47a0-7c00-7000-8000-000000000199", State: room.RuntimeError,
		SessionID: p3ACCTestSessionID, OperationID: p3ACCTestOperationID,
		ErrorCode: "SHOULD_NOT_APPEAR", Revision: 999,
	})
	app.observeAcceptanceEvent(room.StatusEventName, room.RoomRuntimeStatus{
		RoomID: p3ACCTestRoomID, State: room.RuntimeRecording,
		SessionID: p3ACCTestSessionID, OperationID: p3ACCTestOperationID,
		RecordingStatus: capture.RecordingActive, Revision: 10,
	})
	app.observeAcceptanceEvent(capture.RecordingProgressEventName, capture.RecordingProgressDTO{
		RoomID: p3ACCTestRoomID, SessionID: p3ACCTestSessionID, OperationID: p3ACCTestOperationID,
		State: capture.RecordingActive, ElapsedMS: 10_000, BytesWritten: 100,
		SegmentCount: 1, RestartCount: 0, UpdatedAt: 10,
	})
	app.observeAcceptanceEvent(capture.RecordingProgressEventName, capture.RecordingProgressDTO{
		RoomID: p3ACCTestRoomID, SessionID: p3ACCTestSessionID, OperationID: p3ACCTestOperationID,
		State: capture.RecordingActive, ElapsedMS: 99_000, BytesWritten: 999,
		SegmentCount: 9, RestartCount: 9, UpdatedAt: 9,
	})
	const retryAt int64 = 1_900_000_000_000
	app.observeAcceptanceEvent(room.StatusEventName, room.RoomRuntimeStatus{
		RoomID: p3ACCTestRoomID, State: room.RuntimeFinalizing,
		SessionID: p3ACCTestSessionID, OperationID: p3ACCTestOperationID,
		RecordingStatus: capture.RecordingFinalizing, ErrorCode: "CAPTURE_FINALIZE_FAILED", RetryAt: retryAt, Revision: 11,
	})
	app.observeAcceptanceEvent(eventstore.LiveEventEventName, eventstore.LiveEventBatchDTO{
		SessionID: p3ACCTestSessionID,
		Events:    []eventstore.LiveEventDTO{{ID: p3ACCTestAttemptID}, {ID: p3ACCTestOperationID}},
	})

	state.mu.Lock()
	status := state.status
	progress := state.progress
	state.mu.Unlock()
	if status.State != room.RuntimeFinalizing || status.Revision != 11 ||
		status.ErrorCode != "CAPTURE_FINALIZE_FAILED" || status.RetryAt != retryAt {
		t.Fatalf("unexpected safe status: %#v", status)
	}
	if progress.SampleCount != 1 || progress.ElapsedMS != 10_000 || progress.BytesWritten != 100 ||
		progress.LiveBatchCount != 1 || progress.LiveEventCount != 2 {
		t.Fatalf("unexpected progress summary: %#v", progress)
	}
}

func TestP3ACCAcceptanceObserverRetainsOfflineProofAcrossStartingPoll(t *testing.T) {
	app := &DesktopApp{}
	state := &p3ACCAcceptanceState{
		roomID: p3ACCTestRoomID, observedEventIDs: make(map[string]struct{}),
	}
	p3ACCAcceptanceRegistry.Lock()
	p3ACCAcceptanceRegistry.states[app] = state
	p3ACCAcceptanceRegistry.Unlock()
	t.Cleanup(func() {
		p3ACCAcceptanceRegistry.Lock()
		delete(p3ACCAcceptanceRegistry.states, app)
		p3ACCAcceptanceRegistry.Unlock()
	})

	statuses := []room.RoomRuntimeStatus{
		{
			RoomID: p3ACCTestRoomID, State: room.RuntimeRecording,
			SessionID: p3ACCTestSessionID, OperationID: p3ACCTestOperationID,
			RecordingStatus: capture.RecordingActive, Revision: 10,
		},
		{
			RoomID: p3ACCTestRoomID, State: room.RuntimeReconnecting,
			SessionID: p3ACCTestSessionID, OperationID: p3ACCTestOperationID,
			RecordingStatus: capture.RecordingReconnecting,
			ErrorCode:       "ROOM_OFFLINE_CONFIRMING", Revision: 11,
		},
		{
			RoomID: p3ACCTestRoomID, State: room.RuntimeWaiting,
			OperationID: p3ACCTestOperationID, ErrorCode: "ROOM_OFFLINE", Revision: 12,
		},
		{
			RoomID: p3ACCTestRoomID, State: room.RuntimeStarting,
			OperationID: p3ACCTestOperationID, Revision: 13,
		},
	}
	for _, status := range statuses {
		app.observeAcceptanceEvent(room.StatusEventName, status)
	}
	state.mu.Lock()
	confirmed, finalized, proven := state.offlineConfirmRevision, state.offlineFinalRevision, state.offlineSequenceProven
	state.mu.Unlock()
	if confirmed != 11 || finalized != 12 || !proven {
		t.Fatalf("starting poll cleared offline proof: confirm=%d final=%d proven=%t", confirmed, finalized, proven)
	}

	app.observeAcceptanceEvent(room.StatusEventName, room.RoomRuntimeStatus{
		RoomID: p3ACCTestRoomID, State: room.RuntimeLive,
		OperationID: p3ACCTestOperationID, Revision: 14,
	})
	state.mu.Lock()
	confirmed, finalized, proven = state.offlineConfirmRevision, state.offlineFinalRevision, state.offlineSequenceProven
	state.mu.Unlock()
	if confirmed != 0 || finalized != 0 || proven {
		t.Fatalf("live state retained stale offline proof: confirm=%d final=%d proven=%t", confirmed, finalized, proven)
	}
}

func TestP3ACCAcceptanceSnapshotIsStrictAndPrivacySafe(t *testing.T) {
	snapshot := validP3ACCTestSnapshot()
	if err := validateP3ACCAcceptanceSnapshot(snapshot); err != nil {
		t.Fatalf("valid snapshot rejected: %v", err)
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	text := string(payload)
	for _, forbidden := range []string{
		`"roomId"`, `"sessionId"`, `"operationId"`, `"attemptId"`, `"recorderPid"`,
		`"processId"`, `"liveId"`, `"liveName"`, `"anchor"`, `"displayName"`,
		`"content"`, `"cookie"`, `"url"`, `"path"`, `"command"`, `"ffmpegArgs"`,
		`"token"`, `"rawFile"`, `"spoolFile"`, "http://", "https://", `C:\\`,
		p3ACCTestRoomID, p3ACCTestSessionID, p3ACCTestOperationID, p3ACCTestAttemptID,
	} {
		if strings.Contains(strings.ToLower(text), strings.ToLower(forbidden)) {
			t.Fatalf("snapshot contains forbidden marker %q: %s", forbidden, text)
		}
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(payload, &top); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	wantKeys := []string{
		"schema", "stage", "capturedAt", "ui", "runtime", "progress", "database",
		"sessionManifest", "mediaManifest", "gaps", "checkpoint", "resources",
	}
	if len(top) != len(wantKeys) {
		t.Fatalf("top-level key count = %d, want %d", len(top), len(wantKeys))
	}
	for _, key := range wantKeys {
		if _, ok := top[key]; !ok {
			t.Fatalf("missing top-level key %q", key)
		}
	}

	invalid := snapshot
	invalid.Runtime.ErrorCode = "SHOULD_NOT_APPEAR"
	if err := validateP3ACCAcceptanceSnapshot(invalid); err == nil {
		t.Fatal("non-whitelisted error code accepted")
	}
	invalid = snapshot
	invalid.Resources.SampleCount = p3ACCAcceptanceMaximumResourceSamples + 1
	if err := validateP3ACCAcceptanceSnapshot(invalid); err == nil {
		t.Fatal("unbounded resource aggregate accepted")
	}
}

func TestP3ACCMediaManifestSummaryDoesNotReturnPathsOrHashes(t *testing.T) {
	payload := `{
	  "schemaVersion": 1,
	  "session": {
	    "id": "018f47a0-7c00-7000-8000-000000000102",
	    "relativePath": "rooms/private/sessions/2026/07/private",
	    "state": "open",
	    "revision": 2,
	    "createdAt": 1,
	    "updatedAt": 2
	  },
	  "attempts": [{
	    "id": "018f47a0-7c00-7000-8000-000000000104",
	    "ordinal": 1,
	    "startedAt": 1,
	    "segmentSeconds": 300,
	    "committed": true,
	    "clean": false,
	    "protocol": "flv",
	    "codec": "h264"
	  }],
	  "segments": [{
	    "id": "018f47a0-7c00-7000-8000-000000000105",
	    "sequence": 1,
	    "relativePath": "media/private.mkv",
	    "container": "matroska",
	    "startedAt": 1,
	    "endedAt": 2,
	    "durationMs": 1,
	    "sizeBytes": 1,
	    "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	    "status": "complete",
	    "attemptId": "018f47a0-7c00-7000-8000-000000000104",
	    "attemptSequence": 1
	  }],
	  "artifacts": []
	}`
	summary, err := summarizeP3ACCMediaManifest([]byte(payload), p3ACCTestSessionID)
	if err != nil {
		t.Fatalf("summarizeP3ACCMediaManifest() error = %v", err)
	}
	if !summary.Exists || summary.State != capture.SessionMediaOpen || summary.Revision != 2 ||
		summary.AttemptCount != 1 || summary.CommittedAttemptCount != 1 ||
		summary.SegmentCount != 1 || summary.CompleteSegmentCount != 1 {
		t.Fatalf("unexpected media summary: %#v", summary)
	}
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("marshal media summary: %v", err)
	}
	for _, forbidden := range []string{"private", "relativePath", "sha256", "media/"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("media summary leaks %q: %s", forbidden, encoded)
		}
	}
}

func TestP3ACCMediaManifestFileVerifiesInternalProductionPathsFromDataRoot(t *testing.T) {
	dataRoot := t.TempDir()
	dataPath := "rooms/test-room/sessions/2026/07/018f47a0-7c00-7000-8000-000000000102"
	sessionDirectory := filepath.Join(dataRoot, filepath.FromSlash(dataPath))
	segmentRelativePath := dataPath + "/media/segment-000001.mkv"
	segmentFilename := filepath.Join(dataRoot, filepath.FromSlash(segmentRelativePath))
	if err := os.MkdirAll(filepath.Dir(segmentFilename), 0o700); err != nil {
		t.Fatalf("create production media directory: %v", err)
	}
	segmentPayload := []byte("production-media-evidence")
	if err := os.WriteFile(segmentFilename, segmentPayload, 0o600); err != nil {
		t.Fatalf("write production media fixture: %v", err)
	}
	segmentDigest := fmt.Sprintf("%x", sha256.Sum256(segmentPayload))
	attempt := capture.MediaAttempt{
		ID: p3ACCTestAttemptID, Ordinal: 1, StartedAt: 1,
		SegmentSeconds: 300, Committed: true, Clean: true,
		Protocol: "flv", Codec: "h264",
	}
	segment := capture.MediaSegment{
		ID: p3ACCTestSessionID, Sequence: 1, RelativePath: segmentRelativePath,
		SizeBytes: int64(len(segmentPayload)), SHA256: segmentDigest,
		Status: capture.MediaSegmentRecovered, AttemptID: attempt.ID, AttemptSequence: 1,
	}
	database := p3ACCInternalDatabaseSnapshot{
		Found: true, SessionID: p3ACCTestSessionID, DataPath: dataPath,
		Media: capture.SessionMedia{
			SessionID: p3ACCTestSessionID, RelativePath: dataPath,
			State: capture.SessionMediaCompleted, ManifestRevision: 7,
			CreatedAt: 1, UpdatedAt: 2,
		},
		Attempts: []capture.MediaAttempt{attempt},
		Segments: []capture.MediaSegment{segment},
	}
	manifest := p3ACCMediaManifestWire{SchemaVersion: 1}
	manifest.Session.ID = database.Media.SessionID
	manifest.Session.RelativePath = database.Media.RelativePath
	manifest.Session.State = database.Media.State
	manifest.Session.Revision = database.Media.ManifestRevision
	manifest.Session.CreatedAt = database.Media.CreatedAt
	manifest.Session.UpdatedAt = database.Media.UpdatedAt
	manifest.Attempts = append([]capture.MediaAttempt(nil), database.Attempts...)
	manifest.Segments = append([]capture.MediaSegment(nil), database.Segments...)
	manifestPayload, err := encodeP3ACCMediaManifestWire(manifest)
	if err != nil {
		t.Fatalf("encode production media manifest: %v", err)
	}
	manifestFilename := filepath.Join(sessionDirectory, "manifests", "media.json")
	if err := os.MkdirAll(filepath.Dir(manifestFilename), 0o700); err != nil {
		t.Fatalf("create manifest directory: %v", err)
	}
	if err := os.WriteFile(manifestFilename, manifestPayload, 0o600); err != nil {
		t.Fatalf("write production media manifest: %v", err)
	}

	state := &p3ACCAcceptanceState{ctx: context.Background()}
	summary, err := summarizeP3ACCMediaManifestFile(state, dataRoot, database)
	if err != nil {
		t.Fatalf("summarize production media manifest: %v", err)
	}
	if !summary.Exists || !summary.MatchesDatabase || !summary.CanonicalHashMatches ||
		!summary.ManifestClean || !summary.AllFilesMatch || summary.FileCheckCount != 1 ||
		summary.FileFailureCount != 0 {
		t.Fatalf("production media evidence did not match: %#v", summary)
	}

	wrongRoot := verifyP3ACCMediaFiles(context.Background(), sessionDirectory, database)
	if wrongRoot.AllFilesMatch || wrongRoot.FileCheckCount != 1 || wrongRoot.FileFailureCount != 1 {
		t.Fatalf("legacy duplicated-session-prefix root did not fail closed: %#v", wrongRoot)
	}
	if _, safe := p3ACCMediaFilename(dataRoot, "../outside.mkv"); safe {
		t.Fatal("media path escape was accepted")
	}

	external := database
	external.MediaRootID = sql.NullString{String: p3ACCTestAttemptID, Valid: true}
	if _, err := summarizeP3ACCMediaManifestFile(state, dataRoot, external); err == nil {
		t.Fatal("external recording root was accepted by internal-only acceptance verification")
	}
}

func TestP3ACCMediaManifestFileRejectsExternalRootWithoutAttempts(t *testing.T) {
	database := p3ACCInternalDatabaseSnapshot{
		Found: true,
		MediaRootID: sql.NullString{
			String: p3ACCTestAttemptID,
			Valid:  true,
		},
	}
	if _, err := summarizeP3ACCMediaManifestFile(
		&p3ACCAcceptanceState{ctx: context.Background()}, t.TempDir(), database,
	); err == nil {
		t.Fatal("external recording root with zero attempts was accepted")
	}
	database.Found = false
	summary, err := summarizeP3ACCMediaManifestFile(
		&p3ACCAcceptanceState{ctx: context.Background()}, t.TempDir(), database,
	)
	if err != nil || summary.Exists {
		t.Fatalf("absent database snapshot did not remain empty: summary=%#v err=%v", summary, err)
	}
}

func TestP3ACCMediaFilenameRejectsNonRelativePathForms(t *testing.T) {
	mediaRoot := t.TempDir()
	for _, relativePath := range []string{
		"../outside.mkv",
		"media/../../outside.mkv",
		"/absolute.mkv",
		"//server/share/absolute.mkv",
		"C:/drive-absolute.mkv",
		`C:\drive-absolute.mkv`,
		`media\backslash.mkv`,
	} {
		if filename, safe := p3ACCMediaFilename(mediaRoot, relativePath); safe || filename != "" {
			t.Fatalf("unsafe media path accepted: %q", relativePath)
		}
	}
	valid := "rooms/test-room/sessions/2026/07/test/media/segment.mkv"
	filename, safe := p3ACCMediaFilename(mediaRoot, valid)
	if !safe || !p3ACCAcceptancePathWithin(mediaRoot, filename, false) {
		t.Fatal("valid data-root-relative media path was rejected")
	}
}

func TestP3ACCMediaVerificationCacheReturnsWithoutRehashingSameRoot(t *testing.T) {
	mediaRoot := t.TempDir()
	rootIdentity, valid := captureP3ACCMediaRootIdentity(mediaRoot)
	if !valid {
		t.Fatal("capture media root identity failed")
	}
	database := p3ACCInternalDatabaseSnapshot{
		SessionID: p3ACCTestSessionID,
		Media: capture.SessionMedia{
			ManifestRevision: 17,
			UpdatedAt:        18,
		},
	}
	cached := p3ACCMediaManifestSummary{
		AllFilesMatch:            true,
		FileCheckCount:           3,
		SequenceContinuous:       true,
		AttemptReferencesValid:   true,
		FaultPhaseSegmentsProven: true,
	}
	state := &p3ACCAcceptanceState{
		ctx:                        context.Background(),
		mediaVerificationRevision:  database.Media.ManifestRevision,
		mediaVerificationUpdatedAt: database.Media.UpdatedAt,
		mediaVerificationSessionID: database.SessionID,
		mediaVerificationRoot:      rootIdentity,
		mediaVerification:          cached,
	}
	verifierCalled := false
	verified := verifyP3ACCMediaFilesCachedWithVerifier(
		state, mediaRoot, database,
		func(context.Context, string, p3ACCInternalDatabaseSnapshot,
			*p3ACCMediaRootGuard, p3ACCMediaRootIdentity,
		) p3ACCMediaManifestSummary {
			verifierCalled = true
			return p3ACCMediaManifestSummary{FileFailureCount: 1}
		},
	)
	if verifierCalled || verified != cached {
		t.Fatalf("same-root cache did not take the verified fast return: called=%v result=%#v", verifierCalled, verified)
	}
}

func TestP3ACCMediaVerificationCacheBindsRootAndGeneration(t *testing.T) {
	payload := []byte("original-cache-evidence")
	digest := fmt.Sprintf("%x", sha256.Sum256(payload))
	relativePath := "media/segment.mkv"
	database := p3ACCInternalDatabaseSnapshot{
		SessionID: p3ACCTestSessionID,
		Media: capture.SessionMedia{
			ManifestRevision: 11,
			UpdatedAt:        12,
		},
		Segments: []capture.MediaSegment{{
			RelativePath: relativePath,
			SizeBytes:    int64(len(payload)),
			SHA256:       digest,
			Status:       capture.MediaSegmentRecovered,
		}},
	}
	newCachedState := func(t *testing.T, root string) *p3ACCAcceptanceState {
		t.Helper()
		identity, valid := captureP3ACCMediaRootIdentity(root)
		if !valid {
			t.Fatal("capture media root identity failed")
		}
		return &p3ACCAcceptanceState{
			ctx:                        context.Background(),
			mediaVerificationRevision:  database.Media.ManifestRevision,
			mediaVerificationUpdatedAt: database.Media.UpdatedAt,
			mediaVerificationSessionID: database.SessionID,
			mediaVerificationRoot:      identity,
			mediaVerification: p3ACCMediaManifestSummary{
				AllFilesMatch:            true,
				FileCheckCount:           1,
				SequenceContinuous:       true,
				AttemptReferencesValid:   true,
				FaultPhaseSegmentsProven: true,
			},
		}
	}
	writeFixture := func(t *testing.T, root string, content []byte) {
		t.Helper()
		filename := filepath.Join(root, filepath.FromSlash(relativePath))
		if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
			t.Fatalf("create cache fixture directory: %v", err)
		}
		if err := os.WriteFile(filename, content, 0o600); err != nil {
			t.Fatalf("write cache fixture: %v", err)
		}
	}

	t.Run("different root", func(t *testing.T) {
		firstRoot := t.TempDir()
		secondRoot := t.TempDir()
		writeFixture(t, firstRoot, payload)
		writeFixture(t, secondRoot, []byte("changed-cache-evidence"))
		state := newCachedState(t, firstRoot)
		verified := verifyP3ACCMediaFilesCached(state, secondRoot, database)
		if verified.AllFilesMatch || verified.FileCheckCount != 1 || verified.FileFailureCount != 1 {
			t.Fatalf("cache was reused across media roots: %#v", verified)
		}
	})

	t.Run("same path aba", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "data")
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatalf("create original media root: %v", err)
		}
		writeFixture(t, root, payload)
		state := newCachedState(t, root)
		originalIdentity := state.mediaVerificationRoot
		if err := os.RemoveAll(root); err != nil {
			t.Fatalf("remove original media root: %v", err)
		}
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatalf("recreate media root: %v", err)
		}
		writeFixture(t, root, []byte("changed-cache-evidence"))
		replacementIdentity, valid := captureP3ACCMediaRootIdentity(root)
		if !valid || sameP3ACCMediaRootIdentity(originalIdentity, replacementIdentity) {
			t.Fatal("same-path media root replacement did not change its identity")
		}
		verified := verifyP3ACCMediaFilesCached(state, root, database)
		if verified.AllFilesMatch || verified.FileCheckCount != 1 || verified.FileFailureCount != 1 {
			t.Fatalf("cache was reused across same-path root replacement: %#v", verified)
		}
	})
}

func TestP3ACCMediaFileEvidenceRejectsConcurrentPathReplacement(t *testing.T) {
	mediaRoot := t.TempDir()
	relativePath := "media/segment.mkv"
	filename := filepath.Join(mediaRoot, filepath.FromSlash(relativePath))
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		t.Fatalf("create replacement fixture directory: %v", err)
	}
	payload := []byte("same-content-new-identity")
	if err := os.WriteFile(filename, payload, 0o600); err != nil {
		t.Fatalf("write original replacement fixture: %v", err)
	}
	staleFilename := filename + ".stale"
	size, digest, valid := readP3ACCMediaFileEvidenceWithHook(
		context.Background(), mediaRoot, relativePath,
		func() {
			if err := os.Rename(filename, staleFilename); err != nil {
				t.Fatalf("rename original media file: %v", err)
			}
			if err := os.WriteFile(filename, payload, 0o600); err != nil {
				t.Fatalf("write same-content replacement: %v", err)
			}
		},
	)
	if valid || size != 0 || digest != "" {
		t.Fatalf("concurrent same-content path replacement was accepted: size=%d digest=%q", size, digest)
	}
}

func TestP3ACCBoundedManifestRejectsConcurrentPathReplacement(t *testing.T) {
	dataRoot := t.TempDir()
	manifestDirectory := filepath.Join(dataRoot, "manifests")
	if err := os.Mkdir(manifestDirectory, 0o700); err != nil {
		t.Fatalf("create manifest directory: %v", err)
	}
	filename := filepath.Join(manifestDirectory, "media.json")
	payload := []byte("same-content-new-manifest-identity")
	if err := os.WriteFile(filename, payload, 0o600); err != nil {
		t.Fatalf("write original manifest: %v", err)
	}
	staleFilename := filename + ".stale"
	readPayload, err := readP3ACCBoundedFileWithHooks(
		dataRoot, filename, int64(len(payload)+1), nil,
		func() {
			if err := os.Rename(filename, staleFilename); err != nil {
				t.Fatalf("rename original manifest: %v", err)
			}
			if err := os.WriteFile(filename, payload, 0o600); err != nil {
				t.Fatalf("write same-content replacement manifest: %v", err)
			}
		},
	)
	if err == nil || readPayload != nil {
		t.Fatalf("concurrent same-content manifest replacement was accepted: payload=%q", readPayload)
	}
	stablePayload, err := readP3ACCBoundedFile(
		dataRoot, filename, int64(len(payload)+1),
	)
	if err != nil || !bytes.Equal(stablePayload, payload) {
		t.Fatalf("stable replacement manifest was not readable: payload=%q err=%v", stablePayload, err)
	}
}

func TestP3ACCBoundedManifestPreservesMissingFileError(t *testing.T) {
	dataRoot := t.TempDir()
	payload, err := readP3ACCBoundedFile(dataRoot, filepath.Join(dataRoot, "missing.json"), 1024)
	if payload != nil || !os.IsNotExist(err) {
		t.Fatalf("missing manifest error was not preserved: payload=%q err=%v", payload, err)
	}
}

func TestP3ACCMediaFilesMatchDurableSizeDigestAndStatus(t *testing.T) {
	sessionDirectory := t.TempDir()
	segmentPayload := []byte("segment-evidence")
	artifactPayload := []byte("artifact-evidence")
	segmentPath := "media/segments/segment.mkv"
	artifactPath := "media/artifacts/audio.wav"
	for relative, payload := range map[string][]byte{
		segmentPath:  segmentPayload,
		artifactPath: artifactPayload,
	} {
		filename := filepath.Join(sessionDirectory, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
			t.Fatalf("create media directory: %v", err)
		}
		if err := os.WriteFile(filename, payload, 0o600); err != nil {
			t.Fatalf("write media fixture: %v", err)
		}
	}
	segmentDigest := sha256.Sum256(segmentPayload)
	artifactDigest := sha256.Sum256(artifactPayload)
	database := p3ACCInternalDatabaseSnapshot{
		Segments: []capture.MediaSegment{{
			ID: p3ACCTestSessionID, RelativePath: segmentPath, SizeBytes: int64(len(segmentPayload)),
			SHA256: strings.ToLower(strings.TrimSpace(fmt.Sprintf("%x", segmentDigest))),
			Status: capture.MediaSegmentRecovered,
		}},
		Artifacts: []capture.MediaArtifact{{
			MediaSegmentID: p3ACCTestSessionID, RelativePath: artifactPath,
			SizeBytes:    int64(len(artifactPayload)),
			SHA256:       strings.ToLower(strings.TrimSpace(fmt.Sprintf("%x", artifactDigest))),
			SourceSHA256: strings.ToLower(strings.TrimSpace(fmt.Sprintf("%x", segmentDigest))),
			Status:       capture.MediaArtifactComplete,
		}},
	}
	summary := verifyP3ACCMediaFiles(context.Background(), sessionDirectory, database)
	if !summary.AllFilesMatch || summary.FileCheckCount != 2 || summary.FileFailureCount != 0 {
		t.Fatalf("durable media evidence did not match: %#v", summary)
	}
	database.Artifacts[0].SourceSHA256 = ""
	summary = verifyP3ACCMediaFiles(context.Background(), sessionDirectory, database)
	if summary.AllFilesMatch || summary.FileFailureCount != 1 {
		t.Fatalf("complete artifact accepted without a bound source digest: %#v", summary)
	}
	database.Artifacts[0].SourceSHA256 = fmt.Sprintf("%x", segmentDigest)
	database.Segments[0].Status = capture.MediaSegmentCorrupt
	database.Segments[0].SourceRelativePath = segmentPath
	database.Segments[0].ErrorCode = "MEDIA_PROBE_UNREADABLE"
	summary = verifyP3ACCMediaFiles(context.Background(), sessionDirectory, database)
	if summary.AllFilesMatch || summary.FileFailureCount != 1 {
		t.Fatalf("complete artifact accepted with a non-durable source segment: %#v", summary)
	}
	database.Segments[0].Status = capture.MediaSegmentRecovered
	database.Segments[0].SourceRelativePath = ""
	database.Segments[0].ErrorCode = ""
	if err := os.WriteFile(filepath.Join(sessionDirectory, filepath.FromSlash(segmentPath)),
		[]byte("tampered"), 0o600); err != nil {
		t.Fatalf("tamper segment fixture: %v", err)
	}
	summary = verifyP3ACCMediaFiles(context.Background(), sessionDirectory, database)
	if summary.AllFilesMatch || summary.FileFailureCount != 2 {
		t.Fatalf("tampered media evidence did not fail closed: %#v", summary)
	}
}

func TestP3ACCIncompleteMediaUsesStatusSemanticsWithoutHashingMissingEntry(t *testing.T) {
	sessionDirectory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sessionDirectory, "media"), 0o700); err != nil {
		t.Fatalf("create media directory: %v", err)
	}
	payload := []byte("durable-recovered-segment")
	durablePath := "media/durable.mkv"
	if err := os.WriteFile(filepath.Join(sessionDirectory, filepath.FromSlash(durablePath)), payload, 0o600); err != nil {
		t.Fatalf("write durable media: %v", err)
	}
	digest := sha256.Sum256(payload)
	database := p3ACCInternalDatabaseSnapshot{Segments: []capture.MediaSegment{
		{RelativePath: durablePath, SizeBytes: int64(len(payload)), SHA256: fmt.Sprintf("%x", digest), Status: capture.MediaSegmentRecovered},
		{RelativePath: "media/missing.mkv", SizeBytes: int64(len(payload)), SHA256: fmt.Sprintf("%x", digest),
			Status: capture.MediaSegmentMissing, ErrorCode: "MEDIA_FINAL_MISSING"},
	}}
	verified := verifyP3ACCMediaFiles(context.Background(), sessionDirectory, database)
	if !verified.AllFilesMatch || verified.FileCheckCount != 1 || verified.FileFailureCount != 0 ||
		verified.IncompleteEntryCount != 1 || verified.IncompleteSegmentCount != 1 {
		t.Fatalf("incomplete semantic verification failed: %#v", verified)
	}
	verified.State = capture.SessionMediaIncomplete
	verified.SegmentCount = 2
	verified.SequenceContinuous = true
	verified.AttemptReferencesValid = true
	verified.FaultPhaseSegmentsProven = true
	if !validP3ACCTerminalMediaEvidence(verified) {
		t.Fatalf("valid incomplete terminal media rejected: %#v", verified)
	}
	verified.State = capture.SessionMediaCompleted
	if validP3ACCTerminalMediaEvidence(verified) {
		t.Fatal("completed media accepted with an incomplete segment")
	}
}

func TestP3ACCCorruptMediaRequiresErrorSpecificPhysicalEvidence(t *testing.T) {
	sessionDirectory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sessionDirectory, "media"), 0o700); err != nil {
		t.Fatalf("create media directory: %v", err)
	}
	durablePayload := []byte("durable-segment")
	durablePath := "media/durable.mkv"
	if err := os.WriteFile(
		filepath.Join(sessionDirectory, filepath.FromSlash(durablePath)), durablePayload, 0o600,
	); err != nil {
		t.Fatalf("write durable segment: %v", err)
	}
	durableDigest := sha256.Sum256(durablePayload)
	corruptPayload := []byte("corrupt-source-evidence")
	corruptSourcePath := "media/corrupt.partial.mkv"
	if err := os.WriteFile(
		filepath.Join(sessionDirectory, filepath.FromSlash(corruptSourcePath)), corruptPayload, 0o600,
	); err != nil {
		t.Fatalf("write corrupt source: %v", err)
	}
	corruptDigest := sha256.Sum256(corruptPayload)
	database := p3ACCInternalDatabaseSnapshot{Segments: []capture.MediaSegment{
		{RelativePath: durablePath, SizeBytes: int64(len(durablePayload)),
			SHA256: fmt.Sprintf("%x", durableDigest), Status: capture.MediaSegmentRecovered},
		{RelativePath: "media/corrupt.mkv", SourceRelativePath: corruptSourcePath,
			SizeBytes: int64(len(corruptPayload)), SHA256: fmt.Sprintf("%x", corruptDigest),
			Status: capture.MediaSegmentCorrupt, ErrorCode: "MEDIA_PROBE_UNREADABLE"},
	}}
	verified := verifyP3ACCMediaFiles(context.Background(), sessionDirectory, database)
	if !verified.AllFilesMatch || verified.FileFailureCount != 0 {
		t.Fatalf("safe corrupt-source evidence rejected: %#v", verified)
	}
	if err := os.Remove(filepath.Join(sessionDirectory, filepath.FromSlash(corruptSourcePath))); err != nil {
		t.Fatalf("remove corrupt source: %v", err)
	}
	verified = verifyP3ACCMediaFiles(context.Background(), sessionDirectory, database)
	if verified.AllFilesMatch || verified.FileFailureCount != 1 {
		t.Fatalf("corrupt entry without physical evidence accepted: %#v", verified)
	}

	changedPath := filepath.Join(sessionDirectory, "media", "corrupt.mkv")
	if err := os.WriteFile(changedPath, []byte("changed-after-baseline"), 0o600); err != nil {
		t.Fatalf("write changed final: %v", err)
	}
	database.Segments[1].ErrorCode = "MEDIA_FINAL_CHANGED"
	verified = verifyP3ACCMediaFiles(context.Background(), sessionDirectory, database)
	if !verified.AllFilesMatch || verified.FileFailureCount != 0 {
		t.Fatalf("changed final with mismatched baseline rejected: %#v", verified)
	}
	if err := os.WriteFile(changedPath, corruptPayload, 0o600); err != nil {
		t.Fatalf("restore baseline bytes: %v", err)
	}
	verified = verifyP3ACCMediaFiles(context.Background(), sessionDirectory, database)
	if verified.AllFilesMatch || verified.FileFailureCount != 1 {
		t.Fatalf("MEDIA_FINAL_CHANGED accepted an unchanged file: %#v", verified)
	}
}

func TestP3ACCFailedArtifactBindsSourceAndErrorSpecificTargetEvidence(t *testing.T) {
	sessionDirectory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sessionDirectory, "media"), 0o700); err != nil {
		t.Fatalf("create media directory: %v", err)
	}
	payload := []byte("durable-artifact-source")
	sourcePath := "media/source.mkv"
	if err := os.WriteFile(
		filepath.Join(sessionDirectory, filepath.FromSlash(sourcePath)), payload, 0o600,
	); err != nil {
		t.Fatalf("write source segment: %v", err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(payload))
	database := p3ACCInternalDatabaseSnapshot{
		Segments: []capture.MediaSegment{{
			ID: p3ACCTestSessionID, RelativePath: sourcePath,
			SizeBytes: int64(len(payload)), SHA256: digest,
			Status: capture.MediaSegmentRecovered,
		}},
		Artifacts: []capture.MediaArtifact{{
			MediaSegmentID: p3ACCTestSessionID, Kind: capture.MediaArtifactASRWAV,
			RelativePath: "media/failed.wav", SourceSHA256: digest,
			Status: capture.MediaArtifactFailed, ErrorCode: "MEDIA_ARTIFACT_FAILED",
		}},
	}
	verified := verifyP3ACCMediaFiles(context.Background(), sessionDirectory, database)
	if !verified.AllFilesMatch || verified.FileFailureCount != 0 {
		t.Fatalf("failed generation with durable source rejected: %#v", verified)
	}
	database.Artifacts[0].SourceSHA256 = strings.Repeat("0", sha256.Size*2)
	verified = verifyP3ACCMediaFiles(context.Background(), sessionDirectory, database)
	if verified.AllFilesMatch || verified.FileFailureCount != 1 {
		t.Fatalf("failed artifact accepted without bound source evidence: %#v", verified)
	}
	database.Artifacts[0].SourceSHA256 = digest
	database.Artifacts[0].ErrorCode = "MEDIA_ARTIFACT_CONFLICT"
	verified = verifyP3ACCMediaFiles(context.Background(), sessionDirectory, database)
	if verified.AllFilesMatch || verified.FileFailureCount != 1 {
		t.Fatalf("artifact conflict accepted without conflicting target: %#v", verified)
	}
	if err := os.WriteFile(
		filepath.Join(sessionDirectory, "media", "failed.wav"), []byte("conflict"), 0o600,
	); err != nil {
		t.Fatalf("write conflicting target: %v", err)
	}
	verified = verifyP3ACCMediaFiles(context.Background(), sessionDirectory, database)
	if !verified.AllFilesMatch || verified.FileFailureCount != 0 {
		t.Fatalf("artifact conflict with safe physical evidence rejected: %#v", verified)
	}
}

func TestP3ACCRecorderGapProofBindsExactAttemptOperationAndFault(t *testing.T) {
	gap := p3ACCRecorderRecoveryGapEvidence{
		StartedAt: 101, EndedAt: sql.NullInt64{Int64: 102, Valid: true}, Recovered: true,
		ReasonCode: capture.RecorderProcessExitedErrorCode,
		DedupeKey:  "recorder-recovery:" + p3ACCTestAttemptID,
		Details: p3ACCRecorderRecoveryGapDetails{
			Version: 1, SourceAttemptID: p3ACCTestAttemptID,
			SourceOperationID: p3ACCTestOperationID,
			SourceErrorCode:   capture.RecorderProcessExitedErrorCode,
			RestartAttempts:   1, LastErrorCode: capture.RecorderProcessExitedErrorCode,
			LastOccurredAtMS: 102,
		},
	}
	if !p3ACCRecorderGapMatches(
		gap, p3ACCTestAttemptID, p3ACCTestOperationID, capture.RecorderProcessExitedErrorCode, 100,
	) {
		t.Fatal("exact durable crash gap rejected")
	}
	if p3ACCRecorderGapMatches(
		gap, p3ACCTestSessionID, p3ACCTestOperationID, capture.RecorderProcessExitedErrorCode, 100,
	) {
		t.Fatal("wrong source attempt accepted")
	}
}

func TestP3ACCNetworkRecorderOperationAcceptsBothDurableRaceOrders(t *testing.T) {
	message := p3ACCMessageRecoveryGapEvidence{
		StartedAt: 200, OpenedOperationID: p3ACCTestSessionID,
	}
	if !p3ACCNetworkRecorderOperationMatches(
		199, message, p3ACCTestOperationID, p3ACCTestOperationID,
	) || p3ACCNetworkRecorderOperationMatches(
		199, message, p3ACCTestOperationID, message.OpenedOperationID,
	) {
		t.Fatal("recorder-first ordering did not bind exclusively to the baseline operation")
	}
	if !p3ACCNetworkRecorderOperationMatches(
		201, message, p3ACCTestOperationID, message.OpenedOperationID,
	) || p3ACCNetworkRecorderOperationMatches(
		201, message, p3ACCTestOperationID, p3ACCTestOperationID,
	) {
		t.Fatal("message-first ordering did not bind exclusively to the opened operation")
	}
	if !p3ACCNetworkRecorderOperationMatches(
		200, message, p3ACCTestOperationID, p3ACCTestOperationID,
	) || !p3ACCNetworkRecorderOperationMatches(
		200, message, p3ACCTestOperationID, message.OpenedOperationID,
	) {
		t.Fatal("same-millisecond ordering did not accept both durable operation bindings")
	}
	if p3ACCNetworkRecorderOperationMatches(
		200, message, p3ACCTestOperationID, p3ACCTestAttemptID,
	) {
		t.Fatal("same-millisecond ordering accepted an unrelated operation")
	}
}

func TestP3ACCMediaLineageRequiresAllThreeFaultPhaseSegments(t *testing.T) {
	const (
		crashAttempt    = "018f47a0-7c00-7000-8000-000000000104"
		networkAttempt  = "018f47a0-7c00-7000-8000-000000000105"
		finalAttempt    = "018f47a0-7c00-7000-8000-000000000106"
		crashSegment    = "018f47a0-7c00-7000-8000-000000000107"
		networkSegment  = "018f47a0-7c00-7000-8000-000000000108"
		finalSegment    = "018f47a0-7c00-7000-8000-000000000109"
		firstArtifactID = "018f47a0-7c00-7000-8000-000000000110"
	)
	database := p3ACCInternalDatabaseSnapshot{
		Attempts: []capture.MediaAttempt{
			{ID: crashAttempt, Ordinal: 1, Committed: true},
			{ID: networkAttempt, Ordinal: 2, Committed: true},
			{ID: finalAttempt, Ordinal: 3, Committed: true},
		},
		Segments: []capture.MediaSegment{
			{ID: crashSegment, Sequence: 1, AttemptID: crashAttempt, AttemptSequence: 1, Status: capture.MediaSegmentComplete},
			{ID: networkSegment, Sequence: 2, AttemptID: networkAttempt, AttemptSequence: 1, Status: capture.MediaSegmentRecovered},
			{ID: finalSegment, Sequence: 3, AttemptID: finalAttempt, AttemptSequence: 1, Status: capture.MediaSegmentComplete},
		},
		Artifacts: []capture.MediaArtifact{
			{ID: firstArtifactID, MediaSegmentID: crashSegment, Kind: capture.MediaArtifactASRWAV},
			{ID: "018f47a0-7c00-7000-8000-000000000111", MediaSegmentID: crashSegment, Kind: capture.MediaArtifactPlaybackMP4},
			{ID: "018f47a0-7c00-7000-8000-000000000112", MediaSegmentID: networkSegment, Kind: capture.MediaArtifactASRWAV},
			{ID: "018f47a0-7c00-7000-8000-000000000113", MediaSegmentID: networkSegment, Kind: capture.MediaArtifactPlaybackMP4},
			{ID: "018f47a0-7c00-7000-8000-000000000114", MediaSegmentID: finalSegment, Kind: capture.MediaArtifactASRWAV},
			{ID: "018f47a0-7c00-7000-8000-000000000115", MediaSegmentID: finalSegment, Kind: capture.MediaArtifactPlaybackMP4},
		},
		CurrentAttemptID: finalAttempt,
	}
	continuous, references, phases := verifyP3ACCMediaLineage(database, crashAttempt, networkAttempt)
	if !continuous || !references || !phases {
		t.Fatalf("valid three-phase lineage rejected: continuous=%t references=%t phases=%t", continuous, references, phases)
	}
	artifacts := database.Artifacts
	database.Artifacts = database.Artifacts[:len(database.Artifacts)-1]
	_, references, _ = verifyP3ACCMediaLineage(database, crashAttempt, networkAttempt)
	if references {
		t.Fatal("durable segment without both required artifact kinds accepted")
	}
	database.Artifacts = artifacts
	database.Segments[0].Status = capture.MediaSegmentCorrupt
	_, references, _ = verifyP3ACCMediaLineage(database, crashAttempt, networkAttempt)
	if references {
		t.Fatal("artifacts attached to a non-durable segment accepted")
	}
	database.Segments[0].Status = capture.MediaSegmentComplete
	database.Segments[1].Sequence = 4
	continuous, _, _ = verifyP3ACCMediaLineage(database, crashAttempt, networkAttempt)
	if continuous {
		t.Fatal("non-contiguous global segment sequence accepted")
	}
	database.Segments[1].Sequence = 2
	database.Segments[2].AttemptID = crashAttempt
	database.Segments[2].AttemptSequence = 2
	_, references, phases = verifyP3ACCMediaLineage(database, crashAttempt, networkAttempt)
	if !references || phases {
		t.Fatalf("missing post-network attempt segment proof not rejected: references=%t phases=%t", references, phases)
	}
}

func TestP3ACCEmbeddedScriptOwnsStrictFaultOrchestration(t *testing.T) {
	for _, required := range []string{
		"stableWindowProven", "cpuWithinTarget", "CrashP3ACCAcceptanceRecorder",
		"recoveryProven", "ArmP3ACCAcceptanceNetworkFault", "networkFaultArmed",
		"尚无录制状态", "AckP3ACCAcceptanceLiveEventRendered", "requestAnimationFrame",
		"for (;;)",
	} {
		if !strings.Contains(p3ACCAcceptanceScript, required) {
			t.Fatalf("embedded script lacks strict phase marker %q", required)
		}
	}
	for _, forbidden := range []string{
		"console.log", "console.error", "innerHTML", "outerHTML",
		"iteration < 3600", "P3ACC_UI_TIMEOUT",
	} {
		if strings.Contains(p3ACCAcceptanceScript, forbidden) {
			t.Fatalf("embedded script contains forbidden output surface %q", forbidden)
		}
	}
}

func TestP3ACCEmbeddedScriptLeavesProbeLifetimeToController(t *testing.T) {
	const loopStartMarker = "    for (;;) {\n"
	const loopEndMarker = "      await sleep(1000)\n    }\n"
	start := strings.Index(p3ACCAcceptanceScript, loopStartMarker)
	if start < 0 {
		t.Fatal("embedded script lacks controller-owned observation loop")
	}
	endOffset := strings.Index(p3ACCAcceptanceScript[start:], loopEndMarker)
	if endOffset < 0 {
		t.Fatal("embedded observation loop lacks fixed one-second tail")
	}
	body := p3ACCAcceptanceScript[start : start+endOffset+len(loopEndMarker)]
	if strings.Count(body, "\n          return\n") != 1 ||
		!strings.Contains(body, "dataset.p3AccAcceptance = 'ready'\n          return") {
		t.Fatal("embedded observation loop must return only after final evidence is ready")
	}
	for _, forbidden := range []string{"break", "throw new Error", "setTimeout("} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("embedded observation loop contains independent lifetime control %q", forbidden)
		}
	}
}

func TestP3ACCTerminalSessionEvidenceRequiresCleanOfflineCombination(t *testing.T) {
	testCases := []struct {
		name            string
		status          capture.SessionStatus
		recordingStatus capture.RecordingStatus
		want            bool
	}{
		{name: "clean_offline", status: capture.SessionCompleted, recordingStatus: capture.RecordingIncomplete, want: true},
		{name: "cleanup_error", status: capture.SessionInterrupted, recordingStatus: capture.RecordingIncomplete},
		{name: "completed_recording", status: capture.SessionCompleted, recordingStatus: capture.RecordingCompleted},
		{name: "failed_session", status: capture.SessionFailed, recordingStatus: capture.RecordingFailed},
		{name: "interrupted_completed", status: capture.SessionInterrupted, recordingStatus: capture.RecordingCompleted},
		{name: "active_session", status: capture.SessionRecording, recordingStatus: capture.RecordingIncomplete},
		{name: "empty", status: "", recordingStatus: ""},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := validP3ACCCleanOfflineTerminalOutcome(testCase.status, testCase.recordingStatus); got != testCase.want {
				t.Fatalf("validP3ACCCleanOfflineTerminalOutcome(%q, %q) = %t, want %t",
					testCase.status, testCase.recordingStatus, got, testCase.want)
			}
		})
	}
}

func TestP3ACCTerminalFinalizeFailureMapsToImmediateErrorStage(t *testing.T) {
	const code = "CAPTURE_FINALIZE_FAILED"
	if safeP3ACCErrorCode(code) != code || !validP3ACCErrorCode(code) {
		t.Fatal("terminal finalize failure code was not preserved")
	}
	status := p3ACCObservedStatus{
		State:           room.RuntimeFinalizing,
		RecordingStatus: capture.RecordingIncomplete,
		ErrorCode:       code,
	}
	database := p3ACCInternalDatabaseSnapshot{
		Found:           true,
		SessionStatus:   capture.SessionFinalizing,
		RecordingStatus: capture.RecordingFinalizing,
	}
	if got := determineP3ACCStage(status, database, true, true, false); got != p3ACCStageError {
		t.Fatalf("terminal finalize failure stage = %s, want %s", got, p3ACCStageError)
	}
	status.RetryAt = time.Now().Add(time.Second).UnixMilli()
	if got := determineP3ACCStage(status, database, true, true, false); got != p3ACCStageFinalizing {
		t.Fatalf("retryable finalize stage = %s, want %s", got, p3ACCStageFinalizing)
	}
}

func TestP3ACCUIEventLatencySummaryUsesBoundedP95AndPendingFence(t *testing.T) {
	state := &p3ACCAcceptanceState{uiLatencySamples: []int64{10, 20, 30, 40, 900}}
	state.updateP3ACCUIEventLatencyLocked()
	if state.ui.LatencySampleCount != 5 || state.ui.LatencyP95MS != 900 ||
		state.ui.LatencyMaxMS != 900 || !state.ui.LatencyWithinTarget {
		t.Fatalf("unexpected UI latency summary: %#v", state.ui)
	}
	state.uiLatencyPending = []p3ACCUIEventLatencyPending{{emittedAt: time.Now(), eventCount: 1}}
	state.updateP3ACCUIEventLatencyLocked()
	if state.ui.LatencyPendingCount != 1 || state.ui.LatencyWithinTarget {
		t.Fatalf("pending UI ack did not fail closed: %#v", state.ui)
	}
	state.uiLatencyPending = nil
	state.uiLatencySamples = []int64{1_000}
	state.updateP3ACCUIEventLatencyLocked()
	if state.ui.LatencyWithinTarget {
		t.Fatalf("one-second UI P95 incorrectly met strict sub-second target: %#v", state.ui)
	}
	state.uiLatencyInvalid = true
	state.uiLatencySamples = []int64{10}
	state.updateP3ACCUIEventLatencyLocked()
	if state.ui.LatencyWithinTarget {
		t.Fatal("invalid/overflow latency state was accepted")
	}
}

func TestP3ACCLiveURLIsOneUseAndUnsetBeforeInfrastructure(t *testing.T) {
	clearP3ACCBootstrap()
	t.Cleanup(clearP3ACCBootstrap)
	t.Setenv("P3ACC_LIVE_URL", "p3-acc-private-fixture")
	root := t.TempDir()
	t.Setenv("P3ACC_ROOT", root)
	t.Setenv("P3ACC_RESULT_PATH", filepath.Join(root, "evidence", p3ACCAcceptanceResultName))
	if err := os.WriteFile(filepath.Join(root, p3ACCAcceptanceSentinelName),
		[]byte(p3ACCAcceptanceSentinelContent), 0o600); err != nil {
		t.Fatalf("write controller sentinel: %v", err)
	}
	if err := captureP3ACCBootstrap(); err != nil {
		t.Fatalf("captureP3ACCBootstrap() error = %v", err)
	}
	if _, exists := os.LookupEnv("P3ACC_LIVE_URL"); exists {
		t.Fatal("P3ACC_LIVE_URL remains in process environment")
	}
	value, paths, err := takeP3ACCBootstrap()
	if err != nil || value == "" {
		t.Fatalf("takeP3ACCBootstrap() returned empty private value: %v", err)
	}
	if paths.Root != filepath.Clean(root) {
		t.Fatal("bootstrap paths were not pinned")
	}
	value = ""
	if _, _, err := takeP3ACCBootstrap(); err == nil {
		t.Fatal("private bootstrap value was reusable")
	}
}

func TestP3ACCResourceSummaryUsesOnlyBoundedProcessTreeAggregates(t *testing.T) {
	base := time.Unix(200, 0)
	summary := summarizeP3ACCResources([]p3ACCResourceSample{
		{CapturedAt: base, Complete: true, ProcessCPU100NS: 1_000, ProcessCount: 3, WorkingSetBytes: 100, PrivateBytes: 80, ThreadCount: 8, HandleCount: 10},
		{CapturedAt: base.Add(10 * time.Second), Complete: true, ProcessCPU100NS: 2_000, ProcessCount: 4, WorkingSetBytes: 200, PrivateBytes: 160, ThreadCount: 9, HandleCount: 12},
		{CapturedAt: base.Add(20 * time.Second), Complete: true, ProcessCPU100NS: 3_000, ProcessCount: 4, WorkingSetBytes: 300, PrivateBytes: 240, ThreadCount: 10, HandleCount: 14},
	})
	if summary.SampleCount != 3 || summary.ProcessCount.Peak != 4 || summary.WorkingSet.Delta != 200 ||
		summary.Handles.LatterHalfTrend == "INSUFFICIENT" || summary.AverageCPUPercent < 0 ||
		summary.StableWindowProven || summary.CPUWithinTarget {
		t.Fatalf("unexpected resource aggregates: %#v", summary)
	}
	payload, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("marshal resource summary: %v", err)
	}
	for _, forbidden := range []string{"capturedAt", "takenAt", "processId", "recorderPid", p3ACCTestRoomID} {
		if strings.Contains(strings.ToLower(string(payload)), strings.ToLower(forbidden)) {
			t.Fatalf("resource aggregate leaks %q: %s", forbidden, payload)
		}
	}
}

func TestP3ACCStageCannotBeAdvancedByUIOrTerminalStatusAlone(t *testing.T) {
	status := p3ACCObservedStatus{State: room.RuntimeRecording, RecordingStatus: capture.RecordingActive}
	database := p3ACCInternalDatabaseSnapshot{Found: true, SessionStatus: capture.SessionCompleted}
	if stage := determineP3ACCStage(status, database, true, false, false); stage == p3ACCStageFinalized {
		t.Fatal("terminal database state alone advanced finalized stage")
	}
	if stage := determineP3ACCStage(status, database, true, true, false); stage != p3ACCStageRecovered {
		t.Fatalf("real recovery proof stage = %q, want recovered", stage)
	}
	if stage := determineP3ACCStage(status, database, true, true, true); stage != p3ACCStageFinalized {
		t.Fatalf("full finalization proof stage = %q, want finalized", stage)
	}
}

func TestP3ACCSteadyWindowRequiresFreshMonotonicProgressAndStableFence(t *testing.T) {
	base := time.Unix(300, 0)
	state := &p3ACCAcceptanceState{
		progress:                  p3ACCProgressSummary{SampleCount: 1, ElapsedMS: 1_000, BytesWritten: 0, SegmentCount: 1, RestartCount: 1},
		progressInternalUpdatedAt: 1,
		progressReceivedAt:        base,
		progressFrame:             10,
	}
	status := p3ACCObservedStatus{
		State: room.RuntimeRecording, RecordingStatus: capture.RecordingActive,
		SessionID: p3ACCTestSessionID, OperationID: p3ACCTestOperationID,
	}
	database := p3ACCInternalDatabaseSnapshot{
		Found: true, SessionID: p3ACCTestSessionID, OperationID: p3ACCTestOperationID,
		CurrentAttemptID: p3ACCTestAttemptID, CurrentAttemptCommitted: true,
	}
	media := capture.P3AcceptanceRecorderMediaActivity{FileCount: 1, TotalBytes: 100}
	state.updateP3ACCSteadyLocked(base, status, database, true, true, media, true)
	if state.progress.SteadySampleCount != 1 || state.progress.SteadyRecordingMS != 0 {
		t.Fatalf("unexpected initial steady baseline: %#v", state.progress)
	}
	state.progress.SampleCount++
	state.progress.ElapsedMS = 2_000
	state.progressFrame = 20
	state.progressInternalUpdatedAt = 2
	state.progressReceivedAt = base.Add(time.Second)
	media.TotalBytes = 200
	state.updateP3ACCSteadyLocked(base.Add(time.Second), status, database, true, true, media, true)
	if state.progress.SteadySampleCount != 2 || state.progress.SteadyRecordingMS != 1_000 {
		t.Fatalf("zero-byte muxer progress did not advance physical steady window: %#v", state.progress)
	}
	state.updateP3ACCSteadyLocked(base.Add(7*time.Second), status, database, true, true, media, true)
	if state.progress.SteadyRecordingMS != 0 || state.progress.SteadySampleCount != 0 {
		t.Fatalf("stale progress did not reset steady window: %#v", state.progress)
	}
}

func TestP3ACCSteadyWindowRejectsSyntheticOrUnboundActivity(t *testing.T) {
	base := time.Unix(320, 0)
	newState := func() *p3ACCAcceptanceState {
		return &p3ACCAcceptanceState{
			progress: p3ACCProgressSummary{
				SampleCount: 1, ElapsedMS: 1_000, BytesWritten: 0, SegmentCount: 1, RestartCount: 1,
			},
			progressInternalUpdatedAt: 1, progressReceivedAt: base, progressFrame: 10,
		}
	}
	status := p3ACCObservedStatus{
		State: room.RuntimeRecording, RecordingStatus: capture.RecordingActive,
		SessionID: p3ACCTestSessionID, OperationID: p3ACCTestOperationID,
	}
	database := p3ACCInternalDatabaseSnapshot{
		Found: true, SessionID: p3ACCTestSessionID, OperationID: p3ACCTestOperationID,
		CurrentAttemptID: p3ACCTestAttemptID, CurrentAttemptCommitted: true,
	}
	media := capture.P3AcceptanceRecorderMediaActivity{FileCount: 1, TotalBytes: 100}

	state := newState()
	state.updateP3ACCSteadyLocked(base, status, database, true, true, media, true)
	state.progress.SampleCount++
	state.progress.SegmentCount = 2
	state.progressInternalUpdatedAt = 2
	state.progressReceivedAt = base.Add(time.Second)
	media.TotalBytes = 200
	state.updateP3ACCSteadyLocked(base.Add(time.Second), status, database, true, true, media, true)
	if state.progress.SteadyRecordingMS != 0 || state.progress.SteadySampleCount != 1 {
		t.Fatalf("updatedAt/segment-only activity retained steady duration: %#v", state.progress)
	}

	state = newState()
	media.TotalBytes = 100
	state.updateP3ACCSteadyLocked(base, status, database, true, true, media, true)
	for second := 1; second <= 6; second++ {
		state.progress.SampleCount++
		state.progress.ElapsedMS += 1_000
		state.progressFrame += 10
		state.progressInternalUpdatedAt++
		state.progressReceivedAt = base.Add(time.Duration(second) * time.Second)
		state.updateP3ACCSteadyLocked(
			base.Add(time.Duration(second)*time.Second), status, database, true, true, media, true,
		)
	}
	if state.progress.SteadyRecordingMS != 0 || state.progress.SteadySampleCount != 1 {
		t.Fatalf("stalled physical bytes retained steady duration: %#v", state.progress)
	}

	state = newState()
	state.updateP3ACCSteadyLocked(base, status, database, true, true, media, true)
	state.progress.ElapsedMS = 2_000
	state.progressFrame = 20
	state.progressInternalUpdatedAt = 2
	state.progressReceivedAt = base.Add(time.Second)
	media.TotalBytes = 200
	state.updateP3ACCSteadyLocked(base.Add(time.Second), status, database, true, false, media, true)
	if state.progress.SteadySampleCount != 0 {
		t.Fatal("exited or mismatched recorder retained steady evidence")
	}
}

func TestP3ACCRecoveryProgressRequiresSecondPhysicalSampleFromAdvancedAttempt(t *testing.T) {
	base := time.Unix(340, 0)
	state := &p3ACCAcceptanceState{
		progress: p3ACCProgressSummary{
			SampleCount: 2, ElapsedMS: 10_000, BytesWritten: 0, SegmentCount: 2, RestartCount: 2,
		},
		progressInternalUpdatedAt: 2, progressReceivedAt: base, progressFrame: 100,
	}
	database := p3ACCInternalDatabaseSnapshot{
		CurrentAttemptID: p3ACCTestAttemptID, CurrentAttemptCommitted: true,
	}
	media := capture.P3AcceptanceRecorderMediaActivity{FileCount: 1, TotalBytes: 100}
	var baseline p3ACCRecoveryProgressBaseline
	if state.p3ACCRecoveryProgressAdvancedLocked(
		base, &baseline, database, 1, 1, true, media, true,
	) {
		t.Fatal("first new-attempt sample proved recovery")
	}
	state.progressInternalUpdatedAt = 3
	state.progressReceivedAt = base.Add(time.Second)
	state.progress.SegmentCount++
	media.TotalBytes = 200
	if state.p3ACCRecoveryProgressAdvancedLocked(
		base.Add(time.Second), &baseline, database, 1, 1, true, media, true,
	) {
		t.Fatal("updatedAt/segment/physical-only sample proved recovery")
	}
	state.progress.ElapsedMS++
	state.progressFrame++
	if !state.p3ACCRecoveryProgressAdvancedLocked(
		base.Add(time.Second), &baseline, database, 1, 1, true, media, true,
	) {
		t.Fatal("strict zero-byte progress plus physical growth did not prove recovery")
	}
	if state.p3ACCRecoveryProgressAdvancedLocked(
		base.Add(10*time.Second), &baseline, database, 1, 1, true, media, true,
	) {
		t.Fatal("stale recovery progress was accepted")
	}
}

func TestP3ACCResourceTargetRequiresCompleteTenMinuteWindow(t *testing.T) {
	base := time.Unix(400, 0)
	samples := make([]p3ACCResourceSample, p3ACCAcceptanceMinimumStableSamples)
	for index := range samples {
		samples[index] = p3ACCResourceSample{
			CapturedAt: base.Add(time.Duration(index) * p3ACCAcceptanceStableWindow /
				time.Duration(p3ACCAcceptanceMinimumStableSamples-1)),
			Complete: true, ProcessCPU100NS: int64(index) * 10_000,
			ProcessReadBytes: int64(index) * 1_000, ProcessWriteBytes: int64(index) * 2_000,
			DataRootPhysicalBytes: int64(index) * 3_000,
			ProcessCount:          3, WorkingSetBytes: 100, PrivateBytes: 80,
			ThreadCount: 8, HandleCount: 20,
			DatabaseWALBytes: int64(index%4) * 4_096, DatabaseWALPresent: true,
			EventQueueCount: 1, EventQueueItems: int64(index % 2), EventQueueBytes: int64(index%2) * 1_024,
			EventQueueItemCapacity: 64, EventQueueByteCapacity: 1 << 20,
		}
	}
	summary := summarizeP3ACCResources(samples)
	if !summary.SampleComplete || !summary.StableWindowProven || !summary.CPUWithinTarget ||
		!summary.DatabaseWALObserved || !summary.DiskIOObserved || !summary.EventQueueObserved ||
		summary.AverageDiskWriteBytesPerSecond <= 0 ||
		summary.WindowDurationMS < p3ACCAcceptanceStableWindow.Milliseconds() {
		t.Fatalf("complete stable window not proven: %#v", summary)
	}
	if !validP3ACCResourceSummary(summary) {
		t.Fatalf("complete stable resource summary rejected: %#v", summary)
	}
	if early := summarizeP3ACCResources(samples[:1]); !validP3ACCResourceSummary(early) ||
		early.EventQueueItemCapacity.LatterHalfTrend != "INSUFFICIENT" {
		t.Fatalf("valid first resource sample rejected: %#v", early)
	}
	forgedCapacity := summary
	forgedCapacity.EventQueueItemCapacity.LatterHalfTrend = "RISING"
	if validP3ACCEventQueueCapacity(forgedCapacity) || validP3ACCResourceSummary(forgedCapacity) {
		t.Fatal("forged zero-delta rising capacity trend was accepted")
	}
	forgedPeak := summary
	forgedPeak.EventQueueItems.Peak = forgedPeak.EventQueueItems.Latest - 1
	if validP3ACCResourceSummary(forgedPeak) {
		t.Fatal("metric peak below latest was accepted")
	}
	forgedTrend := summary
	forgedTrend.WorkingSet.LatterHalfTrend = "RISING"
	if validP3ACCResourceSummary(forgedTrend) {
		t.Fatal("metric trend inconsistent with delta/threshold was accepted")
	}
	forgedDiskRate := summary
	forgedDiskRate.AverageDiskWriteBytesPerSecond = 0
	if validP3ACCResourceSummary(forgedDiskRate) {
		t.Fatal("positive persisted-footprint delta with zero rate was accepted")
	}
	forgedProcessRate := summary
	forgedProcessRate.AverageProcessWriteBytesPerSecond = 0
	if validP3ACCResourceSummary(forgedProcessRate) {
		t.Fatal("positive process-I/O delta with zero rate was accepted")
	}
	samples[len(samples)/2].Complete = false
	summary = summarizeP3ACCResources(samples)
	if summary.SampleComplete || summary.StableWindowProven || summary.CPUWithinTarget {
		t.Fatalf("incomplete process sample did not fail closed: %#v", summary)
	}
	for index := range samples {
		samples[index].Complete = true
		samples[index].DatabaseWALPresent = false
	}
	summary = summarizeP3ACCResources(samples)
	if summary.StableWindowProven || summary.DatabaseWALObserved {
		t.Fatalf("unobserved WAL was accepted: %#v", summary)
	}
	for index := range samples {
		samples[index].DatabaseWALPresent = true
		samples[index].EventQueueCount = 0
	}
	summary = summarizeP3ACCResources(samples)
	if summary.StableWindowProven || summary.EventQueueObserved {
		t.Fatalf("unobserved event queue was accepted: %#v", summary)
	}
	for index := range samples {
		samples[index].EventQueueCount = 1
	}
	samples[len(samples)/2].EventQueueItemCapacity++
	summary = summarizeP3ACCResources(samples)
	if summary.StableWindowProven || validP3ACCEventQueueCapacity(summary) {
		t.Fatalf("changing queue capacity was accepted: %#v", summary)
	}
	samples[len(samples)/2].EventQueueItemCapacity--
	samples[len(samples)/2].EventQueueItems = 65
	summary = summarizeP3ACCResources(samples)
	if summary.StableWindowProven || validP3ACCEventQueueCapacity(summary) {
		t.Fatalf("queue occupancy above real capacity was accepted: %#v", summary)
	}
	samples[len(samples)/2].EventQueueItems = int64((len(samples) / 2) % 2)
	for index := range samples {
		samples[index].DataRootPhysicalBytes = 0
	}
	summary = summarizeP3ACCResources(samples)
	if summary.StableWindowProven || summary.DiskIOObserved {
		t.Fatalf("process I/O without persisted-footprint growth was accepted as disk I/O: %#v", summary)
	}
	for index := range samples {
		samples[index].DataRootPhysicalBytes = int64(index) * 3_000
	}
	gapIndex := len(samples) / 2
	for index := gapIndex; index < len(samples); index++ {
		samples[index].CapturedAt = samples[index].CapturedAt.Add(p3ACCAcceptanceMaximumResourceGap)
	}
	summary = summarizeP3ACCResources(samples)
	if summary.SampleComplete || summary.StableWindowProven || summary.CPUWithinTarget {
		t.Fatalf("resource window with an oversized sample gap did not fail closed: %#v", summary)
	}
}

func TestP3ACCIncompleteResourceAttemptKeepsContinuityButFailsCurrentSummary(t *testing.T) {
	state := &p3ACCAcceptanceState{
		resourceSamples: []p3ACCResourceSample{
			{CapturedAt: time.Unix(1, 0), Complete: true, ProcessCPU100NS: 1},
			{CapturedAt: time.Unix(2, 0), Complete: true, ProcessCPU100NS: 2},
		},
		latestResourceAttemptComplete: true,
	}
	state.appendResourceSampleLocked(p3ACCResourceSample{CapturedAt: time.Unix(3, 0), Complete: false})
	if len(state.resourceSamples) != 2 {
		t.Fatal("one racing attempt destroyed prior coherent samples")
	}
	if summary := state.currentP3ACCResourceSummaryLocked(); summary.SampleComplete ||
		summary.StableWindowProven || summary.CPUWithinTarget {
		t.Fatalf("latest incomplete attempt was hidden by historical samples: %#v", summary)
	}
	state.appendResourceSampleLocked(p3ACCResourceSample{
		CapturedAt: time.Unix(4, 0), Complete: true, ProcessCPU100NS: 3,
	})
	if len(state.resourceSamples) != 3 || !state.currentP3ACCResourceSummaryLocked().SampleComplete {
		t.Fatal("the next coherent attempt did not restore the continuous window immediately")
	}
}

func TestP3ACCResourceContinuityBoundaryRebuildsOnlyOutsideMaximumGap(t *testing.T) {
	newState := func() *p3ACCAcceptanceState {
		return &p3ACCAcceptanceState{
			resourceSamples: []p3ACCResourceSample{{
				CapturedAt: time.Unix(1, 0), Complete: true, ProcessCPU100NS: 1,
			}},
			latestResourceAttemptComplete: true,
		}
	}
	state := newState()
	state.appendResourceSampleLocked(p3ACCResourceSample{
		CapturedAt: time.Unix(31, 0), Complete: true, ProcessCPU100NS: 2,
	})
	if len(state.resourceSamples) != 2 {
		t.Fatal("exactly maximum allowed resource gap reset the window")
	}
	state = newState()
	state.appendResourceSampleLocked(p3ACCResourceSample{
		CapturedAt: time.Unix(31, 1), Complete: true, ProcessCPU100NS: 2,
	})
	if len(state.resourceSamples) != 1 || !state.resourceSamples[0].CapturedAt.Equal(time.Unix(31, 1)) {
		t.Fatal("oversized resource gap did not rebuild from the new good sample")
	}
	state = newState()
	state.appendResourceSampleLocked(p3ACCResourceSample{
		CapturedAt: time.Unix(1, 0), Complete: true, ProcessCPU100NS: 2,
	})
	if len(state.resourceSamples) != 1 || state.resourceSamples[0].ProcessCPU100NS != 2 {
		t.Fatal("non-monotonic resource timestamp did not rebuild the window")
	}
}

func TestP3ACCBestResourceWindowCannotHidePreFaultSamplingFailure(t *testing.T) {
	best := p3ACCResourceSummary{
		SampleComplete: true, StableWindowProven: true, CPUWithinTarget: true,
	}
	state := &p3ACCAcceptanceState{bestResourceSummary: best}
	if summary := state.p3ACCResourceSummaryForSnapshotLocked(); summary.SampleComplete ||
		summary.StableWindowProven || summary.CPUWithinTarget || summary.Frozen {
		t.Fatalf("pre-fault incomplete attempt was hidden by a best window: %#v", summary)
	}
	state.crashInFlight = true
	if summary := state.p3ACCResourceSummaryForSnapshotLocked(); !summary.SampleComplete ||
		!summary.StableWindowProven || !summary.CPUWithinTarget || !summary.Frozen {
		t.Fatalf("proven pre-fault window was not frozen after fault phase began: %#v", summary)
	}
	state.resourceSamplingFailed = true
	if summary := state.p3ACCResourceSummaryForSnapshotLocked(); summary.SampleComplete ||
		summary.StableWindowProven || summary.CPUWithinTarget {
		t.Fatalf("terminal sampler failure was hidden by frozen evidence: %#v", summary)
	}
}

func TestP3ACCUIEvidenceIsOrderedButCannotAdvanceCoreStage(t *testing.T) {
	valid := p3ACCUIObservationSummary{
		Ready: true, RecordingSeen: true, ProgressAdvanced: true, TimelineSeen: true,
		ReconnectingSeen: true, RecoveredSeen: true, NetworkReconnectingSeen: true,
		NetworkRecoveredSeen: true, OfflineSeen: true, FinalizedSeen: true,
		ObservationCount: 10, LatencySampleCount: 1, LatencyP95MS: 10,
		LatencyMaxMS: 10, LatencyWithinTarget: true,
	}
	if !validP3ACCUI(valid) {
		t.Fatal("valid ordered UI evidence rejected")
	}
	invalid := p3ACCUIObservationSummary{RecoveredSeen: true, ObservationCount: 1}
	if validP3ACCUI(invalid) {
		t.Fatal("out-of-order UI evidence accepted")
	}
	status := p3ACCObservedStatus{State: room.RuntimeWaiting, ErrorCode: "ROOM_OFFLINE"}
	if stage := determineP3ACCStage(status, p3ACCInternalDatabaseSnapshot{}, true, false, false); stage == p3ACCStageFinalized {
		t.Fatal("UI-independent core stage advanced without durable proof")
	}
}

func validP3ACCTestSnapshot() p3ACCAcceptanceSnapshot {
	return p3ACCAcceptanceSnapshot{
		Schema: p3ACCAcceptanceSchema, Stage: p3ACCStageRecording, CapturedAt: 100,
		UI: p3ACCUIObservationSummary{Ready: true, ObservationCount: 1},
		Runtime: p3ACCRuntimeSummary{
			State: room.RuntimeRecording, RecordingStatus: capture.RecordingActive,
			Revision: 1, HasSession: true, SessionFenceStable: true,
			CurrentAttemptCommitted: true, AttemptCount: 1, RecorderTargetMatched: true,
		},
		Progress:        p3ACCProgressSummary{SampleCount: 1},
		Database:        p3ACCDatabaseSummary{SessionCount: 1},
		SessionManifest: p3ACCSessionManifestSummary{},
		MediaManifest:   p3ACCMediaManifestSummary{},
		Gaps:            p3ACCGapSummary{},
		Checkpoint:      p3ACCCheckpointSummary{},
		Resources: summarizeP3ACCResources([]p3ACCResourceSample{
			{
				CapturedAt: time.Unix(100, 0), Complete: true, ProcessCPU100NS: 1_000,
				ProcessCount: 3, WorkingSetBytes: 100, PrivateBytes: 80, ThreadCount: 8, HandleCount: 20,
				Goroutines: 10, HeapAllocBytes: 100, HeapInUseBytes: 200, SysBytes: 300,
			},
			{
				CapturedAt: time.Unix(110, 0), Complete: true, ProcessCPU100NS: 2_000,
				ProcessCount: 3, WorkingSetBytes: 200, PrivateBytes: 160, ThreadCount: 9, HandleCount: 21,
				Goroutines: 11, HeapAllocBytes: 200, HeapInUseBytes: 250, SysBytes: 350,
			},
		}),
	}
}
