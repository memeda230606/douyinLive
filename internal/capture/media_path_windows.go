//go:build windows

package capture

import (
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

func mediaPathIsReparsePoint(path string, info os.FileInfo) (bool, error) {
	if info.Mode()&os.ModeSymlink != 0 {
		return true, nil
	}
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	attributes, err := windows.GetFileAttributes(pointer)
	if err != nil {
		return false, err
	}
	return attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0, nil
}

func validMediaPlatformRelativePath(value string) bool {
	// Production media-relative paths are generated from ASCII constants and
	// UUIDs. Keeping that contract explicit makes SQLite's ASCII lower() key
	// identical to Windows path identity and avoids Unicode case-fold aliases
	// that vary across NTFS upcase tables.
	for _, character := range value {
		if character > 0x7f {
			return false
		}
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") ||
			isReservedWindowsMediaPathComponent(component) {
			return false
		}
	}
	return true
}

func mediaPlatformRelativePathKey(value string) string {
	return strings.ToLower(value)
}

func isReservedWindowsMediaPathComponent(component string) bool {
	base := component
	if index := strings.IndexByte(base, '.'); index >= 0 {
		base = base[:index]
	}
	base = strings.ToUpper(base)
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CLOCK$", "CONIN$", "CONOUT$":
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) {
		return base[3] >= '1' && base[3] <= '9'
	}
	return false
}
