package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRedactingHandlerRemovesSensitiveKeysValuesAndURLs(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&output, nil)).With(
		"component", "storage",
		"cookie", "session=secret",
	)
	logger.InfoContext(context.Background(),
		"request failed at https://media.invalid/live.flv?token=secret",
		"error_code", "STREAM_EXPIRED",
		"correlation_id", "startup",
		"err", errors.New("upstream https://secret.invalid/live?signature=hidden"),
		"details", slog.GroupValue(
			slog.String("safe", "value"),
			slog.String("signed_url", "https://media.invalid/secret"),
			slog.String("message", "msToken=hidden"),
		),
	)

	text := output.String()
	for _, forbidden := range []string{"session=secret", "media.invalid", "secret.invalid", "token=secret", "hidden", "signed_url", "cookie"} {
		if strings.Contains(strings.ToLower(text), strings.ToLower(forbidden)) {
			t.Fatalf("log contains forbidden value %q: %s", forbidden, text)
		}
	}
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	for key, want := range map[string]string{
		"component":      "storage",
		"error_code":     "STREAM_EXPIRED",
		"correlation_id": "startup",
	} {
		if got := record[key]; got != want {
			t.Fatalf("record[%q] = %#v, want %q", key, got, want)
		}
	}
}

func TestRedactTextRemovesQuotedAndEscapedJSONAssignmentsWithoutDroppingSafeFields(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		forbidden []string
		preserved []string
	}{
		{
			name:  "plain JSON with commas and an escaped quote",
			input: `prefix {"room_id":"private-room","title":"Private, \"Creator\" Name","status":503,"message":"safe"} suffix`,
			forbidden: []string{
				"private-room", "Private", "Creator", "room_id", "title",
			},
			preserved: []string{`"status":503`, `"message":"safe"`, "prefix", "suffix"},
		},
		{
			name:  "escaped JSON with an internally escaped quote",
			input: `prefix {\"room_id\":\"private-room\",\"title\":\"Private \\\"Creator\\\" Name\",\"status\":204,\"message\":\"safe\"} suffix`,
			forbidden: []string{
				"private-room", "Private", "Creator", `\"room_id\"`, `\"title\"`,
			},
			preserved: []string{`\"status\":204`, `\"message\":\"safe\"`, "prefix", "suffix"},
		},
		{
			name:      "single quoted assignment",
			input:     `{'token':'private-token','status':201,'message':'safe'}`,
			forbidden: []string{"private-token", "token"},
			preserved: []string{"'status':201", "'message':'safe'"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clean := RedactText(test.input)
			for _, forbidden := range test.forbidden {
				if strings.Contains(clean, forbidden) {
					t.Fatalf("RedactText() retained %q: %s", forbidden, clean)
				}
			}
			for _, preserved := range test.preserved {
				if !strings.Contains(clean, preserved) {
					t.Fatalf("RedactText() dropped %q: %s", preserved, clean)
				}
			}
			if !strings.Contains(clean, "[REDACTED]") {
				t.Fatalf("RedactText() lacks a redaction marker: %s", clean)
			}
		})
	}

	malformed := RedactText("prefix {\"room_id\":\"private-room\nstatus=503")
	if strings.Contains(malformed, "private-room") ||
		!strings.Contains(malformed, "\nstatus=503") {
		t.Fatalf("malformed quoted assignment was not fail-closed per line: %s", malformed)
	}

	unquoted := RedactText("room_id=private-room status=503 signature_len=64")
	if strings.Contains(unquoted, "private-room") ||
		!strings.Contains(unquoted, "status=503") ||
		!strings.Contains(unquoted, "signature_len=64") {
		t.Fatalf("legacy key=value semantics changed: %s", unquoted)
	}
	safeJSON := RedactText(`{"room_count":2,"signature_len":64,"status":503}`)
	for _, preserved := range []string{`"room_count":2`, `"signature_len":64`, `"status":503`} {
		if !strings.Contains(safeJSON, preserved) {
			t.Fatalf("safe JSON field %q was removed: %s", preserved, safeJSON)
		}
	}
}

func TestRedactTextTokenizerHandlesAliasesEscapesCompositeValuesAndMalformedInput(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		forbidden []string
		preserved []string
	}{
		{
			name:      "plain JSON object",
			input:     `prefix {"authorization":{"scheme":"Bearer","credential":"private-object"},"status":200} suffix`,
			forbidden: []string{"authorization", "private-object", "credential"},
			preserved: []string{`"status":200`, "prefix", "suffix"},
		},
		{
			name:      "single quoted nested array",
			input:     `{'TOKEN':['private-array',{'nested':'private-nested'}],'status':201}`,
			forbidden: []string{"TOKEN", "private-array", "private-nested"},
			preserved: []string{"'status':201"},
		},
		{
			name:      "one escaped layer unicode key and object",
			input:     `{\"room\\u005fID\":{\"nested\":\"private-one\"},\"status\":202}`,
			forbidden: []string{`room\\u005fID`, "private-one"},
			preserved: []string{`\"status\":202`},
		},
		{
			name:      "two escaped layers unicode key and array",
			input:     `{\\\"ROOM\\\\u005fID\\\":[\\\"private-two\\\",{\\\"nested\\\":\\\"private-nested-two\\\"}],\\\"status\\\":203,\\\"message\\\":\\\"safe-two-layer\\\"}`,
			forbidden: []string{"ROOM", "private-two", "private-nested-two"},
			preserved: []string{`\\\"status\\\":203`, `\\\"message\\\":\\\"safe-two-layer\\\"`},
		},
		{
			name:      "separatorless uppercase alias",
			input:     `{"PLATFORMROOMID":"private-uppercase","status":204}`,
			forbidden: []string{"PLATFORMROOMID", "private-uppercase"},
			preserved: []string{`"status":204`},
		},
		{
			name:      "plain unicode escape key",
			input:     `{"room\u005fID":"private-unicode","status":205}`,
			forbidden: []string{`room\u005fID`, "private-unicode"},
			preserved: []string{`"status":205`},
		},
		{
			name:      "quoted authorization with spaces",
			input:     `authorization="Bearer private credential with spaces" status=206`,
			forbidden: []string{"authorization", "private credential", "with spaces"},
			preserved: []string{"status=206"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clean := RedactText(test.input)
			for _, forbidden := range test.forbidden {
				if strings.Contains(clean, forbidden) {
					t.Fatalf("RedactText() retained %q: %s", forbidden, clean)
				}
			}
			for _, preserved := range test.preserved {
				if !strings.Contains(clean, preserved) {
					t.Fatalf("RedactText() dropped %q: %s", preserved, clean)
				}
			}
			if !strings.Contains(clean, "[REDACTED]") {
				t.Fatalf("RedactText() lacks redaction marker: %s", clean)
			}
		})
	}

	for _, malformed := range []string{
		"prefix {\"room_id\":{\"nested\":\"private-truncated\"\nstatus=207",
		`prefix {"room\u00ZZid":"private-invalid-unicode","status":208} suffix`,
		`prefix {"room\u005":"private-truncated-unicode","status":209} suffix`,
	} {
		clean := RedactText(malformed)
		if strings.Contains(clean, "private-") || !strings.Contains(clean, "[REDACTED]") {
			t.Fatalf("malformed assignment did not fail closed: %s", clean)
		}
	}

	for _, test := range []struct {
		input     string
		forbidden []string
		preserved string
	}{
		{
			input:     "title_len=private secret tail\nstatus=510",
			forbidden: []string{"private", "secret", "tail", "title_len"},
			preserved: "\nstatus=510",
		},
		{
			input:     "room_id_len=64oops private-tail\nstatus=511",
			forbidden: []string{"64oops", "private-tail", "room_id_len"},
			preserved: "\nstatus=511",
		},
	} {
		clean := RedactText(test.input)
		for _, forbidden := range test.forbidden {
			if strings.Contains(clean, forbidden) {
				t.Fatalf("invalid sensitive bare value retained %q: %s", forbidden, clean)
			}
		}
		if !strings.Contains(clean, test.preserved) {
			t.Fatalf("line-bounded redaction lost %q: %s", test.preserved, clean)
		}
	}

	for _, test := range []struct {
		name  string
		input string
	}{
		{
			name:  "quoted illegal suffix",
			input: "token=\"private-quoted\"codexprivate-tail\nstatus=512",
		},
		{
			name:  "composite illegal suffix",
			input: "authorization={\"value\":\"private-composite\"}codexprivate-tail\nstatus=512",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			clean := RedactText(test.input)
			if strings.Contains(clean, "private-") || strings.Contains(clean, "codexprivate-tail") ||
				!strings.Contains(clean, "\nstatus=512") {
				t.Fatalf("malformed value suffix did not fail closed per line: %s", clean)
			}
		})
	}

	legalBoundaries := RedactText(`token="private-quoted";status=513 authorization={"value":"private-composite"},code=SAFE`)
	for _, forbidden := range []string{"private-quoted", "private-composite", "token", "authorization"} {
		if strings.Contains(legalBoundaries, forbidden) {
			t.Fatalf("legal boundary retained %q: %s", forbidden, legalBoundaries)
		}
	}
	for _, preserved := range []string{"status=513", "code=SAFE"} {
		if !strings.Contains(legalBoundaries, preserved) {
			t.Fatalf("legal boundary lost %q: %s", preserved, legalBoundaries)
		}
	}

	metrics := RedactText(`token_len=64 SIGNATURELEN=-6.4e+1 authorization_len="64" cookie_len={"value":64} secret_len=NaN status=503`)
	for _, preserved := range []string{"token_len=64", "SIGNATURELEN=-6.4e+1", "status=503"} {
		if !strings.Contains(metrics, preserved) {
			t.Fatalf("strict numeric metric %q was not preserved: %s", preserved, metrics)
		}
	}
	for _, forbidden := range []string{"authorization_len", "cookie_len", "secret_len"} {
		if strings.Contains(metrics, forbidden) {
			t.Fatalf("non-numeric _len bypass %q survived: %s", forbidden, metrics)
		}
	}
}

func TestRedactTextTokenizerHasBoundedKeyAndLinearLongInput(t *testing.T) {
	boundaryKey := "ROOMID" + strings.Repeat("-", maximumEmbeddedLogKeyBytes-len("ROOMID"))
	boundary := `"` + boundaryKey + `":"private-boundary","status":503`
	clean := RedactText(boundary)
	if strings.Contains(clean, "private-boundary") || !strings.Contains(clean, `"status":503`) {
		t.Fatalf("exact-boundary key was not decoded and redacted: %s", clean)
	}

	overlongKey := "authorization" + strings.Repeat("a", maximumEmbeddedLogKeyBytes)
	for _, test := range []struct {
		name  string
		input string
	}{
		{
			name:  "bare",
			input: overlongKey + `="private-bare-overlong"` + "\nstatus=504",
		},
		{
			name:  "plain quoted",
			input: `"` + overlongKey + `":"private-quoted-overlong"` + "\nstatus=504",
		},
		{
			name:  "one escaped layer",
			input: `{\"` + overlongKey + `\":\"private-one-overlong\"}` + "\nstatus=504",
		},
		{
			name:  "two escaped layers",
			input: `{\\\"` + overlongKey + `\\\":\\\"private-two-overlong\\\"}` + "\nstatus=504",
		},
	} {
		t.Run("overlong "+test.name, func(t *testing.T) {
			clean := RedactText(test.input)
			if strings.Contains(clean, "private-") || !strings.Contains(clean, "[REDACTED]") ||
				!strings.Contains(clean, "\nstatus=504") {
				t.Fatalf("overlong key did not fail closed per line: %s", clean)
			}
		})
	}

	longInput := strings.Repeat("safe-prefix;", 1<<15) + `{"ROOMID":{"nested":"private-tail"},"status":210}`
	clean = RedactText(longInput)
	if strings.Contains(clean, "private-tail") || !strings.Contains(clean, `"status":210`) {
		t.Fatalf("long input was not processed correctly: length=%d suffix=%q", len(clean), clean[max(0, len(clean)-128):])
	}
}

type quotedJSONLogStringer string

func (value quotedJSONLogStringer) String() string { return string(value) }

func TestRedactingHandlerRemovesQuotedJSONFromMessagesErrorsAndStringers(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&output, nil))
	logger.Info(
		`payload {"room_id":"private-message-room","status":503}`,
		"err", errors.New(`{"token":"private-error-token","code":"SAFE_ERROR"}`),
		"detail", quotedJSONLogStringer(`{\"title\":\"private stringer title\",\"status\":204}`),
	)
	text := output.String()
	for _, forbidden := range []string{
		"private-message-room", "private-error-token", "private stringer title",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("quoted JSON value %q leaked: %s", forbidden, text)
		}
	}
	for _, preserved := range []string{"503", "SAFE_ERROR", "204"} {
		if !strings.Contains(text, preserved) {
			t.Fatalf("safe adjacent value %q was removed: %s", preserved, text)
		}
	}
}

