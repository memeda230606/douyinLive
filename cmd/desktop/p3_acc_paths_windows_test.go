//go:build p3accacceptance && windows

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestP3ACCMediaPathInspectionRejectsDirectoryAndReparse(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing.mkv")
	if state := inspectP3ACCAcceptanceMediaPath(missing); state != p3ACCMediaPathMissing {
		t.Fatalf("missing state = %d", state)
	}
	directory := filepath.Join(root, "directory.mkv")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatalf("create directory fixture: %v", err)
	}
	if state := inspectP3ACCAcceptanceMediaPath(directory); state != p3ACCMediaPathUnsafe {
		t.Fatalf("directory state = %d", state)
	}
	regular := filepath.Join(root, "regular.mkv")
	if err := os.WriteFile(regular, []byte("safe"), 0o600); err != nil {
		t.Fatalf("create regular fixture: %v", err)
	}
	if state := inspectP3ACCAcceptanceMediaPath(regular); state != p3ACCMediaPathRegular {
		t.Fatalf("regular state = %d", state)
	}
	targetDirectory := filepath.Join(root, "target")
	junction := filepath.Join(root, "junction")
	if err := os.Mkdir(targetDirectory, 0o700); err != nil {
		t.Fatalf("create junction target: %v", err)
	}
	if err := os.WriteFile(filepath.Join(targetDirectory, "escaped.mkv"), []byte("unsafe"), 0o600); err != nil {
		t.Fatalf("create junction target file: %v", err)
	}
	if err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", junction, targetDirectory).Run(); err != nil {
		t.Fatalf("create junction fixture: %v", err)
	}
	if state := inspectP3ACCAcceptanceMediaPath(filepath.Join(junction, "escaped.mkv")); state != p3ACCMediaPathUnsafe {
		t.Fatalf("reparse-parent state = %d", state)
	}
}

func TestP3ACCMediaEvidenceRejectsTerminalReparse(t *testing.T) {
	mediaRoot := t.TempDir()
	mediaDirectory := filepath.Join(mediaRoot, "media")
	if err := os.Mkdir(mediaDirectory, 0o700); err != nil {
		t.Fatalf("create media directory: %v", err)
	}
	target := t.TempDir()
	junction := filepath.Join(mediaDirectory, "segment.mkv")
	if err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", junction, target).Run(); err != nil {
		t.Fatalf("create terminal junction fixture: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("cmd.exe", "/d", "/c", "rmdir", junction).Run()
	})
	if size, digest, valid := readP3ACCMediaFileEvidence(
		context.Background(), mediaRoot, "media/segment.mkv",
	); valid || size != 0 || digest != "" {
		t.Fatalf("terminal reparse point was accepted: size=%d digest=%q", size, digest)
	}
}

func TestP3ACCMediaEvidenceRejectsParentReparseReplacement(t *testing.T) {
	mediaRoot := t.TempDir()
	mediaDirectory := filepath.Join(mediaRoot, "media")
	if err := os.Mkdir(mediaDirectory, 0o700); err != nil {
		t.Fatalf("create original media directory: %v", err)
	}
	payload := []byte("same-content-through-parent-reparse")
	filename := filepath.Join(mediaDirectory, "segment.mkv")
	if err := os.WriteFile(filename, payload, 0o600); err != nil {
		t.Fatalf("write original media file: %v", err)
	}
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "segment.mkv"), payload, 0o600); err != nil {
		t.Fatalf("write reparse target media file: %v", err)
	}
	staleDirectory := filepath.Join(mediaRoot, "media-stale")
	size, digest, valid := readP3ACCMediaFileEvidenceWithHooks(
		context.Background(), mediaRoot, "media/segment.mkv",
		func() {
			if err := os.Rename(mediaDirectory, staleDirectory); err != nil {
				t.Fatalf("rename original media directory: %v", err)
			}
			if err := exec.Command(
				"cmd.exe", "/d", "/c", "mklink", "/J", mediaDirectory, target,
			).Run(); err != nil {
				t.Fatalf("create replacement parent junction: %v", err)
			}
			t.Cleanup(func() {
				_ = exec.Command("cmd.exe", "/d", "/c", "rmdir", mediaDirectory).Run()
			})
		},
		nil,
	)
	if valid || size != 0 || digest != "" {
		t.Fatalf("parent reparse replacement was accepted: size=%d digest=%q", size, digest)
	}
}

func TestP3ACCMediaRootGuardBlocksSamePathReplacement(t *testing.T) {
	parent := t.TempDir()
	mediaRoot := filepath.Join(parent, "data")
	if err := os.Mkdir(mediaRoot, 0o700); err != nil {
		t.Fatalf("create media root: %v", err)
	}
	guard, identity, valid := openP3ACCMediaRootGuard(mediaRoot)
	if !valid {
		t.Fatal("open media root guard failed")
	}
	staleParent := parent + "-stale"
	if err := os.Rename(parent, staleParent); err == nil {
		_ = closeP3ACCMediaRootGuard(guard)
		_ = os.Rename(staleParent, parent)
		t.Fatal("held media root handle allowed an ancestor A-to-B replacement step")
	}
	staleRoot := filepath.Join(parent, "data-stale")
	renameErr := os.Rename(mediaRoot, staleRoot)
	if renameErr == nil {
		_ = closeP3ACCMediaRootGuard(guard)
		_ = os.Rename(staleRoot, mediaRoot)
		t.Fatal("held media root handle allowed the first A-to-B replacement step")
	}
	if !validateP3ACCMediaRootGuard(guard, identity) {
		_ = closeP3ACCMediaRootGuard(guard)
		t.Fatal("media root guard became invalid after blocked replacement")
	}
	if !closeP3ACCMediaRootGuard(guard) {
		t.Fatal("close media root guard failed")
	}
	if err := os.Rename(mediaRoot, staleRoot); err != nil {
		t.Fatalf("rename media root after releasing guard: %v", err)
	}
	if err := os.Rename(staleRoot, mediaRoot); err != nil {
		t.Fatalf("restore media root after releasing guard: %v", err)
	}
	if err := os.Rename(parent, staleParent); err != nil {
		t.Fatalf("rename media root ancestor after releasing guard: %v", err)
	}
	if err := os.Rename(staleParent, parent); err != nil {
		t.Fatalf("restore media root ancestor after releasing guard: %v", err)
	}
}

