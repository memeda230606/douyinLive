//go:build windows

package main

import (
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jwwsjlm/douyinLive/v2/internal/releasegate"
	"github.com/jwwsjlm/douyinLive/v2/internal/update"
)

func TestPrepareEnvelopeBindsReleaseArtifactsAndSigningKey(t *testing.T) {
	releaseDirectory, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	version := "0.2.1"
	installerName := "douyin-live-desktop-" + version + "-windows-amd64-installer.exe"
	installerPath := filepath.Join(releaseDirectory, installerName)
	if err := os.WriteFile(installerPath, []byte("installer"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, size, err := releasegate.HashFile(installerPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{
		"schema": releasegate.ManifestSchema, "product": update.Product,
		"version": version, "gitCommit": strings.Repeat("a", 40),
		"platform": update.Platform, "dirty": false,
		"installer": map[string]any{
			"path": installerName, "sha256": digest, "size": size, "scope": "user",
		},
	}
	manifestContent, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(releaseDirectory, "release-manifest.json"), manifestContent, 0o600); err != nil {
		t.Fatal(err)
	}
	notesPath := filepath.Join(releaseDirectory, "notes.txt")
	if err := os.WriteFile(notesPath, []byte("安全更新"), 0o600); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(releaseDirectory, "signing-key.json")
	publicHex, err := update.CreateProtectedSigningKey(keyPath, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	envelope, identity, err := prepareEnvelope(options{
		releaseDirectory: releaseDirectory, version: version, channel: "canary",
		notesFile: notesPath, signingKey: keyPath,
		bucket: defaultBucket, region: defaultRegion,
	}, now)
	if err != nil {
		t.Fatalf("prepareEnvelope() error = %v", err)
	}
	publicKey, err := update.DecodePublicKey(publicHex)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := update.VerifyEnvelope(
		envelope, map[string]ed25519.PublicKey{"test-key": publicKey},
		"0.2.0", "", "canary", update.ProductionBaseURL,
	)
	if err != nil {
		t.Fatalf("VerifyEnvelope() error = %v", err)
	}
	if identity.Version != version || verified.Payload.Installer.SHA256 != digest ||
		verified.Payload.PublishedAt != now.Format(time.RFC3339) ||
		verified.Payload.DatabaseSchemaVersion != 6 {
		t.Fatalf("verified payload = %+v", verified.Payload)
	}

	if err := os.WriteFile(installerPath, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepareEnvelope(options{
		releaseDirectory: releaseDirectory, version: version, channel: "canary",
		notesFile: notesPath, signingKey: keyPath,
		bucket: defaultBucket, region: defaultRegion,
	}, now); update.ErrorCode(err) != "UPDATE_HASH_MISMATCH" {
		t.Fatalf("tampered installer error = %v", err)
	}
}