func TestRedactingHandlerUsesTokenizerForEscapesCompositeValuesAndTypedLengths(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&output, nil))
	logger.Info(
		`payload {\\\"ROOM\\\\u005fID\\\":{\\\"nested\\\":\\\"private-message-object\\\"},\\\"status\\\":211}`,
		"err", errors.New(`authorization="Bearer private error credential" code=SAFE_ERROR`),
		"detail", quotedJSONLogStringer(`{'TOKEN':['private-stringer-array'],'status':212}`),
		"signature_len", 64,
		"token_len", "64",
		"cookie_len", slog.GroupValue(slog.Int("value", 64)),
	)
	text := output.String()
	for _, forbidden := range []string{
		"private-message-object", "private error credential", "private-stringer-array",
		"ROOM", "token_len", "cookie_len",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("tokenizer value %q leaked through slog: %s", forbidden, text)
		}
	}
	for _, preserved := range []string{"211", "SAFE_ERROR", "212", `"signature_len":64`} {
		if !strings.Contains(text, preserved) {
			t.Fatalf("safe slog value %q was removed: %s", preserved, text)
		}
	}
}

func TestRedactTextRemovesSupportedNetworkSchemesAndWebRID(t *testing.T) {
	input := strings.Join([]string{
		"ws://ws-private.invalid/socket",
		"wss://wss-private.invalid/socket",
		"rtmp://rtmp-private.invalid/live",
		"rtmps://rtmps-private.invalid/live",
		"rtsp://rtsp-private.invalid/live",
		"srt://srt-private.invalid:9000",
		"rist://rist-private.invalid:9000",
		"udp://udp-private.invalid:9000",
		"tcp://tcp-private.invalid:9000",
		"web_rid=private-web-rid",
	}, " ")
	clean := RedactText(input)
	if strings.Contains(clean, "private") || !strings.Contains(clean, "[REDACTED") {
		t.Fatalf("RedactText() = %q", clean)
	}
}

func TestRedactingHandlerRedactsDurableCorrelationIdentifiers(t *testing.T) {
	const sessionID = "019f7b6c-597a-774e-9bc7-6784e6438b04"
	var output bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&output, nil))
	logger.Info("repair", "correlation_id", sessionID, "session_id", sessionID)
	text := output.String()
	if strings.Contains(text, sessionID) || strings.Contains(text, "correlation_id") ||
		strings.Contains(RedactText("correlation_id="+sessionID), sessionID) {
		t.Fatalf("durable correlation identifier leaked: %s", text)
	}
	if !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("durable correlation identifier lacks redaction marker: %s", text)
	}
	if !safeSymbolicCorrelation("startup") || !safeSymbolicCorrelation("shutdown") ||
		safeSymbolicCorrelation(sessionID) {
		t.Fatal("symbolic correlation allowlist is invalid")
	}
}

func TestRedactTextRemovesInternalIdentifiersAndPIDs(t *testing.T) {
	input := strings.Join([]string{
		"MediaAttempt{id:private-attempt ordinal:1}",
		"SessionMedia{session:private-session revision:2}",
		"RoomRuntimeStatus{room=private-room state:recording}",
		"operation_id=private-operation gap:private-gap",
		"pid=12345 processId:54321",
	}, " ")
	clean := RedactText(input)
	for _, forbidden := range []string{"private", "12345", "54321"} {
		if strings.Contains(clean, forbidden) {
			t.Fatalf("RedactText() retained %q: %s", forbidden, clean)
		}
	}
}

func TestRedactingHandlerRemovesRoomIdentityAliasesAndEmbeddedAssignments(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&output, nil)).With(
		"Live-ID", "private-live-value",
		"component", "websocket",
		"signature_len", 64,
	)
	logger.InfoContext(context.Background(),
		"dial failed live_id=private-message-id status=503 title=Private Anchor Name",
		"platformRoomID", "private-room-value",
		"status_code", 503,
		"host", "safe.example",
		"url_len", 128,
		"attempt", 3,
		"details", slog.GroupValue(
			slog.String("userUniqueId", "private-user-value"),
			slog.String("display-name", "private-name-value"),
			slog.String("cursor", "private-cursor-value"),
			slog.Int("payload_len", 256),
		),
		"opaque", slog.AnyValue(map[string]any{
			"live_id": "private-map-value",
		}),
		"err", errors.New("upstream room_id=private-error-room internal_ext=private internal payload"),
	)
	logger.WithGroup("room-id").Info("group", "value", "private-group-value")

	text := output.String()
	for _, forbidden := range []string{
		"private-live-value", "private-message-id", "Private Anchor Name",
		"private-room-value", "private-user-value", "private-name-value",
		"private-cursor-value", "private-error-room", "private internal payload",
		"private-group-value", "platformRoomID", "userUniqueId", "display-name",
		"private-map-value",
	} {
		if strings.Contains(strings.ToLower(text), strings.ToLower(forbidden)) {
			t.Fatalf("log contains room identity %q: %s", forbidden, text)
		}
	}
	if !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("log contains no redaction marker: %s", text)
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) != 2 {
		t.Fatalf("line count = %d, want 2", len(lines))
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	for key, want := range map[string]any{
		"component":     "websocket",
		"status_code":   float64(503),
		"host":          "safe.example",
		"url_len":       float64(128),
		"signature_len": float64(64),
		"attempt":       float64(3),
	} {
		if got := record[key]; got != want {
			t.Fatalf("record[%q] = %#v, want %#v", key, got, want)
		}
	}
	details, ok := record["details"].(map[string]any)
	if !ok || details["payload_len"] != float64(256) {
		t.Fatalf("safe nested metrics missing: %#v", record["details"])
	}
}

func TestSensitiveKeyNormalizesAliases(t *testing.T) {
	for _, key := range []string{
		"id", "pid", "processId", "room", "session", "sessionID", "operation", "operation-id",
		"attempt", "attempt id", "gap", "gapId", "correlationId",
		"live_id", "Live-ID", "liveId", "webRid", "platformRoomID", "room config id",
		"userUniqueID", "anchor-name", "DisplayName", "internalExt",
		"websocketKey", "x-ms-stub", "pushServerV2", "proxy_server",
		"ROOMID", "PLATFORMROOMID", "USERUNIQUEID", "AUTHORIZATIONTOKEN",
	} {
		if !sensitiveKey(key) {
			t.Fatalf("sensitiveKey(%q) = false", key)
		}
	}
	for _, key := range []string{"status_code", "host", "payload_len", "attempt_count"} {
		if sensitiveKey(key) {
			t.Fatalf("sensitiveKey(%q) = true", key)
		}
	}
}

func TestRedactKeyValueArgsCopiesAndSanitizesLegacyArguments(t *testing.T) {
	original := []any{
		"roomID", "private-room",
		"title", "private title",
		"err", errors.New("request live_id=private-message-id failed"),
		"status_code", 503,
		"signature_len", 64,
		"attempt", 3,
		"opaque", map[string]any{"room_id": "private-map-room"},
	}
	clean := RedactKeyValueArgs(original)
	if original[1] != "private-room" || original[3] != "private title" {
		t.Fatalf("input arguments were mutated: %#v", original)
	}
	text := fmt.Sprint(clean...)
	for _, forbidden := range []string{
		"private-room", "private title", "private-message-id", "private-map-room",
		"roomID", "title",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("sanitized arguments contain %q: %s", forbidden, text)
		}
	}
	for _, preserved := range []string{"status_code", "503", "signature_len", "64", "attempt", "3"} {
		if !strings.Contains(text, preserved) {
			t.Fatalf("sanitized arguments lost %q: %s", preserved, text)
		}
	}
	if !strings.Contains(RedactText("room_id=private-room status=503"), "[REDACTED]") {
		t.Fatal("RedactText did not redact embedded identity")
	}
}

