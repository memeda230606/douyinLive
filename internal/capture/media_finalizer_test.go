package capture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSQLiteSessionMediaFinalizerCompletesRawAndArtifactsIdempotently(t *testing.T) {
	fixture := newMediaFinalizerFixture(t, writeSegmentProbeJSON(validSegmentProbeJSON))
	var asrCalls atomic.Int32
	var playbackCalls atomic.Int32
	fixture.dependencies.generateASR = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
		asrCalls.Add(1)
		return writeTestMediaArtifact(ctx, source, target, []byte("wav-proxy"), verify, nil)
	}
	fixture.dependencies.generatePlayback = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
		playbackCalls.Add(1)
		return writeTestMediaArtifact(ctx, source, target, []byte("mp4-playback"), verify, nil)
	}
	finalizer := fixture.open(t)
	result, err := finalizer.Finalize(context.Background(), []MediaAttempt{fixture.attempt})
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot.Session.State != SessionMediaCompleted || result.Snapshot.Session.ManifestDirty {
		t.Fatalf("unexpected session media state: session=%#v segments=%#v artifacts=%#v warnings=%v",
			result.Snapshot.Session, result.Snapshot.Segments, result.Snapshot.Artifacts, result.WarningCodes)
	}
	if len(result.Snapshot.Segments) != 1 || len(result.Snapshot.Artifacts) != 2 {
		t.Fatalf("unexpected snapshot: %#v", result.Snapshot)
	}
	segment := result.Snapshot.Segments[0]
	if segment.Status != MediaSegmentComplete || segment.VideoCodec != "h264" || segment.AudioCodec != "aac" ||
		segment.DurationMS != 10_000 || segment.SizeBytes == 0 || len(segment.SHA256) != 64 {
		t.Fatalf("unexpected completed segment: %#v", segment)
	}
	if _, err := os.Stat(fixture.partialPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial still exists: %v", err)
	}
	finalPath, err := mediaAbsolutePath(fixture.root, segment.RelativePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(finalPath); err != nil {
		t.Fatalf("final segment missing: %v", err)
	}
	for _, artifact := range result.Snapshot.Artifacts {
		if artifact.Status != MediaArtifactComplete || artifact.SizeBytes == 0 || len(artifact.SHA256) != 64 ||
			artifact.SourceSHA256 != segment.SHA256 {
			t.Fatalf("unexpected artifact: %#v", artifact)
		}
		if artifact.Kind == MediaArtifactASRWAV && (artifact.SampleRate != 16_000 || artifact.Channels != 1) {
			t.Fatalf("unexpected ASR proxy metadata: %#v", artifact)
		}
		if artifact.Kind == MediaArtifactPlaybackMP4 && artifact.Codec != "h264" {
			t.Fatalf("playback manifest codec = %q, want h264", artifact.Codec)
		}
	}
	manifestPath := filepath.Join(fixture.sessionDirectory, "manifests", "media.json")
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest mediaManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != mediaManifestSchemaVersion || manifest.Session.State != SessionMediaCompleted ||
		manifest.Session.Revision != result.Snapshot.Session.ManifestRevision {
		t.Fatalf("manifest does not match snapshot: %#v", manifest.Session)
	}
	for _, secret := range []string{fixture.root, fixture.tools.ffmpegPath, "https://"} {
		if strings.Contains(string(manifestBytes), secret) {
			t.Fatalf("media manifest exposed private value: %q", secret)
		}
	}
	var epoch int64
	var clockSource string
	if err := fixture.repository.reader.QueryRow(`SELECT media_epoch_at, clock_source
		FROM live_sessions WHERE id = ?`, fixture.session.ID).Scan(&epoch, &clockSource); err != nil {
		t.Fatal(err)
	}
	if epoch != fixture.attempt.StartedAt-500 || clockSource != "media" {
		t.Fatalf("media epoch = %d/%s", epoch, clockSource)
	}

	revision := result.Snapshot.Session.ManifestRevision
	retried, err := finalizer.Finalize(context.Background(), []MediaAttempt{fixture.attempt})
	if err != nil {
		t.Fatal(err)
	}
	if retried.Snapshot.Session.ManifestRevision != revision || asrCalls.Load() != 1 || playbackCalls.Load() != 1 {
		t.Fatalf("idempotent retry changed durable output: revision=%d calls=%d/%d",
			retried.Snapshot.Session.ManifestRevision, asrCalls.Load(), playbackCalls.Load())
	}
	assertMediaFinalizerPrivateFormatting(t, finalizer, fixture)
}

func TestSQLiteSessionMediaFinalizerMergesDurableAttemptJournalAcrossRestartInputs(t *testing.T) {
	fixture := newMediaFinalizerFixture(t, writeSegmentProbeJSON(validSegmentProbeJSON))
	fixture.dependencies.generateASR = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
		return writeTestMediaArtifact(ctx, source, target, []byte("valid-wav"), verify, nil)
	}
	fixture.dependencies.generatePlayback = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
		return writeTestMediaArtifact(ctx, source, target, []byte("valid-mp4"), verify, nil)
	}
	finalizer := fixture.open(t)
	first, err := finalizer.Finalize(context.Background(), []MediaAttempt{fixture.attempt})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Snapshot.Session.Attempts) != 1 || first.Snapshot.Session.Attempts[0] != fixture.attempt {
		t.Fatalf("first durable journal = %#v", first.Snapshot.Session.Attempts)
	}

	secondAttempt := fixture.attempt
	secondAttempt.ID = uuid.Must(uuid.NewV7()).String()
	secondAttempt.Ordinal = 2
	secondAttempt.StartedAt += int64(time.Minute / time.Millisecond)
	secondDirectory := filepath.Join(fixture.sessionDirectory, "media", ".attempt-"+secondAttempt.ID)
	if err := os.Mkdir(secondDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	secondPartial := filepath.Join(secondDirectory, mediaAttemptSegmentName(1, secondAttempt))
	if err := os.WriteFile(secondPartial, []byte("second-immutable-matroska"), 0o600); err != nil {
		t.Fatal(err)
	}

	second, err := finalizer.Finalize(context.Background(), []MediaAttempt{secondAttempt})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Snapshot.Session.Attempts) != 2 ||
		second.Snapshot.Session.Attempts[0] != fixture.attempt ||
		second.Snapshot.Session.Attempts[1] != secondAttempt {
		t.Fatalf("suffix-only restart input replaced durable journal: %#v", second.Snapshot.Session.Attempts)
	}
	third, err := finalizer.Finalize(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Snapshot.Session.Attempts) != 2 ||
		third.Snapshot.Session.Attempts[0] != fixture.attempt ||
		third.Snapshot.Session.Attempts[1] != secondAttempt {
		t.Fatalf("empty restart input replaced durable journal: %#v", third.Snapshot.Session.Attempts)
	}
}

func TestSQLiteSessionMediaFinalizerRejectsAttemptJournalConflictAndDowngrade(t *testing.T) {
	fixture := newMediaFinalizerFixture(t, writeSegmentProbeJSON(validSegmentProbeJSON))
	fixture.dependencies.generateASR = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
		return writeTestMediaArtifact(ctx, source, target, []byte("valid-wav"), verify, nil)
	}
	fixture.dependencies.generatePlayback = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
		return writeTestMediaArtifact(ctx, source, target, []byte("valid-mp4"), verify, nil)
	}
	finalizer := fixture.open(t)
	baseline, err := finalizer.Finalize(context.Background(), []MediaAttempt{fixture.attempt})
	if err != nil {
		t.Fatal(err)
	}
	identityConflict := fixture.attempt
	identityConflict.VariantID = "different"
	ordinalConflict := fixture.attempt
	ordinalConflict.ID = uuid.Must(uuid.NewV7()).String()
	committedDowngrade := fixture.attempt
	committedDowngrade.Committed, committedDowngrade.Clean = false, false
	cleanDowngrade := fixture.attempt
	cleanDowngrade.Clean = false
	for name, incoming := range map[string]MediaAttempt{
		"identity":            identityConflict,
		"ordinal":             ordinalConflict,
		"committed-downgrade": committedDowngrade,
		"clean-downgrade":     cleanDowngrade,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := finalizer.Finalize(context.Background(), []MediaAttempt{incoming}); !errors.Is(err, ErrMediaSnapshotConflict) {
				t.Fatalf("Finalize() error = %v, want ErrMediaSnapshotConflict", err)
			}
			after, err := fixture.repository.LoadSnapshot(context.Background(), fixture.session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Session.ManifestRevision != baseline.Snapshot.Session.ManifestRevision ||
				len(after.Session.Attempts) != 1 || after.Session.Attempts[0] != fixture.attempt {
				t.Fatalf("rejected transition changed durable journal: %#v", after.Session)
			}
		})
	}
}

func TestSQLiteSessionMediaFinalizerRetainsUnreadablePartial(t *testing.T) {
	fixture := newMediaFinalizerFixture(t, func(
		context.Context,
		segmentProbeInvocation,
		io.Writer,
		io.Writer,
	) error {
		return errors.New("unreadable")
	})
	fixture.dependencies.generateASR = failUnexpectedMediaGenerator(t)
	fixture.dependencies.generatePlayback = failUnexpectedMediaGenerator(t)
	result, err := fixture.open(t).Finalize(context.Background(), []MediaAttempt{fixture.attempt})
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot.Session.State != SessionMediaIncomplete || len(result.Snapshot.Segments) != 1 ||
		result.Snapshot.Segments[0].Status != MediaSegmentCorrupt {
		t.Fatalf("unexpected corrupt result: %#v", result)
	}
	if result.Snapshot.Segments[0].ErrorCode != "MEDIA_PROBE_UNREADABLE" ||
		!containsMediaWarning(result.WarningCodes, "MEDIA_PROBE_UNREADABLE") {
		t.Fatalf("missing stable probe warning: %#v", result)
	}
	before, err := os.ReadFile(fixture.partialPath)
	if err != nil {
		t.Fatalf("unreadable partial was removed: %v", err)
	}
	if string(before) != fixture.partialContent {
		t.Fatal("unreadable partial bytes changed")
	}
}

