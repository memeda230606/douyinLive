package analysis

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
	"strings"
)

const maxSafeInteger = int64(1<<53 - 1)

type sessionInput struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	StartedAt int64  `json:"started_at"`
	EndedAt   int64  `json:"ended_at"`
}

type eventInput struct {
	ID                string   `json:"id"`
	OffsetMS          int64    `json:"offset_ms"`
	IngestSequence    int64    `json:"ingest_sequence"`
	Role              string   `json:"role"`
	Kind              string   `json:"kind"`
	UserHash          string   `json:"user_hash,omitempty"`
	NumericValue      *float64 `json:"numeric_value,omitempty"`
	NormalizedJSON    string   `json:"normalized_json"`
	DedupeKey         string   `json:"dedupe_key"`
	ParseStatus       string   `json:"parse_status"`
	NormalizerVersion string   `json:"normalizer_version"`
}

type gapInput struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	StartMS    int64  `json:"start_ms"`
	EndMS      *int64 `json:"end_ms,omitempty"`
	Recovered  bool   `json:"recovered"`
	ReasonCode string `json:"reason_code"`
}

type computationInput struct {
	Session sessionInput `json:"session"`
	Events  []eventInput `json:"events"`
	Gaps    []gapInput   `json:"gaps"`
}

type computedAnalysis struct {
	Fingerprint     string
	AnalysisVersion string
	Summary         SummaryDTO
	Buckets         []MetricBucketDTO
	Peaks           []CandidateDTO
	Troughs         []CandidateDTO
	Highlights      []CandidateDTO
}

type bucketWork struct {
	metric       MetricBucketDTO
	chatters     map[string]struct{}
	activeUsers  map[string]struct{}
	giftValue    float64
	hasGiftValue bool
}

type interval struct{ start, end int64 }

type giftSourceFallback struct {
	offset int64
	count  int64
}

type giftSourceJSON struct {
	ComboKey string `json:"combo_key"`
	Count    int64  `json:"count"`
}

type giftAggregateJSON struct {
	TotalCount int64  `json:"total_count"`
	ValueKind  string `json:"value_kind"`
}

type likeJSON struct {
	Total *uint64 `json:"total"`
}