func TestOpenFileLoggerAppliesRetentionAndWritesJSONL(t *testing.T) {
	logsDir := t.TempDir()
	for name, content := range map[string]string{
		"app-2026-06-01.jsonl": "old",
		"app-2026-07-10.jsonl": "recent",
		"notes.txt":            "unrelated",
	} {
		if err := os.WriteFile(filepath.Join(logsDir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", name, err)
		}
	}
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.Local)
	fileLogger, err := OpenFileLogger(logsDir, FileOptions{Now: now, RetentionDays: 14})
	if err != nil {
		t.Fatalf("OpenFileLogger() error = %v", err)
	}
	fileLogger.Logger.Info("database ready", "component", "storage", "schema_version", 1)
	if err := fileLogger.Sync(); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if err := fileLogger.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(logsDir, "app-2026-06-01.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("expired log still exists: %v", err)
	}
	for _, name := range []string{"app-2026-07-10.jsonl", "notes.txt", "app-2026-07-17.jsonl"} {
		if _, err := os.Stat(filepath.Join(logsDir, name)); err != nil {
			t.Fatalf("expected file %q: %v", name, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(logsDir, "app-2026-07-17.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 || !json.Valid([]byte(lines[0])) {
		t.Fatalf("log is not one JSONL record: %q", data)
	}
}

type embeddedLogMatrixStyle struct {
	name   string
	marker string
	bare   bool
}

var embeddedLogMatrixStyles = []embeddedLogMatrixStyle{
	{name: "bare", bare: true},
	{name: "double", marker: `"`},
	{name: "single", marker: `'`},
	{name: "escaped-one", marker: `\"`},
	{name: "escaped-two", marker: `\\\"`},
}

func embeddedLogSensitiveAliasCorpus() []string {
	return []string{
		"id", "pid", "processid", "room", "session", "sessionid",
		"operation", "operationid", "attempt", "attemptid", "correlationid",
		"gap", "gapid", "liveid", "webrid", "platformroomid", "roomid",
		"roomconfigid", "useruniqueid", "userid", "anchorid", "secanchorid",
		"livename", "anchorname", "displayname", "nickname", "title", "cursor",
		"internalext", "websocketkey", "xmsstub", "pushserver", "pushserverv2",
		"proxyserver", "cookie", "authorization", "token", "signature", "abogus",
		"credential", "secret", "password", "streamurl", "signedurl", "url",
		"process-ID", "room config ID", "Live-ID", "web_rid", "platformRoomID",
		"userUniqueID", "anchor-name", "DisplayName", "x-ms-stub", "pushServerV2",
		"AUTHORIZATION_TOKEN", "media_url", "+2token", "-2authorization",
		".2room_id", "room_id_value", "session-id-text", "title.text",
		"cursor value", "roomIDValue", "prefixRoomIDValueSuffix",
		"prefix_session_id_text_suffix", `room\u005fid_value`,
	}
}

func embeddedLogMatrixAssignment(style embeddedLogMatrixStyle, key, valueKind, secret string) string {
	marker := style.marker
	separator := ":"
	keyToken := marker + key + marker
	if style.bare {
		marker = `"`
		separator = "="
		keyToken = key
	}
	var value string
	switch valueKind {
	case "bare":
		value = secret
	case "quoted":
		value = marker + secret + marker
	case "object":
		value = "{" + marker + "nested" + marker + ":" + marker + secret + marker + "}"
	case "array":
		value = "[" + marker + secret + marker + "]"
	default:
		panic("unknown embedded log matrix value kind: " + valueKind)
	}
	return keyToken + separator + value
}

func TestRedactTextTokenizerSensitiveAliasEntryValueMatrix(t *testing.T) {
	aliases := embeddedLogSensitiveAliasCorpus()
	for key := range sensitiveLogKeys {
		found := false
		for _, alias := range aliases {
			if classifyLogKey(alias).canonical == key {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("sensitive alias corpus does not cover canonical key %q", key)
		}
	}
	for aliasIndex, alias := range aliases {
		if !sensitiveKey(alias) {
			t.Fatalf("sensitive alias corpus item %d is not classified", aliasIndex)
		}
		for _, style := range embeddedLogMatrixStyles {
			for _, valueKind := range []string{"bare", "quoted", "object", "array"} {
				name := fmt.Sprintf("alias-%03d/%s/%s", aliasIndex, style.name, valueKind)
				t.Run(name, func(t *testing.T) {
					const secret = "private-matrix-value"
					input := embeddedLogMatrixAssignment(style, alias, valueKind, secret) + "\nstatus=299"
					clean := RedactText(input)
					if strings.Contains(clean, secret) || !strings.Contains(clean, "[REDACTED]") {
						t.Fatalf("sensitive matrix assignment was not redacted: %q", clean)
					}
					if !strings.Contains(clean, "\nstatus=299") {
						t.Fatalf("sensitive matrix assignment crossed the line boundary: %q", clean)
					}
				})
			}
		}
	}
}

func TestRedactTextTokenizerSensitiveLengthMetricMatrix(t *testing.T) {
	for aliasIndex, alias := range embeddedLogSensitiveAliasCorpus() {
		for _, style := range embeddedLogMatrixStyles {
			name := fmt.Sprintf("alias-%03d/%s", aliasIndex, style.name)
			t.Run(name, func(t *testing.T) {
				key := alias + "_len"
				valid := embeddedLogMatrixAssignment(style, key, "bare", "64") + "\nstatus=300"
				if clean := RedactText(valid); clean != valid {
					t.Fatalf("strict numeric length metric changed: got %q want %q", clean, valid)
				}
				for _, valueKind := range []string{"quoted", "object", "array"} {
					const secret = "private-metric-value"
					input := embeddedLogMatrixAssignment(style, key, valueKind, secret) + "\nstatus=301"
					clean := RedactText(input)
					if strings.Contains(clean, secret) || !strings.Contains(clean, "[REDACTED]") ||
						!strings.Contains(clean, "\nstatus=301") {
						t.Fatalf("non-numeric length metric was not line-bounded redacted: %q", clean)
					}
				}
				invalidBare := embeddedLogMatrixAssignment(style, key, "bare", "64oops confidential tail") +
					" status=302"
				clean := RedactText(invalidBare)
				for _, forbidden := range []string{"64oops", "confidential", "tail"} {
					if strings.Contains(clean, forbidden) {
						t.Fatalf("invalid bare length metric retained %q: %q", forbidden, clean)
					}
				}
				if !strings.Contains(clean, "status=302") {
					t.Fatalf("invalid bare length metric lost proven adjacent status: %q", clean)
				}
			})
		}
	}
}

func TestRedactTextTokenizerBoundariesUnicodeAndAmbiguity(t *testing.T) {
	for _, leading := range []string{"", " ", "\t", "{", "[", ",", ";", "prefix "} {
		for _, style := range embeddedLogMatrixStyles {
			input := leading + embeddedLogMatrixAssignment(style, "authorization", "quoted", "private-leading") +
				"\nstatus=303"
			clean := RedactText(input)
			if strings.Contains(clean, "private-leading") || !strings.Contains(clean, "[REDACTED]") ||
				!strings.Contains(clean, "\nstatus=303") {
				t.Fatalf("leading boundary %q style %s was not safely tokenized: %q", leading, style.name, clean)
			}
		}
	}

	for _, valueKind := range []string{"quoted", "object", "array"} {
		for _, suffix := range []string{"", " ", "\t", ",", ";", "}", "]", "\r\nstatus=304"} {
			input := embeddedLogMatrixAssignment(embeddedLogMatrixStyles[1], "token", valueKind, "private-boundary") + suffix
			clean := RedactText(input)
			if strings.Contains(clean, "private-boundary") || !strings.Contains(clean, "[REDACTED]") {
				t.Fatalf("legal %s suffix %q was not redacted: %q", valueKind, suffix, clean)
			}
			if suffix != "" && !strings.Contains(clean, suffix) {
				t.Fatalf("legal %s suffix %q was not preserved: %q", valueKind, suffix, clean)
			}
		}
		for _, suffix := range []string{"tail", ":tail", "=tail", "{tail", "(tail"} {
			input := embeddedLogMatrixAssignment(embeddedLogMatrixStyles[1], "token", valueKind, "private-invalid-boundary") +
				suffix + "\nstatus=305"
			clean := RedactText(input)
			if clean != "[REDACTED]\nstatus=305" {
				t.Fatalf("ambiguous %s suffix %q did not fail closed per line: %q", valueKind, suffix, clean)
			}
		}
	}

	for _, input := range []string{
		"room＿id=私密值 trailing confidential status=306",
		`room\u005fID=private-unicode-escape trailing confidential status=306`,
		`authorization\u002dtoken=private-unicode-token trailing confidential status=306`,
		`room\uD83D\uDE00id=private-surrogate-key trailing confidential status=306`,
		`\u0072oom\u005fid=private-leading-unicode-room trailing confidential status=306`,
		`\u0061uthorization=private-leading-unicode-authorization trailing confidential status=306`,
		`\u0032token=private-leading-unicode-digit trailing confidential status=306`,
	} {
		clean := RedactText(input)
		for _, forbidden := range []string{"私密值", "private-", "trailing", "confidential"} {
			if strings.Contains(clean, forbidden) {
				t.Fatalf("Unicode bare key retained %q: %q", forbidden, clean)
			}
		}
		if !strings.Contains(clean, "status=306") {
			t.Fatalf("Unicode bare key lost proven adjacent status: %q", clean)
		}
	}

	for _, input := range []string{
		`room\u00ZZid=private-invalid-unicode` + "\nstatus=307",
		`room\uD800id=private-invalid-surrogate` + "\nstatus=307",
		`\u00ZZroomid=private-invalid-leading-unicode` + "\nstatus=307",
		"room" + string([]byte{0xff}) + "id=private-invalid-utf8\nstatus=307",
	} {
		clean := RedactText(input)
		if strings.Contains(clean, "private-") || !strings.Contains(clean, "[REDACTED]") ||
			!strings.Contains(clean, "\nstatus=307") {
			t.Fatalf("malformed Unicode bare key did not fail closed per line: %q", clean)
		}
	}

	for _, alias := range embeddedLogSensitiveAliasCorpus() {
		input := alias + "=private-bare-value trailing confidential tail status=308"
		clean := RedactText(input)
		for _, forbidden := range []string{"private-bare-value", "trailing", "confidential", "tail"} {
			if strings.Contains(clean, forbidden) {
				t.Fatalf("sensitive bare tail retained %q: %q", forbidden, clean)
			}
		}
		if !strings.Contains(clean, "status=308") {
			t.Fatalf("sensitive bare tail lost proven adjacent status: %q", clean)
		}
	}
}

func TestRedactTextTokenizerUnicodeAndOverlongBareKeyByteBounds(t *testing.T) {
	exact := "token" + strings.Repeat("＿", 41)
	if len(exact) != maximumEmbeddedLogKeyBytes {
		t.Fatalf("exact Unicode key fixture bytes = %d", len(exact))
	}
	clean := RedactText(exact + "=private-exact-unicode\nstatus=309")
	if strings.Contains(clean, "private-exact-unicode") || !strings.Contains(clean, "\nstatus=309") {
		t.Fatalf("exact Unicode key byte boundary was not redacted: %q", clean)
	}

	overlong := exact + "x"
	clean = RedactText(overlong + "=private-overlong-unicode\nstatus=310")
	if strings.Contains(clean, "private-overlong-unicode") || !strings.Contains(clean, "[REDACTED]") ||
		!strings.Contains(clean, "\nstatus=310") {
		t.Fatalf("overlong Unicode key did not fail closed per line: %q", clean)
	}
}

func TestRedactTextTokenizerDoesNotStartBareKeysInsideSafeValues(t *testing.T) {
	for _, input := range []string{
		"status=513 authorization=private-positive",
		"status=-513 authorization=private-negative",
		"status=+513 authorization=private-plus",
		"status=.513 authorization=private-fraction",
		"status=SAFE authorization=private-symbolic",
	} {
		clean := RedactText(input)
		if strings.Contains(clean, "private-") || !strings.Contains(clean, "[REDACTED]") {
			t.Fatalf("adjacent sensitive key was not redacted: %q", clean)
		}
		statusEnd := strings.IndexByte(input, ' ')
		if statusEnd < 0 || !strings.Contains(clean, input[:statusEnd]) {
			t.Fatalf("proven safe value was swallowed as a key: input=%q clean=%q", input, clean)
		}
	}

	for _, input := range []string{
		"2token=private-digit-key\nstatus=311",
		"prefix 2token=private-spaced-digit-key\nstatus=311",
		"{2token=private-delimited-digit-key\nstatus=311",
	} {
		clean := RedactText(input)
		if strings.Contains(clean, "private-") || !strings.Contains(clean, "[REDACTED]") ||
			!strings.Contains(clean, "\nstatus=311") {
			t.Fatalf("digit-prefixed classified key was not redacted: %q", clean)
		}
	}
}

func TestRedactTextTokenizerFailsClosedOnNestedAmbiguousBareAssignments(t *testing.T) {
	for _, input := range []string{
		"status=authorization=private-nested-assignment\nstatus=312",
		`status=\u0072oomid=private-unicode-nested-assignment` + "\nstatus=312",
		"message=token=private-message-nested-assignment\nstatus=312",
	} {
		clean := RedactText(input)
		if clean != "[REDACTED]\nstatus=312" {
			t.Fatalf("ambiguous nested bare assignment did not fail closed per line: %q", clean)
		}
	}

	urlValue := RedactText("status=http://private-url-like-value\nstatus=312")
	if urlValue != "status=[REDACTED-URL]\nstatus=312" {
		t.Fatalf("bare URL assignment did not preserve its safe structure: %q", urlValue)
	}

	for _, input := range []string{
		`message="token=private-inside-quoted" status=313`,
		`details={"token":"private-inside-composite"} status=313`,
		`message='authorization=private-inside-single' status=313`,
	} {
		clean := RedactText(input)
		if strings.Contains(clean, "private-") || !strings.Contains(clean, "[REDACTED]") {
			t.Fatalf("sensitive assignment inside a safe value was not redacted: %q", clean)
		}
		if !strings.Contains(clean, "status=313") {
			t.Fatalf("sensitive assignment inside a safe value lost adjacent status: %q", clean)
		}
	}
}

func TestRedactTextTokenizerSymbolPrefixedNumericKeysAndSafeNumbers(t *testing.T) {
	for _, input := range []string{
		"+2token=private-plus-prefix\nstatus=314",
		"-2authorization=private-minus-prefix\nstatus=314",
		".2room_id=private-dot-prefix\nstatus=314",
		"prefix +2token=private-spaced-plus-prefix\nstatus=314",
		"{-2authorization=private-braced-minus-prefix\nstatus=314",
		"[.2room_id=private-bracketed-dot-prefix\nstatus=314",
	} {
		clean := RedactText(input)
		if strings.Contains(clean, "private-") || !strings.Contains(clean, "[REDACTED]") ||
			!strings.Contains(clean, "\nstatus=314") {
			t.Fatalf("symbol-prefixed numeric key was not redacted: %q", clean)
		}
	}

	for _, test := range []struct {
		input      string
		safePrefix string
	}{
		{input: "status=+513 +2token=private-adjacent-plus", safePrefix: "status=+513"},
		{input: "status=-513 -2authorization=private-adjacent-minus", safePrefix: "status=-513"},
		{input: "status=.513 .2room_id=private-adjacent-dot", safePrefix: "status=.513"},
		{input: "status= +513 +2token=private-spaced-value-plus", safePrefix: "status= +513"},
		{input: "status = -513 -2authorization=private-spaced-value-minus", safePrefix: "status = -513"},
		{input: "status = .513 .2room_id=private-spaced-value-dot", safePrefix: "status = .513"},
		{input: "status=+5.13e+2,+2token=private-scientific-plus", safePrefix: "status=+5.13e+2"},
	} {
		clean := RedactText(test.input)
		if strings.Contains(clean, "private-") || !strings.Contains(clean, "[REDACTED]") {
			t.Fatalf("adjacent symbol-prefixed key was not redacted: %q", clean)
		}
		if !strings.Contains(clean, test.safePrefix) {
			t.Fatalf("safe signed or decimal value was swallowed: input=%q clean=%q", test.input, clean)
		}
	}

	for _, input := range []string{
		"status=+513",
		"status=-513",
		"status=.513",
		"status= +513",
		"status = -513",
		"status = .513",
		"status=+5.13e+2",
	} {
		if clean := RedactText(input); clean != input {
			t.Fatalf("safe signed or decimal status changed: got %q want %q", clean, input)
		}
	}
}

func TestLogKeyComponentClassificationBoundaries(t *testing.T) {
	componentCases := []struct {
		input string
		want  string
	}{
		{input: "room_id_value", want: "room/id/value"},
		{input: "session-id-text", want: "session/id/text"},
		{input: "title.text", want: "title/text"},
		{input: "cursor value", want: "cursor/value"},
		{input: "roomIDValue", want: "room/id/value"},
		{input: "webRIDValue", want: "web/rid/value"},
		{input: "pushServerV2Value", want: "push/server/v/2/value"},
		{input: ".2room_id", want: "2/room/id"},
	}
	for _, test := range componentCases {
		if got := strings.Join(splitLogKeyComponents(test.input), "/"); got != test.want {
			t.Fatalf("splitLogKeyComponents(%q) = %q, want %q", test.input, got, test.want)
		}
	}

	for _, key := range []string{
		"room_id_value", "prefix_room_id_value_suffix", "room-id-value-suffix",
		"prefix.room.id.value", "prefix room id value", "prefixRoomIDValueSuffix",
		"session_id_text", "prefix-session-id-text-suffix", "prefixSessionIDTextSuffix",
		"title_text", "prefix-title-text-suffix", "prefixTitleTextSuffix",
		"cursor_value", "prefix.cursor.value.suffix", "prefixCursorValueSuffix",
		"prefix_id_value", "prefix-pid-value", "prefix room value", "prefix.gap.value",
		"+2token_value", "-2authorization-text", ".2room_id_value",
		`room\u005fid_value`, `prefix_\u0072oom\u005fid_value_suffix`,
		"prefix_tokenized_value", "notauthorizationish",
	} {
		if !sensitiveKey(key) {
			t.Fatalf("component-sensitive key %q was not classified", key)
		}
	}

	for _, key := range []string{
		"valid", "fluid", "classroom", "status", "rapid", "gapped", "bedroom",
		"grid", "paid", "headroom", "cooperation", "precursor",
		"roomvalue", "pidvalue", "gapvalue", "prefixroomidvalue",
		"prefixsessionidtext", "prefixtitletext", "prefixcursorvalue",
		"valid_len", "fluid_len", "classroom_len", "status_len",
		"grid_len", "paid_len", "headroom_len", "cooperation_len", "precursor_len",
		"room_count", "attempt_count",
	} {
		if sensitiveKey(key) {
			t.Fatalf("safe component key %q was classified sensitive", key)
		}
	}
}

func TestRedactTextTokenizerComponentAliasesAndSafeKeysAcrossStyles(t *testing.T) {
	sensitive := []string{
		"room_id_value", "session-id-text", "title.text", "cursor value",
		"prefixRoomIDValueSuffix", `room\u005fid_value`, ".2room_id_value",
	}
	for keyIndex, key := range sensitive {
		for _, style := range embeddedLogMatrixStyles {
			name := fmt.Sprintf("sensitive-%02d/%s", keyIndex, style.name)
			t.Run(name, func(t *testing.T) {
				const secret = "private-component-matrix"
				input := embeddedLogMatrixAssignment(style, key, "quoted", secret) + "\nstatus=315"
				clean := RedactText(input)
				if strings.Contains(clean, secret) || !strings.Contains(clean, "[REDACTED]") ||
					!strings.Contains(clean, "\nstatus=315") {
					t.Fatalf("component alias was not line-bounded redacted: %q", clean)
				}
			})
		}
	}

	safe := []string{
		"valid", "fluid", "classroom", "status", "rapid", "gapped", "bedroom",
		"grid", "paid", "headroom", "cooperation", "precursor",
		"roomvalue", "pidvalue", "gapvalue", "prefixroomidvalue",
		"prefixsessionidtext", "prefixtitletext", "prefixcursorvalue",
		"valid_len", "fluid_len", "classroom_len", "status_len",
		"grid_len", "paid_len", "headroom_len", "cooperation_len", "precursor_len",
		"room_count", "attempt_count",
	}
	for keyIndex, key := range safe {
		for _, style := range embeddedLogMatrixStyles {
			name := fmt.Sprintf("safe-%02d/%s", keyIndex, style.name)
			t.Run(name, func(t *testing.T) {
				input := embeddedLogMatrixAssignment(style, key, "quoted", "safe-component-value")
				if clean := RedactText(input); clean != input {
					t.Fatalf("safe component key changed: got %q want %q", clean, input)
				}
			})
		}
	}
}

func TestComponentAliasClassificationAcrossStructuredIngresses(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&output, nil))
	logger.Info(
		"component ingress",
		"room_id_value", "private-slog-room",
		"session-id-text", "private-slog-session",
		"title.text", "private-slog-title",
		"cursor value", "private-slog-cursor",
		"prefixRoomIDValueSuffix", "private-slog-camel",
		"valid", "safe-slog-valid",
		"fluid", "safe-slog-fluid",
		"classroom", "safe-slog-classroom",
		"status", "safe-slog-status",
		"grid", "safe-slog-grid",
		"paid", "safe-slog-paid",
		"headroom", "safe-slog-headroom",
		"cooperation", "safe-slog-cooperation",
		"precursor", "safe-slog-precursor",
	)
	text := output.String()
	for _, forbidden := range []string{
		"private-slog-room", "private-slog-session", "private-slog-title",
		"private-slog-cursor", "private-slog-camel",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("slog component ingress retained %q: %s", forbidden, text)
		}
	}
	for _, preserved := range []string{
		"safe-slog-valid", "safe-slog-fluid", "safe-slog-classroom", "safe-slog-status",
		"safe-slog-grid", "safe-slog-paid", "safe-slog-headroom",
		"safe-slog-cooperation", "safe-slog-precursor",
	} {
		if !strings.Contains(text, preserved) {
			t.Fatalf("slog component ingress lost %q: %s", preserved, text)
		}
	}

	legacy := RedactKeyValueArgs([]any{
		"room_id_value", "private-legacy-room",
		"session-id-text", "private-legacy-session",
		"title.text", "private-legacy-title",
		"cursor value", "private-legacy-cursor",
		"prefixRoomIDValueSuffix", "private-legacy-camel",
		"valid", "safe-legacy-valid",
		"fluid", "safe-legacy-fluid",
		"classroom", "safe-legacy-classroom",
		"status", "safe-legacy-status",
		"grid", "safe-legacy-grid",
		"paid", "safe-legacy-paid",
		"headroom", "safe-legacy-headroom",
		"cooperation", "safe-legacy-cooperation",
		"precursor", "safe-legacy-precursor",
	})
	legacyText := fmt.Sprint(legacy...)
	for _, forbidden := range []string{
		"private-legacy-room", "private-legacy-session", "private-legacy-title",
		"private-legacy-cursor", "private-legacy-camel",
	} {
		if strings.Contains(legacyText, forbidden) {
			t.Fatalf("legacy component ingress retained %q: %s", forbidden, legacyText)
		}
	}
	for _, preserved := range []string{
		"safe-legacy-valid", "safe-legacy-fluid", "safe-legacy-classroom", "safe-legacy-status",
		"safe-legacy-grid", "safe-legacy-paid", "safe-legacy-headroom",
		"safe-legacy-cooperation", "safe-legacy-precursor",
	} {
		if !strings.Contains(legacyText, preserved) {
			t.Fatalf("legacy component ingress lost %q: %s", preserved, legacyText)
		}
	}
}

func TestRedactTextTokenizerStructuralControlAndInvalidBoundaries(t *testing.T) {
	structural := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "closing brace before token",
			input: "prefix}token=private-structural,status=716",
			want:  "prefix}[REDACTED],status=716",
		},
		{
			name:  "closing bracket before room id",
			input: "prefix]room_id=private-room;code=SAFE",
			want:  "prefix][REDACTED];code=SAFE",
		},
		{
			name:  "closing parenthesis before title",
			input: "prefix)title=private-title>status=718",
			want:  "prefix)[REDACTED]>status=718",
		},
		{
			name:  "closing angle before token",
			input: "prefix>token=private-angle,status=719",
			want:  "prefix>[REDACTED],status=719",
		},
		{
			name:  "closing brace after bare value",
			input: "room_id=private-room},status=720",
			want:  "[REDACTED]},status=720",
		},
		{
			name:  "closing bracket after bare value",
			input: "token=private-token],status=721",
			want:  "[REDACTED]],status=721",
		},
	}
	for _, test := range structural {
		t.Run(test.name, func(t *testing.T) {
			got := RedactText(test.input)
			if got != test.want {
				t.Fatalf("RedactText() = %q, want %q", got, test.want)
			}
			if second := RedactText(got); second != got {
				t.Fatalf("RedactText() is not idempotent: first %q second %q", got, second)
			}
		})
	}

	taints := []struct {
		name  string
		value byte
	}{
		{name: "NUL", value: 0x00},
		{name: "ESC", value: 0x1b},
		{name: "DEL", value: 0x7f},
		{name: "invalid UTF-8", value: 0xff},
	}
	forms := []struct {
		name  string
		build func(byte) string
	}{
		{
			name: "before key",
			build: func(taint byte) string {
				return "prefix" + string([]byte{taint}) + "room_id=private-boundary\nstatus=722"
			},
		},
		{
			name: "inside key",
			build: func(taint byte) string {
				return "to" + string([]byte{taint}) + "ken=private-inside\nstatus=723"
			},
		},
		{
			name: "before delimiter",
			build: func(taint byte) string {
				return "room_id" + string([]byte{taint}) + "=private-delimiter\nstatus=724"
			},
		},
	}
	for _, taint := range taints {
		for _, form := range forms {
			t.Run(taint.name+"/"+form.name, func(t *testing.T) {
				clean := RedactText(form.build(taint.value))
				if strings.Contains(clean, "private-") || !strings.Contains(clean, "[REDACTED]") ||
					!strings.HasSuffix(clean, "\nstatus=72"+map[string]string{
						"before key": "2", "inside key": "3", "before delimiter": "4",
					}[form.name]) {
					t.Fatalf("tainted key was not fail-closed and line-bounded: %q", clean)
				}
				if !utf8.ValidString(clean) {
					t.Fatalf("tainted redaction emitted invalid UTF-8: %q", clean)
				}
				if second := RedactText(clean); second != clean {
					t.Fatalf("tainted redaction is not idempotent: first %q second %q", clean, second)
				}
			})
		}
	}

	safe := "cooperation=safe,precursor=safe;valid=safe fluid=safe grid=safe paid=safe classroom=safe headroom=safe"
	if clean := RedactText(safe); clean != safe {
		t.Fatalf("safe collision words changed: got %q want %q", clean, safe)
	}
}

