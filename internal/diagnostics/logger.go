// Package diagnostics provides local structured logging with mandatory redaction.
package diagnostics

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const DefaultRetentionDays = 14

var (
	logFilePattern = regexp.MustCompile(`^app-(\d{4}-\d{2}-\d{2})\.jsonl$`)
	logURLSchemes  = [...]string{
		"https", "http", "wss", "ws", "rtmps", "rtmp", "rtsp", "srt", "rist", "udp", "tcp",
	}
	sensitiveLogKeys = map[string]struct{}{
		"id":             {},
		"pid":            {},
		"processid":      {},
		"room":           {},
		"session":        {},
		"sessionid":      {},
		"operation":      {},
		"operationid":    {},
		"attempt":        {},
		"attemptid":      {},
		"correlationid":  {},
		"gap":            {},
		"gapid":          {},
		"liveid":         {},
		"webrid":         {},
		"platformroomid": {},
		"roomid":         {},
		"roomconfigid":   {},
		"useruniqueid":   {},
		"userid":         {},
		"anchorid":       {},
		"secanchorid":    {},
		"livename":       {},
		"anchorname":     {},
		"displayname":    {},
		"nickname":       {},
		"title":          {},
		"cursor":         {},
		"internalext":    {},
		"websocketkey":   {},
		"xmsstub":        {},
		"pushserver":     {},
		"pushserverv2":   {},
		"proxyserver":    {},
	}
	sensitiveLogKeyMarkers = []string{
		"cookie", "authorization", "token", "signature", "abogus",
		"credential", "secret", "password", "streamurl", "signedurl",
	}
)

type FileOptions struct {
	Now           time.Time
	RetentionDays int
	Level         slog.Leveler
}

type FileLogger struct {
	Logger *slog.Logger
	Path   string
	file   *os.File
}

func OpenFileLogger(logsDir string, options FileOptions) (*FileLogger, error) {
	if options.Now.IsZero() {
		options.Now = time.Now()
	}
	if options.RetentionDays <= 0 {
		options.RetentionDays = DefaultRetentionDays
	}
	if options.Level == nil {
		options.Level = slog.LevelInfo
	}
	if err := os.MkdirAll(logsDir, 0o700); err != nil {
		return nil, fmt.Errorf("create diagnostics log directory: %w", err)
	}
	if err := removeExpiredLogs(logsDir, options.Now, options.RetentionDays); err != nil {
		return nil, err
	}

	path := filepath.Join(logsDir, "app-"+options.Now.Local().Format("2006-01-02")+".jsonl")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open diagnostics log: %w", err)
	}
	handler := NewRedactingJSONHandler(file, &slog.HandlerOptions{Level: options.Level})
	return &FileLogger{Logger: slog.New(handler), Path: path, file: file}, nil
}

// NewDiscardFileLogger provides the same lifecycle surface without creating
// a diagnostics file. Callers must opt in explicitly; production keeps the
// redacted JSONL logger by default.
func NewDiscardFileLogger() *FileLogger {
	return &FileLogger{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func NewRedactingJSONHandler(writer io.Writer, options *slog.HandlerOptions) slog.Handler {
	cleanOptions := &slog.HandlerOptions{}
	if options != nil {
		*cleanOptions = *options
	}
	replaceAttr := cleanOptions.ReplaceAttr
	cleanOptions.ReplaceAttr = func(groups []string, attr slog.Attr) slog.Attr {
		if replaceAttr != nil {
			attr = replaceAttr(groups, attr)
		}
		if attr.Equal(slog.Attr{}) {
			return attr
		}
		return redactAttr(attr, false)
	}
	return &redactingHandler{next: slog.NewJSONHandler(writer, cleanOptions)}
}

type redactingHandler struct {
	next      slog.Handler
	redactAll bool
}

func (h *redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *redactingHandler) Handle(ctx context.Context, record slog.Record) error {
	clean := slog.NewRecord(record.Time, record.Level, redactString(record.Message), record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		clean.AddAttrs(redactAttr(attr, h.redactAll))
		return true
	})
	return h.next.Handle(ctx, clean)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, 0, len(attrs))
	for _, attr := range attrs {
		clean = append(clean, redactAttr(attr, h.redactAll))
	}
	return &redactingHandler{next: h.next.WithAttrs(clean), redactAll: h.redactAll}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	redactAll := h.redactAll || !safeStructuredLogName(name) || sensitiveKey(name)
	if redactAll {
		name = "redacted_group"
	}
	return &redactingHandler{next: h.next.WithGroup(name), redactAll: redactAll}
}

func redactAttr(attr slog.Attr, redactAll bool) slog.Attr {
	nameSafe := safeStructuredLogName(attr.Key)
	value := attr.Value.Resolve()
	if !nameSafe || !redactAll && sensitiveKey(attr.Key) && !safeSensitiveMetric(attr.Key, value) {
		if value.Kind() == slog.KindGroup {
			group := value.Group()
			clean := make([]slog.Attr, 0, len(group))
			for _, child := range group {
				clean = append(clean, redactAttr(child, true))
			}
			return slog.Attr{Key: "redacted_group", Value: slog.GroupValue(clean...)}
		}
		return slog.String("redacted_field", "[REDACTED]")
	}
	if redactAll {
		return slog.String("redacted_field", "[REDACTED]")
	}
	switch value.Kind() {
	case slog.KindString:
		attr.Value = slog.StringValue(redactString(value.String()))
	case slog.KindAny:
		if text, ok := protectedLogString(value.Any()); ok {
			attr.Value = slog.StringValue(redactString(text))
		} else {
			attr.Value = slog.StringValue("[REDACTED]")
		}
	case slog.KindGroup:
		group := value.Group()
		clean := make([]slog.Attr, 0, len(group))
		for _, child := range group {
			clean = append(clean, redactAttr(child, false))
		}
		attr.Value = slog.GroupValue(clean...)
	}
	return attr
}

func safeStructuredLogName(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, current := range value {
		if unicode.IsControl(current) {
			return false
		}
	}
	return redactString(value) == value
}

func logKeyWordRune(value rune) bool {
	return unicode.IsLetter(value) || unicode.IsNumber(value)
}

func compactLogKey(key string) string {
	var compact strings.Builder
	for _, current := range strings.TrimSpace(key) {
		if logKeyWordRune(current) {
			compact.WriteRune(unicode.ToLower(current))
		}
	}
	return compact.String()
}

type logKeyClassification struct {
	canonical    string
	sensitive    bool
	lengthMetric bool
	valid        bool
}

func classifyLogKey(key string) logKeyClassification {
	normalized, valid := decodeLogKeyUnicodeEscapes(key)
	canonical := compactLogKey(normalized)
	components := splitLogKeyComponents(normalized)
	classification := logKeyClassification{
		canonical: canonical,
		sensitive: !valid || sensitiveCanonicalLogKey(canonical, components),
		valid:     valid,
	}
	if valid && len(canonical) > len("len") && strings.HasSuffix(canonical, "len") {
		baseCanonical := strings.TrimSuffix(canonical, "len")
		baseComponents := trimLogKeyLengthComponent(components)
		if sensitiveCanonicalLogKey(baseCanonical, baseComponents) {
			classification.sensitive = true
			classification.lengthMetric = true
		}
	}
	return classification
}

