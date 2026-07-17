package douyinLive

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

var (
	errInvalidStreamResponse = errors.New("invalid stream response")
	errNoStreamCandidates    = errors.New("no stream candidates")
)

// ResolvedStream is a point-in-time stream candidate returned by ResolveStreams.
// URL and SourcePath are intentionally excluded from JSON so this type cannot
// accidentally expose stream credentials through Wails or other JSON bindings.
type ResolvedStream struct {
	ID         string
	Protocol   string
	QualityKey string
	Quality    string
	Codec      string
	Bitrate    int64
	URL        string `json:"-"`
	SourcePath string `json:"-"`
}

// String returns a diagnostic-safe representation without the stream URL.
func (stream ResolvedStream) String() string {
	return fmt.Sprintf(
		"{ID:%s Protocol:%s QualityKey:%s Quality:%s Codec:%s Bitrate:%d URL:<redacted> SourcePath:<redacted>}",
		stream.ID,
		stream.Protocol,
		stream.QualityKey,
		stream.Quality,
		stream.Codec,
		stream.Bitrate,
	)
}

func (stream ResolvedStream) GoString() string {
	return stream.String()
}

type resolvedStreamCandidate struct {
	ID         string
	Protocol   string
	QualityKey string
	Quality    string
	Codec      string
	Bitrate    int64
	URL        string
	SourcePath string
}

func (candidate resolvedStreamCandidate) String() string {
	return fmt.Sprintf(
		"{ID:%s Protocol:%s QualityKey:%s Quality:%s Codec:%s Bitrate:%d URL:<redacted> SourcePath:<redacted>}",
		candidate.ID,
		candidate.Protocol,
		candidate.QualityKey,
		candidate.Quality,
		candidate.Codec,
		candidate.Bitrate,
	)
}

func (candidate resolvedStreamCandidate) GoString() string {
	return candidate.String()
}

type streamCandidateMetadata struct {
	codec   string
	bitrate int64
}

type streamCandidateBuilder struct {
	candidates []resolvedStreamCandidate
	seen       map[string]struct{}
	issues     []error
}

// ResolveStreams fetches the current room-enter response with this instance's
// existing HTTP, cookie, signing, cache, and lifecycle context, then returns a
// snapshot of its stream candidates. Callers must assume that URLs can expire.
func (dl *DouyinLive) ResolveStreams() ([]ResolvedStream, error) {
	dl.contextMu.Lock()
	defer dl.contextMu.Unlock()

	body, err := dl.fetchRoomEnterData()
	if err != nil {
		return nil, err
	}
	candidates, err := parseResolvedStreams(body)
	if err != nil && len(candidates) == 0 {
		return nil, err
	}

	streams := make([]ResolvedStream, len(candidates))
	for index, candidate := range candidates {
		streams[index] = ResolvedStream{
			ID:         candidate.ID,
			Protocol:   candidate.Protocol,
			QualityKey: candidate.QualityKey,
			Quality:    candidate.Quality,
			Codec:      candidate.Codec,
			Bitrate:    candidate.Bitrate,
			URL:        candidate.URL,
			SourcePath: candidate.SourcePath,
		}
	}
	return streams, err
}

func parseResolvedStreams(body string) ([]resolvedStreamCandidate, error) {
	var document map[string]any
	if err := json.Unmarshal([]byte(body), &document); err != nil {
		return nil, fmt.Errorf("%w: body_len=%d", errInvalidStreamResponse, len(body))
	}

	builder := streamCandidateBuilder{seen: make(map[string]struct{})}
	data, _ := document["data"].(map[string]any)
	rooms, _ := data["data"].([]any)
	for index, rawRoom := range rooms {
		room, ok := rawRoom.(map[string]any)
		if !ok {
			continue
		}
		prefix := "data.data." + strconv.Itoa(index)
		builder.parseStreamContainer(room["stream_url"], prefix+".stream_url")
		builder.parseStreamContainer(room["additional_stream_url"], prefix+".additional_stream_url")
	}

	if len(builder.candidates) == 0 {
		builder.parseFlexibleSource(data["web_stream_url"], "data.web_stream_url", "", streamCandidateMetadata{})
	}

	if len(builder.candidates) == 0 {
		errs := append([]error{errNoStreamCandidates}, builder.issues...)
		return nil, errors.Join(errs...)
	}
	if len(builder.issues) > 0 {
		return builder.candidates, errors.Join(builder.issues...)
	}
	return builder.candidates, nil
}

