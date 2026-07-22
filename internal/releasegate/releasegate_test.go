package releasegate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateVersionRequiresAlignedExactPins(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "cmd", "desktop"))
	mustMkdir(t, filepath.Join(root, "frontend"))
	mustWrite(t, filepath.Join(root, "cmd", "desktop", "wails.json"), `{"info":{"productVersion":"0.1.0"}}`)
	mustWrite(t, filepath.Join(root, "frontend", "package.json"), `{"version":"0.1.0","packageManager":"pnpm@9.12.3"}`)
	if err := ValidateVersion(root, "0.1.0"); err != nil {
		t.Fatalf("ValidateVersion() error = %v", err)
	}
	for _, version := range []string{"v0.1.0", "0.1", "0.1.1"} {
		if err := ValidateVersion(root, version); err == nil {
			t.Fatalf("ValidateVersion(%q) error = nil", version)
		}
	}
}

func TestScanContentRejectsSecretsWithoutReturningValues(t *testing.T) {
	token := "ghp_" + strings.Repeat("A", 32)
	key := "AKIA" + strings.Repeat("B", 16)
	content := []byte("safe\n" + token + "\n" + key + "\nCookie=example-placeholder\n")
	findings := scanContent("fixture.txt", content)
	if len(findings) != 2 {
		t.Fatalf("findings = %#v, want 2", findings)
	}
	encoded := strings.ToLower(findings[0].Rule + findings[1].Rule)
	if strings.Contains(encoded, strings.ToLower(token)) || strings.Contains(encoded, strings.ToLower(key)) {
		t.Fatal("finding exposed a secret value")
	}
}

func TestLoadFFmpegLockFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock.json")
	mustWrite(t, path, `{
  "schemaVersion": 1,
  "distribution": "Gyan release essentials",
  "version": "8.1.2-essentials_build-www.gyan.dev",
  "archive": {"url": "https://www.gyan.dev/ffmpeg/builds/packages/ffmpeg-8.1.2-essentials_build.zip", "sha256": "`+strings.Repeat("a", 64)+`"},
  "license": "GPL-3.0-or-later",
  "sourceCommit": "38b88335f9",
  "binaries": {"ffmpeg.exe": "`+strings.Repeat("b", 64)+`", "ffprobe.exe": "`+strings.Repeat("c", 64)+`"}
}`)
	lock, err := LoadFFmpegLock(path)
	if err != nil || lock.Version == "" {
		t.Fatalf("LoadFFmpegLock() = %#v, %v", lock, err)
	}
	mustWrite(t, path, strings.ReplaceAll(string(mustRead(t, path)), strings.Repeat("b", 64), "bad"))
	if _, err := LoadFFmpegLock(path); err == nil {
		t.Fatal("LoadFFmpegLock() accepted an invalid binary checksum")
	}
}

func TestLicenseDetectionAndPathContainment(t *testing.T) {
	if got := detectLicense("Permission is hereby granted, free of charge, to any person"); got != "MIT" {
		t.Fatalf("detectLicense() = %q", got)
	}
	root := t.TempDir()
	if !isWithin(root, filepath.Join(root, "release", "v0.1.0")) {
		t.Fatal("valid child rejected")
	}
	if isWithin(root, root) || isWithin(root, filepath.Dir(root)) {
		t.Fatal("broad path accepted")
	}
}

func TestLicenseEvidencePreservesMissingUpstreamAsNoAssertion(t *testing.T) {
	license, filename, digest, text, err := licenseEvidence(t.TempDir(), "")
	if err != nil {
		t.Fatalf("licenseEvidence() error = %v", err)
	}
	if license != "NOASSERTION" || filename != "" || digest != "" || text != "" {
		t.Fatalf("licenseEvidence() = %q, %q, %q, %q", license, filename, digest, text)
	}
}

func TestWriteJSONIsStableLFAndNoHTMLRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	value := map[string]string{"value": "a<b"}
	if err := writeJSON(path, value); err != nil {
		t.Fatal(err)
	}
	first := mustRead(t, path)
	if strings.Contains(string(first), "\\u003c") || strings.Contains(string(first), "\r\n") || !strings.HasSuffix(string(first), "\n") {
		t.Fatalf("unexpected JSON encoding %q", first)
	}
	if err := writeJSON(path, value); err != nil {
		t.Fatal(err)
	}
	if second := mustRead(t, path); string(first) != string(second) {
		t.Fatal("JSON output changed across identical writes")
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}
