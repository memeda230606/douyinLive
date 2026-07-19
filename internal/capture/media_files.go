package capture

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

var (
	ErrMediaFileInvalid  = errors.New("MEDIA_FILE_INVALID")
	ErrMediaFileConflict = errors.New("MEDIA_FILE_CONFLICT")
	ErrMediaFileIO       = errors.New("MEDIA_FILE_IO")
)

// The legal manifest ceiling is 4096 segments plus two artifacts per segment.
// UTF-8 validation and SetEscapeHTML(false) bound JSON string expansion; the
// maximum-input contract test exercises the full cardinality and path width.
const mediaManifestPayloadMax = 128 << 20

type mediaFileSyncFunc func(*os.File) error

// publishMediaFile promotes a completed file without ever replacing an
// existing target. Platform implementations keep the operation atomic.
func publishMediaFile(source, target string) error {
	return publishMediaFileWithSync(source, target, func(file *os.File) error {
		return file.Sync()
	})
}

func publishMediaFileWithSync(source, target string, syncFile mediaFileSyncFunc) error {
	if !validMediaAbsolutePath(source) || !validMediaAbsolutePath(target) ||
		recorderPathsEqual(filepath.Clean(source), filepath.Clean(target)) || syncFile == nil {
		return ErrMediaFileInvalid
	}
	cleanedSource := filepath.Clean(source)
	cleanedTarget := filepath.Clean(target)
	info, err := os.Lstat(cleanedSource)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return ErrMediaFileInvalid
	}

	file, err := os.OpenFile(cleanedSource, os.O_RDWR, 0)
	if err != nil {
		return ErrMediaFileIO
	}
	openedInfo, statErr := file.Stat()
	if statErr != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		closeErr := file.Close()
		if statErr != nil || closeErr != nil {
			return ErrMediaFileIO
		}
		return ErrMediaFileInvalid
	}
	// Durability is established before the atomic publication. Always close
	// the handle before the platform move/link, including on Sync failure.
	syncErr := syncFile(file)
	closeErr := file.Close()
	if syncErr != nil || closeErr != nil {
		return ErrMediaFileIO
	}

	if err := publishMediaFilePlatform(cleanedSource, cleanedTarget); err != nil {
		return err
	}
	return nil
}

func writeMediaFileAtomic(target string, payload []byte) error {
	if !validMediaAbsolutePath(target) || len(payload) == 0 || len(payload) > mediaManifestPayloadMax {
		return ErrMediaFileInvalid
	}
	directory := filepath.Dir(filepath.Clean(target))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return ErrMediaFileIO
	}
	temporary := filepath.Join(directory, ".media-"+uuid.NewString()+".tmp")
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return ErrMediaFileIO
	}
	removeTemporary := true
	defer func() {
		_ = file.Close()
		if removeTemporary {
			_ = os.Remove(temporary)
		}
	}()
	if _, err := file.Write(payload); err != nil {
		return ErrMediaFileIO
	}
	if err := file.Sync(); err != nil {
		return ErrMediaFileIO
	}
	if err := file.Close(); err != nil {
		return ErrMediaFileIO
	}
	if err := replaceMediaFilePlatform(temporary, filepath.Clean(target)); err != nil {
		return err
	}
	removeTemporary = false
	return nil
}

func validMediaAbsolutePath(value string) bool {
	if value == "" || !filepath.IsAbs(value) || strings.Contains(value, "%") ||
		ffmpegControlCharacters.MatchString(value) {
		return false
	}
	cleaned := filepath.Clean(value)
	return cleaned != filepath.VolumeName(cleaned)+string(filepath.Separator)
}
