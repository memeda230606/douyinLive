package analysis

import "errors"

const (
	ContractVersion  = 1
	AlgorithmVersion = "basic-analysis/v1"
	BucketSizeMS     = int64(10_000)
	maxBuckets       = 100_000
)

var (
	ErrInvalidArgument  = errors.New("analysis argument is invalid")
	ErrSessionNotFound  = errors.New("analysis session not found")
	ErrReportNotFound   = errors.New("analysis report not found")
	ErrAnalysisNotReady = errors.New("analysis session is not ready")
	ErrInputCorrupt     = errors.New("analysis input is corrupt")
	ErrInputTooLarge    = errors.New("analysis input is too large")
)

type AnalyzeRequest struct {
	SessionID string `json:"sessionId"`
}

type MetricTotalsDTO struct {
	ChatCount      int64    `json:"chatCount"`
	UniqueChatters int64    `json:"uniqueChatters"`
	LikeDelta      int64    `json:"likeDelta"`
	GiftCount      int64    `json:"giftCount"`
	GiftValue      *float64 `json:"giftValue,omitempty"`
	FollowCount    int64    `json:"followCount"`
	EnterCount     int64    `json:"enterCount"`
	ActiveUsers    int64    `json:"activeUsers"`
	MessageTotal   int64    `json:"messageTotal"`
}

type MetricBucketDTO struct {
	BucketStartMS  int64    `json:"bucketStartMs"`
	BucketSizeMS   int64    `json:"bucketSizeMs"`
	ChatCount      int64    `json:"chatCount"`
	UniqueChatters int64    `json:"uniqueChatters"`
	LikeDelta      int64    `json:"likeDelta"`
	GiftCount      int64    `json:"giftCount"`
	GiftValue      *float64 `json:"giftValue,omitempty"`
	FollowCount    int64    `json:"followCount"`
	EnterCount     int64    `json:"enterCount"`
	ActiveUsers    int64    `json:"activeUsers"`
	MessageTotal   int64    `json:"messageTotal"`
	Completeness   float64  `json:"completeness"`
}

type MetricContributionDTO struct {
	Metric string  `json:"metric"`
	Weight float64 `json:"weight"`
	Score  float64 `json:"score"`
}

type CandidateDTO struct {
	ID                string                  `json:"id"`
	Kind              string                  `json:"kind"`
	StartMS           int64                   `json:"startMs"`
	EndMS             int64                   `json:"endMs"`
	Score             float64                 `json:"score"`
	Threshold         float64                 `json:"threshold"`
	BaselineMedian    float64                 `json:"baselineMedian"`
	BaselineMAD       float64                 `json:"baselineMad"`
	Completeness      float64                 `json:"completeness"`
	Contributions     []MetricContributionDTO `json:"contributions"`
	EvidenceBucketMS  []int64                 `json:"evidenceBucketMs"`
	AlgorithmVersion  string                  `json:"algorithmVersion"`
	SourceCandidateID string                  `json:"sourceCandidateId,omitempty"`
}

type SummaryDTO struct {
	DurationMS     int64           `json:"durationMs"`
	BucketSizeMS   int64           `json:"bucketSizeMs"`
	BucketCount    int             `json:"bucketCount"`
	Completeness   float64         `json:"completeness"`
	Totals         MetricTotalsDTO `json:"totals"`
	PeakCount      int             `json:"peakCount"`
	TroughCount    int             `json:"troughCount"`
	HighlightCount int             `json:"highlightCount"`
	Warnings       []string        `json:"warnings"`
}

type ReportDTO struct {
	Version          int               `json:"version"`
	ID               string            `json:"id"`
	SessionID        string            `json:"sessionId"`
	Status           string            `json:"status"`
	AnalysisVersion  string            `json:"analysisVersion"`
	AlgorithmVersion string            `json:"algorithmVersion"`
	StartedAt        int64             `json:"startedAt"`
	CompletedAt      int64             `json:"completedAt"`
	Summary          SummaryDTO        `json:"summary"`
	Buckets          []MetricBucketDTO `json:"buckets"`
	Peaks            []CandidateDTO    `json:"peaks"`
	Troughs          []CandidateDTO    `json:"troughs"`
	Highlights       []CandidateDTO    `json:"highlights"`
}
