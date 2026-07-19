//go:build windows

package capture

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestWindowsMediaRelativePathRejectsAliasesAndReservedDevices(t *testing.T) {
	for _, value := range []string{
		"rooms/trailing./session",
		"rooms/trailing /session",
		"rooms/CON/session",
		"rooms/nul.txt/session",
		"rooms/COM1/session",
		"rooms/lpt9.log/session",
		"rooms/Ä/session",
		"rooms/é/session",
		"rooms/用户/session",
	} {
		if validMediaRelativePath(value) {
			t.Fatalf("Windows-ambiguous relative path accepted: %q", value)
		}
	}
	if !validMediaRelativePath("rooms/safe/session") {
		t.Fatal("safe Windows relative path was rejected")
	}
	if mediaPlatformRelativePathKey("Rooms/ABC/Session") !=
		mediaPlatformRelativePathKey("rooms/abc/session") {
		t.Fatal("Windows case aliases did not share one identity key")
	}
}

func TestSecureMediaSessionDirectoryRejectsWindowsJunctionWithoutSideEffects(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	junction := filepath.Join(root, "junction")
	if output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", junction, outside).CombinedOutput(); err != nil {
		t.Skipf("junction unavailable: %v (%s)", err, output)
	}
	defer os.Remove(junction)
	info, err := os.Lstat(junction)
	if err != nil {
		t.Fatal(err)
	}
	if reparse, err := mediaPathIsReparsePoint(junction, info); err != nil || !reparse {
		t.Fatalf("junction reparse detection = (%v, %v)", reparse, err)
	}
	if _, err := secureMediaSessionDirectory(root, "junction/session"); !errors.Is(err, ErrMediaPathInvalid) {
		t.Fatalf("junction component error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "session")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected junction created an outside directory: %v", err)
	}
}
