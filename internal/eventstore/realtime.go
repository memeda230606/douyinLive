package eventstore

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	LiveEventEventName = "live:event"

	liveEventBatchMaximum          = 100
	liveEventBatchWindow           = 100 * time.Millisecond
	liveEventInputCapacity         = 2000
	liveEventPublishQueueCapacity  = liveEventInputCapacity / liveEventBatchMaximum
	liveEventPublisherShutdownWait = 100 * time.Millisecond
)

// LiveEventDTO is the complete Wails-facing event allowlist. It deliberately
// excludes platform identifiers, user hashes, methods, dedupe/raw references,
// normalized JSON, protobuf payloads, filesystem paths, and stream URLs.
type LiveEventDTO struct {
	ID              string      `json:"id"`
	IngestSequence  int64       `json:"ingestSequence"`
	Role            EventRole   `json:"role"`
	Kind            EventKind   `json:"kind"`
	ReceivedAt      int64       `json:"receivedAt"`
	SessionOffsetMS int64       `json:"sessionOffsetMs"`
	DisplayName     string      `json:"displayName,omitempty"`
	Content         string      `json:"content,omitempty"`
	NumericValue    *float64    `json:"numericValue,omitempty"`
	ParseStatus     ParseStatus `json:"parseStatus"`
}

type LiveEventBatchDTO struct {
	SessionID string         `json:"sessionId"`
	EmittedAt int64          `json:"emittedAt"`
	Events    []LiveEventDTO `json:"events"`
}

type LiveEventPublisher func(LiveEventBatchDTO)

type liveEventDispatchItem struct {
	sessionID string
	queuedAt  time.Time
	event     LiveEventDTO
}

type pendingLiveEventBatch struct {
	firstAt time.Time
	events  []LiveEventDTO
}

// liveEventDispatcher isolates best-effort UI delivery from the durable
// event pipeline. The single publisher worker ensures that even a permanently
// blocking callback can strand at most one callback goroutine.
type liveEventDispatcher struct {
	publisher LiveEventPublisher
	now       func() time.Time
	input     chan liveEventDispatchItem
	stop      chan struct{}
	done      chan struct{}
	publish   chan LiveEventBatchDTO
	published chan struct{}
	stopOnce  sync.Once
	closed    atomic.Bool
}

