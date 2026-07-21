//go:build windows

package playback

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func openPlaybackRootGuard(root string) (*os.File, os.FileInfo, error) {
	pointer, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return nil, nil, err
	}
	handle, err := windows.CreateFile(
		pointer,
		windows.FILE_LIST_DIRECTORY|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(handle), root)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, nil, errors.New("create playback root guard")
	}
	info, err := file.Stat()
	if err != nil || !info.IsDir() {
		_ = file.Close()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, errors.New("playback root guard is not a directory")
	}
	return file, info, nil
}

// Completed playback media is opened without FILE_SHARE_WRITE or
// FILE_SHARE_DELETE. The verified handle therefore cannot be mutated or
// replaced between digest validation and http.ServeContent.
func openPlaybackMediaHandle(target string) (*os.File, error) {
	pointer, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pointer,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), target)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("create playback media handle")
	}
	return file, nil
}

func playbackFinalPath(file *os.File, _ string) (string, error) {
	buffer := make([]uint16, windows.MAX_LONG_PATH)
	for attempts := 0; attempts < 2; attempts++ {
		length, err := windows.GetFinalPathNameByHandle(
			windows.Handle(file.Fd()), &buffer[0], uint32(len(buffer)), 0,
		)
		if err != nil {
			return "", err
		}
		if length < uint32(len(buffer)) {
			return normalizePlaybackWindowsPath(windows.UTF16ToString(buffer[:length])), nil
		}
		buffer = make([]uint16, length+1)
	}
	return "", errors.New("playback final path exceeds limit")
}

func normalizePlaybackWindowsPath(value string) string {
	value = filepath.Clean(value)
	lower := strings.ToLower(value)
	switch {
	case strings.HasPrefix(lower, `\\?\unc\`):
		return `\\` + value[len(`\\?\UNC\`):]
	case strings.HasPrefix(lower, `\\?\`):
		return value[len(`\\?\`):]
	default:
		return value
	}
}

func playbackPathsEqual(left, right string) bool {
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}
