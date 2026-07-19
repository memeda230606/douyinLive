//go:build p3uiacceptance

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
	"strings"
	"sync"
	"time"

	"github.com/jwwsjlm/douyinLive/v2/internal/capture"
	"github.com/jwwsjlm/douyinLive/v2/internal/eventstore"
	"github.com/jwwsjlm/douyinLive/v2/internal/room"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	p3UIAcceptanceSchema = "P3-UI-ACC-001/v1"
	p3UIAcceptanceLiveID = "p3-ui-acceptance-fixture"
	p3UIAcceptanceAlias  = "P3 实时验收房间"

	p3UIAcceptanceSessionID        = "018f47a0-7c00-7000-8000-000000000001"
	p3UIAcceptanceOperationID      = "018f47a0-7c00-7000-8000-000000000002"
	p3UIAcceptanceWrongSessionID   = "018f47a0-7c00-7000-8000-000000000003"
	p3UIAcceptanceWrongOperationID = "018f47a0-7c00-7000-8000-000000000004"

	p3UIAcceptanceEventCount = 2005
)

//go:embed p3_ui_acceptance.js
var p3UIAcceptanceScriptTemplate string

var p3UIAcceptanceErrorCode = regexp.MustCompile(`^[A-Z0-9_]{1,64}$`)

var p3UIAcceptanceRegistry = struct {
	sync.Mutex
	states map[*DesktopApp]*p3UIAcceptanceState
}{states: make(map[*DesktopApp]*p3UIAcceptanceState)}

type p3UIAcceptancePaths struct {
	Root       string
	ResultPath string
}

type p3UIAcceptanceState struct {
	ctx       context.Context
	paths     p3UIAcceptancePaths
	room      room.RoomConfig
	baseAt    int64
	nextStage int
}

type p3UIAcceptanceResult struct {
	Schema             string          `json:"schema"`
	Success            bool            `json:"success"`
	Checks             map[string]bool `json:"checks,omitempty"`
	VisibleEventCount  int             `json:"visibleEventCount,omitempty"`
	FilteredEventCount int             `json:"filteredEventCount,omitempty"`
	AlertCount         int             `json:"alertCount,omitempty"`
	StatusLabel        string          `json:"statusLabel,omitempty"`
	ErrorCode          string          `json:"errorCode,omitempty"`
}

var p3UIAcceptanceChecks = []string{
	"roomVisible",
	"statusFence",
	"timelineCapacity",
	"sessionFence",
	"eventFilter",
	"progressMetrics",
	"operationFence",
	"retryCountdown",
	"gapAlerts",
	"privacySafe",
	"layoutUsable",
}

func (a *DesktopApp) startAcceptanceHook(ctx context.Context) {
	rootConfigured := strings.TrimSpace(os.Getenv("P3UIACC_ROOT")) != ""
	resultConfigured := strings.TrimSpace(os.Getenv("P3UIACC_RESULT_PATH")) != ""
	if !rootConfigured && !resultConfigured {
		return
	}
	paths, err := loadP3UIAcceptancePaths()
	if err != nil {
		return
	}
	settingsService, err := a.settingsService()
	if err != nil {
		a.writeP3UIAcceptanceFailure("HOOK_ISOLATION_INVALID")
		return
	}
	settings, err := settingsService.GetSettings(ctx)
	if err != nil || validateP3UIAcceptanceStorageRoot(paths.Root, settings.StorageRoot) != nil {
		a.writeP3UIAcceptanceFailure("HOOK_ISOLATION_INVALID")
		return
	}
	roomService, err := a.roomService()
	if err != nil {
		a.writeP3UIAcceptanceFailure("HOOK_ISOLATION_INVALID")
		return
	}
	existingRooms, err := roomService.ListRooms(ctx)
	if err != nil || len(existingRooms) != 0 {
		a.writeP3UIAcceptanceFailure("HOOK_ISOLATION_INVALID")
		return
	}
	fixtureRoom, err := roomService.CreateRoom(ctx, room.CreateRoomInput{
		LiveID: p3UIAcceptanceLiveID,
		Alias:  p3UIAcceptanceAlias,
		RecordingProfile: room.RecordingProfile{
			Quality:        room.QualityHigh,
			SegmentMinutes: 10,
		},
		MonitorEnabled: false,
		RecordEnabled:  true,
	})
	if err != nil {
		a.writeP3UIAcceptanceFailure("FIXTURE_ROOM_CREATE_FAILED")
		return
	}
	now := time.Now().UTC().UnixMilli()
	if now < 0 {
		now = 0
	}
	p3UIAcceptanceRegistry.Lock()
	p3UIAcceptanceRegistry.states[a] = &p3UIAcceptanceState{
		ctx: ctx, paths: paths, room: fixtureRoom, baseAt: now, nextStage: 1,
	}
	p3UIAcceptanceRegistry.Unlock()

	aliasJSON, _ := json.Marshal(p3UIAcceptanceAlias)
	script := strings.ReplaceAll(
		p3UIAcceptanceScriptTemplate,
		"__P3_UI_ROOM_ALIAS__",
		string(aliasJSON),
	)
	go func() {
		scriptTimer := time.NewTimer(750 * time.Millisecond)
		defer scriptTimer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-scriptTimer.C:
			runtime.WindowExecJS(ctx, script)
		}
	}()
}

