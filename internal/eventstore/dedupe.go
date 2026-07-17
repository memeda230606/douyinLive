package eventstore

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultDedupeCapacity = 65536
	DefaultDedupeTTL      = 2 * time.Minute
	dedupeFallbackBucket  = 30 * time.Second
)

// BuildDedupeKey uses the platform message ID when present. Otherwise it binds
// a payload digest to a trusted platform time, or to a coarse receive-time
// bucket when no platform clock is trustworthy. Raw identifiers and payloads
// never appear in the returned key.
func BuildDedupeKey(envelope IngestEnvelope, platformMessageID string, messageCreateAt *time.Time) string {
	payloadDigest := sha256.Sum256(envelope.Payload)
	parts := []string{
		envelope.PlatformRoomID,
		envelope.Method,
	}
	prefix := "d1.b."
	if platformMessageID = strings.TrimSpace(platformMessageID); platformMessageID != "" {
		prefix = "d1.p."
		parts = append(parts, platformMessageID)
	} else if messageCreateAt != nil && !messageCreateAt.IsZero() {
		prefix = "d1.t."
		parts = append(parts, strconv.FormatInt(messageCreateAt.UTC().UnixMilli(), 10), hex.EncodeToString(payloadDigest[:]))
	} else {
		bucket := int64(0)
		if !envelope.ReceivedAt.IsZero() {
			bucket = envelope.ReceivedAt.UTC().UnixNano() / int64(dedupeFallbackBucket)
		}
		parts = append(parts, strconv.FormatInt(bucket, 10), hex.EncodeToString(payloadDigest[:]))
	}
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return prefix + hex.EncodeToString(digest[:])
}

type dedupeEntry struct {
	key       string
	expiresAt time.Time
}

// Deduplicator is a bounded, concurrency-safe in-memory first line of defense.
// SQLite's unique constraint remains the final replay/idempotency boundary.
type Deduplicator struct {
	mu       sync.Mutex
	ttl      time.Duration
	capacity int
	entries  map[string]*list.Element
	order    *list.List
}

func NewDeduplicator(ttl time.Duration, capacity int) *Deduplicator {
	if ttl <= 0 {
		ttl = DefaultDedupeTTL
	}
	if capacity <= 0 {
		capacity = DefaultDedupeCapacity
	}
	return &Deduplicator{
		ttl:      ttl,
		capacity: capacity,
		entries:  make(map[string]*list.Element, capacity),
		order:    list.New(),
	}
}

// Seen reports whether a key was already committed within the short window.
// It never inserts a new key, so a failed transaction cannot poison recovery.
func (d *Deduplicator) Seen(key string, now time.Time) bool {
	if d == nil || key == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pruneLocked(now)
	element, ok := d.entries[key]
	return ok && now.Before(element.Value.(dedupeEntry).expiresAt)
}

// AddCommitted records a key only after its SQLite transaction commits.
// Re-adding an unexpired key does not extend TTL, so a hot key cannot remain
// resident forever.
func (d *Deduplicator) AddCommitted(key string, now time.Time) {
	if d == nil || key == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pruneLocked(now)
	if element, ok := d.entries[key]; ok {
		if now.Before(element.Value.(dedupeEntry).expiresAt) {
			return
		}
		d.removeLocked(element)
	}
	d.addLocked(key, now)
}

// SeenOrAdd remains useful for standalone callers. Persistence code uses the
// split Seen/AddCommitted API so only committed keys enter the cache.
func (d *Deduplicator) SeenOrAdd(key string, now time.Time) bool {
	if d == nil || key == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pruneLocked(now)
	if element, ok := d.entries[key]; ok &&
		now.Before(element.Value.(dedupeEntry).expiresAt) {
		return true
	}
	d.addLocked(key, now)
	return false
}

func addCommittedDedupeKeys(dedupe *Deduplicator, keys []string, now time.Time) {
	if dedupe == nil {
		return
	}
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		dedupe.AddCommitted(key, now)
	}
}

func (d *Deduplicator) Prune(now time.Time) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.pruneLocked(now)
	d.mu.Unlock()
}

func (d *Deduplicator) Len() int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.entries)
}

func (d *Deduplicator) pruneLocked(now time.Time) {
	for element := d.order.Front(); element != nil; element = d.order.Front() {
		entry := element.Value.(dedupeEntry)
		if now.Before(entry.expiresAt) {
			return
		}
		d.removeLocked(element)
	}
}

func (d *Deduplicator) addLocked(key string, now time.Time) {
	element := d.order.PushBack(dedupeEntry{key: key, expiresAt: now.Add(d.ttl)})
	d.entries[key] = element
	for d.order.Len() > d.capacity {
		d.removeLocked(d.order.Front())
	}
}

func (d *Deduplicator) removeLocked(element *list.Element) {
	if element == nil {
		return
	}
	entry := element.Value.(dedupeEntry)
	delete(d.entries, entry.key)
	d.order.Remove(element)
}