func TestRedactTextTokenizerPreservesQuotedAndCompositeContainerStructure(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "plain quoted container",
			input: `"message":"room_id=private","status":632`,
			want:  `"message":"[REDACTED]","status":632`,
		},
		{
			name:  "one escaped quoted container",
			input: `\"message\":\"room_id=private-one\",\"status\":633`,
			want:  `\"message\":\"[REDACTED]\",\"status\":633`,
		},
		{
			name:  "two escaped quoted container",
			input: `{\\\"message\\\":\\\"room_id=private-two\\\",\\\"status\\\":634}`,
			want:  `{\\\"message\\\":\\\"[REDACTED]\\\",\\\"status\\\":634}`,
		},
		{
			name:  "nested composite container",
			input: `"message":{"detail":"token=private-composite","status":635},"outer":636`,
			want:  `"message":{"detail":"[REDACTED]","status":635},"outer":636`,
		},
		{
			name:  "one escaped nested composite container",
			input: `{\"message\":{\"detail\":\"token=private-escaped-composite\",\"status\":637},\"outer\":638}`,
			want:  `{\"message\":{\"detail\":\"[REDACTED]\",\"status\":637},\"outer\":638}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := RedactText(test.input)
			if got != test.want {
				t.Fatalf("RedactText() = %q, want exact structure %q", got, test.want)
			}
			if second := RedactText(got); second != got {
				t.Fatalf("container redaction is not idempotent: first %q second %q", got, second)
			}
		})
	}
}

func TestRedactTextRemovesBoundedJSONEncodedURLForms(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "plain",
			input: `message="https://private-url.invalid/live/value" status=640`,
			want:  `message="[REDACTED-URL]" status=640`,
		},
		{
			name:  "one escaped slash layer",
			input: `message="https:\/\/private-url.invalid\/live\/value" status=641`,
			want:  `message="[REDACTED-URL]" status=641`,
		},
		{
			name:  "two escaped slash layers and escaped quote",
			input: `message=\"https:\\/\\/private-url.invalid\\/live\\/value\" status=642`,
			want:  `message=\"[REDACTED-URL]\" status=642`,
		},
		{
			name:  "only second slash escaped",
			input: `message="https:/\/private-url.invalid\/mixed" status=643`,
			want:  `message="[REDACTED-URL]" status=643`,
		},
		{
			name:  "unicode slash mixed hex case",
			input: `message="https\u003A\u002f\u002Fprivate-url.invalid\u002flive" status=644`,
			want:  `message="[REDACTED-URL]" status=644`,
		},
		{
			name:  "encoded scheme letter and delimiters",
			input: `message="h\u0074tps\u003a\u002f\u002fprivate-url.invalid\u002fx" status=645`,
			want:  `message="[REDACTED-URL]" status=645`,
		},
		{
			name:  "two encoded layers",
			input: `message=\"h\\u0074tps\\u003a\\u002f\\u002fprivate-url.invalid\\u002fx\" status=646`,
			want:  `message=\"[REDACTED-URL]\" status=646`,
		},
		{
			name:  "wss encoded",
			input: `message="wss:\/\/private-url.invalid\/socket" status=647`,
			want:  `message="[REDACTED-URL]" status=647`,
		},
		{
			name:  "only first slash escaped",
			input: `message="https:\//private-url.invalid/x" status=648`,
			want:  `message="[REDACTED-URL]" status=648`,
		},
		{
			name:  "malformed single slash fails closed",
			input: `message="https:\/private-url.invalid/x" status=649`,
			want:  `message="[REDACTED-URL]" status=649`,
		},
		{
			name:  "decoded unicode space terminates",
			input: `https:\/\/private-url.invalid\/x\u0020status=650`,
			want:  `[REDACTED-URL]\u0020status=650`,
		},
		{
			name:  "two layer decoded unicode space terminates",
			input: `https:\\/\\/private-url.invalid\\/x\\u0020status=651`,
			want:  `[REDACTED-URL]\\u0020status=651`,
		},
		{
			name:  "decoded tab terminates",
			input: `https:\/\/private-url.invalid\/x\tstatus=652`,
			want:  `[REDACTED-URL]\tstatus=652`,
		},
		{
			name:  "raw and decoded overlap once",
			input: `https://private-url.invalid\/x status=653`,
			want:  `[REDACTED-URL] status=653`,
		},
		{
			name:  "decoded quote terminates",
			input: `https:\/\/private-url.invalid\/x\"status=654`,
			want:  `[REDACTED-URL]\"status=654`,
		},
		{
			name:  "two layer decoded quote terminates",
			input: `https:\\/\\/private-url.invalid\\/x\\\"status=655`,
			want:  `[REDACTED-URL]\\\"status=655`,
		},
		{
			name:  "adjacent raw and encoded URLs",
			input: `https://first-private.invalid/xhttps:\/\/second-private.invalid\/y status=656`,
			want:  `[REDACTED-URL] status=656`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clean := RedactText(test.input)
			if clean != test.want {
				t.Fatalf("RedactText() = %q, want %q", clean, test.want)
			}
			if strings.Contains(clean, "private-url.invalid") || strings.Count(clean, "[REDACTED-URL]") != 1 {
				t.Fatalf("URL redaction was incomplete or duplicated: %q", clean)
			}
			if second := RedactText(clean); second != clean {
				t.Fatalf("URL redaction is not idempotent: first %q second %q", clean, second)
			}
		})
	}

	safe := `httpsx://safe.invalid prefixhttps://safe.invalid http:/safe.invalid https_status=safe cooperation=safe precursor=safe valid=safe fluid=safe grid=safe paid=safe classroom=safe headroom=safe`
	if clean := RedactText(safe); clean != safe {
		t.Fatalf("safe URL collision words changed: got %q want %q", clean, safe)
	}
}

