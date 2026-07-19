//go:build !windows

package capture

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
)

func normalizeRecordingRootPathIdentity(path string) (string, error) {
	return filepath.Clean(path), nil
}

func recordingRootVolumeIdentity(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	system := reflect.ValueOf(info.Sys())
	if !system.IsValid() {
		return "", errors.New("filesystem identity is unavailable")
	}
	if system.Kind() == reflect.Pointer {
		if system.IsNil() {
			return "", errors.New("filesystem identity is unavailable")
		}
		system = system.Elem()
	}
	if system.Kind() != reflect.Struct {
		return "", errors.New("filesystem identity is unavailable")
	}
	device := system.FieldByName("Dev")
	if !device.IsValid() {
		return "", errors.New("filesystem device identity is unavailable")
	}
	var deviceValue string
	switch device.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		deviceValue = fmt.Sprintf("%d", device.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		deviceValue = fmt.Sprintf("%d", device.Uint())
	default:
		return "", errors.New("filesystem device identity is unavailable")
	}
	return recordingRootDigest(runtime.GOOS + "|" + deviceValue), nil
}

func promoteRecordingRootMarkerExclusive(temporaryPath, markerPath string) error {
	if err := os.Link(temporaryPath, markerPath); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fs.ErrExist
		}
		return err
	}
	directory, err := os.Open(filepath.Dir(markerPath))
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}
