// Package settings owns versioned, atomically persisted application settings.
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/jwwsjlm/douyinLive/v2/internal/room"
)

const SettingsVersion = 1

type AppSettings struct {
	Version                 int          `json:"version"`
	StorageRoot             string       `json:"storageRoot"`
	RecordingDirectory      string       `json:"recordingDirectory"`
	DefaultQuality          room.Quality `json:"defaultQuality"`
	DefaultSegmentMinutes   int          `json:"defaultSegmentMinutes"`
	MaxConcurrentRecordings int          `json:"maxConcurrentRecordings"`
	MinimumFreeSpaceGiB     int          `json:"minimumFreeSpaceGiB"`
	SaveDisplayNames        bool         `json:"saveDisplayNames"`
}

type UpdateSettingsInput struct {
	RecordingDirectory      string       `json:"recordingDirectory"`
	DefaultQuality          room.Quality `json:"defaultQuality"`
	DefaultSegmentMinutes   int          `json:"defaultSegmentMinutes"`
	MaxConcurrentRecordings int          `json:"maxConcurrentRecordings"`
	MinimumFreeSpaceGiB     int          `json:"minimumFreeSpaceGiB"`
	SaveDisplayNames        bool         `json:"saveDisplayNames"`
}

type Error struct {
	Code    string
	Field   string
	Message string
}

func (e *Error) Error() string {
	if e.Field == "" {
		return e.Code + ": " + e.Message
	}
	return e.Code + " (" + e.Field + "): " + e.Message
}

func ErrorCode(err error) string {
	var settingsError *Error
	if errors.As(err, &settingsError) {
		return settingsError.Code
	}
	return ""
}

type persistedSettings struct {
	Version                 int          `json:"version"`
	RecordingDirectory      string       `json:"recordingDirectory"`
	DefaultQuality          room.Quality `json:"defaultQuality"`
	DefaultSegmentMinutes   int          `json:"defaultSegmentMinutes"`
	MaxConcurrentRecordings int          `json:"maxConcurrentRecordings"`
	MinimumFreeSpaceGiB     int          `json:"minimumFreeSpaceGiB"`
	SaveDisplayNames        bool         `json:"saveDisplayNames"`
}

type Service struct {
	mu          sync.RWMutex
	path        string
	storageRoot string
	settings    persistedSettings
}

func Open(configDirectory, storageRoot, defaultRecordingDirectory string) (*Service, error) {
	if strings.TrimSpace(configDirectory) == "" || strings.TrimSpace(storageRoot) == "" {
		return nil, errors.New("settings paths are required")
	}
	if defaultRecordingDirectory == "" {
		defaultRecordingDirectory = filepath.Join(storageRoot, "rooms")
	}
	service := &Service{
		path:        filepath.Join(configDirectory, "settings.json"),
		storageRoot: filepath.Clean(storageRoot),
		settings: persistedSettings{
			Version:                 SettingsVersion,
			RecordingDirectory:      filepath.Clean(defaultRecordingDirectory),
			DefaultQuality:          room.QualityAuto,
			DefaultSegmentMinutes:   10,
			MaxConcurrentRecordings: 1,
			MinimumFreeSpaceGiB:     10,
			SaveDisplayNames:        true,
		},
	}
	data, err := os.ReadFile(service.path)
	if errors.Is(err, os.ErrNotExist) {
		if err := service.persistLocked(); err != nil {
			return nil, err
		}
		return service, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read settings: %w", err)
	}
	if err := json.Unmarshal(data, &service.settings); err != nil {
		return nil, fmt.Errorf("decode settings: %w", err)
	}
	if service.settings.Version != SettingsVersion {
		return nil, fmt.Errorf("unsupported settings version %d", service.settings.Version)
	}
	if _, err := validate(UpdateSettingsInput{
		RecordingDirectory:      service.settings.RecordingDirectory,
		DefaultQuality:          service.settings.DefaultQuality,
		DefaultSegmentMinutes:   service.settings.DefaultSegmentMinutes,
		MaxConcurrentRecordings: service.settings.MaxConcurrentRecordings,
		MinimumFreeSpaceGiB:     service.settings.MinimumFreeSpaceGiB,
		SaveDisplayNames:        service.settings.SaveDisplayNames,
	}); err != nil {
		return nil, fmt.Errorf("validate persisted settings: %w", err)
	}
	return service, nil
}

