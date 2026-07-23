package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInstallJobRoundTripAndReverifiesSignedInstaller(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	downloadDir := filepath.Join(root, "updates", "downloads", "0.2.1")
	if err := os.MkdirAll(downloadDir, 0o700); err != nil {
		t.Fatal(err)
	}
	installer := filepath.Join(downloadDir, "douyin-live-desktop-0.2.1-windows-amd64-installer.exe")
	installerBytes := []byte("verified installer")
	if err := os.WriteFile(installer, installerBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(root, "backups")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(backupDir, "app-v6-20260723T080910.123Z.db")
	if err := os.WriteFile(backup, []byte("backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(installerBytes)
	payload := testPayload()
	payload.Channel = "canary"
	payload.Version = "0.2.1"
	payload.Installer = FileDescriptor{
		ObjectKey: "releases/v0.2.1/douyin-live-desktop-0.2.1-windows-amd64-installer.exe",
		SHA256:    hex.EncodeToString(digest[:]), Size: int64(len(installerBytes)),
	}
	payload.ReleaseManifest.ObjectKey = "releases/v0.2.1/release-manifest.json"
	envelope, err := Sign(payload, "test", privateKey)
	if err != nil {
		t.Fatal(err)
	}
	health := filepath.Join(root, "updates", "health", "0123456789abcdef0123456789abcdef.json")
	job := NewInstallJob(
		123, "0.2.0", envelope, Verified{Payload: payload},
		installer, filepath.Join(t.TempDir(), "app"), root, backup, health,
		"0123456789abcdef0123456789abcdef", time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC),
	)
	jobPath := filepath.Join(root, "updates", "jobs", "job.json")
	if err := WriteInstallJob(jobPath, job); err != nil {
		t.Fatalf("WriteInstallJob() error = %v", err)
	}
	verified, err := loadAndVerifyInstallJob(
		jobPath, map[string]ed25519.PublicKey{"test": publicKey}, "https://updates.example.invalid",
	)
	if err != nil {
		t.Fatalf("loadAndVerifyInstallJobWithConfig() error = %v", err)
	}
	if verified.Job.TargetVersion != "0.2.1" || verified.Job.Channel != "canary" ||
		verified.Job.InstallerPath != installer {
		t.Fatalf("verified job = %+v", verified.Job)
	}

	if err := os.WriteFile(installer, []byte("tampered installer"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAndVerifyInstallJob(
		jobPath, map[string]ed25519.PublicKey{"test": publicKey}, "https://updates.example.invalid",
	); errorCode(err) != "UPDATE_HASH_MISMATCH" {
		t.Fatalf("tampered installer error = %v", err)
	}
}

func TestInstallJobRejectsDuplicateFieldAndEscapingPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.json")
	if err := os.WriteFile(path, []byte(`{"schema":"douyinlive-install-job/v1","schema":"duplicate"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAndVerifyInstallJob(path, nil); errorCode(err) != "UPDATE_INSTALL_JOB_INVALID" {
		t.Fatalf("duplicate-field error = %v", err)
	}

	root := filepath.Join(t.TempDir(), "root")
	job := InstallJob{
		Schema: InstallJobSchema, ParentPID: 1, CurrentVersion: "0.2.0",
		TargetVersion: "0.2.1", ExecutableName: "douyin-live-desktop.exe",
		Channel:       "stable",
		InstallerPath: filepath.Join(root, "outside.exe"),
		InstallDir:    filepath.Join(t.TempDir(), "app"), DataRoot: root,
		DatabaseBackup: filepath.Join(root, "backups", "app-v6-20260723T080910.123Z.db"),
		HealthMarker:   filepath.Join(root, "updates", "health", "marker.json"),
		HealthNonce:    "0123456789abcdef0123456789abcdef",
		CreatedAt:      time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC).Format(time.RFC3339),
	}
	if err := validateInstallJobShape(job); errorCode(err) != "UPDATE_INSTALL_JOB_PATH_INVALID" {
		t.Fatalf("escaping-path error = %v", err)
	}
	job.Channel = "preview"
	if err := validateInstallJobShape(job); errorCode(err) != "UPDATE_INSTALL_JOB_INVALID" {
		t.Fatalf("invalid-channel error = %v", err)
	}
}

func TestWriteInstallJobRejectsOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.json")
	job := InstallJob{}
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteInstallJob(path, job); errorCode(err) != "UPDATE_INSTALL_JOB_ALREADY_EXISTS" {
		t.Fatalf("WriteInstallJob overwrite error = %v", err)
	}
}
