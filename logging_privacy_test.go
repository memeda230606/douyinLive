package douyinLive

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

type privacyLogStringer string

func (value privacyLogStringer) String() string { return string(value) }

type privacyCaptureSink struct {
	entries []string
}

func (sink *privacyCaptureSink) append(message string) {
	sink.entries = append(sink.entries, message)
}

func (sink *privacyCaptureSink) Print(v ...interface{}) {
	sink.append(fmt.Sprint(v...))
}

func (sink *privacyCaptureSink) Printf(format string, v ...interface{}) {
	sink.append(fmt.Sprintf(format, v...))
}

func (sink *privacyCaptureSink) Println(v ...interface{}) {
	sink.append(fmt.Sprintln(v...))
}

func (sink *privacyCaptureSink) Debug(msg string, args ...interface{}) {
	sink.append(formatLogMessage(msg, args...))
}

func (sink *privacyCaptureSink) Info(msg string, args ...interface{}) {
	sink.append(formatLogMessage(msg, args...))
}

func (sink *privacyCaptureSink) Warn(msg string, args ...interface{}) {
	sink.append(formatLogMessage(msg, args...))
}

func (sink *privacyCaptureSink) Error(msg string, args ...interface{}) {
	sink.append(formatLogMessage(msg, args...))
}

func TestNormalizeLoggerWrapsCustomSinkWithPrivacyBoundary(t *testing.T) {
	capture := &privacyCaptureSink{}
	logger := normalizeLogger(capture)
	logger.Info(
		"connect live_id=private-message-id",
		"roomID", "private-room-id",
		"title", "private creator title",
		"err", errors.New("user_unique_id=private-user-id failed"),
		"status_code", 503,
		"signature_len", 64,
	)
	logger.Printf("fallback room_id=%s", "private-print-id")
	logger.Println("creator title=private print title")
	logger.Printf("status=%#v", struct {
		RoomID    string
		SessionID string
		PID       int
	}{RoomID: "private-struct-room", SessionID: "private-struct-session", PID: 24680})
	logger.Info(
		`payload {"room_id":"private-json-room","status":204}`,
		"err", errors.New(`{\"token\":\"private-json-token\",\"code\":\"SAFE_JSON\"}`),
	)
	logger.Printf(`creator=%s`, `{"title":"private json title","status":201}`)
	logger.Info(
		`payload {\\\"ROOM\\\\u005fID\\\":{\\\"nested\\\":\\\"private-two-layer-object\\\"},\\\"status\\\":208}`,
		"err", errors.New(`authorization="Bearer private spaced credential" code=SAFE_AUTH`),
		"detail", privacyLogStringer(`{'TOKEN':['private-custom-array'],'status':209}`),
	)
	logger.Printf(`lengths token_len=%q signature_len=%d status=%d`, "64", 64, 210)

	text := strings.Join(capture.entries, "\n")
	for _, forbidden := range []string{
		"private-message-id", "private-room-id", "private creator title",
		"private-user-id", "private-print-id", "private print title",
		"private-struct-room", "private-struct-session", "24680",
		"private-json-room", "private-json-token", "private json title",
		"private-two-layer-object", "private spaced credential", "private-custom-array",
		"token_len=", "ROOM",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("custom logger received private value %q: %s", forbidden, text)
		}
	}
	for _, preserved := range []string{"status_code=503", "signature_len=64"} {
		if !strings.Contains(text, preserved) {
			t.Fatalf("custom logger lost safe field %q: %s", preserved, text)
		}
	}
	for _, preserved := range []string{"status", "204", "SAFE_JSON", "201", "208", "SAFE_AUTH", "209", "signature_len=64", "210"} {
		if !strings.Contains(text, preserved) {
			t.Fatalf("custom logger lost quoted JSON field %q: %s", preserved, text)
		}
	}
	if !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("custom logger has no redaction marker: %s", text)
	}
}
