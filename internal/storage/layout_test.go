package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareLayoutCreatesExpectedDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	layout, err := PrepareLayout(root)
	if err != nil {
		t.Fatalf("PrepareLayout() error = %v", err)
	}

	if layout.Database != filepath.Join(root, "app.db") {
		t.Fatalf("Database = %q", layout.Database)
	}
	for _, directory := range []string{
		layout.Root,
		layout.ConfigDir,
		layout.RoomsDir,
		layout.LogsDir,
		layout.CacheDir,
		layout.ExportsDir,
		layout.BackupsDir,
	} {
		info, err := os.Stat(directory)
		if err != nil {
			t.Fatalf("Stat(%q) error = %v", directory, err)
		}
		if !info.IsDir() {
			t.Fatalf("%q is not a directory", directory)
		}
	}

	matches, err := filepath.Glob(filepath.Join(root, ".write-probe-*"))
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("write probe was not cleaned: %v", matches)
	}
}

func TestPrepareLayoutRejectsFileAsRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(root, []byte("fixture"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := PrepareLayout(root); err == nil {
		t.Fatal("PrepareLayout() error = nil, want file-root error")
	}
}

func TestDefaultRootUsesUserLocalDataDirectory(t *testing.T) {
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("UserCacheDir() error = %v", err)
	}
	got, err := DefaultRoot()
	if err != nil {
		t.Fatalf("DefaultRoot() error = %v", err)
	}
	want := filepath.Join(cache, ProductDirectory)
	if got != want {
		t.Fatalf("DefaultRoot() = %q, want %q", got, want)
	}
}
