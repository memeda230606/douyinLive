package eventstore

import (
	"strings"
	"testing"
	"time"
)

func TestBuildDedupeKeyPriorityAndFallback(t *testing.T) {
	received := time.Unix(1_800_000_000, 0).UTC()
	platformTime := received.Add(-time.Second)
	envelope := IngestEnvelope{
		SessionID:      "session",
		Method:         "WebcastChatMessage",
		PlatformRoomID: "room-sensitive",
		ReceivedAt:     received,
		Payload:        []byte("payload-sensitive"),
	}
	platformKey := BuildDedupeKey(envelope, "message-sensitive", &platformTime)
	if !strings.HasPrefix(platformKey, "d1.p.") {
		t.Fatalf("platform key = %q", platformKey)
	}
	for _, secret := range []string{"room-sensitive", "message-sensitive", "payload-sensitive"} {
		if strings.Contains(platformKey, secret) {
			t.Fatalf("dedupe key leaked %q", secret)
		}
	}

	timeKey := BuildDedupeKey(envelope, "", &platformTime)
	if !strings.HasPrefix(timeKey, "d1.t.") {
		t.Fatalf("trusted-time key = %q", timeKey)
	}
	bucketKey := BuildDedupeKey(envelope, "", nil)
	if !strings.HasPrefix(bucketKey, "d1.b.") {
		t.Fatalf("bucket key = %q", bucketKey)
	}
	envelope.ReceivedAt = received.Add(29 * time.Second)
	if got := BuildDedupeKey(envelope, "", nil); got != bucketKey {
		t.Fatalf("same 30-second bucket changed key: %q != %q", got, bucketKey)
	}
	envelope.ReceivedAt = received.Add(31 * time.Second)
	if got := BuildDedupeKey(envelope, "", nil); got == bucketKey {
		t.Fatal("next 30-second bucket must change key")
	}
}

func TestDeduplicatorTTLAndCapacity(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	dedupe := NewDeduplicator(2*time.Minute, 2)
	if dedupe.SeenOrAdd("a", now) {
		t.Fatal("first observation marked duplicate")
	}
	if !dedupe.SeenOrAdd("a", now.Add(time.Minute)) {
		t.Fatal("unexpired observation not marked duplicate")
	}
	if dedupe.SeenOrAdd("a", now.Add(2*time.Minute)) {
		t.Fatal("expired observation marked duplicate")
	}

	if dedupe.SeenOrAdd("b", now.Add(2*time.Minute)) || dedupe.SeenOrAdd("c", now.Add(2*time.Minute)) {
		t.Fatal("new keys marked duplicate")
	}
	if dedupe.Len() != 2 {
		t.Fatalf("len = %d, want 2", dedupe.Len())
	}
	if dedupe.SeenOrAdd("a", now.Add(2*time.Minute+time.Second)) {
		t.Fatal("oldest key should have been evicted by capacity")
	}
	if dedupe.Len() != 2 {
		t.Fatalf("len after eviction = %d, want 2", dedupe.Len())
	}
}

func TestDeduplicatorDefaultBounds(t *testing.T) {
	dedupe := NewDeduplicator(0, 0)
	if dedupe.ttl != DefaultDedupeTTL || dedupe.capacity != DefaultDedupeCapacity {
		t.Fatalf("defaults = (%v,%d)", dedupe.ttl, dedupe.capacity)
	}
}

func TestDeduplicatorCommittedAPISeparatesLookupFromMutation(t *testing.T) {
	now := time.Unix(1_900_000_000, 0).UTC()
	dedupe := NewDeduplicator(2*time.Minute, 2)
	if dedupe.Seen("committed", now) || dedupe.Len() != 0 {
		t.Fatal("lookup inserted an uncommitted key")
	}
	dedupe.AddCommitted("committed", now)
	if !dedupe.Seen("committed", now.Add(time.Minute)) || dedupe.Len() != 1 {
		t.Fatal("committed key was not visible")
	}
	dedupe.AddCommitted("committed", now.Add(time.Minute))
	if dedupe.Seen("committed", now.Add(2*time.Minute)) {
		t.Fatal("re-adding a hot committed key extended its TTL")
	}
	dedupe.AddCommitted("second", now.Add(2*time.Minute))
	dedupe.AddCommitted("third", now.Add(2*time.Minute))
	dedupe.AddCommitted("fourth", now.Add(2*time.Minute))
	if dedupe.Len() != 2 {
		t.Fatalf("committed cache exceeded capacity: %d", dedupe.Len())
	}
}