func TestSQLiteSessionMediaFinalizerTreatsProxyFailureAsNonFatal(t *testing.T) {
	fixture := newMediaFinalizerFixture(t, writeSegmentProbeJSON(validSegmentProbeJSON))
	fixture.dependencies.generateASR = func(context.Context, string, *mediaArtifactSource, string, mediaArtifactVerifyFunc) error {
		return ErrMediaArtifactFailed
	}
	fixture.dependencies.generatePlayback = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
		return writeTestMediaArtifact(ctx, source, target, []byte("mp4"), verify, nil)
	}
	result, err := fixture.open(t).Finalize(context.Background(), []MediaAttempt{fixture.attempt})
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot.Session.State != SessionMediaCompleted ||
		!containsMediaWarning(result.WarningCodes, "MEDIA_ARTIFACT_FAILED") {
		t.Fatalf("proxy failure changed raw completion: %#v", result)
	}
	statuses := make(map[MediaArtifactKind]MediaArtifactStatus)
	for _, artifact := range result.Snapshot.Artifacts {
		statuses[artifact.Kind] = artifact.Status
	}
	if statuses[MediaArtifactASRWAV] != MediaArtifactFailed ||
		statuses[MediaArtifactPlaybackMP4] != MediaArtifactComplete {
		t.Fatalf("unexpected proxy statuses: %#v", statuses)
	}
}

func TestSQLiteSessionMediaFinalizerLeavesDurablePendingStateOnCancellation(t *testing.T) {
	fixture := newMediaFinalizerFixture(t, writeSegmentProbeJSON(validSegmentProbeJSON))
	ctx, cancel := context.WithCancel(context.Background())
	fixture.dependencies.generateASR = func(context.Context, string, *mediaArtifactSource, string, mediaArtifactVerifyFunc) error {
		cancel()
		return context.Canceled
	}
	fixture.dependencies.generatePlayback = failUnexpectedMediaGenerator(t)
	result, err := fixture.open(t).Finalize(ctx, []MediaAttempt{fixture.attempt})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	if result.Snapshot.Session.State != SessionMediaFinalizing || len(result.Snapshot.Segments) != 1 ||
		result.Snapshot.Segments[0].Status != MediaSegmentComplete {
		t.Fatalf("raw core was not durable before proxy cancellation: %#v", result.Snapshot)
	}
	loaded, loadErr := fixture.repository.LoadSnapshot(context.Background(), fixture.session.ID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if loaded.Session.State != SessionMediaFinalizing || loaded.Session.ManifestDirty {
		t.Fatalf("durable cancellation state = %#v", loaded.Session)
	}
	if len(loaded.Artifacts) != 2 || loaded.Artifacts[0].Status != MediaArtifactPending {
		t.Fatalf("pending proxies were not retained: %#v", loaded.Artifacts)
	}
}

func TestSQLiteSessionMediaFinalizerRejectsInvalidGeneratedArtifact(t *testing.T) {
	fixture := newMediaFinalizerFixture(t, writeSegmentProbeJSON(validSegmentProbeJSON))
	fixture.dependencies.generateASR = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
		return writeTestMediaArtifact(ctx, source, target, []byte("invalid-wav"), verify, nil)
	}
	fixture.dependencies.generatePlayback = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
		return writeTestMediaArtifact(ctx, source, target, []byte("valid-mp4"), verify, nil)
	}
	fixture.dependencies.inspectArtifact = func(
		_ context.Context,
		_, _ string,
		kind MediaArtifactKind,
	) error {
		if kind == MediaArtifactASRWAV {
			return ErrMediaArtifactFailed
		}
		return nil
	}
	result, err := fixture.open(t).Finalize(context.Background(), []MediaAttempt{fixture.attempt})
	if err != nil {
		t.Fatal(err)
	}
	statuses := make(map[MediaArtifactKind]MediaArtifactStatus)
	for _, artifact := range result.Snapshot.Artifacts {
		statuses[artifact.Kind] = artifact.Status
	}
	if statuses[MediaArtifactASRWAV] != MediaArtifactFailed ||
		statuses[MediaArtifactPlaybackMP4] != MediaArtifactComplete ||
		!containsMediaWarning(result.WarningCodes, "MEDIA_ARTIFACT_FAILED") {
		t.Fatalf("post-generation validation was not enforced: %#v", result)
	}
}

func TestSQLiteSessionMediaFinalizerAdoptsPublishedPendingArtifactsAfterCrash(t *testing.T) {
	fixture := newMediaFinalizerFixture(t, writeSegmentProbeJSON(validSegmentProbeJSON))
	var asrCalls atomic.Int32
	var playbackCalls atomic.Int32
	var inspectionCalls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	fixture.dependencies.generateASR = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
		asrCalls.Add(1)
		return writeTestMediaArtifact(ctx, source, target, []byte("published-wav"), verify, nil)
	}
	fixture.dependencies.generatePlayback = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
		playbackCalls.Add(1)
		if err := writeTestMediaArtifact(ctx, source, target, []byte("published-mp4"), verify, nil); err != nil {
			return err
		}
		cancel()
		return nil
	}
	fixture.dependencies.inspectArtifact = func(
		context.Context,
		string,
		string,
		MediaArtifactKind,
	) error {
		inspectionCalls.Add(1)
		return nil
	}
	finalizer := fixture.open(t)
	result, err := finalizer.Finalize(ctx, []MediaAttempt{fixture.attempt})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("crash-window cancellation error = %v", err)
	}
	if result.Snapshot.Session.State != SessionMediaFinalizing {
		t.Fatalf("unexpected pre-crash snapshot: %#v", result.Snapshot.Session)
	}
	durable, err := fixture.repository.LoadSnapshot(context.Background(), fixture.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(durable.Artifacts) != 2 {
		t.Fatalf("durable artifacts = %#v", durable.Artifacts)
	}
	for _, artifact := range durable.Artifacts {
		if artifact.Status != MediaArtifactPending {
			t.Fatalf("artifact was persisted past the simulated crash: %#v", artifact)
		}
		absolutePath, pathErr := mediaAbsolutePath(fixture.root, artifact.RelativePath)
		if pathErr != nil {
			t.Fatal(pathErr)
		}
		if _, statErr := os.Stat(absolutePath); statErr != nil {
			t.Fatalf("published artifact missing after simulated crash: %v", statErr)
		}
	}
	finalizer.dependencies.generateASR = failUnexpectedMediaGenerator(t)
	finalizer.dependencies.generatePlayback = failUnexpectedMediaGenerator(t)
	retried, err := finalizer.Finalize(context.Background(), []MediaAttempt{fixture.attempt})
	if err != nil {
		t.Fatal(err)
	}
	if retried.Snapshot.Session.State != SessionMediaCompleted {
		t.Fatalf("retry did not complete session media: %#v", retried.Snapshot.Session)
	}
	for _, artifact := range retried.Snapshot.Artifacts {
		if artifact.Status != MediaArtifactComplete || artifact.SizeBytes == 0 ||
			len(artifact.SHA256) != 64 {
			t.Fatalf("published artifact was not adopted: %#v", artifact)
		}
	}
	if asrCalls.Load() != 1 || playbackCalls.Load() != 1 || inspectionCalls.Load() != 3 {
		t.Fatalf("retry regenerated artifacts: generators=%d/%d inspections=%d",
			asrCalls.Load(), playbackCalls.Load(), inspectionCalls.Load())
	}
}

