//go:build p4accacceptance

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jwwsjlm/douyinLive/v2/internal/analysis"
	"github.com/jwwsjlm/douyinLive/v2/internal/playback"
	"github.com/jwwsjlm/douyinLive/v2/internal/room"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	p4AcceptanceSchema    = "P4-ACC-001/v1"
	p4AcceptanceSessionID = "019b0000-0000-7000-8000-000000000001"
	p4AcceptanceSegment1  = "019b0000-0000-7000-8000-000000000002"
	p4AcceptanceSegment2  = "019b0000-0000-7000-8000-000000000003"
	p4AcceptanceArtifact1 = "019b0000-0000-7000-8000-000000000004"
	p4AcceptanceArtifact2 = "019b0000-0000-7000-8000-000000000005"
	p4AcceptanceAlias     = "P4 一体化验收房间"
	p4AcceptanceTitle     = "P4 回放分析验收场次"
	p4AcceptanceStartedAt = int64(1_700_000_000_000)
	p4AcceptanceOffsetMS  = int64(250)
)

//go:embed p4_acc_acceptance.js
var p4AcceptanceScript string

var p4AcceptanceErrorCode = regexp.MustCompile(`^[A-Z0-9_]{1,64}$`)

var p4AcceptanceRegistry = struct {
	sync.Mutex
	states map[*DesktopApp]*p4AcceptanceState
}{states: make(map[*DesktopApp]*p4AcceptanceState)}

type p4AcceptancePaths struct {
	Root           string
	ResultPath     string
	ScreenshotPath string
	ExportRoot     string
}

type p4AcceptanceState struct {
	ctx             context.Context
	paths           p4AcceptancePaths
	timelineP95MS   int64
	reportStable    bool
	reportID        string
	analysisVersion string
}

type p4AcceptanceClientResult struct {
	Schema    string          `json:"schema"`
	Success   bool            `json:"success"`
	Checks    map[string]bool `json:"checks,omitempty"`
	ErrorCode string          `json:"errorCode,omitempty"`
}

type p4AcceptanceResult struct {
	Schema           string          `json:"schema"`
	Success          bool            `json:"success"`
	Checks           map[string]bool `json:"checks,omitempty"`
	TimelineP95MS    int64           `json:"timelineP95Ms,omitempty"`
	ReportStable     bool            `json:"reportStable,omitempty"`
	ExportFileCount  int             `json:"exportFileCount,omitempty"`
	ScreenshotSHA256 string          `json:"screenshotSha256,omitempty"`
	ScreenshotWidth  int             `json:"screenshotWidth,omitempty"`
	ScreenshotHeight int             `json:"screenshotHeight,omitempty"`
	ScreenshotColors int             `json:"screenshotColors,omitempty"`
	ErrorCode        string          `json:"errorCode,omitempty"`
}

type p4AcceptanceScreenshot struct {
	SHA256 string
	Width  int
	Height int
	Colors int
}

var p4AcceptanceChecks = []string{
	"historyVisible",
	"mediaDecoded",
	"crossSegmentAdvance",
	"timelineAligned",
	"analysisVisible",
	"asrDegraded",
	"exportVisible",
	"privacySafe",
	"layoutUsable",
}

func (a *DesktopApp) startAcceptanceHook(ctx context.Context) {
	paths, err := loadP4AcceptancePaths()
	if err != nil || filepath.Clean(a.infrastructureOptions.DataRoot) != paths.Root {
		a.writeP4AcceptanceFailure(paths, "HOOK_ISOLATION_INVALID")
		return
	}
	state, err := a.seedP4Acceptance(ctx, paths)
	if err != nil {
		a.writeP4AcceptanceFailure(paths, "FIXTURE_PREPARE_FAILED")
		return
	}
	p4AcceptanceRegistry.Lock()
	p4AcceptanceRegistry.states[a] = &state
	p4AcceptanceRegistry.Unlock()

	go func() {
		timer := time.NewTimer(750 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			runtime.WindowExecJS(ctx, p4AcceptanceScript)
		}
	}()
}

