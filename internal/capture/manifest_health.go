package capture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/google/uuid"
)

func (r *SQLiteRepository) lockManifestSession(sessionID string) func() {
	r.sessionLockMu.Lock()
	lock := r.sessionLocks[sessionID]
	if lock == nil {
		lock = &manifestSessionLock{}
		r.sessionLocks[sessionID] = lock
	}
	lock.refs++
	r.sessionLockMu.Unlock()
	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		r.sessionLockMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(r.sessionLocks, sessionID)
		}
		r.sessionLockMu.Unlock()
	}
}

func (r *SQLiteRepository) materialize(ctx context.Context, session LiveSession) (LiveSession, error) {
	unlock := r.lockManifestSession(session.ID)
	defer unlock()
	return r.materializeLocked(ctx, session.ID)
}

func (r *SQLiteRepository) materializeCommitted(ctx context.Context, session LiveSession) LiveSession {
	unlock := r.lockManifestSession(session.ID)
	defer unlock()
	return r.materializeCommittedLocked(ctx, session)
}

func (r *SQLiteRepository) materializeCommittedLocked(ctx context.Context, session LiveSession) LiveSession {
	materialized, _ := r.materializeLocked(ctx, session.ID)
	if materialized.ID != "" {
		return materialized
	}
	return session
}

func (r *SQLiteRepository) materializeLocked(ctx context.Context, sessionID string) (LiveSession, error) {
	session, err := r.loadSession(ctx, sessionID)
	if err != nil {
		return LiveSession{}, err
	}
	matches := r.manifestMatches(session)
	if !session.ManifestDirty && matches {
		r.forgetManifestIssue(session.ID)
		return session, nil
	}
	if !session.ManifestDirty {
		if err := r.updateManifestDirty(ctx, session, true); err != nil {
			_, _ = r.raiseManifestIssue(session.ID, false)
			return session, fmt.Errorf("persist dirty manifest marker: %w", err)
		}
		session.ManifestDirty = true
	}
	if _, err := r.raiseManifestIssue(session.ID, false); err != nil {
		return session, err
	}
	if !matches {
		if _, err := r.writeManifest(session); err != nil {
			return session, fmt.Errorf("materialize committed session manifest: %w", err)
		}
	}
	return r.completeManifestRepairLocked(ctx, session)
}

func (r *SQLiteRepository) promoteCommitted(ctx context.Context, staged *stagedManifest, session LiveSession) LiveSession {
	unlock := r.lockManifestSession(session.ID)
	defer unlock()
	return r.promoteCommittedLocked(ctx, staged, session)
}

func (r *SQLiteRepository) promoteCommittedLocked(ctx context.Context, staged *stagedManifest, session LiveSession) LiveSession {
	current, err := r.loadSession(ctx, session.ID)
	if err != nil {
		_, _ = r.raiseManifestIssue(session.ID, false)
		return session
	}
	if !sameManifestVersion(current, session) {
		return current
	}
	current.ManifestDirty = true
	if err := r.promoteManifest(staged); err != nil {
		_, _ = r.raiseManifestIssue(current.ID, false)
		return current
	}
	completed, err := r.completeManifestRepairLocked(ctx, current)
	if err != nil {
		_, _ = r.raiseManifestIssue(current.ID, false)
		return current
	}
	return completed
}

func (r *SQLiteRepository) completeManifestRepairLocked(ctx context.Context, session LiveSession) (LiveSession, error) {
	if r.hasManifestIssue(session.ID) {
		if _, err := r.raiseManifestIssue(session.ID, false); err != nil {
			return session, err
		}
	}
	cleared, err := r.clearManifestDirty(ctx, session, func() error {
		return r.reportManifestCleared(session.ID)
	})
	if err != nil {
		return session, err
	}
	if !cleared {
		current, loadErr := r.loadSession(ctx, session.ID)
		if loadErr == nil {
			return current, nil
		}
		return session, nil
	}
	session.ManifestDirty = false
	r.forgetManifestIssue(session.ID)
	return session, nil
}

