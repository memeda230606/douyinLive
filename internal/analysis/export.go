package analysis

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	ExportContractVersion = 1
	ExportSchema          = "analysis-export/v1"
	maxExportEvents       = 1_000_000
	maxExportTranscripts  = 1_000_000
	maxExportSegments     = 4096
	utcMillisLayout       = "2006-01-02T15:04:05.000Z"
)

var (
	ErrExportUnavailable = errors.New("analysis export is unavailable")
	ErrExportFailed      = errors.New("analysis export failed")
)

type ExportRequest struct {
	SessionID   string `json:"sessionId"`
	IncludeText bool   `json:"includeText"`
}

type ExportFileDTO struct {
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
	RowCount  int    `json:"rowCount"`
	SizeBytes int64  `json:"sizeBytes"`
	SHA256    string `json:"sha256"`
}

type ExportResultDTO struct {
	Version       int             `json:"version"`
	ExportID      string          `json:"exportId"`
	DirectoryName string          `json:"directoryName"`
	GeneratedAt   string          `json:"generatedAt"`
	IncludeText   bool            `json:"includeText"`
	Files         []ExportFileDTO `json:"files"`
}

type exportSession struct {
	ID             string  `json:"id"`
	Status         string  `json:"status"`
	StartedAtUTC   string  `json:"startedAtUtc"`
	EndedAtUTC     string  `json:"endedAtUtc"`
	DurationMS     int64   `json:"durationMs"`
	IntegrityScore float64 `json:"integrityScore"`
}

type exportReport struct {
	ID               string         `json:"id"`
	Status           string         `json:"status"`
	AnalysisVersion  string         `json:"analysisVersion"`
	AlgorithmVersion string         `json:"algorithmVersion"`
	StartedAtUTC     string         `json:"startedAtUtc"`
	CompletedAtUTC   string         `json:"completedAtUtc"`
	Summary          SummaryDTO     `json:"summary"`
	Peaks            []CandidateDTO `json:"peaks"`
	Troughs          []CandidateDTO `json:"troughs"`
	Highlights       []CandidateDTO `json:"highlights"`
}

type exportPrivacy struct {
	Mode           string   `json:"mode"`
	TextIncluded   bool     `json:"textIncluded"`
	UserIdentifier string   `json:"userIdentifier"`
	ExcludedFields []string `json:"excludedFields"`
}

type exportManifest struct {
	Version     int             `json:"version"`
	Schema      string          `json:"schema"`
	GeneratedAt string          `json:"generatedAt"`
	Session     exportSession   `json:"session"`
	Report      exportReport    `json:"report"`
	Privacy     exportPrivacy   `json:"privacy"`
	Files       []ExportFileDTO `json:"files"`
}

func (s *Service) ExportAnalysis(ctx context.Context, request ExportRequest) (result ExportResultDTO, resultErr error) {
	if ctx == nil || !validUUIDv7(request.SessionID) {
		return ExportResultDTO{}, ErrInvalidArgument
	}
	if s == nil || s.reader == nil || s.exportRoot == "" {
		return ExportResultDTO{}, ErrExportUnavailable
	}
	if err := ctx.Err(); err != nil {
		return ExportResultDTO{}, err
	}
	s.exportMu.Lock()
	defer s.exportMu.Unlock()
	result, err := s.exportAnalysis(ctx, request)
	if err == nil {
		return result, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ExportResultDTO{}, ctxErr
	}
	switch {
	case errors.Is(err, ErrInvalidArgument), errors.Is(err, ErrSessionNotFound),
		errors.Is(err, ErrReportNotFound), errors.Is(err, ErrAnalysisNotReady),
		errors.Is(err, ErrInputCorrupt), errors.Is(err, ErrInputTooLarge),
		errors.Is(err, ErrExportUnavailable):
		return ExportResultDTO{}, err
	default:
		return ExportResultDTO{}, ErrExportFailed
	}
}

