package playback

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestServiceOpenMediaRevalidatesIdentitySizeAndDigest(t *testing.T) {
	fixture := newPlaybackFixture(t)
	defer fixture.close()
	sessionID := fixture.sessionIDs[0]
	segmentID := insertPlaybackMedia(t, fixture, sessionID)[0]

	var artifactID string
	if err := fixture.writer.QueryRow(
		`SELECT id FROM media_artifacts WHERE media_segment_id = ? AND kind = 'playback_mp4'`,
		segmentID,
	).Scan(&artifactID); err != nil {
		t.Fatalf("load artifact id: %v", err)
	}
	dataRoot := t.TempDir()
	relativePath := "rooms/019aa000-0000-7000-8000-000000000001/sessions/" + sessionID + "/media/playback.mp4"
	absolutePath := filepath.Join(dataRoot, filepath.FromSlash(relativePath))
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o700); err != nil {
		t.Fatalf("create media directory: %v", err)
	}
	payload := []byte("verified-playback-media")
	if err := os.WriteFile(absolutePath, payload, 0o600); err != nil {
		t.Fatalf("write media file: %v", err)
	}
	digest := sha256.Sum256(payload)
	digestText := hex.EncodeToString(digest[:])
	if _, err := fixture.writer.Exec(`INSERT INTO session_media(
		session_id, root_id, relative_path, state, manifest_revision,
		manifest_dirty, media_epoch_at, attempts_json, created_at, updated_at
	) VALUES (?, NULL, ?, 'completed', 1, 0, 1000, '[]', 1, 1)`,
		sessionID, filepath.ToSlash(filepath.Dir(filepath.Dir(relativePath))),
	); err != nil {
		t.Fatalf("insert session media: %v", err)
	}
	if _, err := fixture.writer.Exec(
		`UPDATE media_segments SET size_bytes = ?, sha256 = ?, status = 'complete' WHERE id = ?`,
		len(payload), digestText, segmentID,
	); err != nil {
		t.Fatalf("update media segment evidence: %v", err)
	}
	if _, err := fixture.writer.Exec(`UPDATE media_artifacts SET
		relative_path = ?, size_bytes = ?, sha256 = ?, source_sha256 = ?,
		status = 'complete', container = 'mp4', codec = 'h264'
		WHERE id = ?`, relativePath, len(payload), digestText, digestText, artifactID); err != nil {
		t.Fatalf("update media artifact evidence: %v", err)
	}

	service, err := NewServiceWithOptions(fixture.writer, ServiceOptions{DataRoot: dataRoot})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	opened, err := service.OpenMedia(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("OpenMedia() error = %v", err)
	}
	read, readErr := io.ReadAll(opened)
	closeErr := opened.Close()
	if readErr != nil || closeErr != nil || string(read) != string(payload) {
		t.Fatalf("opened media = %q, readErr=%v closeErr=%v", read, readErr, closeErr)
	}
	if opened.Size() != int64(len(payload)) || opened.ContentType() != "video/mp4" {
		t.Fatalf("opened metadata = size %d type %q", opened.Size(), opened.ContentType())
	}

	tampered := append([]byte(nil), payload...)
	tampered[0] ^= 0xff
	if err := os.WriteFile(absolutePath, tampered, 0o600); err != nil {
		t.Fatalf("tamper media file: %v", err)
	}
	if _, err := service.OpenMedia(context.Background(), artifactID); !errors.Is(err, ErrMediaUnavailable) {
		t.Fatalf("tampered media error = %v", err)
	}

	if _, err := fixture.writer.Exec(
		`UPDATE media_artifacts SET relative_path = '../outside.mp4' WHERE id = ?`, artifactID,
	); err != nil {
		t.Fatalf("inject traversal path: %v", err)
	}
	if _, err := service.OpenMedia(context.Background(), artifactID); !errors.Is(err, ErrMediaUnavailable) {
		t.Fatalf("traversal media error = %v", err)
	}
}

func TestServiceOpenMediaRejectsUnknownAndNonPlaybackArtifacts(t *testing.T) {
	fixture := newPlaybackFixture(t)
	defer fixture.close()
	sessionID := fixture.sessionIDs[0]
	segmentID := insertPlaybackMedia(t, fixture, sessionID)[0]
	if _, err := fixture.writer.Exec(`INSERT INTO session_media(
		session_id, relative_path, state, manifest_revision, manifest_dirty,
		attempts_json, created_at, updated_at
	) VALUES (?, ?, 'completed', 1, 0, '[]', 1, 1)`, sessionID, "rooms/session"); err != nil {
		t.Fatalf("insert session media: %v", err)
	}
	service, err := NewServiceWithOptions(fixture.writer, ServiceOptions{DataRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	if _, err := service.OpenMedia(context.Background(), "invalid"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalid artifact id error = %v", err)
	}
	if _, err := service.OpenMedia(context.Background(), newUUIDv7(t)); !errors.Is(err, ErrMediaNotFound) {
		t.Fatalf("unknown artifact error = %v", err)
	}
	var asrID string
	if err := fixture.writer.QueryRow(
		`SELECT id FROM media_artifacts WHERE media_segment_id = ? AND kind = 'asr_wav'`, segmentID,
	).Scan(&asrID); err != nil {
		t.Fatalf("load ASR artifact: %v", err)
	}
	if _, err := service.OpenMedia(context.Background(), asrID); !errors.Is(err, ErrMediaUnavailable) {
		t.Fatalf("ASR artifact error = %v", err)
	}
}