func TestEncodedURLsAcrossSlogAndLegacyIngresses(t *testing.T) {
	message := `request https:\/\/slog-private.invalid\/x\u0020status=661`
	errorValue := errors.New(`wss\u003a\u002f\u002ferror-private.invalid\u002fx\tcode=SAFE_ERROR`)
	stringerValue := quotedJSONLogStringer(`https:\\/\\/stringer-private.invalid\\/x\\u0020status=663`)

	var output bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&output, nil))
	logger.Info(message, "err", errorValue, "detail", stringerValue)
	slogText := output.String()
	for _, forbidden := range []string{"slog-private.invalid", "error-private.invalid", "stringer-private.invalid"} {
		if strings.Contains(slogText, forbidden) {
			t.Fatalf("encoded URL %q leaked through slog: %s", forbidden, slogText)
		}
	}
	for _, preserved := range []string{"status=661", "SAFE_ERROR", "status=663"} {
		if !strings.Contains(slogText, preserved) {
			t.Fatalf("slog redaction removed safe suffix %q: %s", preserved, slogText)
		}
	}

	legacy := RedactKeyValueArgs([]any{
		"message", message,
		"err", errorValue,
		"detail", stringerValue,
	})
	legacyText := fmt.Sprint(legacy...)
	for _, forbidden := range []string{"slog-private.invalid", "error-private.invalid", "stringer-private.invalid"} {
		if strings.Contains(legacyText, forbidden) {
			t.Fatalf("encoded URL %q leaked through legacy args: %s", forbidden, legacyText)
		}
	}
	for _, preserved := range []string{"status=661", "SAFE_ERROR", "status=663"} {
		if !strings.Contains(legacyText, preserved) {
			t.Fatalf("legacy redaction removed safe suffix %q: %s", preserved, legacyText)
		}
	}
}

func TestRedactTextTokenizerAllTerminatorBoundaries(t *testing.T) {
	leading := []struct {
		name  string
		input string
		want  string
	}{
		{name: "leading colon", input: ":room=private-leading-colon,status=740", want: ":[REDACTED],status=740"},
		{name: "leading equals", input: "=token=private-leading-equals;code=SAFE", want: "=[REDACTED];code=SAFE"},
		{name: "braced colon", input: "{:title=private-braced-colon},status=741", want: "{:[REDACTED]},status=741"},
		{name: "bracketed equals", input: "[=cursor=private-bracketed-equals],status=742", want: "[=[REDACTED]],status=742"},
		{name: "consecutive colons", input: "::room_id=private-double-colon,status=743", want: "::[REDACTED],status=743"},
		{name: "consecutive equals", input: "==token=private-double-equals;code=SAFE", want: "==[REDACTED];code=SAFE"},
		{name: "nested colon equals", input: "{:=title=private-nested-prefix},status=744", want: "{:=[REDACTED]},status=744"},
		{name: "nested equals colon", input: "[=:cursor=private-reverse-prefix],status=745", want: "[=:[REDACTED]],status=745"},
	}
	for _, test := range leading {
		t.Run(test.name, func(t *testing.T) {
			if got := RedactText(test.input); got != test.want {
				t.Fatalf("RedactText() = %q, want %q", got, test.want)
			}
		})
	}

	boundaries := []struct {
		name   string
		prefix string
	}{
		{name: "colon", prefix: ":"},
		{name: "equals", prefix: "="},
		{name: "open brace", prefix: "{"},
		{name: "close brace", prefix: "}"},
		{name: "open bracket", prefix: "["},
		{name: "close bracket", prefix: "]"},
		{name: "open parenthesis", prefix: "("},
		{name: "close parenthesis", prefix: ")"},
		{name: "open angle", prefix: "<"},
		{name: "close angle", prefix: ">"},
		{name: "comma", prefix: ","},
		{name: "semicolon", prefix: ";"},
		{name: "double quote", prefix: `"`},
		{name: "single quote", prefix: `'`},
		{name: "backslash", prefix: `\`},
		{name: "space", prefix: " "},
		{name: "tab", prefix: "\t"},
	}
	for index, boundary := range boundaries {
		t.Run("sensitive/"+boundary.name, func(t *testing.T) {
			input := boundary.prefix + "room_id=private-boundary\nstatus=746"
			clean := RedactText(input)
			if strings.Contains(clean, "private-boundary") || !strings.Contains(clean, "[REDACTED]") ||
				!strings.HasSuffix(clean, "\nstatus=746") {
				t.Fatalf("terminator boundary %q did not redact line-bounded: %q", boundary.prefix, clean)
			}
		})
		for _, safeKey := range []string{
			"cooperation", "precursor", "valid", "fluid", "grid", "paid", "classroom", "headroom",
		} {
			t.Run(fmt.Sprintf("safe-%02d/%s", index, safeKey), func(t *testing.T) {
				input := boundary.prefix + safeKey + "=safe"
				if clean := RedactText(input); clean != input {
					t.Fatalf("safe key after %q changed: got %q want %q", boundary.prefix, clean, input)
				}
			})
		}
	}
}

func TestRedactTextInvalidUTF8FailsClosedPerLine(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "invalid before structural boundary",
			input: "prefix" + string([]byte{0xff}) + "}room_id=private-invalid\r\nstatus=744",
			want:  "[REDACTED]\r\nstatus=744",
		},
		{
			name:  "invalid after comma",
			input: "prefix," + string([]byte{0xc0}) + "room_id=private-invalid\nstatus=745",
			want:  "[REDACTED]\nstatus=745",
		},
		{
			name:  "truncated sequence without sensitive key",
			input: "ordinary" + string([]byte{0xc3}) + "text\rstatus=746",
			want:  "[REDACTED]\rstatus=746",
		},
		{
			name:  "mixed line separators",
			input: "status=1\r\nbad" + string([]byte{0xff}) + "line\rstatus=2\n",
			want:  "status=1\r\n[REDACTED]\rstatus=2\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clean := RedactText(test.input)
			if clean != test.want {
				t.Fatalf("RedactText() = %q, want %q", clean, test.want)
			}
			if !utf8.ValidString(clean) {
				t.Fatalf("RedactText() emitted invalid UTF-8: %q", clean)
			}
			if second := RedactText(clean); second != clean {
				t.Fatalf("invalid UTF-8 redaction is not idempotent: first %q second %q", clean, second)
			}
		})
	}
}

func TestRedactTextTokenizerPreservesStandaloneQuotedCompositeItems(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "plain single and double quoted array items",
			input: `message=['token=private-array',"status=SAFE"],code=756`,
			want:  `message=['[REDACTED]',"status=SAFE"],code=756`,
		},
		{
			name:  "one escaped layer array items",
			input: `message=[\"token=private-one\",\"status=SAFE\"],code=757`,
			want:  `message=[\"[REDACTED]\",\"status=SAFE\"],code=757`,
		},
		{
			name:  "two escaped layers array items",
			input: `message=[\\\"token=private-two\\\",\\\"status=SAFE\\\"],code=758`,
			want:  `message=[\\\"[REDACTED]\\\",\\\"status=SAFE\\\"],code=758`,
		},
		{
			name:  "mixed object and nested array",
			input: `message={"items":['authorization=private-auth',\"title=private-title\"],"status":759},code=SAFE`,
			want:  `message={"items":['[REDACTED]',\"[REDACTED]\"],"status":759},code=SAFE`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clean := RedactText(test.input)
			if clean != test.want {
				t.Fatalf("RedactText() = %q, want exact structure %q", clean, test.want)
			}
			if second := RedactText(clean); second != clean {
				t.Fatalf("standalone quoted redaction is not idempotent: first %q second %q", clean, second)
			}
		})
	}
}

func TestRedactTextPreservesBareURLAssignmentTails(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "safe bare message URL",
			input: `message=https://private-url-tail.invalid/x status=730`,
			want:  `message=[REDACTED-URL] status=730`,
		},
		{
			name:  "safe escaped endpoint URL",
			input: `endpoint=https:\/\/private-url-tail.invalid\/x status=731`,
			want:  `endpoint=[REDACTED-URL] status=731`,
		},
		{
			name:  "sensitive bare URL key",
			input: `url=https://private-url-tail.invalid/x status=732`,
			want:  `[REDACTED] status=732`,
		},
		{
			name:  "safe Unicode endpoint URL",
			input: `endpoint=h\u0074tps\u003a\u002f\u002fprivate-url-tail.invalid\u002fx\u0020status=733`,
			want:  `endpoint=[REDACTED-URL]\u0020status=733`,
		},
		{
			name:  "safe quoted message URL",
			input: `message="https:\/\/private-url-tail.invalid\/x" status=734`,
			want:  `message="[REDACTED-URL]" status=734`,
		},
		{
			name:  "sensitive quoted URL key",
			input: `url="https://private-url-tail.invalid/x" status=735`,
			want:  `[REDACTED] status=735`,
		},
		{
			name:  "encoded tab after URL marker",
			input: `endpoint=https:\/\/private-url-tail.invalid\/x\tstatus=736`,
			want:  `endpoint=[REDACTED-URL]\tstatus=736`,
		},
		{
			name:  "two layer encoded space after URL marker",
			input: `endpoint=https:\\/\\/private-url-tail.invalid\\/x\\u0020status=737`,
			want:  `endpoint=[REDACTED-URL]\\u0020status=737`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clean := RedactText(test.input)
			if clean != test.want {
				t.Fatalf("RedactText() = %q, want %q", clean, test.want)
			}
			if strings.Contains(clean, "private-url-tail.invalid") {
				t.Fatalf("bare URL assignment leaked: %q", clean)
			}
		})
	}
}

