//go:build p2acceptance

package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const p2AcceptanceSchema = "P2-ACC-001/v1"

//go:embed p2_acceptance.js
var p2AcceptanceScriptTemplate string

var p2AcceptanceErrorCode = regexp.MustCompile(`^[A-Z0-9_]{1,64}$`)

type p2AcceptancePaths struct {
	Root               string
	ResultPath         string
	RecordingDirectory string
}

type p2AcceptanceResult struct {
	Schema            string          `json:"schema"`
	Phase             int             `json:"phase"`
	Success           bool            `json:"success"`
	Checks            map[string]bool `json:"checks,omitempty"`
	StatusLabel       string          `json:"statusLabel,omitempty"`
	ErrorCode         string          `json:"errorCode,omitempty"`
	ConfirmationCount int             `json:"confirmationCount,omitempty"`
}

func (a *DesktopApp) startAcceptanceHook(ctx context.Context) {
	phaseText := strings.TrimSpace(os.Getenv("P2ACC_PHASE"))
	if phaseText == "" {
		return
	}
	phase, err := strconv.Atoi(phaseText)
	if err != nil || phase < 1 || phase > 3 {
		return
	}
	liveRoomURL := os.Getenv("P2ACC_LIVE_URL")
	_ = os.Unsetenv("P2ACC_LIVE_URL")
	paths, err := loadP2AcceptancePaths(phase)
	if err != nil {
		a.writeAcceptanceFailure(phase, "HOOK_ISOLATION_INVALID")
		return
	}
	settingsService, err := a.settingsService()
	if err != nil {
		a.writeAcceptanceFailure(phase, "HOOK_ISOLATION_INVALID")
		return
	}
	currentSettings, err := settingsService.GetSettings(ctx)
	if err != nil || validateP2AcceptanceStorageRoot(paths.Root, currentSettings.StorageRoot) != nil {
		a.writeAcceptanceFailure(phase, "HOOK_ISOLATION_INVALID")
		return
	}
	if phase == 1 && strings.TrimSpace(liveRoomURL) == "" {
		a.writeAcceptanceFailure(phase, "HOOK_CONFIG_INVALID")
		return
	}

	phaseJSON, _ := json.Marshal(phase)
	liveRoomJSON, _ := json.Marshal(liveRoomURL)
	recordingJSON, _ := json.Marshal(paths.RecordingDirectory)
	script := strings.ReplaceAll(p2AcceptanceScriptTemplate, "__P2_PHASE__", string(phaseJSON))
	script = strings.ReplaceAll(script, "__P2_LIVE_ROOM_URL__", string(liveRoomJSON))
	script = strings.ReplaceAll(script, "__P2_RECORDING_DIRECTORY__", string(recordingJSON))

	go func() {
		timer := time.NewTimer(750 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			runtime.WindowExecJS(ctx, script)
		}
	}()
}

func (a *DesktopApp) writeAcceptanceFailure(phase int, code string) {
	payload, _ := json.Marshal(p2AcceptanceResult{
		Schema: p2AcceptanceSchema, Phase: phase, Success: false, ErrorCode: code,
	})
	_ = a.ReportAcceptanceResult(string(payload))
}

func loadP2AcceptancePaths(phase int) (p2AcceptancePaths, error) {
	root, err := cleanP2AcceptanceAbsolutePath(os.Getenv("P2ACC_ROOT"))
	if err != nil {
		return p2AcceptancePaths{}, fmt.Errorf("acceptance root is invalid: %w", err)
	}
	resultPath, err := cleanP2AcceptanceAbsolutePath(os.Getenv("P2ACC_RESULT_PATH"))
	if err != nil {
		return p2AcceptancePaths{}, fmt.Errorf("acceptance result path is invalid: %w", err)
	}
	recordingDirectory, err := cleanP2AcceptanceAbsolutePath(os.Getenv("P2ACC_RECORDING_DIRECTORY"))
	if err != nil {
		return p2AcceptancePaths{}, fmt.Errorf("acceptance recording directory is invalid: %w", err)
	}
	if !p2AcceptancePathWithin(root, resultPath, false) {
		return p2AcceptancePaths{}, errors.New("acceptance result escapes isolated root")
	}
	if !p2AcceptancePathWithin(root, recordingDirectory, true) {
		return p2AcceptancePaths{}, errors.New("acceptance recording directory escapes isolated root")
	}
	expectedName := fmt.Sprintf("phase-%d.result.json", phase)
	if filepath.Base(resultPath) != expectedName {
		return p2AcceptancePaths{}, errors.New("acceptance result filename is invalid")
	}
	return p2AcceptancePaths{Root: root, ResultPath: resultPath, RecordingDirectory: recordingDirectory}, nil
}