// RepairManifests scans only dirty or active sessions in bounded keyset pages.
// Each page closes its rows before filesystem or marker writes begin.
func (r *SQLiteRepository) RepairManifests(ctx context.Context) (ManifestRepairReport, error) {
	var report ManifestRepairReport
	if err := requireContext(ctx); err != nil {
		return report, err
	}
	var cursor string
	hasCursor := false
	var repairErrors []error
	for {
		page, err := r.queryManifestRepairPage(ctx, cursor, hasCursor)
		if err != nil {
			return report, errors.Join(append(repairErrors, err)...)
		}
		if len(page) == 0 {
			break
		}
		report.Scanned += len(page)
		if batchReporter, ok := r.manifestReporter.(ManifestHealthBatchReporter); ok {
			pageReport, pageErrors := r.repairManifestPageBatch(ctx, page, batchReporter)
			report.Repaired += pageReport.Repaired
			report.Failed += pageReport.Failed
			repairErrors = append(repairErrors, pageErrors...)
		} else {
			for _, snapshot := range page {
				unlock := r.lockManifestSession(snapshot.ID)
				current, loadErr := r.loadSession(ctx, snapshot.ID)
				if loadErr != nil {
					unlock()
					report.Failed++
					repairErrors = append(repairErrors, manifestRepairFailure(snapshot.ID))
					continue
				}
				needsRepair := current.ManifestDirty || !r.manifestMatches(current)
				if !needsRepair || !manifestRepairCandidate(current) {
					unlock()
					continue
				}
				_, repairErr := r.materializeLocked(ctx, current.ID)
				unlock()
				if repairErr != nil {
					report.Failed++
					repairErrors = append(repairErrors, manifestRepairFailure(current.ID))
					continue
				}
				report.Repaired++
			}
		}
		cursor = page[len(page)-1].ID
		hasCursor = true
		if len(page) < manifestRepairPageSize {
			break
		}
	}
	return report, errors.Join(repairErrors...)
}

type lockedManifestRepair struct {
	session  LiveSession
	unlock   func()
	reported bool
}

