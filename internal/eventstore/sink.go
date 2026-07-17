package eventstore

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	douyinLive "github.com/jwwsjlm/douyinLive/v2"
)

type dropAccumulator struct {
	count       int64
	startedAt   time.Time
	endedAt     time.Time
	startOffset int64
	endOffset   int64
}

const maxDropCount int64 = 1<<63 - 1

type sessionRuntime struct {
	sink       *SessionSink
	queue      *EnvelopeQueue
	spool      *Spool
	normalizer *Normalizer
	dropLedger *DropLedger
	dedupe     *Deduplicator

	mu          sync.Mutex
	accepting   bool
	closing     bool
	spoolFatal  bool
	sequence    int64
	drops       dropAccumulator
	dropMerging dropAccumulator

	closeOnce      sync.Once
	closeRequested chan struct{}
	done           chan struct{}
	closeErr       error

	checkpoint Checkpoint
	degraded   bool
}

func (m *Manager) openSession(ctx context.Context, descriptor SessionDescriptor) (*SessionSink, error) {
	if m == nil || ctx == nil {
		return nil, ErrManagerOptions
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	eventsRoot, err := resolveSessionEventsRoot(m.options.DataRoot, descriptor)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.accepting {
		return nil, ErrManagerClosed
	}
	if _, exists := m.sessions[descriptor.SessionID]; exists {
		return nil, ErrSessionAlreadyOpen
	}
	dropLedger, err := OpenDropLedger(eventsRoot, descriptor.SessionID)
	if err != nil {
		return nil, err
	}
	dedupe := NewDeduplicator(DefaultDedupeTTL, DefaultDedupeCapacity)
	checkpoint, found, err := m.options.Writer.Checkpoint(ctx, descriptor.SessionID)
	if err != nil {
		return nil, stablePersistenceError(err)
	}
	if found && checkpoint.PrivacyKeyID != m.privacy.KeyID() {
		return nil, ErrPrivacyKeyMismatch
	}
	if found && (checkpoint.State == CheckpointClosing || checkpoint.State == CheckpointClosed) {
		return nil, ErrSessionClosed
	}
	if found {
		checkpoint, err = m.recoverAtRoot(
			ctx, descriptor, eventsRoot, checkpoint, nil, checkpoint.State, dropLedger, dedupe,
		)
		if err != nil {
			if isDatabasePersistenceError(err) {
				return nil, stablePersistenceError(err)
			}
			return nil, err
		}
	}
	queue, err := NewEnvelopeQueue(m.options.QueueLimits)
	if err != nil {
		return nil, ErrManagerOptions
	}
	if err := validateSpoolDirectory(eventsRoot); err != nil {
		return nil, err
	}
	spoolOptions := m.options.SpoolOptions(eventsRoot)
	spoolOptions.Root = eventsRoot
	spool, err := OpenSpool(spoolOptions)
	if err != nil {
		return nil, ErrEventSpoolFatal
	}
	normalizer, err := NewNormalizer(m.privacy, DefaultNormalizerVersion)
	if err != nil {
		_ = spool.Close(context.Background())
		return nil, ErrPrivacyKeyInvalid
	}
	if !found {
		checkpoint = Checkpoint{
			SessionID: descriptor.SessionID, State: CheckpointOpen,
			PrivacyKeyID: m.privacy.KeyID(), UpdatedAt: m.options.Now().UTC(),
		}
		if err := m.persistWithRetry(ctx, Batch{
			SessionID: descriptor.SessionID, Checkpoint: checkpoint,
		}); err != nil {
			_ = spool.Close(context.Background())
			return nil, stablePersistenceError(err)
		}
	}
	if checkpoint.State == CheckpointDegraded {
		reopened := checkpoint
		reopened.State = CheckpointOpen
		reopened.UpdatedAt = m.options.Now().UTC()
		if err := m.persistWithRetry(ctx, Batch{
			SessionID:        descriptor.SessionID,
			PreviousSequence: checkpoint.CommittedSequence,
			Checkpoint:       reopened,
		}); err != nil {
			_ = spool.Close(context.Background())
			return nil, stablePersistenceError(err)
		}
		checkpoint = reopened
	}
	sink := &SessionSink{
		manager: m, descriptor: descriptor, eventsRoot: eventsRoot,
	}
	runtime := &sessionRuntime{
		sink: sink, queue: queue, spool: spool, normalizer: normalizer,
		dropLedger: dropLedger, dedupe: dedupe, accepting: true,
		sequence:       checkpoint.CommittedSequence,
		closeRequested: make(chan struct{}), done: make(chan struct{}),
		checkpoint: checkpoint,
	}
	sink.runtime = runtime
	m.sessions[descriptor.SessionID] = sink
	go runtime.run()
	return sink, nil
}

func resolveSessionEventsRoot(dataRoot string, descriptor SessionDescriptor) (string, error) {
	if !validIdentifier(descriptor.SessionID) || descriptor.StartedAt.IsZero() ||
		!validSessionRelativePath(descriptor.DataPath) {
		return "", ErrSessionPathInvalid
	}
	parts := strings.Split(descriptor.DataPath, "/")
	hasSessionsComponent := false
	for _, part := range parts[:len(parts)-1] {
		if part == "sessions" {
			hasSessionsComponent = true
			break
		}
	}
	if path.Base(descriptor.DataPath) != descriptor.SessionID || !hasSessionsComponent {
		return "", ErrSessionPathInvalid
	}
	root := filepath.Clean(dataRoot)
	target := filepath.Join(root, filepath.FromSlash(descriptor.DataPath), "events")
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == "." || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", ErrSessionPathInvalid
	}
	current := root
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return "", ErrSessionPathInvalid
		}
	}
	return target, nil
}