func (builder *streamCandidateBuilder) parseStreamContainer(value any, path string) {
	container := builder.decodeObject(value, path)
	if container == nil {
		return
	}

	metadata := builder.sdkQualityMetadata(container["live_core_sdk_data"], path+".live_core_sdk_data")
	builder.parseURLMap(container["flv_pull_url"], "flv", path+".flv_pull_url", metadata)
	builder.parseURLMap(container["hls_pull_url_map"], "hls", path+".hls_pull_url_map", metadata)
	builder.addURLValue(
		container["hls_pull_url"],
		"hls",
		"origin",
		path+".hls_pull_url",
		metadataForQuality(metadata, "origin"),
	)
	builder.parseSDKStreamData(container["live_core_sdk_data"], path+".live_core_sdk_data", metadata)
	builder.parseFlexibleSource(container["pull_datas"], path+".pull_datas", "", streamCandidateMetadata{})
}

func (builder *streamCandidateBuilder) parseURLMap(
	value any,
	protocol string,
	path string,
	metadata map[string]streamCandidateMetadata,
) {
	if value == nil {
		return
	}
	if rawURL, ok := value.(string); ok {
		builder.addURL(rawURL, protocol, "unknown", path, streamCandidateMetadata{})
		return
	}

	variants := builder.decodeObject(value, path)
	if variants == nil {
		return
	}
	for _, key := range sortedMapKeys(variants) {
		builder.addURLValue(
			variants[key],
			protocol,
			key,
			path+"."+key,
			metadataForQuality(metadata, key),
		)
	}
}

func (builder *streamCandidateBuilder) sdkQualityMetadata(
	value any,
	path string,
) map[string]streamCandidateMetadata {
	result := make(map[string]streamCandidateMetadata)
	sdk := builder.decodeObject(value, path)
	if sdk == nil {
		return result
	}
	pullData := builder.decodeObject(sdk["pull_data"], path+".pull_data")
	if pullData == nil {
		return result
	}
	options := builder.decodeObject(pullData["options"], path+".pull_data.options")
	if options == nil {
		return result
	}

	add := func(raw any) {
		quality, ok := raw.(map[string]any)
		if !ok {
			return
		}
		key := stringValue(quality["sdk_key"])
		if key == "" {
			return
		}
		result[normalizeQualityKey(key)] = streamCandidateMetadata{
			codec:   normalizeCodec(stringValue(quality["v_codec"])),
			bitrate: int64Value(quality["v_bit_rate"]),
		}
	}
	add(options["default_quality"])
	if qualities, ok := options["qualities"].([]any); ok {
		for _, quality := range qualities {
			add(quality)
		}
	}
	return result
}

func (builder *streamCandidateBuilder) parseSDKStreamData(
	value any,
	path string,
	metadata map[string]streamCandidateMetadata,
) {
	sdk := builder.decodeObject(value, path)
	if sdk == nil {
		return
	}
	pullData := builder.decodeObject(sdk["pull_data"], path+".pull_data")
	if pullData == nil {
		return
	}
	streamDataValue, exists := pullData["stream_data"]
	if !exists || stringValue(streamDataValue) == "" {
		return
	}
	streamData := builder.decodeObject(streamDataValue, path+".pull_data.stream_data")
	if streamData == nil {
		return
	}
	variants := builder.decodeObject(streamData["data"], path+".pull_data.stream_data.data")
	if variants == nil {
		return
	}

	for _, qualityKey := range sortedMapKeys(variants) {
		builder.parseFlexibleSource(
			variants[qualityKey],
			path+".pull_data.stream_data.data."+qualityKey,
			qualityKey,
			metadataForQuality(metadata, qualityKey),
		)
	}
}