func TestSQLiteSessionMediaFinalizerRetriesFailedArtifacts(t *testing.T) {
	tests := []struct {
		name               string
		configure          func(*testing.T, *mediaFinalizerFixture, *atomic.Int32)
		wantGeneratorCalls int32
		wantStatus         MediaArtifactStatus
		wantErrorCode      string
	}{
		{
			name: "generator failure",
			configure: func(t *testing.T, fixture *mediaFinalizerFixture, calls *atomic.Int32) {
				fixture.dependencies.generateASR = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
					if calls.Add(1) == 1 {
						return ErrMediaArtifactFailed
					}
					return writeTestMediaArtifact(ctx, source, target, []byte("valid-wav"), verify, nil)
				}
			},
			wantGeneratorCalls: 2,
			wantStatus:         MediaArtifactComplete,
		},
		{
			name: "inspection failure after publication",
			configure: func(t *testing.T, fixture *mediaFinalizerFixture, calls *atomic.Int32) {
				fixture.dependencies.generateASR = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
					calls.Add(1)
					return writeTestMediaArtifact(ctx, source, target, []byte("valid-wav"), verify, nil)
				}
				var inspections atomic.Int32
				fixture.dependencies.inspectArtifact = func(
					_ context.Context,
					_, _ string,
					kind MediaArtifactKind,
				) error {
					if kind == MediaArtifactASRWAV && inspections.Add(1) == 1 {
						return ErrMediaArtifactFailed
					}
					return nil
				}
			},
			wantGeneratorCalls: 1,
			wantStatus:         MediaArtifactComplete,
		},
		{
			name: "hash failure after publication",
			configure: func(t *testing.T, fixture *mediaFinalizerFixture, calls *atomic.Int32) {
				fixture.dependencies.generateASR = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
					calls.Add(1)
					return writeTestMediaArtifact(ctx, source, target, []byte("valid-wav"), verify, nil)
				}
				var hashes atomic.Int32
				fixture.dependencies.hashFile = func(ctx context.Context, path string) (int64, string, error) {
					if strings.HasSuffix(strings.ToLower(path), ".wav") && hashes.Add(1) == 1 {
						return 0, "", ErrMediaHash
					}
					return hashMediaFile(ctx, path)
				}
			},
			wantGeneratorCalls: 1,
			wantStatus:         MediaArtifactComplete,
		},
		{
			name: "invalid published target",
			configure: func(t *testing.T, fixture *mediaFinalizerFixture, calls *atomic.Int32) {
				fixture.dependencies.generateASR = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
					value := "invalid-wav"
					if calls.Add(1) > 1 {
						value = "valid-wav"
					}
					return writeTestMediaArtifact(ctx, source, target, []byte(value), verify, nil)
				}
				fixture.dependencies.inspectArtifact = func(
					_ context.Context,
					_, path string,
					kind MediaArtifactKind,
				) error {
					if kind != MediaArtifactASRWAV {
						return nil
					}
					payload, err := os.ReadFile(path)
					if err != nil || string(payload) != "valid-wav" {
						return ErrMediaArtifactFailed
					}
					return nil
				}
			},
			wantGeneratorCalls: 1,
			wantStatus:         MediaArtifactFailed,
			wantErrorCode:      "MEDIA_ARTIFACT_CONFLICT",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMediaFinalizerFixture(t, writeSegmentProbeJSON(validSegmentProbeJSON))
			var generatorCalls atomic.Int32
			test.configure(t, fixture, &generatorCalls)
			fixture.dependencies.generatePlayback = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
				return writeTestMediaArtifact(ctx, source, target, []byte("valid-mp4"), verify, nil)
			}
			finalizer := fixture.open(t)
			first, err := finalizer.Finalize(context.Background(), []MediaAttempt{fixture.attempt})
			if err != nil {
				t.Fatalf("first Finalize() error = %v", err)
			}
			firstASR, ok := mediaArtifactOfKind(first.Snapshot.Artifacts, MediaArtifactASRWAV)
			if !ok || firstASR.Status != MediaArtifactFailed {
				t.Fatalf("first ASR artifact = %#v, want failed", firstASR)
			}
			second, err := finalizer.Finalize(context.Background(), []MediaAttempt{fixture.attempt})
			if err != nil {
				t.Fatalf("second Finalize() error = %v", err)
			}
			secondASR, ok := mediaArtifactOfKind(second.Snapshot.Artifacts, MediaArtifactASRWAV)
			if !ok || secondASR.Status != test.wantStatus || secondASR.ErrorCode != test.wantErrorCode {
				t.Fatalf("retried ASR artifact = %#v, want status=%s error=%s",
					secondASR, test.wantStatus, test.wantErrorCode)
			}
			if secondASR.Status == MediaArtifactComplete &&
				(secondASR.SizeBytes <= 0 || !validMediaDigest(secondASR.SHA256)) {
				t.Fatalf("completed ASR artifact lacks evidence: %#v", secondASR)
			}
			if got := generatorCalls.Load(); got != test.wantGeneratorCalls {
				t.Fatalf("ASR generator calls = %d, want %d", got, test.wantGeneratorCalls)
			}
			revision := second.Snapshot.Session.ManifestRevision
			third, err := finalizer.Finalize(context.Background(), []MediaAttempt{fixture.attempt})
			if err != nil {
				t.Fatalf("idempotent Finalize() error = %v", err)
			}
			if third.Snapshot.Session.ManifestRevision != revision || generatorCalls.Load() != test.wantGeneratorCalls {
				t.Fatalf("idempotent retry changed output: revision=%d calls=%d",
					third.Snapshot.Session.ManifestRevision, generatorCalls.Load())
			}
		})
	}
}

func TestSQLiteSessionMediaFinalizerRegeneratesMissingArtifact(t *testing.T) {
	fixture := newMediaFinalizerFixture(t, writeSegmentProbeJSON(validSegmentProbeJSON))
	var asrCalls atomic.Int32
	var playbackCalls atomic.Int32
	fixture.dependencies.generateASR = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
		asrCalls.Add(1)
		return writeTestMediaArtifact(ctx, source, target, []byte("valid-wav"), verify, nil)
	}
	fixture.dependencies.generatePlayback = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
		playbackCalls.Add(1)
		return writeTestMediaArtifact(ctx, source, target, []byte("valid-mp4"), verify, nil)
	}
	finalizer := fixture.open(t)
	first, err := finalizer.Finalize(context.Background(), []MediaAttempt{fixture.attempt})
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range first.Snapshot.Artifacts {
		if artifact.Status != MediaArtifactComplete {
			t.Fatalf("first artifact = %#v", artifact)
		}
		missingPath, pathErr := mediaAbsolutePath(fixture.root, artifact.RelativePath)
		if pathErr != nil {
			t.Fatal(pathErr)
		}
		if err := os.Remove(missingPath); err != nil {
			t.Fatal(err)
		}
	}
	second, err := finalizer.Finalize(context.Background(), []MediaAttempt{fixture.attempt})
	if err != nil {
		t.Fatal(err)
	}
	for _, recovered := range second.Snapshot.Artifacts {
		if recovered.Status != MediaArtifactComplete || recovered.SizeBytes <= 0 ||
			!validMediaDigest(recovered.SHA256) {
			t.Fatalf("regenerated artifact = %#v", recovered)
		}
	}
	if asrCalls.Load() != 2 || playbackCalls.Load() != 2 {
		t.Fatalf("generator calls = %d/%d, want 2/2", asrCalls.Load(), playbackCalls.Load())
	}
}

func TestSQLiteSessionMediaFinalizerPreservesVerifiedSegmentOnTransientReprobeFailure(t *testing.T) {
	fixture := newMediaFinalizerFixture(t, func(
		ctx context.Context,
		invocation segmentProbeInvocation,
		stdout io.Writer,
		stderr io.Writer,
	) error {
		if strings.Contains(filepath.Base(invocation.partialPath), "segment-000002-") {
			return errors.New("transient unreadable second segment")
		}
		return writeSegmentProbeJSON(validSegmentProbeJSON)(ctx, invocation, stdout, stderr)
	})
	secondPartial := filepath.Join(
		filepath.Dir(fixture.partialPath), mediaAttemptSegmentName(2, fixture.attempt),
	)
	if err := os.WriteFile(secondPartial, []byte("second-matroska-fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.dependencies.generateASR = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
		return writeTestMediaArtifact(ctx, source, target, []byte("wav"), verify, nil)
	}
	fixture.dependencies.generatePlayback = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
		return writeTestMediaArtifact(ctx, source, target, []byte("mp4"), verify, nil)
	}
	finalizer := fixture.open(t)
	first, err := finalizer.Finalize(context.Background(), []MediaAttempt{fixture.attempt})
	if err != nil {
		t.Fatal(err)
	}
	firstSegment, ok := mediaSegmentOfSequence(first.Snapshot.Segments, 1)
	if !ok || firstSegment.Status != MediaSegmentComplete || first.Snapshot.Session.State != SessionMediaIncomplete {
		t.Fatalf("first finalization = %#v", first)
	}

	finalizer.prober.dependencies.run = func(
		ctx context.Context,
		invocation segmentProbeInvocation,
		stdout io.Writer,
		stderr io.Writer,
	) error {
		name := filepath.Base(invocation.partialPath)
		if strings.Contains(name, "segment-000001-") && strings.HasSuffix(strings.ToLower(name), ".mkv") {
			return context.DeadlineExceeded
		}
		return writeSegmentProbeJSON(validSegmentProbeJSON)(ctx, invocation, stdout, stderr)
	}
	second, err := finalizer.Finalize(context.Background(), []MediaAttempt{fixture.attempt})
	if err != nil {
		t.Fatal(err)
	}
	preserved, ok := mediaSegmentOfSequence(second.Snapshot.Segments, 1)
	if !ok || second.Snapshot.Session.State != SessionMediaCompleted ||
		preserved.ID != firstSegment.ID || preserved.Status != firstSegment.Status ||
		preserved.SizeBytes != firstSegment.SizeBytes || preserved.SHA256 != firstSegment.SHA256 ||
		!containsMediaWarning(second.WarningCodes, "MEDIA_PROBE_TIMEOUT") {
		t.Fatalf("verified segment was not preserved: first=%#v second=%#v warnings=%v",
			firstSegment, preserved, second.WarningCodes)
	}
	revision := second.Snapshot.Session.ManifestRevision
	third, err := finalizer.Finalize(context.Background(), []MediaAttempt{fixture.attempt})
	if err != nil {
		t.Fatal(err)
	}
	if third.Snapshot.Session.ManifestRevision != revision {
		t.Fatalf("completed retry revision = %d, want %d", third.Snapshot.Session.ManifestRevision, revision)
	}
}

