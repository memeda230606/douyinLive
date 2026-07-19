//go:build !windows

package capture

import (
	"errors"
	"os"
	"path/filepath"
)

func publishMediaFilePlatform(source, target string) error {
	if err := os.Link(source, target); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrMediaFileConflict
		}
		return ErrMediaFileIO
	}
	if err := syncMediaDirectory(filepath.Dir(target)); err != nil {
		return err
	}
	if err := os.Remove(source); err != nil {
		return ErrMediaFileIO
	}
	if !recorderPathsEqual(filepath.Dir(source), filepath.Dir(target)) {
		if err := syncMediaDirectory(filepath.Dir(source)); err != nil {
			return err
		}
	}
	return nil
}

func replaceMediaFilePlatform(source, target string) error {
	if err := os.Rename(source, target); err != nil {
		return ErrMediaFileIO
	}
	return syncMediaDirectory(filepath.Dir(target))
}

func syncMediaDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return ErrMediaFileIO
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return ErrMediaFileIO
	}
	return nil
}
