package capture

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestEncodeMediaManifestIsDeterministicAndPrivacySafe(t *testing.T) {
	sessionID := uuid.Must(uuid.NewV7()).String()
	attemptID := uuid.Must(uuid.NewV7()).String()
	segmentID := uuid.Must(uuid.NewV7()).String()
	artifactID := uuid.Must(uuid.NewV7()).String()
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC).UnixMilli()
	ptsStart := int64(250)
	ptsEnd := int64(10_250)
	snapshot := MediaSnapshot{
		Session: SessionMedia{
			SessionID: sessionID, RelativePath: "rooms/test/sessions/2026/07/" + sessionID,
			State: SessionMediaCompleted, ManifestRevision: 2, CreatedAt: now, UpdatedAt: now + 1,
			Attempts: []MediaAttempt{{
				ID: attemptID, Ordinal: 1, StartedAt: now, SegmentSeconds: 600,
				Committed: true, Clean: true, Protocol: "flv", Codec: "h264",
			}},
		},
		Segments: []MediaSegment{{
			ID: segmentID, Sequence: 1,
			RelativePath: "rooms/test/sessions/2026/07/" + sessionID + "/media/segment-000001.mkv",
			Container:    "mkv", VideoCodec: "h264", AudioCodec: "aac",
			StartedAt: now, EndedAt: now + 10_000, PTSStartMS: &ptsStart, PTSEndMS: &ptsEnd,
			DurationMS: 10_000, SizeBytes: 42, SHA256: strings.Repeat("a", 64),
			Status: MediaSegmentComplete, AttemptID: attemptID, AttemptSequence: 1,
			SourceRelativePath: "rooms/test/sessions/2026/07/" + sessionID + "/media/.attempt-" + attemptID + "/segment.mkv.partial",
			ProbeVersion:       "ffprobe_v1",
		}},
		Artifacts: []MediaArtifact{{
			ID: artifactID, MediaSegmentID: segmentID, Kind: MediaArtifactASRWAV,
			RelativePath: "rooms/test/sessions/2026/07/" + sessionID + "/audio/asr-000001.wav",
			Container:    "wav", Codec: "pcm_s16le", DurationMS: 10_000, SizeBytes: 10,
			SampleRate: 16_000, Channels: 1, SHA256: strings.Repeat("b", 64),
			SourceSHA256: strings.Repeat("a", 64), Status: MediaArtifactComplete,
			CreatedAt: now, UpdatedAt: now + 1,
		}},
	}
	first, err := encodeMediaManifest(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	second, err := encodeMediaManifest(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) || first[len(first)-1] != '\n' {
		t.Fatal("media manifest is not deterministic newline-terminated JSON")
	}
	if !json.Valid(bytes.TrimSpace(first)) {
		t.Fatalf("invalid manifest JSON: %s", first)
	}
	for _, private := range []string{"https://", "http://", `C:\\`, "stream-token", "cookie"} {
		if bytes.Contains(bytes.ToLower(first), bytes.ToLower([]byte(private))) {
			t.Fatalf("manifest exposed private value %q", private)
		}
	}
	if !bytes.Contains(first, []byte(`"schemaVersion": 1`)) ||
		!bytes.Contains(first, []byte(`"revision": 2`)) {
		t.Fatalf("manifest omitted required versioning: %s", first)
	}
}

func TestEncodeMediaManifestRejectsInvalidPathsAndAttempts(t *testing.T) {
	sessionID := uuid.Must(uuid.NewV7()).String()
	snapshot := MediaSnapshot{Session: SessionMedia{
		SessionID: sessionID, RelativePath: "../escape", State: SessionMediaOpen,
		CreatedAt: 1, UpdatedAt: 1,
	}}
	if _, err := encodeMediaManifest(snapshot); err == nil {
		t.Fatal("expected escaping path to be rejected")
	}
	snapshot.Session.RelativePath = "rooms/safe"
	snapshot.Session.Attempts = []MediaAttempt{{
		ID: uuid.Must(uuid.NewV7()).String(), Ordinal: 1, StartedAt: 1,
		SegmentSeconds: 600, Protocol: "flv", Codec: "h264", Quality: "https://secret.invalid",
	}}
	if _, err := encodeMediaManifest(snapshot); err == nil {
		t.Fatal("expected URL-bearing attempt metadata to be rejected")
	}
}

