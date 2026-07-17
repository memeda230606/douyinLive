package eventstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	DefaultGiftComboIdle     = 10 * time.Second
	DefaultGiftComboCapacity = 512
)

var (
	ErrGiftSessionMissing = errors.New("EVENT_GIFT_SESSION_MISSING")
	ErrGiftComboCapacity  = errors.New("EVENT_GIFT_CAPACITY")
)

// GiftObservation is transient normalization output. GroupID and TraceID are
// never copied into Event, GiftComboState, errors, or diagnostics.
type GiftObservation struct {
	SessionID         string
	SourceEventID     string
	Sequence          int64
	SessionOffsetMS   int64
	ReceivedAt        time.Time
	UserHash          string
	DisplayName       string
	GiftID            string
	GiftName          string
	GroupID           uint64
	TraceID           string
	Count             int64
	UnitDiamond       int64
	Combo             bool
	RepeatEnd         bool
	NormalizerVersion string
}

type GiftComboUpdate struct {
	State     GiftComboState
	Aggregate *Event
}

type giftComboEntry struct {
	state       GiftComboState
	lastSource  Event
	unitDiamond int64
}

// GiftComboAggregator folds cumulative combo counters while retaining every
// source event separately. Closed combo keys are terminal and never reopened.
type GiftComboAggregator struct {
	mu           sync.Mutex
	idle         time.Duration
	capacity     int
	tombstoneTTL time.Duration
	entries      map[string]*giftComboEntry
	closed       map[string]time.Time
}

func NewGiftComboAggregator(idle time.Duration) *GiftComboAggregator {
	return NewGiftComboAggregatorWithCapacity(idle, DefaultGiftComboCapacity)
}

func NewGiftComboAggregatorWithCapacity(idle time.Duration, capacity int) *GiftComboAggregator {
	if idle <= 0 {
		idle = DefaultGiftComboIdle
	}
	if capacity <= 0 {
		capacity = DefaultGiftComboCapacity
	}
	return &GiftComboAggregator{
		idle: idle, capacity: capacity, tombstoneTTL: time.Hour,
		entries: make(map[string]*giftComboEntry),
		closed:  make(map[string]time.Time),
	}
}

// GiftComboKey applies group ID, trace hash, then user/gift/30-second fallback.
// The returned value is a one-way digest and cannot expose group or trace data.
func GiftComboKey(observation GiftObservation) (string, error) {
	if strings.TrimSpace(observation.SessionID) == "" {
		return "", ErrGiftSessionMissing
	}
	source := "fallback"
	value := strings.Join([]string{
		observation.UserHash,
		observation.GiftID,
		strconv.FormatInt(observation.ReceivedAt.UTC().Unix()/30, 10),
	}, "\x00")
	if observation.GroupID != 0 {
		source = "group"
		value = strconv.FormatUint(observation.GroupID, 10)
	} else if trace := strings.TrimSpace(observation.TraceID); trace != "" {
		source = "trace"
		traceDigest := sha256.Sum256([]byte(trace))
		value = hex.EncodeToString(traceDigest[:])
	}
	digest := sha256.Sum256([]byte(strings.Join([]string{
		"douyin-gift-combo-v1",
		observation.SessionID,
		source,
		value,
	}, "\x00")))
	return "gc1." + source[:1] + "." + hex.EncodeToString(digest[:]), nil
}

func GiftObservationKey(observation GiftObservation) (string, error) {
	key, err := GiftComboKey(observation)
	if err != nil {
		return "", err
	}
	if !observation.Combo && observation.GroupID == 0 && strings.TrimSpace(observation.TraceID) == "" {
		onceDigest := sha256.Sum256([]byte(key + "\x00" + observation.SourceEventID + "\x00" + strconv.FormatInt(observation.Sequence, 10)))
		key = "gc1.o." + hex.EncodeToString(onceDigest[:])
	}
	return key, nil
}