func (a *DesktopApp) seedP4Acceptance(ctx context.Context, paths p4AcceptancePaths) (p4AcceptanceState, error) {
	store := a.application.Store()
	if store == nil || store.Writer() == nil || store.Reader() == nil {
		return p4AcceptanceState{}, errors.New("store unavailable")
	}
	roomService, err := a.roomService()
	if err != nil {
		return p4AcceptanceState{}, err
	}
	rooms, err := roomService.ListRooms(ctx)
	if err != nil || len(rooms) != 0 {
		return p4AcceptanceState{}, errors.New("fixture root is not empty")
	}
	fixtureRoom, err := roomService.CreateRoom(ctx, room.CreateRoomInput{
		LiveID: "p4-acceptance-fixture", Alias: p4AcceptanceAlias,
		RecordingProfile: room.RecordingProfile{Quality: room.QualityHigh, SegmentMinutes: 10},
		MonitorEnabled:   false, RecordEnabled: true,
	})
	if err != nil {
		return p4AcceptanceState{}, err
	}

	media, err := createP4AcceptanceMedia(ctx, paths.Root)
	if err != nil {
		return p4AcceptanceState{}, err
	}
	if err := insertP4AcceptanceRows(ctx, store.Writer(), fixtureRoom.ID, media); err != nil {
		return p4AcceptanceState{}, err
	}
	service, err := a.analysisService()
	if err != nil {
		return p4AcceptanceState{}, err
	}
	first, err := service.AnalyzeSession(ctx, analysis.AnalyzeRequest{SessionID: p4AcceptanceSessionID})
	if err != nil {
		return p4AcceptanceState{}, err
	}
	second, err := service.AnalyzeSession(ctx, analysis.AnalyzeRequest{SessionID: p4AcceptanceSessionID})
	if err != nil {
		return p4AcceptanceState{}, err
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		return p4AcceptanceState{}, err
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		return p4AcceptanceState{}, err
	}
	reportStable := first.ID == second.ID && first.AnalysisVersion == second.AnalysisVersion && bytes.Equal(firstJSON, secondJSON)
	if !reportStable {
		return p4AcceptanceState{}, errors.New("analysis report was not reused")
	}
	timelineP95MS, err := p4AcceptanceTimelineP95(ctx, a.application.PlaybackService())
	if err != nil || timelineP95MS > 1500 {
		return p4AcceptanceState{}, errors.New("timeline acceptance failed")
	}
	return p4AcceptanceState{
		ctx: ctx, paths: paths, timelineP95MS: timelineP95MS,
		reportStable: true, reportID: first.ID, analysisVersion: first.AnalysisVersion,
	}, nil
}

type p4AcceptanceMedia struct {
	RelativePath string
	Size         int64
	SHA256       string
}

func createP4AcceptanceMedia(ctx context.Context, root string) ([2]p4AcceptanceMedia, error) {
	var result [2]p4AcceptanceMedia
	ffmpeg := strings.TrimSpace(os.Getenv("P4ACC_FFMPEG_PATH"))
	if ffmpeg == "" {
		var err error
		ffmpeg, err = exec.LookPath("ffmpeg")
		if err != nil {
			return result, err
		}
	}
	if !filepath.IsAbs(ffmpeg) {
		return result, errors.New("ffmpeg path is not absolute")
	}
	directory := filepath.Join(root, "p4-acceptance", "media")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return result, err
	}
	colors := []string{"0x176b87", "0xd97706"}
	frequencies := []string{"660", "880"}
	for index := range result {
		relative := filepath.ToSlash(filepath.Join("p4-acceptance", "media", fmt.Sprintf("playback-%02d.mp4", index+1)))
		absolute := filepath.Join(root, filepath.FromSlash(relative))
		command := exec.CommandContext(ctx, ffmpeg,
			"-hide_banner", "-loglevel", "error", "-nostdin", "-n",
			"-f", "lavfi", "-i", "color=c="+colors[index]+":s=640x360:r=25",
			"-f", "lavfi", "-i", "sine=frequency="+frequencies[index]+":sample_rate=48000",
			"-t", "4", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
			"-c:a", "aac", "-movflags", "+faststart", absolute,
		)
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		if err := command.Run(); err != nil {
			return result, err
		}
		info, err := os.Lstat(absolute)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 {
			return result, errors.New("generated media is invalid")
		}
		digest, err := p4AcceptanceFileSHA256(absolute)
		if err != nil {
			return result, err
		}
		result[index] = p4AcceptanceMedia{RelativePath: relative, Size: info.Size(), SHA256: digest}
	}
	return result, nil
}