// EmitP3UIAcceptanceFixture only exists in the p3uiacceptance build. Each
// ordered stage emits privacy-safe DTOs through the real Wails event runtime.
func (a *DesktopApp) EmitP3UIAcceptanceFixture(stage int) error {
	p3UIAcceptanceRegistry.Lock()
	state := p3UIAcceptanceRegistry.states[a]
	if state == nil {
		p3UIAcceptanceRegistry.Unlock()
		return errors.New("P3UI_ACCEPTANCE_NOT_READY")
	}
	paths, err := loadP3UIAcceptancePaths()
	if err != nil || paths != state.paths {
		p3UIAcceptanceRegistry.Unlock()
		return errors.New("P3UI_ACCEPTANCE_ISOLATION_CHANGED")
	}
	if stage != state.nextStage || stage < 1 || stage > 3 {
		p3UIAcceptanceRegistry.Unlock()
		return errors.New("P3UI_ACCEPTANCE_STAGE_INVALID")
	}
	state.nextStage++
	snapshot := *state
	p3UIAcceptanceRegistry.Unlock()

	switch stage {
	case 1:
		a.emitP3UIAcceptanceInitial(snapshot)
	case 2:
		a.emitP3UIAcceptanceReconnecting(snapshot)
	case 3:
		a.emitP3UIAcceptanceRecovered(snapshot)
	}
	return nil
}