func TestTerminatorURLAndStandaloneContainersAcrossStructuredIngresses(t *testing.T) {
	message := strings.Join([]string{
		`{:title=private-slog-title},status=770`,
		`message=https://private-message-url.invalid/x status=773`,
		`url="h\u0074tps\u003a\u002f\u002fprivate-sensitive-url.invalid\u002fx" status=774`,
	}, "\n")
	errorValue := errors.New(`endpoint=https:\/\/private-ingress.invalid\/x status=771`)
	stringerValue := quotedJSONLogStringer(`message=['token=private-stringer-token',"status=SAFE"],code=772`)

	var output bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&output, nil))
	logger.Info(message, "err", errorValue, "detail", stringerValue)
	slogText := output.String()
	for _, forbidden := range []string{
		"private-slog-title", "private-message-url.invalid", "private-sensitive-url.invalid",
		"private-ingress.invalid", "private-stringer-token",
	} {
		if strings.Contains(slogText, forbidden) {
			t.Fatalf("slog ingress retained %q: %s", forbidden, slogText)
		}
	}
	for _, preserved := range []string{"status=770", "status=771", "status=SAFE", "code=772", "status=773", "status=774"} {
		if !strings.Contains(slogText, preserved) {
			t.Fatalf("slog ingress lost %q: %s", preserved, slogText)
		}
	}

	legacy := RedactKeyValueArgs([]any{
		"message", message,
		"err", errorValue,
		"detail", stringerValue,
	})
	legacyText := fmt.Sprint(legacy...)
	for _, forbidden := range []string{
		"private-slog-title", "private-message-url.invalid", "private-sensitive-url.invalid",
		"private-ingress.invalid", "private-stringer-token",
	} {
		if strings.Contains(legacyText, forbidden) {
			t.Fatalf("legacy/custom ingress retained %q: %s", forbidden, legacyText)
		}
	}
	for _, preserved := range []string{"status=770", "status=771", "status=SAFE", "code=772", "status=773", "status=774"} {
		if !strings.Contains(legacyText, preserved) {
			t.Fatalf("legacy/custom ingress lost %q: %s", preserved, legacyText)
		}
	}
}

func TestRedactTextRejectsGlobalControlsExceptTab(t *testing.T) {
	controls := []struct {
		name  string
		value rune
	}{
		{name: "NUL", value: '\x00'},
		{name: "SOH", value: '\x01'},
		{name: "BEL", value: '\x07'},
		{name: "backspace", value: '\x08'},
		{name: "vertical tab", value: '\x0b'},
		{name: "form feed", value: '\x0c'},
		{name: "ESC", value: '\x1b'},
		{name: "unit separator", value: '\x1f'},
		{name: "DEL", value: '\x7f'},
		{name: "next line", value: '\u0085'},
		{name: "application program command", value: '\u009f'},
	}
	for _, control := range controls {
		t.Run(control.name+"/safe-only", func(t *testing.T) {
			input := "safe" + string(control.value) + "tail\r\nstatus=780"
			clean := RedactText(input)
			if clean != "[REDACTED]\r\nstatus=780" {
				t.Fatalf("safe-only control line = %q", clean)
			}
			if !utf8.ValidString(clean) || strings.ContainsRune(clean, control.value) {
				t.Fatalf("control or invalid UTF-8 survived: %q", clean)
			}
			if second := RedactText(clean); second != clean {
				t.Fatalf("control redaction is not idempotent: first %q second %q", clean, second)
			}
		})
		t.Run(control.name+"/mixed-sensitive", func(t *testing.T) {
			input := "prefix" + string(control.value) + "tail room_id=private-control-secret\nstatus=781"
			clean := RedactText(input)
			if clean != "[REDACTED]\nstatus=781" || strings.Contains(clean, "private-control-secret") {
				t.Fatalf("mixed control line was not fail-closed: %q", clean)
			}
		})
	}

	for _, safe := range []string{
		"safe\ttail",
		"status\t=782",
		"alpha\r\nbeta\rgamma\ndelta",
	} {
		if clean := RedactText(safe); clean != safe {
			t.Fatalf("allowed tab or line separators changed: got %q want %q", clean, safe)
		}
	}
	tabSensitive := RedactText("room_id\t=private-tab-secret\nstatus=783")
	if tabSensitive != "[REDACTED]\nstatus=783" {
		t.Fatalf("TAB-sensitive assignment changed semantics: %q", tabSensitive)
	}
}

func TestGlobalControlGateAcrossStructuredIngresses(t *testing.T) {
	message := "safe\x1btail\nstatus=790"
	errorValue := errors.New("prefix\u0085tail token=private-control-secret\ncode=SAFE_CONTROL")
	stringerValue := quotedJSONLogStringer("safe\x7ftail\r\nstatus=791")

	var output bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&output, nil))
	logger.Info(message,
		"err", errorValue,
		"detail", stringerValue,
		"tab", "safe\ttab",
	)
	slogText := output.String()
	for _, forbidden := range []string{"private-control-secret", "safe\\u001b", "safe\\u007f", "safe\u0085"} {
		if strings.Contains(slogText, forbidden) {
			t.Fatalf("control ingress retained %q: %s", forbidden, slogText)
		}
	}
	for _, preserved := range []string{"status=790", "SAFE_CONTROL", "status=791", `safe\ttab`} {
		if !strings.Contains(slogText, preserved) {
			t.Fatalf("slog control gate lost %q: %s", preserved, slogText)
		}
	}

	legacy := RedactKeyValueArgs([]any{
		"message", message,
		"err", errorValue,
		"detail", stringerValue,
		"tab", "safe\ttab",
	})
	legacyText := fmt.Sprint(legacy...)
	for _, forbidden := range []string{"private-control-secret", "\x1b", "\x7f", "\u0085"} {
		if strings.Contains(legacyText, forbidden) {
			t.Fatalf("legacy/custom control ingress retained %q: %q", forbidden, legacyText)
		}
	}
	for _, preserved := range []string{"status=790", "SAFE_CONTROL", "status=791", "safe\ttab"} {
		if !strings.Contains(legacyText, preserved) {
			t.Fatalf("legacy/custom control gate lost %q: %q", preserved, legacyText)
		}
	}
}

func TestURLMarkerQuoteBoundariesAcrossIngresses(t *testing.T) {
	views := []struct {
		name string
		url  string
	}{
		{name: "plain", url: `https://quote-private.invalid/x`},
		{name: "one encoded layer", url: `https:\/\/quote-private.invalid\/x`},
		{name: "two encoded layers", url: `https:\\/\\/quote-private.invalid\\/x`},
	}
	keys := []struct {
		name      string
		value     string
		sensitive bool
	}{
		{name: "safe endpoint", value: "endpoint"},
		{name: "sensitive url", value: "url", sensitive: true},
	}
	terminators := []struct {
		name  string
		value string
	}{
		{name: "plain double quote", value: `"`},
		{name: "plain single quote", value: `'`},
		{name: "one layer double quote", value: `\"`},
		{name: "one layer single quote", value: `\'`},
		{name: "two layer double quote", value: `\\\"`},
		{name: "two layer single quote", value: `\\\'`},
	}

	for _, view := range views {
		for _, key := range keys {
			for _, terminator := range terminators {
				name := view.name + "/" + key.name + "/" + terminator.name
				t.Run(name, func(t *testing.T) {
					input := key.value + "=" + view.url + terminator.value + "status=END"
					want := key.value + "=[REDACTED-URL]" + terminator.value + "status=END"
					if key.sensitive {
						want = "[REDACTED]" + terminator.value + "status=END"
					}
					clean := RedactText(input)
					if clean != want {
						t.Fatalf("RedactText() = %q, want %q", clean, want)
					}
					if strings.Contains(clean, "quote-private.invalid") || RedactText(clean) != clean {
						t.Fatalf("quote boundary leaked or was not idempotent: %q", clean)
					}

					var output bytes.Buffer
					logger := slog.New(NewRedactingJSONHandler(&output, nil))
					logger.Info(input)
					var record map[string]any
					if err := json.Unmarshal(output.Bytes(), &record); err != nil {
						t.Fatalf("Unmarshal() error = %v", err)
					}
					if got := record["msg"]; got != want {
						t.Fatalf("slog msg = %#v, want %q", got, want)
					}

					legacy := RedactKeyValueArgs([]any{"message", input})
					if got := legacy[1]; got != want {
						t.Fatalf("legacy/custom value = %#v, want %q", got, want)
					}
				})
			}
		}
	}

	for _, suffix := range []string{
		`\xstatus=END`,
		`\\"status=END`,
		`\\\\"status=END`,
	} {
		input := "endpoint=[REDACTED-URL]" + suffix
		if clean := RedactText(input); clean != "[REDACTED]" {
			t.Fatalf("non-quote backslash boundary %q was accepted: %q", suffix, clean)
		}
	}
}

func decodeV8LogRecord(t *testing.T, output *bytes.Buffer) map[string]any {
	t.Helper()
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; output=%q", err, output.String())
	}
	return record
}

func TestStructuredLogNamesAreValidatedBeforeMetricsAcrossIngresses(t *testing.T) {
	invalidUTF8 := string([]byte{'k', 0xff})
	nameCases := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "ordinary", value: "status", want: true},
		{name: "sensitive but structurally valid", value: "room_id", want: true},
		{name: "safe metric", value: "attempt_len", want: true},
		{name: "safe collision", value: "roomvalue", want: true},
		{name: "invalid UTF-8", value: invalidUTF8},
		{name: "NUL", value: "attempt_len\x00"},
		{name: "TAB", value: "attempt_len\t"},
		{name: "Unicode control", value: "field\u0085name"},
		{name: "URL", value: "https://structured-key.invalid/x"},
		{name: "embedded sensitive assignment", value: "prefix room_id=private"},
	}
	for _, test := range nameCases {
		t.Run("predicate/"+test.name, func(t *testing.T) {
			if got := safeStructuredLogName(test.value); got != test.want {
				t.Fatalf("safeStructuredLogName(%q) = %v, want %v", test.value, got, test.want)
			}
		})
	}

	unsafeKeys := []struct {
		name string
		key  string
	}{
		{name: "sensitive", key: "room_id"},
		{name: "invalid UTF-8", key: invalidUTF8},
		{name: "control before metric", key: "attempt_len\x00"},
		{name: "URL", key: "https://structured-key.invalid/x"},
		{name: "assignment", key: "prefix token=private"},
	}
	for _, test := range unsafeKeys {
		t.Run("direct/"+test.name, func(t *testing.T) {
			var output bytes.Buffer
			slog.New(NewRedactingJSONHandler(&output, nil)).Info("direct", slog.Int64(test.key, 7))
			record := decodeV8LogRecord(t, &output)
			if got := record["redacted_field"]; got != "[REDACTED]" {
				t.Fatalf("direct unsafe key marker = %#v; record=%#v", got, record)
			}
		})
		t.Run("WithAttrs/"+test.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := slog.New(NewRedactingJSONHandler(&output, nil)).With(slog.Int64(test.key, 7))
			logger.Info("with attrs")
			record := decodeV8LogRecord(t, &output)
			if got := record["redacted_field"]; got != "[REDACTED]" {
				t.Fatalf("WithAttrs unsafe key marker = %#v; record=%#v", got, record)
			}
		})
		t.Run("GroupValue/"+test.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := slog.New(NewRedactingJSONHandler(&output, nil))
			logger.Info("group value", slog.Any("payload", slog.GroupValue(slog.Int64(test.key, 7))))
			record := decodeV8LogRecord(t, &output)
			payload, ok := record["payload"].(map[string]any)
			if !ok || payload["redacted_field"] != "[REDACTED]" {
				t.Fatalf("GroupValue unsafe key was not marked: %#v", record)
			}
		})
		t.Run("GroupValueName/"+test.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := slog.New(NewRedactingJSONHandler(&output, nil))
			logger.Info("group value name", slog.Any(test.key, slog.GroupValue(slog.String("status", "must-not-survive"))))
			record := decodeV8LogRecord(t, &output)
			group, ok := record["redacted_group"].(map[string]any)
			if !ok || group["redacted_field"] != "[REDACTED]" || group["status"] != nil {
				t.Fatalf("unsafe GroupValue name did not rename and redact all: %#v", record)
			}
		})
	}

	var groupOutput bytes.Buffer
	groupName := "scope\x00room_id=private"
	slog.New(NewRedactingJSONHandler(&groupOutput, nil)).
		WithGroup(groupName).
		Info("unsafe group", "status", "must-not-survive", "attempt_len", 9)
	groupRecord := decodeV8LogRecord(t, &groupOutput)
	group, ok := groupRecord["redacted_group"].(map[string]any)
	if !ok || group["redacted_field"] != "[REDACTED]" ||
		group["status"] != nil || group["attempt_len"] != nil {
		t.Fatalf("unsafe WithGroup did not rename and redact all children: %#v", groupRecord)
	}

	var safeMetricOutput bytes.Buffer
	slog.New(NewRedactingJSONHandler(&safeMetricOutput, nil)).Info("safe metric", "attempt_len", 9)
	if got := decodeV8LogRecord(t, &safeMetricOutput)["attempt_len"]; got != float64(9) {
		t.Fatalf("safe numeric length metric = %#v", got)
	}

	legacy := RedactKeyValueArgs([]any{
		"attempt_len\x00", 9,
		invalidUTF8, 8,
		"room_id", "private",
		"attempt_len", 7,
	})
	for _, index := range []int{0, 2, 4} {
		if legacy[index] != "redacted_field" || legacy[index+1] != "[REDACTED]" {
			t.Fatalf("legacy unsafe key pair at %d = %#v", index, legacy[index:index+2])
		}
	}
	if legacy[6] != "attempt_len" || legacy[7] != 7 {
		t.Fatalf("legacy safe metric changed: %#v", legacy[6:8])
	}
	odd := RedactKeyValueArgs([]any{"status", "SAFE", "prefix token=private"})
	if odd[2] != "redacted_field" {
		t.Fatalf("legacy dangling unsafe key = %#v", odd)
	}
}