func (r *SQLiteRepository) repairManifestPageBatch(ctx context.Context, page []LiveSession, reporter ManifestHealthBatchReporter) (ManifestRepairReport, []error) {
	var report ManifestRepairReport
	if err := beginManifestHealthBatch(reporter); err != nil {
		return report, []error{err}
	}
	locked := make([]lockedManifestRepair, 0, len(page))
	reportedRequired := make([]string, 0, len(page))
	var repairErrors []error
	for _, snapshot := range page {
		unlock := r.lockManifestSession(snapshot.ID)
		current, err := r.loadSession(ctx, snapshot.ID)
		if err != nil {
			unlock()
			report.Failed++
			repairErrors = append(repairErrors, manifestRepairFailure(snapshot.ID))
			continue
		}
		if !manifestRepairCandidate(current) {
			unlock()
			continue
		}
		matches := r.manifestMatches(current)
		if !current.ManifestDirty && matches {
			unlock()
			continue
		}
		if !current.ManifestDirty {
			if err := r.updateManifestDirty(ctx, current, true); err != nil {
				_, _ = r.raiseManifestIssue(current.ID, true)
				unlock()
				report.Failed++
				repairErrors = append(repairErrors, manifestRepairFailure(current.ID))
				continue
			}
			current.ManifestDirty = true
		}
		reported, err := r.raiseManifestIssue(current.ID, true)
		if err != nil {
			unlock()
			report.Failed++
			repairErrors = append(repairErrors, manifestRepairFailure(current.ID))
			continue
		}
		if reported {
			reportedRequired = append(reportedRequired, current.ID)
		}
		if !matches {
			if _, err := r.writeManifest(current); err != nil {
				locked = append(locked, lockedManifestRepair{session: current, unlock: unlock, reported: reported})
				report.Failed++
				repairErrors = append(repairErrors, manifestRepairFailure(current.ID))
				continue
			}
		}
		locked = append(locked, lockedManifestRepair{session: current, unlock: unlock, reported: reported})
	}

	ready := make([]lockedManifestRepair, 0, len(locked))
	shadowOutstanding := r.manifestIssueCount()
	tx, txErr := r.writer.BeginTx(ctx, nil)
	if txErr == nil {
		for _, item := range locked {
			if !r.manifestMatches(item.session) {
				continue
			}
			reserved, err := reserveManifestVersion(ctx, tx, item.session)
			if err != nil {
				txErr = err
				break
			}
			if !reserved {
				continue
			}
			if r.hasManifestIssue(item.session.ID) && shadowOutstanding > 0 {
				shadowOutstanding--
			}
			if err := r.reportManifestClearedWithOutstanding(item.session.ID, shadowOutstanding); err != nil {
				txErr = err
				break
			}
			ready = append(ready, item)
		}
	}
	endErr := endManifestHealthBatch(reporter)
	if endErr == nil {
		r.acknowledgeManifestRequired(reportedRequired)
	}
	if txErr != nil || endErr != nil {
		if tx != nil {
			_ = tx.Rollback()
		}
		for _, item := range locked {
			if !containsLockedRepair(ready, item.session.ID) && !r.manifestMatches(item.session) {
				continue
			}
			report.Failed++
			repairErrors = append(repairErrors, manifestRepairFailure(item.session.ID))
		}
		if endErr != nil {
			repairErrors = append(repairErrors, endErr)
		}
		for _, item := range locked {
			item.unlock()
		}
		return report, repairErrors
	}

	if len(ready) > 0 && r.beforeManifestMarkerClear != nil {
		if err := r.beforeManifestMarkerClear(); err != nil {
			_ = tx.Rollback()
			for _, item := range ready {
				report.Failed++
				repairErrors = append(repairErrors, manifestRepairFailure(item.session.ID))
			}
			for _, item := range locked {
				item.unlock()
			}
			return report, repairErrors
		}
	}
	for _, item := range ready {
		if err := clearReservedManifestMarker(ctx, tx, item.session); err != nil {
			_ = tx.Rollback()
			for _, candidate := range ready {
				report.Failed++
				repairErrors = append(repairErrors, manifestRepairFailure(candidate.session.ID))
			}
			for _, candidate := range locked {
				candidate.unlock()
			}
			return report, repairErrors
		}
	}
	if err := tx.Commit(); err != nil {
		for _, item := range ready {
			report.Failed++
			repairErrors = append(repairErrors, manifestRepairFailure(item.session.ID))
		}
		for _, item := range locked {
			item.unlock()
		}
		return report, repairErrors
	}
	for _, item := range ready {
		report.Repaired++
		r.forgetManifestIssue(item.session.ID)
	}
	for _, item := range locked {
		item.unlock()
	}
	return report, repairErrors
}

func containsLockedRepair(items []lockedManifestRepair, sessionID string) bool {
	for _, item := range items {
		if item.session.ID == sessionID {
			return true
		}
	}
	return false
}

func (r *SQLiteRepository) queryManifestRepairPage(ctx context.Context, cursor string, hasCursor bool) ([]LiveSession, error) {
	query := sessionSelectSQL + ` WHERE (manifest_dirty = 1 OR status IN ('starting', 'recording', 'finalizing'))`
	args := make([]any, 0, 2)
	if hasCursor {
		query += ` AND id > ?`
		args = append(args, cursor)
	}
	query += ` ORDER BY id ASC LIMIT ?`
	args = append(args, manifestRepairPageSize)
	rows, err := r.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query sessions for manifest repair: %w", err)
	}
	page := make([]LiveSession, 0, manifestRepairPageSize)
	for rows.Next() {
		session, scanErr := scanSession(rows)
		if scanErr != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan session for manifest repair: %w", scanErr)
		}
		page = append(page, session)
	}
	rowsErr := rows.Err()
	closeErr := rows.Close()
	if rowsErr != nil || closeErr != nil {
		return nil, errors.Join(rowsErr, closeErr)
	}
	return page, nil
}

func manifestRepairCandidate(session LiveSession) bool {
	return session.ManifestDirty || activeSessionStatus(session.Status)
}

func (r *SQLiteRepository) loadSession(ctx context.Context, sessionID string) (LiveSession, error) {
	session, err := querySession(ctx, r.reader, sessionSelectSQL+` WHERE id = ?`, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return LiveSession{}, ErrSessionNotFound
	}
	if err != nil {
		return LiveSession{}, fmt.Errorf("load session manifest state: %w", err)
	}
	return session, nil
}