// Observe returns any combos idled by this timestamp followed by this
// observation's state update. Non-combo and repeat-end observations close
// immediately. Count is cumulative and therefore only its maximum is retained.
func (a *GiftComboAggregator) Observe(source Event, observation GiftObservation) ([]GiftComboUpdate, error) {
	if a == nil {
		return nil, errors.New("EVENT_GIFT_AGGREGATOR_MISSING")
	}
	if strings.TrimSpace(observation.GiftID) == "" {
		a.mu.Lock()
		updates := a.closeIdleLocked(observation.ReceivedAt)
		a.mu.Unlock()
		return updates, nil
	}
	key, err := GiftObservationKey(observation)
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	a.pruneClosedLocked(observation.ReceivedAt)
	updates := a.closeIdleLocked(observation.ReceivedAt)
	if _, terminal := a.closed[key]; terminal {
		return updates, nil
	}
	entry, exists := a.entries[key]
	if !exists && len(a.entries)+len(a.closed) >= a.capacity {
		a.evictOldestClosedLocked()
	}
	if !exists && len(a.entries)+len(a.closed) >= a.capacity {
		return updates, ErrGiftComboCapacity
	}
	count := observation.Count
	if count <= 0 {
		count = 1
	}
	if !exists {
		entry = &giftComboEntry{
			state: GiftComboState{
				SessionID:         observation.SessionID,
				ComboKey:          key,
				Status:            ComboOpen,
				UserHash:          observation.UserHash,
				GiftID:            observation.GiftID,
				GiftName:          observation.GiftName,
				TotalCount:        count,
				FirstSequence:     observation.Sequence,
				LastSequence:      observation.Sequence,
				StartedAt:         observation.ReceivedAt,
				UpdatedAt:         observation.ReceivedAt,
				NormalizerVersion: observation.NormalizerVersion,
			},
			lastSource:  source,
			unitDiamond: observation.UnitDiamond,
		}
		a.entries[key] = entry
	} else {
		if count > entry.state.TotalCount {
			entry.state.TotalCount = count
		}
		entry.state.LastSequence = observation.Sequence
		entry.state.UpdatedAt = observation.ReceivedAt
		entry.lastSource = source
		if entry.unitDiamond <= 0 && observation.UnitDiamond > 0 {
			entry.unitDiamond = observation.UnitDiamond
		}
	}
	entry.state.TotalValue = aggregateGiftValue(entry.state.TotalCount, entry.unitDiamond)

	if observation.RepeatEnd || !observation.Combo {
		updates = append(updates, a.closeLocked(key, entry, observation.ReceivedAt))
		return updates, nil
	}
	updates = append(updates, GiftComboUpdate{State: entry.state})
	return updates, nil
}

func (a *GiftComboAggregator) Restore(snapshot GiftFoldSnapshot) error {
	if a == nil || snapshot.State.Status != ComboOpen ||
		!validCombo(snapshot.State, snapshot.State.SessionID, snapshot.State.LastSequence) {
		return ErrPersistenceCorrupt
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruneClosedLocked(snapshot.State.UpdatedAt)
	if _, exists := a.entries[snapshot.State.ComboKey]; !exists && len(a.entries)+len(a.closed) >= a.capacity {
		a.evictOldestClosedLocked()
	}
	if _, exists := a.entries[snapshot.State.ComboKey]; !exists && len(a.entries)+len(a.closed) >= a.capacity {
		return ErrGiftComboCapacity
	}
	a.entries[snapshot.State.ComboKey] = &giftComboEntry{
		state: snapshot.State, lastSource: snapshot.LastSource,
		unitDiamond: snapshot.UnitDiamond,
	}
	return nil
}

func (a *GiftComboAggregator) EntryCount() int {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.entries) + len(a.closed)
}

func (a *GiftComboAggregator) FlushIdle(now time.Time) []GiftComboUpdate {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closeIdleLocked(now)
}

func (a *GiftComboAggregator) CloseAll(now time.Time) []GiftComboUpdate {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	keys := a.openKeysLocked()
	updates := make([]GiftComboUpdate, 0, len(keys))
	for _, key := range keys {
		updates = append(updates, a.closeLocked(key, a.entries[key], now))
	}
	return updates
}