func (builder *streamCandidateBuilder) parseFlexibleSource(
	value any,
	path string,
	qualityKey string,
	metadata streamCandidateMetadata,
) {
	switch typed := value.(type) {
	case nil:
		return
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return
		}
		if isHTTPURL(trimmed) {
			builder.addURL(trimmed, "", qualityKey, path, metadata)
			return
		}
		if trimmed[0] != '{' && trimmed[0] != '[' {
			return
		}
		var nested any
		if err := json.Unmarshal([]byte(trimmed), &nested); err != nil {
			builder.addIssue(path, len(trimmed), "invalid nested JSON")
			return
		}
		builder.parseFlexibleSource(nested, path, qualityKey, metadata)
	case []any:
		for index, child := range typed {
			builder.parseFlexibleSource(
				child,
				path+"."+strconv.Itoa(index),
				qualityKey,
				metadata,
			)
		}
	case map[string]any:
		localMetadata := builder.metadataFromObject(typed, path, metadata)
		foundProtocolURL := false
		for _, protocol := range []string{"flv", "hls"} {
			rawURL, exists := typed[protocol]
			if !exists {
				continue
			}
			foundProtocolURL = true
			builder.addURLValue(rawURL, protocol, qualityKey, path+"."+protocol, localMetadata)
		}
		if rawURL, exists := typed["url"]; exists {
			foundProtocolURL = true
			builder.addURLValue(rawURL, "", qualityKey, path+".url", localMetadata)
		}

		for _, key := range sortedMapKeys(typed) {
			if key == "flv" || key == "hls" || key == "url" || isMetadataKey(key) {
				continue
			}
			childQuality := qualityKey
			if childQuality == "" && !isStreamLineKey(key) {
				childQuality = key
			}
			builder.parseFlexibleSource(typed[key], path+"."+key, childQuality, localMetadata)
		}

		if foundProtocolURL {
			return
		}
	}
}

func (builder *streamCandidateBuilder) metadataFromObject(
	object map[string]any,
	path string,
	base streamCandidateMetadata,
) streamCandidateMetadata {
	result := base
	if codec := firstNonEmptyStreamString(object, "VCodec", "v_codec", "vcodec", "codec"); codec != "" {
		result.codec = normalizeCodec(codec)
	}
	if bitrate := firstPositiveInt64(object, "vbitrate", "v_bit_rate", "bitrate"); bitrate > 0 {
		result.bitrate = bitrate
	}

	if rawParams, exists := object["sdk_params"]; exists {
		params := builder.decodeObject(rawParams, path+".sdk_params")
		if params != nil {
			if codec := firstNonEmptyStreamString(params, "VCodec", "v_codec", "vcodec", "codec"); codec != "" {
				result.codec = normalizeCodec(codec)
			}
			if bitrate := firstPositiveInt64(params, "vbitrate", "v_bit_rate", "bitrate"); bitrate > 0 {
				result.bitrate = bitrate
			}
		}
	}
	return result
}

func (builder *streamCandidateBuilder) decodeObject(value any, path string) map[string]any {
	switch typed := value.(type) {
	case nil:
		return nil
	case map[string]any:
		return typed
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return nil
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
			builder.addIssue(path, len(trimmed), "invalid nested JSON object")
			return nil
		}
		return decoded
	default:
		return nil
	}
}

func (builder *streamCandidateBuilder) addURLValue(
	value any,
	protocol string,
	qualityKey string,
	path string,
	metadata streamCandidateMetadata,
) {
	rawURL, ok := value.(string)
	if !ok {
		return
	}
	builder.addURL(rawURL, protocol, qualityKey, path, metadata)
}

