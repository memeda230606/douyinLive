// Package room implements persistent room configuration without exposing credentials.
package room

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jwwsjlm/douyinLive/v2/internal/credentials"
)

type Quality string

const (
	QualityAuto     Quality = "auto"
	QualityOriginal Quality = "original"
	QualityUltra    Quality = "ultra"
	QualityHigh     Quality = "high"
	QualityStandard Quality = "standard"
)

type RecordingProfile struct {
	Quality        Quality `json:"quality"`
	SegmentMinutes int     `json:"segmentMinutes"`
	SaveDirectory  string  `json:"saveDirectory,omitempty"`
}

type CookieStatus struct {
	Configured bool  `json:"configured"`
	UpdatedAt  int64 `json:"updatedAt,omitempty"`
}

type RoomConfig struct {
	ID               string           `json:"id"`
	LiveID           string           `json:"liveId"`
	RoomID           string           `json:"roomId,omitempty"`
	Alias            string           `json:"alias"`
	AnchorName       string           `json:"anchorName,omitempty"`
	MonitorEnabled   bool             `json:"monitorEnabled"`
	RecordEnabled    bool             `json:"recordEnabled"`
	RecordingProfile RecordingProfile `json:"recordingProfile"`
	Cookie           CookieStatus     `json:"cookie"`
	CreatedAt        int64            `json:"createdAt"`
	UpdatedAt        int64            `json:"updatedAt"`
}

type CreateRoomInput struct {
	LiveID           string           `json:"liveId"`
	Alias            string           `json:"alias"`
	MonitorEnabled   bool             `json:"monitorEnabled"`
	RecordEnabled    bool             `json:"recordEnabled"`
	RecordingProfile RecordingProfile `json:"recordingProfile"`
}

type UpdateRoomInput struct {
	LiveID           string           `json:"liveId"`
	Alias            string           `json:"alias"`
	MonitorEnabled   bool             `json:"monitorEnabled"`
	RecordEnabled    bool             `json:"recordEnabled"`
	RecordingProfile RecordingProfile `json:"recordingProfile"`
}

type SetRoomCookieInput struct {
	RoomID string `json:"roomId"`
	Cookie string `json:"cookie"`
}

type BusinessError struct {
	Code    string
	Field   string
	Message string
}

func (e *BusinessError) Error() string {
	if e.Field == "" {
		return e.Code + ": " + e.Message
	}
	return e.Code + " (" + e.Field + "): " + e.Message
}

func ErrorCode(err error) string {
	var business *BusinessError
	if errors.As(err, &business) {
		return business.Code
	}
	return ""
}

type Service struct {
	writer      *sql.DB
	reader      *sql.DB
	credentials credentials.Store
	now         func() time.Time
}

func NewService(writer, reader *sql.DB, credentialStore credentials.Store) (*Service, error) {
	return newService(writer, reader, credentialStore, time.Now)
}

func newService(writer, reader *sql.DB, credentialStore credentials.Store, now func() time.Time) (*Service, error) {
	if writer == nil || reader == nil {
		return nil, errors.New("room service requires reader and writer databases")
	}
	if credentialStore == nil {
		return nil, errors.New("room service requires a credential store")
	}
	if now == nil {
		now = time.Now
	}
	return &Service{writer: writer, reader: reader, credentials: credentialStore, now: now}, nil
}

