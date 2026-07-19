package capture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"

	"github.com/google/uuid"
)

const (
	recordingRootMarkerName       = ".douyinlive-recording-root.json"
	recordingRootMarkerVersion    = 1
	recordingRootMarkerMaxBytes   = 4096
	recordingRootDigestHexLength  = sha256.Size * 2
	recordingRootStatusReady      = RecordingRootStatus("ready")
	recordingRootWriteProbePrefix = ".douyinlive-write-probe-"
)

var (
	ErrRecordingRootInvalid     = errors.New("recording root is invalid")
	ErrRecordingRootUnavailable = errors.New("recording root is unavailable")
	ErrRecordingRootConflict    = errors.New("recording root identity conflicts with durable state")
	ErrRecordingRootPersistence = errors.New("recording root persistence failed")

	recordingRootMarkerMu sync.Mutex
)

type RecordingRootStatus string

const RecordingRootReady RecordingRootStatus = recordingRootStatusReady

// RecordingRoot deliberately exposes only a durable identifier and health
// status. Its filesystem and volume identities must never cross a DTO, log, or
// generic formatting boundary.
type RecordingRoot struct {
	ID     string              `json:"id"`
	Status RecordingRootStatus `json:"status"`

	absolutePath   string
	canonicalKey   string
	volumeIdentity string
}

func (root RecordingRoot) String() string {
	return fmt.Sprintf("RecordingRoot{id:%q,status:%q}", root.ID, root.Status)
}

func (root RecordingRoot) GoString() string { return root.String() }

func (root RecordingRoot) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID     string              `json:"id"`
		Status RecordingRootStatus `json:"status"`
	}{ID: root.ID, Status: root.Status})
}

func (root RecordingRoot) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("id", root.ID),
		slog.String("status", string(root.Status)),
	)
}

type recordingRootMarker struct {
	Version        int
	RootID         string
	VolumeIdentity string
}

type recordingRootRow struct {
	id             string
	absolutePath   string
	canonicalKey   string
	volumeIdentity string
	status         RecordingRootStatus
	createdAt      int64
	updatedAt      int64
	lastVerifiedAt sql.NullInt64
}

// RegisterRecordingRoot binds an existing directory, its canonical path, its
// volume, an on-disk marker, and a SQLite row into one fail-closed identity.
// The marker is committed before SQLite so interruption is safely retryable.
func (r *SQLiteRepository) RegisterRecordingRoot(ctx context.Context, path string) (RecordingRoot, error) {
	if err := requireContext(ctx); err != nil {
		return RecordingRoot{}, err
	}
	if r == nil || r.writer == nil || r.now == nil {
		return RecordingRoot{}, ErrRecordingRootPersistence
	}

	absolutePath, canonicalKey, volumeIdentity, err := inspectRecordingRoot(path)
	if err != nil {
		return RecordingRoot{}, err
	}
	if err := verifyRecordingRootWritable(absolutePath); err != nil {
		return RecordingRoot{}, err
	}
	if err := ctx.Err(); err != nil {
		return RecordingRoot{}, err
	}

	marker, err := loadOrCreateRecordingRootMarkerLocked(absolutePath, volumeIdentity)
	if err != nil {
		return RecordingRoot{}, err
	}
	verifiedPath, verifiedKey, verifiedVolume, err := inspectRecordingRoot(absolutePath)
	if err != nil {
		return RecordingRoot{}, err
	}
	if verifiedKey != canonicalKey || verifiedVolume != volumeIdentity ||
		marker.VolumeIdentity != verifiedVolume {
		return RecordingRoot{}, ErrRecordingRootConflict
	}
	absolutePath = verifiedPath
	if marker.VolumeIdentity != volumeIdentity {
		return RecordingRoot{}, ErrRecordingRootConflict
	}

	root := RecordingRoot{
		ID: marker.RootID, Status: RecordingRootReady,
		absolutePath: absolutePath, canonicalKey: canonicalKey, volumeIdentity: volumeIdentity,
	}
	if err := r.persistRecordingRoot(ctx, root); err != nil {
		return RecordingRoot{}, err
	}
	return root, nil
}

