package eventstore

import (
	"strings"
	"testing"
	"time"
)

func giftSource(sequence int64, at time.Time) Event {
	return Event{
		ID:              "source",
		SessionID:       "session",
		IngestSequence:  sequence,
		Role:            EventRoleSource,
		Method:          "WebcastGiftMessage",
		Kind:            EventGift,
		ReceivedAt:      at,
		SessionOffsetMS: sequence * 100,
		DisplayName:     "viewer",
	}
}

func TestGiftComboKeyPrecedenceAndPrivacy(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)
	base := GiftObservation{
		SessionID:  "session",
		UserHash:   "h1.safe",
		GiftID:     "gift",
		GroupID:    123456,
		TraceID:    "trace-sensitive",
		ReceivedAt: at,
	}
	groupKey, err := GiftComboKey(base)
	if err != nil {
		t.Fatal(err)
	}
	base.GroupID = 0
	traceKey, err := GiftComboKey(base)
	if err != nil {
		t.Fatal(err)
	}
	base.TraceID = ""
	fallbackKey, err := GiftComboKey(base)
	if err != nil {
		t.Fatal(err)
	}
	if groupKey == traceKey || traceKey == fallbackKey || groupKey == fallbackKey {
		t.Fatalf("precedence did not produce distinct keys: %q %q %q", groupKey, traceKey, fallbackKey)
	}
	for _, key := range []string{groupKey, traceKey, fallbackKey} {
		if strings.Contains(key, "123456") || strings.Contains(key, "trace-sensitive") || strings.Contains(key, "h1.safe") {
			t.Fatalf("combo key leaked transient data: %q", key)
		}
	}

	base.ReceivedAt = at.Add(29 * time.Second)
	same, _ := GiftComboKey(base)
	if same != fallbackKey {
		t.Fatal("fallback must group within the same 30-second bucket")
	}
	base.ReceivedAt = at.Add(31 * time.Second)
	next, _ := GiftComboKey(base)
	if next == fallbackKey {
		t.Fatal("fallback must rotate after the 30-second bucket")
	}
}

func TestGiftComboCumulativeMaxRepeatEndAndDiamondAggregate(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)
	aggregator := NewGiftComboAggregator(10 * time.Second)
	observation := GiftObservation{
		SessionID:         "session",
		SourceEventID:     "event-1",
		Sequence:          1,
		ReceivedAt:        at,
		UserHash:          "h1.viewer",
		GiftID:            "7",
		GiftName:          "Rose",
		GroupID:           99,
		Count:             2,
		UnitDiamond:       3,
		Combo:             true,
		NormalizerVersion: "v1",
	}
	updates, err := aggregator.Observe(giftSource(1, at), observation)
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0].Aggregate != nil || updates[0].State.TotalCount != 2 {
		t.Fatalf("first updates = %#v", updates)
	}

	observation.Sequence = 2
	observation.Count = 1
	observation.ReceivedAt = at.Add(time.Second)
	updates, err = aggregator.Observe(giftSource(2, observation.ReceivedAt), observation)
	if err != nil {
		t.Fatal(err)
	}
	if updates[len(updates)-1].State.TotalCount != 2 {
		t.Fatal("lower cumulative repeat count must not reduce the aggregate")
	}

	observation.Sequence = 3
	observation.Count = 5
	observation.RepeatEnd = true
	observation.ReceivedAt = at.Add(2 * time.Second)
	updates, err = aggregator.Observe(giftSource(3, observation.ReceivedAt), observation)
	if err != nil {
		t.Fatal(err)
	}
	closed := updates[len(updates)-1]
	if closed.State.Status != ComboClosed || closed.State.TotalCount != 5 || closed.Aggregate == nil {
		t.Fatalf("closed update = %#v", closed)
	}
	if closed.Aggregate.Role != EventRoleAggregate || closed.Aggregate.NumericValue == nil || *closed.Aggregate.NumericValue != 15 {
		t.Fatalf("aggregate = %#v", closed.Aggregate)
	}
	if strings.Contains(closed.Aggregate.NormalizedJSON, "99") || strings.Contains(closed.Aggregate.NormalizedJSON, "trace") {
		t.Fatalf("aggregate JSON leaked grouping source: %s", closed.Aggregate.NormalizedJSON)
	}

	observation.Count = 8
	observation.Sequence = 4
	again, err := aggregator.Observe(giftSource(4, at.Add(3*time.Second)), observation)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatal("closed combo must never reopen")
	}
}

func TestGiftComboNonComboImmediateAndIdleBoundary(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)
	aggregator := NewGiftComboAggregator(10 * time.Second)
	nonCombo := GiftObservation{
		SessionID:         "session",
		SourceEventID:     "single",
		Sequence:          1,
		ReceivedAt:        at,
		UserHash:          "h1.viewer",
		GiftID:            "8",
		GiftName:          "Star",
		Count:             1,
		Combo:             false,
		NormalizerVersion: "v1",
	}
	updates, err := aggregator.Observe(giftSource(1, at), nonCombo)
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0].State.Status != ComboClosed || updates[0].Aggregate == nil {
		t.Fatalf("non-combo updates = %#v", updates)
	}

	combo := nonCombo
	combo.SourceEventID = "combo"
	combo.Sequence = 2
	combo.GroupID = 10
	combo.Combo = true
	combo.ReceivedAt = at.Add(time.Second)
	updates, err = aggregator.Observe(giftSource(2, combo.ReceivedAt), combo)
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0].State.Status != ComboOpen {
		t.Fatalf("combo update = %#v", updates)
	}
	if got := aggregator.FlushIdle(at.Add(11*time.Second - time.Nanosecond)); len(got) != 0 {
		t.Fatalf("closed before ten-second idle: %#v", got)
	}
	got := aggregator.FlushIdle(at.Add(11 * time.Second))
	if len(got) != 1 || got[0].State.Status != ComboClosed || got[0].Aggregate == nil {
		t.Fatalf("idle flush = %#v", got)
	}
}

func TestGiftComboMissingSession(t *testing.T) {
	_, err := GiftComboKey(GiftObservation{})
	if err != ErrGiftSessionMissing {
		t.Fatalf("error = %v, want %v", err, ErrGiftSessionMissing)
	}
}
func TestGiftComboMissingIDSkipsAggregate(t *testing.T) {
	aggregator := NewGiftComboAggregator(10 * time.Second)
	updates, err := aggregator.Observe(giftSource(1, time.Unix(1_800_000_000, 0)), GiftObservation{
		SessionID: "session",
		Sequence:  1,
		GiftID:    "",
	})
	if err != nil {
		t.Fatalf("missing gift id must be non-fatal: %v", err)
	}
	if len(updates) != 0 || len(aggregator.entries) != 0 {
		t.Fatalf("missing gift id created combo state: %#v", updates)
	}
}
