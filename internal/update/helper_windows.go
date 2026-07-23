//go:build windows

package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jwwsjlm/douyinLive/v2/internal/storage"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	HealthMarkerSchema                 = "douyinlive-update-health/v1"
	InstallResultSchema                = "douyinlive-install-result/v1"
	parentExitTimeout                  = 60 * time.Second
	healthTimeout                      = 90 * time.Second
	productionUninstallRegistryKeyPath = `Software\Microsoft\Windows\CurrentVersion\Uninstall\DouyinLiveDesktop`
)

type HealthMarker struct {
	Schema  string `json:"schema"`
	Version string `json:"version"`
	Nonce   string `json:"nonce"`
}

type InstallResult struct {
	Schema       string `json:"schema"`
	Success      bool   `json:"success"`
	Version      string `json:"version"`
	ErrorCode    string `json:"errorCode,omitempty"`
	Instructions string `json:"instructions,omitempty"`
	FinishedAt   string `json:"finishedAt"`
}

func RunInstallHelper(jobPath string) error {
	keys, err := ProductionTrustedKeyBytes()
	if err != nil {
		return err
	}
	verified, err := LoadAndVerifyInstallJob(jobPath, keys)
	if err != nil {
		return err
	}
	job := verified.Job
	fail := func(code string, cause error, instructions string) error {
		_ = writeInstallResult(jobPath, InstallResult{
			Schema: InstallResultSchema, Success: false, Version: job.TargetVersion,
			ErrorCode: code, Instructions: instructions, FinishedAt: time.Now().UTC().Format(time.RFC3339),
		})
		return fmt.Errorf("%s: %w", code, cause)
	}
	if err := validateHelperRuntime(job); err != nil {
		return fail("UPDATE_HELPER_PATH_INVALID", err, "")
	}
	if err := waitForProcessExit(uint32(job.ParentPID), parentExitTimeout); err != nil {
		return fail("UPDATE_PARENT_EXIT_TIMEOUT", err, "请关闭应用后重试安装。")
	}
	if err := reverifyPreparedInstaller(verified); err != nil {
		return fail(errorCode(err), err, "")
	}

	programBackup := filepath.Join(
		filepath.Dir(job.InstallDir),
		"."+filepath.Base(job.InstallDir)+".update-backup-"+job.HealthNonce,
	)
	if _, err := os.Lstat(programBackup); err == nil {
		return fail("UPDATE_PROGRAM_BACKUP_EXISTS", errors.New("program backup already exists"), "")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail("UPDATE_PROGRAM_BACKUP_INSPECT_FAILED", err, "")
	}
	if err := os.Rename(job.InstallDir, programBackup); err != nil {
		return fail("UPDATE_PROGRAM_BACKUP_FAILED", err, "请确认没有其他实例正在运行。")
	}
	rollback := func(code string, cause error) error {
		if err := rollbackProgramDirectory(job.InstallDir, programBackup); err != nil {
			return fail("UPDATE_PROGRAM_ROLLBACK_FAILED", errors.Join(cause, err),
				"旧程序目录仍保留在更新备份目录，请勿删除并联系维护者。")
		}
		layout := storage.Layout{
			Root: job.DataRoot, Database: filepath.Join(job.DataRoot, "app.db"),
			BackupsDir: filepath.Join(job.DataRoot, "backups"),
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, databaseErr := storage.RestoreBackup(ctx, layout, job.DatabaseBackup, time.Now())
		cancel()
		if databaseErr != nil {
			return fail("UPDATE_DATABASE_ROLLBACK_FAILED", errors.Join(cause, databaseErr),
				"旧程序已恢复，但数据库未能安全回滚。所有备份均已保留，请勿启动旧版本并联系维护者。")
		}
		if err := startApplication(filepath.Join(job.InstallDir, job.ExecutableName), nil); err != nil {
			return fail("UPDATE_OLD_VERSION_RESTART_FAILED", errors.Join(cause, err),
				"程序和数据库已恢复，请手动启动旧版本。")
		}
		return fail(code, cause, "更新失败，已恢复并重新启动旧版本。")
	}

	installer := exec.Command(job.InstallerPath, "/S")
	installer.Stdout = nil
	installer.Stderr = nil
	if err := installer.Run(); err != nil {
		return rollback("UPDATE_INSTALLER_FAILED", err)
	}
	if err := verifyInstalledIdentity(job); err != nil {
		return rollback(errorCode(err), err)
	}
	_ = os.Remove(job.HealthMarker)
	environment := append(os.Environ(),
		"DOUYINLIVE_UPDATE_HEALTH_FILE="+job.HealthMarker,
		"DOUYINLIVE_UPDATE_HEALTH_NONCE="+job.HealthNonce,
		"DOUYINLIVE_UPDATE_TARGET_VERSION="+job.TargetVersion,
	)
	command, done, err := launchApplication(
		filepath.Join(job.InstallDir, job.ExecutableName), environment,
	)
	if err != nil {
		return rollback("UPDATE_NEW_VERSION_START_FAILED", err)
	}
	if err := waitForHealthMarker(job, command, done, healthTimeout); err != nil {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
		return rollback(errorCode(err), err)
	}
	if err := os.RemoveAll(programBackup); err != nil {
		return fail("UPDATE_PROGRAM_BACKUP_CLEANUP_FAILED", err,
			"新版本已启动，但旧程序备份未能删除，可在退出应用后人工清理。")
	}
	result := InstallResult{
		Schema: InstallResultSchema, Success: true, Version: job.TargetVersion,
		FinishedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeInstallResult(jobPath, result); err != nil {
		return fmt.Errorf("UPDATE_RESULT_WRITE_FAILED: %w", err)
	}
	_ = os.Remove(jobPath)
	return nil
}

func validateHelperRuntime(job InstallJob) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return err
	}
	if isDescendant(executable, job.InstallDir) ||
		strings.EqualFold(filepath.Clean(executable), filepath.Join(job.InstallDir, "douyin-live-updater.exe")) {
		return errors.New("update helper must run outside the installation directory")
	}
	info, err := os.Lstat(job.InstallDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("installation directory is not a normal directory")
	}
	if filepath.VolumeName(job.InstallDir) != filepath.VolumeName(filepath.Dir(job.InstallDir)) {
		return errors.New("program backup must stay on the installation volume")
	}
	return nil
}