func TestMediaAttemptsEmptyArrayHasCanonicalRoundTrip(t *testing.T) {
	normalized, encoded, err := normalizeMediaAttempts(nil)
	if err != nil || normalized == nil || string(encoded) != "[]" {
		t.Fatalf("normalize empty attempts = %#v/%q/%v", normalized, encoded, err)
	}
	decoded, err := decodeMediaAttempts("[]")
	if err != nil || decoded == nil || len(decoded) != 0 {
		t.Fatalf("decode empty attempts = %#v/%v", decoded, err)
	}
}

func TestNormalizeMediaAttemptsAcceptsMaximumLegalPayloadAndRejectsOverLimit(t *testing.T) {
	attempts := maximumLegalMediaAttempts(time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC).UnixMilli())
	normalized, payload, err := normalizeMediaAttempts(attempts)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized) != maximumMediaAttempts || len(payload) <= 64<<10 || len(payload) > maxMediaAttemptsJSONBytes {
		t.Fatalf("maximum attempts normalized to count=%d bytes=%d ceiling=%d",
			len(normalized), len(payload), maxMediaAttemptsJSONBytes)
	}
	decoded, err := decodeMediaAttempts(string(payload))
	if err != nil || len(decoded) != maximumMediaAttempts || decoded[0].ID != normalized[0].ID ||
		decoded[len(decoded)-1].ID != normalized[len(normalized)-1].ID {
		t.Fatalf("maximum attempts round trip failed: count=%d error=%v", len(decoded), err)
	}
	overLimit := append([]MediaAttempt(nil), attempts...)
	overLimit = append(overLimit, MediaAttempt{
		ID: uuid.Must(uuid.NewV7()).String(), Ordinal: maximumMediaAttempts + 1,
		StartedAt: attempts[len(attempts)-1].StartedAt + 1, SegmentSeconds: 300,
		Protocol: "flv", Codec: "h264",
	})
	if _, _, err := normalizeMediaAttempts(overLimit); !errors.Is(err, ErrMediaContractInvalid) {
		t.Fatalf("over-limit attempts error = %v, want ErrMediaContractInvalid", err)
	}
}

func TestMediaManifestMatchesRejectsSymlinkAndOversize(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "media.json")
	payload := []byte("{}\n")
	if err := writeMediaFileAtomic(target, payload); err != nil {
		t.Fatal(err)
	}
	if !mediaManifestMatches(target, payload) {
		t.Fatal("expected byte-exact manifest match")
	}
	if mediaManifestMatches(target, []byte("different")) {
		t.Fatal("different manifest unexpectedly matched")
	}
}

