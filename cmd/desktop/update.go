package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jwwsjlm/douyinLive/v2/internal/room"
	"github.com/jwwsjlm/douyinLive/v2/internal/update"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	firstAutomaticUpdateCheck = 30 * time.Second
	automaticUpdateInterval   = 6 * time.Hour
	automaticUpdateJitter     = 30 * time.Minute
)

func (a *DesktopApp) initializeUpdater(parent context.Context) error {
	if a == nil {
		return errors.New("UPDATE_DESKTOP_UNAVAILABLE")
	}
	settingsService := a.application.SettingsService()
	if settingsService == nil {
		return errors.New("UPDATE_SETTINGS_UNAVAILABLE")
	}
	currentSettings, err := settingsService.GetSettings(parent)
	if err != nil {
		return fmt.Errorf("UPDATE_SETTINGS_UNAVAILABLE: %w", err)
	}
	trustedKeys, err := update.ProductionTrustedKeys()
	if err != nil {
		return err
	}
	channel, err := update.ProductionUpdateChannel()
	if err != nil {
		return err
	}
	updateContext, cancel := context.WithCancel(parent)
	service, err := update.NewService(update.Options{
		BaseURL: update.ProductionBaseURL, Channel: channel,
		CurrentVersion: a.application.Bootstrap().Build.ProductVersion,
		TrustedKeys:    trustedKeys, Root: filepath.Join(currentSettings.StorageRoot, "updates"),
		CanTransfer: a.updateTransferAllowed,
		Publisher: func(status update.StatusDTO) {
			a.emit(a.application.Context(), update.StatusEventName, status)
		},
	})
	if err != nil {
		cancel()
		return err
	}
	a.updaterMu.Lock()
	if a.updateCancel != nil {
		a.updateCancel()
	}
	a.updater = service
	a.updateCancel = cancel
	a.updaterMu.Unlock()
	go a.runUpdaterScheduler(updateContext, service)
	go a.cleanupStaleUpdateHelpers(updateContext, currentSettings.StorageRoot)
	return nil
}

func (a *DesktopApp) stopUpdater() {
	if a == nil {
		return
	}
	a.updaterMu.Lock()
	cancel := a.updateCancel
	a.updateCancel = nil
	a.updaterMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (a *DesktopApp) acknowledgeUpdaterLaunch() {
	settingsService := a.application.SettingsService()
	if settingsService == nil {
		return
	}
	currentSettings, err := settingsService.GetSettings(a.application.Context())
	if err != nil {
		return
	}
	version := a.application.Bootstrap().Build.ProductVersion
	if err := update.WriteStartupHealthMarker(currentSettings.StorageRoot, version); err != nil {
		return
	}
}

func (a *DesktopApp) runUpdaterScheduler(ctx context.Context, service *update.Service) {
	timer := time.NewTimer(firstAutomaticUpdateCheck)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if a.automaticUpdatesEnabled(ctx) && service.Status().State != update.StateDisabled {
			status, err := service.Check(ctx)
			if err == nil && status.State == update.StateAvailable {
				if allowed, _ := a.updateTransferAllowed(); allowed {
					_, _ = service.Prepare(ctx)
				}
			}
		}
		delay := automaticUpdateInterval
		if automaticUpdateJitter > 0 {
			delay += time.Duration(mathrand.Int63n(int64(automaticUpdateJitter)))
		}
		timer.Reset(delay)
	}
}

func (a *DesktopApp) automaticUpdatesEnabled(ctx context.Context) bool {
	service := a.application.SettingsService()
	if service == nil {
		return false
	}
	current, err := service.GetSettings(ctx)
	return err == nil && current.AutomaticUpdates
}

func (a *DesktopApp) updateTransferAllowed() (bool, string) {
	if a == nil {
		return false, "UPDATE_STATE_UNAVAILABLE"
	}
	service := a.application.RoomService()
	manager := a.application.MonitorManager()
	if service == nil || manager == nil {
		return false, "UPDATE_STATE_UNAVAILABLE"
	}
	ctx := a.application.Context()
	rooms, err := service.ListRooms(ctx)
	if err != nil {
		return false, "UPDATE_STATE_UNAVAILABLE"
	}
	busyStates := map[room.RuntimeState]bool{
		room.RuntimeStarting: true, room.RuntimeLive: true,
		room.RuntimeRecording: true, room.RuntimeReconnecting: true,
		room.RuntimeFinalizing: true,
	}
	for _, configuredRoom := range rooms {
		status, err := manager.GetRoomStatus(ctx, configuredRoom.ID)
		if err != nil {
			return false, "UPDATE_STATE_UNAVAILABLE"
		}
		if busyStates[status.State] {
			return false, "UPDATE_BUSY_" + string(status.State)
		}
	}
	return true, ""
}

func (a *DesktopApp) updateService() (*update.Service, error) {
	if a == nil {
		return nil, errors.New("UPDATE_SERVICE_UNAVAILABLE")
	}
	a.updaterMu.RLock()
	defer a.updaterMu.RUnlock()
	if a.updater == nil {
		return nil, errors.New("UPDATE_SERVICE_UNAVAILABLE")
	}
	return a.updater, nil
}

