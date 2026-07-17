package eventstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

const (
	DefaultNormalQueueItems    = 4096
	DefaultNormalQueueBytes    = 32 << 20
	DefaultEmergencyQueueItems = 512
	DefaultEmergencyQueueBytes = 8 << 20
	DefaultQueueTotalBytes     = 128 << 20
	DefaultMaxPayloadBytes     = 4 << 20
)

var (
	ErrQueueClosed       = errors.New("event queue closed")
	ErrQueueFull         = errors.New("event queue capacity exhausted")
	ErrPayloadTooLarge   = errors.New("event payload exceeds limit")
	ErrInvalidQueueClass = errors.New("invalid event queue class")
)

type QueueClass uint8

const (
	QueueClassNormal QueueClass = iota
	QueueClassEmergency
)

type QueueLimits struct {
	NormalItems     int
	NormalBytes     int64
	EmergencyItems  int
	EmergencyBytes  int64
	TotalItems      int
	TotalBytes      int64
	MaxPayloadBytes int
}

func DefaultQueueLimits() QueueLimits {
	return QueueLimits{
		NormalItems:     DefaultNormalQueueItems,
		NormalBytes:     DefaultNormalQueueBytes,
		EmergencyItems:  DefaultEmergencyQueueItems,
		EmergencyBytes:  DefaultEmergencyQueueBytes,
		TotalItems:      DefaultNormalQueueItems + DefaultEmergencyQueueItems,
		TotalBytes:      DefaultQueueTotalBytes,
		MaxPayloadBytes: DefaultMaxPayloadBytes,
	}
}

type QueuedEnvelope struct {
	Envelope IngestEnvelope
	Class    QueueClass
}

type QueueStats struct {
	Items          int
	Bytes          int64
	NormalItems    int
	NormalBytes    int64
	EmergencyItems int
	EmergencyBytes int64
	Closed         bool
}

type queueEntry struct {
	item   QueuedEnvelope
	charge int64
}

// EnvelopeQueue is a single FIFO ring. Emergency capacity is admission
// reserve, not a second lane, so mixed classes can never overtake each other.
type EnvelopeQueue struct {
	mu     sync.Mutex
	limits QueueLimits
	ring   []queueEntry
	head   int
	count  int
	stats  QueueStats
	closed bool
	change chan struct{}
}

func NewEnvelopeQueue(limits QueueLimits) (*EnvelopeQueue, error) {
	if limits.NormalItems <= 0 || limits.EmergencyItems <= 0 || limits.TotalItems <= 0 ||
		limits.NormalBytes <= 0 || limits.EmergencyBytes <= 0 || limits.TotalBytes <= 0 ||
		limits.MaxPayloadBytes <= 0 {
		return nil, fmt.Errorf("invalid queue limits")
	}
	if limits.TotalItems < limits.NormalItems || limits.TotalItems < limits.EmergencyItems {
		return nil, fmt.Errorf("total item limit is smaller than a class limit")
	}
	q := &EnvelopeQueue{
		limits: limits,
		ring:   make([]queueEntry, limits.TotalItems),
		change: make(chan struct{}),
	}
	return q, nil
}

func NewDefaultEnvelopeQueue() *EnvelopeQueue {
	q, err := NewEnvelopeQueue(DefaultQueueLimits())
	if err != nil {
		panic(err)
	}
	return q
}

func EnvelopeChargeBytes(envelope IngestEnvelope) int64 {
	return int64(len(envelope.Payload)) +
		int64(len(envelope.SessionID)) +
		int64(len(envelope.EventID)) +
		int64(len(envelope.Method)) +
		int64(len(envelope.PlatformRoomID))
}

func cloneEnvelope(envelope IngestEnvelope) IngestEnvelope {
	cloned := envelope
	cloned.Payload = append([]byte(nil), envelope.Payload...)
	return cloned
}

func (q *EnvelopeQueue) TryPush(envelope IngestEnvelope, class QueueClass) error {
	charge, err := q.validate(envelope, class)
	if err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return ErrQueueClosed
	}
	if !q.canPushLocked(class, charge) {
		return ErrQueueFull
	}
	owned := cloneEnvelope(envelope)
	q.pushLocked(owned, class, charge)
	return nil
}

