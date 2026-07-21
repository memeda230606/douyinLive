//go:build p3accacceptance && !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestP3ACCNonWindowsRegularFileOpenPreservesMissing(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "missing")
	file, err := openP3ACCAcceptanceRegularFile(filename)
	if file != nil {
		_ = file.Close()
		t.Fatal("missing file returned a handle")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing error = %v", err)
	}
}