func TestMediaSegmentProcessorDowngradesChangedFinalAfterTransientProbeFailure(t *testing.T) {
	fixture := newMediaFinalizerFixture(t, func(
		context.Context,
		segmentProbeInvocation,
		io.Writer,
		io.Writer,
	) error {
		return context.DeadlineExceeded
	})
	defer fixture.close()
	finalPath := filepath.Join(
		fixture.sessionDirectory,
		"media",
		mediaFinalSegmentName(1, fixture.attempt.StartedAt),
	)
	if err := os.WriteFile(finalPath, []byte("verified-final"), 0o600); err != nil {
		t.Fatal(err)
	}
	size, digest, err := hashMediaFile(context.Background(), finalPath)
	if err != nil {
		t.Fatal(err)
	}
	existing := MediaSegment{
		ID: newV7(t), Sequence: 1,
		RelativePath:       strings.ReplaceAll(finalPath, fixture.root+string(filepath.Separator), ""),
		Container:          "mkv",
		StartedAt:          fixture.attempt.StartedAt,
		EndedAt:            fixture.attempt.StartedAt + 1_000,
		DurationMS:         1_000,
		SizeBytes:          size,
		SHA256:             digest,
		Status:             MediaSegmentComplete,
		AttemptID:          fixture.attempt.ID,
		AttemptSequence:    1,
		SourceRelativePath: fixture.session.DataPath + "/media/.attempt-" + fixture.attempt.ID + "/" + mediaAttemptSegmentName(1, fixture.attempt),
		ProbeVersion:       mediaProbeVersion,
	}
	if err := os.WriteFile(finalPath, []byte("changed-final-media"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate := mediaCandidate{
		Attempt: fixture.attempt, Sequence: 1, AttemptSequence: 1,
		WallStartedAt:      fixture.attempt.StartedAt,
		SourceRelativePath: existing.SourceRelativePath,
		FinalRelativePath:  existing.RelativePath,
		FinalPath:          finalPath,
		AlreadyFinal:       true,
	}
	segment, warnings, err := (mediaSegmentProcessor{
		prober: fixture.prober,
		newID: func() (string, error) {
			return newV7(t), nil
		},
	}).finalize(context.Background(), candidate, &existing)
	if err != nil {
		t.Fatal(err)
	}
	if segment.Status != MediaSegmentCorrupt || segment.ErrorCode != "MEDIA_FINAL_CHANGED" ||
		segment.SizeBytes != existing.SizeBytes || segment.SHA256 != existing.SHA256 ||
		!containsMediaWarning(warnings, "MEDIA_FINAL_CHANGED") {
		t.Fatalf("changed final was not downgraded: segment=%#v warnings=%v", segment, warnings)
	}
}

func TestMediaSegmentProcessorRejectsReadableVerifiedReplacementAcrossRetries(t *testing.T) {
	fixture := newMediaFinalizerFixture(t, writeSegmentProbeJSON(validSegmentProbeJSON))
	t.Cleanup(fixture.close)
	finalPath := filepath.Join(
		fixture.sessionDirectory, "media", mediaFinalSegmentName(1, fixture.attempt.StartedAt),
	)
	if err := os.WriteFile(finalPath, []byte("valid-matroska-A"), 0o600); err != nil {
		t.Fatal(err)
	}
	size, digest, err := hashMediaFile(context.Background(), finalPath)
	if err != nil {
		t.Fatal(err)
	}
	existing := verifiedMediaSegmentForTest(t, fixture, finalPath, size, digest)
	if err := os.WriteFile(finalPath, []byte("valid-matroska-B"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate := mediaCandidateForVerifiedFinal(fixture, existing, finalPath)
	processor := mediaSegmentProcessor{prober: fixture.prober, newID: func() (string, error) {
		return newV7(t), nil
	}}
	for round := 1; round <= 3; round++ {
		segment, warnings, finalizeErr := processor.finalize(context.Background(), candidate, &existing)
		if finalizeErr != nil {
			t.Fatalf("round %d Finalize() error = %v", round, finalizeErr)
		}
		if segment.ID != existing.ID || segment.Status != MediaSegmentCorrupt ||
			segment.ErrorCode != "MEDIA_FINAL_CHANGED" || segment.SizeBytes != size || segment.SHA256 != digest ||
			!containsMediaWarning(warnings, "MEDIA_FINAL_CHANGED") {
			t.Fatalf("round %d adopted readable replacement: segment=%#v warnings=%v", round, segment, warnings)
		}
		existing = segment
	}
}

func TestMediaSegmentProcessorRejectsSameStatReplacementDuringProbe(t *testing.T) {
	var finalPath string
	var replaced atomic.Bool
	probeRun := writeSegmentProbeJSON(validSegmentProbeJSON)
	fixture := newMediaFinalizerFixture(t, func(
		ctx context.Context,
		invocation segmentProbeInvocation,
		stdout io.Writer,
		stderr io.Writer,
	) error {
		if invocation.phase == segmentProbePhaseMetadata && finalPath != "" && replaced.CompareAndSwap(false, true) {
			info, err := os.Stat(finalPath)
			if err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(finalPath, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteAt([]byte("valid-matroska-B"), 0); err != nil {
				file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(finalPath, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
		}
		return probeRun(ctx, invocation, stdout, stderr)
	})
	t.Cleanup(fixture.close)
	finalPath = filepath.Join(
		fixture.sessionDirectory, "media", mediaFinalSegmentName(1, fixture.attempt.StartedAt),
	)
	if err := os.WriteFile(finalPath, []byte("valid-matroska-A"), 0o600); err != nil {
		t.Fatal(err)
	}
	size, digest, err := hashMediaFile(context.Background(), finalPath)
	if err != nil {
		t.Fatal(err)
	}
	existing := verifiedMediaSegmentForTest(t, fixture, finalPath, size, digest)
	candidate := mediaCandidateForVerifiedFinal(fixture, existing, finalPath)
	segment, warnings, err := (mediaSegmentProcessor{
		prober: fixture.prober, newID: func() (string, error) { return newV7(t), nil },
	}).finalize(context.Background(), candidate, &existing)
	if err != nil {
		t.Fatal(err)
	}
	if !replaced.Load() || segment.Status != MediaSegmentCorrupt || segment.ErrorCode != "MEDIA_FINAL_CHANGED" ||
		segment.SizeBytes != size || segment.SHA256 != digest ||
		!containsMediaWarning(warnings, "MEDIA_FINAL_CHANGED") {
		t.Fatalf("probe-time replacement was adopted: segment=%#v warnings=%v", segment, warnings)
	}
}

func TestSQLiteSessionMediaFinalizerMarksDeletedVerifiedRawMissing(t *testing.T) {
	fixture := newMediaFinalizerFixture(t, writeSegmentProbeJSON(validSegmentProbeJSON))
	fixture.dependencies.generateASR = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
		return writeTestMediaArtifact(ctx, source, target, []byte("valid-wav"), verify, nil)
	}
	fixture.dependencies.generatePlayback = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
		return writeTestMediaArtifact(ctx, source, target, []byte("valid-mp4"), verify, nil)
	}
	finalizer := fixture.open(t)
	first, err := finalizer.Finalize(context.Background(), []MediaAttempt{fixture.attempt})
	if err != nil {
		t.Fatal(err)
	}
	verified := first.Snapshot.Segments[0]
	finalPath, err := mediaAbsolutePath(fixture.root, verified.RelativePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(finalPath); err != nil {
		t.Fatal(err)
	}
	for round := 2; round <= 3; round++ {
		result, finalizeErr := finalizer.Finalize(context.Background(), []MediaAttempt{fixture.attempt})
		if finalizeErr != nil {
			t.Fatalf("round %d Finalize() error = %v", round, finalizeErr)
		}
		segment := result.Snapshot.Segments[0]
		if result.Snapshot.Session.State != SessionMediaIncomplete || segment.ID != verified.ID ||
			segment.Status != MediaSegmentMissing || segment.ErrorCode != "MEDIA_FINAL_MISSING" ||
			segment.SizeBytes != verified.SizeBytes || segment.SHA256 != verified.SHA256 {
			t.Fatalf("round %d missing raw was retained as complete: %#v", round, result)
		}
	}
}

func TestSQLiteSessionMediaFinalizerCatchesRawDeletionDuringArtifactGeneration(t *testing.T) {
	fixture := newMediaFinalizerFixture(t, writeSegmentProbeJSON(validSegmentProbeJSON))
	var deleted atomic.Bool
	var deletionErr error
	fixture.dependencies.generateASR = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
		if err := writeTestMediaArtifact(ctx, source, target, []byte("valid-wav"), verify, nil); err != nil {
			return err
		}
		if deleted.CompareAndSwap(false, true) {
			deletionErr = os.Remove(source.path)
			return deletionErr
		}
		return nil
	}
	fixture.dependencies.generatePlayback = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
		return writeTestMediaArtifact(ctx, source, target, []byte("valid-mp4"), verify, nil)
	}
	result, err := fixture.open(t).Finalize(context.Background(), []MediaAttempt{fixture.attempt})
	if err != nil {
		t.Fatal(err)
	}
	artifact, ok := mediaArtifactOfKind(result.Snapshot.Artifacts, MediaArtifactASRWAV)
	if !deleted.Load() || !ok || artifact.Status == MediaArtifactComplete {
		t.Fatalf("raw deletion attempt reached a complete artifact: %#v", result)
	}
	segment := result.Snapshot.Segments[0]
	if deletionErr == nil {
		if result.Snapshot.Session.State != SessionMediaIncomplete ||
			segment.Status != MediaSegmentMissing || segment.ErrorCode != "MEDIA_FINAL_MISSING" {
			t.Fatalf("successful unlink escaped the source audit: %#v", result)
		}
		return
	}
	if result.Snapshot.Session.State != SessionMediaCompleted ||
		segment.Status != MediaSegmentComplete || segment.ErrorCode != "" {
		t.Fatalf("blocked Windows unlink changed verified raw evidence: delete=%v result=%#v", deletionErr, result)
	}
	sourcePath, err := mediaAbsolutePath(fixture.root, segment.RelativePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sourcePath); err != nil {
		t.Fatalf("source disappeared after a blocked unlink: %v", err)
	}
}

func TestSQLiteSessionMediaFinalizerRejectsTransientSourceReplacementDuringArtifactGeneration(t *testing.T) {
	fixture := newMediaFinalizerFixture(t, writeSegmentProbeJSON(validSegmentProbeJSON))
	var attacked atomic.Bool
	fixture.dependencies.generateASR = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
		info, err := os.Stat(source.path)
		if err != nil {
			return err
		}
		original, err := os.ReadFile(source.path)
		if err != nil {
			return err
		}
		attacker := []byte(strings.Repeat("X", len(original)))
		if err := os.WriteFile(source.path, attacker, 0o600); err != nil {
			return err
		}
		if err := os.Chtimes(source.path, info.ModTime(), info.ModTime()); err != nil {
			return err
		}
		payload, err := io.ReadAll(source)
		if err != nil {
			return err
		}
		if err := os.WriteFile(source.path, original, 0o600); err != nil {
			return err
		}
		if err := os.Chtimes(source.path, info.ModTime(), info.ModTime()); err != nil {
			return err
		}
		attacked.Store(true)
		if err := verify(ctx); err != nil {
			return err
		}
		return os.WriteFile(target, append([]byte("wav:"), payload...), 0o600)
	}
	fixture.dependencies.generatePlayback = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
		return writeTestMediaArtifact(ctx, source, target, []byte("valid-mp4"), verify, nil)
	}
	result, err := fixture.open(t).Finalize(context.Background(), []MediaAttempt{fixture.attempt})
	if err != nil {
		t.Fatal(err)
	}
	artifact, ok := mediaArtifactOfKind(result.Snapshot.Artifacts, MediaArtifactASRWAV)
	if !attacked.Load() || !ok {
		t.Fatalf("transient source replacement did not run: %#v", result)
	}
	target, err := mediaAbsolutePath(fixture.root, artifact.RelativePath)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Status != MediaArtifactFailed || artifact.ErrorCode != "MEDIA_ARTIFACT_CONFLICT" {
		t.Fatalf("attacker-derived proxy reached durable state: %#v", artifact)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("attacker-derived proxy was published: %v", err)
	}
}

func TestSQLiteSessionMediaFinalizerRejectsReplacedCompleteArtifacts(t *testing.T) {
	fixture := newMediaFinalizerFixture(t, writeSegmentProbeJSON(validSegmentProbeJSON))
	fixture.dependencies.generateASR = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
		return writeTestMediaArtifact(ctx, source, target, []byte("valid-wav"), verify, nil)
	}
	fixture.dependencies.generatePlayback = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
		return writeTestMediaArtifact(ctx, source, target, []byte("valid-mp4"), verify, nil)
	}
	finalizer := fixture.open(t)
	first, err := finalizer.Finalize(context.Background(), []MediaAttempt{fixture.attempt})
	if err != nil {
		t.Fatal(err)
	}
	baselines := make(map[MediaArtifactKind]MediaArtifact)
	attackers := map[MediaArtifactKind]string{
		MediaArtifactASRWAV: "other-wav", MediaArtifactPlaybackMP4: "other-mp4",
	}
	for _, artifact := range first.Snapshot.Artifacts {
		baselines[artifact.Kind] = artifact
		path, pathErr := mediaAbsolutePath(fixture.root, artifact.RelativePath)
		if pathErr != nil {
			t.Fatal(pathErr)
		}
		if err := os.WriteFile(path, []byte(attackers[artifact.Kind]), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	second, err := finalizer.Finalize(context.Background(), []MediaAttempt{fixture.attempt})
	if err != nil {
		t.Fatal(err)
	}
	if !containsMediaWarning(second.WarningCodes, "MEDIA_ARTIFACT_CHANGED") {
		t.Fatalf("replacement warning missing: %v", second.WarningCodes)
	}
	for _, artifact := range second.Snapshot.Artifacts {
		baseline := baselines[artifact.Kind]
		if artifact.Status != MediaArtifactFailed || artifact.ErrorCode != "MEDIA_ARTIFACT_CHANGED" ||
			artifact.SizeBytes != baseline.SizeBytes || artifact.SHA256 != baseline.SHA256 {
			t.Fatalf("replacement was adopted for %s: baseline=%#v got=%#v", artifact.Kind, baseline, artifact)
		}
		path, _ := mediaAbsolutePath(fixture.root, artifact.RelativePath)
		content, readErr := os.ReadFile(path)
		if readErr != nil || string(content) != attackers[artifact.Kind] {
			t.Fatalf("replacement was deleted for %s: content=%q error=%v", artifact.Kind, content, readErr)
		}
	}
	revision := second.Snapshot.Session.ManifestRevision
	third, err := finalizer.Finalize(context.Background(), []MediaAttempt{fixture.attempt})
	if err != nil || third.Snapshot.Session.ManifestRevision != revision {
		t.Fatalf("terminal artifact conflict was not idempotent: revision=%d/%d error=%v",
			third.Snapshot.Session.ManifestRevision, revision, err)
	}
}

func TestMediaSegmentProcessorRetainsUnknownZeroLengthCandidateAsCorrupt(t *testing.T) {
	fixture := newMediaFinalizerFixture(t, writeSegmentProbeJSON(validSegmentProbeJSON))
	t.Cleanup(fixture.close)
	candidates, err := discoverMediaCandidates(
		fixture.sessionDirectory, fixture.session.DataPath, []MediaAttempt{fixture.attempt},
	)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("discover candidate = %d/%v", len(candidates), err)
	}
	if err := os.WriteFile(fixture.partialPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	segment, warnings, err := (mediaSegmentProcessor{
		prober: fixture.prober, newID: func() (string, error) { return newV7(t), nil },
	}).finalize(context.Background(), candidates[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	if segment.Status != MediaSegmentCorrupt || segment.ErrorCode != "MEDIA_HASH_FAILED" ||
		segment.SizeBytes != 0 || segment.SHA256 != "" ||
		!containsMediaWarning(warnings, "MEDIA_HASH_FAILED") {
		t.Fatalf("unknown zero candidate = %#v warnings=%v", segment, warnings)
	}
	info, statErr := os.Stat(fixture.partialPath)
	if statErr != nil || info.Size() != 0 {
		t.Fatalf("unknown zero candidate was removed: info=%v err=%v", info, statErr)
	}
}

func TestSQLiteSessionMediaFinalizerPersistsChangedForInvalidVerifiedRawObjects(t *testing.T) {
	for _, objectKind := range mediaInvalidObjectKinds() {
		t.Run(objectKind, func(t *testing.T) {
			fixture := newMediaFinalizerFixture(t, writeSegmentProbeJSON(validSegmentProbeJSON))
			fixture.dependencies.generateASR = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
				return writeTestMediaArtifact(ctx, source, target, []byte("valid-wav"), verify, nil)
			}
			fixture.dependencies.generatePlayback = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
				return writeTestMediaArtifact(ctx, source, target, []byte("valid-mp4"), verify, nil)
			}
			finalizer := fixture.open(t)
			first, err := finalizer.Finalize(context.Background(), []MediaAttempt{fixture.attempt})
			if err != nil {
				t.Fatal(err)
			}
			baseline := first.Snapshot.Segments[0]
			path, err := mediaAbsolutePath(fixture.root, baseline.RelativePath)
			if err != nil {
				t.Fatal(err)
			}
			replaceMediaTestPathWithInvalidObject(t, path, objectKind)
			for round := 2; round <= 3; round++ {
				result, finalizeErr := finalizer.Finalize(
					context.Background(), []MediaAttempt{fixture.attempt},
				)
				if finalizeErr != nil {
					t.Fatalf("round %d Finalize() error = %v", round, finalizeErr)
				}
				segment := result.Snapshot.Segments[0]
				if result.Snapshot.Session.State != SessionMediaIncomplete ||
					segment.ID != baseline.ID || segment.Status != MediaSegmentCorrupt ||
					segment.ErrorCode != "MEDIA_FINAL_CHANGED" ||
					segment.SizeBytes != baseline.SizeBytes || segment.SHA256 != baseline.SHA256 {
					t.Fatalf("round %d invalid raw object did not converge: %#v", round, result)
				}
				assertMediaTestInvalidObjectPreserved(t, path, objectKind)
			}
		})
	}
}

func TestSQLiteSessionMediaFinalizerPersistsChangedForInvalidVerifiedArtifactObjects(t *testing.T) {
	for _, objectKind := range mediaInvalidObjectKinds() {
		t.Run(objectKind, func(t *testing.T) {
			fixture := newMediaFinalizerFixture(t, writeSegmentProbeJSON(validSegmentProbeJSON))
			fixture.dependencies.generateASR = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
				return writeTestMediaArtifact(ctx, source, target, []byte("valid-wav"), verify, nil)
			}
			fixture.dependencies.generatePlayback = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
				return writeTestMediaArtifact(ctx, source, target, []byte("valid-mp4"), verify, nil)
			}
			finalizer := fixture.open(t)
			first, err := finalizer.Finalize(context.Background(), []MediaAttempt{fixture.attempt})
			if err != nil {
				t.Fatal(err)
			}
			baselines := make(map[MediaArtifactKind]MediaArtifact, 2)
			paths := make(map[MediaArtifactKind]string, 2)
			for _, artifact := range first.Snapshot.Artifacts {
				path, pathErr := mediaAbsolutePath(fixture.root, artifact.RelativePath)
				if pathErr != nil {
					t.Fatal(pathErr)
				}
				baselines[artifact.Kind] = artifact
				paths[artifact.Kind] = path
				replaceMediaTestPathWithInvalidObject(t, path, objectKind)
			}
			var changedRevision int64
			for round := 2; round <= 3; round++ {
				result, finalizeErr := finalizer.Finalize(
					context.Background(), []MediaAttempt{fixture.attempt},
				)
				if finalizeErr != nil {
					t.Fatalf("round %d Finalize() error = %v", round, finalizeErr)
				}
				if result.Snapshot.Session.State != SessionMediaCompleted || len(result.Snapshot.Artifacts) != 2 {
					t.Fatalf("round %d artifact snapshot = %#v", round, result)
				}
				if round == 2 {
					changedRevision = result.Snapshot.Session.ManifestRevision
					if !containsMediaWarning(result.WarningCodes, "MEDIA_ARTIFACT_CHANGED") {
						t.Fatalf("round 2 warning missing: %v", result.WarningCodes)
					}
				} else if result.Snapshot.Session.ManifestRevision != changedRevision {
					t.Fatalf("terminal artifact revision changed: %d/%d",
						result.Snapshot.Session.ManifestRevision, changedRevision)
				}
				for _, artifact := range result.Snapshot.Artifacts {
					baseline := baselines[artifact.Kind]
					if artifact.Status != MediaArtifactFailed || artifact.ErrorCode != "MEDIA_ARTIFACT_CHANGED" ||
						artifact.SizeBytes != baseline.SizeBytes || artifact.SHA256 != baseline.SHA256 {
						t.Fatalf("round %d invalid %s artifact was adopted: %#v", round, artifact.Kind, artifact)
					}
					assertMediaTestInvalidObjectPreserved(t, paths[artifact.Kind], objectKind)
				}
			}
		})
	}
}

