//go:build p3accacceptance

package eventstore

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestManagerAggregateQueueStatsIsCompleteAndPrivacySafe(t *testing.T) {
	normal := NewDefaultEnvelopeQueue()
	emergency := NewDefaultEnvelopeQueue()
	first := IngestEnvelope{
		SessionID: "private-session-one", EventID: "private-event-one",
		Method: "WebcastChatMessage", PlatformRoomID: "private-room-one",
		Payload: []byte("private-payload-one"),
	}
	second := IngestEnvelope{
		SessionID: "private-session-two", EventID: "private-event-two",
		Method: "WebcastGiftMessage", PlatformRoomID: "private-room-two",
		Payload: []byte("private-payload-two"),
	}
	if err := normal.TryPush(first, QueueClassNormal); err != nil {
		t.Fatalf("push normal envelope: %v", err)
	}
	if err := emergency.TryPush(second, QueueClassEmergency); err != nil {
		t.Fatalf("push emergency envelope: %v", err)
	}
	emergency.Close()
	manager := queueStatsTestManager(map[string]*EnvelopeQueue{
		first.SessionID: normal, second.SessionID: emergency,
	})

	stats := manager.AggregateQueueStats()
	wantBytes := EnvelopeChargeBytes(first) + EnvelopeChargeBytes(second)
	defaultLimits := DefaultQueueLimits()
	if !stats.Complete || stats.QueueCount != 2 || stats.ClosedQueueCount != 1 ||
		stats.ItemCapacity != int64(2*defaultLimits.TotalItems) ||
		stats.ByteCapacity != 2*(defaultLimits.NormalBytes+defaultLimits.EmergencyBytes) ||
		stats.Items != 2 || stats.Bytes != wantBytes || stats.NormalItems != 1 ||
		stats.EmergencyItems != 1 || stats.NormalBytes != EnvelopeChargeBytes(first) ||
		stats.EmergencyBytes != EnvelopeChargeBytes(second) {
		t.Fatalf("unexpected aggregate: %#v", stats)
	}
	payload, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("marshal aggregate: %v", err)
	}
	for _, secret := range []string{
		first.SessionID, first.EventID, first.PlatformRoomID, string(first.Payload),
		second.SessionID, second.EventID, second.PlatformRoomID, string(second.Payload),
	} {
		if strings.Contains(string(payload), secret) {
			t.Fatalf("aggregate leaked %q: %s", secret, payload)
		}
	}
}

func TestManagerAggregateQueueStatsUsesEffectiveClassCapacity(t *testing.T) {
	limits := QueueLimits{
		NormalItems: 3, NormalBytes: 11, EmergencyItems: 2, EmergencyBytes: 7,
		TotalItems: 9, TotalBytes: 99, MaxPayloadBytes: 4,
	}
	queue, err := NewEnvelopeQueue(limits)
	if err != nil {
		t.Fatalf("create queue: %v", err)
	}
	stats := queueStatsTestManager(map[string]*EnvelopeQueue{"session": queue}).AggregateQueueStats()
	if !stats.Complete || stats.ItemCapacity != 5 || stats.ByteCapacity != 18 {
		t.Fatalf("effective class capacity = %#v", stats)
	}
	limits.TotalItems = 4
	limits.TotalBytes = 13
	queue, err = NewEnvelopeQueue(limits)
	if err != nil {
		t.Fatalf("create total-bounded queue: %v", err)
	}
	stats = queueStatsTestManager(map[string]*EnvelopeQueue{"session": queue}).AggregateQueueStats()
	if !stats.Complete || stats.ItemCapacity != 4 || stats.ByteCapacity != 13 {
		t.Fatalf("effective total capacity = %#v", stats)
	}
}

