package capture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

var ErrMediaArtifactFailed = errors.New("MEDIA_ARTIFACT_FAILED")

const mediaArtifactTimeout = 15 * time.Minute
const mediaArtifactPipeInput = "pipe:0"

type mediaArtifactVerifyFunc func(context.Context) error

// mediaArtifactSource owns one already-open source snapshot. Read hashes the
// exact bytes consumed by FFmpeg; verify rejects short reads, altered bytes,
// and a source path that no longer resolves to the opened file version.
type mediaArtifactSource struct {
	snapshot       *mediaFileSnapshot
	path           string
	expectedSize   int64
	expectedDigest string
	observedSize   int64
	hash           hash.Hash
	readErr        error
}

func newMediaArtifactSource(path string, expectedSize int64, expectedDigest string) (*mediaArtifactSource, error) {
	if !validMediaAbsolutePath(path) || expectedSize <= 0 || expectedDigest == "" ||
		!validMediaDigest(expectedDigest) {
		return nil, ErrMediaArtifactFailed
	}
	snapshot, err := openMediaFileSnapshot(path)
	if err != nil {
		return nil, err
	}
	if snapshot.info.Size() != expectedSize {
		snapshot.Close()
		return nil, ErrMediaFileConflict
	}
	return &mediaArtifactSource{
		snapshot: snapshot, path: filepath.Clean(path), expectedSize: expectedSize,
		expectedDigest: expectedDigest, hash: sha256.New(),
	}, nil
}

func (source *mediaArtifactSource) Read(buffer []byte) (int, error) {
	if source == nil || source.snapshot == nil || source.snapshot.file == nil || source.hash == nil {
		return 0, ErrMediaArtifactFailed
	}
	count, err := source.snapshot.file.Read(buffer)
	if count > 0 {
		source.observedSize += int64(count)
		_, _ = source.hash.Write(buffer[:count])
	}
	if err != nil && !errors.Is(err, io.EOF) {
		source.readErr = err
	}
	return count, err
}

func (source *mediaArtifactSource) verify(ctx context.Context) error {
	if ctx == nil || source == nil || source.snapshot == nil || source.hash == nil {
		return ErrMediaArtifactFailed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if source.readErr != nil || source.observedSize != source.expectedSize ||
		hex.EncodeToString(source.hash.Sum(nil)) != source.expectedDigest {
		return ErrMediaFileConflict
	}
	if err := source.snapshot.matchesPath(source.path); err != nil {
		return ErrMediaFileConflict
	}
	return nil
}

func (source *mediaArtifactSource) Close() error {
	if source == nil || source.snapshot == nil {
		return nil
	}
	return source.snapshot.Close()
}

func generateASRAudio(ctx context.Context, ffmpegPath string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
	return generateMediaArtifact(ctx, ffmpegPath, source, target, verify, buildASRAudioArgs)
}

func generatePlaybackMP4(ctx context.Context, ffmpegPath string, source *mediaArtifactSource, target string, verify mediaArtifactVerifyFunc) error {
	return generateMediaArtifact(ctx, ffmpegPath, source, target, verify, buildPlaybackMP4Args)
}

func generateMediaArtifact(
	ctx context.Context,
	ffmpegPath string,
	source *mediaArtifactSource,
	target string,
	verify mediaArtifactVerifyFunc,
	buildArgs func(string, string) ([]string, error),
) error {
	if ctx == nil || !validMediaExecutable(ffmpegPath) || source == nil || verify == nil ||
		!validMediaAbsolutePath(target) || buildArgs == nil {
		return ErrMediaArtifactFailed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Lstat(target); err == nil {
		return ErrMediaFileConflict
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrMediaArtifactFailed
	}
	directory := filepath.Dir(filepath.Clean(target))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return ErrMediaArtifactFailed
	}
	temporary := filepath.Join(directory, ".artifact-"+uuid.NewString()+filepath.Ext(target)+".partial")
	defer os.Remove(temporary)
	args, err := buildArgs(mediaArtifactPipeInput, temporary)
	if err != nil {
		return err
	}
	commandCtx, cancel := context.WithTimeout(ctx, mediaArtifactTimeout)
	defer cancel()
	stderr := newBoundedTextBuffer(ffmpegLogDefaultBytes)
	command := exec.CommandContext(commandCtx, filepath.Clean(ffmpegPath), args...)
	configureMediaCommand(command)
	command.Stdin = source
	command.Stdout = io.Discard
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		_ = stderr.Snapshot()
		if commandCtx.Err() != nil {
			return commandCtx.Err()
		}
		return ErrMediaArtifactFailed
	}
	info, err := os.Lstat(temporary)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return ErrMediaArtifactFailed
	}
	if err := verify(commandCtx); err != nil {
		return err
	}
	if err := publishMediaFile(temporary, target); err != nil {
		return err
	}
	if err := verify(commandCtx); err != nil {
		return err
	}
	return nil
}

func buildASRAudioArgs(input, output string) ([]string, error) {
	if input != mediaArtifactPipeInput || !validMediaAbsolutePath(output) {
		return nil, ErrMediaArtifactFailed
	}
	return []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-n",
		"-i", input,
		"-map", "0:a:0", "-vn", "-ac", "1", "-ar", "16000",
		"-c:a", "pcm_s16le", "-map_metadata", "-1", "-map_chapters", "-1",
		"-f", "wav", output,
	}, nil
}

func buildPlaybackMP4Args(input, output string) ([]string, error) {
	if input != mediaArtifactPipeInput || !validMediaAbsolutePath(output) {
		return nil, ErrMediaArtifactFailed
	}
	return []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-n",
		"-i", input,
		"-map", "0:v:0", "-map", "0:a:0?", "-c", "copy",
		"-map_metadata", "-1", "-map_chapters", "-1",
		"-movflags", "+faststart", "-f", "mp4", output,
	}, nil
}

func playbackCopyCompatible(videoCodec, audioCodec string) bool {
	videoCodec = strings.ToLower(strings.TrimSpace(videoCodec))
	audioCodec = strings.ToLower(strings.TrimSpace(audioCodec))
	return videoCodec == "h264" && (audioCodec == "" || audioCodec == "aac")
}

func validMediaExecutable(path string) bool {
	if !validMediaAbsolutePath(path) {
		return false
	}
	info, err := os.Lstat(filepath.Clean(path))
	return err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0
}

func validMediaInput(path string) bool {
	if !validMediaAbsolutePath(path) {
		return false
	}
	info, err := os.Lstat(filepath.Clean(path))
	return err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.Size() > 0
}
