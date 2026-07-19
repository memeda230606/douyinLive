package eventstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

func (m *Manager) persistWithRetry(ctx context.Context, batch Batch) error {
	if ctx == nil {
		return ErrInvalidBatch
	}
	started := time.Now()
	delay := m.options.BusyRetryInitial
	for {
		err := m.options.Writer.PersistBatch(ctx, batch)
		if !errors.Is(err, ErrPersistenceBusy) {
			return err
		}
		remaining := m.options.BusyRetryWindow - time.Since(started)
		if remaining <= 0 {
			return err
		}
		wait := delay
		if wait > remaining {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
		if delay < m.options.BusyRetryWindow/2 {
			delay *= 2
		} else {
			delay = m.options.BusyRetryWindow
		}
	}
}

// classifyRecoveryFinalizationError is the public recovery boundary. It keeps
// retryable or state-ambiguous failures distinguishable from errors that
// describe explicit, auditable data damage. Returned values contain stable
// codes only; implementation and native database details never cross it.
func classifyRecoveryFinalizationError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return errors.Join(ErrRecoveryDeferred, ErrPersistenceDegraded)

	case errors.Is(err, ErrManagerOptions):
		return ErrManagerOptions
	case errors.Is(err, ErrRecoveryCutoff):
		return ErrRecoveryCutoff
	case errors.Is(err, ErrSessionPathInvalid):
		return ErrSessionPathInvalid
	case errors.Is(err, ErrPrivacyKeyInvalid):
		return ErrPrivacyKeyInvalid
	case errors.Is(err, ErrPrivacyKeyMissing):
		return ErrPrivacyKeyMissing
	case errors.Is(err, ErrPrivacyKeyMismatch):
		return ErrPrivacyKeyMismatch
	case errors.Is(err, ErrDropLedgerCorrupt):
		return ErrDropLedgerCorrupt
	case errors.Is(err, ErrDropLedgerInvalid):
		return ErrDropLedgerInvalid
	case errors.Is(err, ErrGiftComboCapacity):
		return ErrGiftComboCapacity
	case errors.Is(err, ErrEventSpoolFatal), errors.Is(err, ErrSpoolFailed),
		errors.Is(err, ErrFrameTruncated), errors.Is(err, ErrFrameCorrupt),
		errors.Is(err, ErrFrameTooLarge):
		return ErrEventSpoolFatal

	case errors.Is(err, ErrManagerClosed):
		return errors.Join(ErrRecoveryDeferred, ErrManagerClosed)
	case errors.Is(err, ErrSessionAlreadyOpen):
		return errors.Join(ErrRecoveryDeferred, ErrSessionAlreadyOpen)
	case errors.Is(err, ErrSessionClosed):
		return errors.Join(ErrRecoveryDeferred, ErrSessionClosed)
	case errors.Is(err, ErrEventManagerNotReady):
		return errors.Join(ErrRecoveryDeferred, ErrEventManagerNotReady)
	case errors.Is(err, ErrDropLedgerIO):
		return errors.Join(ErrRecoveryDeferred, ErrDropLedgerIO)
	case errors.Is(err, ErrPersistenceDegraded), isDatabasePersistenceError(err),
		errors.Is(err, ErrWriterUnavailable):
		return errors.Join(ErrRecoveryDeferred, ErrPersistenceDegraded)
	case errors.Is(err, ErrRecoveryDeferred):
		return errors.Join(ErrRecoveryDeferred, ErrPersistenceDegraded)
	default:
		return errors.Join(ErrRecoveryDeferred, ErrPersistenceDegraded)
	}
}

// recoveryFailureWithAuthoritativeCutoff refreshes the cutoff after a recovery
// stage that may already have committed a durable prefix. If that authoritative
// read fails, the permanent disposition is no longer safe: return only the
// stable persistence failure so the public boundary defers terminalization.
func (m *Manager) recoveryFailureWithAuthoritativeCutoff(
	ctx context.Context,
	sessionID string,
	currentCutoff time.Time,
	recoveryErr error,
) (time.Time, error) {
	cutoff, err := m.latestRecoveryCutoff(ctx, sessionID, currentCutoff)
	if err != nil {
		return currentCutoff, ErrPersistenceDegraded
	}
	return cutoff, recoveryErr
}