func (a *DesktopApp) emitP3UIAcceptanceInitial(state p3UIAcceptanceState) {
	a.emit(state.ctx, room.StatusEventName, p3UIRoomStatus(
		state, room.RuntimeRecording, capture.RecordingActive, 100,
		state.baseAt, 0, "正在录制验收夹具", "",
	))
	a.emit(state.ctx, room.StatusEventName, room.RoomRuntimeStatus{
		RoomID: state.room.ID, LiveID: state.room.LiveID, Alias: state.room.Alias,
		State: room.RuntimeError, Revision: 99,
		SessionID: p3UIAcceptanceWrongSessionID, OperationID: p3UIAcceptanceWrongOperationID,
		RecordingStatus: capture.RecordingFailed, ChangedAt: state.baseAt + 1,
		ErrorCode: "P3UI_STALE_SHOULD_NOT_RENDER", Message: "过期状态不得显示",
	})
	a.emit(state.ctx, capture.RecordingProgressEventName, p3UIRecordingProgress(
		state, capture.RecordingActive, p3UIAcceptanceSessionID, p3UIAcceptanceOperationID,
		125_000, 5*1024*1024, 3, 3_750, 29.7, 1.02, 1, state.baseAt,
	))
	a.emit(state.ctx, capture.RecordingProgressEventName, p3UIRecordingProgress(
		state, capture.RecordingActive, p3UIAcceptanceSessionID, p3UIAcceptanceWrongOperationID,
		9*60*60*1000, 9*1024*1024*1024, 99, 999_999, 120, 9, 99, state.baseAt+10_000,
	))

	wrongEvent := eventstore.LiveEventDTO{
		ID: "018f47a0-7c00-7000-8000-000000000010", IngestSequence: 1,
		Role: eventstore.EventRoleSource, Kind: eventstore.EventChat,
		ReceivedAt: state.baseAt, SessionOffsetMS: 0,
		DisplayName: "越界场次", Content: "P3UI_WRONG_SESSION_SHOULD_NOT_RENDER",
		ParseStatus: eventstore.ParseParsed,
	}
	a.emit(state.ctx, eventstore.LiveEventEventName, eventstore.LiveEventBatchDTO{
		SessionID: p3UIAcceptanceWrongSessionID, EmittedAt: state.baseAt,
		Events: []eventstore.LiveEventDTO{wrongEvent},
	})

	events := make([]eventstore.LiveEventDTO, 0, 100)
	for index := 0; index < p3UIAcceptanceEventCount; index++ {
		event := p3UIAcceptanceEvent(state.baseAt, index)
		events = append(events, event)
		if len(events) == 100 || index == p3UIAcceptanceEventCount-1 {
			a.emit(state.ctx, eventstore.LiveEventEventName, eventstore.LiveEventBatchDTO{
				SessionID: p3UIAcceptanceSessionID,
				EmittedAt: state.baseAt + int64(index),
				Events:    append([]eventstore.LiveEventDTO(nil), events...),
			})
			events = events[:0]
		}
	}
}

func (a *DesktopApp) emitP3UIAcceptanceReconnecting(state p3UIAcceptanceState) {
	now := time.Now().UTC().UnixMilli()
	if now < state.baseAt {
		now = state.baseAt
	}
	a.emit(state.ctx, room.StatusEventName, p3UIRoomStatus(
		state, room.RuntimeReconnecting, capture.RecordingReconnecting, 101,
		now, now+60_000, "录制连接中断，正在自动重试", "FFMPEG_EXITED",
	))
	a.emit(state.ctx, capture.RecordingProgressEventName, p3UIRecordingProgress(
		state, capture.RecordingReconnecting, p3UIAcceptanceSessionID, p3UIAcceptanceOperationID,
		126_000, 6*1024*1024, 3, 3_780, 0, 0, 2, state.baseAt+1_000,
	))
}

func (a *DesktopApp) emitP3UIAcceptanceRecovered(state p3UIAcceptanceState) {
	now := time.Now().UTC().UnixMilli()
	if now < state.baseAt {
		now = state.baseAt
	}
	a.emit(state.ctx, room.StatusEventName, p3UIRoomStatus(
		state, room.RuntimeRecording, capture.RecordingActive, 102,
		now, 0, "录制连接已恢复", "",
	))
	a.emit(state.ctx, capture.RecordingProgressEventName, p3UIRecordingProgress(
		state, capture.RecordingActive, p3UIAcceptanceSessionID, p3UIAcceptanceOperationID,
		130_000, 7*1024*1024, 4, 3_900, 29.8, 1.01, 2, state.baseAt+2_000,
	))
	a.emit(state.ctx, room.StatusEventName, room.RoomRuntimeStatus{
		RoomID: state.room.ID, LiveID: state.room.LiveID, Alias: state.room.Alias,
		State: room.RuntimeError, Revision: 101,
		SessionID: p3UIAcceptanceSessionID, OperationID: p3UIAcceptanceOperationID,
		RecordingStatus: capture.RecordingFailed, ChangedAt: now + 1,
		ErrorCode: "P3UI_STALE_SHOULD_NOT_RENDER", Message: "过期异常不得显示",
	})
	a.emit(state.ctx, capture.RecordingProgressEventName, p3UIRecordingProgress(
		state, capture.RecordingActive, p3UIAcceptanceSessionID, p3UIAcceptanceWrongOperationID,
		10*60*60*1000, 10*1024*1024*1024, 100, 1_000_000, 144, 10, 100, state.baseAt+3_000,
	))
}