func splitLogKeyComponents(key string) []string {
	runes := []rune(strings.TrimSpace(key))
	components := make([]string, 0, 4)
	current := make([]rune, 0, 16)
	var previous rune
	flush := func() {
		if len(current) == 0 {
			return
		}
		components = append(components, string(current))
		current = current[:0]
		previous = 0
	}
	for index, value := range runes {
		if !logKeyWordRune(value) {
			flush()
			continue
		}
		var next rune
		if index+1 < len(runes) && logKeyWordRune(runes[index+1]) {
			next = runes[index+1]
		}
		if len(current) > 0 && logKeyComponentBoundary(previous, value, next) {
			flush()
		}
		current = append(current, unicode.ToLower(value))
		previous = value
	}
	flush()
	return components
}

func logKeyComponentBoundary(previous, current, next rune) bool {
	previousNumber := unicode.IsNumber(previous)
	currentNumber := unicode.IsNumber(current)
	if previousNumber != currentNumber {
		return true
	}
	if unicode.IsLower(previous) && unicode.IsUpper(current) {
		return true
	}
	return unicode.IsUpper(previous) && unicode.IsUpper(current) && unicode.IsLower(next)
}

func trimLogKeyLengthComponent(components []string) []string {
	if len(components) == 0 {
		return nil
	}
	last := components[len(components)-1]
	if last == "len" {
		return components[:len(components)-1]
	}
	if len(last) <= len("len") || !strings.HasSuffix(last, "len") {
		return nil
	}
	trimmed := make([]string, len(components))
	copy(trimmed, components)
	trimmed[len(trimmed)-1] = strings.TrimSuffix(last, "len")
	return trimmed
}

func decodeLogKeyUnicodeEscapes(value string) (string, bool) {
	if !utf8.ValidString(value) {
		return "", false
	}
	if !strings.Contains(value, `\u`) {
		return value, true
	}
	var decoded strings.Builder
	decoded.Grow(len(value))
	for index := 0; index < len(value); {
		if value[index] == '\\' && index+1 < len(value) && value[index+1] == 'u' {
			current, next, valid := decodeEmbeddedLogUnicode(value, index)
			if !valid || unicode.IsControl(current) {
				return "", false
			}
			decoded.WriteRune(current)
			index = next
			continue
		}
		current, size := utf8.DecodeRuneInString(value[index:])
		if current == utf8.RuneError && size == 1 || unicode.IsControl(current) {
			return "", false
		}
		decoded.WriteRune(current)
		index += size
	}
	return decoded.String(), true
}

func sensitiveCanonicalLogKey(canonical string, components []string) bool {
	if safeCanonicalLogKey(canonical) {
		return false
	}
	for start := range components {
		var candidate strings.Builder
		for end := start; end < len(components) && candidate.Len() <= maximumEmbeddedLogKeyBytes; end++ {
			candidate.WriteString(components[end])
			if _, found := sensitiveLogKeys[candidate.String()]; found {
				return true
			}
		}
	}
	for _, marker := range sensitiveLogKeyMarkers {
		if strings.Contains(canonical, marker) {
			return true
		}
	}
	return canonical == "url" || strings.HasSuffix(canonical, "url")
}

func safeCanonicalLogKey(canonical string) bool {
	switch canonical {
	case "attemptcount", "roomcount":
		return true
	default:
		return false
	}
}

func sensitiveKey(key string) bool {
	return classifyLogKey(key).sensitive
}

func safeSensitiveMetric(key string, value slog.Value) bool {
	classification := classifyLogKey(key)
	if classification.canonical == "correlationid" && value.Kind() == slog.KindString {
		return safeSymbolicCorrelation(value.String())
	}
	if classification.canonical != "attempt" && !classification.lengthMetric {
		return false
	}
	switch value.Kind() {
	case slog.KindInt64, slog.KindUint64, slog.KindFloat64:
		return true
	default:
		return false
	}
}

// RedactText removes URLs, secrets, room identities, and creator metadata
// embedded in an unstructured log message.
func RedactText(value string) string {
	return redactString(value)
}

// RedactKeyValueArgs copies and sanitizes structured key/value arguments. It
// is used by log adapters that cannot rely on a slog.Handler boundary.
func RedactKeyValueArgs(args []any) []any {
	clean := make([]any, len(args))
	copy(clean, args)
	for index := 0; index < len(clean); index += 2 {
		key, keyIsString := clean[index].(string)
		if !keyIsString {
			clean[index] = "redacted_field"
			if index+1 < len(clean) {
				clean[index+1] = "[REDACTED]"
			}
			continue
		}
		if index+1 >= len(clean) {
			if !safeStructuredLogName(key) {
				clean[index] = "redacted_field"
				continue
			}
			clean[index] = redactString(key)
			if sensitiveKey(key) {
				clean[index] = "redacted_field"
			}
			continue
		}
		value := clean[index+1]
		if !safeStructuredLogName(key) || (sensitiveKey(key) && !safeGenericSensitiveMetric(key, value)) {
			clean[index] = "redacted_field"
			clean[index+1] = "[REDACTED]"
			continue
		}
		clean[index] = redactString(key)
		clean[index+1] = redactGenericLogValue(value)
	}
	return clean
}

func safeGenericSensitiveMetric(key string, value any) bool {
	classification := classifyLogKey(key)
	if classification.canonical == "correlationid" {
		text, ok := value.(string)
		return ok && safeSymbolicCorrelation(text)
	}
	if classification.canonical != "attempt" && !classification.lengthMetric {
		return false
	}
	switch value.(type) {
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, uintptr,
		float32, float64:
		return true
	default:
		return false
	}
}

func safeSymbolicCorrelation(value string) bool {
	return value == "startup" || value == "shutdown"
}

func redactGenericLogValue(value any) any {
	switch item := value.(type) {
	case string:
		return redactString(item)
	default:
		if value == nil {
			return nil
		}
		if text, ok := protectedLogString(value); ok {
			return redactString(text)
		}
		switch reflect.TypeOf(value).Kind() {
		case reflect.Bool,
			reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
			reflect.Float32, reflect.Float64:
			return value
		default:
			return "[REDACTED]"
		}
	}
}

func protectedLogString(value any) (text string, ok bool) {
	if value == nil || nilLogInterface(value) {
		return "", false
	}
	defer func() {
		if recover() != nil {
			text = ""
			ok = false
		}
	}()
	switch item := value.(type) {
	case error:
		return item.Error(), true
	case fmt.Stringer:
		return item.String(), true
	default:
		return "", false
	}
}