func (builder *streamCandidateBuilder) addURL(
	rawURL string,
	protocol string,
	qualityKey string,
	sourcePath string,
	metadata streamCandidateMetadata,
) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		builder.addIssue(sourcePath, len(rawURL), "invalid stream URL")
		return
	}

	protocol = inferStreamProtocol(protocol, parsed.Path)
	if protocol == "" {
		builder.addIssue(sourcePath, len(rawURL), "unknown stream protocol")
		return
	}
	qualityKey = normalizeQualityKey(qualityKey)
	codec := normalizeCodec(metadata.codec)
	hostPath := strings.ToLower(parsed.Hostname()) + parsed.EscapedPath()
	dedupeKey := strings.Join([]string{protocol, qualityKey, codec, hostPath}, "|")
	if _, exists := builder.seen[dedupeKey]; exists {
		return
	}
	builder.seen[dedupeKey] = struct{}{}

	sum := sha256.Sum256([]byte(dedupeKey))
	builder.candidates = append(builder.candidates, resolvedStreamCandidate{
		ID:         fmt.Sprintf("stream-%x", sum[:8]),
		Protocol:   protocol,
		QualityKey: qualityKey,
		Quality:    qualityLabel(qualityKey),
		Codec:      codec,
		Bitrate:    metadata.bitrate,
		URL:        rawURL,
		SourcePath: sourcePath,
	})
}

func (builder *streamCandidateBuilder) addIssue(path string, length int, reason string) {
	builder.issues = append(builder.issues, fmt.Errorf("%s: %s length=%d", path, reason, length))
}

func metadataForQuality(
	metadata map[string]streamCandidateMetadata,
	qualityKey string,
) streamCandidateMetadata {
	return metadata[normalizeQualityKey(qualityKey)]
}

func normalizeQualityKey(value string) string {
	key := strings.ToLower(strings.TrimSpace(value))
	switch key {
	case "", "unknown":
		return "unknown"
	case "full_hd1", "fullhd1", "uhd":
		return "uhd"
	case "hd1", "hd":
		return "hd"
	case "sd2", "sd":
		return "sd"
	case "sd1", "ld":
		return "ld"
	case "origin", "original", "or4":
		return "origin"
	default:
		return key
	}
}

func qualityLabel(key string) string {
	switch normalizeQualityKey(key) {
	case "origin":
		return "原画"
	case "uhd":
		return "蓝光"
	case "hd":
		return "超清"
	case "sd":
		return "高清"
	case "ld":
		return "标清"
	case "unknown":
		return "unknown"
	default:
		return "unknown:" + normalizeQualityKey(key)
	}
}

func normalizeCodec(value string) string {
	codec := strings.ToLower(strings.TrimSpace(value))
	switch codec {
	case "264", "h264", "avc", "avc1":
		return "h264"
	case "265", "h265", "hevc", "hev1", "hvc1":
		return "h265"
	default:
		return "unknown"
	}
}

func inferStreamProtocol(preferred string, path string) string {
	lowerPath := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lowerPath, ".flv"):
		return "flv"
	case strings.HasSuffix(lowerPath, ".m3u8"):
		return "hls"
	}
	switch strings.ToLower(strings.TrimSpace(preferred)) {
	case "flv":
		return "flv"
	case "hls":
		return "hls"
	default:
		return ""
	}
}

func isHTTPURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Hostname() != "" && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

func sortedMapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func isMetadataKey(key string) bool {
	switch key {
	case "sdk_params", "VCodec", "v_codec", "vcodec", "codec", "vbitrate", "v_bit_rate", "bitrate",
		"resolution", "fps", "gop", "drType", "enableEncryption", "templateRealTimeInfo":
		return true
	default:
		return false
	}
}

func isStreamLineKey(key string) bool {
	lower := strings.ToLower(key)
	return lower == "main" || lower == "backup" || strings.HasPrefix(lower, "line_")
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case float64:
		return strconv.FormatInt(int64(typed), 10)
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}

func int64Value(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case json.Number:
		result, _ := typed.Int64()
		return result
	case int64:
		return typed
	case int:
		return int64(typed)
	case string:
		result, _ := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return result
	default:
		return 0
	}
}

func firstNonEmptyStreamString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringValue(values[key]); value != "" {
			return value
		}
	}
	return ""
}

func firstPositiveInt64(values map[string]any, keys ...string) int64 {
	for _, key := range keys {
		if value := int64Value(values[key]); value > 0 {
			return value
		}
	}
	return 0
}