func TestRedactingHandlerWrapsReplaceAttrExactlyOnceAndRedactsItsResult(t *testing.T) {
	type replaceCall struct {
		count  int
		groups string
	}
	calls := make(map[string]replaceCall)
	replace := func(groups []string, attr slog.Attr) slog.Attr {
		call := calls[attr.Key]
		call.count++
		call.groups = strings.Join(groups, "/")
		calls[attr.Key] = call
		switch attr.Key {
		case slog.TimeKey:
			return slog.String(slog.TimeKey, "https://replace-time.invalid/private")
		case slog.LevelKey:
			return slog.String(slog.LevelKey, "token=replace-level-private")
		case slog.SourceKey:
			return slog.String(slog.SourceKey, "source\x1bprivate")
		case slog.MessageKey:
			return slog.String(slog.MessageKey, "https://replace-message.invalid/private")
		case "drop":
			return slog.Attr{}
		case "ordinary":
			return slog.String("room_id", "replace-ordinary-private")
		case "with_attr":
			return slog.String("with_attr", "https://replace-with.invalid/private")
		case "group_child":
			return slog.String("group_child", "token=replace-group-private")
		case "err":
			return slog.Any("err", errors.New("https://replace-error.invalid/private"))
		case "stringer":
			return slog.Any("stringer", quotedJSONLogStringer("room_id=replace-stringer-private"))
		default:
			return attr
		}
	}
	options := &slog.HandlerOptions{AddSource: true, Level: slog.LevelDebug, ReplaceAttr: replace}
	replacePointer := reflect.ValueOf(options.ReplaceAttr).Pointer()
	var output bytes.Buffer
	handler := NewRedactingJSONHandler(&output, options)
	if !options.AddSource || options.Level != slog.LevelDebug || options.ReplaceAttr == nil ||
		reflect.ValueOf(options.ReplaceAttr).Pointer() != replacePointer {
		t.Fatalf("NewRedactingJSONHandler mutated caller options: %#v", options)
	}

	logger := slog.New(handler).With("with_attr", "safe").WithGroup("scope")
	logger.Info("safe message",
		"ordinary", "safe",
		"drop", "safe",
		"payload", slog.GroupValue(slog.String("group_child", "safe")),
		"err", errors.New("safe"),
		"stringer", quotedJSONLogStringer("safe"),
	)
	for _, key := range []string{
		slog.TimeKey, slog.LevelKey, slog.SourceKey, slog.MessageKey,
		"with_attr", "ordinary", "drop", "group_child", "err", "stringer",
	} {
		if calls[key].count != 1 {
			t.Fatalf("ReplaceAttr call count for %q = %d, want 1; all=%#v", key, calls[key].count, calls)
		}
	}
	if calls["with_attr"].groups != "" || calls["ordinary"].groups != "scope" ||
		calls["group_child"].groups != "scope/payload" {
		t.Fatalf("ReplaceAttr groups = with:%q ordinary:%q child:%q", calls["with_attr"].groups, calls["ordinary"].groups, calls["group_child"].groups)
	}

	text := output.String()
	for _, forbidden := range []string{
		"replace-time.invalid", "replace-level-private", "source\\u001bprivate",
		"replace-message.invalid", "replace-ordinary-private", "replace-with.invalid",
		"replace-group-private", "replace-error.invalid", "replace-stringer-private",
		`"drop"`,
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("ReplaceAttr result leaked %q: %s", forbidden, text)
		}
	}
	for _, marker := range []string{"[REDACTED]", "[REDACTED-URL]", "redacted_field"} {
		if !strings.Contains(text, marker) {
			t.Fatalf("ReplaceAttr result is missing %q: %s", marker, text)
		}
	}
}

func encodeV8LogURLLayer(value string) string {
	return strings.NewReplacer(`\`, `\\`, "/", `\/`).Replace(value)
}

func TestLogURLViewsMalformedLocatorsAndStructuralTails(t *testing.T) {
	canonical := "https://authority.invalid/path"
	for layer := 0; layer <= maximumLogURLDecodeLayers; layer++ {
		encoded := canonical
		for pass := 0; pass < layer; pass++ {
			encoded = encodeV8LogURLLayer(encoded)
		}
		view := newLogURLView(encoded)
		for pass := 0; pass <= layer; pass++ {
			_, found := logURLEndAt(view, 0)
			if found != (pass == layer) {
				t.Fatalf("layer=%d pass=%d literal // match=%v; view=%q", layer, pass, found, view.text)
			}
			if pass < layer {
				var changed bool
				view, changed = decodeLogURLViewLayer(view)
				if !changed {
					t.Fatalf("layer=%d pass=%d did not decode", layer, pass)
				}
			}
		}
	}
	if _, found := logURLEndAt(newLogURLView(`http:\keep`), 0); found {
		t.Fatal("single backslash plus ordinary letter was treated as //")
	}
	if got := RedactText(`http:\keep`); got != `http:\keep` {
		t.Fatalf("single ordinary backslash candidate changed: %q", got)
	}

	for count := 2; count <= 8; count++ {
		input := "endpoint=https:" + strings.Repeat(`\`, count) + "target.invalid/path status=801"
		clean := RedactText(input)
		if strings.Contains(clean, "target.invalid") || strings.Count(clean, "[REDACTED-URL]") != 1 ||
			!strings.Contains(clean, "status=801") || RedactText(clean) != clean {
			t.Fatalf("%d-backslash candidate was partial/non-idempotent: %q", count, clean)
		}
	}
	candidates := []string{
		`https:\/candidate.invalid/path`,
		`https:/\/candidate.invalid/path`,
		`https:\//candidate.invalid/path`,
		`https:\u002f\u002fcandidate.invalid/path`,
		`https:\\u002f\\u002fcandidate.invalid/path`,
		`https:/\\\\candidate.invalid/path`,
	}
	for index, candidate := range candidates {
		input := "endpoint=" + candidate + " status=802"
		clean := RedactText(input)
		if strings.Contains(clean, "candidate.invalid") || strings.Count(clean, "[REDACTED-URL]") != 1 ||
			!strings.Contains(clean, "status=802") || RedactText(clean) != clean {
			t.Fatalf("candidate %d was partial/non-idempotent: input=%q clean=%q", index, input, clean)
		}
	}

	punctuation := []string{",", ";", ")", "]", "}"}
	for _, mark := range punctuation {
		safeInput := "endpoint=https://tail.invalid/x" + mark + "status=SAFE"
		safeWant := "endpoint=[REDACTED-URL]" + mark + "status=SAFE"
		if got := RedactText(safeInput); got != safeWant {
			t.Fatalf("safe tail %q = %q, want %q", mark, got, safeWant)
		}
		sensitiveInput := "endpoint=https://tail.invalid/x" + mark + "room_id=SECRET,status=SAFE"
		sensitiveWant := "endpoint=[REDACTED-URL]" + mark + "[REDACTED],status=SAFE"
		if got := RedactText(sensitiveInput); got != sensitiveWant {
			t.Fatalf("sensitive tail %q = %q, want %q", mark, got, sensitiveWant)
		}
		unknownInput := "endpoint=https://tail.invalid/x" + mark + "foo=secret"
		if got := RedactText(unknownInput); got != "endpoint=[REDACTED-URL]" {
			t.Fatalf("unknown tail %q escaped URL range: %q", mark, got)
		}
	}
	if got := RedactText("url=https://tail.invalid/x,status=SAFE"); got != "[REDACTED],status=SAFE" {
		t.Fatalf("sensitive URL key lost safe status: %q", got)
	}

	encodedSensitive := `endpoint=https:\/\/a.invalid\/x\u0020token=private status=809`
	encodedWant := `endpoint=[REDACTED-URL]\u0020[REDACTED]status=809`
	if got := RedactText(encodedSensitive); got != encodedWant || RedactText(got) != got {
		t.Fatalf("encoded boundary sensitive tail = %q, want stable %q", got, encodedWant)
	}
	encodedUnknown := `endpoint=https:\/\/a.invalid\/x\u0020foo=secret`
	unknownWant := `endpoint=[REDACTED-URL]\u0020foo=secret`
	if got := RedactText(encodedUnknown); got != unknownWant || RedactText(got) != got {
		t.Fatalf("encoded boundary ordinary tail = %q, want stable %q", got, unknownWant)
	}
}

func TestLogURLCloserRunIsStructurallyLinear(t *testing.T) {
	closers := strings.Repeat(")", 32768)
	input := "https://audit.invalid/x" + closers
	view := newLogURLView(input)
	afterScheme, found := logURLSchemeEndAt(input, 0)
	if !found {
		t.Fatal("test URL scheme was not recognized")
	}
	end, scanned := logURLTokenEndScanned(view, afterScheme+2)
	if end != len(input) {
		t.Fatalf("closer run ended at %d, want %d", end, len(input))
	}
	if scanned > len(input)*4 {
		t.Fatalf("closer run inspected %d units for %d-byte input", scanned, len(input))
	}
	if got := RedactText(input); got != "[REDACTED-URL]" || RedactText(got) != got {
		t.Fatalf("large closer run redaction = %q", got)
	}
}

func TestMalformedQuotedKeyUsesCanonicalClassification(t *testing.T) {
	prefixes := []string{`"`, `\"`, `\\\"`}
	sensitive := []string{
		"room_id", "session_id_text", "title_text", "cursor_value", `room\u005fid`,
	}
	for _, prefix := range prefixes {
		for _, key := range sensitive {
			input := prefix + key + "=private"
			if got := RedactText(input); got != "[REDACTED]" || RedactText(got) != got {
				t.Fatalf("malformed sensitive key prefix=%q key=%q = %q", prefix, key, got)
			}
		}
	}
	safe := []string{
		"gapped", "roomvalue", "pidvalue", "gapvalue", "room_count", "attempt_count",
		"valid", "fluid", "grid", "paid", "classroom", "headroom",
	}
	for _, prefix := range prefixes {
		for _, key := range safe {
			input := prefix + key + "=ordinary"
			if got := RedactText(input); got != input {
				t.Fatalf("malformed safe key prefix=%q key=%q changed: %q", prefix, key, got)
			}
		}
	}
}

func TestLogURLBoundaryNormalizationHasBoundedLinearWork(t *testing.T) {
	segment := `https:\/\/scale.invalid\/path\t`
	previousWork := 0
	for _, count := range []int{32, 64, 128, 256, 512} {
		input := strings.Repeat(segment, count)
		var stats logURLScanStats
		clean := redactLogURLsWithStats(input, &stats)
		if strings.Contains(clean, "scale.invalid") ||
			strings.Count(clean, "[REDACTED-URL]") != count {
			t.Fatalf("count=%d URL normalization incomplete: %q", count, clean)
		}
		if stats.boundaryProbes == 0 ||
			stats.boundaryLookaheadBytes > stats.boundaryProbes*maximumLogURLBoundaryLookaheadBytes {
			t.Fatalf("count=%d unbounded boundary stats: %#v", count, stats)
		}
		if stats.boundaryProbes > count*(maximumLogURLDecodeLayers+1) {
			t.Fatalf("count=%d boundary probes grew faster than candidates: %#v", count, stats)
		}
		if stats.decodedViewBytes > len(input)*maximumLogURLDecodeLayers {
			t.Fatalf("count=%d decoded %d bytes for %d-byte input", count, stats.decodedViewBytes, len(input))
		}
		work := stats.decodedViewBytes + stats.boundaryLookaheadBytes
		if previousWork > 0 && work > previousWork*3 {
			t.Fatalf("count=%d work scaled superlinearly: previous=%d current=%d stats=%#v", count, previousWork, work, stats)
		}
		previousWork = work
		if second := RedactText(clean); second != clean {
			t.Fatalf("count=%d normalized output is not idempotent", count)
		}
	}
}

