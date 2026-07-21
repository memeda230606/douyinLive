package analysis

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	maxAnalysisEvents = 1_000_000
	maxAnalysisGaps   = 100_000
)

type ServiceOptions struct {
	Now         func() time.Time
	ASRProvider ASRProvider
}

type Service struct {
	writer      *sql.DB
	reader      *sql.DB
	now         func() time.Time
	asrProvider ASRProvider
	mu          sync.Mutex
}

type persistedCandidates struct {
	Peaks      []CandidateDTO `json:"peaks"`
	Troughs    []CandidateDTO `json:"troughs"`
	Highlights []CandidateDTO `json:"highlights"`
}

func NewService(writer, reader *sql.DB) (*Service, error) {
	return NewServiceWithOptions(writer, reader, ServiceOptions{})
}

func NewServiceWithOptions(writer, reader *sql.DB, options ServiceOptions) (*Service, error) {
	if writer == nil || reader == nil {
		return nil, ErrInvalidArgument
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	provider, err := normalizeASRProvider(options.ASRProvider)
	if err != nil {
		return nil, err
	}
	return &Service{writer: writer, reader: reader, now: options.Now, asrProvider: provider}, nil
}

func (s *Service) AnalyzeSession(ctx context.Context, request AnalyzeRequest) (ReportDTO, error) {
	if ctx == nil || !validUUIDv7(request.SessionID) {
		return ReportDTO{}, ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.writer.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return ReportDTO{}, err
	}
	defer tx.Rollback()
	input, err := loadComputationInput(ctx, tx, request.SessionID)
	if err != nil {
		return ReportDTO{}, err
	}
	computed, err := compute(input)
	if err != nil {
		return ReportDTO{}, err
	}
	var existingID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM analysis_reports
		WHERE session_id = ? AND input_fingerprint = ? AND status = 'completed'
		ORDER BY completed_at DESC, id DESC LIMIT 1`, request.SessionID, computed.Fingerprint).Scan(&existingID)
	switch {
	case err == nil:
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			return ReportDTO{}, rollbackErr
		}
		return s.getReportByID(ctx, request.SessionID, existingID)
	case !errors.Is(err, sql.ErrNoRows):
		return ReportDTO{}, err
	}

	for _, bucket := range computed.Buckets {
		if _, err := tx.ExecContext(ctx, `INSERT INTO metric_buckets(
			session_id, analysis_version, bucket_start_ms, bucket_size_ms,
			chat_count, unique_chatters, like_delta, gift_count, gift_value,
			follow_count, enter_count, active_users, message_total, completeness
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			request.SessionID, computed.AnalysisVersion, bucket.BucketStartMS, bucket.BucketSizeMS,
			bucket.ChatCount, bucket.UniqueChatters, bucket.LikeDelta, bucket.GiftCount, bucket.GiftValue,
			bucket.FollowCount, bucket.EnterCount, bucket.ActiveUsers, bucket.MessageTotal, bucket.Completeness,
		); err != nil {
			return ReportDTO{}, err
		}
	}
	summaryJSON, err := json.Marshal(computed.Summary)
	if err != nil {
		return ReportDTO{}, ErrInputCorrupt
	}
	candidatesJSON, err := json.Marshal(persistedCandidates{
		Peaks: computed.Peaks, Troughs: computed.Troughs, Highlights: computed.Highlights,
	})
	if err != nil {
		return ReportDTO{}, ErrInputCorrupt
	}
	reportID, err := newUUIDv7()
	if err != nil {
		return ReportDTO{}, err
	}
	now := s.now().UTC().UnixMilli()
	if now < 0 {
		return ReportDTO{}, ErrInputCorrupt
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO analysis_reports(
		id, session_id, status, analysis_version, input_fingerprint,
		summary_json, highlights_json, topics_json, questions_json,
		started_at, completed_at
	) VALUES (?, ?, 'completed', ?, ?, ?, ?, '[]', '[]', ?, ?)`,
		reportID, request.SessionID, computed.AnalysisVersion, computed.Fingerprint,
		string(summaryJSON), string(candidatesJSON), now, now,
	); err != nil {
		return ReportDTO{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReportDTO{}, err
	}
	return s.getReportByID(ctx, request.SessionID, reportID)
}

func (s *Service) GetAnalysisReport(ctx context.Context, sessionID string) (ReportDTO, error) {
	if ctx == nil || !validUUIDv7(sessionID) {
		return ReportDTO{}, ErrInvalidArgument
	}
	var reportID string
	err := s.reader.QueryRowContext(ctx, `SELECT id FROM analysis_reports
		WHERE session_id = ? AND status = 'completed' AND analysis_version LIKE ?
		ORDER BY completed_at DESC, id DESC LIMIT 1`, sessionID, AlgorithmVersion+"+%").Scan(&reportID)
	if errors.Is(err, sql.ErrNoRows) {
		return ReportDTO{}, ErrReportNotFound
	}
	if err != nil {
		return ReportDTO{}, err
	}
	return s.getReportByID(ctx, sessionID, reportID)
}

func (s *Service) getReportByID(ctx context.Context, sessionID, reportID string) (ReportDTO, error) {
	var (
		status, analysisVersion, summaryText, candidatesText string
		startedAt, completedAt                               sql.NullInt64
	)
	err := s.reader.QueryRowContext(ctx, `SELECT status, analysis_version,
		summary_json, highlights_json, started_at, completed_at
		FROM analysis_reports WHERE id = ? AND session_id = ?`, reportID, sessionID).Scan(
		&status, &analysisVersion, &summaryText, &candidatesText, &startedAt, &completedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ReportDTO{}, ErrReportNotFound
	}
	if err != nil {
		return ReportDTO{}, err
	}
	if status != "completed" || !startedAt.Valid || !completedAt.Valid ||
		startedAt.Int64 < 0 || completedAt.Int64 < startedAt.Int64 ||
		!strings.HasPrefix(analysisVersion, AlgorithmVersion+"+") {
		return ReportDTO{}, ErrInputCorrupt
	}
	var summary SummaryDTO
	if err := strictJSON(summaryText, &summary); err != nil {
		return ReportDTO{}, err
	}
	var candidates persistedCandidates
	if err := strictJSON(candidatesText, &candidates); err != nil {
		return ReportDTO{}, err
	}
	buckets, err := loadMetricBuckets(ctx, s.reader, sessionID, analysisVersion)
	if err != nil {
		return ReportDTO{}, err
	}
	if len(buckets) != summary.BucketCount || summary.BucketSizeMS != BucketSizeMS || summary.DurationMS <= 0 {
		return ReportDTO{}, ErrInputCorrupt
	}
	return ReportDTO{
		Version: ContractVersion, ID: reportID, SessionID: sessionID, Status: status,
		AnalysisVersion: analysisVersion, AlgorithmVersion: AlgorithmVersion,
		StartedAt: startedAt.Int64, CompletedAt: completedAt.Int64,
		Summary: summary, Buckets: buckets, Peaks: candidates.Peaks,
		Troughs: candidates.Troughs, Highlights: candidates.Highlights,
	}, nil
}

func loadComputationInput(ctx context.Context, tx *sql.Tx, sessionID string) (computationInput, error) {
	input := computationInput{}
	input.Session.ID = sessionID
	var endedAt sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT status, started_at, ended_at
		FROM live_sessions WHERE id = ?`, sessionID).Scan(
		&input.Session.Status, &input.Session.StartedAt, &endedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return computationInput{}, ErrSessionNotFound
	}
	if err != nil {
		return computationInput{}, err
	}
	if !endedAt.Valid || (input.Session.Status != "completed" && input.Session.Status != "interrupted" && input.Session.Status != "failed") {
		return computationInput{}, ErrAnalysisNotReady
	}
	input.Session.EndedAt = endedAt.Int64
	events, err := tx.QueryContext(ctx, `SELECT id, ingest_sequence, event_role, kind,
		session_offset_ms, user_hash, numeric_value, normalized_json, dedupe_key,
		parse_status, normalizer_version
		FROM live_events WHERE session_id = ?
		ORDER BY session_offset_ms ASC, id ASC LIMIT ?`, sessionID, maxAnalysisEvents+1)
	if err != nil {
		return computationInput{}, err
	}
	defer events.Close()
	for events.Next() {
		if len(input.Events) == maxAnalysisEvents {
			return computationInput{}, ErrInputTooLarge
		}
		var value eventInput
		var userHash sql.NullString
		var numeric sql.NullFloat64
		if err := events.Scan(
			&value.ID, &value.IngestSequence, &value.Role, &value.Kind,
			&value.OffsetMS, &userHash, &numeric, &value.NormalizedJSON,
			&value.DedupeKey, &value.ParseStatus, &value.NormalizerVersion,
		); err != nil {
			return computationInput{}, err
		}
		value.UserHash = userHash.String
		if numeric.Valid {
			copyValue := numeric.Float64
			value.NumericValue = &copyValue
		}
		input.Events = append(input.Events, value)
	}
	if err := events.Err(); err != nil {
		return computationInput{}, err
	}
	gaps, err := tx.QueryContext(ctx, `SELECT id, kind, start_offset_ms,
		end_offset_ms, recovered, reason_code FROM capture_gaps
		WHERE session_id = ? ORDER BY start_offset_ms ASC, id ASC LIMIT ?`, sessionID, maxAnalysisGaps+1)
	if err != nil {
		return computationInput{}, err
	}
	defer gaps.Close()
	for gaps.Next() {
		if len(input.Gaps) == maxAnalysisGaps {
			return computationInput{}, ErrInputTooLarge
		}
		var value gapInput
		var end sql.NullInt64
		var recovered int
		if err := gaps.Scan(&value.ID, &value.Kind, &value.StartMS, &end, &recovered, &value.ReasonCode); err != nil {
			return computationInput{}, err
		}
		if recovered != 0 && recovered != 1 {
			return computationInput{}, ErrInputCorrupt
		}
		value.Recovered = recovered == 1
		if end.Valid {
			copyValue := end.Int64
			value.EndMS = &copyValue
		}
		input.Gaps = append(input.Gaps, value)
	}
	if err := gaps.Err(); err != nil {
		return computationInput{}, err
	}
	return input, nil
}