// verifyRecordingRootBinding validates an existing marker and database row
// without creating either. Callers that already persist a root ID must fail
// closed instead of accidentally registering a different supplied directory.
func (r *SQLiteRepository) verifyRecordingRootBinding(
	ctx context.Context,
	path string,
	rootID string,
) (RecordingRoot, error) {
	if err := requireContext(ctx); err != nil {
		return RecordingRoot{}, err
	}
	if r == nil || r.reader == nil || validateUUIDv7("recording root binding", rootID) != nil {
		return RecordingRoot{}, ErrRecordingRootConflict
	}
	absolutePath, canonicalKey, volumeIdentity, err := inspectRecordingRoot(path)
	if err != nil {
		return RecordingRoot{}, err
	}
	if err := verifyRecordingRootWritable(absolutePath); err != nil {
		return RecordingRoot{}, err
	}
	marker, err := readRecordingRootMarker(filepath.Join(absolutePath, recordingRootMarkerName))
	if errors.Is(err, fs.ErrNotExist) {
		return RecordingRoot{}, ErrRecordingRootConflict
	}
	if err != nil {
		return RecordingRoot{}, err
	}
	if marker.RootID != rootID || marker.VolumeIdentity != volumeIdentity {
		return RecordingRoot{}, ErrRecordingRootConflict
	}
	row, err := queryRecordingRootRow(ctx, r.reader, rootID)
	if errors.Is(err, sql.ErrNoRows) {
		return RecordingRoot{}, ErrRecordingRootConflict
	}
	if err != nil {
		return RecordingRoot{}, recordingRootPersistenceError(ctx)
	}
	root := RecordingRoot{
		ID: rootID, Status: RecordingRootReady,
		absolutePath: absolutePath, canonicalKey: canonicalKey, volumeIdentity: volumeIdentity,
	}
	if !recordingRootRowMatches(row, root) {
		return RecordingRoot{}, ErrRecordingRootConflict
	}
	if err := ctx.Err(); err != nil {
		return RecordingRoot{}, err
	}
	return root, nil
}

func inspectRecordingRoot(path string) (string, string, string, error) {
	if !validRecordingRootPathText(path) {
		return "", "", "", ErrRecordingRootInvalid
	}
	absolutePath, err := filepath.Abs(filepath.Clean(path))
	if err != nil || !filepath.IsAbs(absolutePath) {
		return "", "", "", ErrRecordingRootInvalid
	}
	resolvedPath, err := filepath.EvalSymlinks(absolutePath)
	if err != nil {
		return "", "", "", ErrRecordingRootUnavailable
	}
	resolvedPath, err = filepath.Abs(filepath.Clean(resolvedPath))
	if err != nil || !validRecordingRootPathText(resolvedPath) {
		return "", "", "", ErrRecordingRootInvalid
	}
	info, err := os.Stat(resolvedPath)
	if err != nil || !info.IsDir() {
		return "", "", "", ErrRecordingRootUnavailable
	}
	normalizedPath, err := normalizeRecordingRootPathIdentity(resolvedPath)
	if err != nil || normalizedPath == "" {
		return "", "", "", ErrRecordingRootInvalid
	}
	canonicalKey := recordingRootDigest(normalizedPath)
	volumeIdentity, err := recordingRootVolumeIdentity(resolvedPath)
	if err != nil || !validRecordingRootDigest(volumeIdentity) {
		return "", "", "", ErrRecordingRootUnavailable
	}
	return resolvedPath, canonicalKey, volumeIdentity, nil
}

func validRecordingRootPathText(path string) bool {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return false
	}
	for _, value := range path {
		if value == '%' || unicode.IsControl(value) {
			return false
		}
	}
	return true
}

func recordingRootDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func validRecordingRootDigest(value string) bool {
	if len(value) != recordingRootDigestHexLength {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func verifyRecordingRootWritable(root string) error {
	temporary, err := os.CreateTemp(root, recordingRootWriteProbePrefix)
	if err != nil {
		return ErrRecordingRootUnavailable
	}
	temporaryPath := temporary.Name()
	renamedPath := temporaryPath + ".renamed"
	defer os.Remove(temporaryPath)
	defer os.Remove(renamedPath)
	if _, err := temporary.Write([]byte("douyinlive-root-probe\n")); err != nil {
		_ = temporary.Close()
		return ErrRecordingRootUnavailable
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return ErrRecordingRootUnavailable
	}
	if err := temporary.Close(); err != nil {
		return ErrRecordingRootUnavailable
	}
	if err := os.Rename(temporaryPath, renamedPath); err != nil {
		return ErrRecordingRootUnavailable
	}
	if err := os.Remove(renamedPath); err != nil {
		return ErrRecordingRootUnavailable
	}
	return nil
}

func loadOrCreateRecordingRootMarkerLocked(root, volumeIdentity string) (recordingRootMarker, error) {
	recordingRootMarkerMu.Lock()
	defer recordingRootMarkerMu.Unlock()
	return loadOrCreateRecordingRootMarker(root, volumeIdentity)
}

func loadOrCreateRecordingRootMarker(root, volumeIdentity string) (recordingRootMarker, error) {
	markerPath := filepath.Join(root, recordingRootMarkerName)
	marker, err := readRecordingRootMarker(markerPath)
	if err == nil {
		return marker, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return recordingRootMarker{}, err
	}

	rootID, err := uuid.NewV7()
	if err != nil {
		return recordingRootMarker{}, ErrRecordingRootUnavailable
	}
	marker = recordingRootMarker{
		Version: recordingRootMarkerVersion, RootID: rootID.String(), VolumeIdentity: volumeIdentity,
	}
	payload, err := encodeRecordingRootMarker(marker)
	if err != nil {
		return recordingRootMarker{}, ErrRecordingRootUnavailable
	}
	temporary, err := os.CreateTemp(root, ".douyinlive-recording-root-*.tmp")
	if err != nil {
		return recordingRootMarker{}, ErrRecordingRootUnavailable
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return recordingRootMarker{}, ErrRecordingRootUnavailable
	}
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return recordingRootMarker{}, ErrRecordingRootUnavailable
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return recordingRootMarker{}, ErrRecordingRootUnavailable
	}
	if err := temporary.Close(); err != nil {
		return recordingRootMarker{}, ErrRecordingRootUnavailable
	}
	if err := promoteRecordingRootMarkerExclusive(temporaryPath, markerPath); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return readRecordingRootMarker(markerPath)
		}
		return recordingRootMarker{}, ErrRecordingRootUnavailable
	}
	return marker, nil
}