func (s *Service) exportAnalysis(ctx context.Context, request ExportRequest) (result ExportResultDTO, resultErr error) {
	rootInfo, err := os.Lstat(s.exportRoot)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return ExportResultDTO{}, ErrExportUnavailable
	}
	root, err := os.OpenRoot(s.exportRoot)
	if err != nil {
		return ExportResultDTO{}, ErrExportUnavailable
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()

	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ExportResultDTO{}, err
	}
	defer tx.Rollback()
	session, report, err := loadExportSnapshot(ctx, tx, request.SessionID)
	if err != nil {
		return ExportResultDTO{}, err
	}

	exportID, err := newUUIDv7()
	if err != nil {
		return ExportResultDTO{}, err
	}
	temporaryName := ".analysis-export-" + exportID
	finalName := "analysis-" + report.ID + "-" + exportID
	if filepath.Base(temporaryName) != temporaryName || filepath.Base(finalName) != finalName {
		return ExportResultDTO{}, ErrInputCorrupt
	}
	if err := root.Mkdir(temporaryName, 0o700); err != nil {
		return ExportResultDTO{}, err
	}
	published := false
	defer func() {
		if !published {
			resultErr = errors.Join(resultErr, root.RemoveAll(temporaryName))
		}
	}()

	files := make([]ExportFileDTO, 0, 4)
	for _, writer := range []func(context.Context, *os.Root, string, *sql.Tx, exportSession, ReportDTO, bool) (ExportFileDTO, error){
		writeEventsCSV, writeMetricBucketsCSV, writeTranscriptsCSV, writeMediaSegmentsCSV,
	} {
		metadata, writeErr := writer(ctx, root, temporaryName, tx, session, report, request.IncludeText)
		if writeErr != nil {
			return ExportResultDTO{}, writeErr
		}
		files = append(files, metadata)
	}
	generatedAt, err := formatUTCMS(s.now().UTC().UnixMilli())
	if err != nil {
		return ExportResultDTO{}, err
	}
	manifest := exportManifest{
		Version: ExportContractVersion, Schema: ExportSchema, GeneratedAt: generatedAt,
		Session: session,
		Report: exportReport{
			ID: report.ID, Status: report.Status, AnalysisVersion: report.AnalysisVersion,
			AlgorithmVersion: report.AlgorithmVersion,
			StartedAtUTC:     mustFormatUTCMS(report.StartedAt), CompletedAtUTC: mustFormatUTCMS(report.CompletedAt),
			Summary: report.Summary, Peaks: report.Peaks, Troughs: report.Troughs, Highlights: report.Highlights,
		},
		Privacy: exportPrivacy{
			Mode: "privacy-safe", TextIncluded: request.IncludeText, UserIdentifier: "hmac-sha256",
			ExcludedFields: []string{
				"cookie", "dataPath", "displayName", "mediaPath", "mediaSha256", "normalizedPayload",
				"platformMessageId", "platformRoomId", "rawPayload", "signature", "sourceAudioSha256", "streamUrl",
			},
		},
		Files: append([]ExportFileDTO(nil), files...),
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return ExportResultDTO{}, ErrInputCorrupt
	}
	manifestBytes = append(manifestBytes, '\n')
	manifestFile, err := writeBytesFile(root, temporaryName, "manifest.json", "application/json", 1, manifestBytes)
	if err != nil {
		return ExportResultDTO{}, err
	}
	allFiles := append(files, manifestFile)
	if s.beforeExportPublish != nil {
		if err := s.beforeExportPublish(); err != nil {
			return ExportResultDTO{}, err
		}
	}
	if err := root.Rename(temporaryName, finalName); err != nil {
		return ExportResultDTO{}, err
	}
	published = true
	if err := tx.Rollback(); err != nil {
		return ExportResultDTO{}, err
	}
	return ExportResultDTO{
		Version: ExportContractVersion, ExportID: exportID, DirectoryName: finalName,
		GeneratedAt: generatedAt, IncludeText: request.IncludeText, Files: allFiles,
	}, nil
}

