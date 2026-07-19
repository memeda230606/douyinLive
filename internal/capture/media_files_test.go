package capture

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPublishMediaFileNeverReplacesTarget(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source.partial")
	target := filepath.Join(directory, "target.mkv")
	if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := publishMediaFile(source, target); !errors.Is(err, ErrMediaFileConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	assertMediaTestContent(t, target, "old")
	assertMediaTestContent(t, source, "new")
}

func TestPublishMediaFileMovesCompletedFile(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source.partial")
	target := filepath.Join(directory, "target.mkv")
	if err := os.WriteFile(source, []byte("completed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := publishMediaFile(source, target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source still exists: %v", err)
	}
	assertMediaTestContent(t, target, "completed")
}

func TestPublishMediaFileSyncFailureKeepsSourceAndDoesNotPublish(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source.partial")
	target := filepath.Join(directory, "target.mkv")
	if err := os.WriteFile(source, []byte("completed"), 0o600); err != nil {
		t.Fatal(err)
	}

	syncFailure := errors.New("injected sync failure")
	syncCalled := false
	err := publishMediaFileWithSync(source, target, func(file *os.File) error {
		syncCalled = true
		if file.Name() != source {
			return errors.New("unexpected source handle")
		}
		return syncFailure
	})
	if err != ErrMediaFileIO {
		t.Fatalf("error = %v, want %v", err, ErrMediaFileIO)
	}
	if !syncCalled {
		t.Fatal("source Sync was not attempted")
	}
	assertMediaTestContent(t, source, "completed")
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target was published after Sync failure: %v", err)
	}

	// Windows cannot rename a file while this process still holds its open
	// handle, so this also proves the failure path closed the source.
	renamed := filepath.Join(directory, "source-after-failure.partial")
	if err := os.Rename(source, renamed); err != nil {
		t.Fatalf("source handle was not closed: %v", err)
	}
	assertMediaTestContent(t, renamed, "completed")
}

func TestWriteMediaFileAtomicReplacesOnlyAfterCompleteWrite(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "manifests", "media.json")
	if err := writeMediaFileAtomic(target, []byte("first")); err != nil {
		t.Fatal(err)
	}
	assertMediaTestContent(t, target, "first")
	if err := writeMediaFileAtomic(target, []byte("second")); err != nil {
		t.Fatal(err)
	}
	assertMediaTestContent(t, target, "second")
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "media.json" {
		t.Fatalf("unexpected manifest directory: %#v", entries)
	}
}

func TestMediaFileHelpersRejectUnsafeInputs(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source.partial")
	if err := os.WriteFile(source, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := publishMediaFile(source, "relative.mkv"); !errors.Is(err, ErrMediaFileInvalid) {
		t.Fatalf("expected invalid path, got %v", err)
	}
	if err := writeMediaFileAtomic(filepath.Join(directory, "media.json"), nil); !errors.Is(err, ErrMediaFileInvalid) {
		t.Fatalf("expected invalid payload, got %v", err)
	}
}

func assertMediaTestContent(t *testing.T, path, want string) {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != want {
		t.Fatalf("content = %q, want %q", payload, want)
	}
}