func (a *GiftComboAggregator) closeIdleLocked(now time.Time) []GiftComboUpdate {
	keys := a.openKeysLocked()
	updates := make([]GiftComboUpdate, 0)
	for _, key := range keys {
		entry := a.entries[key]
		if now.Before(entry.state.UpdatedAt.Add(a.idle)) {
			continue
		}
		updates = append(updates, a.closeLocked(key, entry, entry.state.UpdatedAt.Add(a.idle)))
	}
	return updates
}

func (a *GiftComboAggregator) openKeysLocked() []string {
	keys := make([]string, 0, len(a.entries))
	for key, entry := range a.entries {
		if entry.state.Status == ComboOpen {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func (a *GiftComboAggregator) closeLocked(key string, entry *giftComboEntry, closedAt time.Time) GiftComboUpdate {
	if closedAt.Before(entry.state.UpdatedAt) {
		closedAt = entry.state.UpdatedAt
	}
	entry.state.Status = ComboClosed
	entry.state.ClosedAt = timePointer(closedAt)
	if entry.state.AggregateEventID == "" {
		entry.state.AggregateEventID = newEventID()
	}
	aggregate := buildGiftAggregate(entry, closedAt)
	delete(a.entries, key)
	a.closed[key] = closedAt
	return GiftComboUpdate{State: entry.state, Aggregate: &aggregate}
}

func (a *GiftComboAggregator) pruneClosedLocked(now time.Time) {
	if a.tombstoneTTL <= 0 || now.IsZero() {
		return
	}
	cutoff := now.Add(-a.tombstoneTTL)
	for key, closedAt := range a.closed {
		if closedAt.Before(cutoff) {
			delete(a.closed, key)
		}
	}
}

func (a *GiftComboAggregator) evictOldestClosedLocked() {
	var oldestKey string
	var oldestAt time.Time
	for key, closedAt := range a.closed {
		if oldestKey == "" || closedAt.Before(oldestAt) ||
			(closedAt.Equal(oldestAt) && key < oldestKey) {
			oldestKey = key
			oldestAt = closedAt
		}
	}
	if oldestKey != "" {
		delete(a.closed, oldestKey)
	}
}

func buildGiftAggregate(entry *giftComboEntry, closedAt time.Time) Event {
	valueKind := "count"
	numeric := float64(entry.state.TotalCount)
	if entry.state.TotalValue != nil {
		valueKind = "diamond"
		numeric = *entry.state.TotalValue
	}
	allowlist, _ := json.Marshal(struct {
		GiftID     string `json:"gift_id,omitempty"`
		GiftName   string `json:"gift_name,omitempty"`
		TotalCount int64  `json:"total_count"`
		ValueKind  string `json:"value_kind"`
	}{
		GiftID:     entry.state.GiftID,
		GiftName:   entry.state.GiftName,
		TotalCount: entry.state.TotalCount,
		ValueKind:  valueKind,
	})
	source := entry.lastSource
	return Event{
		ID:                entry.state.AggregateEventID,
		SessionID:         entry.state.SessionID,
		IngestSequence:    entry.state.LastSequence,
		Role:              EventRoleAggregate,
		Method:            source.Method,
		Kind:              EventGift,
		DedupeKey:         "gift-combo:" + entry.state.ComboKey,
		ReceivedAt:        closedAt,
		SessionOffsetMS:   source.SessionOffsetMS,
		ClockConfidence:   source.ClockConfidence,
		UserHash:          entry.state.UserHash,
		DisplayName:       source.DisplayName,
		Content:           entry.state.GiftName,
		NumericValue:      floatPointer(numeric),
		NormalizedJSON:    string(allowlist),
		ParseStatus:       ParseParsed,
		NormalizerVersion: entry.state.NormalizerVersion,
	}
}

func aggregateGiftValue(totalCount, unitDiamond int64) *float64 {
	if totalCount <= 0 || unitDiamond <= 0 {
		return nil
	}
	value := float64(totalCount) * float64(unitDiamond)
	return &value
}

func newEventID() string {
	id, err := uuid.NewV7()
	if err == nil {
		return id.String()
	}
	return uuid.NewString()
}

func timePointer(value time.Time) *time.Time {
	copy := value
	return &copy
}

func floatPointer(value float64) *float64 {
	copy := value
	return &copy
}
