//go:build p4accacceptance

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	application "github.com/jwwsjlm/douyinLive/v2/internal/app"
)

func desktopInfrastructureOptions() (application.InfrastructureOptions, error) {
	value := strings.TrimSpace(os.Getenv("P4ACC_ROOT"))
	if value == "" {
		return application.InfrastructureOptions{}, errors.New("P4ACC_CONFIG_INVALID")
	}
	root, err := filepath.Abs(filepath.Clean(value))
	if err != nil || !filepath.IsAbs(root) {
		return application.InfrastructureOptions{}, errors.New("P4ACC_CONFIG_INVALID")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return application.InfrastructureOptions{}, errors.New("P4ACC_CONFIG_INVALID")
	}
	return application.InfrastructureOptions{
		DataRoot: root, DisableDiagnostics: true,
	}, nil
}