func (r *SQLiteRepository) persistManifestDirty(ctx context.Context, session LiveSession, dirty bool) error {
	want := 0
	if dirty {
		want = 1
	}
	result, err := r.writer.ExecContext(ctx, `UPDATE live_sessions SET manifest_dirty = ?
		WHERE id = ? AND updated_at = ? AND operation_id = ? AND status = ?
		AND recording_status = ? AND manifest_dirty <> ?`,
		want, session.ID, session.UpdatedAt, session.OperationID, session.Status, session.RecordingStatus, want)
	if err != nil {
		return fmt.Errorf("update manifest marker: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect manifest marker update: %w", err)
	}
	if affected == 1 {
		return nil
	}
	return errors.New("live session changed while updating manifest marker")
}

func (r *SQLiteRepository) clearManifestDirty(ctx context.Context, session LiveSession, acknowledge func() error) (bool, error) {
	tx, err := r.writer.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin manifest marker clear: %w", err)
	}
	defer tx.Rollback()
	reserved, err := reserveManifestVersion(ctx, tx, session)
	if err != nil || !reserved {
		return false, err
	}
	if acknowledge != nil {
		if err := acknowledge(); err != nil {
			return false, err
		}
	}
	if r.beforeManifestMarkerClear != nil {
		if err := r.beforeManifestMarkerClear(); err != nil {
			return false, errors.New("manifest marker clear was interrupted")
		}
	}
	if err := clearReservedManifestMarker(ctx, tx, session); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit manifest marker clear: %w", err)
	}
	return true, nil
}

func reserveManifestVersion(ctx context.Context, tx *sql.Tx, session LiveSession) (bool, error) {
	result, err := tx.ExecContext(ctx, `UPDATE live_sessions SET manifest_dirty = manifest_dirty
		WHERE id = ? AND updated_at = ? AND operation_id = ? AND status = ?
		AND recording_status = ? AND manifest_dirty = 1`,
		session.ID, session.UpdatedAt, session.OperationID, session.Status, session.RecordingStatus)
	if err != nil {
		return false, fmt.Errorf("reserve manifest marker version: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect reserved manifest version: %w", err)
	}
	return affected == 1, nil
}

func clearReservedManifestMarker(ctx context.Context, tx *sql.Tx, session LiveSession) error {
	result, err := tx.ExecContext(ctx, `UPDATE live_sessions SET manifest_dirty = 0
		WHERE id = ? AND updated_at = ? AND operation_id = ? AND status = ?
		AND recording_status = ? AND manifest_dirty = 1`,
		session.ID, session.UpdatedAt, session.OperationID, session.Status, session.RecordingStatus)
	if err != nil {
		return fmt.Errorf("clear reserved manifest marker: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect cleared manifest marker: %w", err)
	}
	if affected != 1 {
		return errors.New("reserved manifest version changed before clear")
	}
	return nil
}

func sameManifestVersion(left, right LiveSession) bool {
	leftPayload, leftErr := encodeManifest(left)
	rightPayload, rightErr := encodeManifest(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftPayload, rightPayload)
}

// LastManifestError exposes sanitized outstanding derivative-manifest issues.
func (r *SQLiteRepository) LastManifestError() error {
	r.manifestIssueMu.RLock()
	defer r.manifestIssueMu.RUnlock()
	if len(r.manifestIssues) == 0 {
		return nil
	}
	ids := make([]string, 0, len(r.manifestIssues))
	for sessionID := range r.manifestIssues {
		ids = append(ids, sessionID)
	}
	sort.Strings(ids)
	issues := make([]error, 0, len(ids))
	for _, sessionID := range ids {
		issues = append(issues, manifestRepairFailure(sessionID))
	}
	return errors.Join(issues...)
}

func (r *SQLiteRepository) raiseManifestIssue(sessionID string, deferAcknowledgement bool) (bool, error) {
	r.manifestIssueMu.Lock()
	state, exists := r.manifestIssues[sessionID]
	if !exists {
		r.manifestIssues[sessionID] = state
	}
	if state.requiredAcknowledged {
		r.manifestIssueMu.Unlock()
		return false, nil
	}
	event := ManifestHealthEvent{
		SessionID: manifestSessionCorrelation(sessionID), State: ManifestHealthRepairRequired,
		ErrorCode: ManifestRepairRequiredErrorCode, Outstanding: len(r.manifestIssues),
	}
	reporter := r.manifestReporter
	r.manifestIssueMu.Unlock()
	if err := reportManifestHealth(reporter, event); err != nil {
		return false, err
	}
	if !deferAcknowledgement {
		r.acknowledgeManifestRequired([]string{sessionID})
	}
	return true, nil
}