func TestSQLiteSessionMediaFinalizerCatchesArtifactDeletionAfterHash(t *testing.T) {
	fixture := newMediaFinalizerFixture(t, writeSegmentProbeJSON(validSegmentProbeJSON))
	var asrCalls atomic.Int32
	var deleted atomic.Bool
	fixture.dependencies.generateASR = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
		asrCalls.Add(1)
		return writeTestMediaArtifact(ctx, source, target, []byte("valid-wav"), verify, nil)
	}
	fixture.dependencies.generatePlayback = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
		return writeTestMediaArtifact(ctx, source, target, []byte("valid-mp4"), verify, nil)
	}
	fixture.dependencies.hashFile = func(ctx context.Context, path string) (int64, string, error) {
		size, digest, err := hashMediaFile(ctx, path)
		if err == nil && strings.HasSuffix(strings.ToLower(path), ".wav") && deleted.CompareAndSwap(false, true) {
			if removeErr := os.Remove(path); removeErr != nil {
				t.Fatal(removeErr)
			}
		}
		return size, digest, err
	}
	finalizer := fixture.open(t)
	first, err := finalizer.Finalize(context.Background(), []MediaAttempt{fixture.attempt})
	if err != nil {
		t.Fatal(err)
	}
	missing, ok := mediaArtifactOfKind(first.Snapshot.Artifacts, MediaArtifactASRWAV)
	if !deleted.Load() || !ok || missing.Status != MediaArtifactMissing ||
		missing.ErrorCode != "MEDIA_ARTIFACT_MISSING" || first.Snapshot.Session.State != SessionMediaCompleted {
		t.Fatalf("post-hash deletion reached complete artifact: %#v", first)
	}
	second, err := finalizer.Finalize(context.Background(), []MediaAttempt{fixture.attempt})
	if err != nil {
		t.Fatal(err)
	}
	recovered, ok := mediaArtifactOfKind(second.Snapshot.Artifacts, MediaArtifactASRWAV)
	if !ok || recovered.Status != MediaArtifactComplete || asrCalls.Load() != 2 {
		t.Fatalf("missing artifact did not regenerate: artifact=%#v calls=%d", recovered, asrCalls.Load())
	}
}

