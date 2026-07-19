//go:build windows

package capture

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordingRootWindowsCanonicalCaseSymlinkAndVolumeIdentity(t *testing.T) {
	repository, store, _ := openRecordingRootRepository(t)
	defer store.Close()
	base := t.TempDir()
	targetPath := filepath.Join(base, "MiXeD-Recording-Root")
	if err := os.Mkdir(targetPath, 0o700); err != nil {
		t.Fatalf("Mkdir(target) error = %v", err)
	}

	upperCasePath := strings.ToUpper(targetPath)
	first, err := repository.RegisterRecordingRoot(context.Background(), upperCasePath)
	if err != nil {
		t.Fatalf("RegisterRecordingRoot(uppercase) error = %v", err)
	}
	second, err := repository.RegisterRecordingRoot(context.Background(), targetPath)
	if err != nil {
		t.Fatalf("RegisterRecordingRoot(original case) error = %v", err)
	}
	if first.ID != second.ID || first.canonicalKey != second.canonicalKey {
		t.Fatalf("case variants produced different identities: %v vs %v", first, second)
	}

	firstVolume, err := recordingRootVolumeIdentity(targetPath)
	if err != nil {
		t.Fatalf("recordingRootVolumeIdentity(first) error = %v", err)
	}
	secondVolume, err := recordingRootVolumeIdentity(upperCasePath)
	if err != nil {
		t.Fatalf("recordingRootVolumeIdentity(second) error = %v", err)
	}
	if firstVolume != secondVolume || !validRecordingRootDigest(firstVolume) {
		t.Fatalf("Windows volume identity is not stable")
	}

	aliasPath := filepath.Join(base, "recording-root-alias")
	if err := os.Symlink(targetPath, aliasPath); err != nil {
		t.Logf("directory symlink unavailable on this Windows host: %v", err)
		return
	}
	throughAlias, err := repository.RegisterRecordingRoot(context.Background(), aliasPath)
	if err != nil {
		t.Fatalf("RegisterRecordingRoot(symlink) error = %v", err)
	}
	if throughAlias.ID != first.ID || throughAlias.canonicalKey != first.canonicalKey {
		t.Fatalf("symlink did not resolve to existing root: %v vs %v", throughAlias, first)
	}
}
