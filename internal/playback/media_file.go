package playback

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jwwsjlm/douyinLive/v2/internal/capture"
)

const playbackMediaRoutePrefix = "/playback/media/"

type MediaFile struct {
	file        *os.File
	size        int64
	modTime     time.Time
	contentType string
}

func (file *MediaFile) Read(buffer []byte) (int, error) { return file.file.Read(buffer) }
func (file *MediaFile) Seek(offset int64, whence int) (int64, error) {
	return file.file.Seek(offset, whence)
}
func (file *MediaFile) Close() error        { return file.file.Close() }
func (file *MediaFile) Size() int64         { return file.size }
func (file *MediaFile) ModTime() time.Time  { return file.modTime }
func (file *MediaFile) ContentType() string { return file.contentType }

type mediaFileBinding struct {
	relativePath string
	expectedSize int64
	expectedHash string
	container    string
	rootID       sql.NullString
	externalRoot sql.NullString
	rootStatus   sql.NullString
	canonicalKey sql.NullString
	volumeID     sql.NullString
}

func (s *Service) OpenMedia(ctx context.Context, artifactID string) (*MediaFile, error) {
	if ctx == nil || s == nil || s.repository == nil || s.repository.reader == nil || !isUUIDv7(artifactID) {
		return nil, fmt.Errorf("%w: media request", ErrInvalidArgument)
	}
	binding, err := s.loadMediaFileBinding(ctx, artifactID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrMediaNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load playback media: %w", err)
	}
	root := s.dataRoot
	if binding.rootID.Valid {
		if !binding.externalRoot.Valid || binding.externalRoot.String == "" ||
			!binding.rootStatus.Valid || binding.rootStatus.String != "ready" {
			return nil, ErrMediaUnavailable
		}
		root = binding.externalRoot.String
	}
	if root == "" || binding.expectedSize <= 0 || !validMediaDigest(binding.expectedHash) {
		return nil, ErrMediaUnavailable
	}
	var verifyRoot func() error
	if binding.rootID.Valid {
		if !binding.canonicalKey.Valid || !binding.volumeID.Valid {
			return nil, ErrMediaUnavailable
		}
		verifyRoot = func() error {
			return capture.VerifyRecordingRootReadOnly(
				root, binding.rootID.String, binding.canonicalKey.String, binding.volumeID.String,
			)
		}
	}
	opened, err := openVerifiedMediaFileWithRootVerifier(
		ctx, root, binding.relativePath, binding.expectedSize, binding.expectedHash, verifyRoot,
	)
	if err != nil {
		return nil, ErrMediaUnavailable
	}
	opened.contentType = "video/mp4"
	return opened, nil
}

func (s *Service) loadMediaFileBinding(ctx context.Context, artifactID string) (mediaFileBinding, error) {
	var binding mediaFileBinding
	var artifactStatus, artifactKind, codec, sourceHash string
	var segmentStatus, segmentContainer, segmentCodec, segmentHash string
	err := s.repository.reader.QueryRowContext(ctx, `SELECT
		ma.relative_path, ma.size_bytes, ma.sha256, ma.status, ma.kind,
		ma.container, ma.codec, ma.source_sha256,
		ms.status, ms.container, ms.video_codec, ms.sha256,
		sm.root_id, rr.absolute_path, rr.status, rr.canonical_key, rr.volume_identity
		FROM media_artifacts ma
		JOIN media_segments ms
			ON ms.session_id = ma.session_id AND ms.id = ma.media_segment_id
		JOIN session_media sm ON sm.session_id = ma.session_id
		LEFT JOIN recording_roots rr ON rr.id = sm.root_id
		WHERE ma.id = ?`, artifactID).Scan(
		&binding.relativePath, &binding.expectedSize, &binding.expectedHash,
		&artifactStatus, &artifactKind, &binding.container, &codec, &sourceHash,
		&segmentStatus, &segmentContainer, &segmentCodec, &segmentHash,
		&binding.rootID, &binding.externalRoot, &binding.rootStatus,
		&binding.canonicalKey, &binding.volumeID,
	)
	if err != nil {
		return mediaFileBinding{}, err
	}
	if artifactStatus != "complete" || artifactKind != "playback_mp4" ||
		binding.container != "mp4" || codec != "h264" ||
		(segmentStatus != "complete" && segmentStatus != "recovered") ||
		segmentContainer != "mkv" || segmentCodec != "h264" ||
		!validMediaDigest(segmentHash) || sourceHash != segmentHash {
		return mediaFileBinding{}, ErrMediaUnavailable
	}
	return binding, nil
}