func encodeRecordingRootMarker(marker recordingRootMarker) ([]byte, error) {
	payload, err := json.Marshal(struct {
		Version        int    `json:"version"`
		RootID         string `json:"rootId"`
		VolumeIdentity string `json:"volumeIdentity"`
	}{Version: marker.Version, RootID: marker.RootID, VolumeIdentity: marker.VolumeIdentity})
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

func readRecordingRootMarker(markerPath string) (recordingRootMarker, error) {
	info, err := os.Lstat(markerPath)
	if errors.Is(err, fs.ErrNotExist) {
		return recordingRootMarker{}, fs.ErrNotExist
	}
	if err != nil {
		return recordingRootMarker{}, ErrRecordingRootUnavailable
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > recordingRootMarkerMaxBytes {
		return recordingRootMarker{}, ErrRecordingRootConflict
	}
	file, err := os.Open(markerPath)
	if err != nil {
		return recordingRootMarker{}, ErrRecordingRootUnavailable
	}
	payload, readErr := io.ReadAll(io.LimitReader(file, recordingRootMarkerMaxBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return recordingRootMarker{}, ErrRecordingRootUnavailable
	}
	if len(payload) == 0 || len(payload) > recordingRootMarkerMaxBytes {
		return recordingRootMarker{}, ErrRecordingRootConflict
	}
	marker, err := decodeRecordingRootMarker(payload)
	if err != nil || marker.Version != recordingRootMarkerVersion {
		return recordingRootMarker{}, ErrRecordingRootConflict
	}
	if err := validateUUIDv7("recording root marker id", marker.RootID); err != nil {
		return recordingRootMarker{}, ErrRecordingRootConflict
	}
	if !validRecordingRootDigest(marker.VolumeIdentity) {
		return recordingRootMarker{}, ErrRecordingRootConflict
	}
	return marker, nil
}

func decodeRecordingRootMarker(payload []byte) (recordingRootMarker, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return recordingRootMarker{}, ErrRecordingRootConflict
	}
	marker := recordingRootMarker{}
	seen := make(map[string]struct{}, 3)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return recordingRootMarker{}, ErrRecordingRootConflict
		}
		if _, exists := seen[key]; exists {
			return recordingRootMarker{}, ErrRecordingRootConflict
		}
		seen[key] = struct{}{}
		switch key {
		case "version":
			err = decoder.Decode(&marker.Version)
		case "rootId":
			err = decoder.Decode(&marker.RootID)
		case "volumeIdentity":
			err = decoder.Decode(&marker.VolumeIdentity)
		default:
			return recordingRootMarker{}, ErrRecordingRootConflict
		}
		if err != nil {
			return recordingRootMarker{}, ErrRecordingRootConflict
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || len(seen) != 3 {
		return recordingRootMarker{}, ErrRecordingRootConflict
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return recordingRootMarker{}, ErrRecordingRootConflict
	}
	return marker, nil
}

func (r *SQLiteRepository) persistRecordingRoot(ctx context.Context, root RecordingRoot) error {
	transaction, err := r.writer.BeginTx(ctx, nil)
	if err != nil {
		return recordingRootPersistenceError(ctx)
	}
	defer transaction.Rollback()

	nowMillis := r.now().UTC().UnixMilli()
	if _, err := transaction.ExecContext(ctx, `INSERT INTO recording_roots(
		id, absolute_path, canonical_key, volume_identity, status, created_at, updated_at, last_verified_at
	) VALUES (?, ?, ?, ?, 'ready', ?, ?, ?)
	ON CONFLICT DO NOTHING`,
		root.ID, root.absolutePath, root.canonicalKey, root.volumeIdentity,
		nowMillis, nowMillis, nowMillis,
	); err != nil {
		return recordingRootPersistenceError(ctx)
	}

	row, err := queryRecordingRootRow(ctx, transaction, root.ID)
	if errors.Is(err, sql.ErrNoRows) {
		var conflictingID string
		if conflictErr := transaction.QueryRowContext(ctx,
			`SELECT id FROM recording_roots WHERE canonical_key = ?`, root.canonicalKey,
		).Scan(&conflictingID); conflictErr == nil {
			return ErrRecordingRootConflict
		}
		return recordingRootPersistenceError(ctx)
	}
	if err != nil {
		return recordingRootPersistenceError(ctx)
	}
	if !recordingRootRowMatches(row, root) {
		return ErrRecordingRootConflict
	}
	verifiedAt := nowMillis
	if verifiedAt < row.updatedAt {
		verifiedAt = row.updatedAt
	}
	result, err := transaction.ExecContext(ctx, `UPDATE recording_roots
		SET status = 'ready', updated_at = ?, last_verified_at = ?
		WHERE id = ? AND canonical_key = ? AND volume_identity = ? AND status = 'ready'`,
		verifiedAt, verifiedAt, root.ID, root.canonicalKey, root.volumeIdentity,
	)
	if err != nil {
		return recordingRootPersistenceError(ctx)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return ErrRecordingRootConflict
	}
	if err := transaction.Commit(); err != nil {
		return recordingRootPersistenceError(ctx)
	}
	return nil
}

func queryRecordingRootRow(ctx context.Context, queryer queryRower, id string) (recordingRootRow, error) {
	var row recordingRootRow
	err := queryer.QueryRowContext(ctx, `SELECT id, absolute_path, canonical_key, volume_identity,
		status, created_at, updated_at, last_verified_at
		FROM recording_roots WHERE id = ?`, id).Scan(
		&row.id, &row.absolutePath, &row.canonicalKey, &row.volumeIdentity,
		&row.status, &row.createdAt, &row.updatedAt, &row.lastVerifiedAt,
	)
	return row, err
}

func recordingRootRowMatches(row recordingRootRow, root RecordingRoot) bool {
	if row.id != root.ID || row.status != RecordingRootReady || row.createdAt > row.updatedAt {
		return false
	}
	if row.lastVerifiedAt.Valid && row.lastVerifiedAt.Int64 < row.createdAt {
		return false
	}
	if !validRecordingRootDigest(row.canonicalKey) || !validRecordingRootDigest(row.volumeIdentity) {
		return false
	}
	if row.canonicalKey != root.canonicalKey || row.volumeIdentity != root.volumeIdentity {
		return false
	}
	if !validRecordingRootPathText(row.absolutePath) {
		return false
	}
	normalizedPath, err := normalizeRecordingRootPathIdentity(filepath.Clean(row.absolutePath))
	return err == nil && recordingRootDigest(normalizedPath) == row.canonicalKey
}

func recordingRootPersistenceError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrRecordingRootPersistence
}
