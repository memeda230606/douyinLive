//go:build p3accacceptance && windows

package main

import (
	"errors"
	"os"
	"path/filepath"

	windowsoptions "github.com/wailsapp/wails/v2/pkg/options/windows"
)

const p3ACCWebviewUserDataDirectory = "webview2"

func desktopWindowsOptions() (*windowsoptions.Options, error) {
	paths, err := peekP3ACCBootstrapPaths()
	if err != nil {
		clearP3ACCBootstrap()
		return nil, errors.New("P3ACC_CONFIG_INVALID")
	}
	userDataPath := filepath.Join(paths.Root, p3ACCWebviewUserDataDirectory)
	if err := prepareP3ACCWebviewUserDataPath(paths.Root, userDataPath); err != nil {
		clearP3ACCBootstrap()
		return nil, errors.New("P3ACC_CONFIG_INVALID")
	}
	return &windowsoptions.Options{WebviewUserDataPath: userDataPath}, nil
}

func prepareP3ACCWebviewUserDataPath(root, candidate string) error {
	cleanRoot := filepath.Clean(root)
	cleanCandidate := filepath.Clean(candidate)
	if err := validateP3ACCAcceptanceRoot(cleanRoot); err != nil ||
		!p3ACCAcceptancePathWithin(cleanRoot, cleanCandidate, false) ||
		filepath.Dir(cleanCandidate) != cleanRoot {
		return errors.New("P3ACC_CONFIG_INVALID")
	}
	if _, err := os.Lstat(cleanCandidate); !errors.Is(err, os.ErrNotExist) {
		return errors.New("P3ACC_CONFIG_INVALID")
	}
	if err := os.Mkdir(cleanCandidate, 0o700); err != nil {
		return errors.New("P3ACC_CONFIG_INVALID")
	}
	if err := p3ACCValidateNoReparsePath(cleanCandidate, true); err != nil {
		return errors.New("P3ACC_CONFIG_INVALID")
	}
	return nil
}
