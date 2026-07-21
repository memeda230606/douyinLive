//go:build p3accacceptance && windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func writeP3ACCWebviewSentinel(t *testing.T, root string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, p3ACCAcceptanceSentinelName), []byte(p3ACCAcceptanceSentinelContent), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
}

func TestPrepareP3ACCWebviewUserDataPath(t *testing.T) {
	root := t.TempDir()
	writeP3ACCWebviewSentinel(t, root)
	candidate := filepath.Join(root, p3ACCWebviewUserDataDirectory)
	if err := prepareP3ACCWebviewUserDataPath(root, candidate); err != nil {
		t.Fatalf("prepare valid path: %v", err)
	}
	info, err := os.Lstat(candidate)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("prepared path is not a regular directory")
	}
	if err := prepareP3ACCWebviewUserDataPath(root, candidate); err == nil {
		t.Fatal("accepted an existing user data directory")
	}
}

func TestPrepareP3ACCWebviewUserDataPathRejectsInvalidRootsAndEscapes(t *testing.T) {
	parent := t.TempDir()
	missing := filepath.Join(parent, "missing")
	if err := prepareP3ACCWebviewUserDataPath(missing, filepath.Join(missing, p3ACCWebviewUserDataDirectory)); err == nil {
		t.Fatal("accepted a missing root")
	}
	if err := prepareP3ACCWebviewUserDataPath("relative", filepath.Join("relative", p3ACCWebviewUserDataDirectory)); err == nil {
		t.Fatal("accepted a relative root")
	}
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatalf("create root: %v", err)
	}
	writeP3ACCWebviewSentinel(t, root)
	if err := prepareP3ACCWebviewUserDataPath(root, filepath.Join(parent, "outside")); err == nil {
		t.Fatal("accepted a root escape")
	}
}

func TestPrepareP3ACCWebviewUserDataPathRejectsReparseRoot(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	junction := filepath.Join(parent, "junction")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("create target: %v", err)
	}
	writeP3ACCWebviewSentinel(t, target)
	if err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", junction, target).Run(); err != nil {
		t.Fatalf("create junction: %v", err)
	}
	if err := prepareP3ACCWebviewUserDataPath(junction, filepath.Join(junction, p3ACCWebviewUserDataDirectory)); err == nil {
		t.Fatal("accepted a reparse root")
	}
}