func (r *sessionRuntime) accept(message *douyinLive.LiveMessage) {
	if message == nil {
		return
	}
	receivedAt := message.ReceivedAt
	if receivedAt.IsZero() {
		receivedAt = r.sink.manager.options.Now().UTC()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.accepting {
		if r.spoolFatal && !r.closing {
			r.sequence++
			offset := receivedAt.Sub(r.sink.descriptor.StartedAt).Milliseconds()
			if offset < 0 {
				offset = 0
			}
			r.recordDropLocked(receivedAt.UTC(), offset)
		}
		return
	}
	method := strings.Clone(message.GetMethod())
	payload := message.GetPayload()
	r.sequence++
	offset := receivedAt.Sub(r.sink.descriptor.StartedAt).Milliseconds()
	if offset < 0 {
		offset = 0
	}
	envelope := IngestEnvelope{
		SessionID: r.sink.descriptor.SessionID, EventID: newEventID(),
		Sequence: r.sequence, Method: method,
		PlatformRoomID: r.sink.descriptor.PlatformRoomID,
		ReceivedAt:     receivedAt.UTC(), SessionOffsetMS: offset, Payload: payload,
	}
	if err := r.queue.TryPush(envelope, QueueClassNormal); err == nil {
		return
	}
	if err := r.queue.TryPush(envelope, QueueClassEmergency); err == nil {
		return
	}
	r.recordDropLocked(receivedAt.UTC(), offset)
}

func (r *sessionRuntime) recordDropLocked(at time.Time, offset int64) {
	if r.drops.count == 0 {
		r.drops.startedAt = at
		r.drops.startOffset = offset
	}
	if r.drops.count < maxDropCount {
		r.drops.count++
	}
	r.drops.endedAt = at
	r.drops.endOffset = offset
}

func (r *sessionRuntime) takeDrops() dropAccumulator {
	r.mu.Lock()
	if r.dropMerging.count != 0 {
		value := r.dropMerging
		r.mu.Unlock()
		return value
	}
	value := r.drops
	r.drops = dropAccumulator{}
	r.dropMerging = value
	r.mu.Unlock()
	return value
}

func (r *sessionRuntime) commitDrops() {
	r.mu.Lock()
	r.dropMerging = dropAccumulator{}
	r.mu.Unlock()
}

func (r *sessionRuntime) restoreDrops(value dropAccumulator) {
	r.mu.Lock()
	if r.dropMerging.count != 0 {
		value = r.dropMerging
		r.dropMerging = dropAccumulator{}
	}
	if value.count == 0 {
		r.mu.Unlock()
		return
	}
	current := r.drops
	if current.count == 0 {
		r.drops = value
	} else {
		if value.count > maxDropCount-current.count {
			value.count = maxDropCount
		} else {
			value.count += current.count
		}
		value.endedAt = current.endedAt
		value.endOffset = current.endOffset
		r.drops = value
	}
	r.mu.Unlock()
}

func (r *sessionRuntime) stageDrops() (*DropSnapshot, error) {
	value := r.takeDrops()
	if r.dropLedger == nil {
		r.restoreDrops(value)
		return nil, ErrDropLedgerInvalid
	}
	if value.count > 0 {
		snapshot, err := r.dropLedger.Merge(DropDelta{
			Count: value.count, StartedAt: value.startedAt, EndedAt: value.endedAt,
			StartOffsetMS: value.startOffset, EndOffsetMS: value.endOffset,
		})
		if err != nil {
			r.restoreDrops(value)
			return nil, err
		}
		r.commitDrops()
		return &snapshot, nil
	}
	if snapshot, pending := r.dropLedger.Pending(); pending {
		return &snapshot, nil
	}
	return nil, nil
}

func (r *sessionRuntime) acknowledgeDrops(snapshot *DropSnapshot) {
	if snapshot == nil {
		return
	}
	if err := r.dropLedger.Acknowledge(*snapshot); err != nil {
		r.logFailure(stableErrorCode(err), snapshot.TotalCount)
	}
}

func (r *sessionRuntime) close(ctx context.Context) error {
	r.startClose()
	if ctx == nil {
		return ErrManagerOptions
	}
	select {
	case <-r.done:
		r.mu.Lock()
		err := r.closeErr
		r.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *sessionRuntime) startClose() {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closing = true
		r.accepting = false
		r.queue.Close()
		close(r.closeRequested)
		r.mu.Unlock()
	})
}