func nilLogInterface(value any) bool {
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

const (
	maximumEmbeddedLogKeyBytes   = 128
	maximumEmbeddedLogQuoteLayer = 2
	maximumEmbeddedLogDepth      = 64
)

type embeddedLogParseStatus uint8

const (
	embeddedLogNoMatch embeddedLogParseStatus = iota
	embeddedLogParsed
	embeddedLogMalformed
)

type embeddedLogValueKind uint8

const (
	embeddedLogBareValue embeddedLogValueKind = iota
	embeddedLogQuotedValue
	embeddedLogCompositeValue
)

type embeddedLogQuoteMarker struct {
	quote        byte
	layer        int
	contentStart int
}

func redactEmbeddedAssignments(value string) string {
	return redactEmbeddedAssignmentsLayer(value, 0)
}

func redactEmbeddedAssignmentsLayer(value string, depth int) string {
	var clean strings.Builder
	last := 0
	changed := false
	for index := 0; index < len(value); {
		if boundaryEnd, found := embeddedLogEncodedWhitespaceEndAt(value, index); found {
			index = boundaryEnd
			continue
		}
		key, valueStart, quotedKey, status := embeddedLogAssignmentAt(value, index)
		if status == embeddedLogNoMatch {
			if quotedKey {
				contentStart, contentEnd, afterValue, valid := embeddedLogQuotedContent(value, index)
				if valid {
					nested := "[REDACTED]"
					if depth < maximumEmbeddedLogDepth {
						nested = redactEmbeddedAssignmentsLayer(value[contentStart:contentEnd], depth+1)
					}
					if nested != value[contentStart:contentEnd] {
						if !changed {
							clean.Grow(len(value))
							changed = true
						}
						clean.WriteString(value[last:contentStart])
						clean.WriteString(nested)
						last = contentEnd
					}
					index = afterValue
					continue
				}
			}
			if quotedKey && valueStart > index {
				index = valueStart
			} else {
				index++
			}
			continue
		}
		end := 0
		kind := embeddedLogBareValue
		if status == embeddedLogParsed {
			end, kind, status = embeddedLogAssignmentValueEnd(value, valueStart)
		}
		if status == embeddedLogMalformed {
			end = embeddedLogLineEnd(value, index)
			if !changed {
				clean.Grow(len(value))
				changed = true
			}
			clean.WriteString(value[last:index])
			clean.WriteString("[REDACTED]")
			last = end
			index = end
			continue
		}
		if !sensitiveKey(key) {
			if kind != embeddedLogBareValue {
				contentStart, contentEnd, valid := embeddedLogContainerContent(value, valueStart, end, kind)
				if !valid {
					contentStart = valueStart
					contentEnd = end
				}
				nested := "[REDACTED]"
				if depth < maximumEmbeddedLogDepth {
					nested = redactEmbeddedAssignmentsLayer(value[contentStart:contentEnd], depth+1)
				}
				if nested != value[contentStart:contentEnd] {
					if !changed {
						clean.Grow(len(value))
						changed = true
					}
					clean.WriteString(value[last:contentStart])
					clean.WriteString(nested)
					last = contentEnd
				}
			}
			index = end
			continue
		}
		if safeEmbeddedLogMetric(key, value[valueStart:end], kind) {
			index = end
			continue
		}
		if kind == embeddedLogBareValue {
			end = embeddedLogSensitiveBareValueEnd(value, end)
		}
		if !changed {
			clean.Grow(len(value))
			changed = true
		}
		clean.WriteString(value[last:index])
		clean.WriteString("[REDACTED]")
		last = end
		index = end
	}
	if !changed {
		return value
	}
	clean.WriteString(value[last:])
	return clean.String()
}

func embeddedLogQuotedContent(value string, start int) (int, int, int, bool) {
	marker, markerStatus := embeddedLogQuoteMarkerAt(value, start)
	if markerStatus != embeddedLogParsed {
		return 0, 0, 0, false
	}
	contentEnd, afterValue, endStatus := embeddedLogQuotedEnd(
		value, marker.contentStart, marker.quote, marker.layer, 0,
	)
	if endStatus != embeddedLogParsed {
		return 0, 0, 0, false
	}
	return marker.contentStart, contentEnd, afterValue, true
}

func embeddedLogContainerContent(value string, start, end int, kind embeddedLogValueKind) (int, int, bool) {
	if start < 0 || end < start || end > len(value) {
		return 0, 0, false
	}
	switch kind {
	case embeddedLogQuotedValue:
		contentStart, contentEnd, afterValue, valid := embeddedLogQuotedContent(value, start)
		if !valid || afterValue != end {
			return 0, 0, false
		}
		return contentStart, contentEnd, true
	case embeddedLogCompositeValue:
		if end <= start+1 || value[start] == '{' && value[end-1] != '}' || value[start] == '[' && value[end-1] != ']' {
			return 0, 0, false
		}
		return start + 1, end - 1, true
	default:
		return 0, 0, false
	}
}

func embeddedLogAssignmentAt(value string, start int) (string, int, bool, embeddedLogParseStatus) {
	marker, markerStatus := embeddedLogQuoteMarkerAt(value, start)
	if markerStatus == embeddedLogMalformed {
		return "", 0, false, embeddedLogMalformed
	}
	if markerStatus == embeddedLogParsed {
		contentEnd, afterKey, endStatus := embeddedLogQuotedEnd(
			value, marker.contentStart, marker.quote, marker.layer, maximumEmbeddedLogKeyBytes,
		)
		if endStatus != embeddedLogParsed {
			if endStatus == embeddedLogMalformed {
				return "", 0, true, embeddedLogMalformed
			}
			if embeddedLogPossiblySensitiveMalformedKey(value, marker.contentStart) {
				return "", 0, true, embeddedLogMalformed
			}
			return "", marker.contentStart, true, embeddedLogNoMatch
		}
		cursor := skipHorizontalLogSpace(value, afterKey)
		if cursor >= len(value) || value[cursor] != ':' && value[cursor] != '=' {
			return "", marker.contentStart, true, embeddedLogNoMatch
		}
		key, valid := decodeEmbeddedLogKey(value[marker.contentStart:contentEnd], marker.quote, marker.layer)
		if !valid || compactLogKey(key) == "" {
			return "", 0, true, embeddedLogMalformed
		}
		return key, skipHorizontalLogSpace(value, cursor+1), true, embeddedLogParsed
	}

	if !embeddedLogBareKeyStart(value, start) {
		return "", 0, false, embeddedLogNoMatch
	}
	var keyBuilder strings.Builder
	keyBuilder.Grow(maximumEmbeddedLogKeyBytes)
	cursor := start
	hasWord := false
	for cursor < len(value) {
		current, size := utf8.DecodeRuneInString(value[cursor:])
		if cursor-start+size > maximumEmbeddedLogKeyBytes {
			return "", 0, false, embeddedLogMalformed
		}
		if current == utf8.RuneError && size == 1 {
			keyBuilder.WriteByte('_')
			cursor++
			continue
		}
		if current == ':' || current == '=' {
			break
		}
		if current == '\r' || current == '\n' {
			break
		}
		if unicode.IsControl(current) && current != '\t' {
			keyBuilder.WriteByte('_')
			cursor += size
			continue
		}
		if !embeddedLogBareKeyRune(current) {
			break
		}
		hasWord = hasWord || logKeyWordRune(current)
		keyBuilder.WriteString(value[cursor : cursor+size])
		cursor += size
	}
	if cursor == start || !hasWord || cursor >= len(value) || value[cursor] != ':' && value[cursor] != '=' {
		return "", 0, false, embeddedLogNoMatch
	}
	key := strings.TrimSpace(keyBuilder.String())
	classification := classifyLogKey(key)
	if !classification.valid {
		return "", 0, false, embeddedLogMalformed
	}
	if classification.canonical == "" {
		return "", 0, false, embeddedLogNoMatch
	}
	return key, skipHorizontalLogSpace(value, cursor+1), false, embeddedLogParsed
}

func embeddedLogBareKeyStart(value string, index int) bool {
	if index < 0 || index >= len(value) || !utf8.RuneStart(value[index]) {
		return false
	}
	if index > 0 {
		if !embeddedLogEncodedWhitespaceEndsAt(value, index) {
			previous, size := utf8.DecodeLastRuneInString(value[:index])
			invalid := previous == utf8.RuneError && size == 1
			if !invalid && !unicode.IsSpace(previous) && !unicode.IsControl(previous) && previous != '\\' &&
				!embeddedLogStructuralBoundaryRune(previous) {
				return false
			}
		}
	}
	current, size := utf8.DecodeRuneInString(value[index:])
	if value[index] == '\\' && index+1 < len(value) && value[index+1] == 'u' {
		decoded, _, valid := decodeEmbeddedLogUnicode(value, index)
		if valid {
			current = decoded
		} else {
			current = 'x'
		}
	} else if current == utf8.RuneError && size == 1 {
		return true
	}
	if unicode.IsSpace(current) {
		return false
	}
	return embeddedLogBareKeyRune(current)
}

func embeddedLogBareKeyRune(value rune) bool {
	if logKeyWordRune(value) || value == ' ' || value == '\t' || value == '\\' {
		return true
	}
	if unicode.IsControl(value) || value == ':' || value == '=' {
		return false
	}
	return !embeddedLogStructuralBoundaryRune(value)
}

func embeddedLogStructuralBoundaryRune(value rune) bool {
	switch value {
	case ':', '=', '{', '}', '[', ']', '(', ')', '<', '>', ',', ';', '"', '\'':
		return true
	default:
		return false
	}
}

func embeddedLogQuoteMarkerAt(value string, start int) (embeddedLogQuoteMarker, embeddedLogParseStatus) {
	if start < 0 || start >= len(value) {
		return embeddedLogQuoteMarker{}, embeddedLogNoMatch
	}
	if value[start] == '"' || value[start] == '\'' {
		if start > 0 && value[start-1] == '\\' {
			return embeddedLogQuoteMarker{}, embeddedLogNoMatch
		}
		return embeddedLogQuoteMarker{quote: value[start], contentStart: start + 1}, embeddedLogParsed
	}
	if value[start] != '\\' {
		return embeddedLogQuoteMarker{}, embeddedLogNoMatch
	}
	cursor := start
	for cursor < len(value) && value[cursor] == '\\' && cursor-start < 8 {
		cursor++
	}
	if cursor == len(value) || value[cursor] == '\\' || value[cursor] != '"' && value[cursor] != '\'' {
		return embeddedLogQuoteMarker{}, embeddedLogNoMatch
	}
	slashes := cursor - start
	layer := -1
	switch slashes {
	case 1:
		layer = 1
	case 3:
		layer = 2
	}
	if layer < 0 || layer > maximumEmbeddedLogQuoteLayer {
		return embeddedLogQuoteMarker{}, embeddedLogMalformed
	}
	return embeddedLogQuoteMarker{quote: value[cursor], layer: layer, contentStart: cursor + 1}, embeddedLogParsed
}

func embeddedLogQuotedEnd(value string, contentStart int, quote byte, layer, keyLimit int) (int, int, embeddedLogParseStatus) {
	for index := contentStart; index < len(value); index++ {
		if keyLimit > 0 && index-contentStart > keyLimit+(1<<maximumEmbeddedLogQuoteLayer)-1 {
			return 0, 0, embeddedLogMalformed
		}
		if value[index] == '\r' || value[index] == '\n' {
			return 0, 0, embeddedLogNoMatch
		}
		if value[index] != quote || !embeddedLogQuoteCloses(value, contentStart, index, layer) {
			continue
		}
		markerBytes := (1 << layer) - 1
		contentEnd := index - markerBytes
		if keyLimit > 0 && contentEnd-contentStart > keyLimit {
			return 0, 0, embeddedLogMalformed
		}
		return contentEnd, index + 1, embeddedLogParsed
	}
	return 0, 0, embeddedLogNoMatch
}

func embeddedLogQuoteCloses(value string, contentStart, quoteIndex, layer int) bool {
	slashes := 0
	for index := quoteIndex - 1; index >= contentStart && value[index] == '\\'; index-- {
		slashes++
	}
	modulus := 1 << (layer + 1)
	return slashes%modulus == (1<<layer)-1
}

func decodeEmbeddedLogKey(value string, quote byte, layer int) (string, bool) {
	if len(value) > maximumEmbeddedLogKeyBytes || layer < 0 || layer > maximumEmbeddedLogQuoteLayer {
		return "", false
	}
	decoded := value
	for pass := 0; pass <= layer; pass++ {
		var valid bool
		decoded, valid = decodeEmbeddedLogStringLayer(decoded, quote)
		if !valid || len(decoded) > maximumEmbeddedLogKeyBytes {
			return "", false
		}
	}
	return decoded, utf8.ValidString(decoded)
}

func decodeEmbeddedLogStringLayer(value string, quote byte) (string, bool) {
	if !utf8.ValidString(value) {
		return "", false
	}
	var decoded strings.Builder
	decoded.Grow(len(value))
	for index := 0; index < len(value); {
		current := value[index]
		if current != '\\' {
			if current < 0x20 {
				return "", false
			}
			decoded.WriteByte(current)
			index++
			continue
		}
		if index+1 >= len(value) {
			return "", false
		}
		next := value[index+1]
		switch next {
		case '"', '\'', '\\', '/':
			if next == '\'' && quote != '\'' {
				return "", false
			}
			decoded.WriteByte(next)
			index += 2
		case 'b':
			decoded.WriteByte('\b')
			index += 2
		case 'f':
			decoded.WriteByte('\f')
			index += 2
		case 'n':
			decoded.WriteByte('\n')
			index += 2
		case 'r':
			decoded.WriteByte('\r')
			index += 2
		case 't':
			decoded.WriteByte('\t')
			index += 2
		case 'u':
			decodedRune, nextIndex, valid := decodeEmbeddedLogUnicode(value, index)
			if !valid {
				return "", false
			}
			decoded.WriteRune(decodedRune)
			index = nextIndex
		default:
			return "", false
		}
	}
	return decoded.String(), true
}

func decodeEmbeddedLogUnicode(value string, start int) (rune, int, bool) {
	first, valid := embeddedLogHexQuad(value, start+2)
	if !valid {
		return 0, 0, false
	}
	next := start + 6
	if first >= 0xD800 && first <= 0xDBFF {
		if next+6 > len(value) || value[next] != '\\' || value[next+1] != 'u' {
			return 0, 0, false
		}
		second, secondValid := embeddedLogHexQuad(value, next+2)
		if !secondValid || second < 0xDC00 || second > 0xDFFF {
			return 0, 0, false
		}
		decoded := 0x10000 + (rune(first)-0xD800)*0x400 + rune(second) - 0xDC00
		return decoded, next + 6, true
	}
	if first >= 0xDC00 && first <= 0xDFFF {
		return 0, 0, false
	}
	return rune(first), next, true
}

func embeddedLogHexQuad(value string, start int) (uint16, bool) {
	if start < 0 || start+4 > len(value) {
		return 0, false
	}
	var decoded uint16
	for index := start; index < start+4; index++ {
		decoded <<= 4
		switch current := value[index]; {
		case current >= '0' && current <= '9':
			decoded += uint16(current - '0')
		case current >= 'a' && current <= 'f':
			decoded += uint16(current-'a') + 10
		case current >= 'A' && current <= 'F':
			decoded += uint16(current-'A') + 10
		default:
			return 0, false
		}
	}
	return decoded, true
}

func embeddedLogPossiblySensitiveMalformedKey(value string, start int) bool {
	end := embeddedLogLineEnd(value, start)
	if end-start > maximumEmbeddedLogKeyBytes {
		end = start + maximumEmbeddedLogKeyBytes
	}
	candidate, valid := decodeLogKeyUnicodeEscapes(strings.TrimSpace(value[start:end]))
	if !valid {
		return true
	}
	if separator := strings.IndexAny(candidate, ":="); separator >= 0 {
		candidate = candidate[:separator]
	}
	candidate = strings.TrimSpace(candidate)
	return candidate != "" && classifyLogKey(candidate).sensitive
}

func embeddedLogAssignmentValueEnd(value string, start int) (int, embeddedLogValueKind, embeddedLogParseStatus) {
	if start >= len(value) || value[start] == '\r' || value[start] == '\n' {
		return 0, embeddedLogBareValue, embeddedLogMalformed
	}
	marker, markerStatus := embeddedLogQuoteMarkerAt(value, start)
	if markerStatus == embeddedLogMalformed {
		return 0, embeddedLogQuotedValue, embeddedLogMalformed
	}
	if markerStatus == embeddedLogParsed {
		_, end, endStatus := embeddedLogQuotedEnd(value, marker.contentStart, marker.quote, marker.layer, 0)
		if endStatus != embeddedLogParsed {
			return 0, embeddedLogQuotedValue, embeddedLogMalformed
		}
		return validatedEmbeddedLogValueEnd(value, end, embeddedLogQuotedValue)
	}
	if value[start] == '{' || value[start] == '[' {
		end, status := embeddedLogCompositeEnd(value, start)
		if status != embeddedLogParsed {
			return 0, embeddedLogCompositeValue, embeddedLogMalformed
		}
		return validatedEmbeddedLogValueEnd(value, end, embeddedLogCompositeValue)
	}
	end := start
	for end < len(value) && !strings.ContainsRune(":= \t,;})]>\r\n", rune(value[end])) {
		end++
	}
	if end == start {
		return 0, embeddedLogBareValue, embeddedLogMalformed
	}
	return validatedEmbeddedLogValueEnd(value, end, embeddedLogBareValue)
}

func validatedEmbeddedLogValueEnd(value string, end int, kind embeddedLogValueKind) (int, embeddedLogValueKind, embeddedLogParseStatus) {
	if end < 0 || end > len(value) {
		return 0, kind, embeddedLogMalformed
	}
	if end == len(value) || strings.ContainsRune(" \t,;})]>\r\n", rune(value[end])) ||
		embeddedLogEncodedWhitespaceAt(value, end) || embeddedLogQuoteBoundaryAt(value, end) ||
		embeddedLogRedactionMarkerBoundaryAt(value, end) {
		return end, kind, embeddedLogParsed
	}
	return 0, kind, embeddedLogMalformed
}

func embeddedLogRedactionMarkerBoundaryAt(value string, start int) bool {
	return start >= 0 && start < len(value) &&
		(strings.HasPrefix(value[start:], "[REDACTED]") || strings.HasPrefix(value[start:], "[REDACTED-URL]"))
}

func embeddedLogEncodedWhitespaceAt(value string, start int) bool {
	_, found := embeddedLogEncodedWhitespaceEndAt(value, start)
	return found
}

func embeddedLogEncodedWhitespaceEndAt(value string, start int) (int, bool) {
	return embeddedLogEncodedWhitespaceEndAtLayers(value, start, maximumLogURLDecodeLayers)
}

func embeddedLogEncodedWhitespaceEndAtLayers(value string, start, maximumLayers int) (int, bool) {
	if start < 0 || start >= len(value) || value[start] != '\\' || maximumLayers <= 0 {
		return 0, false
	}
	end := min(len(value), start+maximumLogURLBoundaryLookaheadBytes)
	view := newLogURLView(value[start:end])
	for layer := 0; layer < maximumLayers; layer++ {
		decoded, changed := decodeLogURLViewLayer(view)
		if !changed {
			return 0, false
		}
		view = decoded
		current, size := utf8.DecodeRuneInString(view.text)
		if current != utf8.RuneError || size != 1 {
			if unicode.IsSpace(current) {
				return start + view.spans[0].end, true
			}
		}
	}
	return 0, false
}

func embeddedLogEncodedQuoteEndAt(value string, start, maximumLayers int) (int, bool) {
	if maximumLayers <= 0 {
		return 0, false
	}
	marker, status := embeddedLogQuoteMarkerAt(value, start)
	if status != embeddedLogParsed || marker.layer <= 0 || marker.layer > maximumLayers {
		return 0, false
	}
	return marker.contentStart, true
}

func embeddedLogEncodedWhitespaceEndsAt(value string, end int) bool {
	start := max(0, end-maximumLogURLBoundaryLookaheadBytes)
	for ; start < end; start++ {
		if value[start] != '\\' {
			continue
		}
		boundaryEnd, found := embeddedLogEncodedWhitespaceEndAt(value, start)
		if found && boundaryEnd == end {
			return true
		}
	}
	return false
}

func embeddedLogQuoteBoundaryAt(value string, start int) bool {
	_, status := embeddedLogQuoteMarkerAt(value, start)
	return status == embeddedLogParsed
}

func embeddedLogCompositeEnd(value string, start int) (int, embeddedLogParseStatus) {
	var expected [maximumEmbeddedLogDepth]byte
	depth := 1
	if value[start] == '{' {
		expected[0] = '}'
	} else {
		expected[0] = ']'
	}
	for index := start + 1; index < len(value); {
		if value[index] == '\r' || value[index] == '\n' {
			return 0, embeddedLogMalformed
		}
		marker, markerStatus := embeddedLogQuoteMarkerAt(value, index)
		if markerStatus == embeddedLogMalformed {
			return 0, embeddedLogMalformed
		}
		if markerStatus == embeddedLogParsed {
			_, end, endStatus := embeddedLogQuotedEnd(value, marker.contentStart, marker.quote, marker.layer, 0)
			if endStatus != embeddedLogParsed {
				return 0, embeddedLogMalformed
			}
			index = end
			continue
		}
		switch value[index] {
		case '{', '[':
			if depth >= maximumEmbeddedLogDepth {
				return 0, embeddedLogMalformed
			}
			if value[index] == '{' {
				expected[depth] = '}'
			} else {
				expected[depth] = ']'
			}
			depth++
		case '}', ']':
			if depth == 0 || value[index] != expected[depth-1] {
				return 0, embeddedLogMalformed
			}
			depth--
			if depth == 0 {
				return index + 1, embeddedLogParsed
			}
		}
		index++
	}
	return 0, embeddedLogMalformed
}

func safeEmbeddedLogMetric(key, value string, kind embeddedLogValueKind) bool {
	classification := classifyLogKey(key)
	if classification.canonical != "attempt" && !classification.lengthMetric {
		return false
	}
	return kind == embeddedLogBareValue && strictEmbeddedLogNumber(value)
}

func strictEmbeddedLogNumber(value string) bool {
	if value == "" {
		return false
	}
	index := 0
	if value[index] == '-' {
		index++
		if index == len(value) {
			return false
		}
	}
	if value[index] == '0' {
		index++
	} else {
		if value[index] < '1' || value[index] > '9' {
			return false
		}
		for index < len(value) && value[index] >= '0' && value[index] <= '9' {
			index++
		}
	}
	if index < len(value) && value[index] == '.' {
		index++
		fractionStart := index
		for index < len(value) && value[index] >= '0' && value[index] <= '9' {
			index++
		}
		if index == fractionStart {
			return false
		}
	}
	if index < len(value) && (value[index] == 'e' || value[index] == 'E') {
		index++
		if index < len(value) && (value[index] == '+' || value[index] == '-') {
			index++
		}
		exponentStart := index
		for index < len(value) && value[index] >= '0' && value[index] <= '9' {
			index++
		}
		if index == exponentStart {
			return false
		}
	}
	return index == len(value)
}

func embeddedLogSensitiveBareValueEnd(value string, tokenEnd int) int {
	if tokenEnd < len(value) && strings.ContainsRune(",;})]>", rune(value[tokenEnd])) {
		return tokenEnd
	}
	lineEnd := embeddedLogLineEnd(value, tokenEnd)
	for cursor := tokenEnd; cursor < lineEnd; cursor++ {
		if value[cursor] != ' ' && value[cursor] != '\t' {
			continue
		}
		candidate := skipHorizontalLogSpace(value, cursor)
		if candidate >= lineEnd {
			return lineEnd
		}
		if strings.ContainsRune(",;})]>", rune(value[candidate])) {
			return cursor
		}
		key, _, _, status := embeddedLogAssignmentAt(value, candidate)
		if status == embeddedLogMalformed {
			return lineEnd
		}
		if status == embeddedLogParsed && (sensitiveKey(key) || safeEmbeddedAdjacentKey(key)) {
			return candidate
		}
	}
	return lineEnd
}

func safeEmbeddedAdjacentKey(key string) bool {
	classification := classifyLogKey(key)
	if !classification.valid {
		return false
	}
	switch classification.canonical {
	case "status", "statuscode", "errorcode", "code", "ordinal", "revision", "count", "roomcount":
		return true
	default:
		return false
	}
}

func embeddedLogLineEnd(value string, start int) int {
	for start < len(value) && value[start] != '\r' && value[start] != '\n' {
		start++
	}
	return start
}

func skipHorizontalLogSpace(value string, index int) int {
	for index < len(value) && (value[index] == ' ' || value[index] == '\t') {
		index++
	}
	return index
}

const (
	maximumLogURLDecodeLayers           = 2
	maximumLogURLBoundaryLookaheadBytes = 16
)

type logURLSpan struct {
	start int
	end   int
}

type logURLView struct {
	text  string
	spans []logURLSpan
	layer int
}

type logURLRange struct {
	start int
	end   int
}

func redactLogURLs(value string) string {
	return redactLogURLsWithStats(value, nil)
}

type logURLScanStats struct {
	decodedViewBytes       int
	boundaryProbes         int
	boundaryLookaheadBytes int
	rawRescanBytes         int
}

func redactLogURLsWithStats(value string, stats *logURLScanStats) string {
	views := []logURLView{newLogURLView(value)}
	for layer := 0; layer < maximumLogURLDecodeLayers; layer++ {
		if stats != nil {
			stats.decodedViewBytes += len(views[len(views)-1].text)
		}
		decoded, changed := decodeLogURLViewLayer(views[len(views)-1])
		if !changed {
			break
		}
		views = append(views, decoded)
	}

	var ranges []logURLRange
	for index := len(views) - 1; index >= 0; index-- {
		candidates := logURLRangesInView(views[index])
		candidates = normalizeLogURLRanges(views[0], candidates, stats)
		candidates = logURLRangesWithoutOverlap(candidates, ranges)
		ranges = mergeLogURLRanges(ranges, candidates)
	}
	ranges = mergeLogURLRanges(ranges, logURLMalformedRangesInRawView(views[0], ranges))
	if len(ranges) == 0 {
		return value
	}

	var clean strings.Builder
	clean.Grow(len(value))
	last := 0
	for _, item := range ranges {
		clean.WriteString(value[last:item.start])
		clean.WriteString("[REDACTED-URL]")
		last = item.end
	}
	clean.WriteString(value[last:])
	return clean.String()
}

func newLogURLView(value string) logURLView {
	spans := make([]logURLSpan, len(value))
	for index := range spans {
		spans[index] = logURLSpan{start: index, end: index + 1}
	}
	return logURLView{text: value, spans: spans, layer: 0}
}

func decodeLogURLViewLayer(view logURLView) (logURLView, bool) {
	var decoded strings.Builder
	decoded.Grow(len(view.text))
	spans := make([]logURLSpan, 0, len(view.spans))
	changed := false
	for index := 0; index < len(view.text); {
		item, next, valid := decodeLogURLJSONEscape(view.text, index)
		if !valid {
			decoded.WriteByte(view.text[index])
			spans = append(spans, view.spans[index])
			index++
			continue
		}
		changed = true
		decoded.WriteString(item)
		span := logURLSpan{start: view.spans[index].start, end: view.spans[next-1].end}
		for range []byte(item) {
			spans = append(spans, span)
		}
		index = next
	}
	return logURLView{text: decoded.String(), spans: spans, layer: view.layer + 1}, changed
}

func decodeLogURLJSONEscape(value string, start int) (string, int, bool) {
	if start < 0 || start+1 >= len(value) || value[start] != '\\' {
		return "", 0, false
	}
	switch value[start+1] {
	case '"', '\'', '\\', '/':
		return value[start+1 : start+2], start + 2, true
	case 'b':
		return "\b", start + 2, true
	case 'f':
		return "\f", start + 2, true
	case 'n':
		return "\n", start + 2, true
	case 'r':
		return "\r", start + 2, true
	case 't':
		return "\t", start + 2, true
	case 'u':
		decoded, next, valid := decodeEmbeddedLogUnicode(value, start)
		if !valid {
			return "", 0, false
		}
		return string(decoded), next, true
	default:
		return "", 0, false
	}
}

func logURLRangesInView(view logURLView) []logURLRange {
	var ranges []logURLRange
	for index := 0; index < len(view.text); {
		end, found := logURLEndAt(view, index)
		if !found {
			index++
			continue
		}
		if end > index && end <= len(view.spans) {
			ranges = append(ranges, logURLRange{
				start: view.spans[index].start,
				end:   view.spans[end-1].end,
			})
		}
		index = end
	}
	return ranges
}

func logURLEndAt(view logURLView, start int) (int, bool) {
	afterScheme, found := logURLSchemeEndAt(view.text, start)
	if !found || afterScheme >= len(view.text) {
		return 0, false
	}
	if view.text[afterScheme] != '/' || afterScheme+1 >= len(view.text) || view.text[afterScheme+1] != '/' {
		return 0, false
	}
	afterPrefix := afterScheme + 2
	end := logURLTokenEnd(view, afterPrefix)
	return end, end > afterPrefix
}

func logURLMalformedRangesInRawView(view logURLView, valid []logURLRange) []logURLRange {
	var ranges []logURLRange
	validIndex := 0
	for index := 0; index < len(view.text); {
		afterScheme, found := logURLSchemeEndAt(view.text, index)
		if !found || !logURLMalformedLocatorCandidateAt(view.text, afterScheme) {
			index++
			continue
		}
		start := view.spans[index].start
		for validIndex < len(valid) && valid[validIndex].start < start {
			validIndex++
		}
		if validIndex < len(valid) && valid[validIndex].start == start {
			index = afterScheme
			continue
		}

		// Encoded slash, mixed slash/backslash, or repeated backslash runs are
		// unmistakable malformed network-locator candidates. If no bounded
		// decode view produced a real // prefix, redact the complete raw token
		// rather than risk leaving a host suffix after a partial view match.
		boundaryFloor := logURLMalformedLocatorPrefixEndAt(view.text, afterScheme)
		end := logURLTokenEndWithBoundaryFloor(view, afterScheme, boundaryFloor)
		if end <= afterScheme {
			index++
			continue
		}
		ranges = append(ranges, logURLRange{start: start, end: view.spans[end-1].end})
		index = end
	}
	return ranges
}

func logURLMalformedLocatorCandidateAt(value string, start int) bool {
	if start < 0 || start >= len(value) {
		return false
	}
	if value[start] == '/' {
		return start+1 < len(value) && value[start+1] == '\\'
	}
	if value[start] != '\\' {
		return false
	}
	runEnd := start
	for runEnd < len(value) && value[runEnd] == '\\' {
		runEnd++
	}
	if runEnd-start > 1 {
		return true
	}
	decoded, _, valid := decodeLogURLJSONEscape(value, start)
	return valid && decoded == "/"
}

func logURLMalformedLocatorPrefixEndAt(value string, start int) int {
	cursor := start
	for cursor < len(value) {
		if value[cursor] == '/' {
			cursor++
			continue
		}
		if value[cursor] != '\\' {
			break
		}
		runEnd := cursor
		for runEnd < len(value) && value[runEnd] == '\\' {
			runEnd++
		}
		if runEnd-cursor > 1 {
			cursor = runEnd
			continue
		}
		decoded, next, valid := decodeLogURLJSONEscape(value, cursor)
		if !valid || decoded != "/" {
			break
		}
		cursor = next
	}
	return cursor
}

func normalizeLogURLRanges(raw logURLView, ranges []logURLRange, stats *logURLScanStats) []logURLRange {
	normalized := ranges[:0]
	for _, candidate := range ranges {
		item := candidate
		if len(normalized) > 0 && item.start < normalized[len(normalized)-1].end {
			current := &normalized[len(normalized)-1]
			if item.end > current.end {
				current.end = item.end
				normalizeLogURLRangeEnd(raw, current, stats)
			}
			continue
		}
		normalizeLogURLRangeEnd(raw, &item, stats)
		normalized = append(normalized, item)
	}
	return normalized
}

func normalizeLogURLRangeEnd(raw logURLView, item *logURLRange, stats *logURLScanStats) {
	if item.start < 0 || item.end <= item.start || item.end >= len(raw.text) ||
		logURLRawBoundarySafeWithStats(raw.text, item.end, stats) {
		return
	}
	rawEnd, scanned := logURLTokenEndScanned(raw, item.end)
	if stats != nil {
		stats.rawRescanBytes += scanned
	}
	if rawEnd > item.end {
		item.end = rawEnd
	}
}

func logURLRawBoundarySafe(value string, start int) bool {
	return logURLRawBoundarySafeWithStats(value, start, nil)
}

func logURLRawBoundarySafeWithStats(value string, start int, stats *logURLScanStats) bool {
	if start < 0 || start >= len(value) {
		return start == len(value)
	}
	current, size := utf8.DecodeRuneInString(value[start:])
	if current != utf8.RuneError || size != 1 {
		if unicode.IsSpace(current) || unicode.IsControl(current) || strings.ContainsRune("\"'<> ", current) {
			return true
		}
	}
	if strings.ContainsRune(",;)]}", current) && logURLStructuralTailAt(value, start) {
		return true
	}
	if value[start] != '\\' {
		return false
	}

	probeEnd := min(len(value), start+maximumLogURLBoundaryLookaheadBytes)
	if stats != nil {
		stats.boundaryProbes++
		stats.boundaryLookaheadBytes += probeEnd - start
	}
	view := newLogURLView(value[start:probeEnd])
	for layer := 0; layer < maximumLogURLDecodeLayers; layer++ {
		decoded, changed := decodeLogURLViewLayer(view)
		if !changed || len(decoded.text) == 0 || len(decoded.spans) == 0 {
			return false
		}
		view = decoded
		boundary, boundarySize := utf8.DecodeRuneInString(view.text)
		if boundary == utf8.RuneError && boundarySize == 1 ||
			!unicode.IsSpace(boundary) && !unicode.IsControl(boundary) && !strings.ContainsRune("\"'<> ", boundary) {
			continue
		}
		return true
	}
	return false
}

func logURLStructuralTailAt(value string, start int) bool {
	_, matched, _ := logURLStructuralTailScan(value, start)
	return matched
}

func logURLStructuralTailScan(value string, start int) (int, bool, int) {
	if start < 0 || start >= len(value) || !strings.ContainsRune(",;)]}", rune(value[start])) {
		return start, false, 0
	}
	contiguousEnd := start + 1
	scanned := 1
	for contiguousEnd < len(value) && strings.ContainsRune(")]}", rune(value[contiguousEnd])) {
		contiguousEnd++
		scanned++
	}
	cursor := start + 1
	for {
		beforeSpace := cursor
		cursor = skipHorizontalLogSpace(value, cursor)
		scanned += cursor - beforeSpace
		if cursor >= len(value) || !strings.ContainsRune(")]}", rune(value[cursor])) {
			break
		}
		cursor++
		scanned++
	}
	key, valueStart, _, status := embeddedLogAssignmentAt(value, cursor)
	if status != embeddedLogParsed || !sensitiveKey(key) && !safeEmbeddedAdjacentKey(key) {
		return contiguousEnd, false, scanned
	}
	_, _, status = embeddedLogAssignmentValueEnd(value, valueStart)
	return contiguousEnd, status == embeddedLogParsed, scanned
}

func logURLSchemeEndAt(value string, start int) (int, bool) {
	if start < 0 || start >= len(value) || start > 0 && logURLWordByte(value[start-1]) {
		return 0, false
	}
	for _, scheme := range logURLSchemes {
		end := start + len(scheme)
		if end < len(value) && value[end] == ':' && strings.EqualFold(value[start:end], scheme) {
			return end + 1, true
		}
	}
	return 0, false
}

func logURLWordByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || value == '_'
}

