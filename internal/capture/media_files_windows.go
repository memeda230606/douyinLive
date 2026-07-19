//go:build windows

package capture

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func publishMediaFilePlatform(source, target string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return ErrMediaFileInvalid
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return ErrMediaFileInvalid
	}
	err = windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
		return ErrMediaFileConflict
	}
	if err != nil {
		return ErrMediaFileIO
	}
	return nil
}

func replaceMediaFilePlatform(source, target string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return ErrMediaFileInvalid
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return ErrMediaFileInvalid
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return ErrMediaFileIO
		}
		return ErrMediaFileIO
	}
	return nil
}
