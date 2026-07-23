package update

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const InstallJobSchema = "douyinlive-install-job/v1"

var healthNoncePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

type InstallJob struct {
	Schema         string `json:"schema"`
	ParentPID      int    `json:"parentPid"`
	CurrentVersion string `json:"currentVersion"`
	TargetVersion  string `json:"targetVersion"`
	Channel        string `json:"channel"`
	Envelope       string `json:"envelope"`
	InstallerPath  string `json:"installerPath"`
	InstallDir     string `json:"installDir"`
	ExecutableName string `json:"executableName"`
	DataRoot       string `json:"dataRoot"`
	DatabaseBackup string `json:"databaseBackup"`
	HealthMarker   string `json:"healthMarker"`
	HealthNonce    string `json:"healthNonce"`
	CreatedAt      string `json:"createdAt"`
}

type VerifiedInstallJob struct {
	Job      InstallJob
	Envelope []byte
	Update   Verified
}

func NewInstallJob(parentPID int, currentVersion string, envelope []byte, verified Verified,
	installerPath, installDir, dataRoot, databaseBackup, healthMarker, healthNonce string,
	now time.Time,
) InstallJob {
	return InstallJob{
		Schema: InstallJobSchema, ParentPID: parentPID,
		CurrentVersion: currentVersion, TargetVersion: verified.Payload.Version,
		Channel:       verified.Payload.Channel,
		Envelope:      base64.StdEncoding.EncodeToString(envelope),
		InstallerPath: installerPath, InstallDir: installDir,
		ExecutableName: "douyin-live-desktop.exe",
		DataRoot:       dataRoot, DatabaseBackup: databaseBackup,
		HealthMarker: healthMarker, HealthNonce: healthNonce,
		CreatedAt: now.UTC().Format(time.RFC3339),
	}
}

