package douyinLive

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"sort"
	"strings"
)

var errNoSelectableStreamCandidates = errors.New("no selectable stream candidates")

type streamSelectionPreference struct {
	QualityKey        string
	Protocol          string
	CompatibilityMode bool
}

type streamFailureClass string

const (
	streamFailureUnknown          streamFailureClass = "unknown"
	streamFailureNoCandidates     streamFailureClass = "no_candidates"
	streamFailureURLExpired       streamFailureClass = "url_expired"
	streamFailureTemporaryNetwork streamFailureClass = "temporary_network"
	streamFailureUnsupported      streamFailureClass = "unsupported"
)

// rankResolvedStreams returns the safe attempt order for the current snapshot.
// Invalid candidates are skipped and never passed to a recorder.
func rankResolvedStreams(
	candidates []ResolvedStream,
	preference streamSelectionPreference,
) ([]ResolvedStream, error) {
	ranked := make([]ResolvedStream, 0, len(candidates))
	for _, candidate := range candidates {
		candidate.URL = strings.TrimSpace(candidate.URL)
		parsed, err := url.Parse(candidate.URL)
		if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			continue
		}
		candidate.Protocol = inferStreamProtocol(candidate.Protocol, parsed.Path)
		if candidate.Protocol == "" {
			continue
		}
		candidate.QualityKey = normalizeQualityKey(candidate.QualityKey)
		candidate.Quality = qualityLabel(candidate.QualityKey)
		candidate.Codec = normalizeCodec(candidate.Codec)
		ranked = append(ranked, candidate)
	}
	if len(ranked) == 0 {
		return nil, errors.Join(
			errNoSelectableStreamCandidates,
			errNoStreamCandidates,
		)
	}

	qualityPreference := normalizeSelectionQuality(preference.QualityKey)
	protocolPreference := normalizeSelectionProtocol(preference.Protocol)
	sort.SliceStable(ranked, func(leftIndex, rightIndex int) bool {
		left := ranked[leftIndex]
		right := ranked[rightIndex]

		leftQuality := selectionQualityRank(left.QualityKey, qualityPreference)
		rightQuality := selectionQualityRank(right.QualityKey, qualityPreference)
		if leftQuality != rightQuality {
			return leftQuality < rightQuality
		}

		leftCodec := selectionCodecRank(left.Codec, preference.CompatibilityMode)
		rightCodec := selectionCodecRank(right.Codec, preference.CompatibilityMode)
		if leftCodec != rightCodec {
			return leftCodec < rightCodec
		}

		leftProtocol := selectionProtocolRank(left.Protocol, protocolPreference)
		rightProtocol := selectionProtocolRank(right.Protocol, protocolPreference)
		if leftProtocol != rightProtocol {
			return leftProtocol < rightProtocol
		}

		leftKnownBitrate := left.Bitrate > 0
		rightKnownBitrate := right.Bitrate > 0
		if leftKnownBitrate != rightKnownBitrate {
			return leftKnownBitrate
		}
		if leftKnownBitrate && left.Bitrate != right.Bitrate {
			return left.Bitrate > right.Bitrate
		}
		if left.QualityKey != right.QualityKey {
			return left.QualityKey < right.QualityKey
		}
		if left.ID != right.ID {
			return left.ID < right.ID
		}
		return left.SourcePath < right.SourcePath
	})
	return ranked, nil
}

func normalizeSelectionQuality(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || value == "auto" {
		return ""
	}
	return normalizeQualityKey(value)
}

func normalizeSelectionProtocol(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "flv":
		return "flv"
	case "hls":
		return "hls"
	default:
		return ""
	}
}

func selectionQualityRank(qualityKey, preferred string) int {
	qualityKey = normalizeQualityKey(qualityKey)
	if preferred != "" && qualityKey == preferred {
		return 0
	}
	rank := automaticQualityRank(qualityKey)
	if preferred != "" {
		return rank + 1
	}
	return rank
}

func automaticQualityRank(qualityKey string) int {
	switch normalizeQualityKey(qualityKey) {
	case "origin":
		return 0
	case "uhd":
		return 1
	case "hd":
		return 2
	case "sd":
		return 3
	case "ld":
		return 4
	case "unknown":
		return 100
	default:
		return 50
	}
}

func selectionCodecRank(codec string, compatibilityMode bool) int {
	codec = normalizeCodec(codec)
	if !compatibilityMode {
		if codec == "unknown" {
			return 1
		}
		return 0
	}
	switch codec {
	case "h264":
		return 0
	case "unknown":
		return 1
	case "h265":
		return 2
	default:
		return 3
	}
}

func selectionProtocolRank(protocol, preferred string) int {
	if preferred != "" {
		if protocol == preferred {
			return 0
		}
		return 1
	}
	if protocol == "flv" {
		return 0
	}
	return 1
}

// classifyStreamFailure maps recorder/fetch failures to a stable, redacted
// recovery category. It never returns the original error text.
func classifyStreamFailure(statusCode int, err error) streamFailureClass {
	switch statusCode {
	case 401, 403, 404, 410:
		return streamFailureURLExpired
	}
	if errors.Is(err, errNoStreamCandidates) || errors.Is(err, errNoSelectableStreamCandidates) {
		return streamFailureNoCandidates
	}
	if err == nil {
		return streamFailureUnknown
	}
	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) {
		return streamFailureTemporaryNetwork
	}
	var networkError net.Error
	if errors.As(err, &networkError) && (networkError.Timeout() || networkError.Temporary()) {
		return streamFailureTemporaryNetwork
	}

	summary := strings.ToLower(err.Error())
	for _, marker := range []string{
		"403 forbidden",
		"404 not found",
		"410 gone",
		"http 403",
		"http 404",
		"http 410",
		"status=403",
		"status=404",
		"status=410",
	} {
		if strings.Contains(summary, marker) {
			return streamFailureURLExpired
		}
	}
	for _, marker := range []string{
		"connection reset",
		"connection refused",
		"timed out",
		"timeout",
		"unexpected eof",
	} {
		if strings.Contains(summary, marker) {
			return streamFailureTemporaryNetwork
		}
	}
	for _, marker := range []string{
		"unsupported codec",
		"decoder not found",
		"invalid data found when processing input",
	} {
		if strings.Contains(summary, marker) {
			return streamFailureUnsupported
		}
	}
	return streamFailureUnknown
}