func logURLTokenEnd(view logURLView, start int) int {
	end, _ := logURLTokenEndScannedFrom(view, start, start)
	return end
}

func logURLTokenEndScanned(view logURLView, start int) (int, int) {
	return logURLTokenEndScannedFrom(view, start, start)
}

func logURLTokenEndWithBoundaryFloor(view logURLView, start, boundaryFloor int) int {
	end, _ := logURLTokenEndScannedFrom(view, start, boundaryFloor)
	return end
}

func logURLTokenEndScannedFrom(view logURLView, start, boundaryFloor int) (int, int) {
	if boundaryFloor < start {
		boundaryFloor = start
	}
	end := start
	scanned := 0
	for end < len(view.text) {
		current, size := utf8.DecodeRuneInString(view.text[end:])
		scanned += size
		if current != utf8.RuneError || size != 1 {
			if unicode.IsSpace(current) || unicode.IsControl(current) || strings.ContainsRune("\"'<> ", current) {
				break
			}
		}
		if size == 1 && current == '\\' && end >= boundaryFloor &&
			view.layer < maximumLogURLDecodeLayers {
			remainingLayers := maximumLogURLDecodeLayers - view.layer
			if _, found := embeddedLogEncodedWhitespaceEndAtLayers(view.text, end, remainingLayers); found {
				break
			}
			if _, found := embeddedLogEncodedQuoteEndAt(view.text, end, remainingLayers); found {
				break
			}
		}
		if size == 1 && strings.ContainsRune(",;)]}", current) && view.spans[end].end-view.spans[end].start == 1 {
			skipEnd, structuralTail, tailScanned := logURLStructuralTailScan(view.text, end)
			scanned += tailScanned
			if structuralTail {
				break
			}
			if strings.ContainsRune(")]}", current) && skipEnd > end+1 {
				end = skipEnd
				continue
			}
		}
		end += size
	}
	return end, scanned
}

