package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
)

const applicationInstanceLeaseNamePrefix = `Global\DouyinLive.Infrastructure.`

var (
	ErrApplicationInstanceActive = errors.New("APPLICATION_INSTANCE_ACTIVE")
	ErrApplicationInstanceLease  = errors.New("APPLICATION_INSTANCE_LEASE_FAILED")
)

type applicationInstanceLease interface {
	Close() error
}

func applicationInstanceLeaseName(dataRoot string) (string, error) {
	normalized, err := normalizeApplicationInstanceDataRoot(dataRoot)
	if err != nil {
		return "", ErrApplicationInstanceLease
	}
	digest := sha256.Sum256([]byte(normalized))
	return applicationInstanceLeaseNamePrefix + hex.EncodeToString(digest[:]), nil
}

func normalizeApplicationInstanceDataRoot(dataRoot string) (string, error) {
	if dataRoot == "" {
		return "", ErrApplicationInstanceLease
	}
	absoluteRoot, err := filepath.Abs(filepath.Clean(dataRoot))
	if err != nil {
		return "", ErrApplicationInstanceLease
	}
	canonicalRoot, err := filepath.EvalSymlinks(absoluteRoot)
	if err != nil {
		return "", ErrApplicationInstanceLease
	}
	canonicalRoot = filepath.Clean(canonicalRoot)
	if runtime.GOOS == "windows" {
		canonicalRoot = strings.ToLower(canonicalRoot)
	}
	return filepath.ToSlash(canonicalRoot), nil
}
