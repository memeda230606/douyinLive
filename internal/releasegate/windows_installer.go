package releasegate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func reproducibleGoToolBuild(root, packagePath, artifact string) (string, int64, error) {
	verification := artifact + ".verify.exe"
	defer os.Remove(verification)
	build := func(target string) (string, int64, error) {
		command := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w -buildid=", "-o", target, packagePath)
		command.Dir = root
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Run(); err != nil {
			return "", 0, fmt.Errorf("build release tool %s: %w", packagePath, err)
		}
		return HashFile(target)
	}
	firstHash, firstSize, err := build(artifact)
	if err != nil {
		return "", 0, err
	}
	secondHash, secondSize, err := build(verification)
	if err != nil {
		return "", 0, err
	}
	if firstHash != secondHash || firstSize != secondSize {
		return "", 0, fmt.Errorf("release tool reproducibility mismatch: first=%s/%d second=%s/%d", firstHash, firstSize, secondHash, secondSize)
	}
	return firstHash, firstSize, nil
}

func buildWindowsInstaller(root, outDir, installer, desktop, rollback, updateHelper, webView2Bootstrapper, version string, lock FFmpegLock) (string, int64, error) {
	makensis, err := exec.LookPath("makensis")
	if err != nil {
		return "", 0, fmt.Errorf("locate NSIS compiler: %w", err)
	}
	ffmpeg, err := lockedBinaryPath("ffmpeg", lock.Binaries["ffmpeg.exe"])
	if err != nil {
		return "", 0, err
	}
	ffprobe, err := lockedBinaryPath("ffprobe", lock.Binaries["ffprobe.exe"])
	if err != nil {
		return "", 0, err
	}
	installerScript := filepath.Join(root, "cmd", "desktop", "build", "windows", "installer", "project.nsi")
	arguments := installerArguments(map[string]string{
		"ARG_WAILS_AMD64_BINARY":    desktop,
		"ARG_FFMPEG_BINARY":         ffmpeg,
		"ARG_FFPROBE_BINARY":        ffprobe,
		"ARG_WEBVIEW2_BOOTSTRAPPER": webView2Bootstrapper,
		"ARG_WEBVIEW2_LOCK":         filepath.Join(outDir, "webview2-bootstrapper-windows.lock.json"),
		"ARG_DBROLLBACK_BINARY":     rollback,
		"ARG_UPDATE_HELPER_BINARY":  updateHelper,
		"ARG_LICENSE_FILE":          filepath.Join(outDir, "LICENSE.txt"),
		"ARG_LICENSE_MANIFEST":      filepath.Join(outDir, "licenses.json"),
		"ARG_NOTICES_FILE":          filepath.Join(outDir, "THIRD-PARTY-NOTICES.txt"),
		"ARG_SBOM_FILE":             filepath.Join(outDir, "sbom.spdx.json"),
		"ARG_FFMPEG_LOCK":           filepath.Join(outDir, "ffmpeg-windows-amd64.lock.json"),
		"ARG_INSTALLATION_GUIDE":    filepath.Join(outDir, "INSTALLATION.md"),
		"ARG_USER_GUIDE":            filepath.Join(outDir, "USER-GUIDE.md"),
		"ARG_PRIVACY_GUIDE":         filepath.Join(outDir, "PRIVACY.md"),
		"ARG_LIMITATIONS_GUIDE":     filepath.Join(outDir, "KNOWN-LIMITATIONS.md"),
		"ARG_RELEASE_CHECKLIST":     filepath.Join(outDir, "RELEASE-CHECKLIST.md"),
		"ARG_INSTALLER_OUTPUT":      installer,
		"INFO_PRODUCTVERSION":       version,
	})
	arguments = append(arguments, installerScript)
	command := exec.Command(makensis, arguments...)
	command.Dir = filepath.Dir(installerScript)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return "", 0, fmt.Errorf("build NSIS installer: %w", err)
	}
	return HashFile(installer)
}

func lockedBinaryPath(name, expectedHash string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("locate %s: %w", name, err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	digest, _, err := HashFile(absolute)
	if err != nil {
		return "", err
	}
	if digest != expectedHash {
		return "", fmt.Errorf("%s checksum changed before installer build", name)
	}
	return absolute, nil
}

func installerArguments(defines map[string]string) []string {
	order := []string{
		"ARG_WAILS_AMD64_BINARY", "ARG_FFMPEG_BINARY", "ARG_FFPROBE_BINARY",
		"ARG_WEBVIEW2_BOOTSTRAPPER", "ARG_WEBVIEW2_LOCK",
		"ARG_DBROLLBACK_BINARY", "ARG_UPDATE_HELPER_BINARY", "ARG_LICENSE_FILE", "ARG_LICENSE_MANIFEST",
		"ARG_NOTICES_FILE", "ARG_SBOM_FILE", "ARG_FFMPEG_LOCK",
		"ARG_INSTALLATION_GUIDE", "ARG_INSTALLER_OUTPUT",
		"ARG_USER_GUIDE", "ARG_PRIVACY_GUIDE", "ARG_LIMITATIONS_GUIDE",
		"ARG_RELEASE_CHECKLIST",
	}
	arguments := []string{"/WX", "/INPUTCHARSET", "UTF8"}
	for _, name := range order {
		arguments = append(arguments, "-D"+name+"="+defines[name])
	}
	arguments = append(arguments,
		"-DINFO_PROJECTNAME=DouyinLiveDesktop",
		"-DINFO_COMPANYNAME=DouyinLive",
		"-DINFO_PRODUCTNAME=DouyinLive Desktop",
		"-DINFO_PRODUCTVERSION="+defines["INFO_PRODUCTVERSION"],
		"-DPRODUCT_EXECUTABLE=douyin-live-desktop.exe",
		"-DUNINST_KEY_NAME=DouyinLiveDesktop",
		"-DUPDATE_COMPAT_UNINST_KEY_NAME=DouyinLiveDouyinLiveDesktop",
	)
	return arguments
}

func refreshInstallerManifest(outDir, installer, installerHash string, installerSize int64, rollback, rollbackHash string, rollbackSize int64, updateHelper, updateHelperHash string, updateHelperSize int64) error {
	manifestPath := filepath.Join(outDir, "release-manifest.json")
	content, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var manifest map[string]any
	if err := json.Unmarshal(content, &manifest); err != nil {
		return fmt.Errorf("decode release manifest for installer: %w", err)
	}
	if manifest["schema"] != ManifestSchema {
		return errors.New("unexpected release manifest schema before installer finalization")
	}
	manifest["installer"] = map[string]any{"path": filepath.Base(installer), "sha256": installerHash, "size": installerSize, "scope": "user"}
	manifest["rollbackTool"] = map[string]any{"path": filepath.Base(rollback), "sha256": rollbackHash, "size": rollbackSize}
	manifest["updateHelper"] = map[string]any{"path": filepath.Base(updateHelper), "sha256": updateHelperHash, "size": updateHelperSize}
	files, err := manifestFiles(outDir)
	if err != nil {
		return err
	}
	manifest["files"] = files
	return writeJSON(manifestPath, manifest)
}