func insertP4AcceptanceRows(ctx context.Context, writer *sql.DB, roomID string, media [2]p4AcceptanceMedia) (resultErr error) {
	tx, err := writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, tx.Rollback())
		}
	}()
	endedAt := p4AcceptanceStartedAt + p4AcceptanceOffsetMS + 8000
	if _, err := tx.ExecContext(ctx, `INSERT INTO live_sessions(
		id, room_config_id, title, status, recording_status, operation_id,
		manifest_dirty, started_at, ended_at, media_epoch_at, capture_offset_ms,
		clock_source, integrity_score, data_path, schema_version, created_at, updated_at
	) VALUES (?, ?, ?, 'completed', 'completed', 'p4-acceptance-operation',
		0, ?, ?, ?, ?, 'calibrated', 1, 'p4-acceptance/session', 1, ?, ?)`,
		p4AcceptanceSessionID, roomID, p4AcceptanceTitle,
		p4AcceptanceStartedAt, endedAt, p4AcceptanceStartedAt+p4AcceptanceOffsetMS,
		p4AcceptanceOffsetMS, p4AcceptanceStartedAt, endedAt,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_media(
		session_id, relative_path, state, manifest_revision, manifest_dirty,
		media_epoch_at, attempts_json, created_at, updated_at
	) VALUES (?, 'p4-acceptance/media', 'completed', 1, 0, ?, '[]', ?, ?)`,
		p4AcceptanceSessionID, p4AcceptanceStartedAt+p4AcceptanceOffsetMS,
		p4AcceptanceStartedAt, endedAt,
	); err != nil {
		return err
	}
	segmentIDs := []string{p4AcceptanceSegment1, p4AcceptanceSegment2}
	artifactIDs := []string{p4AcceptanceArtifact1, p4AcceptanceArtifact2}
	for index := range media {
		ptsStart := int64(index) * 4000
		ptsEnd := ptsStart + 4000
		startedAt := p4AcceptanceStartedAt + p4AcceptanceOffsetMS + ptsStart
		segmentHash := sha256.Sum256([]byte(fmt.Sprintf("p4-acceptance-source-%d", index+1)))
		segmentDigest := hex.EncodeToString(segmentHash[:])
		if _, err := tx.ExecContext(ctx, `INSERT INTO media_segments(
			id, session_id, sequence, relative_path, container, video_codec, audio_codec,
			started_at, ended_at, pts_start_ms, pts_end_ms, duration_ms, size_bytes,
			sha256, status
		) VALUES (?, ?, ?, ?, 'mkv', 'h264', 'aac', ?, ?, ?, ?, 4000, ?, ?, 'complete')`,
			segmentIDs[index], p4AcceptanceSessionID, index+1,
			fmt.Sprintf("p4-acceptance/media/source-%02d.mkv", index+1),
			startedAt, startedAt+4000, ptsStart, ptsEnd, media[index].Size, segmentDigest,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO media_artifacts(
			id, session_id, media_segment_id, kind, relative_path, container, codec,
			duration_ms, size_bytes, sha256, source_sha256, status, created_at, updated_at
		) VALUES (?, ?, ?, 'playback_mp4', ?, 'mp4', 'h264', 4000, ?, ?, ?,
			'complete', ?, ?)`,
			artifactIDs[index], p4AcceptanceSessionID, segmentIDs[index], media[index].RelativePath,
			media[index].Size, media[index].SHA256, segmentDigest, p4AcceptanceStartedAt, endedAt,
		); err != nil {
			return err
		}
	}
	events := []struct {
		id, kind, name, content string
		sequence, offset        int64
	}{
		{"019b0000-0000-7000-8000-000000000011", "chat", "观众甲", "第一分片互动", 1, 1000},
		{"019b0000-0000-7000-8000-000000000012", "like", "观众乙", "点赞", 2, 3000},
		{"019b0000-0000-7000-8000-000000000013", "chat", "观众丙", "第二分片互动", 3, 4500},
		{"019b0000-0000-7000-8000-000000000014", "gift", "观众丁", "礼物", 4, 7000},
	}
	for _, event := range events {
		if _, err := tx.ExecContext(ctx, `INSERT INTO live_events(
			id, session_id, ingest_sequence, event_role, method, kind, dedupe_key,
			received_at, session_offset_ms, clock_confidence, user_hash, display_name,
			content, numeric_value, normalized_json, parse_status, normalizer_version
		) VALUES (?, ?, ?, 'source', 'P4AcceptanceMessage', ?, ?, ?, ?, 1,
			'p4-acceptance-user', ?, ?, 1, '{}', 'parsed', 'event-normalizer/v1')`,
			event.id, p4AcceptanceSessionID, event.sequence, event.kind, "p4acc-"+event.id,
			p4AcceptanceStartedAt+event.offset, event.offset, event.name, event.content,
		); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func p4AcceptanceTimelineP95(ctx context.Context, service *playback.Service) (int64, error) {
	if service == nil {
		return 0, errors.New("playback service unavailable")
	}
	samples := []int64{0, 250, 500, 750, 1000, 1500, 2000, 2500, 3000, 3500, 4000, 4250, 4500, 5000, 5500, 6000, 6500, 7000, 7500, 7999}
	errorsMS := make([]int64, 0, len(samples))
	for _, offset := range samples {
		location, err := service.LocateMedia(ctx, playback.MediaLocationRequest{
			SessionID: p4AcceptanceSessionID, SessionOffsetMS: offset,
		})
		if err != nil || location.State != playback.MediaLocationPlaybackMP4 || location.SegmentPlaybackMS == nil {
			return 0, errors.New("calibration sample is not playable")
		}
		expected := offset % 4000
		delta := *location.SegmentPlaybackMS - expected
		if delta < 0 {
			delta = -delta
		}
		errorsMS = append(errorsMS, delta)
	}
	slices.Sort(errorsMS)
	index := (95*len(errorsMS)+99)/100 - 1
	return errorsMS[index], nil
}

func (a *DesktopApp) ReportP4AcceptanceResult(payload string) error {
	client, err := decodeP4AcceptanceClientResult(payload)
	if err != nil {
		return err
	}
	p4AcceptanceRegistry.Lock()
	state := p4AcceptanceRegistry.states[a]
	p4AcceptanceRegistry.Unlock()
	if state == nil {
		return errors.New("acceptance state is unavailable")
	}
	if !client.Success {
		return a.writeP4AcceptanceResult(state.paths, p4AcceptanceResult{
			Schema: p4AcceptanceSchema, Success: false, ErrorCode: client.ErrorCode,
		})
	}
	exportFileCount, err := verifyP4AcceptanceExport(state.paths.ExportRoot)
	if err != nil {
		return errors.New("EXPORT_VERIFY_FAILED")
	}
	screenshot, err := captureP4AcceptanceWindow(state.paths.ScreenshotPath)
	if err != nil {
		return errors.New("SCREENSHOT_CAPTURE_FAILED")
	}
	result := p4AcceptanceResult{
		Schema: p4AcceptanceSchema, Success: true, Checks: client.Checks,
		TimelineP95MS: state.timelineP95MS, ReportStable: state.reportStable,
		ExportFileCount: exportFileCount, ScreenshotSHA256: screenshot.SHA256,
		ScreenshotWidth: screenshot.Width, ScreenshotHeight: screenshot.Height,
		ScreenshotColors: screenshot.Colors,
	}
	if err := a.writeP4AcceptanceResult(state.paths, result); err != nil {
		return err
	}
	go func() {
		timer := time.NewTimer(750 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-state.ctx.Done():
		case <-timer.C:
			runtime.Quit(state.ctx)
		}
	}()
	return nil
}

func decodeP4AcceptanceClientResult(payload string) (p4AcceptanceClientResult, error) {
	if len(payload) == 0 || len(payload) > 8192 {
		return p4AcceptanceClientResult{}, errors.New("acceptance result size is invalid")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(payload))
	decoder.DisallowUnknownFields()
	var result p4AcceptanceClientResult
	if err := decoder.Decode(&result); err != nil {
		return p4AcceptanceClientResult{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return p4AcceptanceClientResult{}, errors.New("acceptance result contains trailing JSON")
	}
	if err := validateP4AcceptanceClientResult(result); err != nil {
		return p4AcceptanceClientResult{}, err
	}
	return result, nil
}

func validateP4AcceptanceClientResult(result p4AcceptanceClientResult) error {
	if result.Schema != p4AcceptanceSchema {
		return errors.New("acceptance result identity is invalid")
	}
	if !result.Success {
		if !p4AcceptanceErrorCode.MatchString(result.ErrorCode) || len(result.Checks) != 0 {
			return errors.New("acceptance failure result is invalid")
		}
		return nil
	}
	if result.ErrorCode != "" || len(result.Checks) != len(p4AcceptanceChecks) {
		return errors.New("acceptance success result is incomplete")
	}
	allowed := make(map[string]struct{}, len(p4AcceptanceChecks))
	for _, name := range p4AcceptanceChecks {
		allowed[name] = struct{}{}
		if !result.Checks[name] {
			return errors.New("acceptance success check is false")
		}
	}
	for name := range result.Checks {
		if _, ok := allowed[name]; !ok {
			return errors.New("acceptance result contains an unknown check")
		}
	}
	return nil
}

func verifyP4AcceptanceExport(root string) (int, error) {
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() || entries[0].Type()&os.ModeSymlink != 0 {
		return 0, errors.New("acceptance export directory is invalid")
	}
	directory := filepath.Join(root, entries[0].Name())
	files, err := os.ReadDir(directory)
	if err != nil || len(files) != 5 {
		return 0, errors.New("acceptance export file count is invalid")
	}
	want := []string{"events.csv", "manifest.json", "media-segments.csv", "metric-buckets.csv", "transcripts.csv"}
	got := make([]string, 0, len(files))
	combined := make([]byte, 0, 64*1024)
	for _, file := range files {
		if file.IsDir() || file.Type()&os.ModeSymlink != 0 {
			return 0, errors.New("acceptance export contains a non-regular file")
		}
		got = append(got, file.Name())
		content, err := os.ReadFile(filepath.Join(directory, file.Name()))
		if err != nil || len(content) == 0 {
			return 0, errors.New("acceptance export file is unreadable")
		}
		if strings.HasSuffix(file.Name(), ".csv") && !bytes.HasPrefix(content, []byte{0xef, 0xbb, 0xbf}) {
			return 0, errors.New("acceptance export CSV has no BOM")
		}
		combined = append(combined, content...)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) || !bytes.Contains(combined, []byte(`"schema": "analysis-export/v1"`)) {
		return 0, errors.New("acceptance export contract is invalid")
	}
	for _, forbidden := range []string{"观众甲", "观众乙", "观众丙", "观众丁", "p4-acceptance-operation", "p4-acceptance/session", "msToken", "a_bogus", "https://", "http://"} {
		if bytes.Contains(combined, []byte(forbidden)) {
			return 0, errors.New("acceptance export crossed the privacy boundary")
		}
	}
	return len(files), nil
}

func (a *DesktopApp) writeP4AcceptanceFailure(paths p4AcceptancePaths, code string) {
	if paths.ResultPath == "" || !p4AcceptanceErrorCode.MatchString(code) {
		return
	}
	_ = a.writeP4AcceptanceResult(paths, p4AcceptanceResult{
		Schema: p4AcceptanceSchema, Success: false, ErrorCode: code,
	})
	go func() {
		time.Sleep(500 * time.Millisecond)
		runtime.Quit(context.Background())
	}()
}

func (a *DesktopApp) writeP4AcceptanceResult(paths p4AcceptancePaths, result p4AcceptanceResult) error {
	if _, err := os.Lstat(paths.ResultPath); err == nil || !errors.Is(err, os.ErrNotExist) {
		return errors.New("acceptance result already exists")
	}
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(paths.Root, ".p4-acceptance-*.tmp")
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

func loadP4AcceptancePaths() (p4AcceptancePaths, error) {
	root, err := cleanP4AcceptancePath(os.Getenv("P4ACC_ROOT"))
	if err != nil {
		return p4AcceptancePaths{}, err
	}
	result, err := cleanP4AcceptancePath(os.Getenv("P4ACC_RESULT_PATH"))
	if err != nil || filepath.Base(result) != "p4-acc.result.json" || !p4AcceptancePathWithin(root, result) {
		return p4AcceptancePaths{}, errors.New("acceptance result path is invalid")
	}
	return p4AcceptancePaths{
		Root: root, ResultPath: result,
		ScreenshotPath: filepath.Join(root, "p4-acc.png"),
		ExportRoot:     filepath.Join(root, "exports"),
	}, nil
}

func cleanP4AcceptancePath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("acceptance path is empty")
	}
	cleaned, err := filepath.Abs(filepath.Clean(value))
	if err != nil || !filepath.IsAbs(cleaned) {
		return "", errors.New("acceptance path is invalid")
	}
	return cleaned, nil
}

func p4AcceptancePathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && !filepath.IsAbs(relative) && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func p4AcceptanceFileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