func p3UIRoomStatus(
	state p3UIAcceptanceState,
	runtimeState room.RuntimeState,
	recordingStatus capture.RecordingStatus,
	revision, changedAt, retryAt int64,
	message, errorCode string,
) room.RoomRuntimeStatus {
	return room.RoomRuntimeStatus{
		RoomID: state.room.ID, LiveID: state.room.LiveID, Alias: state.room.Alias,
		State: runtimeState, Revision: revision,
		SessionID: p3UIAcceptanceSessionID, OperationID: p3UIAcceptanceOperationID,
		RecordingStatus: recordingStatus, ChangedAt: changedAt, RetryAt: retryAt,
		ErrorCode: errorCode, Message: message,
	}
}

func p3UIRecordingProgress(
	state p3UIAcceptanceState,
	recordingState capture.RecordingStatus,
	sessionID, operationID string,
	elapsedMS, bytesWritten, segmentCount, frame int64,
	fps, speed float64,
	restartCount int,
	updatedAt int64,
) capture.RecordingProgressDTO {
	return capture.RecordingProgressDTO{
		RoomID: state.room.ID, SessionID: sessionID, OperationID: operationID,
		State: recordingState, ElapsedMS: elapsedMS, BytesWritten: bytesWritten,
		SegmentCount: segmentCount, Frame: frame, FPS: fps, Speed: speed,
		RestartCount: restartCount, UpdatedAt: updatedAt,
	}
}

func p3UIAcceptanceEvent(baseAt int64, index int) eventstore.LiveEventDTO {
	kinds := []eventstore.EventKind{
		eventstore.EventChat, eventstore.EventGift, eventstore.EventLike,
		eventstore.EventMember, eventstore.EventFollow, eventstore.EventSystem,
	}
	kind := kinds[index%len(kinds)]
	content := fmt.Sprintf("验收事件 %04d", index+1)
	if index == p3UIAcceptanceEventCount-1 {
		kind = eventstore.EventChat
		content = "容量上限后最新事件"
	}
	result := eventstore.LiveEventDTO{
		ID:             fmt.Sprintf("018f47a0-7c00-7000-8000-%012x", index+0x100),
		IngestSequence: int64(index + 1), Role: eventstore.EventRoleSource,
		Kind: kind, ReceivedAt: baseAt + int64(index+1),
		SessionOffsetMS: int64(index) * 100,
		DisplayName:     fmt.Sprintf("观众-%02d", index%17),
		Content:         content, ParseStatus: eventstore.ParseParsed,
	}
	if kind == eventstore.EventGift || kind == eventstore.EventLike {
		value := float64(index%9 + 1)
		result.NumericValue = &value
	}
	return result
}

// ReportP3UIAcceptanceResult only exists in the p3uiacceptance build and
// writes one strict, sanitized result below the isolated acceptance root.
func (a *DesktopApp) ReportP3UIAcceptanceResult(payload string) error {
	if len(payload) == 0 || len(payload) > 8192 {
		return errors.New("acceptance result size is invalid")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(payload))
	decoder.DisallowUnknownFields()
	var result p3UIAcceptanceResult
	if err := decoder.Decode(&result); err != nil {
		return fmt.Errorf("decode acceptance result: %w", err)
	}
	if err := ensureP3UIAcceptanceJSONEOF(decoder); err != nil {
		return err
	}
	if err := validateP3UIAcceptanceResult(result); err != nil {
		return err
	}
	if result.Success {
		p3UIAcceptanceRegistry.Lock()
		state := p3UIAcceptanceRegistry.states[a]
		complete := state != nil && state.nextStage == 4
		p3UIAcceptanceRegistry.Unlock()
		if !complete {
			return errors.New("acceptance stages are incomplete")
		}
	}
	paths, err := loadP3UIAcceptancePaths()
	if err != nil {
		return err
	}
	if _, err := os.Stat(paths.ResultPath); err == nil || !errors.Is(err, os.ErrNotExist) {
		return errors.New("acceptance result already exists")
	}
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(paths.ResultPath), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(paths.ResultPath), ".p3-ui-acceptance-*.tmp")
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
	return os.Rename(temporaryPath, paths.ResultPath)
}