func loadMetricBuckets(ctx context.Context, reader *sql.DB, sessionID, analysisVersion string) ([]MetricBucketDTO, error) {
	rows, err := reader.QueryContext(ctx, `SELECT bucket_start_ms, bucket_size_ms,
		chat_count, unique_chatters, like_delta, gift_count, gift_value,
		follow_count, enter_count, active_users, message_total, completeness
		FROM metric_buckets WHERE session_id = ? AND analysis_version = ?
		ORDER BY bucket_start_ms ASC LIMIT ?`, sessionID, analysisVersion, maxBuckets+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]MetricBucketDTO, 0)
	for rows.Next() {
		if len(result) == maxBuckets {
			return nil, ErrInputTooLarge
		}
		var value MetricBucketDTO
		var giftValue sql.NullFloat64
		if err := rows.Scan(
			&value.BucketStartMS, &value.BucketSizeMS, &value.ChatCount,
			&value.UniqueChatters, &value.LikeDelta, &value.GiftCount, &giftValue,
			&value.FollowCount, &value.EnterCount, &value.ActiveUsers,
			&value.MessageTotal, &value.Completeness,
		); err != nil {
			return nil, err
		}
		if giftValue.Valid {
			copyValue := giftValue.Float64
			value.GiftValue = &copyValue
		}
		if value.BucketStartMS != int64(len(result))*BucketSizeMS || value.BucketSizeMS != BucketSizeMS ||
			value.Completeness < 0 || value.Completeness > 1 {
			return nil, ErrInputCorrupt
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func strictJSON(text string, target any) error {
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return ErrInputCorrupt
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ErrInputCorrupt
	}
	return nil
}

func validUUIDv7(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.Version() == 7
}

func newUUIDv7() (string, error) {
	value, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	return value.String(), nil
}