func loadExportSnapshot(ctx context.Context, tx *sql.Tx, sessionID string) (exportSession, ReportDTO, error) {
	var status string
	var startedAt int64
	var endedAt sql.NullInt64
	var integrity float64
	err := tx.QueryRowContext(ctx, `SELECT status, started_at, ended_at, integrity_score
		FROM live_sessions WHERE id = ?`, sessionID).Scan(&status, &startedAt, &endedAt, &integrity)
	if errors.Is(err, sql.ErrNoRows) {
		return exportSession{}, ReportDTO{}, ErrSessionNotFound
	}
	if err != nil {
		return exportSession{}, ReportDTO{}, err
	}
	if !endedAt.Valid || (status != "completed" && status != "interrupted" && status != "failed") {
		return exportSession{}, ReportDTO{}, ErrAnalysisNotReady
	}
	if endedAt.Int64 <= startedAt || math.IsNaN(integrity) || math.IsInf(integrity, 0) || integrity < 0 || integrity > 1 {
		return exportSession{}, ReportDTO{}, ErrInputCorrupt
	}
	startedUTC, err := formatUTCMS(startedAt)
	if err != nil {
		return exportSession{}, ReportDTO{}, err
	}
	endedUTC, err := formatUTCMS(endedAt.Int64)
	if err != nil {
		return exportSession{}, ReportDTO{}, err
	}
	var reportID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM analysis_reports
		WHERE session_id = ? AND status = 'completed' AND analysis_version LIKE ?
		ORDER BY completed_at DESC, id DESC LIMIT 1`, sessionID, AlgorithmVersion+"+%").Scan(&reportID)
	if errors.Is(err, sql.ErrNoRows) {
		return exportSession{}, ReportDTO{}, ErrReportNotFound
	}
	if err != nil {
		return exportSession{}, ReportDTO{}, err
	}
	report, err := getReportByIDFrom(ctx, tx, sessionID, reportID)
	if err != nil {
		return exportSession{}, ReportDTO{}, err
	}
	if _, err := formatUTCMS(report.StartedAt); err != nil {
		return exportSession{}, ReportDTO{}, err
	}
	if _, err := formatUTCMS(report.CompletedAt); err != nil {
		return exportSession{}, ReportDTO{}, err
	}
	return exportSession{
		ID: sessionID, Status: status, StartedAtUTC: startedUTC, EndedAtUTC: endedUTC,
		DurationMS: endedAt.Int64 - startedAt, IntegrityScore: integrity,
	}, report, nil
}

func writeEventsCSV(ctx context.Context, root *os.Root, directory string, tx *sql.Tx, session exportSession, _ ReportDTO, includeText bool) (ExportFileDTO, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, session_offset_ms, user_hash,
		content, numeric_value, parse_status FROM live_events
		WHERE session_id = ? AND event_role = 'source'
		ORDER BY session_offset_ms ASC, ingest_sequence ASC, id ASC LIMIT ?`, session.ID, maxExportEvents+1)
	if err != nil {
		return ExportFileDTO{}, err
	}
	defer rows.Close()
	return writeCSVFile(root, directory, "events.csv", []string{
		"event_id", "event_at_utc", "session_offset_ms", "kind", "user_hash", "content", "numeric_value", "parse_status",
	}, func(writer *csv.Writer) (int, error) {
		count := 0
		for rows.Next() {
			if count == maxExportEvents {
				return 0, ErrInputTooLarge
			}
			var id, kind, parseStatus string
			var offset int64
			var userHash, content sql.NullString
			var numeric sql.NullFloat64
			if err := rows.Scan(&id, &kind, &offset, &userHash, &content, &numeric, &parseStatus); err != nil {
				return 0, err
			}
			instant, err := addMillisUTC(session.StartedAtUTC, offset)
			if err != nil || offset < 0 {
				return 0, ErrInputCorrupt
			}
			text := ""
			if includeText && content.Valid {
				text = safeCSVText(content.String)
			}
			if err := writer.Write([]string{
				id, instant, strconv.FormatInt(offset, 10), safeCSVText(kind), safeCSVText(userHash.String),
				text, nullableFloat(numeric), safeCSVText(parseStatus),
			}); err != nil {
				return 0, err
			}
			count++
		}
		return count, rows.Err()
	})
}

func writeMetricBucketsCSV(ctx context.Context, root *os.Root, directory string, _ *sql.Tx, session exportSession, report ReportDTO, _ bool) (ExportFileDTO, error) {
	return writeCSVFile(root, directory, "metric-buckets.csv", []string{
		"bucket_at_utc", "bucket_start_ms", "bucket_size_ms", "chat_count", "unique_chatters", "like_delta",
		"gift_count", "gift_value", "follow_count", "enter_count", "active_users", "message_total", "completeness",
	}, func(writer *csv.Writer) (int, error) {
		for _, bucket := range report.Buckets {
			instant, err := addMillisUTC(session.StartedAtUTC, bucket.BucketStartMS)
			if err != nil {
				return 0, err
			}
			giftValue := ""
			if bucket.GiftValue != nil {
				giftValue = strconv.FormatFloat(*bucket.GiftValue, 'g', -1, 64)
			}
			if err := writer.Write([]string{
				instant, strconv.FormatInt(bucket.BucketStartMS, 10), strconv.FormatInt(bucket.BucketSizeMS, 10),
				strconv.FormatInt(bucket.ChatCount, 10), strconv.FormatInt(bucket.UniqueChatters, 10),
				strconv.FormatInt(bucket.LikeDelta, 10), strconv.FormatInt(bucket.GiftCount, 10), giftValue,
				strconv.FormatInt(bucket.FollowCount, 10), strconv.FormatInt(bucket.EnterCount, 10),
				strconv.FormatInt(bucket.ActiveUsers, 10), strconv.FormatInt(bucket.MessageTotal, 10),
				strconv.FormatFloat(bucket.Completeness, 'g', -1, 64),
			}); err != nil {
				return 0, err
			}
		}
		return len(report.Buckets), nil
	})
}