func logURLRangesWithoutOverlap(candidates, preferred []logURLRange) []logURLRange {
	if len(candidates) == 0 || len(preferred) == 0 {
		return candidates
	}
	clean := make([]logURLRange, 0, len(candidates))
	preferredIndex := 0
	for _, candidate := range candidates {
		for preferredIndex < len(preferred) && preferred[preferredIndex].end <= candidate.start {
			preferredIndex++
		}
		if preferredIndex < len(preferred) && preferred[preferredIndex].start < candidate.end {
			continue
		}
		clean = append(clean, candidate)
	}
	return clean
}

func mergeLogURLRanges(left, right []logURLRange) []logURLRange {
	merged := make([]logURLRange, 0, len(left)+len(right))
	leftIndex := 0
	rightIndex := 0
	appendRange := func(item logURLRange) {
		if len(merged) > 0 && item.start < merged[len(merged)-1].end {
			if item.end > merged[len(merged)-1].end {
				merged[len(merged)-1].end = item.end
			}
			return
		}
		merged = append(merged, item)
	}
	for leftIndex < len(left) || rightIndex < len(right) {
		if rightIndex >= len(right) || leftIndex < len(left) && left[leftIndex].start <= right[rightIndex].start {
			appendRange(left[leftIndex])
			leftIndex++
		} else {
			appendRange(right[rightIndex])
			rightIndex++
		}
	}
	return merged
}

