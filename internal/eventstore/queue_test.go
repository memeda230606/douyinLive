package eventstore

import (
	"context"
	"errors"
	"testing"
	"time"
)

func testQueue(t *testing.T, limits QueueLimits) *EnvelopeQueue {
	t.Helper()
	q, err := NewEnvelopeQueue(limits)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestEnvelopeQueueMixedClassesRemainStrictFIFO(t *testing.T) {
	q := testQueue(t, QueueLimits{NormalItems: 3, NormalBytes: 128, EmergencyItems: 2, EmergencyBytes: 128, TotalItems: 5, TotalBytes: 256, MaxPayloadBytes: 32})
	classes := []QueueClass{QueueClassNormal, QueueClassEmergency, QueueClassNormal, QueueClassEmergency}
	for i, class := range classes {
		if err := q.TryPush(IngestEnvelope{Sequence: int64(i + 1), Payload: []byte{byte(i)}}, class); err != nil {
			t.Fatal(err)
		}
	}
	for i, class := range classes {
		item, err := q.Pop(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if item.Envelope.Sequence != int64(i+1) || item.Class != class {
			t.Fatalf("item %d = sequence %d class %d", i, item.Envelope.Sequence, item.Class)
		}
	}
}

func TestEnvelopeQueueReservesEmergencyCapacityAndEnforcesDualLimits(t *testing.T) {
	q := testQueue(t, QueueLimits{NormalItems: 1, NormalBytes: 4, EmergencyItems: 1, EmergencyBytes: 3, TotalItems: 2, TotalBytes: 7, MaxPayloadBytes: 4})
	if err := q.TryPush(IngestEnvelope{Payload: []byte("1234")}, QueueClassNormal); err != nil {
		t.Fatal(err)
	}
	if err := q.TryPush(IngestEnvelope{Payload: []byte("x")}, QueueClassNormal); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("normal overflow = %v", err)
	}
	if err := q.TryPush(IngestEnvelope{Payload: []byte("123")}, QueueClassEmergency); err != nil {
		t.Fatalf("emergency reserve unavailable: %v", err)
	}
	stats := q.Stats()
	if stats.Items != 2 || stats.Bytes != 7 || stats.NormalBytes != 4 || stats.EmergencyBytes != 3 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if err := q.TryPush(IngestEnvelope{}, QueueClassEmergency); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("item overflow = %v", err)
	}
}

func TestEnvelopeQueuePayloadLimitAndDeepCopy(t *testing.T) {
	q := testQueue(t, QueueLimits{NormalItems: 2, NormalBytes: 32, EmergencyItems: 1, EmergencyBytes: 8, TotalItems: 3, TotalBytes: 40, MaxPayloadBytes: 4})
	if err := q.TryPush(IngestEnvelope{Payload: make([]byte, 5)}, QueueClassNormal); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("payload limit = %v", err)
	}
	payload := []byte("copy")
	if err := q.TryPush(IngestEnvelope{Payload: payload}, QueueClassNormal); err != nil {
		t.Fatal(err)
	}
	payload[0] = 'X'
	item, err := q.Pop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(item.Envelope.Payload) != "copy" {
		t.Fatalf("payload was not copied: %q", item.Envelope.Payload)
	}
}

func TestEnvelopeQueueBlockingPushAndCallerCancellation(t *testing.T) {
	q := testQueue(t, QueueLimits{NormalItems: 1, NormalBytes: 8, EmergencyItems: 1, EmergencyBytes: 8, TotalItems: 2, TotalBytes: 16, MaxPayloadBytes: 8})
	if err := q.TryPush(IngestEnvelope{Sequence: 1, Payload: []byte("a")}, QueueClassNormal); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := q.Push(ctx, IngestEnvelope{Sequence: 2}, QueueClassNormal); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled push = %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- q.Push(context.Background(), IngestEnvelope{Sequence: 2}, QueueClassNormal) }()
	if _, err := q.Pop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("push did not wake after capacity became available")
	}
}

func TestEnvelopeQueueCloseDrainsAcceptedAndWakesWaiters(t *testing.T) {
	q := testQueue(t, QueueLimits{NormalItems: 1, NormalBytes: 8, EmergencyItems: 1, EmergencyBytes: 8, TotalItems: 2, TotalBytes: 16, MaxPayloadBytes: 8})
	if err := q.TryPush(IngestEnvelope{Sequence: 1}, QueueClassNormal); err != nil {
		t.Fatal(err)
	}
	q.Close()
	q.Close()
	if err := q.TryPush(IngestEnvelope{Sequence: 2}, QueueClassEmergency); !errors.Is(err, ErrQueueClosed) {
		t.Fatalf("late push = %v", err)
	}
	item, err := q.Pop(context.Background())
	if err != nil || item.Envelope.Sequence != 1 {
		t.Fatalf("drain = %+v, %v", item, err)
	}
	if _, err := q.Pop(context.Background()); !errors.Is(err, ErrQueueClosed) {
		t.Fatalf("empty closed pop = %v", err)
	}
	stats := q.Stats()
	if !stats.Closed || stats.Items != 0 {
		t.Fatalf("unexpected final stats: %+v", stats)
	}
}

func TestEnvelopeQueueCloseWakesEmptyPop(t *testing.T) {
	q := NewDefaultEnvelopeQueue()
	done := make(chan error, 1)
	go func() {
		_, err := q.Pop(context.Background())
		done <- err
	}()
	q.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrQueueClosed) {
			t.Fatalf("pop = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pop did not wake on close")
	}
}

func TestEnvelopeQueueTryPushFullDoesNotCloneRejectedPayload(t *testing.T) {
	q := testQueue(t, QueueLimits{
		NormalItems: 1, NormalBytes: 2048,
		EmergencyItems: 1, EmergencyBytes: 2048,
		TotalItems: 2, TotalBytes: 4096, MaxPayloadBytes: 2048,
	})
	if err := q.TryPush(IngestEnvelope{Payload: []byte("accepted")}, QueueClassNormal); err != nil {
		t.Fatal(err)
	}
	rejected := IngestEnvelope{
		SessionID: "session", EventID: "event", Method: "method",
		PlatformRoomID: "room", Payload: make([]byte, 1024),
	}
	var pushErr error
	allocations := testing.AllocsPerRun(1000, func() {
		pushErr = q.TryPush(rejected, QueueClassNormal)
	})
	if !errors.Is(pushErr, ErrQueueFull) {
		t.Fatalf("TryPush() error = %v", pushErr)
	}
	if allocations != 0 {
		t.Fatalf("full TryPush allocations = %f, want 0", allocations)
	}
}