func newLiveEventDispatcher(
	publisher LiveEventPublisher,
	now func() time.Time,
) *liveEventDispatcher {
	if publisher == nil {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	dispatcher := &liveEventDispatcher{
		publisher: publisher,
		now:       now,
		input:     make(chan liveEventDispatchItem, liveEventInputCapacity),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		publish:   make(chan LiveEventBatchDTO, liveEventPublishQueueCapacity),
		published: make(chan struct{}),
	}
	go dispatcher.runPublisher()
	go dispatcher.run()
	return dispatcher
}

func (m *Manager) publishLiveEventSources(sessionID string, events []Event) {
	if m == nil || m.liveEvents == nil || sessionID == "" || len(events) == 0 {
		return
	}
	m.liveEvents.enqueueSources(sessionID, events)
}

func (d *liveEventDispatcher) enqueueSources(sessionID string, events []Event) {
	if d == nil || sessionID == "" {
		return
	}
	queuedAt := time.Now()
	for _, event := range events {
		if event.Role != EventRoleSource || event.SessionID != sessionID {
			continue
		}
		item := liveEventDispatchItem{
			sessionID: sessionID,
			queuedAt:  queuedAt,
			event:     liveEventDTO(event),
		}
		select {
		case <-d.stop:
			return
		default:
		}
		select {
		case d.input <- item:
		case <-d.stop:
			return
		default:
			// UI copies are intentionally lossy. Durable storage and recovery do
			// not observe or react to this queue overflow.
		}
	}
}

func liveEventDTO(event Event) LiveEventDTO {
	var numericValue *float64
	if event.NumericValue != nil {
		value := *event.NumericValue
		numericValue = &value
	}
	return LiveEventDTO{
		ID:              strings.Clone(event.ID),
		IngestSequence:  event.IngestSequence,
		Role:            event.Role,
		Kind:            event.Kind,
		ReceivedAt:      event.ReceivedAt.UTC().UnixMilli(),
		SessionOffsetMS: event.SessionOffsetMS,
		DisplayName:     strings.Clone(event.DisplayName),
		Content:         strings.Clone(event.Content),
		NumericValue:    numericValue,
		ParseStatus:     event.ParseStatus,
	}
}

func (d *liveEventDispatcher) run() {
	defer close(d.done)
	defer close(d.publish)

	pending := make(map[string]*pendingLiveEventBatch)
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for {
		deadline, hasDeadline := nextLiveEventDeadline(pending)
		var timerC <-chan time.Time
		if hasDeadline {
			delay := time.Until(deadline)
			if delay < 0 {
				delay = 0
			}
			resetLiveEventTimer(timer, delay)
			timerC = timer.C
		}

		select {
		case item := <-d.input:
			d.add(pending, item)
		case now := <-timerC:
			d.flushDue(pending, now)
		case <-d.stop:
			d.drainInput(pending)
			d.flushAll(pending)
			return
		}
	}
}

func (d *liveEventDispatcher) add(
	pending map[string]*pendingLiveEventBatch,
	item liveEventDispatchItem,
) {
	batch := pending[item.sessionID]
	if batch == nil {
		batch = &pendingLiveEventBatch{firstAt: item.queuedAt}
		pending[item.sessionID] = batch
	}
	batch.events = append(batch.events, item.event)
	if len(batch.events) < liveEventBatchMaximum {
		return
	}
	d.offer(item.sessionID, batch.events[:liveEventBatchMaximum])
	batch.events = batch.events[liveEventBatchMaximum:]
	if len(batch.events) == 0 {
		delete(pending, item.sessionID)
		return
	}
	batch.firstAt = item.queuedAt
}

func (d *liveEventDispatcher) flushDue(
	pending map[string]*pendingLiveEventBatch,
	now time.Time,
) {
	for sessionID, batch := range pending {
		if now.Before(batch.firstAt.Add(liveEventBatchWindow)) {
			continue
		}
		d.offer(sessionID, batch.events)
		delete(pending, sessionID)
	}
}

func (d *liveEventDispatcher) drainInput(pending map[string]*pendingLiveEventBatch) {
	for {
		select {
		case item := <-d.input:
			d.add(pending, item)
		default:
			return
		}
	}
}

func (d *liveEventDispatcher) flushAll(pending map[string]*pendingLiveEventBatch) {
	for sessionID, batch := range pending {
		d.offer(sessionID, batch.events)
		delete(pending, sessionID)
	}
}

func (d *liveEventDispatcher) offer(sessionID string, events []LiveEventDTO) {
	if len(events) == 0 {
		return
	}
	copyEvents := append([]LiveEventDTO(nil), events...)
	batch := LiveEventBatchDTO{
		SessionID: sessionID,
		EmittedAt: d.now().UTC().UnixMilli(),
		Events:    copyEvents,
	}
	select {
	case d.publish <- batch:
	default:
		// A slow UI publisher cannot apply backpressure to the dispatcher.
	}
}

func (d *liveEventDispatcher) runPublisher() {
	defer close(d.published)
	for batch := range d.publish {
		if d.closed.Load() {
			return
		}
		func() {
			defer func() { _ = recover() }()
			d.publisher(batch)
		}()
	}
}

func (d *liveEventDispatcher) shutdown() {
	if d == nil {
		return
	}
	d.stopOnce.Do(func() { close(d.stop) })
	<-d.done
	timer := time.NewTimer(liveEventPublisherShutdownWait)
	defer timer.Stop()
	select {
	case <-d.published:
	case <-timer.C:
		d.closed.Store(true)
	}
}

func nextLiveEventDeadline(
	pending map[string]*pendingLiveEventBatch,
) (time.Time, bool) {
	var earliest time.Time
	for _, batch := range pending {
		deadline := batch.firstAt.Add(liveEventBatchWindow)
		if earliest.IsZero() || deadline.Before(earliest) {
			earliest = deadline
		}
	}
	return earliest, !earliest.IsZero()
}

func resetLiveEventTimer(timer *time.Timer, delay time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(delay)
}