func (a *DesktopApp) GetUpdateStatus() update.StatusDTO {
	service, err := a.updateService()
	if err != nil {
		return update.StatusDTO{
			Version: 1, State: update.StateDisabled,
			CurrentVersion: a.application.Bootstrap().Build.ProductVersion,
			ErrorCode:      "UPDATE_SERVICE_UNAVAILABLE",
		}
	}
	return service.Status()
}

func (a *DesktopApp) CheckForUpdate() (update.StatusDTO, error) {
	service, err := a.updateService()
	if err != nil {
		return a.GetUpdateStatus(), err
	}
	return service.Check(a.application.Context())
}

func (a *DesktopApp) PrepareUpdate() (update.StatusDTO, error) {
	service, err := a.updateService()
	if err != nil {
		return a.GetUpdateStatus(), err
	}
	return service.Prepare(a.application.Context())
}

func (a *DesktopApp) CancelUpdateDownload() update.StatusDTO {
	service, err := a.updateService()
	if err != nil {
		return a.GetUpdateStatus()
	}
	return service.CancelDownload()
}

func (a *DesktopApp) InstallPreparedUpdate() (update.StatusDTO, error) {
	service, err := a.updateService()
	if err != nil {
		return a.GetUpdateStatus(), err
	}
	installerPath, envelope, verified, err := service.PreparedPackage()
	if err != nil {
		return service.Status(), err
	}
	if allowed, reason := a.updateTransferAllowed(); !allowed {
		return service.Status(), fmt.Errorf("UPDATE_BUSY: %s", reason)
	}
	store := a.application.Store()
	settingsService := a.application.SettingsService()
	if store == nil || settingsService == nil {
		return service.ReportFailure("UPDATE_INFRASTRUCTURE_UNAVAILABLE", errors.New("update infrastructure is unavailable"))
	}
	currentSettings, err := settingsService.GetSettings(a.application.Context())
	if err != nil {
		return service.ReportFailure("UPDATE_SETTINGS_UNAVAILABLE", err)
	}
	databaseBackup, err := store.CreateUpdateBackup(a.application.Context(), time.Now())
	if err != nil {
		return service.ReportFailure("UPDATE_DATABASE_BACKUP_FAILED", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return service.ReportFailure("UPDATE_INSTALL_PATH_INVALID", err)
	}
	executable, err = filepath.Abs(filepath.Clean(executable))
	if err != nil {
		return service.ReportFailure("UPDATE_INSTALL_PATH_INVALID", err)
	}
	installDir := filepath.Dir(executable)
	if err := checkUpdateDiskSpace(installDir, currentSettings.StorageRoot, verified.Payload.Installer.Size); err != nil {
		return service.ReportFailure(update.ErrorCode(err), err)
	}
	nonce, err := updateNonce()
	if err != nil {
		return service.ReportFailure("UPDATE_NONCE_FAILED", err)
	}
	updateRoot := filepath.Join(currentSettings.StorageRoot, "updates")
	helperSource := filepath.Join(installDir, "douyin-live-updater.exe")
	helperDirectory := filepath.Join(updateRoot, "helpers")
	helperCopy := filepath.Join(helperDirectory, "douyin-live-updater-"+nonce+".exe")
	if err := copyUpdateHelper(helperSource, helperCopy); err != nil {
		return service.ReportFailure("UPDATE_HELPER_PREPARE_FAILED", err)
	}
	cleanupPrepared := true
	defer func() {
		if cleanupPrepared {
			_ = os.Remove(helperCopy)
		}
	}()
	healthMarker := filepath.Join(updateRoot, "health", nonce+".json")
	jobPath := filepath.Join(updateRoot, "jobs", nonce+".json")
	job := update.NewInstallJob(
		os.Getpid(), a.application.Bootstrap().Build.ProductVersion, envelope, verified,
		installerPath, installDir, currentSettings.StorageRoot, databaseBackup,
		healthMarker, nonce, time.Now(),
	)
	if err := update.WriteInstallJob(jobPath, job); err != nil {
		return service.ReportFailure("UPDATE_INSTALL_JOB_WRITE_FAILED", err)
	}
	if _, err := service.MarkInstalling(); err != nil {
		_ = os.Remove(jobPath)
		return service.Status(), err
	}
	helper := exec.Command(helperCopy, "--job", jobPath)
	if err := helper.Start(); err != nil {
		_ = os.Remove(jobPath)
		return service.ReportFailure("UPDATE_HELPER_START_FAILED", err)
	}
	if err := helper.Process.Release(); err != nil {
		return service.ReportFailure("UPDATE_HELPER_RELEASE_FAILED", err)
	}
	cleanupPrepared = false
	status := service.Status()
	go func(ctx context.Context) {
		timer := time.NewTimer(250 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
			runtime.Quit(ctx)
		}
	}(a.application.Context())
	return status, nil
}

func updateNonce() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func copyUpdateHelper(source, target string) error {
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("installed update helper is missing or unsafe")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	syncErr := output.Sync()
	closeErr := output.Close()
	if joined := errors.Join(copyErr, syncErr, closeErr); joined != nil {
		_ = os.Remove(target)
		return joined
	}
	return nil
}

func (a *DesktopApp) cleanupStaleUpdateHelpers(ctx context.Context, dataRoot string) {
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	directory := filepath.Join(dataRoot, "updates", "helpers")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), "douyin-live-updater-") &&
			strings.HasSuffix(entry.Name(), ".exe") {
			_ = os.Remove(filepath.Join(directory, entry.Name()))
		}
	}
}