func (m *Manager) recoverSession(ctx context.Context, descriptor SessionDescriptor) error {
	if m == nil || ctx == nil {
		return ErrManagerOptions
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	eventsRoot, err := resolveSessionEventsRoot(m.options.DataRoot, descriptor)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.accepting {
		return ErrManagerClosed
	}
	if _, exists := m.sessions[descriptor.SessionID]; exists {
		return ErrSessionAlreadyOpen
	}
	checkpoint, found, err := m.options.Writer.Checkpoint(ctx, descriptor.SessionID)
	if err != nil {
		return stablePersistenceError(err)
	}
	if !found {
		return nil
	}
	if checkpoint.PrivacyKeyID != m.privacy.KeyID() {
		return ErrPrivacyKeyMismatch
	}
	dropLedger, err := OpenDropLedger(eventsRoot, descriptor.SessionID)
	if err != nil {
		return err
	}
	dedupe := NewDeduplicator(DefaultDedupeTTL, DefaultDedupeCapacity)
	_, err = m.recoverAtRoot(
		ctx, descriptor, eventsRoot, checkpoint, nil, checkpoint.State, dropLedger, dedupe,
	)
	if isDatabasePersistenceError(err) {
		return stablePersistenceError(err)
	}
	return err
}

// recoverAndCloseSession replays the durable crash tail, closes every open
// gift fold at the greater of the caller-provided media cutoff and the latest
// recovered event/fold time, and advances the checkpoint through closing to
// closed. It is intentionally unavailable for a live SessionSink owner.
func (m *Manager) recoverAndCloseSession(
	ctx context.Context,
	descriptor SessionDescriptor,
	minimumCutoff time.Time,
) (time.Time, error) {
	if m == nil || ctx == nil || minimumCutoff.IsZero() || descriptor.StartedAt.IsZero() {
		return time.Time{}, ErrManagerOptions
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	cutoff := minimumCutoff.UTC()
	if cutoff.Before(descriptor.StartedAt.UTC()) {
		return cutoff, ErrRecoveryCutoff
	}
	eventsRoot, err := resolveSessionEventsRoot(m.options.DataRoot, descriptor)
	if err != nil {
		return m.recoveryFailureWithAuthoritativeCutoff(
			ctx, descriptor.SessionID, cutoff, err,
		)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.accepting {
		return cutoff, ErrManagerClosed
	}
	if _, exists := m.sessions[descriptor.SessionID]; exists {
		return cutoff, ErrSessionAlreadyOpen
	}
	checkpoint, found, err := m.options.Writer.Checkpoint(ctx, descriptor.SessionID)
	if err != nil {
		return cutoff, stablePersistenceError(err)
	}
	authoritativeCutoff, durableEvidence, err := m.latestRecoveryCutoffAndEvidence(
		ctx, descriptor.SessionID, cutoff,
	)
	if err != nil {
		return cutoff, err
	}
	cutoff = authoritativeCutoff
	if !found {
		if durableEvidence {
			return cutoff, ErrRecoveryCutoff
		}
		return cutoff, nil
	}
	if checkpoint.State == CheckpointClosed {
		latestSource, sourceFound, sourceErr := m.options.Writer.latestRecoverySourceSequence(
			ctx, descriptor.SessionID,
		)
		if sourceErr != nil {
			return cutoff, stablePersistenceError(sourceErr)
		}
		if sourceFound && latestSource > checkpoint.CommittedSequence {
			return cutoff, ErrRecoveryCutoff
		}
		// A closed checkpoint is immutable, but corrupted or externally edited
		// databases can still contain an open fold. Never report that state as a
		// clean idempotent recovery.
		remaining, foldErr := m.options.Writer.OpenGiftFolds(
			ctx, descriptor.SessionID, nil, 1,
		)
		if foldErr != nil {
			return cutoff, stablePersistenceError(foldErr)
		}
		if len(remaining) != 0 {
			return cutoff, ErrRecoveryCutoff
		}
		return cutoff, nil
	}
	if checkpoint.PrivacyKeyID != m.privacy.KeyID() {
		return cutoff, ErrPrivacyKeyMismatch
	}

	dropLedger, err := OpenDropLedger(eventsRoot, descriptor.SessionID)
	if err != nil {
		return cutoff, err
	}
	replayState := checkpoint.State
	if replayState == CheckpointDegraded {
		replayState = CheckpointOpen
	}
	current, err := m.recoverAtRoot(
		ctx, descriptor, eventsRoot, checkpoint, nil, replayState, dropLedger,
		NewDeduplicator(DefaultDedupeTTL, DefaultDedupeCapacity),
	)
	if err != nil {
		recoveryErr := err
		if isDatabasePersistenceError(err) {
			recoveryErr = stablePersistenceError(err)
		}
		return m.recoveryFailureWithAuthoritativeCutoff(
			ctx, descriptor.SessionID, cutoff, recoveryErr,
		)
	}
	authoritativeCutoff, err = m.latestRecoveryCutoff(ctx, descriptor.SessionID, cutoff)
	if err != nil {
		return cutoff, err
	}
	cutoff = authoritativeCutoff

	cutoffMillis := cutoff.UnixMilli()
	for {
		folds, foldErr := m.options.Writer.OpenGiftFolds(
			ctx, descriptor.SessionID, &cutoffMillis, m.options.BatchSize,
		)
		if foldErr != nil {
			return cutoff, stablePersistenceError(foldErr)
		}
		if len(folds) == 0 {
			break
		}
		events, combos, foldErr := foldGiftSnapshots(folds, cutoff, false)
		if foldErr != nil {
			return cutoff, ErrPersistenceDegraded
		}
		next := current
		next.UpdatedAt = recoveryCheckpointTime(m.options.Now(), current.UpdatedAt)
		if err := m.persistWithRetry(ctx, Batch{
			SessionID: descriptor.SessionID, PreviousSequence: current.CommittedSequence,
			Events: events, GiftCombos: combos, Checkpoint: next,
		}); err != nil {
			return cutoff, stablePersistenceError(err)
		}
		current = next
	}
	remaining, err := m.options.Writer.OpenGiftFolds(ctx, descriptor.SessionID, nil, 1)
	if err != nil {
		return cutoff, stablePersistenceError(err)
	}
	if len(remaining) != 0 {
		return cutoff, ErrRecoveryCutoff
	}

	if current.State != CheckpointClosing {
		current, err = m.persistRecoveryCheckpointState(ctx, current, CheckpointClosing)
		if err != nil {
			return cutoff, err
		}
	}
	if current.State != CheckpointClosed {
		_, err = m.persistRecoveryCheckpointState(ctx, current, CheckpointClosed)
	}
	return cutoff, err
}

func (m *Manager) latestRecoveryCutoff(
	ctx context.Context,
	sessionID string,
	minimum time.Time,
) (time.Time, error) {
	cutoff, _, err := m.latestRecoveryCutoffAndEvidence(ctx, sessionID, minimum)
	return cutoff, err
}

func (m *Manager) latestRecoveryCutoffAndEvidence(
	ctx context.Context,
	sessionID string,
	minimum time.Time,
) (time.Time, bool, error) {
	latest, found, err := m.options.Writer.latestRecoveryReceivedAt(ctx, sessionID)
	if err != nil {
		return time.Time{}, false, stablePersistenceError(err)
	}
	minimum = minimum.UTC()
	if found && latest.After(minimum) {
		return latest.UTC(), true, nil
	}
	return minimum, found, nil
}

func (w *Writer) latestRecoveryReceivedAt(
	ctx context.Context,
	sessionID string,
) (time.Time, bool, error) {
	if w == nil || w.db == nil || ctx == nil || !validIdentifier(sessionID) {
		return time.Time{}, false, ErrWriterUnavailable
	}
	var milliseconds sql.NullInt64
	err := w.db.QueryRowContext(ctx, `SELECT MAX(recovery_time) FROM (
		SELECT MAX(received_at) AS recovery_time FROM live_events WHERE session_id = ?
		UNION ALL
		SELECT MAX(updated_at) AS recovery_time FROM gift_combo_states WHERE session_id = ?
		UNION ALL
		SELECT MAX(COALESCE(ended_at, started_at)) AS recovery_time FROM capture_gaps
		WHERE session_id = ? AND kind = ? AND reason_code = ?
	)`, sessionID, sessionID, sessionID,
		eventPersistenceGapKind, eventDroppedLocalReasonCode).Scan(&milliseconds)
	if err != nil {
		return time.Time{}, false, classifyPersistenceError(err)
	}
	if !milliseconds.Valid {
		return time.Time{}, false, nil
	}
	return time.UnixMilli(milliseconds.Int64).UTC(), true, nil
}

func (w *Writer) latestRecoverySourceSequence(
	ctx context.Context,
	sessionID string,
) (int64, bool, error) {
	if w == nil || w.db == nil || ctx == nil || !validIdentifier(sessionID) {
		return 0, false, ErrWriterUnavailable
	}
	var sequence sql.NullInt64
	err := w.db.QueryRowContext(ctx, `SELECT MAX(ingest_sequence)
		FROM live_events
		WHERE session_id = ? AND event_role = ? AND ingest_sequence > 0`,
		sessionID, EventRoleSource).Scan(&sequence)
	if err != nil {
		return 0, false, classifyPersistenceError(err)
	}
	if !sequence.Valid {
		return 0, false, nil
	}
	return sequence.Int64, true, nil
}

func (m *Manager) persistRecoveryCheckpointState(
	ctx context.Context,
	current Checkpoint,
	state CheckpointState,
) (Checkpoint, error) {
	next := current
	next.State = state
	next.UpdatedAt = recoveryCheckpointTime(m.options.Now(), current.UpdatedAt)
	if err := m.persistWithRetry(ctx, Batch{
		SessionID: current.SessionID, PreviousSequence: current.CommittedSequence,
		Checkpoint: next,
	}); err != nil {
		return current, stablePersistenceError(err)
	}
	return next, nil
}

func recoveryCheckpointTime(now time.Time, previous time.Time) time.Time {
	now = now.UTC()
	if now.Before(previous) {
		return previous
	}
	return now
}

func (m *Manager) recoverAtRoot(
	ctx context.Context,
	descriptor SessionDescriptor,
	eventsRoot string,
	checkpoint Checkpoint,
	active *Spool,
	targetState CheckpointState,
	dropLedger *DropLedger,
	dedupe *Deduplicator,
) (Checkpoint, error) {
	if ctx == nil || checkpoint.SessionID != descriptor.SessionID ||
		!validCheckpointState(targetState) {
		return checkpoint, ErrPersistenceDegraded
	}
	if checkpoint.PrivacyKeyID != m.privacy.KeyID() {
		return checkpoint, ErrPrivacyKeyMismatch
	}
	stored, found, err := m.options.Writer.Checkpoint(ctx, descriptor.SessionID)
	if err != nil {
		return checkpoint, err
	}
	if !found {
		return checkpoint, ErrCheckpointConflict
	}
	if stored.PrivacyKeyID != m.privacy.KeyID() {
		return checkpoint, ErrPrivacyKeyMismatch
	}
	checkpoint = stored
	current := checkpoint
	var pendingDrops *DropSnapshot
	if dropLedger != nil {
		if snapshot, pending := dropLedger.Pending(); pending {
			pendingDrops = &snapshot
		}
	}
	acknowledgePending := func() {
		snapshot := pendingDrops
		pendingDrops = nil
		if snapshot == nil || dropLedger == nil {
			return
		}
		if err := dropLedger.Acknowledge(*snapshot); err != nil {
			if logger := m.options.Logger; logger != nil {
				logger.Warn("event drop ledger acknowledgement failed",
					"session_id", descriptor.SessionID,
					"error_code", stableErrorCode(err),
					"count", snapshot.TotalCount,
				)
			}
		}
	}
	persistPendingOnly := func() error {
		if pendingDrops == nil {
			return nil
		}
		next := current
		next.State = targetState
		next.UpdatedAt = m.options.Now().UTC()
		if err := m.persistWithRetry(ctx, Batch{
			SessionID: descriptor.SessionID, PreviousSequence: current.CommittedSequence,
			Gaps: []CaptureGap{pendingDrops.Gap}, Checkpoint: next,
		}); err != nil {
			return err
		}
		current = next
		acknowledgePending()
		return nil
	}
	if err := validateSpoolDirectory(eventsRoot); err != nil {
		return checkpoint, err
	}
	if active == nil {
		_, statErr := os.Stat(filepath.Join(eventsRoot, "spool"))
		if errors.Is(statErr, fs.ErrNotExist) {
			if err := persistPendingOnly(); err != nil {
				return current, err
			}
			if checkpoint.CommittedSequence == 0 {
				return current, nil
			}
			return current, ErrEventSpoolFatal
		}
		if statErr != nil {
			return checkpoint, ErrEventSpoolFatal
		}
	}
	normalizer, err := NewNormalizer(m.privacy, DefaultNormalizerVersion)
	if err != nil {
		return checkpoint, ErrPrivacyKeyInvalid
	}
	lastSequence := checkpoint.CommittedSequence
	envelopes := make([]IngestEnvelope, 0, m.options.BatchSize)
	raws := make([]RawRef, 0, m.options.BatchSize)
	var lastSpool SpoolPosition
	var lastRaw SpoolPosition
	flush := func() error {
		if len(envelopes) == 0 {
			return nil
		}
		folded, err := m.foldDurableBatch(ctx, normalizer, dedupe, envelopes, raws)
		if err != nil {
			return err
		}
		next := Checkpoint{
			SessionID:         descriptor.SessionID,
			CommittedSequence: envelopes[len(envelopes)-1].Sequence,
			State:             targetState, PrivacyKeyID: m.privacy.KeyID(),
			Spool: lastSpool, Raw: lastRaw, UpdatedAt: m.options.Now().UTC(),
		}
		events := make([]Event, 0, len(folded.Sources)+len(folded.Aggregates))
		events = append(events, folded.Sources...)
		events = append(events, folded.Aggregates...)
		var gaps []CaptureGap
		if pendingDrops != nil {
			gaps = append(gaps, pendingDrops.Gap)
		}
		if err := m.persistWithRetry(ctx, Batch{
			SessionID:        descriptor.SessionID,
			PreviousSequence: current.CommittedSequence,
			Events:           events, GiftCombos: folded.Combos, Gaps: gaps,
			Checkpoint: next,
		}); err != nil {
			return err
		}
		current = next
		addCommittedDedupeKeys(dedupe, folded.DedupeKeys, m.options.Now().UTC())
		acknowledgePending()
		envelopes = envelopes[:0]
		raws = raws[:0]
		return nil
	}
	visit := func(record SpoolRecord, position SpoolPosition) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		envelope := record.Envelope
		if record.Version != ContractVersion || envelope.SessionID != descriptor.SessionID ||
			!validIdentifier(envelope.EventID) || envelope.Sequence <= lastSequence ||
			envelope.ReceivedAt.IsZero() || envelope.SessionOffsetMS < 0 {
			return fmt.Errorf("%w: invalid recovery record", ErrFrameCorrupt)
		}
		lastSequence = envelope.Sequence
		envelopes = append(envelopes, envelope)
		raws = append(raws, record.Raw)
		lastSpool = position
		lastRaw = SpoolPosition{
			File: record.Raw.File, Offset: record.Raw.Offset + record.Raw.Length,
		}
		if len(envelopes) >= m.options.BatchSize {
			return flush()
		}
		return nil
	}
	if active != nil {
		err = active.ReplayCheckpoint(ctx, checkpoint, visit)
	} else {
		options := m.options.SpoolOptions(eventsRoot)
		options.Root = eventsRoot
		err = ReplaySpoolCheckpointWithOptions(options, checkpoint, visit)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
			isDatabasePersistenceError(err) || errors.Is(err, ErrGiftComboCapacity) {
			return current, err
		}
		return current, ErrEventSpoolFatal
	}
	if err := flush(); err != nil {
		return current, err
	}
	if err := persistPendingOnly(); err != nil {
		return current, err
	}
	return current, nil
}

func validateSpoolDirectory(eventsRoot string) error {
	spoolRoot := filepath.Join(eventsRoot, "spool")
	info, err := os.Lstat(spoolRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return ErrSessionPathInvalid
	}
	return nil
}