func (r *sessionRuntime) run() {
	var terminal error
	for {
		ctx, cancel := context.WithTimeout(context.Background(), r.sink.manager.options.BatchInterval)
		first, err := r.queue.Pop(ctx)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			if err := r.processIdle(); err != nil {
				terminal = err
				break
			}
			continue
		}
		if errors.Is(err, ErrQueueClosed) {
			break
		}
		if err != nil {
			terminal = ErrEventSpoolFatal
			break
		}
		batch := []IngestEnvelope{first.Envelope}
		r.collectBatch(&batch)
		if err := r.processEnvelopes(batch); err != nil {
			terminal = err
			break
		}
	}
	if errors.Is(terminal, ErrEventSpoolFatal) {
		r.waitForCloseAfterFatal()
	}
	result := r.finalize(terminal)
	r.mu.Lock()
	r.accepting = false
	r.closeErr = result
	r.mu.Unlock()
	close(r.done)
	r.sink.manager.sessionFinished(r.sink)
}

func (r *sessionRuntime) waitForCloseAfterFatal() {
	interval := r.sink.manager.options.BatchInterval
	if interval <= 0 {
		interval = time.Second
	}
	if _, err := r.stageDrops(); err != nil {
		r.logFailure(stableErrorCode(err), 0)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.closeRequested:
			return
		case <-ticker.C:
			if _, err := r.stageDrops(); err != nil {
				r.logFailure(stableErrorCode(err), 0)
			}
		}
	}
}

func (r *sessionRuntime) collectBatch(batch *[]IngestEnvelope) {
	deadline := time.Now().Add(r.sink.manager.options.BatchInterval)
	for len(*batch) < r.sink.manager.options.BatchSize {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), remaining)
		item, err := r.queue.Pop(ctx)
		cancel()
		if err != nil {
			return
		}
		*batch = append(*batch, item.Envelope)
	}
}