// Push waits for capacity. Context cancellation only stops this caller's wait;
// it never closes or mutates the shared queue.
func (q *EnvelopeQueue) Push(ctx context.Context, envelope IngestEnvelope, class QueueClass) error {
	owned, charge, err := q.prepare(envelope, class)
	if err != nil {
		return err
	}
	for {
		q.mu.Lock()
		if q.closed {
			q.mu.Unlock()
			return ErrQueueClosed
		}
		if q.canPushLocked(class, charge) {
			q.pushLocked(owned, class, charge)
			q.mu.Unlock()
			return nil
		}
		change := q.change
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-change:
		}
	}
}

func (q *EnvelopeQueue) Pop(ctx context.Context) (QueuedEnvelope, error) {
	for {
		q.mu.Lock()
		if q.count > 0 {
			entry := q.ring[q.head]
			q.ring[q.head] = queueEntry{}
			q.head = (q.head + 1) % len(q.ring)
			q.count--
			q.stats.Items--
			q.stats.Bytes -= entry.charge
			switch entry.item.Class {
			case QueueClassNormal:
				q.stats.NormalItems--
				q.stats.NormalBytes -= entry.charge
			case QueueClassEmergency:
				q.stats.EmergencyItems--
				q.stats.EmergencyBytes -= entry.charge
			}
			q.signalLocked()
			q.mu.Unlock()
			return entry.item, nil
		}
		if q.closed {
			q.mu.Unlock()
			return QueuedEnvelope{}, ErrQueueClosed
		}
		change := q.change
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return QueuedEnvelope{}, ctx.Err()
		case <-change:
		}
	}
}

func (q *EnvelopeQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	q.stats.Closed = true
	q.signalLocked()
}

func (q *EnvelopeQueue) Stats() QueueStats {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.stats
}

func (q *EnvelopeQueue) prepare(envelope IngestEnvelope, class QueueClass) (IngestEnvelope, int64, error) {
	charge, err := q.validate(envelope, class)
	if err != nil {
		return IngestEnvelope{}, 0, err
	}
	return cloneEnvelope(envelope), charge, nil
}

func (q *EnvelopeQueue) validate(envelope IngestEnvelope, class QueueClass) (int64, error) {
	if class != QueueClassNormal && class != QueueClassEmergency {
		return 0, ErrInvalidQueueClass
	}
	if len(envelope.Payload) > q.limits.MaxPayloadBytes {
		return 0, ErrPayloadTooLarge
	}
	return EnvelopeChargeBytes(envelope), nil
}

func (q *EnvelopeQueue) canPushLocked(class QueueClass, charge int64) bool {
	if q.stats.Items+1 > q.limits.TotalItems || q.stats.Bytes+charge > q.limits.TotalBytes {
		return false
	}
	switch class {
	case QueueClassNormal:
		return q.stats.NormalItems+1 <= q.limits.NormalItems && q.stats.NormalBytes+charge <= q.limits.NormalBytes
	case QueueClassEmergency:
		return q.stats.EmergencyItems+1 <= q.limits.EmergencyItems && q.stats.EmergencyBytes+charge <= q.limits.EmergencyBytes
	default:
		return false
	}
}

func (q *EnvelopeQueue) pushLocked(envelope IngestEnvelope, class QueueClass, charge int64) {
	tail := (q.head + q.count) % len(q.ring)
	q.ring[tail] = queueEntry{item: QueuedEnvelope{Envelope: envelope, Class: class}, charge: charge}
	q.count++
	q.stats.Items++
	q.stats.Bytes += charge
	if class == QueueClassNormal {
		q.stats.NormalItems++
		q.stats.NormalBytes += charge
	} else {
		q.stats.EmergencyItems++
		q.stats.EmergencyBytes += charge
	}
	q.signalLocked()
}

func (q *EnvelopeQueue) signalLocked() {
	close(q.change)
	q.change = make(chan struct{})
}