func compute(input computationInput) (computedAnalysis, error) {
	if input.Session.ID == "" || input.Session.StartedAt < 0 || input.Session.EndedAt <= input.Session.StartedAt {
		return computedAnalysis{}, ErrInputCorrupt
	}
	sort.Slice(input.Events, func(i, j int) bool {
		if input.Events[i].OffsetMS == input.Events[j].OffsetMS {
			return input.Events[i].ID < input.Events[j].ID
		}
		return input.Events[i].OffsetMS < input.Events[j].OffsetMS
	})
	sort.Slice(input.Gaps, func(i, j int) bool {
		if input.Gaps[i].StartMS == input.Gaps[j].StartMS {
			return input.Gaps[i].ID < input.Gaps[j].ID
		}
		return input.Gaps[i].StartMS < input.Gaps[j].StartMS
	})
	fingerprint, err := inputFingerprint(input)
	if err != nil {
		return computedAnalysis{}, err
	}
	analysisVersion := AlgorithmVersion + "+" + fingerprint[:16]
	duration := input.Session.EndedAt - input.Session.StartedAt
	wallDuration := duration
	for _, event := range input.Events {
		if event.ID == "" || event.OffsetMS < 0 {
			return computedAnalysis{}, ErrInputCorrupt
		}
		if event.OffsetMS >= duration {
			if event.OffsetMS == math.MaxInt64 {
				return computedAnalysis{}, ErrInputTooLarge
			}
			duration = event.OffsetMS + 1
		}
	}
	for _, gap := range input.Gaps {
		if gap.ID == "" || gap.StartMS < 0 || (gap.EndMS != nil && *gap.EndMS < gap.StartMS) {
			return computedAnalysis{}, ErrInputCorrupt
		}
		if gap.EndMS != nil && *gap.EndMS > duration {
			duration = *gap.EndMS
		} else if gap.EndMS == nil && gap.StartMS >= duration {
			if gap.StartMS == math.MaxInt64 {
				return computedAnalysis{}, ErrInputTooLarge
			}
			duration = gap.StartMS + 1
		}
	}
	bucketCount64 := (duration + BucketSizeMS - 1) / BucketSizeMS
	if bucketCount64 <= 0 || bucketCount64 > maxBuckets {
		return computedAnalysis{}, ErrInputTooLarge
	}
	work := make([]bucketWork, int(bucketCount64))
	for index := range work {
		work[index].metric.BucketStartMS = int64(index) * BucketSizeMS
		work[index].metric.BucketSizeMS = BucketSizeMS
		work[index].chatters = make(map[string]struct{})
		work[index].activeUsers = make(map[string]struct{})
	}

	aggregatedGiftKeys := make(map[string]struct{})
	for _, event := range input.Events {
		if event.Role == "aggregate" && event.Kind == "gift" && strings.HasPrefix(event.DedupeKey, "gift-combo:") {
			aggregatedGiftKeys[strings.TrimPrefix(event.DedupeKey, "gift-combo:")] = struct{}{}
		}
	}
	fallbackGifts := make(map[string]giftSourceFallback)
	var likeBaseline *uint64
	unparsed := false
	for _, event := range input.Events {
		bucket := &work[event.OffsetMS/BucketSizeMS]
		if event.Role == "source" {
			if err := addCount(&bucket.metric.MessageTotal, 1); err != nil {
				return computedAnalysis{}, err
			}
			if event.ParseStatus != "parsed" {
				unparsed = true
			}
			if event.UserHash != "" {
				bucket.activeUsers[event.UserHash] = struct{}{}
			}
		}
		switch {
		case event.Role == "source" && event.Kind == "chat":
			if err := addCount(&bucket.metric.ChatCount, 1); err != nil {
				return computedAnalysis{}, err
			}
			if event.UserHash != "" {
				bucket.chatters[event.UserHash] = struct{}{}
			}
		case event.Role == "source" && event.Kind == "like":
			delta, nextBaseline, countErr := likeDelta(event, likeBaseline)
			if countErr != nil {
				return computedAnalysis{}, countErr
			}
			likeBaseline = nextBaseline
			if err := addCount(&bucket.metric.LikeDelta, delta); err != nil {
				return computedAnalysis{}, err
			}
		case event.Role == "source" && event.Kind == "follow":
			if err := addCount(&bucket.metric.FollowCount, 1); err != nil {
				return computedAnalysis{}, err
			}
		case event.Role == "source" && event.Kind == "member":
			if err := addCount(&bucket.metric.EnterCount, 1); err != nil {
				return computedAnalysis{}, err
			}
		case event.Role == "aggregate" && event.Kind == "gift":
			if err := addAggregateGift(bucket, event); err != nil {
				return computedAnalysis{}, err
			}
		case event.Role == "source" && event.Kind == "gift":
			key, count, countErr := sourceGift(event)
			if countErr != nil {
				return computedAnalysis{}, countErr
			}
			if key == "" {
				key = event.ID
			}
			if _, aggregated := aggregatedGiftKeys[key]; aggregated {
				continue
			}
			previous, exists := fallbackGifts[key]
			if !exists || count > previous.count || (count == previous.count && event.OffsetMS > previous.offset) {
				fallbackGifts[key] = giftSourceFallback{offset: event.OffsetMS, count: count}
			}
		}
	}
	for _, fallback := range fallbackGifts {
		if err := addCount(&work[fallback.offset/BucketSizeMS].metric.GiftCount, fallback.count); err != nil {
			return computedAnalysis{}, err
		}
	}

	mergedGaps, err := mergeGapIntervals(input.Gaps, duration)
	if err != nil {
		return computedAnalysis{}, err
	}
	applyCompleteness(work, mergedGaps, duration)
	buckets := make([]MetricBucketDTO, len(work))
	globalChatters := make(map[string]struct{})
	globalUsers := make(map[string]struct{})
	totals := MetricTotalsDTO{}
	var totalGiftValue float64
	hasGiftValue := false
	weightedCompleteness := 0.0
	for index := range work {
		work[index].metric.UniqueChatters = int64(len(work[index].chatters))
		work[index].metric.ActiveUsers = int64(len(work[index].activeUsers))
		if work[index].hasGiftValue {
			value := work[index].giftValue
			work[index].metric.GiftValue = &value
			totalGiftValue += value
			hasGiftValue = true
		}
		for user := range work[index].chatters {
			globalChatters[user] = struct{}{}
		}
		for user := range work[index].activeUsers {
			globalUsers[user] = struct{}{}
		}
		metric := work[index].metric
		for target, value := range map[*int64]int64{
			&totals.ChatCount: metric.ChatCount, &totals.LikeDelta: metric.LikeDelta,
			&totals.GiftCount: metric.GiftCount, &totals.FollowCount: metric.FollowCount,
			&totals.EnterCount: metric.EnterCount, &totals.MessageTotal: metric.MessageTotal,
		} {
			if err := addCount(target, value); err != nil {
				return computedAnalysis{}, err
			}
		}
		span := bucketSpan(index, len(work), duration)
		weightedCompleteness += metric.Completeness * float64(span)
		buckets[index] = metric
	}
	totals.UniqueChatters = int64(len(globalChatters))
	totals.ActiveUsers = int64(len(globalUsers))
	if hasGiftValue {
		totals.GiftValue = &totalGiftValue
	}
	overallCompleteness := weightedCompleteness / float64(duration)
	scores, contributions := effectScores(work, duration)
	median := medianOf(scores)
	deviations := make([]float64, len(scores))
	for index, score := range scores {
		deviations[index] = math.Abs(score - median)
	}
	mad := medianOf(deviations)
	peakThreshold := median + 2*mad
	peaks := detectCandidates("peak", scores, contributions, buckets, duration, median, mad, peakThreshold, true, analysisVersion)
	troughs := make([]CandidateDTO, 0)
	if duration > 10*60*1000 && overallCompleteness >= 0.8 {
		troughThreshold := median - 2*mad
		troughs = detectCandidates("trough", scores, contributions, buckets, duration, median, mad, troughThreshold, false, analysisVersion)
	}
	highlights := makeHighlights(peaks, buckets, analysisVersion)
	warnings := make([]string, 0, 5)
	if len(mergedGaps) > 0 {
		warnings = append(warnings, "GAPS_PRESENT")
	}
	if totals.GiftCount > 0 && totals.GiftValue == nil {
		warnings = append(warnings, "GIFT_VALUE_UNAVAILABLE")
	}
	if overallCompleteness < 0.8 {
		warnings = append(warnings, "LOW_COMPLETENESS")
	}
	if duration > wallDuration {
		warnings = append(warnings, "TIMELINE_EXTENDED")
	}
	if unparsed {
		warnings = append(warnings, "UNPARSED_EVENTS_PRESENT")
	}
	return computedAnalysis{
		Fingerprint: fingerprint, AnalysisVersion: analysisVersion,
		Summary: SummaryDTO{
			DurationMS: duration, BucketSizeMS: BucketSizeMS, BucketCount: len(buckets),
			Completeness: overallCompleteness, Totals: totals, PeakCount: len(peaks),
			TroughCount: len(troughs), HighlightCount: len(highlights), Warnings: warnings,
		},
		Buckets: buckets, Peaks: peaks, Troughs: troughs, Highlights: highlights,
	}, nil
}

