package capture

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSecureMediaSessionDirectoryCreatesBoundedLayout(t *testing.T) {
	root := t.TempDir()
	relative := "rooms/room-id/sessions/2026/07/session-id"
	directory, err := secureMediaSessionDirectory(root, relative)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, filepath.FromSlash(relative))
	if !recorderPathsEqual(directory, want) {
		t.Fatalf("directory = %q, want %q", directory, want)
	}
	for _, name := range []string{"media", "audio", "manifests"} {
		info, statErr := os.Stat(filepath.Join(directory, name))
		if statErr != nil || !info.IsDir() {
			t.Fatalf("missing %s directory: %v", name, statErr)
		}
	}
}

func TestMediaPathHelpersRejectEscapesAndAmbiguity(t *testing.T) {
	root := t.TempDir()
	tests := []string{
		"", ".", "..", "../outside", "/absolute", `rooms\\escape`,
		"rooms/../outside", "rooms//double", "rooms/%secret", "C:/absolute", "rooms/control\x00",
	}
	for _, value := range tests {
		if _, err := mediaAbsolutePath(root, value); !errors.Is(err, ErrMediaPathInvalid) {
			t.Fatalf("mediaAbsolutePath(%q) error = %v", value, err)
		}
	}
	if _, err := joinMediaRelativePath("rooms/safe", "../escape"); !errors.Is(err, ErrMediaPathInvalid) {
		t.Fatalf("joined escape error = %v", err)
	}
	if _, err := joinMediaRelativePath("rooms/safe", "nested/name"); !errors.Is(err, ErrMediaPathInvalid) {
		t.Fatalf("joined nested element error = %v", err)
	}
}

func TestSecureMediaSessionDirectoryRejectsSymlinkComponent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := secureMediaSessionDirectory(root, "linked/session"); !errors.Is(err, ErrMediaPathInvalid) {
		t.Fatalf("symlink component error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "session")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected symlink created an outside directory: %v", err)
	}
}

func TestCanonicalMediaRootRequiresExistingRealDirectory(t *testing.T) {
	root := t.TempDir()
	if got, err := canonicalMediaRoot(root); err != nil || !filepath.IsAbs(got) {
		t.Fatalf("canonical root = %q, err = %v", got, err)
	}
	if _, err := canonicalMediaRoot(filepath.Join(root, "missing")); !errors.Is(err, ErrMediaPathUnavailable) {
		t.Fatalf("missing root error = %v", err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := canonicalMediaRoot(file); !errors.Is(err, ErrMediaPathUnavailable) {
		t.Fatalf("file root error = %v", err)
	}
}

func TestMediaPathErrorsDoNotExposePrivatePaths(t *testing.T) {
	private := filepath.Join(t.TempDir(), "private-secret")
	for _, err := range []error{
		func() error { _, err := canonicalMediaRoot(private); return err }(),
		func() error { _, err := mediaAbsolutePath(private, "../escape"); return err }(),
	} {
		if err == nil {
			t.Fatal("expected path error")
		}
		if err.Error() != ErrMediaPathInvalid.Error() && err.Error() != ErrMediaPathUnavailable.Error() {
			t.Fatalf("unstable detailed error: %v", err)
		}
	}
}
