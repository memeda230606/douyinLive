package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
)

var ErrMediaManifest = errors.New("MEDIA_MANIFEST_FAILED")

const (
	mediaManifestSchemaVersion = 1
	mediaManifestCASAttempts   = 3
)

type mediaManifestSession struct {
	ID              string            `json:"id"`
	RecordingRootID *string           `json:"recordingRootId,omitempty"`
	RelativePath    string            `json:"relativePath"`
	State           SessionMediaState `json:"state"`
	Revision        int64             `json:"revision"`
	MediaEpochAt    *int64            `json:"mediaEpochAt,omitempty"`
	CreatedAt       int64             `json:"createdAt"`
	UpdatedAt       int64             `json:"updatedAt"`
}

type mediaManifest struct {
	SchemaVersion int                  `json:"schemaVersion"`
	Session       mediaManifestSession `json:"session"`
	Attempts      []MediaAttempt       `json:"attempts"`
	Segments      []MediaSegment       `json:"segments"`
	Artifacts     []MediaArtifact      `json:"artifacts"`
}

func encodeMediaManifest(snapshot MediaSnapshot) ([]byte, error) {
	if len(snapshot.Segments) > maximumMediaSegments || len(snapshot.Artifacts) > maximumMediaArtifacts {
		return nil, ErrMediaManifest
	}
	if validateUUIDv7("media manifest session", snapshot.Session.SessionID) != nil ||
		!validMediaRelativePath(snapshot.Session.RelativePath) ||
		!validSessionMediaState(snapshot.Session.State) || snapshot.Session.ManifestRevision < 0 ||
		snapshot.Session.CreatedAt < 0 || snapshot.Session.UpdatedAt < snapshot.Session.CreatedAt ||
		(snapshot.Session.MediaEpochAt != nil && *snapshot.Session.MediaEpochAt < 0) {
		return nil, ErrMediaManifest
	}
	if snapshot.Session.RootID != nil && validateUUIDv7("media manifest root", *snapshot.Session.RootID) != nil {
		return nil, ErrMediaManifest
	}
	attempts, _, err := normalizeMediaAttempts(snapshot.Session.Attempts)
	if err != nil {
		return nil, ErrMediaManifest
	}
	segments := append([]MediaSegment(nil), snapshot.Segments...)
	sort.Slice(segments, func(left, right int) bool {
		if segments[left].Sequence == segments[right].Sequence {
			return segments[left].ID < segments[right].ID
		}
		return segments[left].Sequence < segments[right].Sequence
	})
	for _, segment := range segments {
		if err := validateMediaSegment(segment); err != nil {
			return nil, ErrMediaManifest
		}
	}
	artifacts := append([]MediaArtifact(nil), snapshot.Artifacts...)
	sort.Slice(artifacts, func(left, right int) bool {
		if artifacts[left].MediaSegmentID == artifacts[right].MediaSegmentID {
			if artifacts[left].Kind == artifacts[right].Kind {
				return artifacts[left].ID < artifacts[right].ID
			}
			return artifacts[left].Kind < artifacts[right].Kind
		}
		return artifacts[left].MediaSegmentID < artifacts[right].MediaSegmentID
	})
	for _, artifact := range artifacts {
		if err := validateMediaArtifact(artifact); err != nil {
			return nil, ErrMediaManifest
		}
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(mediaManifest{
		SchemaVersion: mediaManifestSchemaVersion,
		Session: mediaManifestSession{
			ID: snapshot.Session.SessionID, RecordingRootID: snapshot.Session.RootID,
			RelativePath: snapshot.Session.RelativePath, State: snapshot.Session.State,
			Revision: snapshot.Session.ManifestRevision, MediaEpochAt: snapshot.Session.MediaEpochAt,
			CreatedAt: snapshot.Session.CreatedAt, UpdatedAt: snapshot.Session.UpdatedAt,
		},
		Attempts: attempts, Segments: segments, Artifacts: artifacts,
	})
	if err != nil || buffer.Len() == 0 || buffer.Len() > mediaManifestPayloadMax {
		return nil, ErrMediaManifest
	}
	return append([]byte(nil), buffer.Bytes()...), nil
}

func (r *SQLiteRepository) materializeMediaManifest(
	ctx context.Context,
	root, sessionID string,
) (MediaSnapshot, error) {
	if r == nil || ctx == nil || validateUUIDv7("media manifest session", sessionID) != nil {
		return MediaSnapshot{}, ErrMediaManifest
	}
	if err := ctx.Err(); err != nil {
		return MediaSnapshot{}, err
	}
	unlock := r.lockManifestSession(sessionID)
	defer unlock()

	for attempt := 0; attempt < mediaManifestCASAttempts; attempt++ {
		snapshot, err := r.LoadSnapshot(ctx, sessionID)
		if err != nil {
			return MediaSnapshot{}, err
		}
		canonicalRoot, err := r.resolveSessionMediaRoot(ctx, root, snapshot.Session.RootID)
		if err != nil {
			return snapshot, ErrMediaManifest
		}
		sessionDirectory, err := secureMediaSessionDirectory(canonicalRoot, snapshot.Session.RelativePath)
		if err != nil {
			return snapshot, ErrMediaManifest
		}
		payload, err := encodeMediaManifest(snapshot)
		if err != nil {
			return snapshot, err
		}
		manifestPath := filepath.Join(sessionDirectory, "manifests", "media.json")
		matches := mediaManifestMatches(manifestPath, payload)
		if !matches {
			if err := writeMediaFileAtomic(manifestPath, payload); err != nil {
				return snapshot, ErrMediaManifest
			}
		}
		if !snapshot.Session.ManifestDirty {
			return snapshot, nil
		}
		cleared, err := r.ClearDirty(ctx, sessionID, snapshot.Session.ManifestRevision)
		if err != nil {
			return snapshot, err
		}
		if cleared {
			snapshot.Session.ManifestDirty = false
			return snapshot, nil
		}
	}
	return MediaSnapshot{}, ErrMediaSnapshotConflict
}

func mediaManifestMatches(path string, expected []byte) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Size() <= 0 || info.Size() > mediaManifestPayloadMax {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	actual, readErr := io.ReadAll(io.LimitReader(file, mediaManifestPayloadMax+1))
	closeErr := file.Close()
	return readErr == nil && closeErr == nil && bytes.Equal(actual, expected)
}