func (s *Service) GetSettings(ctx context.Context) (AppSettings, error) {
	if err := contextError(ctx); err != nil {
		return AppSettings{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dtoLocked(), nil
}

func (s *Service) UpdateSettings(ctx context.Context, input UpdateSettingsInput) (AppSettings, error) {
	if err := contextError(ctx); err != nil {
		return AppSettings{}, err
	}
	normalized, err := validate(input)
	if err != nil {
		return AppSettings{}, err
	}
	if err := verifyDirectory(normalized.RecordingDirectory); err != nil {
		return AppSettings{}, &Error{Code: "STORAGE_NOT_WRITABLE", Field: "recordingDirectory", Message: "录制目录不可写"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.settings
	s.settings = normalized
	if err := s.persistLocked(); err != nil {
		s.settings = previous
		return AppSettings{}, err
	}
	return s.dtoLocked(), nil
}

func (s *Service) dtoLocked() AppSettings {
	return AppSettings{
		Version:                 s.settings.Version,
		StorageRoot:             s.storageRoot,
		RecordingDirectory:      s.settings.RecordingDirectory,
		DefaultQuality:          s.settings.DefaultQuality,
		DefaultSegmentMinutes:   s.settings.DefaultSegmentMinutes,
		MaxConcurrentRecordings: s.settings.MaxConcurrentRecordings,
		MinimumFreeSpaceGiB:     s.settings.MinimumFreeSpaceGiB,
		SaveDisplayNames:        s.settings.SaveDisplayNames,
	}
}

func (s *Service) persistLocked() error {
	data, err := json.MarshalIndent(s.settings, "", "  ")
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create settings directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".settings-*.tmp")
	if err != nil {
		return fmt.Errorf("create settings temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("restrict settings temporary file: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write settings temporary file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync settings temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close settings temporary file: %w", err)
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		return fmt.Errorf("replace settings: %w", err)
	}
	return nil
}

func validate(input UpdateSettingsInput) (persistedSettings, error) {
	directory := strings.TrimSpace(input.RecordingDirectory)
	if directory == "" || !filepath.IsAbs(directory) {
		return persistedSettings{}, &Error{Code: "SETTINGS_INVALID", Field: "recordingDirectory", Message: "录制目录必须是绝对路径"}
	}
	switch input.DefaultQuality {
	case room.QualityAuto, room.QualityOriginal, room.QualityUltra, room.QualityHigh, room.QualityStandard:
	default:
		return persistedSettings{}, &Error{Code: "SETTINGS_INVALID", Field: "defaultQuality", Message: "默认录制质量无效"}
	}
	if input.DefaultSegmentMinutes < 1 || input.DefaultSegmentMinutes > 60 {
		return persistedSettings{}, &Error{Code: "SETTINGS_INVALID", Field: "defaultSegmentMinutes", Message: "默认分片时长必须为 1 到 60 分钟"}
	}
	if input.MaxConcurrentRecordings < 1 || input.MaxConcurrentRecordings > 4 {
		return persistedSettings{}, &Error{Code: "SETTINGS_INVALID", Field: "maxConcurrentRecordings", Message: "并发录制上限必须为 1 到 4"}
	}
	if input.MinimumFreeSpaceGiB < 1 || input.MinimumFreeSpaceGiB > 1024 {
		return persistedSettings{}, &Error{Code: "SETTINGS_INVALID", Field: "minimumFreeSpaceGiB", Message: "最小剩余空间必须为 1 到 1024 GiB"}
	}
	return persistedSettings{
		Version: SettingsVersion, RecordingDirectory: filepath.Clean(directory), DefaultQuality: input.DefaultQuality,
		DefaultSegmentMinutes: input.DefaultSegmentMinutes, MaxConcurrentRecordings: input.MaxConcurrentRecordings,
		MinimumFreeSpaceGiB: input.MinimumFreeSpaceGiB, SaveDisplayNames: input.SaveDisplayNames,
	}, nil
}

func verifyDirectory(directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	probe, err := os.CreateTemp(directory, ".write-probe-*")
	if err != nil {
		return err
	}
	probePath := probe.Name()
	renamedPath := probePath + ".renamed"
	defer os.Remove(probePath)
	defer os.Remove(renamedPath)
	if err := probe.Close(); err != nil {
		return err
	}
	return os.Rename(probePath, renamedPath)
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return errors.New("settings context is nil")
	}
	return ctx.Err()
}