func TestP3ACCBoundedManifestRejectsTerminalReparse(t *testing.T) {
	dataRoot := t.TempDir()
	manifestDirectory := filepath.Join(dataRoot, "manifests")
	if err := os.Mkdir(manifestDirectory, 0o700); err != nil {
		t.Fatalf("create manifest directory: %v", err)
	}
	target := t.TempDir()
	junction := filepath.Join(manifestDirectory, "media.json")
	if err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", junction, target).Run(); err != nil {
		t.Fatalf("create terminal manifest junction: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("cmd.exe", "/d", "/c", "rmdir", junction).Run()
	})
	if payload, err := readP3ACCBoundedFile(dataRoot, junction, 1024); err == nil || payload != nil {
		t.Fatalf("terminal manifest reparse point was accepted: payload=%q", payload)
	}
}

func TestP3ACCBoundedManifestRejectsParentReparseReplacement(t *testing.T) {
	dataRoot := t.TempDir()
	manifestDirectory := filepath.Join(dataRoot, "manifests")
	if err := os.Mkdir(manifestDirectory, 0o700); err != nil {
		t.Fatalf("create original manifest directory: %v", err)
	}
	payload := []byte("same-content-through-manifest-parent-reparse")
	filename := filepath.Join(manifestDirectory, "media.json")
	if err := os.WriteFile(filename, payload, 0o600); err != nil {
		t.Fatalf("write original manifest: %v", err)
	}
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "media.json"), payload, 0o600); err != nil {
		t.Fatalf("write reparse target manifest: %v", err)
	}
	staleDirectory := filepath.Join(dataRoot, "manifests-stale")
	readPayload, err := readP3ACCBoundedFileWithHooks(
		dataRoot, filename, 1024,
		func() {
			if err := os.Rename(manifestDirectory, staleDirectory); err != nil {
				t.Fatalf("rename original manifest directory: %v", err)
			}
			if err := exec.Command(
				"cmd.exe", "/d", "/c", "mklink", "/J", manifestDirectory, target,
			).Run(); err != nil {
				t.Fatalf("create replacement manifest parent junction: %v", err)
			}
			t.Cleanup(func() {
				_ = exec.Command("cmd.exe", "/d", "/c", "rmdir", manifestDirectory).Run()
			})
		},
		nil,
	)
	if err == nil || readPayload != nil {
		t.Fatalf("parent manifest reparse replacement was accepted: payload=%q", readPayload)
	}
}

func TestP3ACCDatabaseWALSampleIsRegularBoundedAndReparseSafe(t *testing.T) {
	dataRoot := t.TempDir()
	database := filepath.Join(dataRoot, "app.db")
	wal := filepath.Join(dataRoot, "app.db-wal")
	if err := os.WriteFile(database, []byte("database"), 0o600); err != nil {
		t.Fatalf("create database fixture: %v", err)
	}
	if size, present, complete := readP3ACCDatabaseWALSample(dataRoot); size != 0 || present || !complete {
		t.Fatalf("missing WAL sample = (%d, %v, %v)", size, present, complete)
	}
	if err := os.WriteFile(wal, []byte("wal-data"), 0o600); err != nil {
		t.Fatalf("create WAL fixture: %v", err)
	}
	if size, present, complete := readP3ACCDatabaseWALSample(dataRoot); size != 8 || !present || !complete {
		t.Fatalf("regular WAL sample = (%d, %v, %v)", size, present, complete)
	}
	if err := os.Remove(wal); err != nil {
		t.Fatalf("remove WAL fixture: %v", err)
	}
	if err := os.Mkdir(wal, 0o700); err != nil {
		t.Fatalf("create directory WAL fixture: %v", err)
	}
	if _, _, complete := readP3ACCDatabaseWALSample(dataRoot); complete {
		t.Fatal("directory WAL was accepted")
	}

	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	junction := filepath.Join(parent, "data-junction")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("create target: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "app.db"), []byte("database"), 0o600); err != nil {
		t.Fatalf("create target database: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "app.db-wal"), []byte("wal"), 0o600); err != nil {
		t.Fatalf("create target WAL: %v", err)
	}
	if err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", junction, target).Run(); err != nil {
		t.Fatalf("create data junction: %v", err)
	}
	if _, _, complete := readP3ACCDatabaseWALSample(junction); complete {
		t.Fatal("reparse-parent WAL was accepted")
	}
	if _, _, complete := readP3ACCDatabaseWALSample(filepath.Join(parent, "missing")); complete {
		t.Fatal("missing database root was accepted")
	}
}
