package eventstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jwwsjlm/douyinlive-proto/generated/new_douyin"
)

func TestManagerLiveEventPublisherRunsOnlyAfterDurableCommit(t *testing.T) {
	published := make(chan LiveEventBatchDTO, 1)
	fixture := newManagerFixture(t, func(options *ManagerOptions) {
		options.BatchInterval = 5 * time.Millisecond
		options.LiveEventPublisher = func(batch LiveEventBatchDTO) {
			published <- batch
		}
	})
	sink, err := fixture.manager.OpenSession(context.Background(), fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	payload := managerProtoPayload(t, &new_douyin.Webcast_Im_ChatMessage{
		Common: &new_douyin.Webcast_Im_Common{MsgId: 701},
		User: &new_douyin.Webcast_Data_User{
			WebcastUid: "durable-user",
			Nickname:   "durable-name",
		},
		Content: "durable-content",
	})
	sink.Accept(managerLiveMessage(methodChat, payload, fixture.now.Add(time.Second)))

	var batch LiveEventBatchDTO
	select {
	case batch = <-published:
	case <-time.After(time.Second):
		t.Fatal("live event was not published")
	}
	var persisted int
	if err := fixture.store.Reader().QueryRow(`SELECT COUNT(*) FROM live_events
		WHERE session_id = ? AND event_role = 'source'`, fixture.sessionID).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted != 1 {
		t.Fatalf("persisted source events = %d, want 1 before publish", persisted)
	}
	if batch.SessionID != fixture.sessionID || len(batch.Events) != 1 ||
		batch.Events[0].ID == "" || batch.Events[0].IngestSequence != 1 ||
		batch.Events[0].Role != EventRoleSource || batch.Events[0].Kind != EventChat ||
		batch.Events[0].Content != "durable-content" {
		t.Fatalf("published batch = %+v", batch)
	}
	if err := sink.FlushAndClose(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestManagerLiveEventPublisherSkipsFailedCommitAndRecoveryReplay(t *testing.T) {
	published := make(chan LiveEventBatchDTO, 1)
	fixture := newManagerFixture(t, func(options *ManagerOptions) {
		options.BatchInterval = 5 * time.Millisecond
		options.BusyRetryWindow = 10 * time.Millisecond
		options.LiveEventPublisher = func(batch LiveEventBatchDTO) {
			published <- batch
		}
	})
	sink, err := fixture.manager.OpenSession(context.Background(), fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Writer().Exec(`CREATE TRIGGER block_live_event_publish
		BEFORE INSERT ON live_events BEGIN
			SELECT RAISE(ABORT, 'forced live event failure');
		END`); err != nil {
		t.Fatal(err)
	}
	sink.Accept(managerLiveMessage(methodChat, []byte{1}, fixture.now.Add(time.Second)))
	eventually(t, func() bool {
		sink.runtime.mu.Lock()
		degraded := sink.runtime.degraded
		sink.runtime.mu.Unlock()
		return degraded
	})
	assertNoLiveEventBatch(t, published, 2*liveEventBatchWindow)

	if _, err := fixture.store.Writer().Exec(`DROP TRIGGER block_live_event_publish`); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		checkpoint, found, err := fixture.writer.Checkpoint(context.Background(), fixture.sessionID)
		return err == nil && found && checkpoint.CommittedSequence == 1
	})
	assertNoLiveEventBatch(t, published, 2*liveEventBatchWindow)
	if err := sink.FlushAndClose(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestManagerLiveEventPublisherDedupesWithinSameDurableBatch(t *testing.T) {
	published := make(chan LiveEventBatchDTO, 2)
	fixture := newManagerFixture(t, func(options *ManagerOptions) {
		options.BatchSize = 2
		options.BatchInterval = 50 * time.Millisecond
		options.LiveEventPublisher = func(batch LiveEventBatchDTO) {
			published <- batch
		}
	})
	sink, err := fixture.manager.OpenSession(context.Background(), fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	payload := managerProtoPayload(t, &new_douyin.Webcast_Im_ChatMessage{
		Common:  &new_douyin.Webcast_Im_Common{MsgId: 702},
		Content: "same-batch-deduped",
	})
	for index := 1; index <= 2; index++ {
		sink.Accept(managerLiveMessage(
			methodChat,
			payload,
			fixture.now.Add(time.Duration(index)*time.Second),
		))
	}
	eventually(t, func() bool {
		checkpoint, found, checkpointErr := fixture.writer.Checkpoint(
			context.Background(), fixture.sessionID,
		)
		return checkpointErr == nil && found && checkpoint.CommittedSequence == 2
	})
	select {
	case batch := <-published:
		if batch.SessionID != fixture.sessionID || len(batch.Events) != 1 ||
			batch.Events[0].IngestSequence != 1 ||
			batch.Events[0].Content != "same-batch-deduped" {
			t.Fatalf("deduped same-batch publish = %+v", batch)
		}
	case <-time.After(time.Second):
		t.Fatal("new source was not published")
	}
	assertNoLiveEventBatch(t, published, 2*liveEventBatchWindow)
	var persisted int
	if err := fixture.store.Reader().QueryRow(`SELECT COUNT(*) FROM live_events
		WHERE session_id = ? AND event_role = 'source'`, fixture.sessionID).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted != 1 {
		t.Fatalf("persisted same-batch source events = %d, want 1", persisted)
	}
	if err := sink.FlushAndClose(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestManagerLiveEventPublisherDedupesCachedAndCommittedSourcesAcrossBatches(t *testing.T) {
	published := make(chan LiveEventBatchDTO, 3)
	var nowMillis atomic.Int64
	nowMillis.Store(time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC).UnixMilli())
	fixture := newManagerFixture(t, func(options *ManagerOptions) {
		options.BatchSize = 1
		options.BatchInterval = 5 * time.Millisecond
		options.Now = func() time.Time {
			return time.UnixMilli(nowMillis.Load()).UTC()
		}
		options.LiveEventPublisher = func(batch LiveEventBatchDTO) {
			published <- batch
		}
	})
	sink, err := fixture.manager.OpenSession(context.Background(), fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	payload := managerProtoPayload(t, &new_douyin.Webcast_Im_ChatMessage{
		Common:  &new_douyin.Webcast_Im_Common{MsgId: 703},
		Content: "cross-batch-deduped",
	})
	acceptAndWait := func(sequence int64) {
		t.Helper()
		sink.Accept(managerLiveMessage(
			methodChat,
			payload,
			fixture.now.Add(time.Duration(sequence)*time.Second),
		))
		eventually(t, func() bool {
			checkpoint, found, checkpointErr := fixture.writer.Checkpoint(
				context.Background(), fixture.sessionID,
			)
			return checkpointErr == nil && found &&
				checkpoint.CommittedSequence == sequence
		})
	}

	acceptAndWait(1)
	select {
	case batch := <-published:
		if len(batch.Events) != 1 || batch.Events[0].IngestSequence != 1 {
			t.Fatalf("first cross-batch publish = %+v", batch)
		}
	case <-time.After(time.Second):
		t.Fatal("first source was not published")
	}

	// The second copy is rejected by the committed in-memory cache.
	acceptAndWait(2)
	assertNoLiveEventBatch(t, published, 2*liveEventBatchWindow)

	// Expire that bounded cache. SQLite must still reject the already committed
	// non-gift source and prevent a second UI publication.
	nowMillis.Add((DefaultDedupeTTL + time.Millisecond).Milliseconds())
	acceptAndWait(3)
	assertNoLiveEventBatch(t, published, 2*liveEventBatchWindow)

	var persisted int
	if err := fixture.store.Reader().QueryRow(`SELECT COUNT(*) FROM live_events
		WHERE session_id = ? AND event_role = 'source'`, fixture.sessionID).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted != 1 {
		t.Fatalf("persisted cross-batch source events = %d, want 1", persisted)
	}
	if err := sink.FlushAndClose(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLiveEventDispatcherSplitsAtOneHundredAndFlushesRemainder(t *testing.T) {
	published := make(chan LiveEventBatchDTO, 4)
	dispatcher := newLiveEventDispatcher(func(batch LiveEventBatchDTO) {
		published <- batch
	}, time.Now)
	t.Cleanup(dispatcher.shutdown)

	events := make([]Event, 205)
	for index := range events {
		events[index] = realtimeSourceEvent("session-split", int64(index+1))
	}
	dispatcher.enqueueSources("session-split", events)

	wantSizes := []int{100, 100, 5}
	sequence := int64(1)
	for batchIndex, wantSize := range wantSizes {
		select {
		case batch := <-published:
			if batch.SessionID != "session-split" || len(batch.Events) != wantSize {
				t.Fatalf("batch %d = session %q size %d", batchIndex, batch.SessionID, len(batch.Events))
			}
			for _, event := range batch.Events {
				if event.IngestSequence != sequence {
					t.Fatalf("sequence = %d, want %d", event.IngestSequence, sequence)
				}
				sequence++
			}
		case <-time.After(time.Second):
			t.Fatalf("batch %d was not published", batchIndex)
		}
	}
}

func TestLiveEventDispatcherFlushesOneEventAfterWindow(t *testing.T) {
	published := make(chan LiveEventBatchDTO, 1)
	dispatcher := newLiveEventDispatcher(func(batch LiveEventBatchDTO) {
		published <- batch
	}, time.Now)
	t.Cleanup(dispatcher.shutdown)

	started := time.Now()
	dispatcher.enqueueSources("session-window", []Event{
		realtimeSourceEvent("session-window", 1),
	})
	select {
	case batch := <-published:
		elapsed := time.Since(started)
		if len(batch.Events) != 1 || elapsed < liveEventBatchWindow/2 || elapsed > 500*time.Millisecond {
			t.Fatalf("window batch size/elapsed = %d/%s", len(batch.Events), elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("window batch was not published")
	}
}

func TestLiveEventDispatcherNeverMixesSessions(t *testing.T) {
	published := make(chan LiveEventBatchDTO, 2)
	dispatcher := newLiveEventDispatcher(func(batch LiveEventBatchDTO) {
		published <- batch
	}, time.Now)
	t.Cleanup(dispatcher.shutdown)

	dispatcher.enqueueSources("session-a", []Event{realtimeSourceEvent("session-a", 1)})
	dispatcher.enqueueSources("session-b", []Event{realtimeSourceEvent("session-b", 1)})
	seen := map[string]bool{}
	for range 2 {
		select {
		case batch := <-published:
			if len(batch.Events) != 1 || seen[batch.SessionID] {
				t.Fatalf("mixed or duplicate batch = %+v", batch)
			}
			seen[batch.SessionID] = true
		case <-time.After(time.Second):
			t.Fatal("session batch was not published")
		}
	}
	if !seen["session-a"] || !seen["session-b"] {
		t.Fatalf("published sessions = %+v", seen)
	}
}

func TestLiveEventDispatcherContainsPublisherPanic(t *testing.T) {
	var calls atomic.Int32
	dispatcher := newLiveEventDispatcher(func(LiveEventBatchDTO) {
		calls.Add(1)
		panic("publisher panic")
	}, time.Now)
	t.Cleanup(dispatcher.shutdown)

	dispatcher.enqueueSources("session-panic", realtimeEventRange("session-panic", 1, 100))
	eventually(t, func() bool { return calls.Load() == 1 })
	dispatcher.enqueueSources("session-panic", realtimeEventRange("session-panic", 101, 200))
	eventually(t, func() bool { return calls.Load() == 2 })
}

func TestManagerPublisherPanicCannotDegradePersistence(t *testing.T) {
	var calls atomic.Int32
	fixture := newManagerFixture(t, func(options *ManagerOptions) {
		options.BatchInterval = 5 * time.Millisecond
		options.LiveEventPublisher = func(LiveEventBatchDTO) {
			calls.Add(1)
			panic("publisher panic")
		}
	})
	sink, err := fixture.manager.OpenSession(context.Background(), fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 2; index++ {
		payload := managerProtoPayload(t, &new_douyin.Webcast_Im_ChatMessage{
			Common:  &new_douyin.Webcast_Im_Common{MsgId: uint64(index)},
			Content: fmt.Sprintf("panic-safe-%d", index),
		})
		sink.Accept(managerLiveMessage(
			methodChat, payload, fixture.now.Add(time.Duration(index)*time.Second),
		))
	}
	if err := sink.FlushAndClose(context.Background()); err != nil {
		t.Fatalf("FlushAndClose() was affected by publisher panic: %v", err)
	}
	var persisted int
	if err := fixture.store.Reader().QueryRow(`SELECT COUNT(*) FROM live_events
		WHERE session_id = ? AND event_role = 'source'`, fixture.sessionID).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted != 2 {
		t.Fatalf("persisted events = %d, want 2", persisted)
	}
	eventually(t, func() bool { return calls.Load() > 0 })
	checkpoint, found, err := fixture.writer.Checkpoint(context.Background(), fixture.sessionID)
	if err != nil || !found || checkpoint.State != CheckpointClosed ||
		checkpoint.CommittedSequence != 2 {
		t.Fatalf("checkpoint after publisher panic = (%+v, %v, %v)", checkpoint, found, err)
	}
}

func TestManagerBlockedPublisherCannotBlockPersistenceOrShutdown(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	publisherReturned := make(chan struct{})
	var enterOnce sync.Once
	var returnOnce sync.Once
	var active atomic.Int32
	var maximum atomic.Int32
	var calls atomic.Int32
	fixture := newManagerFixture(t, func(options *ManagerOptions) {
		options.BatchSize = MaxEventBatchSize
		options.BatchInterval = 5 * time.Millisecond
		options.LiveEventPublisher = func(LiveEventBatchDTO) {
			calls.Add(1)
			current := active.Add(1)
			for {
				observed := maximum.Load()
				if current <= observed || maximum.CompareAndSwap(observed, current) {
					break
				}
			}
			enterOnce.Do(func() { close(entered) })
			<-release
			active.Add(-1)
			returnOnce.Do(func() { close(publisherReturned) })
		}
	})
	sink, err := fixture.manager.OpenSession(context.Background(), fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	const eventCount = 2201
	for index := 0; index < eventCount; index++ {
		payload := managerProtoPayload(t, &new_douyin.Webcast_Im_ChatMessage{
			Common:  &new_douyin.Webcast_Im_Common{MsgId: uint64(index + 1)},
			Content: "persisted while UI blocked",
		})
		sink.Accept(managerLiveMessage(
			methodChat,
			payload,
			fixture.now.Add(time.Duration(index+1)*time.Millisecond),
		))
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("publisher did not block")
	}
	if err := sink.FlushAndClose(context.Background()); err != nil {
		t.Fatalf("FlushAndClose() was affected by publisher: %v", err)
	}
	var persisted int
	if err := fixture.store.Reader().QueryRow(`SELECT COUNT(*) FROM live_events
		WHERE session_id = ? AND event_role = 'source'`, fixture.sessionID).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted != eventCount {
		t.Fatalf("persisted events = %d, want %d", persisted, eventCount)
	}
	eventually(t, func() bool {
		return len(fixture.manager.liveEvents.publish) == liveEventPublishQueueCapacity
	})

	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	shutdownErr := fixture.manager.Shutdown(ctx)
	cancel()
	if shutdownErr != nil || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("Shutdown() = %v after %s", shutdownErr, time.Since(started))
	}
	if maximum.Load() != 1 {
		t.Fatalf("maximum concurrent publisher calls = %d, want 1", maximum.Load())
	}
	if !fixture.manager.liveEvents.closed.Load() {
		t.Fatal("publisher close fence was not set after shutdown timeout")
	}
	if calls.Load() != 1 {
		t.Fatalf("publisher calls before release = %d, want 1", calls.Load())
	}
	close(release)
	select {
	case <-publisherReturned:
	case <-time.After(time.Second):
		t.Fatal("publisher did not return after release")
	}
	select {
	case <-fixture.manager.liveEvents.published:
		if calls.Load() != 1 {
			t.Fatalf("publisher calls after release = %d, want only the started call", calls.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("publisher worker did not discard queued batches and stop after release")
	}
}

func TestManagerShutdownFlushesPendingLiveEvent(t *testing.T) {
	published := make(chan LiveEventBatchDTO, 1)
	fixture := newManagerFixture(t, func(options *ManagerOptions) {
		options.BatchInterval = 5 * time.Millisecond
		options.LiveEventPublisher = func(batch LiveEventBatchDTO) {
			published <- batch
		}
	})
	sink, err := fixture.manager.OpenSession(context.Background(), fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	payload := managerProtoPayload(t, &new_douyin.Webcast_Im_ChatMessage{
		Common:  &new_douyin.Webcast_Im_Common{MsgId: 909},
		Content: "flush-on-shutdown",
	})
	sink.Accept(managerLiveMessage(methodChat, payload, fixture.now.Add(time.Second)))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	err = fixture.manager.Shutdown(ctx)
	cancel()
	if err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	select {
	case batch := <-published:
		if batch.SessionID != fixture.sessionID || len(batch.Events) != 1 ||
			batch.Events[0].Content != "flush-on-shutdown" {
			t.Fatalf("shutdown batch = %+v", batch)
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown() did not flush the pending live event")
	}
	select {
	case <-fixture.manager.liveEvents.published:
	default:
		t.Fatal("publisher worker was not stopped before responsive shutdown returned")
	}
}

func TestLiveEventDispatcherFullQueuesDropOnlyUICopies(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	dispatcher := newLiveEventDispatcher(func(LiveEventBatchDTO) {
		once.Do(func() { close(entered) })
		<-release
	}, time.Now)
	defer func() {
		close(release)
		dispatcher.shutdown()
	}()

	dispatcher.enqueueSources("session-full", realtimeEventRange("session-full", 1, 100))
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("publisher did not block")
	}
	started := time.Now()
	dispatcher.enqueueSources(
		"session-full",
		realtimeEventRange("session-full", 101, 101+liveEventInputCapacity*2),
	)
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("full dispatcher enqueue blocked for %s", elapsed)
	}
}

func TestLiveEventDTOJSONIsExactPrivacyAllowlist(t *testing.T) {
	numeric := 9.5
	event := Event{
		ID: "event-safe", SessionID: "session-safe", IngestSequence: 7,
		Role: EventRoleSource, Method: "SECRET_METHOD", Kind: EventGift,
		PlatformMessageID: "SECRET_PLATFORM_MESSAGE", DedupeKey: "SECRET_DEDUPE",
		ReceivedAt: time.UnixMilli(1_900_000_000_000).UTC(), SessionOffsetMS: 123,
		UserHash: "SECRET_USER_HASH", DisplayName: "allowed-name",
		Content: "allowed-content", NumericValue: &numeric,
		NormalizedJSON: "{\"secret\":true}",
		Raw:            RawRef{File: "SECRET_PATH", Offset: 1, Length: 2, CRC32C: 3},
		ParseStatus:    ParseParsed, ParseErrorCode: "SECRET_PARSE_ERROR",
		NormalizerVersion: "SECRET_NORMALIZER",
	}
	payload, err := json.Marshal(LiveEventBatchDTO{
		SessionID: "session-safe", EmittedAt: 1_900_000_000_100,
		Events: []LiveEventDTO{liveEventDTO(event)},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"SECRET_METHOD", "SECRET_PLATFORM_MESSAGE", "SECRET_DEDUPE",
		"SECRET_USER_HASH", "SECRET_PATH", "SECRET_PARSE_ERROR", "SECRET_NORMALIZER",
		"normalizedJSON", "userHash", "platformMessageId", "dedupeKey", "method", "raw",
	} {
		if strings.Contains(string(payload), secret) {
			t.Fatalf("JSON leaked %q: %s", secret, payload)
		}
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	assertExactJSONKeys(t, decoded, "sessionId", "emittedAt", "events")
	events, ok := decoded["events"].([]any)
	if !ok || len(events) != 1 {
		t.Fatalf("decoded events = %#v", decoded["events"])
	}
	decodedEvent, ok := events[0].(map[string]any)
	if !ok {
		t.Fatalf("decoded event = %#v", events[0])
	}
	assertExactJSONKeys(t, decodedEvent,
		"id", "ingestSequence", "role", "kind", "receivedAt", "sessionOffsetMs",
		"displayName", "content", "numericValue", "parseStatus",
	)
}

func TestLiveEventDispatcherIgnoresAggregateAndCrossSessionEvents(t *testing.T) {
	published := make(chan LiveEventBatchDTO, 1)
	dispatcher := newLiveEventDispatcher(func(batch LiveEventBatchDTO) {
		published <- batch
	}, time.Now)
	t.Cleanup(dispatcher.shutdown)

	aggregate := realtimeSourceEvent("session-filter", 1)
	aggregate.Role = EventRoleAggregate
	crossSession := realtimeSourceEvent("session-other", 2)
	dispatcher.enqueueSources("session-filter", []Event{aggregate, crossSession})
	assertNoLiveEventBatch(t, published, 2*liveEventBatchWindow)
}

func realtimeSourceEvent(sessionID string, sequence int64) Event {
	return Event{
		ID:        fmt.Sprintf("event-%d", sequence),
		SessionID: sessionID, IngestSequence: sequence,
		Role: EventRoleSource, Kind: EventChat,
		ReceivedAt:      time.UnixMilli(1_900_000_000_000 + sequence).UTC(),
		SessionOffsetMS: sequence, Content: fmt.Sprintf("content-%d", sequence),
		ParseStatus: ParseParsed,
	}
}

func realtimeEventRange(sessionID string, first, end int) []Event {
	result := make([]Event, 0, end-first)
	for sequence := first; sequence < end; sequence++ {
		result = append(result, realtimeSourceEvent(sessionID, int64(sequence)))
	}
	return result
}

func assertNoLiveEventBatch(
	t *testing.T,
	published <-chan LiveEventBatchDTO,
	duration time.Duration,
) {
	t.Helper()
	select {
	case batch := <-published:
		t.Fatalf("unexpected published batch: %+v", batch)
	case <-time.After(duration):
	}
}

func assertExactJSONKeys(t *testing.T, value map[string]any, keys ...string) {
	t.Helper()
	if len(value) != len(keys) {
		t.Fatalf("JSON keys = %v, want %v", mapKeys(value), keys)
	}
	for _, key := range keys {
		if _, found := value[key]; !found {
			t.Fatalf("JSON key %q missing from %v", key, mapKeys(value))
		}
	}
}

func mapKeys(value map[string]any) []string {
	result := make([]string, 0, len(value))
	for key := range value {
		result = append(result, key)
	}
	return result
}
