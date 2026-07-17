// Package storage owns the desktop data-root layout and SQLite lifecycle.
package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const ProductDirectory = "DouyinLive"

type Layout struct {
	Root       string
	Database   string
	ConfigDir  string
	RoomsDir   string
	LogsDir    string
	CacheDir   string
	ExportsDir string
	BackupsDir string
}

func DefaultRoot() (string, error) {
	localData, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve local application data directory: %w", err)
	}
	if localData == "" {
		return "", errors.New("local application data directory is empty")
	}
	return filepath.Join(localData, ProductDirectory), nil
}

// PrepareLayout creates the fixed data-root structure and verifies that files
// can be created and atomically renamed on the selected volume.
func PrepareLayout(root string) (Layout, error) {
	if root == "" {
		var err error
		root, err = DefaultRoot()
		if err != nil {
			return Layout{}, err
		}
	}

	absoluteRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return Layout{}, fmt.Errorf("resolve data root: %w", err)
	}

	layout := Layout{
		Root:       absoluteRoot,
		Database:   filepath.Join(absoluteRoot, "app.db"),
		ConfigDir:  filepath.Join(absoluteRoot, "config"),
		RoomsDir:   filepath.Join(absoluteRoot, "rooms"),
		LogsDir:    filepath.Join(absoluteRoot, "logs"),
		CacheDir:   filepath.Join(absoluteRoot, "cache"),
		ExportsDir: filepath.Join(absoluteRoot, "exports"),
		BackupsDir: filepath.Join(absoluteRoot, "backups"),
	}

	directories := []string{
		layout.Root,
		layout.ConfigDir,
		layout.RoomsDir,
		layout.LogsDir,
		layout.CacheDir,
		layout.ExportsDir,
		layout.BackupsDir,
	}
	for _, directory := range directories {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return Layout{}, fmt.Errorf("create data directory: %w", err)
		}
	}
	if err := verifyAtomicRename(layout.Root); err != nil {
		return Layout{}, err
	}
	return layout, nil
}

func verifyAtomicRename(root string) error {
	probe, err := os.CreateTemp(root, ".write-probe-*")
	if err != nil {
		return fmt.Errorf("create data-root write probe: %w", err)
	}
	probePath := probe.Name()
	renamedPath := probePath + ".renamed"
	defer os.Remove(probePath)
	defer os.Remove(renamedPath)

	if err := probe.Close(); err != nil {
		return fmt.Errorf("close data-root write probe: %w", err)
	}
	if err := os.Rename(probePath, renamedPath); err != nil {
		return fmt.Errorf("atomically rename data-root write probe: %w", err)
	}
	if err := os.Remove(renamedPath); err != nil {
		return fmt.Errorf("remove data-root write probe: %w", err)
	}
	return nil
}