func inputFingerprint(input computationInput) (string, error) {
	encoded, err := json.Marshal(struct {
		Algorithm string           `json:"algorithm"`
		Input     computationInput `json:"input"`
	}{Algorithm: AlgorithmVersion, Input: input})
	if err != nil {
		return "", ErrInputCorrupt
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func addCount(target *int64, value int64) error {
	if value < 0 || *target < 0 || value > maxSafeInteger-*target {
		return ErrInputTooLarge
	}
	*target += value
	return nil
}

func numericCount(value *float64) (int64, bool, error) {
	if value == nil {
		return 0, false, nil
	}
	if math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || math.Trunc(*value) != *value || *value > float64(maxSafeInteger) {
		return 0, false, ErrInputCorrupt
	}
	return int64(*value), true, nil
}

func likeDelta(event eventInput, baseline *uint64) (int64, *uint64, error) {
	var decoded likeJSON
	if event.NormalizedJSON != "" && json.Unmarshal([]byte(event.NormalizedJSON), &decoded) != nil {
		decoded.Total = nil
	}
	count, available, err := numericCount(event.NumericValue)
	if err != nil {
		return 0, baseline, err
	}
	if available {
		if decoded.Total != nil {
			value := *decoded.Total
			baseline = &value
		}
		return count, baseline, nil
	}
	if decoded.Total == nil || *decoded.Total > uint64(maxSafeInteger) {
		return 0, baseline, nil
	}
	current := *decoded.Total
	if baseline == nil || current < *baseline {
		return 0, &current, nil
	}
	delta := current - *baseline
	return int64(delta), &current, nil
}

func sourceGift(event eventInput) (string, int64, error) {
	var decoded giftSourceJSON
	_ = json.Unmarshal([]byte(event.NormalizedJSON), &decoded)
	count := decoded.Count
	if count <= 0 {
		value, available, err := numericCount(event.NumericValue)
		if err != nil {
			return "", 0, err
		}
		if available {
			count = value
		}
	}
	if count <= 0 || count > maxSafeInteger {
		return "", 0, ErrInputCorrupt
	}
	return decoded.ComboKey, count, nil
}

func addAggregateGift(bucket *bucketWork, event eventInput) error {
	var decoded giftAggregateJSON
	_ = json.Unmarshal([]byte(event.NormalizedJSON), &decoded)
	count := decoded.TotalCount
	numeric, available, err := numericCount(event.NumericValue)
	if err != nil {
		return err
	}
	if count <= 0 && decoded.ValueKind != "diamond" && available {
		count = numeric
	}
	if count <= 0 || count > maxSafeInteger {
		return ErrInputCorrupt
	}
	if err := addCount(&bucket.metric.GiftCount, count); err != nil {
		return err
	}
	if decoded.ValueKind == "diamond" {
		if !available {
			return ErrInputCorrupt
		}
		bucket.giftValue += *event.NumericValue
		if math.IsInf(bucket.giftValue, 0) || math.IsNaN(bucket.giftValue) {
			return ErrInputTooLarge
		}
		bucket.hasGiftValue = true
	}
	return nil
}

func mergeGapIntervals(gaps []gapInput, duration int64) ([]interval, error) {
	values := make([]interval, 0, len(gaps))
	for _, gap := range gaps {
		end := duration
		if gap.EndMS != nil {
			end = *gap.EndMS
		}
		start := min(gap.StartMS, duration)
		end = min(end, duration)
		if end > start {
			values = append(values, interval{start: start, end: end})
		}
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].start == values[j].start {
			return values[i].end < values[j].end
		}
		return values[i].start < values[j].start
	})
	merged := make([]interval, 0, len(values))
	for _, value := range values {
		if len(merged) == 0 || value.start > merged[len(merged)-1].end {
			merged = append(merged, value)
			continue
		}
		if value.end > merged[len(merged)-1].end {
			merged[len(merged)-1].end = value.end
		}
	}
	return merged, nil
}

