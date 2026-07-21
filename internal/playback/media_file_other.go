//go:build !windows

package playback

import (
	"os"
	"path/filepath"
)

func openPlaybackRootGuard(root string) (*os.File, os.FileInfo, error) {
	file, err := os.Open(root)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

func openPlaybackMediaHandle(target string) (*os.File, error) {
	return os.Open(target)
}

func playbackFinalPath(_ *os.File, target string) (string, error) {
	return filepath.EvalSymlinks(target)
}

func playbackPathsEqual(left, right string) bool {
	return filepath.Clean(left) == filepath.Clean(right)
}
