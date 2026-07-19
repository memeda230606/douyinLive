package eventstore

import (
	"context"
	"errors"
	"strings"
	"time"
)

type foldedDurableBatch struct {
	Sources        []Event
	PublishSources []Event
	Aggregates     []Event
	Combos         []GiftComboState
	DedupeKeys     []string
}

func (m *Manager) foldDurableBatch(
	ctx context.Context,
	normalizer *Normalizer,
	dedupe *Deduplicator,
	envelopes []IngestEnvelope,
	raws []RawRef,
) (foldedDurableBatch, error) {
	if len(envelopes) == 0 || len(envelopes) != len(raws) {
		return foldedDurableBatch{}, ErrInvalidBatch
	}
	results := make([]NormalizedResult, len(envelopes))
	cachedDuplicates := make([]bool, len(envelopes))
	folded := foldedDurableBatch{
		Sources:    make([]Event, 0, len(envelopes)),
		DedupeKeys: make([]string, 0, len(envelopes)),
	}
	dedupeKeys := make([]string, 0, len(envelopes))
	comboKeys := make([]string, 0, len(envelopes))
	dedupeNow := m.options.Now().UTC()
	for index, envelope := range envelopes {
		result := normalizer.NormalizeDetailed(envelope)
		result.Event.Raw = raws[index]
		results[index] = result
		folded.Sources = append(folded.Sources, result.Event)
		folded.DedupeKeys = append(folded.DedupeKeys, result.Event.DedupeKey)
		if result.Gift != nil && strings.TrimSpace(result.Gift.GiftID) != "" {
			key, err := GiftObservationKey(*result.Gift)
			if err != nil {
				return foldedDurableBatch{}, err
			}
			comboKeys = append(comboKeys, key)
		}
		if dedupe.Seen(result.Event.DedupeKey, dedupeNow) {
			cachedDuplicates[index] = true
			continue
		}
		// Query SQLite for every non-cached source. The in-memory deduper is
		// bounded and expiring, so it cannot prove that a non-gift source is new.
		dedupeKeys = append(dedupeKeys, result.Event.DedupeKey)
	}
	existingDedupe, err := m.options.Writer.ExistingSourceDedupeKeys(
		ctx, envelopes[0].SessionID, dedupeKeys,
	)
	if err != nil {
		return foldedDurableBatch{}, err
	}
	folds, err := m.options.Writer.GiftFolds(ctx, envelopes[0].SessionID, comboKeys)
	if err != nil {
		return foldedDurableBatch{}, err
	}
	capacity := m.options.BatchSize
	if capacity < 1 {
		capacity = DefaultEventBatchSize
	}
	aggregator := NewGiftComboAggregatorWithCapacity(DefaultGiftComboIdle, capacity)
	closed := make(map[string]struct{}, len(folds))
	for key, fold := range folds {
		if fold.State.Status == ComboClosed {
			closed[key] = struct{}{}
			continue
		}
		if err := aggregator.Restore(fold); err != nil {
			return foldedDurableBatch{}, err
		}
	}
	seen := make(map[string]struct{}, len(results))
	appendUpdates := func(updates []GiftComboUpdate) {
		for _, update := range updates {
			folded.Combos = append(folded.Combos, update.State)
			if update.Aggregate != nil {
				folded.Aggregates = append(folded.Aggregates, *update.Aggregate)
			}
		}
	}
	for index, result := range results {
		appendUpdates(aggregator.FlushIdle(result.Event.ReceivedAt))
		if cachedDuplicates[index] {
			continue
		}
		if _, exists := existingDedupe[result.Event.DedupeKey]; exists {
			continue
		}
		if _, exists := seen[result.Event.DedupeKey]; exists {
			continue
		}
		seen[result.Event.DedupeKey] = struct{}{}
		folded.PublishSources = append(folded.PublishSources, result.Event)
		if result.Gift == nil || strings.TrimSpace(result.Gift.GiftID) == "" {
			continue
		}
		key, err := GiftObservationKey(*result.Gift)
		if err != nil {
			return foldedDurableBatch{}, err
		}
		if _, terminal := closed[key]; terminal {
			continue
		}
		updates, err := aggregator.Observe(result.Event, *result.Gift)
		if err != nil {
			return foldedDurableBatch{}, err
		}
		appendUpdates(updates)
	}
	return folded, nil
}

func foldGiftSnapshots(
	snapshots []GiftFoldSnapshot,
	at time.Time,
	idleOnly bool,
) ([]Event, []GiftComboState, error) {
	if len(snapshots) == 0 {
		return nil, nil, nil
	}
	aggregator := NewGiftComboAggregatorWithCapacity(DefaultGiftComboIdle, len(snapshots))
	for _, snapshot := range snapshots {
		if err := aggregator.Restore(snapshot); err != nil {
			return nil, nil, err
		}
	}
	var updates []GiftComboUpdate
	if idleOnly {
		updates = aggregator.FlushIdle(at)
	} else {
		updates = aggregator.CloseAll(at)
	}
	events := make([]Event, 0, len(updates))
	combos := make([]GiftComboState, 0, len(updates))
	for _, update := range updates {
		combos = append(combos, update.State)
		if update.Aggregate != nil {
			events = append(events, *update.Aggregate)
		}
	}
	return events, combos, nil
}

func isDatabasePersistenceError(err error) bool {
	return errors.Is(err, ErrPersistenceBusy) || errors.Is(err, ErrPersistenceFull) ||
		errors.Is(err, ErrPersistenceCorrupt) || errors.Is(err, ErrPersistenceConstraint) ||
		errors.Is(err, ErrCheckpointConflict) || errors.Is(err, ErrPersistenceCommit) ||
		errors.Is(err, ErrInvalidBatch)
}