func WriteInstallJob(path string, job InstallJob) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("UPDATE_INSTALL_JOB_PATH_INVALID")
	}
	content, err := json.Marshal(job)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("UPDATE_INSTALL_JOB_WRITE_FAILED: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".install-job-*.tmp")
	if err != nil {
		return fmt.Errorf("UPDATE_INSTALL_JOB_WRITE_FAILED: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("UPDATE_INSTALL_JOB_WRITE_FAILED: %w", err)
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return fmt.Errorf("UPDATE_INSTALL_JOB_WRITE_FAILED: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("UPDATE_INSTALL_JOB_WRITE_FAILED: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("UPDATE_INSTALL_JOB_WRITE_FAILED: %w", err)
	}
	if _, err := os.Lstat(path); err == nil {
		return errors.New("UPDATE_INSTALL_JOB_ALREADY_EXISTS")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("UPDATE_INSTALL_JOB_WRITE_FAILED: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("UPDATE_INSTALL_JOB_WRITE_FAILED: %w", err)
	}
	return nil
}

func LoadAndVerifyInstallJob(path string, trusted map[string][]byte) (VerifiedInstallJob, error) {
	keys := make(map[string]ed25519.PublicKey, len(trusted))
	for keyID, raw := range trusted {
		keys[keyID] = ed25519.PublicKey(raw)
	}
	return loadAndVerifyInstallJob(path, keys, ProductionBaseURL)
}

func loadAndVerifyInstallJob(path string, trusted map[string]ed25519.PublicKey, baseURL string) (VerifiedInstallJob, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return VerifiedInstallJob{}, fmt.Errorf("UPDATE_INSTALL_JOB_READ_FAILED: %w", err)
	}
	if len(content) == 0 || len(content) > MaxEnvelope*2 {
		return VerifiedInstallJob{}, errors.New("UPDATE_INSTALL_JOB_INVALID")
	}
	var job InstallJob
	if err := DecodeStrictJSON(content, &job); err != nil {
		return VerifiedInstallJob{}, fmt.Errorf("UPDATE_INSTALL_JOB_INVALID: %w", err)
	}
	if err := validateInstallJobShape(job); err != nil {
		return VerifiedInstallJob{}, err
	}
	envelope, err := base64.StdEncoding.DecodeString(job.Envelope)
	if err != nil || len(envelope) == 0 || len(envelope) > MaxEnvelope {
		return VerifiedInstallJob{}, errors.New("UPDATE_INSTALL_JOB_INVALID")
	}
	verified, err := VerifyEnvelope(
		envelope, trusted, job.CurrentVersion, "", job.Channel, baseURL,
	)
	if err != nil {
		return VerifiedInstallJob{}, fmt.Errorf("UPDATE_INSTALL_JOB_SIGNATURE_INVALID: %w", err)
	}
	if verified.Payload.Version != job.TargetVersion {
		return VerifiedInstallJob{}, errors.New("UPDATE_INSTALL_JOB_VERSION_MISMATCH")
	}
	digest, size, err := hashFile(job.InstallerPath)
	if err != nil {
		return VerifiedInstallJob{}, fmt.Errorf("UPDATE_INSTALLER_READ_FAILED: %w", err)
	}
	if size != verified.Payload.Installer.Size || digest != verified.Payload.Installer.SHA256 {
		return VerifiedInstallJob{}, errors.New("UPDATE_HASH_MISMATCH")
	}
	return VerifiedInstallJob{Job: job, Envelope: envelope, Update: verified}, nil
}

func ProductionTrustedKeyBytes() (map[string][]byte, error) {
	keys, err := ProductionTrustedKeys()
	if err != nil {
		return nil, err
	}
	result := make(map[string][]byte, len(keys))
	for keyID, publicKey := range keys {
		result[keyID] = append([]byte(nil), publicKey...)
	}
	return result, nil
}

func validateInstallJobShape(job InstallJob) error {
	if job.Schema != InstallJobSchema || job.ParentPID <= 0 ||
		!ValidVersion(job.CurrentVersion) || !ValidVersion(job.TargetVersion) ||
		CompareVersions(job.TargetVersion, job.CurrentVersion) <= 0 ||
		job.ExecutableName != "douyin-live-desktop.exe" ||
		(job.Channel != "stable" && job.Channel != "canary") ||
		!healthNoncePattern.MatchString(job.HealthNonce) {
		return errors.New("UPDATE_INSTALL_JOB_INVALID")
	}
	createdAt, err := time.Parse(time.RFC3339, job.CreatedAt)
	if err != nil || job.CreatedAt != createdAt.Format(time.RFC3339) {
		return errors.New("UPDATE_INSTALL_JOB_INVALID")
	}
	paths := []*string{
		&job.InstallerPath, &job.InstallDir, &job.DataRoot,
		&job.DatabaseBackup, &job.HealthMarker,
	}
	for _, pathValue := range paths {
		absolute, err := filepath.Abs(filepath.Clean(*pathValue))
		if err != nil || absolute != *pathValue || !filepath.IsAbs(*pathValue) {
			return errors.New("UPDATE_INSTALL_JOB_PATH_INVALID")
		}
	}
	if !isDescendant(job.InstallerPath, filepath.Join(job.DataRoot, "updates", "downloads")) ||
		!isDirectChild(job.DatabaseBackup, filepath.Join(job.DataRoot, "backups")) ||
		!isDirectChild(job.HealthMarker, filepath.Join(job.DataRoot, "updates", "health")) {
		return errors.New("UPDATE_INSTALL_JOB_PATH_INVALID")
	}
	if strings.Contains(strings.ToLower(filepath.Base(job.DatabaseBackup)), "..") ||
		!strings.HasPrefix(filepath.Base(job.DatabaseBackup), "app-v") ||
		!strings.HasSuffix(filepath.Base(job.DatabaseBackup), ".db") {
		return errors.New("UPDATE_INSTALL_JOB_PATH_INVALID")
	}
	for _, filePath := range []string{job.InstallerPath, job.DatabaseBackup} {
		info, err := os.Lstat(filePath)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("UPDATE_INSTALL_JOB_FILE_INVALID")
		}
	}
	return nil
}

func isDescendant(candidate, parent string) bool {
	relative, err := filepath.Rel(parent, candidate)
	return err == nil && relative != "." && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func isDirectChild(candidate, parent string) bool {
	relative, err := filepath.Rel(parent, candidate)
	return err == nil && relative != "." && filepath.Dir(relative) == "."
}
