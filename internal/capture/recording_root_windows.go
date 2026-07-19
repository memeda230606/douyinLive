//go:build windows

package capture

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/windows"
)

func normalizeRecordingRootPathIdentity(path string) (string, error) {
	normalized := filepath.ToSlash(filepath.Clean(path))
	lower := strings.ToLower(normalized)
	switch {
	case strings.HasPrefix(lower, `//?/unc/`):
		normalized = `//` + normalized[len(`//?/UNC/`):]
	case strings.HasPrefix(lower, `//?/`):
		normalized = normalized[len(`//?/`):]
	}
	return strings.ToLower(normalized), nil
}

func recordingRootVolumeIdentity(path string) (string, error) {
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	volumePathBuffer := make([]uint16, windows.MAX_LONG_PATH)
	if err := windows.GetVolumePathName(pathPointer, &volumePathBuffer[0], uint32(len(volumePathBuffer))); err != nil {
		return "", err
	}
	volumePath := windows.UTF16ToString(volumePathBuffer)
	if volumePath == "" {
		return "", errors.New("empty volume path")
	}
	volumePathPointer, err := windows.UTF16PtrFromString(volumePath)
	if err != nil {
		return "", err
	}
	var serialNumber uint32
	var maximumComponentLength uint32
	var fileSystemFlags uint32
	fileSystemBuffer := make([]uint16, 256)
	if err := windows.GetVolumeInformation(
		volumePathPointer,
		nil,
		0,
		&serialNumber,
		&maximumComponentLength,
		&fileSystemFlags,
		&fileSystemBuffer[0],
		uint32(len(fileSystemBuffer)),
	); err != nil {
		return "", err
	}
	volumeNameBuffer := make([]uint16, windows.MAX_LONG_PATH)
	stableVolumeName := ""
	if err := windows.GetVolumeNameForVolumeMountPoint(
		volumePathPointer, &volumeNameBuffer[0], uint32(len(volumeNameBuffer)),
	); err == nil {
		stableVolumeName = windows.UTF16ToString(volumeNameBuffer)
	}
	if stableVolumeName == "" {
		stableVolumeName = volumePath
	}
	identityMaterial := strings.ToLower(filepath.ToSlash(stableVolumeName)) + "|" +
		strconv.FormatUint(uint64(serialNumber), 16) + "|" +
		strings.ToLower(windows.UTF16ToString(fileSystemBuffer))
	return recordingRootDigest(identityMaterial), nil
}

func promoteRecordingRootMarkerExclusive(temporaryPath, markerPath string) error {
	temporaryPointer, err := windows.UTF16PtrFromString(temporaryPath)
	if err != nil {
		return err
	}
	markerPointer, err := windows.UTF16PtrFromString(markerPath)
	if err != nil {
		return err
	}
	err = windows.MoveFileEx(temporaryPointer, markerPointer, windows.MOVEFILE_WRITE_THROUGH)
	if err == nil {
		return nil
	}
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
		return fs.ErrExist
	}
	if _, statErr := os.Lstat(markerPath); statErr == nil {
		return fs.ErrExist
	}
	return err
}