func TestSQLiteSessionMediaFinalizerRejectsArtifactReplacementDuringInspection(t *testing.T) {
	fixture := newMediaFinalizerFixture(t, writeSegmentProbeJSON(validSegmentProbeJSON))
	var replaced atomic.Bool
	fixture.dependencies.generateASR = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
		return writeTestMediaArtifact(ctx, source, target, []byte("valid-wav"), verify, nil)
	}
	fixture.dependencies.generatePlayback = func(ctx context.Context, _ string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
		return writeTestMediaArtifact(ctx, source, target, []byte("valid-mp4"), verify, nil)
	}
	fixture.dependencies.inspectArtifact = func(
		_ context.Context, _, path string, kind MediaArtifactKind,
	) error {
		if kind == MediaArtifactASRWAV && replaced.CompareAndSwap(false, true) {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteAt([]byte("other-wav"), 0); err != nil {
				file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	result, err := fixture.open(t).Finalize(context.Background(), []MediaAttempt{fixture.attempt})
	if err != nil {
		t.Fatal(err)
	}
	artifact, ok := mediaArtifactOfKind(result.Snapshot.Artifacts, MediaArtifactASRWAV)
	if !replaced.Load() || !ok || artifact.Status != MediaArtifactFailed ||
		artifact.ErrorCode != "MEDIA_ARTIFACT_CONFLICT" || artifact.SizeBytes <= 0 || artifact.SHA256 == "" {
		t.Fatalf("inspection-time artifact replacement was adopted: %#v", result)
	}
}

func verifiedMediaSegmentForTest(
	t *testing.T,
	fixture *mediaFinalizerFixture,
	finalPath string,
	size int64,
	digest string,
) MediaSegment {
	t.Helper()
	return MediaSegment{
		ID: newV7(t), Sequence: 1,
		RelativePath: strings.ReplaceAll(finalPath, fixture.root+string(filepath.Separator), ""),
		Container:    "mkv", VideoCodec: "h264", AudioCodec: "aac",
		StartedAt: fixture.attempt.StartedAt, EndedAt: fixture.attempt.StartedAt + 1_000,
		DurationMS: 1_000, SizeBytes: size, SHA256: digest, Status: MediaSegmentComplete,
		AttemptID: fixture.attempt.ID, AttemptSequence: 1,
		SourceRelativePath: fixture.session.DataPath + "/media/.attempt-" + fixture.attempt.ID + "/" +
			mediaAttemptSegmentName(1, fixture.attempt),
		ProbeVersion: mediaProbeVersion,
	}
}

func mediaCandidateForVerifiedFinal(
	fixture *mediaFinalizerFixture,
	existing MediaSegment,
	finalPath string,
) mediaCandidate {
	return mediaCandidate{
		Attempt: fixture.attempt, Sequence: 1, AttemptSequence: 1,
		WallStartedAt:      fixture.attempt.StartedAt,
		SourceRelativePath: existing.SourceRelativePath, FinalRelativePath: existing.RelativePath,
		FinalPath: finalPath, AlreadyFinal: true,
	}
}

func mediaArtifactOfKind(artifacts []MediaArtifact, kind MediaArtifactKind) (MediaArtifact, bool) {
	for _, artifact := range artifacts {
		if artifact.Kind == kind {
			return artifact, true
		}
	}
	return MediaArtifact{}, false
}

func mediaSegmentOfSequence(segments []MediaSegment, sequence int) (MediaSegment, bool) {
	for _, segment := range segments {
		if segment.Sequence == sequence {
			return segment, true
		}
	}
	return MediaSegment{}, false
}

func TestMediaSegmentProcessorKeepsPublishConflictCorruptAcrossRetries(t *testing.T) {
	fixture := newMediaFinalizerFixture(t, writeSegmentProbeJSON(validSegmentProbeJSON))
	t.Cleanup(fixture.close)
	finalName := mediaFinalSegmentName(1, fixture.attempt.StartedAt)
	finalPath := filepath.Join(fixture.sessionDirectory, "media", finalName)
	finalContent := "competing-final-media"
	if err := os.WriteFile(finalPath, []byte(finalContent), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate := mediaCandidate{
		Attempt: fixture.attempt, Sequence: 1, AttemptSequence: 1,
		WallStartedAt: fixture.attempt.StartedAt,
		SourceRelativePath: fixture.session.DataPath + "/media/.attempt-" + fixture.attempt.ID + "/" +
			mediaAttemptSegmentName(1, fixture.attempt),
		FinalRelativePath: fixture.session.DataPath + "/media/" + finalName,
		PartialPath:       fixture.partialPath, FinalPath: finalPath,
	}
	processor := mediaSegmentProcessor{
		prober: fixture.prober,
		newID: func() (string, error) {
			return newV7(t), nil
		},
	}

	var existing *MediaSegment
	for round := 1; round <= 3; round++ {
		candidate.AlreadyFinal = round > 1
		segment, warnings, err := processor.finalize(context.Background(), candidate, existing)
		if err != nil {
			t.Fatalf("round %d finalize error = %v", round, err)
		}
		if segment.Status != MediaSegmentCorrupt || segment.ErrorCode != "MEDIA_TARGET_CONFLICT" {
			t.Fatalf("round %d upgraded conflict: %#v", round, segment)
		}
		if !containsMediaWarning(warnings, "MEDIA_TARGET_CONFLICT") {
			t.Fatalf("round %d warnings = %v, want MEDIA_TARGET_CONFLICT", round, warnings)
		}
		partial, err := os.ReadFile(fixture.partialPath)
		if err != nil {
			t.Fatalf("round %d partial removed: %v", round, err)
		}
		final, err := os.ReadFile(finalPath)
		if err != nil {
			t.Fatalf("round %d final removed: %v", round, err)
		}
		if string(partial) != fixture.partialContent || string(final) != finalContent {
			t.Fatalf("round %d changed conflict files: partial=%q final=%q", round, partial, final)
		}
		existing = &segment
	}
	if err := os.Remove(fixture.partialPath); err != nil {
		t.Fatal(err)
	}
	segment, warnings, err := processor.finalize(context.Background(), candidate, existing)
	if err != nil {
		t.Fatalf("compare-failure finalize error = %v", err)
	}
	if segment.Status != MediaSegmentCorrupt || segment.ErrorCode != "MEDIA_TARGET_CONFLICT" {
		t.Fatalf("compare failure upgraded conflict: %#v", segment)
	}
	if !containsMediaWarning(warnings, "MEDIA_DUPLICATE_CHECK_FAILED") ||
		!containsMediaWarning(warnings, "MEDIA_TARGET_CONFLICT") {
		t.Fatalf("compare-failure warnings = %v", warnings)
	}
	final, err := os.ReadFile(finalPath)
	if err != nil || string(final) != finalContent {
		t.Fatalf("compare failure changed final: content=%q error=%v", final, err)
	}
}

func TestMediaSegmentProcessorRecoveryPreservesDuplicatePartialEvidence(t *testing.T) {
	for _, alreadyFinal := range []bool{true, false} {
		name := "publish_conflict"
		if alreadyFinal {
			name = "already_final"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newMediaFinalizerFixture(t, writeSegmentProbeJSON(validSegmentProbeJSON))
			t.Cleanup(fixture.close)
			partialContent, err := os.ReadFile(fixture.partialPath)
			if err != nil {
				t.Fatal(err)
			}
			finalName := mediaFinalSegmentName(1, fixture.attempt.StartedAt)
			finalPath := filepath.Join(fixture.sessionDirectory, "media", finalName)
			if err := os.WriteFile(finalPath, partialContent, 0o600); err != nil {
				t.Fatal(err)
			}
			candidate := mediaCandidate{
				Attempt: fixture.attempt, Sequence: 1, AttemptSequence: 1,
				WallStartedAt: fixture.attempt.StartedAt,
				SourceRelativePath: fixture.session.DataPath + "/media/.attempt-" + fixture.attempt.ID + "/" +
					mediaAttemptSegmentName(1, fixture.attempt),
				FinalRelativePath: fixture.session.DataPath + "/media/" + finalName,
				PartialPath:       fixture.partialPath,
				FinalPath:         finalPath,
				AlreadyFinal:      alreadyFinal,
			}
			processor := mediaSegmentProcessor{
				prober: fixture.prober,
				newID: func() (string, error) {
					return newV7(t), nil
				},
				recovering: true,
			}
			segment, warnings, err := processor.finalize(context.Background(), candidate, nil)
			if err != nil {
				t.Fatal(err)
			}
			if segment.Status != MediaSegmentRecovered || containsMediaWarning(warnings, "MEDIA_TARGET_CONFLICT") {
				t.Fatalf("unexpected recovery result: segment=%#v warnings=%v", segment, warnings)
			}
			preservedPartial, err := os.ReadFile(fixture.partialPath)
			if err != nil {
				t.Fatalf("recovery removed partial evidence: %v", err)
			}
			preservedFinal, err := os.ReadFile(finalPath)
			if err != nil {
				t.Fatalf("recovery removed final evidence: %v", err)
			}
			if string(preservedPartial) != string(partialContent) || string(preservedFinal) != string(partialContent) {
				t.Fatalf("recovery changed evidence: partial=%q final=%q", preservedPartial, preservedFinal)
			}
		})
	}
}

func TestSQLiteSessionMediaFinalizerRejectsExternalRootDriftBeforeMutation(t *testing.T) {
	probeRun := writeSegmentProbeJSON(validSegmentProbeJSON)
	var probeCalls atomic.Int32
	var driftRoot func()
	fixture := newMediaFinalizerFixture(t, func(
		ctx context.Context,
		invocation segmentProbeInvocation,
		stdout io.Writer,
		stderr io.Writer,
	) error {
		probeCalls.Add(1)
		if invocation.phase == segmentProbePhaseMetadata && driftRoot != nil {
			drift := driftRoot
			driftRoot = nil
			drift()
		}
		return probeRun(ctx, invocation, stdout, stderr)
	})
	t.Cleanup(fixture.close)

	externalRoot := t.TempDir()
	registered, err := fixture.repository.RegisterRecordingRoot(context.Background(), externalRoot)
	if err != nil {
		t.Fatal(err)
	}
	sessionDirectory, err := secureMediaSessionDirectory(externalRoot, fixture.session.DataPath)
	if err != nil {
		t.Fatal(err)
	}
	attemptDirectory := filepath.Join(sessionDirectory, "media", ".attempt-"+fixture.attempt.ID)
	if err := os.Mkdir(attemptDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	partialPath := filepath.Join(attemptDirectory, mediaAttemptSegmentName(1, fixture.attempt))
	partialContent := "external-root-drift-must-not-mutate"
	if err := os.WriteFile(partialPath, []byte(partialContent), 0o600); err != nil {
		t.Fatal(err)
	}
	rootID := registered.ID
	finalizer, err := newSQLiteSessionMediaFinalizer(context.Background(), sessionMediaFinalizerOptions{
		Repository: fixture.repository, Tools: fixture.tools, Root: externalRoot, RootID: &rootID,
		SessionID: fixture.session.ID, RelativePath: fixture.session.DataPath,
		StartedAt: fixture.session.StartedAt, Prober: fixture.prober,
		Dependencies: fixture.dependencies,
	})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := fixture.repository.LoadSnapshot(context.Background(), fixture.session.ID)
	if err != nil {
		t.Fatal(err)
	}

	markerPath := filepath.Join(externalRoot, recordingRootMarkerName)
	marker, err := readRecordingRootMarker(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	marker.RootID = uuid.Must(uuid.NewV7()).String()
	payload, err := encodeRecordingRootMarker(marker)
	if err != nil {
		t.Fatal(err)
	}
	driftRoot = func() {
		if err := os.WriteFile(markerPath, payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := finalizer.Finalize(context.Background(), []MediaAttempt{fixture.attempt}); !errors.Is(err, ErrMediaFinalize) {
		t.Fatalf("Finalize() error = %v, want ErrMediaFinalize", err)
	}
	if probeCalls.Load() != 2 {
		t.Fatalf("probe calls = %d, want both phases before the pre-publish recheck", probeCalls.Load())
	}
	content, err := os.ReadFile(partialPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != partialContent {
		t.Fatalf("partial content changed: %q", content)
	}
	finalPath := filepath.Join(sessionDirectory, "media", mediaFinalSegmentName(1, fixture.attempt.StartedAt))
	if _, err := os.Stat(finalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("final media path was mutated: %v", err)
	}
	after, err := fixture.repository.LoadSnapshot(context.Background(), fixture.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Session.ManifestRevision != baseline.Session.ManifestRevision ||
		len(after.Segments) != len(baseline.Segments) || len(after.Artifacts) != len(baseline.Artifacts) {
		t.Fatalf("snapshot changed after root drift: before=%#v after=%#v", baseline, after)
	}
}

func TestSQLiteSessionMediaFinalizerRejectsExternalRootDriftBeforeArtifactPublish(t *testing.T) {
	fixture := newMediaFinalizerFixture(t, writeSegmentProbeJSON(validSegmentProbeJSON))
	t.Cleanup(fixture.close)
	externalRoot := t.TempDir()
	registered, err := fixture.repository.RegisterRecordingRoot(context.Background(), externalRoot)
	if err != nil {
		t.Fatal(err)
	}
	sessionDirectory, err := secureMediaSessionDirectory(externalRoot, fixture.session.DataPath)
	if err != nil {
		t.Fatal(err)
	}
	attemptDirectory := filepath.Join(sessionDirectory, "media", ".attempt-"+fixture.attempt.ID)
	if err := os.Mkdir(attemptDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	partialPath := filepath.Join(attemptDirectory, mediaAttemptSegmentName(1, fixture.attempt))
	if err := os.WriteFile(partialPath, []byte("external-root-artifact-drift"), 0o600); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(externalRoot, recordingRootMarkerName)
	marker, err := readRecordingRootMarker(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	marker.RootID = uuid.Must(uuid.NewV7()).String()
	driftedMarker, err := encodeRecordingRootMarker(marker)
	if err != nil {
		t.Fatal(err)
	}
	var generatorCalls atomic.Int32
	var coreRevision atomic.Int64
	dependencies := fixture.dependencies
	dependencies.generateASR = func(
		ctx context.Context,
		_ string,
		source *mediaArtifactSource,
		target string,
		verify mediaArtifactVerifyFunc,
	) error {
		generatorCalls.Add(1)
		core, err := fixture.repository.LoadSnapshot(ctx, fixture.session.ID)
		if err != nil {
			return err
		}
		coreRevision.Store(core.Session.ManifestRevision)
		return writeTestMediaArtifact(
			ctx, source, target, []byte("must-not-publish"), verify,
			func() error { return os.WriteFile(markerPath, driftedMarker, 0o600) },
		)
	}
	dependencies.generatePlayback = failUnexpectedMediaGenerator(t)
	rootID := registered.ID
	finalizer, err := newSQLiteSessionMediaFinalizer(context.Background(), sessionMediaFinalizerOptions{
		Repository: fixture.repository, Tools: fixture.tools, Root: externalRoot, RootID: &rootID,
		SessionID: fixture.session.ID, RelativePath: fixture.session.DataPath,
		StartedAt: fixture.session.StartedAt, Prober: fixture.prober, Dependencies: dependencies,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := finalizer.Finalize(context.Background(), []MediaAttempt{fixture.attempt})
	if !errors.Is(err, ErrMediaFinalize) {
		t.Fatalf("Finalize() error = %v, want ErrMediaFinalize", err)
	}
	if generatorCalls.Load() != 1 || result.Snapshot.Session.State != SessionMediaFinalizing {
		t.Fatalf("unexpected drift result: calls=%d result=%#v", generatorCalls.Load(), result)
	}
	target := filepath.Join(sessionDirectory, "audio", "asr-000001.wav")
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact was published after bound root drift: %v", err)
	}
	temporaries, err := filepath.Glob(filepath.Join(sessionDirectory, "audio", ".test-artifact-*.partial"))
	if err != nil || len(temporaries) != 0 {
		t.Fatalf("temporary artifacts survived root drift: %v/%v", temporaries, err)
	}
	after, err := fixture.repository.LoadSnapshot(context.Background(), fixture.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if coreRevision.Load() == 0 || after.Session.ManifestRevision != coreRevision.Load() ||
		after.Session.State != SessionMediaFinalizing {
		t.Fatalf("database advanced after root drift: core=%d after=%#v", coreRevision.Load(), after.Session)
	}
	for _, artifact := range after.Artifacts {
		if artifact.Status != MediaArtifactPending {
			t.Fatalf("artifact advanced after root drift: %#v", artifact)
		}
	}
}

func TestMediaEpochAndCompletionIgnoreProxyFailures(t *testing.T) {
	start := int64(10_000)
	pts := int64(250)
	segments := []MediaSegment{{StartedAt: start, PTSStartMS: &pts, Status: MediaSegmentComplete}}
	if epoch := mediaEpochFromSegments(segments); epoch == nil || *epoch != 9_750 {
		t.Fatalf("media epoch = %v", epoch)
	}
	if state := completedMediaState(segments); state != SessionMediaCompleted {
		t.Fatalf("completed state = %s", state)
	}
	segments[0].Status = MediaSegmentCorrupt
	if state := completedMediaState(segments); state != SessionMediaIncomplete {
		t.Fatalf("corrupt state = %s", state)
	}
}

type mediaFinalizerFixture struct {
	repository       *SQLiteRepository
	close            func()
	root             string
	session          LiveSession
	sessionDirectory string
	tools            ffmpegTools
	prober           *ffprobeSegmentProber
	attempt          MediaAttempt
	partialPath      string
	partialContent   string
	dependencies     mediaFinalizerDependencies
}

func newMediaFinalizerFixture(t *testing.T, run segmentProbeRun) *mediaFinalizerFixture {
	t.Helper()
	repository, store, layout, roomID, now := openRepository(t)
	session, err := repository.Create(context.Background(), CreateSessionInput{
		RoomConfigID: roomID, OperationID: newV7(t), Recording: RecordingPending,
		StartedAt: now,
	})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	toolDirectory := t.TempDir()
	ffmpegPath := filepath.Join(toolDirectory, "ffmpeg-private.exe")
	ffprobePath := filepath.Join(toolDirectory, "ffprobe-private.exe")
	for _, path := range []string{ffmpegPath, ffprobePath} {
		if err := os.WriteFile(path, []byte("verified-tool"), 0o600); err != nil {
			store.Close()
			t.Fatal(err)
		}
	}
	prober, err := newFFprobeSegmentProberWithDependencies(
		ffmpegTools{ffprobePath: ffprobePath}, segmentProbeDependencies{run: run},
	)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	attempt := MediaAttempt{
		ID: uuid.Must(uuid.NewV7()).String(), Ordinal: 1,
		StartedAt: now.Add(time.Minute).UnixMilli(), SegmentSeconds: 600,
		Committed: true, Clean: true, VariantID: "origin",
		Protocol: "flv", QualityKey: "origin", Quality: "origin", Codec: "h264",
	}
	sessionDirectory, err := secureMediaSessionDirectory(layout.Root, session.DataPath)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	attemptDirectory := filepath.Join(sessionDirectory, "media", ".attempt-"+attempt.ID)
	if err := os.Mkdir(attemptDirectory, 0o700); err != nil {
		store.Close()
		t.Fatal(err)
	}
	partialContent := "immutable-matroska-fixture"
	partialPath := filepath.Join(attemptDirectory, mediaAttemptSegmentName(1, attempt))
	if err := os.WriteFile(partialPath, []byte(partialContent), 0o600); err != nil {
		store.Close()
		t.Fatal(err)
	}
	dependencies := defaultMediaFinalizerDependencies()
	dependencies.inspectArtifact = func(
		context.Context,
		string,
		string,
		MediaArtifactKind,
	) error {
		return nil
	}
	return &mediaFinalizerFixture{
		repository: repository, close: func() { _ = store.Close() }, root: layout.Root,
		session: session, sessionDirectory: sessionDirectory,
		tools: ffmpegTools{ffmpegPath: ffmpegPath, ffprobePath: ffprobePath}, prober: prober,
		attempt: attempt, partialPath: partialPath, partialContent: partialContent,
		dependencies: dependencies,
	}
}

func (fixture *mediaFinalizerFixture) open(t *testing.T) *sqliteSessionMediaFinalizer {
	t.Helper()
	t.Cleanup(fixture.close)
	finalizer, err := newSQLiteSessionMediaFinalizer(context.Background(), sessionMediaFinalizerOptions{
		Repository: fixture.repository, Tools: fixture.tools, Root: fixture.root,
		SessionID: fixture.session.ID, RelativePath: fixture.session.DataPath,
		StartedAt: fixture.session.StartedAt, Prober: fixture.prober,
		Dependencies: fixture.dependencies,
	})
	if err != nil {
		t.Fatal(err)
	}
	return finalizer
}

func writeTestMediaArtifact(
	ctx context.Context,
	source *mediaArtifactSource,
	target string,
	payload []byte,
	verify mediaArtifactVerifyFunc,
	beforeVerify func() error,
) error {
	if ctx == nil || source == nil || !validMediaAbsolutePath(target) || len(payload) == 0 || verify == nil {
		return ErrMediaArtifactFailed
	}
	if _, err := os.Lstat(target); err == nil {
		return ErrMediaFileConflict
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrMediaArtifactFailed
	}
	directory := filepath.Dir(target)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary := filepath.Join(directory, ".test-artifact-"+uuid.NewString()+filepath.Ext(target)+".partial")
	defer os.Remove(temporary)
	if _, err := io.Copy(io.Discard, source); err != nil {
		return err
	}
	if err := os.WriteFile(temporary, payload, 0o600); err != nil {
		return err
	}
	if beforeVerify != nil {
		if err := beforeVerify(); err != nil {
			return err
		}
	}
	if err := verify(ctx); err != nil {
		return err
	}
	if err := publishMediaFile(temporary, target); err != nil {
		return err
	}
	return verify(ctx)
}

func failUnexpectedMediaGenerator(t *testing.T) func(context.Context, string, *mediaArtifactSource, string, mediaArtifactVerifyFunc) error {
	t.Helper()
	return func(context.Context, string, *mediaArtifactSource, string, mediaArtifactVerifyFunc) error {
		t.Fatal("artifact generator was unexpectedly called")
		return ErrMediaArtifactFailed
	}
}

func containsMediaWarning(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func mediaInvalidObjectKinds() []string {
	kinds := []string{"zero", "directory", "symlink"}
	if runtime.GOOS == "windows" {
		kinds = append(kinds, "junction")
	}
	return kinds
}

func replaceMediaTestPathWithInvalidObject(t *testing.T, path, objectKind string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	switch objectKind {
	case "zero":
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	case "directory":
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	case "symlink":
		target := path + ".symlink-target"
		if err := os.WriteFile(target, []byte("attacker"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
	case "junction":
		if runtime.GOOS != "windows" {
			t.Skip("junction is Windows-only")
		}
		target := path + ".junction-target"
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		command := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", path, target)
		if output, err := command.CombinedOutput(); err != nil {
			t.Skipf("junction unavailable: %v (%s)", err, strings.TrimSpace(string(output)))
		}
	default:
		t.Fatalf("unknown invalid media object kind %q", objectKind)
	}
}

func assertMediaTestInvalidObjectPreserved(t *testing.T, path, objectKind string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("invalid media object was removed: %v", err)
	}
	switch objectKind {
	case "zero":
		if !info.Mode().IsRegular() || info.Size() != 0 {
			t.Fatalf("zero object changed: mode=%v size=%d", info.Mode(), info.Size())
		}
	case "directory":
		if !info.IsDir() {
			t.Fatalf("directory object changed: mode=%v", info.Mode())
		}
	case "symlink", "junction":
		reparse, reparseErr := mediaPathIsReparsePoint(path, info)
		if reparseErr != nil || !reparse {
			t.Fatalf("reparse object changed: mode=%v reparse=%v err=%v", info.Mode(), reparse, reparseErr)
		}
	default:
		t.Fatalf("unknown invalid media object kind %q", objectKind)
	}
}

func assertMediaFinalizerPrivateFormatting(
	t *testing.T,
	finalizer *sqliteSessionMediaFinalizer,
	fixture *mediaFinalizerFixture,
) {
	t.Helper()
	rendered := fmt.Sprintf("%v %#v %v", finalizer, finalizer, sessionMediaFinalizerOptions{
		Root: fixture.root, Tools: fixture.tools, SessionID: fixture.session.ID,
	})
	for _, private := range []string{fixture.root, fixture.tools.ffmpegPath, fixture.session.ID} {
		if strings.Contains(rendered, private) {
			t.Fatalf("finalizer formatting exposed private value: %s", rendered)
		}
	}
	if !strings.Contains(rendered, "redacted") {
		t.Fatalf("finalizer formatting omitted redaction: %s", rendered)
	}
}