func openVerifiedMediaFile(
	ctx context.Context,
	root, relative string,
	expectedSize int64,
	expectedHash string,
) (_ *MediaFile, resultErr error) {
	return openVerifiedMediaFileWithRootVerifier(
		ctx, root, relative, expectedSize, expectedHash, nil,
	)
}

func openVerifiedMediaFileWithRootVerifier(
	ctx context.Context,
	root, relative string,
	expectedSize int64,
	expectedHash string,
	verifyRoot func() error,
) (_ *MediaFile, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validPlaybackMediaRelativePath(relative) || expectedSize <= 0 || !validMediaDigest(expectedHash) {
		return nil, ErrMediaUnavailable
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, ErrMediaUnavailable
	}
	absoluteRoot = filepath.Clean(absoluteRoot)
	rootInfo, err := os.Lstat(absoluteRoot)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, ErrMediaUnavailable
	}
	resolvedRoot, err := filepath.EvalSymlinks(absoluteRoot)
	if err != nil || !playbackPathsEqual(filepath.Clean(resolvedRoot), absoluteRoot) {
		return nil, ErrMediaUnavailable
	}
	guard, guardedRootInfo, err := openPlaybackRootGuard(absoluteRoot)
	if err != nil {
		return nil, ErrMediaUnavailable
	}
	defer func() { resultErr = errors.Join(resultErr, guard.Close()) }()
	if !os.SameFile(rootInfo, guardedRootInfo) {
		return nil, ErrMediaUnavailable
	}
	if verifyRoot != nil && verifyRoot() != nil {
		return nil, ErrMediaUnavailable
	}

	target := filepath.Join(absoluteRoot, filepath.FromSlash(relative))
	relativeTarget, err := filepath.Rel(absoluteRoot, target)
	if err != nil || relativeTarget == ".." || strings.HasPrefix(relativeTarget, ".."+string(filepath.Separator)) {
		return nil, ErrMediaUnavailable
	}
	pathInfo, err := os.Lstat(target)
	if err != nil || !pathInfo.Mode().IsRegular() || pathInfo.Mode()&os.ModeSymlink != 0 {
		return nil, ErrMediaUnavailable
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil || !playbackPathsEqual(filepath.Clean(resolvedTarget), filepath.Clean(target)) {
		return nil, ErrMediaUnavailable
	}
	file, err := openPlaybackMediaHandle(target)
	if err != nil {
		return nil, ErrMediaUnavailable
	}
	keepOpen := false
	defer func() {
		if !keepOpen {
			resultErr = errors.Join(resultErr, file.Close())
		}
	}()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) || openedInfo.Size() != expectedSize {
		return nil, ErrMediaUnavailable
	}
	finalPath, err := playbackFinalPath(file, target)
	if err != nil || !playbackPathsEqual(filepath.Clean(finalPath), filepath.Clean(target)) {
		return nil, ErrMediaUnavailable
	}
	digest, err := hashOpenedMediaFile(ctx, file)
	if err != nil || digest != expectedHash {
		return nil, ErrMediaUnavailable
	}
	afterHash, err := file.Stat()
	if err != nil || !os.SameFile(openedInfo, afterHash) || afterHash.Size() != expectedSize ||
		!afterHash.ModTime().Equal(openedInfo.ModTime()) {
		return nil, ErrMediaUnavailable
	}
	reopenedInfo, err := os.Lstat(target)
	if err != nil || !reopenedInfo.Mode().IsRegular() || reopenedInfo.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(openedInfo, reopenedInfo) {
		return nil, ErrMediaUnavailable
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, ErrMediaUnavailable
	}
	keepOpen = true
	return &MediaFile{file: file, size: expectedSize, modTime: openedInfo.ModTime()}, nil
}

func validPlaybackMediaRelativePath(value string) bool {
	if value == "" || strings.ContainsAny(value, "\\:%\x00\r\n") || strings.HasPrefix(value, "/") {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." ||
			strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
			return false
		}
		for _, character := range component {
			if character < 0x20 || character > 0x7e {
				return false
			}
		}
	}
	return true
}

func validMediaDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

func hashOpenedMediaFile(ctx context.Context, file *os.File) (string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	hash := sha256.New()
	buffer := make([]byte, 128<<10)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			if _, err := hash.Write(buffer[:count]); err != nil {
				return "", err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