func (r *sessionRuntime) processEnvelopes(envelopes []IngestEnvelope) error {
	pendingDrops, err := r.stageDrops()
	if err != nil {
		r.failSpool(envelopes)
		r.logFailure(stableErrorCode(err), int64(len(envelopes)))
		return ErrEventSpoolFatal
	}
	appends, err := r.spool.AppendBatch(context.Background(), envelopes)
	if err != nil {
		durableCount := len(appends)
		if durableCount > len(envelopes) {
			// A malformed prefix cannot be trusted. Fail closed and audit the
			// complete input batch instead of under-reporting data loss.
			durableCount = 0
		}
		for index := 0; index < durableCount; index++ {
			if appends[index].Record.Envelope.Sequence != envelopes[index].Sequence {
				durableCount = 0
				break
			}
		}
		r.failSpool(envelopes[durableCount:])
		return ErrEventSpoolFatal
	}
	if r.degraded {
		return r.tryRecover()
	}
	raws := make([]RawRef, len(appends))
	for index := range appends {
		raws[index] = appends[index].Record.Raw
	}
	folded, err := r.sink.manager.foldDurableBatch(
		context.Background(), r.normalizer, r.dedupe, envelopes, raws,
	)
	if err != nil {
		if _, stageErr := r.stageDrops(); stageErr != nil {
			r.failSpool(nil)
			r.logFailure(stableErrorCode(stageErr), 0)
			return ErrEventSpoolFatal
		}
		r.degraded = true
		r.logFailure(stableErrorCode(err), int64(len(envelopes)))
		return nil
	}
	var gaps []CaptureGap
	if pendingDrops != nil {
		gaps = append(gaps, pendingDrops.Gap)
	}
	checkpoint := checkpointFromAppend(
		r.checkpoint, appends[len(appends)-1], CheckpointOpen, r.sink.manager,
	)
	events := append(append([]Event(nil), folded.Sources...), folded.Aggregates...)
	err = r.sink.manager.persistWithRetry(context.Background(), Batch{
		SessionID:        r.sink.descriptor.SessionID,
		PreviousSequence: r.checkpoint.CommittedSequence,
		Events:           events, GiftCombos: folded.Combos, Gaps: gaps,
		Checkpoint: checkpoint,
	})
	if err != nil {
		if _, stageErr := r.stageDrops(); stageErr != nil {
			r.failSpool(nil)
			r.logFailure(stableErrorCode(stageErr), 0)
			return ErrEventSpoolFatal
		}
		r.degraded = true
		r.logFailure(stableErrorCode(err), int64(len(envelopes)))
		return nil
	}
	r.checkpoint = checkpoint
	addCommittedDedupeKeys(
		r.dedupe, folded.DedupeKeys,
		r.sink.manager.options.Now().UTC(),
	)
	r.acknowledgeDrops(pendingDrops)
	if _, err := r.stageDrops(); err != nil {
		r.failSpool(nil)
		r.logFailure(stableErrorCode(err), 0)
		return ErrEventSpoolFatal
	}
	return nil
}

func checkpointFromAppend(previous Checkpoint, appendResult DurableAppend, state CheckpointState, manager *Manager) Checkpoint {
	return Checkpoint{
		SessionID:         previous.SessionID,
		CommittedSequence: appendResult.Record.Envelope.Sequence,
		State:             state, PrivacyKeyID: manager.privacy.KeyID(),
		Spool: appendResult.Spool, Raw: appendResult.Raw,
		UpdatedAt: manager.options.Now().UTC(),
	}
}

func (r *sessionRuntime) tryRecover() error {
	if _, err := r.stageDrops(); err != nil {
		r.failSpool(nil)
		r.logFailure(stableErrorCode(err), 0)
		return ErrEventSpoolFatal
	}
	checkpoint, err := r.sink.manager.recoverAtRoot(
		context.Background(), r.sink.descriptor, r.sink.eventsRoot,
		r.checkpoint, r.spool, CheckpointOpen, r.dropLedger, r.dedupe,
	)
	if err != nil {
		if _, stageErr := r.stageDrops(); stageErr != nil {
			r.failSpool(nil)
			return ErrEventSpoolFatal
		}
		if errors.Is(err, ErrEventSpoolFatal) {
			r.failSpool(nil)
			return ErrEventSpoolFatal
		}
		r.logFailure(stableErrorCode(err), 0)
		return nil
	}
	r.checkpoint = checkpoint
	r.degraded = false
	return r.persistState(CheckpointOpen, nil, nil)
}