func TestAdjacentCompositeAndRedactionMarkerRemainIdempotent(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "object then quoted sensitive assignment",
			input: `w={}"room_id"="private"`,
			want:  `w={}[REDACTED]`,
		},
		{
			name:  "array then quoted sensitive assignment",
			input: `w=[]"token"="private"`,
			want:  `w=[][REDACTED]`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			first := RedactText(test.input)
			if first != test.want {
				t.Fatalf("first RedactText() = %q, want %q", first, test.want)
			}
			if second := RedactText(first); second != first {
				t.Fatalf("second RedactText() = %q, want stable %q", second, first)
			}
			if strings.Contains(first, "private") || strings.Contains(first, "room_id") {
				t.Fatalf("sensitive adjacent assignment leaked: %q", first)
			}
		})
	}
}

func TestMalformedRawURLPreservesEncodedTerminators(t *testing.T) {
	tests := []struct {
		name   string
		suffix string
	}{
		{name: "unicode space", suffix: `\u0020status=SAFE`},
		{name: "encoded tab", suffix: `\tstatus=SAFE`},
		{name: "escaped quote", suffix: `\"status=SAFE`},
		{name: "two-layer escaped quote", suffix: `\\\"status=SAFE`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := `endpoint=https:\\host.invalid/path` + test.suffix
			want := "endpoint=[REDACTED-URL]" + test.suffix
			first := RedactText(input)
			if first != want {
				t.Fatalf("RedactText() = %q, want %q", first, want)
			}
			if strings.Contains(first, "host.invalid") || RedactText(first) != first {
				t.Fatalf("malformed URL leaked or was not idempotent: %q", first)
			}
		})
	}
}

type v9PanickingError struct {
	calls *int
}

func (value *v9PanickingError) Error() string {
	*value.calls++
	panic("private panic error")
}

type v9PanickingStringer struct {
	calls *int
}

func (value *v9PanickingStringer) String() string {
	*value.calls++
	panic("private panic stringer")
}

type v9TypedNilError struct{}

func (value *v9TypedNilError) Error() string {
	if value == nil {
		panic("typed nil error")
	}
	return "private typed error"
}

type v9TypedNilStringer struct{}

func (value *v9TypedNilStringer) String() string {
	if value == nil {
		panic("typed nil stringer")
	}
	return "private typed stringer"
}

func TestPanickingAndTypedNilLogValuesFailClosedAcrossIngresses(t *testing.T) {
	slogErrorCalls := 0
	slogStringerCalls := 0
	var nilError *v9TypedNilError
	var nilStringer *v9TypedNilStringer
	var output bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&output, nil))
	logger.Info("panic-safe",
		"panic_error", &v9PanickingError{calls: &slogErrorCalls},
		"panic_stringer", &v9PanickingStringer{calls: &slogStringerCalls},
		"nil_error", nilError,
		"nil_stringer", nilStringer,
	)
	if slogErrorCalls != 1 || slogStringerCalls != 1 {
		t.Fatalf("slog formatter calls = error:%d stringer:%d, want exactly once", slogErrorCalls, slogStringerCalls)
	}
	record := decodeV8LogRecord(t, &output)
	for _, key := range []string{"panic_error", "panic_stringer", "nil_error", "nil_stringer"} {
		if got := record[key]; got != "[REDACTED]" {
			t.Fatalf("slog %s = %#v; record=%#v", key, got, record)
		}
	}

	legacyErrorCalls := 0
	legacyStringerCalls := 0
	legacy := RedactKeyValueArgs([]any{
		"panic_error", &v9PanickingError{calls: &legacyErrorCalls},
		"panic_stringer", &v9PanickingStringer{calls: &legacyStringerCalls},
		"nil_error", nilError,
		"nil_stringer", nilStringer,
	})
	if legacyErrorCalls != 1 || legacyStringerCalls != 1 {
		t.Fatalf("legacy formatter calls = error:%d stringer:%d, want exactly once", legacyErrorCalls, legacyStringerCalls)
	}
	for _, index := range []int{1, 3, 5, 7} {
		if legacy[index] != "[REDACTED]" {
			t.Fatalf("legacy value at %d = %#v; all=%#v", index, legacy[index], legacy)
		}
	}
}

type v10NamedStringKey string

type v10CountingKeyStringer struct {
	calls *int
	text  string
}

func (value *v10CountingKeyStringer) String() string {
	*value.calls++
	return value.text
}

type v10CountingKeyError struct {
	calls *int
	text  string
}

func (value *v10CountingKeyError) Error() string {
	*value.calls++
	return value.text
}

type v10PanickingKeyStringer struct {
	calls *int
}

func (value *v10PanickingKeyStringer) String() string {
	*value.calls++
	panic("opaque-v10-private-panic-payload")
}

type v10PanickingKeyError struct {
	calls *int
}

func (value *v10PanickingKeyError) Error() string {
	*value.calls++
	panic("opaque-v10-private-panic-payload")
}

var v10TypedNilKeyStringerCalls int

type v10TypedNilKeyStringer struct{}

func (*v10TypedNilKeyStringer) String() string {
	v10TypedNilKeyStringerCalls++
	panic("opaque-v10-private-panic-payload")
}

var v10TypedNilKeyErrorCalls int

type v10TypedNilKeyError struct{}

func (*v10TypedNilKeyError) Error() string {
	v10TypedNilKeyErrorCalls++
	panic("opaque-v10-private-panic-payload")
}

func TestLegacyNonStringKeysNeverInvokeFormattersAndFailClosed(t *testing.T) {
	countingStringerCalls := 0
	countingErrorCalls := 0
	stringerCalls := 0
	errorCalls := 0
	v10TypedNilKeyStringerCalls = 0
	v10TypedNilKeyErrorCalls = 0
	var nilStringer *v10TypedNilKeyStringer
	var nilError *v10TypedNilKeyError

	args := []any{
		42, "number-private",
		v10NamedStringKey("status"), "named-string-private",
		&v10CountingKeyStringer{calls: &countingStringerCalls, text: "ordinary-stringer-private-key"}, "ordinary-stringer-private",
		&v10CountingKeyError{calls: &countingErrorCalls, text: "ordinary-error-private-key"}, "ordinary-error-private",
		&v10PanickingKeyStringer{calls: &stringerCalls}, "stringer-private",
		&v10PanickingKeyError{calls: &errorCalls}, "error-private",
		nilStringer, "nil-stringer-private",
		nilError, "nil-error-private",
		"status", "SAFE",
	}
	original := append([]any(nil), args...)
	clean := RedactKeyValueArgs(args)
	want := []any{
		"redacted_field", "[REDACTED]",
		"redacted_field", "[REDACTED]",
		"redacted_field", "[REDACTED]",
		"redacted_field", "[REDACTED]",
		"redacted_field", "[REDACTED]",
		"redacted_field", "[REDACTED]",
		"redacted_field", "[REDACTED]",
		"redacted_field", "[REDACTED]",
		"status", "SAFE",
	}
	if !reflect.DeepEqual(clean, want) {
		t.Fatal("RedactKeyValueArgs() did not fail closed for every non-string key")
	}
	if !reflect.DeepEqual(args, original) {
		t.Fatal("RedactKeyValueArgs() mutated its input")
	}
	if countingStringerCalls != 0 || countingErrorCalls != 0 || stringerCalls != 0 || errorCalls != 0 ||
		v10TypedNilKeyStringerCalls != 0 || v10TypedNilKeyErrorCalls != 0 {
		t.Fatalf("key formatter calls = ordinary-stringer:%d ordinary-error:%d panic-stringer:%d panic-error:%d nil-stringer:%d nil-error:%d, want zero",
			countingStringerCalls, countingErrorCalls, stringerCalls, errorCalls,
			v10TypedNilKeyStringerCalls, v10TypedNilKeyErrorCalls)
	}

	var output bytes.Buffer
	slog.New(NewRedactingJSONHandler(&output, nil)).Info("legacy-key-safe", clean...)
	text := output.String()
	for _, forbidden := range []string{
		"opaque-v10-private-panic-payload",
		"number-private", "named-string-private", "stringer-private",
		"error-private", "ordinary-stringer-private", "ordinary-error-private",
		"nil-stringer-private", "nil-error-private",
		"%!",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("final JSON leaked forbidden marker %q", forbidden)
		}
	}
	if got := strings.Count(text, `"redacted_field":"[REDACTED]"`); got != 8 {
		t.Fatalf("final JSON redacted pair count = %d, want 8", got)
	}
	record := decodeV8LogRecord(t, &output)
	if got := record["redacted_field"]; got != "[REDACTED]" {
		t.Fatalf("final JSON redacted_field = %#v", got)
	}
	if got := record["status"]; got != "SAFE" {
		t.Fatalf("final JSON status = %#v, want SAFE", got)
	}

	oddKeys := []any{
		99,
		v10NamedStringKey("status"),
		&v10CountingKeyStringer{calls: &countingStringerCalls, text: "ordinary-stringer-private-key"},
		&v10CountingKeyError{calls: &countingErrorCalls, text: "ordinary-error-private-key"},
		&v10PanickingKeyStringer{calls: &stringerCalls},
		&v10PanickingKeyError{calls: &errorCalls},
		nilStringer,
		nilError,
	}
	for _, key := range oddKeys {
		odd := RedactKeyValueArgs([]any{key})
		if len(odd) != 1 || odd[0] != "redacted_field" {
			t.Fatal("odd trailing non-string key did not become redacted_field")
		}
	}
	if countingStringerCalls != 0 || countingErrorCalls != 0 || stringerCalls != 0 || errorCalls != 0 ||
		v10TypedNilKeyStringerCalls != 0 || v10TypedNilKeyErrorCalls != 0 {
		t.Fatal("odd trailing non-string key invoked a formatter")
	}
}

func TestLogURLDecodedAdjacentKeysAndRawNormalizationStayLinear(t *testing.T) {
	for _, mark := range []string{",", ";", ")", "]", "}"} {
		previousWork := 0
		for _, count := range []int{32, 64, 128, 256, 512} {
			segment := `https:\/\/scale.invalid\/path` + mark + `st\u0061tus=SAFE` + mark
			input := strings.Repeat(segment, count)
			want := strings.Repeat("[REDACTED-URL]"+mark+`st\u0061tus=SAFE`+mark, count)
			var stats logURLScanStats
			clean := redactLogURLsWithStats(input, &stats)
			if clean != want {
				t.Fatalf("mark=%q count=%d decoded structural tail was not preserved", mark, count)
			}
			if strings.Contains(clean, "scale.invalid") {
				t.Fatalf("mark=%q count=%d URL host leaked", mark, count)
			}
			full := RedactText(input)
			if full != want {
				t.Fatalf("mark=%q count=%d full redaction did not preserve the decoded structural tail", mark, count)
			}
			if second := RedactText(full); second != full {
				t.Fatalf("mark=%q count=%d full redaction was not idempotent", mark, count)
			}
			if stats.rawRescanBytes > len(input)*2 {
				t.Fatalf("mark=%q count=%d raw rescans=%d for %d bytes", mark, count, stats.rawRescanBytes, len(input))
			}
			work := stats.decodedViewBytes + stats.boundaryLookaheadBytes + stats.rawRescanBytes
			if work <= 0 {
				t.Fatalf("mark=%q count=%d scan work was not recorded", mark, count)
			}
			if previousWork > 0 && work > previousWork*3 {
				t.Fatalf("mark=%q count=%d scan work grew superlinearly: previous=%d current=%d stats=%#v",
					mark, count, previousWork, work, stats)
			}
			previousWork = work
		}
	}

	const count = 512
	var raw strings.Builder
	ranges := make([]logURLRange, 0, count)
	for index := 0; index < count; index++ {
		start := raw.Len()
		raw.WriteString("https://fallback.invalid/path")
		end := raw.Len()
		raw.WriteString(",opaque")
		ranges = append(ranges, logURLRange{start: start, end: end})
	}
	rawText := raw.String()
	var stats logURLScanStats
	normalized := normalizeLogURLRanges(newLogURLView(rawText), ranges, &stats)
	if len(normalized) != 1 || normalized[0].start != 0 || normalized[0].end != len(rawText) {
		t.Fatal("overlapping raw extensions were not merged monotonically")
	}
	if stats.rawRescanBytes <= 0 || stats.rawRescanBytes > len(rawText)*2 {
		t.Fatalf("raw rescan accounting = %d for %d bytes", stats.rawRescanBytes, len(rawText))
	}
}
