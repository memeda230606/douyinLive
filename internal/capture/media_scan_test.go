package capture

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDiscoverMediaCandidatesOrdersAttemptsAndSegments(t *testing.T) {
	sessionDirectory, sessionRelative := newMediaScanSession(t)
	first := newMediaScanAttempt(t, 2, time.Date(2026, 7, 17, 12, 10, 0, 123_000_000, time.UTC))
	second := newMediaScanAttempt(t, 1, time.Date(2026, 7, 17, 12, 0, 0, 456_000_000, time.UTC))
	writeMediaScanPartial(t, sessionDirectory, first, 2, "first-two")
	writeMediaScanPartial(t, sessionDirectory, first, 1, "first-one")
	writeMediaScanPartial(t, sessionDirectory, second, 1, "second-one")

	candidates, err := discoverMediaCandidates(sessionDirectory, sessionRelative, []MediaAttempt{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 3 {
		t.Fatalf("candidate count = %d", len(candidates))
	}
	for index, candidate := range candidates {
		if candidate.Sequence != index+1 {
			t.Fatalf("candidate %d sequence = %d", index, candidate.Sequence)
		}
	}
	if candidates[0].Attempt.ID != second.ID || candidates[0].AttemptSequence != 1 ||
		candidates[1].Attempt.ID != first.ID || candidates[1].AttemptSequence != 1 ||
		candidates[2].Attempt.ID != first.ID || candidates[2].AttemptSequence != 2 {
		t.Fatalf("unexpected ordering: %#v", candidates)
	}
}

func TestDiscoverMediaCandidatesCoalescesPublishedSlot(t *testing.T) {
	sessionDirectory, sessionRelative := newMediaScanSession(t)
	attempt := newMediaScanAttempt(t, 1, time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC))
	partial := writeMediaScanPartial(t, sessionDirectory, attempt, 1, "same-media")
	final := filepath.Join(sessionDirectory, "media", mediaFinalSegmentName(1, attempt.StartedAt))
	if err := os.WriteFile(final, []byte("same-media"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidates, err := discoverMediaCandidates(sessionDirectory, sessionRelative, []MediaAttempt{attempt})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || !candidates[0].AlreadyFinal ||
		candidates[0].PartialPath != partial || candidates[0].FinalPath != final {
		t.Fatalf("published slot was not coalesced: %#v", candidates)
	}
}

func TestDiscoverMediaCandidatesRejectsAmbiguousFinal(t *testing.T) {
	sessionDirectory, sessionRelative := newMediaScanSession(t)
	started := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	first := newMediaScanAttempt(t, 1, started)
	second := newMediaScanAttempt(t, 2, started)
	final := filepath.Join(sessionDirectory, "media", mediaFinalSegmentName(1, started.UnixMilli()))
	if err := os.WriteFile(final, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Ambiguous finals are intentionally left for P3-RCV rather than guessed.
	candidates, err := discoverMediaCandidates(sessionDirectory, sessionRelative, []MediaAttempt{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("ambiguous final was guessed: %#v", candidates)
	}
}

func TestMediaScanFilenameParsingIsStrict(t *testing.T) {
	attempt := newMediaScanAttempt(t, 1, time.Date(2026, 7, 17, 12, 0, 0, 123_000_000, time.UTC))
	valid := mediaAttemptSegmentName(42, attempt)
	if sequence, ok := parseMediaAttemptSegmentName(valid, attempt); !ok || sequence != 42 {
		t.Fatalf("valid attempt name rejected: %q", valid)
	}
	for _, name := range []string{
		"segment-000000-" + strings.TrimPrefix(valid, "segment-000042-"),
		strings.Replace(valid, ".mkv.partial", ".mp4.partial", 1),
		strings.Replace(valid, attempt.ID, uuid.Must(uuid.NewV7()).String(), 1),
		strings.Replace(valid, "123000000Z", "124000000Z", 1),
	} {
		if _, ok := parseMediaAttemptSegmentName(name, attempt); ok {
			t.Fatalf("invalid attempt name accepted: %q", name)
		}
	}
	final := mediaFinalSegmentName(42, attempt.StartedAt)
	if sequence, startedAt, ok := parseMediaFinalSegmentName(final); !ok || sequence != 42 || startedAt != attempt.StartedAt {
		t.Fatalf("valid final name rejected: %q", final)
	}
}

func TestReadMediaDirectoryNamesIsBounded(t *testing.T) {
	directory := t.TempDir()
	for index := 0; index < 3; index++ {
		if err := os.WriteFile(filepath.Join(directory, fmt.Sprintf("file-%d", index)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := readMediaDirectoryNames(directory, 2); !errors.Is(err, ErrMediaScanLimit) {
		t.Fatalf("limit error = %v", err)
	}
}

func TestMediaCandidateFormattingRedactsPaths(t *testing.T) {
	candidate := mediaCandidate{
		Sequence: 1, AttemptSequence: 2,
		PartialPath: `C:\private\token.mkv.partial`, FinalPath: `C:\private\token.mkv`,
	}
	rendered := fmt.Sprintf("%v %#v", candidate, candidate)
	if strings.Contains(rendered, "private") || strings.Contains(rendered, "token") || !strings.Contains(rendered, "redacted") {
		t.Fatalf("unsafe candidate formatting: %s", rendered)
	}
}

func newMediaScanSession(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	relative := "rooms/test/sessions/2026/07/session"
	directory, err := secureMediaSessionDirectory(root, relative)
	if err != nil {
		t.Fatal(err)
	}
	return directory, relative
}

func newMediaScanAttempt(t *testing.T, ordinal int, startedAt time.Time) MediaAttempt {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	return MediaAttempt{
		ID: id.String(), Ordinal: ordinal, StartedAt: startedAt.UTC().UnixMilli(),
		SegmentSeconds: 600, Committed: true, Clean: true,
		Protocol: "flv", QualityKey: "origin", Quality: "origin", Codec: "h264",
	}
}

func writeMediaScanPartial(t *testing.T, sessionDirectory string, attempt MediaAttempt, sequence int, content string) string {
	t.Helper()
	directory := filepath.Join(sessionDirectory, "media", ".attempt-"+attempt.ID)
	if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	path := filepath.Join(directory, mediaAttemptSegmentName(sequence, attempt))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
