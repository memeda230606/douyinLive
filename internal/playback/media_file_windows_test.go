//go:build windows

package playback

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenVerifiedMediaFileFreezesContentUntilClose(t *testing.T) {
	root := t.TempDir()
	relative := "rooms/019aa000-0000-7000-8000-000000000001/media/playback.mp4"
	target := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatalf("create media directory: %v", err)
	}
	payload := []byte("immutable-during-range")
	if err := os.WriteFile(target, payload, 0o600); err != nil {
		t.Fatalf("write media: %v", err)
	}
	digest := sha256.Sum256(payload)
	opened, err := openVerifiedMediaFile(
		context.Background(), root, relative, int64(len(payload)), hex.EncodeToString(digest[:]),
	)
	if err != nil {
		t.Fatalf("openVerifiedMediaFile() error = %v", err)
	}
	writer, writeErr := os.OpenFile(target, os.O_WRONLY, 0)
	if writeErr == nil {
		_ = writer.Close()
		_ = opened.Close()
		t.Fatal("verified media allowed a concurrent writer")
	}
	if err := os.Rename(target, target+".moved"); err == nil {
		_ = opened.Close()
		t.Fatal("verified media allowed replacement while open")
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("close verified media: %v", err)
	}
	writer, err = os.OpenFile(target, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("write after close remained blocked: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer after verified close: %v", err)
	}
}