func cleanP2AcceptanceAbsolutePath(value string) (string, error) {
	cleaned := filepath.Clean(strings.TrimSpace(value))
	if cleaned == "." || !filepath.IsAbs(cleaned) {
		return "", errors.New("path must be absolute")
	}
	return cleaned, nil
}

func p2AcceptancePathWithin(root, candidate string, allowRoot bool) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	if relative == "." {
		return allowRoot
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func validateP2AcceptanceStorageRoot(root, storageRoot string) error {
	cleaned, err := cleanP2AcceptanceAbsolutePath(storageRoot)
	if err != nil {
		return fmt.Errorf("application storage root is invalid: %w", err)
	}
	if !p2AcceptancePathWithin(root, cleaned, true) {
		return errors.New("application storage root escapes isolated root")
	}
	return nil
}

// ReportAcceptanceResult only exists in the p2acceptance build. It accepts a
// strict, sanitized result shape and atomically writes it to the isolated test root.
func (a *DesktopApp) ReportAcceptanceResult(payload string) error {
	if len(payload) == 0 || len(payload) > 4096 {
		return errors.New("acceptance result size is invalid")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(payload))
	decoder.DisallowUnknownFields()
	var result p2AcceptanceResult
	if err := decoder.Decode(&result); err != nil {
		return fmt.Errorf("decode acceptance result: %w", err)
	}
	if err := ensureAcceptanceJSONEOF(decoder); err != nil {
		return err
	}
	if err := validateP2AcceptanceResult(result); err != nil {
		return err
	}
	paths, err := loadP2AcceptancePaths(result.Phase)
	if err != nil {
		return err
	}
	resultPath := paths.ResultPath
	if _, err := os.Stat(resultPath); err == nil || !errors.Is(err, os.ErrNotExist) {
		return errors.New("acceptance result already exists")
	}

	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(resultPath), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(resultPath), ".p2-acceptance-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, resultPath)
}

func ensureAcceptanceJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("acceptance result contains trailing JSON")
	}
	return err
}

func validateP2AcceptanceResult(result p2AcceptanceResult) error {
	if result.Schema != p2AcceptanceSchema || result.Phase < 1 || result.Phase > 3 {
		return errors.New("acceptance result identity is invalid")
	}
	expectedPhase, err := strconv.Atoi(strings.TrimSpace(os.Getenv("P2ACC_PHASE")))
	if err != nil || result.Phase != expectedPhase {
		return errors.New("acceptance result phase does not match process phase")
	}
	expectedChecks := map[int][]string{
		1: {"roomCreated", "roomEdited", "monitoringActive", "settingsSaved"},
		2: {"roomPersisted", "monitoringRestored", "settingsPersisted", "monitoringStopped", "roomDeleted"},
		3: {"deletionPersisted", "settingsPersisted"},
	}[result.Phase]
	allowedChecks := make(map[string]struct{}, len(expectedChecks))
	for _, name := range expectedChecks {
		allowedChecks[name] = struct{}{}
	}
	for name := range result.Checks {
		if _, ok := allowedChecks[name]; !ok {
			return errors.New("acceptance result contains an unknown check")
		}
	}
	if result.Success {
		if result.ErrorCode != "" || len(result.Checks) != len(expectedChecks) {
			return errors.New("successful acceptance result is incomplete")
		}
		for _, name := range expectedChecks {
			if !result.Checks[name] {
				return errors.New("successful acceptance check is false")
			}
		}
		if result.Phase == 2 && result.ConfirmationCount != 1 {
			return errors.New("delete confirmation count is invalid")
		}
		if result.Phase != 2 && result.ConfirmationCount != 0 {
			return errors.New("unexpected confirmation count")
		}
	} else if !p2AcceptanceErrorCode.MatchString(result.ErrorCode) {
		return errors.New("acceptance failure code is invalid")
	}
	if result.Success && result.StatusLabel == "需要处理" {
		return errors.New("successful acceptance status requires attention")
	}
	allowedStatus := map[string]struct{}{
		"": {}, "等待开播": {}, "正在连接": {}, "直播中": {}, "正在重连": {}, "需要处理": {},
	}
	if _, ok := allowedStatus[result.StatusLabel]; !ok {
		return errors.New("acceptance status label is invalid")
	}
	return nil
}