func applyCompleteness(work []bucketWork, gaps []interval, duration int64) {
	gapIndex := 0
	for index := range work {
		start := int64(index) * BucketSizeMS
		end := min(start+BucketSizeMS, duration)
		for gapIndex < len(gaps) && gaps[gapIndex].end <= start {
			gapIndex++
		}
		missing := int64(0)
		for cursor := gapIndex; cursor < len(gaps) && gaps[cursor].start < end; cursor++ {
			missing += max(int64(0), min(end, gaps[cursor].end)-max(start, gaps[cursor].start))
		}
		span := end - start
		work[index].metric.Completeness = float64(span-missing) / float64(span)
	}
}

func bucketSpan(index, count int, duration int64) int64 {
	start := int64(index) * BucketSizeMS
	if index == count-1 {
		return duration - start
	}
	return BucketSizeMS
}

func effectScores(work []bucketWork, duration int64) ([]float64, [][]MetricContributionDTO) {
	features := make([][5]float64, len(work))
	for index := range work {
		from := max(0, index-1)
		to := min(len(work)-1, index+1)
		users := make(map[string]struct{})
		var chat, likes, follows int64
		gift := 0.0
		giftHasValue := false
		for cursor := from; cursor <= to; cursor++ {
			if work[cursor].hasGiftValue {
				giftHasValue = true
				break
			}
		}
		for cursor := from; cursor <= to; cursor++ {
			chat += work[cursor].metric.ChatCount
			likes += work[cursor].metric.LikeDelta
			follows += work[cursor].metric.FollowCount
			for user := range work[cursor].activeUsers {
				users[user] = struct{}{}
			}
			if giftHasValue && work[cursor].hasGiftValue {
				gift += work[cursor].giftValue
			} else if !giftHasValue {
				gift += float64(work[cursor].metric.GiftCount)
			}
		}
		windowStart := int64(from) * BucketSizeMS
		windowEnd := min(int64(to+1)*BucketSizeMS, duration)
		seconds := float64(windowEnd-windowStart) / 1000
		features[index] = [5]float64{
			float64(chat) / seconds, float64(len(users)), float64(likes), float64(follows), gift,
		}
	}
	names := [5]string{"chat_rate", "unique_interactors", "like_delta", "follow_count", "gift_value_or_count"}
	weights := [5]float64{0.30, 0.20, 0.20, 0.15, 0.15}
	zscores := [5][]float64{}
	for column := 0; column < len(names); column++ {
		series := make([]float64, len(features))
		for row := range features {
			series[row] = features[row][column]
		}
		zscores[column] = standardize(series)
	}
	scores := make([]float64, len(features))
	contributions := make([][]MetricContributionDTO, len(features))
	for row := range features {
		contributions[row] = make([]MetricContributionDTO, 0, len(names))
		for column, name := range names {
			part := weights[column] * zscores[column][row]
			scores[row] += part
			contributions[row] = append(contributions[row], MetricContributionDTO{
				Metric: name, Weight: weights[column], Score: part,
			})
		}
	}
	return scores, contributions
}