func (r *sessionRuntime) processIdle() error {
	if r.degraded {
		if err := r.tryRecover(); err != nil || r.degraded {
			return err
		}
	} else {
		if _, err := r.stageDrops(); err != nil {
			r.failSpool(nil)
			r.logFailure(stableErrorCode(err), 0)
			return ErrEventSpoolFatal
		}
	}
	cutoff := r.sink.manager.options.Now().UTC().Add(-DefaultGiftComboIdle).UnixMilli()
	folds, err := r.sink.manager.options.Writer.OpenGiftFolds(
		context.Background(), r.sink.descriptor.SessionID, &cutoff,
		r.sink.manager.options.BatchSize,
	)
	if err != nil {
		if _, stageErr := r.stageDrops(); stageErr != nil {
			r.failSpool(nil)
			r.logFailure(stableErrorCode(stageErr), 0)
			return ErrEventSpoolFatal
		}
		r.degraded = true
		r.logFailure(stableErrorCode(err), 0)
		return nil
	}
	events, combos, err := foldGiftSnapshots(
		folds, r.sink.manager.options.Now().UTC(), true,
	)
	if err != nil {
		r.degraded = true
		r.logFailure(stableErrorCode(err), int64(len(folds)))
		return nil
	}
	return r.persistState(CheckpointOpen, events, combos)
}

func (r *sessionRuntime) persistState(
	state CheckpointState,
	events []Event,
	combos []GiftComboState,
) error {
	pendingDrops, err := r.stageDrops()
	if err != nil {
		r.failSpool(nil)
		r.logFailure(stableErrorCode(err), 0)
		return ErrEventSpoolFatal
	}
	var gaps []CaptureGap
	if pendingDrops != nil {
		gaps = append(gaps, pendingDrops.Gap)
	}
	if len(events) == 0 && len(combos) == 0 && len(gaps) == 0 &&
		state == r.checkpoint.State {
		return nil
	}
	checkpoint := r.checkpoint
	checkpoint.State = state
	checkpoint.UpdatedAt = r.sink.manager.options.Now().UTC()
	err = r.sink.manager.persistWithRetry(context.Background(), Batch{
		SessionID:        r.sink.descriptor.SessionID,
		PreviousSequence: r.checkpoint.CommittedSequence,
		Events:           events, GiftCombos: combos, Gaps: gaps,
		Checkpoint: checkpoint,
	})
	if err != nil {
		if _, stageErr := r.stageDrops(); stageErr != nil {
			r.failSpool(nil)
			r.logFailure(stableErrorCode(stageErr), 0)
			return ErrEventSpoolFatal
		}
		r.degraded = true
		r.logFailure(stableErrorCode(err), int64(len(events)))
		return nil
	}
	r.checkpoint = checkpoint
	r.acknowledgeDrops(pendingDrops)
	if _, err := r.stageDrops(); err != nil {
		r.failSpool(nil)
		r.logFailure(stableErrorCode(err), 0)
		return ErrEventSpoolFatal
	}
	return nil
}

func (r *sessionRuntime) closeAllGiftFolds() error {
	for {
		folds, err := r.sink.manager.options.Writer.OpenGiftFolds(
			context.Background(), r.sink.descriptor.SessionID, nil,
			r.sink.manager.options.BatchSize,
		)
		if err != nil {
			return err
		}
		if len(folds) == 0 {
			return nil
		}
		events, combos, err := foldGiftSnapshots(
			folds, r.sink.manager.options.Now().UTC(), false,
		)
		if err != nil {
			return err
		}
		if err := r.persistState(CheckpointOpen, events, combos); err != nil {
			return err
		}
		if r.degraded {
			return ErrPersistenceDegraded
		}
	}
}

func (r *sessionRuntime) finalize(terminal error) error {
	r.startClose()
	if terminal != nil {
		r.bestEffortDegraded()
		_ = r.spool.Close(context.Background())
		return ErrEventSpoolFatal
	}
	if r.degraded {
		if err := r.tryRecover(); err != nil || r.degraded {
			r.bestEffortDegraded()
			_ = r.spool.Close(context.Background())
			return ErrPersistenceDegraded
		}
	}
	if err := r.closeAllGiftFolds(); err != nil {
		r.logFailure(stableErrorCode(err), 0)
		r.bestEffortDegraded()
		_ = r.spool.Close(context.Background())
		return ErrPersistenceDegraded
	}
	if err := r.persistState(CheckpointClosing, nil, nil); err != nil || r.degraded {
		r.bestEffortDegraded()
		_ = r.spool.Close(context.Background())
		return ErrPersistenceDegraded
	}
	if err := r.spool.Close(context.Background()); err != nil {
		r.bestEffortDegraded()
		return ErrEventSpoolFatal
	}
	if err := r.persistState(CheckpointClosed, nil, nil); err != nil || r.degraded {
		r.bestEffortDegraded()
		return ErrPersistenceDegraded
	}
	return nil
}

