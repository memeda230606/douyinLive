package analysis

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComputeFixedEventSetGolden(t *testing.T) {
	input := computationInput{
		Session: sessionInput{ID: "018f0000-0000-7000-8000-000000000001", Status: "completed", StartedAt: 1_000, EndedAt: 151_000},
		Events: []eventInput{
			testEvent("018f0000-0000-7000-8000-000000000010", 1, 1_000, "source", "chat", "user-a", nil, `{}`, "parsed"),
			testEvent("018f0000-0000-7000-8000-000000000011", 2, 11_000, "source", "chat", "user-a", nil, `{}`, "parsed"),
			testEvent("018f0000-0000-7000-8000-000000000012", 3, 12_000, "source", "like", "user-a", floatTest(3), `{"count":3,"total":10}`, "parsed"),
			testEvent("018f0000-0000-7000-8000-000000000013", 4, 21_000, "source", "like", "user-a", nil, `{"total":12}`, "parsed"),
			testEvent("018f0000-0000-7000-8000-000000000014", 5, 22_000, "source", "gift", "user-b", floatTest(1), `{"combo_key":"gift-a","count":1}`, "parsed"),
			testEvent("018f0000-0000-7000-8000-000000000015", 6, 28_000, "source", "gift", "user-b", floatTest(3), `{"combo_key":"gift-a","count":3}`, "parsed"),
			testEvent("018f0000-0000-7000-8000-000000000016", 7, 31_000, "source", "like", "user-a", nil, `{"total":2}`, "parsed"),
			testEvent("018f0000-0000-7000-8000-000000000017", 8, 32_000, "source", "member", "user-c", nil, `{}`, "parsed"),
			testEvent("018f0000-0000-7000-8000-000000000018", 9, 33_000, "source", "follow", "user-c", nil, `{}`, "parsed"),
			testEvent("018f0000-0000-7000-8000-000000000019", 10, 41_000, "source", "like", "user-a", nil, `{"total":5}`, "parsed"),
			testAggregateGift("018f0000-0000-7000-8000-000000000020", 11, 42_000, "gift-b", 2, "count", 2),
			testEvent("018f0000-0000-7000-8000-000000000021", 12, 51_000, "source", "system", "", nil, `{}`, "failed"),
		},
		Gaps: []gapInput{{
			ID: "018f0000-0000-7000-8000-000000000030", Kind: "message_disconnect",
			StartMS: 15_000, EndMS: int64Test(25_000), Recovered: true, ReasonCode: "TEST_GAP",
		}},
	}
	for index := 0; index < 12; index++ {
		offset := int64(80_500 + index*300)
		input.Events = append(input.Events, testEvent(
			testID(100+index), int64(20+index), offset, "source", "chat",
			"spike-user-"+string(rune('a'+index)), nil, `{}`, "parsed",
		))
	}
	for index := 0; index < 10; index++ {
		offset := int64(90_500 + index*300)
		input.Events = append(input.Events, testEvent(
			testID(200+index), int64(40+index), offset, "source", "chat",
			"spike-two-"+string(rune('a'+index)), nil, `{}`, "parsed",
		))
	}
	for index := 0; index < 10; index++ {
		offset := int64(100_500 + index*300)
		input.Events = append(input.Events, testEvent(
			testID(300+index), int64(60+index), offset, "source", "chat",
			"spike-three-"+string(rune('a'+index)), nil, `{}`, "parsed",
		))
	}
	computed, err := compute(input)
	if err != nil {
		t.Fatalf("compute() error = %v", err)
	}
	view := struct {
		Fingerprint     string            `json:"fingerprint"`
		AnalysisVersion string            `json:"analysisVersion"`
		Summary         SummaryDTO        `json:"summary"`
		Buckets         []MetricBucketDTO `json:"buckets"`
		Peaks           []CandidateDTO    `json:"peaks"`
		Troughs         []CandidateDTO    `json:"troughs"`
		Highlights      []CandidateDTO    `json:"highlights"`
	}{
		computed.Fingerprint, computed.AnalysisVersion, computed.Summary, computed.Buckets,
		computed.Peaks, computed.Troughs, computed.Highlights,
	}
	actual, err := json.MarshalIndent(view, "", "  ")
	if err != nil {
		t.Fatalf("marshal golden view: %v", err)
	}
	actual = append(actual, '\n')
	goldenPath := filepath.Join("testdata", "basic_report.golden.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(goldenPath, actual, 0o600); err != nil {
			t.Fatalf("update golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(actual) != string(want) {
		t.Fatalf("fixed analysis changed\nACTUAL:\n%s\nWANT:\n%s", actual, want)
	}
}

func TestComputeTroughRequiresLongCompleteSession(t *testing.T) {
	input := computationInput{
		Session: sessionInput{ID: testID(1), Status: "completed", StartedAt: 1_000, EndedAt: 702_000},
	}
	sequence := int64(1)
	for bucket := 0; bucket < 70; bucket++ {
		count := 4
		if bucket == 30 || bucket == 31 {
			count = 0
		}
		for item := 0; item < count; item++ {
			input.Events = append(input.Events, testEvent(
				testID(1000+bucket*10+item), sequence, int64(bucket)*BucketSizeMS+int64(item+1)*100,
				"source", "chat", testID(2000+bucket*10+item), nil, `{}`, "parsed",
			))
			sequence++
		}
	}
	computed, err := compute(input)
	if err != nil {
		t.Fatalf("compute() error = %v", err)
	}
	if len(computed.Troughs) == 0 {
		t.Fatal("long complete session must expose the sustained low window")
	}
	input.Gaps = []gapInput{{ID: testID(9999), StartMS: 0, EndMS: int64Test(200_000)}}
	computed, err = compute(input)
	if err != nil {
		t.Fatalf("compute() with gaps error = %v", err)
	}
	if computed.Summary.Completeness >= 0.8 || len(computed.Troughs) != 0 {
		t.Fatalf("low-completeness troughs = %d, completeness = %f", len(computed.Troughs), computed.Summary.Completeness)
	}
}

func TestCandidateThresholdSustainMergeAndHighlight(t *testing.T) {
	scores := []float64{0, 2, 3, 0, 0, 2.5, 3.5, 0}
	contributions := make([][]MetricContributionDTO, len(scores))
	buckets := make([]MetricBucketDTO, len(scores))
	for index := range scores {
		contributions[index] = []MetricContributionDTO{{Metric: "chat_rate", Weight: 0.3, Score: scores[index]}}
		buckets[index] = MetricBucketDTO{BucketStartMS: int64(index) * BucketSizeMS, BucketSizeMS: BucketSizeMS, ChatCount: 1, Completeness: 1}
	}
	peaks := detectCandidates("peak", scores, contributions, buckets, int64(len(scores))*BucketSizeMS, 0, 1, 2, true, "test-version")
	if len(peaks) != 1 || peaks[0].StartMS != 10_000 || peaks[0].EndMS != 70_000 || len(peaks[0].EvidenceBucketMS) < 4 {
		t.Fatalf("merged sustained peaks = %+v", peaks)
	}
	highlights := makeHighlights(peaks, buckets, "test-version")
	if len(highlights) != 1 || highlights[0].Kind != "highlight" || highlights[0].SourceCandidateID != peaks[0].ID {
		t.Fatalf("traceable highlights = %+v", highlights)
	}
}
func TestComputeRejectsInvalidCountsAndOversizedTimeline(t *testing.T) {
	negative := -1.0
	input := computationInput{
		Session: sessionInput{ID: testID(1), Status: "completed", StartedAt: 1, EndedAt: 10_001},
		Events:  []eventInput{testEvent(testID(2), 1, 1, "source", "like", "user", &negative, `{}`, "parsed")},
	}
	if _, err := compute(input); err != ErrInputCorrupt {
		t.Fatalf("negative metric error = %v", err)
	}
	input.Events[0].NumericValue = nil
	input.Events[0].OffsetMS = int64(maxBuckets)*BucketSizeMS + 1
	if _, err := compute(input); err != ErrInputTooLarge {
		t.Fatalf("oversized timeline error = %v", err)
	}
}

func testEvent(id string, sequence, offset int64, role, kind, user string, numeric *float64, normalized, parseStatus string) eventInput {
	return eventInput{
		ID: id, OffsetMS: offset, IngestSequence: sequence, Role: role, Kind: kind,
		UserHash: user, NumericValue: numeric, NormalizedJSON: normalized,
		DedupeKey: "source:" + id, ParseStatus: parseStatus, NormalizerVersion: "event-normalizer/v1",
	}
}

func testAggregateGift(id string, sequence, offset int64, key string, total int64, valueKind string, numeric float64) eventInput {
	encoded, _ := json.Marshal(giftAggregateJSON{TotalCount: total, ValueKind: valueKind})
	value := testEvent(id, sequence, offset, "aggregate", "gift", "", &numeric, string(encoded), "parsed")
	value.DedupeKey = "gift-combo:" + key
	return value
}

func floatTest(value float64) *float64 { return &value }
func int64Test(value int64) *int64     { return &value }

func testID(value int) string {
	text := strings.Repeat("0", 12-len(jsonNumber(value))) + jsonNumber(value)
	return "018f0000-0000-7000-8000-" + text
}

func jsonNumber(value int) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
