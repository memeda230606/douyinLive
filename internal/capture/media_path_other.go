//go:build !windows

package capture

import "os"

func mediaPathIsReparsePoint(_ string, info os.FileInfo) (bool, error) {
	return info.Mode()&os.ModeSymlink != 0, nil
}

func validMediaPlatformRelativePath(string) bool {
	return true
}

func mediaPlatformRelativePathKey(value string) string {
	return value
}