func standardize(values []float64) []float64 {
	result := make([]float64, len(values))
	if len(values) == 0 {
		return result
	}
	mean := 0.0
	for _, value := range values {
		mean += value
	}
	mean /= float64(len(values))
	variance := 0.0
	for _, value := range values {
		delta := value - mean
		variance += delta * delta
	}
	deviation := math.Sqrt(variance / float64(len(values)))
	if deviation == 0 {
		return result
	}
	for index, value := range values {
		result[index] = (value - mean) / deviation
	}
	return result
}

func medianOf(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	copyValues := append([]float64(nil), values...)
	sort.Float64s(copyValues)
	middle := len(copyValues) / 2
	if len(copyValues)%2 == 1 {
		return copyValues[middle]
	}
	return (copyValues[middle-1] + copyValues[middle]) / 2
}

func detectCandidates(
	kind string,
	scores []float64,
	contributions [][]MetricContributionDTO,
	buckets []MetricBucketDTO,
	duration int64,
	median, mad, threshold float64,
	peak bool,
	analysisVersion string,
) []CandidateDTO {
	matches := make([]bool, len(scores))
	for index, score := range scores {
		if peak {
			matches[index] = (mad == 0 && score > median) || (mad > 0 && score >= threshold)
		} else {
			matches[index] = (mad == 0 && score < median) || (mad > 0 && score <= threshold)
		}
	}
	result := make([]CandidateDTO, 0)
	for index := 0; index < len(matches); {
		if !matches[index] {
			index++
			continue
		}
		end := index
		for end+1 < len(matches) && matches[end+1] {
			end++
		}
		if end-index+1 >= 2 {
			result = append(result, candidateFromRun(kind, index, end, scores, contributions, buckets, duration, median, mad, threshold, analysisVersion))
		}
		index = end + 1
	}
	return mergeCandidates(result, analysisVersion)
}