func TestEncodeMediaManifestAcceptsMaximumLegalCardinalityAndRejectsOverLimit(t *testing.T) {
	now := time.Date(2026, 7, 19, 13, 0, 0, 0, time.UTC).UnixMilli()
	base := MediaSnapshot{Session: SessionMedia{
		SessionID: uuid.Must(uuid.NewV7()).String(), RelativePath: "rooms/safe",
		State: SessionMediaOpen, CreatedAt: now, UpdatedAt: now,
	}}
	overLimit := base
	overLimit.Segments = make([]MediaSegment, maximumMediaSegments+1)
	if _, err := encodeMediaManifest(overLimit); !errors.Is(err, ErrMediaManifest) {
		t.Fatalf("over-limit segments error = %v, want ErrMediaManifest", err)
	}
	overLimit.Segments = nil
	overLimit.Artifacts = make([]MediaArtifact, maximumMediaArtifacts+1)
	if _, err := encodeMediaManifest(overLimit); !errors.Is(err, ErrMediaManifest) {
		t.Fatalf("over-limit artifacts error = %v, want ErrMediaManifest", err)
	}

	attempts := maximumLegalMediaAttempts(now)
	maxToken := strings.Repeat("<", maxMediaSafeTokenBytes)
	snapshot := MediaSnapshot{Session: SessionMedia{
		SessionID:    base.Session.SessionID,
		RelativePath: maximumLegalMediaPath("rooms/sessions/", "-session"),
		State:        SessionMediaCompleted, ManifestRevision: 1,
		Attempts: attempts, CreatedAt: now, UpdatedAt: now + 1,
	}}
	snapshot.Segments = make([]MediaSegment, maximumMediaSegments)
	snapshot.Artifacts = make([]MediaArtifact, 0, maximumMediaArtifacts)
	for index := 0; index < maximumMediaSegments; index++ {
		sequence := index + 1
		segmentID := uuid.Must(uuid.NewV7()).String()
		startedAt := now + int64(index)
		snapshot.Segments[index] = MediaSegment{
			ID: segmentID, Sequence: sequence,
			RelativePath: maximumLegalMediaPath("rooms/segments/", fmt.Sprintf("-%06d.mkv", sequence)),
			SourceRelativePath: maximumLegalMediaPath(
				"rooms/sources/", fmt.Sprintf("-%06d.mkv.partial", sequence),
			),
			Container: maxToken, VideoCodec: maxToken, AudioCodec: maxToken,
			StartedAt: startedAt, EndedAt: startedAt + 1, DurationMS: 1, SizeBytes: 1,
			SHA256: strings.Repeat("a", 64), Status: MediaSegmentComplete,
			AttemptID: attempts[index%len(attempts)].ID, AttemptSequence: sequence,
			ProbeVersion: maxToken,
		}
		for kindIndex, kind := range []MediaArtifactKind{MediaArtifactASRWAV, MediaArtifactPlaybackMP4} {
			artifactSequence := index*2 + kindIndex + 1
			snapshot.Artifacts = append(snapshot.Artifacts, MediaArtifact{
				ID: uuid.Must(uuid.NewV7()).String(), MediaSegmentID: segmentID, Kind: kind,
				RelativePath: maximumLegalMediaPath(
					"rooms/artifacts/", fmt.Sprintf("-%06d-%d.bin", sequence, kindIndex),
				),
				Container: maxToken, Codec: maxToken, DurationMS: 1, SizeBytes: int64(artifactSequence),
				SHA256: strings.Repeat("b", 64), SourceSHA256: strings.Repeat("a", 64),
				Status: MediaArtifactComplete, CreatedAt: now, UpdatedAt: now + 1,
			})
		}
	}
	payload, err := encodeMediaManifest(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) <= 32<<20 || len(payload) > mediaManifestPayloadMax {
		t.Fatalf("maximum manifest bytes = %d, ceiling = %d", len(payload), mediaManifestPayloadMax)
	}
	if payload[len(payload)-1] != '\n' || !json.Valid(bytes.TrimSpace(payload)) {
		t.Fatal("maximum manifest is not newline-terminated valid JSON")
	}
}

func maximumLegalMediaAttempts(startedAt int64) []MediaAttempt {
	maxToken := strings.Repeat("<", maxMediaSafeTokenBytes)
	attempts := make([]MediaAttempt, maximumMediaAttempts)
	for index := range attempts {
		attempts[index] = MediaAttempt{
			ID: uuid.Must(uuid.NewV7()).String(), Ordinal: index + 1,
			StartedAt: startedAt + int64(index), SegmentSeconds: 300,
			Committed: true, Clean: true, VariantID: maxToken,
			Protocol: "flv", QualityKey: maxToken, Quality: maxToken, Codec: "h264",
		}
	}
	return attempts
}

func maximumLegalMediaPath(prefix, suffix string) string {
	remaining := maxMediaRelativePathBytes - len(prefix) - len(suffix)
	return prefix + strings.Repeat("x", remaining) + suffix
}