func writeTranscriptsCSV(ctx context.Context, root *os.Root, directory string, tx *sql.Tx, session exportSession, _ ReportDTO, includeText bool) (ExportFileDTO, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, start_ms, end_ms, text, confidence,
		speaker, provider, model, language, analysis_version FROM transcript_segments
		WHERE session_id = ? ORDER BY start_ms ASC, end_ms ASC, id ASC LIMIT ?`, session.ID, maxExportTranscripts+1)
	if err != nil {
		return ExportFileDTO{}, err
	}
	defer rows.Close()
	return writeCSVFile(root, directory, "transcripts.csv", []string{
		"transcript_id", "start_at_utc", "start_ms", "end_ms", "text", "confidence",
		"speaker", "provider", "model", "language", "analysis_version",
	}, func(writer *csv.Writer) (int, error) {
		count := 0
		for rows.Next() {
			if count == maxExportTranscripts {
				return 0, ErrInputTooLarge
			}
			var id, text, provider, model, language, analysisVersion string
			var start, end int64
			var confidence sql.NullFloat64
			var speaker sql.NullString
			if err := rows.Scan(&id, &start, &end, &text, &confidence, &speaker, &provider, &model, &language, &analysisVersion); err != nil {
				return 0, err
			}
			instant, err := addMillisUTC(session.StartedAtUTC, start)
			if err != nil || start < 0 || end < start {
				return 0, ErrInputCorrupt
			}
			if !includeText {
				text = ""
				speaker.String = ""
			} else {
				text = safeCSVText(text)
			}
			if err := writer.Write([]string{
				id, instant, strconv.FormatInt(start, 10), strconv.FormatInt(end, 10), text,
				nullableFloat(confidence), safeCSVText(speaker.String), safeCSVText(provider), safeCSVText(model),
				safeCSVText(language), safeCSVText(analysisVersion),
			}); err != nil {
				return 0, err
			}
			count++
		}
		return count, rows.Err()
	})
}

func writeMediaSegmentsCSV(ctx context.Context, root *os.Root, directory string, tx *sql.Tx, session exportSession, _ ReportDTO, _ bool) (ExportFileDTO, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, sequence, container, video_codec, audio_codec,
		started_at, ended_at, duration_ms, size_bytes, status FROM media_segments
		WHERE session_id = ? ORDER BY sequence ASC, id ASC LIMIT ?`, session.ID, maxExportSegments+1)
	if err != nil {
		return ExportFileDTO{}, err
	}
	defer rows.Close()
	return writeCSVFile(root, directory, "media-segments.csv", []string{
		"segment_id", "sequence", "started_at_utc", "ended_at_utc", "start_offset_ms", "end_offset_ms",
		"duration_ms", "size_bytes", "container", "video_codec", "audio_codec", "status",
	}, func(writer *csv.Writer) (int, error) {
		count := 0
		base, err := parseUTCMS(session.StartedAtUTC)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			if count == maxExportSegments {
				return 0, ErrInputTooLarge
			}
			var id, container, status string
			var sequence, startedAt, endedAt, duration, size int64
			var videoCodec, audioCodec sql.NullString
			if err := rows.Scan(&id, &sequence, &container, &videoCodec, &audioCodec, &startedAt, &endedAt, &duration, &size, &status); err != nil {
				return 0, err
			}
			if sequence <= 0 || endedAt < startedAt || duration < 0 || size < 0 || startedAt < base || endedAt < base {
				return 0, ErrInputCorrupt
			}
			startedUTC, err := formatUTCMS(startedAt)
			if err != nil {
				return 0, err
			}
			endedUTC, err := formatUTCMS(endedAt)
			if err != nil {
				return 0, err
			}
			if err := writer.Write([]string{
				id, strconv.FormatInt(sequence, 10), startedUTC, endedUTC,
				strconv.FormatInt(startedAt-base, 10), strconv.FormatInt(endedAt-base, 10),
				strconv.FormatInt(duration, 10), strconv.FormatInt(size, 10), safeCSVText(container),
				safeCSVText(videoCodec.String), safeCSVText(audioCodec.String), safeCSVText(status),
			}); err != nil {
				return 0, err
			}
			count++
		}
		return count, rows.Err()
	})
}