func reverifyPreparedInstaller(verified VerifiedInstallJob) error {
	digest, size, err := hashFile(verified.Job.InstallerPath)
	if err != nil {
		return fmt.Errorf("UPDATE_INSTALLER_READ_FAILED: %w", err)
	}
	if digest != verified.Update.Payload.Installer.SHA256 ||
		size != verified.Update.Payload.Installer.Size {
		_ = os.Remove(verified.Job.InstallerPath)
		return errors.New("UPDATE_HASH_MISMATCH")
	}
	return nil
}

func waitForProcessExit(processID uint32, timeout time.Duration) error {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, processID)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return nil
	}
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	milliseconds := uint32(timeout / time.Millisecond)
	result, err := windows.WaitForSingleObject(handle, milliseconds)
	if err != nil {
		return err
	}
	switch result {
	case windows.WAIT_OBJECT_0:
		return nil
	case uint32(windows.WAIT_TIMEOUT):
		return errors.New("parent process is still running")
	default:
		return fmt.Errorf("unexpected wait result %d", result)
	}
}

func verifyInstalledIdentity(job InstallJob) error {
	executable := filepath.Join(job.InstallDir, job.ExecutableName)
	info, err := os.Lstat(executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("UPDATE_INSTALLED_EXE_INVALID")
	}
	var key registry.Key
	for _, access := range []uint32{
		registry.QUERY_VALUE | registry.WOW64_64KEY,
		registry.QUERY_VALUE | registry.WOW64_32KEY,
	} {
		key, err = registry.OpenKey(registry.CURRENT_USER, productionUninstallRegistryKeyPath, access)
		if err == nil {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("UPDATE_REGISTRY_NOT_CONVERGED: %w", err)
	}
	defer key.Close()
	version, _, versionErr := key.GetStringValue("DisplayVersion")
	location, _, locationErr := key.GetStringValue("InstallLocation")
	if versionErr != nil || locationErr != nil || version != job.TargetVersion ||
		!strings.EqualFold(filepath.Clean(location), filepath.Clean(job.InstallDir)) {
		return errors.New("UPDATE_REGISTRY_NOT_CONVERGED")
	}
	return nil
}

func launchApplication(executable string, environment []string) (*exec.Cmd, <-chan error, error) {
	command := exec.Command(executable)
	command.Env = environment
	if err := command.Start(); err != nil {
		return nil, nil, err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	return command, done, nil
}

func startApplication(executable string, environment []string) error {
	command := exec.Command(executable)
	if environment != nil {
		command.Env = environment
	}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

func waitForHealthMarker(job InstallJob, command *exec.Cmd, done <-chan error, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case processErr := <-done:
			return fmt.Errorf("UPDATE_HEALTH_PROCESS_EXITED: %w", processErr)
		case <-timer.C:
			return errors.New("UPDATE_HEALTH_TIMEOUT")
		case <-ticker.C:
			content, err := os.ReadFile(job.HealthMarker)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil || len(content) == 0 || len(content) > 4096 {
				return errors.New("UPDATE_HEALTH_MARKER_INVALID")
			}
			var marker HealthMarker
			if err := DecodeStrictJSON(content, &marker); err != nil ||
				marker.Schema != HealthMarkerSchema ||
				marker.Version != job.TargetVersion || marker.Nonce != job.HealthNonce {
				return errors.New("UPDATE_HEALTH_MARKER_INVALID")
			}
			return nil
		}
	}
}

func rollbackProgramDirectory(installDir, programBackup string) error {
	if info, err := os.Lstat(installDir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("new installation path is unsafe")
		}
		if err := os.RemoveAll(installDir); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(programBackup, installDir)
}

func writeInstallResult(jobPath string, result InstallResult) error {
	content, err := json.Marshal(result)
	if err != nil {
		return err
	}
	path := jobPath + ".result.json"
	temporary, err := os.CreateTemp(filepath.Dir(path), ".install-result-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	_ = os.Remove(path)
	return os.Rename(temporaryPath, path)
}