func (a *DesktopApp) writeP3UIAcceptanceFailure(code string) {
	payload, _ := json.Marshal(p3UIAcceptanceResult{
		Schema: p3UIAcceptanceSchema, Success: false, ErrorCode: code,
	})
	_ = a.ReportP3UIAcceptanceResult(string(payload))
}

func loadP3UIAcceptancePaths() (p3UIAcceptancePaths, error) {
	root, err := cleanP3UIAcceptanceAbsolutePath(os.Getenv("P3UIACC_ROOT"))
	if err != nil {
		return p3UIAcceptancePaths{}, fmt.Errorf("acceptance root is invalid: %w", err)
	}
	resultPath, err := cleanP3UIAcceptanceAbsolutePath(os.Getenv("P3UIACC_RESULT_PATH"))
	if err != nil {
		return p3UIAcceptancePaths{}, fmt.Errorf("acceptance result path is invalid: %w", err)
	}
	if !p3UIAcceptancePathWithin(root, resultPath, false) {
		return p3UIAcceptancePaths{}, errors.New("acceptance result escapes isolated root")
	}
	if filepath.Base(resultPath) != "p3-ui.result.json" {
		return p3UIAcceptancePaths{}, errors.New("acceptance result filename is invalid")
	}
	return p3UIAcceptancePaths{Root: root, ResultPath: resultPath}, nil
}

func cleanP3UIAcceptanceAbsolutePath(value string) (string, error) {
	cleaned := filepath.Clean(strings.TrimSpace(value))
	if cleaned == "." || !filepath.IsAbs(cleaned) {
		return "", errors.New("path must be absolute")
	}
	return cleaned, nil
}

func p3UIAcceptancePathWithin(root, candidate string, allowRoot bool) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	if relative == "." {
		return allowRoot
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func validateP3UIAcceptanceStorageRoot(root, storageRoot string) error {
	cleaned, err := cleanP3UIAcceptanceAbsolutePath(storageRoot)
	if err != nil {
		return fmt.Errorf("application storage root is invalid: %w", err)
	}
	if !p3UIAcceptancePathWithin(root, cleaned, true) {
		return errors.New("application storage root escapes isolated root")
	}
	return nil
}

func ensureP3UIAcceptanceJSONEOF(decoder *json.Decoder) error {
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

func validateP3UIAcceptanceResult(result p3UIAcceptanceResult) error {
	if result.Schema != p3UIAcceptanceSchema {
		return errors.New("acceptance result identity is invalid")
	}
	allowed := make(map[string]struct{}, len(p3UIAcceptanceChecks))
	for _, name := range p3UIAcceptanceChecks {
		allowed[name] = struct{}{}
	}
	for name := range result.Checks {
		if _, ok := allowed[name]; !ok {
			return errors.New("acceptance result contains an unknown check")
		}
	}
	if !result.Success {
		if !p3UIAcceptanceErrorCode.MatchString(result.ErrorCode) {
			return errors.New("acceptance failure code is invalid")
		}
		if len(result.Checks) != 0 || result.VisibleEventCount != 0 ||
			result.FilteredEventCount != 0 || result.AlertCount != 0 || result.StatusLabel != "" {
			return errors.New("acceptance failure result contains unexpected detail")
		}
		return nil
	}
	if result.ErrorCode != "" || len(result.Checks) != len(p3UIAcceptanceChecks) {
		return errors.New("successful acceptance result is incomplete")
	}
	for _, name := range p3UIAcceptanceChecks {
		if !result.Checks[name] {
			return errors.New("successful acceptance check is false")
		}
	}
	if result.VisibleEventCount != 2000 || result.FilteredEventCount <= 0 ||
		result.FilteredEventCount >= result.VisibleEventCount || result.AlertCount != 2 ||
		result.StatusLabel != "录制中" {
		return errors.New("successful acceptance evidence is invalid")
	}
	return nil
}