func safeLogText(value string, allowLineSeparators bool) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, current := range value {
		if !unicode.IsControl(current) || current == '\t' {
			continue
		}
		if allowLineSeparators && (current == '\r' || current == '\n') {
			continue
		}
		return false
	}
	return true
}

func redactUnsafeLogLines(value string) string {
	if safeLogText(value, true) {
		return value
	}
	var clean strings.Builder
	clean.Grow(len(value))
	for lineStart := 0; lineStart < len(value); {
		lineEnd := lineStart
		for lineEnd < len(value) && value[lineEnd] != '\r' && value[lineEnd] != '\n' {
			lineEnd++
		}
		line := value[lineStart:lineEnd]
		if safeLogText(line, false) {
			clean.WriteString(line)
		} else {
			clean.WriteString("[REDACTED]")
		}
		separatorEnd := lineEnd
		if separatorEnd < len(value) {
			separatorEnd++
			if value[lineEnd] == '\r' && separatorEnd < len(value) && value[separatorEnd] == '\n' {
				separatorEnd++
			}
			clean.WriteString(value[lineEnd:separatorEnd])
		}
		lineStart = separatorEnd
	}
	return clean.String()
}

func redactString(value string) string {
	value = redactUnsafeLogLines(value)
	value = redactLogURLs(value)
	return redactEmbeddedAssignments(value)
}

func removeExpiredLogs(logsDir string, now time.Time, retentionDays int) error {
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		return fmt.Errorf("list diagnostics logs: %w", err)
	}
	localNow := now.Local()
	localMidnight := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, localNow.Location())
	cutoff := localMidnight.AddDate(0, 0, -retentionDays)
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		match := logFilePattern.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		date, err := time.ParseInLocation("2006-01-02", match[1], now.Location())
		if err != nil || !date.Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(logsDir, entry.Name())); err != nil {
			return fmt.Errorf("remove expired diagnostics log: %w", err)
		}
	}
	return nil
}

func (l *FileLogger) Sync() error {
	if l == nil || l.file == nil {
		return nil
	}
	return l.file.Sync()
}

func (l *FileLogger) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	return l.file.Close()
}