func TestManagerAggregateQueueStatsFailsClosedOnInvalidSource(t *testing.T) {
	queue := NewDefaultEnvelopeQueue()
	queue.mu.Lock()
	queue.stats.Items = 1
	queue.stats.Bytes = -1
	queue.mu.Unlock()
	manager := queueStatsTestManager(map[string]*EnvelopeQueue{"session": queue})
	if stats := manager.AggregateQueueStats(); stats.Complete || stats.QueueCount != 0 || stats.Items != 0 {
		t.Fatalf("invalid queue state was exposed as complete: %#v", stats)
	}
	if stats := (*Manager)(nil).AggregateQueueStats(); stats.Complete {
		t.Fatalf("nil manager returned a complete aggregate: %#v", stats)
	}
	queue = NewDefaultEnvelopeQueue()
	queue.mu.Lock()
	queue.limits.TotalItems = queue.stats.Items
	queue.mu.Unlock()
	manager = queueStatsTestManager(map[string]*EnvelopeQueue{"session": queue})
	if stats := manager.AggregateQueueStats(); stats.Complete || stats.ItemCapacity != 0 {
		t.Fatalf("invalid real queue capacity was exposed as complete: %#v", stats)
	}
}

func TestManagerAggregateQueueStatsRejectsSessionSetABAAndGenerationSaturation(t *testing.T) {
	queue := NewDefaultEnvelopeQueue()
	manager := queueStatsTestManager(map[string]*EnvelopeQueue{"session": queue})
	manager.mu.Lock()
	generation := manager.sessionSetGeneration
	snapshot := map[string]*EnvelopeQueue{"session": queue}
	manager.noteSessionSetChangeLocked()
	manager.noteSessionSetChangeLocked()
	manager.mu.Unlock()
	if stats := manager.aggregateQueueStatsSnapshot(snapshot, generation); stats.Complete {
		t.Fatalf("session-set ABA was accepted: %#v", stats)
	}

	manager.mu.Lock()
	manager.sessionSetGeneration = ^uint64(0)
	manager.noteSessionSetChangeLocked()
	manager.mu.Unlock()
	if stats := manager.AggregateQueueStats(); stats.Complete {
		t.Fatalf("saturated generation was accepted: %#v", stats)
	}
}

func TestManagerAggregateQueueStatsDoesNotDeadlockWithCloseAndRemoval(t *testing.T) {
	queue := NewDefaultEnvelopeQueue()
	manager := &Manager{sessions: make(map[string]*SessionSink)}
	sink := &SessionSink{manager: manager, descriptor: SessionDescriptor{SessionID: "session"}}
	runtime := &sessionRuntime{
		sink: sink, queue: queue, accepting: true,
		closeRequested: make(chan struct{}), done: make(chan struct{}),
	}
	sink.runtime = runtime
	manager.sessions[sink.descriptor.SessionID] = sink
	manager.noteSessionSetChangeLocked()

	start := make(chan struct{})
	done := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		<-start
		for index := 0; index < 1000; index++ {
			stats := manager.AggregateQueueStats()
			if stats.Items < 0 || stats.Bytes < 0 || stats.ClosedQueueCount < 0 {
				t.Errorf("invalid concurrent aggregate: %#v", stats)
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		<-start
		runtime.startClose()
		manager.sessionFinished(sink)
	}()
	close(start)
	go func() {
		workers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("aggregate queue stats deadlocked with close/removal")
	}
	stats := manager.AggregateQueueStats()
	if !stats.Complete || stats.QueueCount != 0 || stats.Items != 0 || stats.Bytes != 0 {
		t.Fatalf("post-removal aggregate = %#v", stats)
	}
}

func queueStatsTestManager(queues map[string]*EnvelopeQueue) *Manager {
	manager := &Manager{sessions: make(map[string]*SessionSink)}
	for sessionID, queue := range queues {
		sink := &SessionSink{manager: manager, descriptor: SessionDescriptor{SessionID: sessionID}}
		sink.runtime = &sessionRuntime{sink: sink, queue: queue}
		manager.sessions[sessionID] = sink
		manager.noteSessionSetChangeLocked()
	}
	return manager
}