func (r *SQLiteRepository) acknowledgeManifestRequired(sessionIDs []string) {
	r.manifestIssueMu.Lock()
	defer r.manifestIssueMu.Unlock()
	for _, sessionID := range sessionIDs {
		state, exists := r.manifestIssues[sessionID]
		if !exists {
			continue
		}
		state.requiredAcknowledged = true
		r.manifestIssues[sessionID] = state
	}
}

func (r *SQLiteRepository) reportManifestCleared(sessionID string) error {
	r.manifestIssueMu.RLock()
	_, exists := r.manifestIssues[sessionID]
	outstanding := len(r.manifestIssues)
	r.manifestIssueMu.RUnlock()
	if exists && outstanding > 0 {
		outstanding--
	}
	return r.reportManifestClearedWithOutstanding(sessionID, outstanding)
}

func (r *SQLiteRepository) reportManifestClearedWithOutstanding(sessionID string, outstanding int) error {
	r.manifestIssueMu.RLock()
	reporter := r.manifestReporter
	r.manifestIssueMu.RUnlock()
	return reportManifestHealth(reporter, ManifestHealthEvent{
		SessionID: manifestSessionCorrelation(sessionID), State: ManifestHealthRepairCleared,
		ErrorCode: ManifestRepairClearedErrorCode, Outstanding: outstanding,
	})
}

func (r *SQLiteRepository) manifestIssueCount() int {
	r.manifestIssueMu.RLock()
	defer r.manifestIssueMu.RUnlock()
	return len(r.manifestIssues)
}

func (r *SQLiteRepository) hasManifestIssue(sessionID string) bool {
	r.manifestIssueMu.RLock()
	defer r.manifestIssueMu.RUnlock()
	_, exists := r.manifestIssues[sessionID]
	return exists
}

func (r *SQLiteRepository) forgetManifestIssue(sessionID string) {
	r.manifestIssueMu.Lock()
	delete(r.manifestIssues, sessionID)
	r.manifestIssueMu.Unlock()
}

func reportManifestHealth(reporter ManifestHealthReporter, event ManifestHealthEvent) (err error) {
	if reporter == nil {
		return nil
	}
	defer func() {
		if recover() != nil {
			err = ErrManifestHealthReportFailed
		}
	}()
	if err := reporter.ReportManifestHealth(event); err != nil {
		return ErrManifestHealthReportFailed
	}
	return nil
}

func beginManifestHealthBatch(reporter ManifestHealthBatchReporter) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrManifestHealthReportFailed
		}
	}()
	if err := reporter.BeginManifestHealthBatch(); err != nil {
		return ErrManifestHealthReportFailed
	}
	return nil
}

func endManifestHealthBatch(reporter ManifestHealthBatchReporter) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrManifestHealthReportFailed
		}
	}()
	if err := reporter.EndManifestHealthBatch(); err != nil {
		return ErrManifestHealthReportFailed
	}
	return nil
}

func manifestRepairFailure(sessionID string) error {
	return fmt.Errorf("%w: session %s", ErrManifestRepairRequired, manifestSessionCorrelation(sessionID))
}

func (r *SQLiteRepository) manifestMatches(session LiveSession) bool {
	manifestPath, err := secureManifestPath(r.dataRoot, session.DataPath)
	if err != nil {
		return false
	}
	actual, err := os.ReadFile(manifestPath)
	if err != nil {
		return false
	}
	expected, err := encodeManifest(session)
	return err == nil && bytes.Equal(actual, expected)
}

func manifestSessionCorrelation(sessionID string) string {
	parsed, err := uuid.Parse(sessionID)
	if err == nil {
		return parsed.String()
	}
	digest := sha256.Sum256([]byte(sessionID))
	return "invalid-" + hex.EncodeToString(digest[:8])
}
