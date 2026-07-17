package eventstore

import (
	"context"
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