func candidateFromRun(
	kind string, start, end int, scores []float64, contributions [][]MetricContributionDTO,
	buckets []MetricBucketDTO, duration int64, median, mad, threshold float64, analysisVersion string,
) CandidateDTO {
	best := start
	completeness := 1.0
	for index := start; index <= end; index++ {
		if (kind == "peak" && scores[index] > scores[best]) || (kind == "trough" && scores[index] < scores[best]) {
			best = index
		}
		completeness = math.Min(completeness, buckets[index].Completeness)
	}
	value := CandidateDTO{
		Kind: kind, StartMS: int64(start) * BucketSizeMS,
		EndMS: min(int64(end+1)*BucketSizeMS, duration), Score: scores[best],
		Threshold: threshold, BaselineMedian: median, BaselineMAD: mad,
		Completeness:     completeness,
		Contributions:    append([]MetricContributionDTO(nil), contributions[best]...),
		EvidenceBucketMS: evidenceBuckets(start, end), AlgorithmVersion: AlgorithmVersion,
	}
	value.ID = candidateID(value, analysisVersion)
	return value
}

func evidenceBuckets(start, end int) []int64 {
	result := make([]int64, 0, min(8, end-start+1))
	for index := start; index <= end && len(result) < 8; index++ {
		result = append(result, int64(index)*BucketSizeMS)
	}
	return result
}

func mergeCandidates(values []CandidateDTO, analysisVersion string) []CandidateDTO {
	if len(values) < 2 {
		return values
	}
	result := []CandidateDTO{values[0]}
	for _, next := range values[1:] {
		current := &result[len(result)-1]
		if next.StartMS-current.EndMS > 60_000 {
			result = append(result, next)
			continue
		}
		current.EndMS = max(current.EndMS, next.EndMS)
		current.Completeness = math.Min(current.Completeness, next.Completeness)
		if (current.Kind == "peak" && next.Score > current.Score) || (current.Kind == "trough" && next.Score < current.Score) {
			current.Score = next.Score
			current.Contributions = next.Contributions
		}
		current.EvidenceBucketMS = appendUniqueEvidence(current.EvidenceBucketMS, next.EvidenceBucketMS...)
		current.ID = candidateID(*current, analysisVersion)
	}
	return result
}

func appendUniqueEvidence(existing []int64, values ...int64) []int64 {
	seen := make(map[int64]struct{}, len(existing)+len(values))
	for _, value := range existing {
		seen[value] = struct{}{}
	}
	for _, value := range values {
		if len(existing) >= 8 {
			break
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		existing = append(existing, value)
	}
	return existing
}

func makeHighlights(peaks []CandidateDTO, buckets []MetricBucketDTO, analysisVersion string) []CandidateDTO {
	result := make([]CandidateDTO, 0, len(peaks))
	for _, peak := range peaks {
		hasChat := false
		for _, bucket := range buckets {
			if bucket.BucketStartMS >= peak.EndMS {
				break
			}
			if bucket.BucketStartMS >= peak.StartMS && bucket.ChatCount > 0 {
				hasChat = true
				break
			}
		}
		if !hasChat {
			continue
		}
		highlight := peak
		highlight.Kind = "highlight"
		highlight.SourceCandidateID = peak.ID
		highlight.ID = candidateID(highlight, analysisVersion)
		result = append(result, highlight)
	}
	return result
}

func candidateID(value CandidateDTO, analysisVersion string) string {
	encoded, _ := json.Marshal(struct {
		Version string `json:"version"`
		Kind    string `json:"kind"`
		Start   int64  `json:"start"`
		End     int64  `json:"end"`
	}{analysisVersion, value.Kind, value.StartMS, value.EndMS})
	digest := sha256.Sum256(encoded)
	return "candidate-" + hex.EncodeToString(digest[:8])
}
