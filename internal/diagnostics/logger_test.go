package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		"correlation_id", "corr-test",
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
		"correlation_id": "corr-test",
	} {
		if got := record[key]; got != want {
			t.Fatalf("record[%q] = %#v, want %q", key, got, want)
		}
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