func (s *Service) ListRooms(ctx context.Context) ([]RoomConfig, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	rows, err := s.reader.QueryContext(ctx, roomSelectSQL+` ORDER BY updated_at DESC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list rooms: %w", err)
	}
	defer rows.Close()
	rooms := make([]RoomConfig, 0)
	for rows.Next() {
		room, credentialRef, err := scanRoom(rows)
		if err != nil {
			return nil, err
		}
		if err := s.attachCookieStatus(ctx, &room, credentialRef); err != nil {
			return nil, err
		}
		rooms = append(rooms, room)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rooms: %w", err)
	}
	return rooms, nil
}

func (s *Service) GetRoom(ctx context.Context, id string) (RoomConfig, error) {
	if err := contextError(ctx); err != nil {
		return RoomConfig{}, err
	}
	if err := validateID(id); err != nil {
		return RoomConfig{}, err
	}
	room, credentialRef, err := scanRoom(s.reader.QueryRowContext(ctx, roomSelectSQL+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return RoomConfig{}, roomNotFound()
	}
	if err != nil {
		return RoomConfig{}, err
	}
	if err := s.attachCookieStatus(ctx, &room, credentialRef); err != nil {
		return RoomConfig{}, err
	}
	return room, nil
}

func (s *Service) CreateRoom(ctx context.Context, input CreateRoomInput) (RoomConfig, error) {
	if err := contextError(ctx); err != nil {
		return RoomConfig{}, err
	}
	liveID, alias, profile, err := normalizeInput(input.LiveID, input.Alias, input.RecordingProfile)
	if err != nil {
		return RoomConfig{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return RoomConfig{}, fmt.Errorf("generate room id: %w", err)
	}
	profileJSON, err := json.Marshal(profile)
	if err != nil {
		return RoomConfig{}, fmt.Errorf("encode recording profile: %w", err)
	}
	now := s.now().UTC().UnixMilli()
	_, err = s.writer.ExecContext(ctx, `INSERT INTO rooms(
		id, live_id, alias, monitor_enabled, record_enabled, recording_profile_json, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id.String(), liveID, alias, input.MonitorEnabled, input.RecordEnabled, string(profileJSON), now, now)
	if isUniqueConstraint(err) {
		return RoomConfig{}, &BusinessError{Code: "ROOM_ALREADY_EXISTS", Field: "liveId", Message: "直播间已存在"}
	}
	if err != nil {
		return RoomConfig{}, fmt.Errorf("create room: %w", err)
	}
	return s.GetRoom(ctx, id.String())
}

func (s *Service) UpdateRoom(ctx context.Context, id string, input UpdateRoomInput) (RoomConfig, error) {
	if err := contextError(ctx); err != nil {
		return RoomConfig{}, err
	}
	if err := validateID(id); err != nil {
		return RoomConfig{}, err
	}
	liveID, alias, profile, err := normalizeInput(input.LiveID, input.Alias, input.RecordingProfile)
	if err != nil {
		return RoomConfig{}, err
	}
	profileJSON, err := json.Marshal(profile)
	if err != nil {
		return RoomConfig{}, fmt.Errorf("encode recording profile: %w", err)
	}
	result, err := s.writer.ExecContext(ctx, `UPDATE rooms SET
		live_id = ?, alias = ?, monitor_enabled = ?, record_enabled = ?, recording_profile_json = ?, updated_at = ?
		WHERE id = ?`, liveID, alias, input.MonitorEnabled, input.RecordEnabled, string(profileJSON), s.now().UTC().UnixMilli(), id)
	if isUniqueConstraint(err) {
		return RoomConfig{}, &BusinessError{Code: "ROOM_ALREADY_EXISTS", Field: "liveId", Message: "直播间已存在"}
	}
	if err != nil {
		return RoomConfig{}, fmt.Errorf("update room: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return RoomConfig{}, fmt.Errorf("inspect room update: %w", err)
	}
	if affected == 0 {
		return RoomConfig{}, roomNotFound()
	}
	return s.GetRoom(ctx, id)
}

func (s *Service) DeleteRoom(ctx context.Context, id string, deleteData bool) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := validateID(id); err != nil {
		return err
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin room deletion: %w", err)
	}
	defer tx.Rollback()
	var credentialRef sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT credential_ref FROM rooms WHERE id = ?`, id).Scan(&credentialRef); errors.Is(err, sql.ErrNoRows) {
		return roomNotFound()
	} else if err != nil {
		return fmt.Errorf("read room before deletion: %w", err)
	}
	var sessionCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM live_sessions WHERE room_config_id = ?`, id).Scan(&sessionCount); err != nil {
		return fmt.Errorf("count room sessions: %w", err)
	}
	if sessionCount > 0 && !deleteData {
		return &BusinessError{Code: "ROOM_HAS_HISTORY", Message: "直播间仍有关联历史场次"}
	}
	if deleteData {
		if _, err := tx.ExecContext(ctx, `DELETE FROM live_sessions WHERE room_config_id = ?`, id); err != nil {
			return fmt.Errorf("delete room history: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM rooms WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete room: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit room deletion: %w", err)
	}
	if credentialRef.Valid {
		if err := s.credentials.Delete(ctx, credentialRef.String); err != nil {
			return &BusinessError{Code: "CREDENTIAL_CLEANUP_FAILED", Message: "房间已删除，但凭据清理失败"}
		}
	}
	return nil
}

func (s *Service) SetRoomCookie(ctx context.Context, input SetRoomCookieInput) (CookieStatus, error) {
	if err := contextError(ctx); err != nil {
		return CookieStatus{}, err
	}
	if err := validateID(input.RoomID); err != nil {
		return CookieStatus{}, err
	}
	cookie := strings.TrimSpace(input.Cookie)
	if cookie == "" || len(cookie) > 64*1024 || strings.ContainsAny(cookie, "\r\n") {
		return CookieStatus{}, &BusinessError{Code: "COOKIE_INVALID", Field: "cookie", Message: "Cookie 为空、过长或包含换行"}
	}
	var count int
	if err := s.reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM rooms WHERE id = ?`, input.RoomID).Scan(&count); err != nil {
		return CookieStatus{}, fmt.Errorf("check room before cookie update: %w", err)
	}
	if count == 0 {
		return CookieStatus{}, roomNotFound()
	}
	ref := "room:" + input.RoomID + ":cookie"
	status, err := s.credentials.Put(ctx, ref, []byte(cookie))
	if err != nil {
		return CookieStatus{}, fmt.Errorf("store room credential: %w", err)
	}
	result, err := s.writer.ExecContext(ctx, `UPDATE rooms SET credential_ref = ?, updated_at = ? WHERE id = ?`, ref, s.now().UTC().UnixMilli(), input.RoomID)
	if err != nil {
		return CookieStatus{}, fmt.Errorf("link room credential: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected == 0 {
		return CookieStatus{}, roomNotFound()
	}
	return CookieStatus{Configured: status.Configured, UpdatedAt: status.UpdatedAt}, nil
}

func (s *Service) ClearRoomCookie(ctx context.Context, id string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := validateID(id); err != nil {
		return err
	}
	var ref sql.NullString
	if err := s.reader.QueryRowContext(ctx, `SELECT credential_ref FROM rooms WHERE id = ?`, id).Scan(&ref); errors.Is(err, sql.ErrNoRows) {
		return roomNotFound()
	} else if err != nil {
		return fmt.Errorf("read room credential reference: %w", err)
	}
	if ref.Valid {
		if err := s.credentials.Delete(ctx, ref.String); err != nil {
			return fmt.Errorf("delete room credential: %w", err)
		}
	}
	if _, err := s.writer.ExecContext(ctx, `UPDATE rooms SET credential_ref = NULL, updated_at = ? WHERE id = ?`, s.now().UTC().UnixMilli(), id); err != nil {
		return fmt.Errorf("clear room credential reference: %w", err)
	}
	return nil
}

const roomSelectSQL = `SELECT id, live_id, room_id, alias, anchor_name, monitor_enabled,
	record_enabled, recording_profile_json, credential_ref, created_at, updated_at FROM rooms`

type rowScanner interface {
	Scan(...any) error
}

func scanRoom(scanner rowScanner) (RoomConfig, sql.NullString, error) {
	var result RoomConfig
	var roomID, anchorName, credentialRef sql.NullString
	var monitorEnabled, recordEnabled bool
	var profileJSON string
	if err := scanner.Scan(&result.ID, &result.LiveID, &roomID, &result.Alias, &anchorName, &monitorEnabled,
		&recordEnabled, &profileJSON, &credentialRef, &result.CreatedAt, &result.UpdatedAt); err != nil {
		return RoomConfig{}, sql.NullString{}, err
	}
	if err := json.Unmarshal([]byte(profileJSON), &result.RecordingProfile); err != nil {
		return RoomConfig{}, sql.NullString{}, fmt.Errorf("decode room recording profile: %w", err)
	}
	result.RoomID = roomID.String
	result.AnchorName = anchorName.String
	result.MonitorEnabled = monitorEnabled
	result.RecordEnabled = recordEnabled
	return result, credentialRef, nil
}

func (s *Service) attachCookieStatus(ctx context.Context, room *RoomConfig, ref sql.NullString) error {
	if !ref.Valid {
		return nil
	}
	status, err := s.credentials.Status(ctx, ref.String)
	if err != nil {
		return fmt.Errorf("read room credential status: %w", err)
	}
	room.Cookie = CookieStatus{Configured: status.Configured, UpdatedAt: status.UpdatedAt}
	return nil
}

func normalizeInput(liveID, alias string, profile RecordingProfile) (string, string, RecordingProfile, error) {
	normalizedLiveID, err := NormalizeLiveID(liveID)
	if err != nil {
		return "", "", RecordingProfile{}, err
	}
	normalizedAlias := strings.TrimSpace(alias)
	if normalizedAlias == "" {
		normalizedAlias = normalizedLiveID
	}
	if len([]rune(normalizedAlias)) > 80 || containsControl(normalizedAlias) {
		return "", "", RecordingProfile{}, &BusinessError{Code: "ROOM_INPUT_INVALID", Field: "alias", Message: "别名不能为空、过长或包含控制字符"}
	}
	normalizedProfile, err := normalizeProfile(profile)
	if err != nil {
		return "", "", RecordingProfile{}, err
	}
	return normalizedLiveID, normalizedAlias, normalizedProfile, nil
}

func NormalizeLiveID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", &BusinessError{Code: "ROOM_INPUT_INVALID", Field: "liveId", Message: "直播间标识不能为空"}
	}
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "live.douyin.com/") || strings.HasPrefix(lower, "www.live.douyin.com/") {
		value = "https://" + value
	}
	if strings.Contains(value, "://") {
		parsed, err := url.Parse(value)
		if err != nil {
			return "", &BusinessError{Code: "ROOM_INPUT_INVALID", Field: "liveId", Message: "直播间 URL 无效"}
		}
		host := strings.ToLower(parsed.Hostname())
		if host != "live.douyin.com" && host != "www.live.douyin.com" {
			return "", &BusinessError{Code: "ROOM_INPUT_INVALID", Field: "liveId", Message: "仅支持抖音直播间 URL"}
		}
		value = strings.Trim(parsed.Path, "/")
	}
	value = strings.Trim(value, "/")
	if value == "" || len(value) > 128 || strings.ContainsAny(value, "/\\?#") || containsControl(value) || strings.IndexFunc(value, unicode.IsSpace) >= 0 {
		return "", &BusinessError{Code: "ROOM_INPUT_INVALID", Field: "liveId", Message: "直播间标识格式无效"}
	}
	return value, nil
}

func normalizeProfile(profile RecordingProfile) (RecordingProfile, error) {
	if profile.Quality == "" {
		profile.Quality = QualityAuto
	}
	switch profile.Quality {
	case QualityAuto, QualityOriginal, QualityUltra, QualityHigh, QualityStandard:
	default:
		return RecordingProfile{}, &BusinessError{Code: "ROOM_INPUT_INVALID", Field: "recordingProfile.quality", Message: "录制质量无效"}
	}
	if profile.SegmentMinutes == 0 {
		profile.SegmentMinutes = 10
	}
	if profile.SegmentMinutes < 1 || profile.SegmentMinutes > 60 {
		return RecordingProfile{}, &BusinessError{Code: "ROOM_INPUT_INVALID", Field: "recordingProfile.segmentMinutes", Message: "分片时长必须为 1 到 60 分钟"}
	}
	profile.SaveDirectory = strings.TrimSpace(profile.SaveDirectory)
	if profile.SaveDirectory != "" {
		if !filepath.IsAbs(profile.SaveDirectory) {
			return RecordingProfile{}, &BusinessError{Code: "ROOM_INPUT_INVALID", Field: "recordingProfile.saveDirectory", Message: "保存目录必须是绝对路径"}
		}
		profile.SaveDirectory = filepath.Clean(profile.SaveDirectory)
	}
	return profile, nil
}

func validateID(id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return &BusinessError{Code: "ROOM_NOT_FOUND", Field: "id", Message: "直播间不存在"}
	}
	return nil
}

func roomNotFound() error {
	return &BusinessError{Code: "ROOM_NOT_FOUND", Message: "直播间不存在"}
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return errors.New("room service context is nil")
	}
	return ctx.Err()
}

func containsControl(value string) bool {
	return strings.IndexFunc(value, unicode.IsControl) >= 0
}

func isUniqueConstraint(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique constraint failed")
}