func writeCSVFile(root *os.Root, directory, name string, header []string, writeRows func(*csv.Writer) (int, error)) (metadata ExportFileDTO, resultErr error) {
	path := filepath.Join(directory, name)
	file, err := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return ExportFileDTO{}, err
	}
	closed := false
	defer func() {
		if !closed {
			resultErr = errors.Join(resultErr, file.Close())
		}
	}()
	digest := sha256.New()
	output := io.MultiWriter(file, digest)
	if _, err := output.Write([]byte{0xef, 0xbb, 0xbf}); err != nil {
		return ExportFileDTO{}, err
	}
	writer := csv.NewWriter(output)
	if err := writer.Write(header); err != nil {
		return ExportFileDTO{}, err
	}
	rowCount, err := writeRows(writer)
	if err != nil {
		return ExportFileDTO{}, err
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return ExportFileDTO{}, err
	}
	if err := file.Sync(); err != nil {
		return ExportFileDTO{}, err
	}
	if err := file.Close(); err != nil {
		return ExportFileDTO{}, err
	}
	closed = true
	info, err := root.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ExportFileDTO{}, ErrExportFailed
	}
	return ExportFileDTO{
		Name: name, MediaType: "text/csv; charset=utf-8", RowCount: rowCount,
		SizeBytes: info.Size(), SHA256: hex.EncodeToString(digest.Sum(nil)),
	}, nil
}

func writeBytesFile(root *os.Root, directory, name, mediaType string, rows int, content []byte) (metadata ExportFileDTO, resultErr error) {
	path := filepath.Join(directory, name)
	file, err := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return ExportFileDTO{}, err
	}
	closed := false
	defer func() {
		if !closed {
			resultErr = errors.Join(resultErr, file.Close())
		}
	}()
	digest := sha256.Sum256(content)
	if _, err := file.Write(content); err != nil {
		return ExportFileDTO{}, err
	}
	if err := file.Sync(); err != nil {
		return ExportFileDTO{}, err
	}
	if err := file.Close(); err != nil {
		return ExportFileDTO{}, err
	}
	closed = true
	info, err := root.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(content)) {
		return ExportFileDTO{}, ErrExportFailed
	}
	return ExportFileDTO{
		Name: name, MediaType: mediaType, RowCount: rows, SizeBytes: info.Size(),
		SHA256: hex.EncodeToString(digest[:]),
	}, nil
}

func nullableFloat(value sql.NullFloat64) string {
	if !value.Valid {
		return ""
	}
	return strconv.FormatFloat(value.Float64, 'g', -1, 64)
}

func safeCSVText(value string) string {
	trimmed := strings.TrimLeft(value, " \t\r\n\ufeff")
	if trimmed != "" && strings.ContainsRune("=+-@", rune(trimmed[0])) {
		return "'" + value
	}
	return value
}

func formatUTCMS(milliseconds int64) (string, error) {
	if milliseconds < -62135596800000 || milliseconds > 253402300799999 {
		return "", ErrInputCorrupt
	}
	return time.UnixMilli(milliseconds).UTC().Format(utcMillisLayout), nil
}

func mustFormatUTCMS(milliseconds int64) string {
	value, _ := formatUTCMS(milliseconds)
	return value
}

func parseUTCMS(value string) (int64, error) {
	parsed, err := time.Parse(utcMillisLayout, value)
	if err != nil {
		return 0, ErrInputCorrupt
	}
	return parsed.UnixMilli(), nil
}

func addMillisUTC(baseUTC string, offset int64) (string, error) {
	base, err := parseUTCMS(baseUTC)
	if err != nil || offset < 0 || base > math.MaxInt64-offset {
		return "", ErrInputCorrupt
	}
	return formatUTCMS(base + offset)
}