func (r *sessionRuntime) bestEffortDegraded() {
	_ = r.persistState(CheckpointDegraded, nil, nil)
}

func (r *sessionRuntime) failSpool(failed []IngestEnvelope) {
	r.mu.Lock()
	r.accepting = false
	r.spoolFatal = true
	r.queue.Close()
	for _, envelope := range failed {
		r.recordDropLocked(envelope.ReceivedAt, envelope.SessionOffsetMS)
	}
	r.mu.Unlock()
	for {
		item, err := r.queue.Pop(context.Background())
		if err != nil {
			break
		}
		r.mu.Lock()
		r.recordDropLocked(item.Envelope.ReceivedAt, item.Envelope.SessionOffsetMS)
		r.mu.Unlock()
	}
	r.mu.Lock()
	count := r.drops.count
	r.mu.Unlock()
	r.logFailure("EVENT_SPOOL_FATAL", count)
}

func (r *sessionRuntime) logFailure(code string, count int64) {
	logger := r.sink.manager.options.Logger
	if logger == nil {
		return
	}
	logger.Warn("event persistence state changed",
		"session_id", r.sink.descriptor.SessionID,
		"error_code", code, "count", count,
	)
}

func (m *Manager) sessionFinished(sink *SessionSink) {
	m.mu.Lock()
	if current := m.sessions[sink.descriptor.SessionID]; current == sink {
		delete(m.sessions, sink.descriptor.SessionID)
	}
	m.mu.Unlock()
}

func (m *Manager) shutdown(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.shutdownOnce.Do(func() {
		m.mu.Lock()
		m.accepting = false
		sinks := make([]*SessionSink, 0, len(m.sessions))
		for _, sink := range m.sessions {
			sinks = append(sinks, sink)
			sink.runtime.startClose()
		}
		m.mu.Unlock()
		go m.runShutdown(sinks)
	})
	if ctx == nil {
		return ErrManagerOptions
	}
	select {
	case <-m.shutdownDone:
		m.mu.Lock()
		err := m.shutdownErr
		m.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) runShutdown(sinks []*SessionSink) {
	var result error
	for _, sink := range sinks {
		<-sink.runtime.done
		sink.runtime.mu.Lock()
		err := sink.runtime.closeErr
		sink.runtime.mu.Unlock()
		result = errors.Join(result, err)
	}
	m.mu.Lock()
	m.shutdownErr = result
	close(m.shutdownDone)
	m.mu.Unlock()
}

func stablePersistenceError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrPrivacyKeyMismatch):
		return ErrPrivacyKeyMismatch
	case isDatabasePersistenceError(err):
		return ErrPersistenceDegraded
	default:
		return ErrPersistenceDegraded
	}
}

func stableErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrPersistenceBusy):
		return "EVENT_DB_BUSY"
	case errors.Is(err, ErrPersistenceFull):
		return "EVENT_DB_FULL"
	case errors.Is(err, ErrPersistenceCorrupt):
		return "EVENT_DB_CORRUPT"
	case errors.Is(err, ErrCheckpointConflict):
		return "EVENT_CHECKPOINT_CONFLICT"
	case errors.Is(err, ErrPersistenceConstraint), errors.Is(err, ErrInvalidBatch):
		return "EVENT_BATCH_INVALID"
	case errors.Is(err, ErrPrivacyKeyMismatch):
		return "EVENT_PRIVACY_KEY_MISMATCH"
	case errors.Is(err, ErrGiftComboCapacity):
		return "EVENT_GIFT_CAPACITY"
	case errors.Is(err, ErrEventSpoolFatal), errors.Is(err, ErrSpoolFailed):
		return "EVENT_SPOOL_FATAL"
	case errors.Is(err, ErrDropLedgerCorrupt):
		return "EVENT_DROP_LEDGER_CORRUPT"
	case errors.Is(err, ErrDropLedgerInvalid):
		return "EVENT_DROP_LEDGER_INVALID"
	case errors.Is(err, ErrDropLedgerIO):
		return "EVENT_DROP_LEDGER_IO"
	default:
		return "EVENT_PERSISTENCE_FAILED"
	}
}
