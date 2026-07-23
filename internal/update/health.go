package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	healthFileEnvironment    = "DOUYINLIVE_UPDATE_HEALTH_FILE"
	healthNonceEnvironment   = "DOUYINLIVE_UPDATE_HEALTH_NONCE"
	healthVersionEnvironment = "DOUYINLIVE_UPDATE_TARGET_VERSION"
)

// WriteStartupHealthMarker acknowledges an updater-launched build only after
// desktop infrastructure initialization has succeeded.
func WriteStartupHealthMarker(dataRoot, currentVersion string) error {
	path := os.Getenv(healthFileEnvironment)
	nonce := os.Getenv(healthNonceEnvironment)
	targetVersion := os.Getenv(healthVersionEnvironment)
	if path == "" && nonce == "" && targetVersion == "" {
		return nil
	}
	if path == "" || nonce == "" || targetVersion == "" ||
		!healthNoncePattern.MatchString(nonce) ||
		!ValidVersion(currentVersion) || targetVersion != currentVersion {
		return errors.New("UPDATE_HEALTH_ENVIRONMENT_INVALID")
	}
	absoluteRoot, err := filepath.Abs(filepath.Clean(dataRoot))
	if err != nil || absoluteRoot != dataRoot {
		return errors.New("UPDATE_HEALTH_PATH_INVALID")
	}
	absolutePath, err := filepath.Abs(filepath.Clean(path))
	if err != nil || absolutePath != path ||
		!isDirectChild(absolutePath, filepath.Join(absoluteRoot, "updates", "health")) {
		return errors.New("UPDATE_HEALTH_PATH_INVALID")
	}
	content, err := json.Marshal(HealthMarker{
		Schema: HealthMarkerSchema, Version: currentVersion, Nonce: nonce,
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o700); err != nil {
		return fmt.Errorf("UPDATE_HEALTH_WRITE_FAILED: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(absolutePath), ".health-*.tmp")
	if err != nil {
		return fmt.Errorf("UPDATE_HEALTH_WRITE_FAILED: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("UPDATE_HEALTH_WRITE_FAILED: %w", err)
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return fmt.Errorf("UPDATE_HEALTH_WRITE_FAILED: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("UPDATE_HEALTH_WRITE_FAILED: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("UPDATE_HEALTH_WRITE_FAILED: %w", err)
	}
	_ = os.Remove(absolutePath)
	if err := os.Rename(temporaryPath, absolutePath); err != nil {
		return fmt.Errorf("UPDATE_HEALTH_WRITE_FAILED: %w", err)
	}
	return nil
}
