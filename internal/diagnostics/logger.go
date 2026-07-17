// Package diagnostics provides local structured logging with mandatory redaction.
package diagnostics

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const DefaultRetentionDays = 14

var (
	logFilePattern     = regexp.MustCompile(`^app-(\d{4}-\d{2}-\d{2})\.jsonl$`)
	urlPattern         = regexp.MustCompile(`(?i)https?://[^\s"'<>]+`)
	secretValuePattern = regexp.MustCompile(`(?i)(?:mstoken|a_bogus|signature|authorization|cookie|token)\s*[:=]\s*[^&\s,;]+`)
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

func NewRedactingJSONHandler(writer io.Writer, options *slog.HandlerOptions) slog.Handler {
	return &redactingHandler{next: slog.NewJSONHandler(writer, options)}
}

type redactingHandler struct {
	next slog.Handler
}

func (h *redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *redactingHandler) Handle(ctx context.Context, record slog.Record) error {
	clean := slog.NewRecord(record.Time, record.Level, redactString(record.Message), record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		clean.AddAttrs(redactAttr(attr))
		return true
	})
	return h.next.Handle(ctx, clean)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, 0, len(attrs))
	for _, attr := range attrs {
		clean = append(clean, redactAttr(attr))
	}
	return &redactingHandler{next: h.next.WithAttrs(clean)}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{next: h.next.WithGroup(name)}
}

func redactAttr(attr slog.Attr) slog.Attr {
	if sensitiveKey(attr.Key) {
		return slog.String("redacted_field", "[REDACTED]")
	}
	value := attr.Value.Resolve()
	switch value.Kind() {
	case slog.KindString:
		attr.Value = slog.StringValue(redactString(value.String()))
	case slog.KindAny:
		switch item := value.Any().(type) {
		case error:
			attr.Value = slog.StringValue(redactString(item.Error()))
		case fmt.Stringer:
			attr.Value = slog.StringValue(redactString(item.String()))
		}
	case slog.KindGroup:
		group := value.Group()
		clean := make([]slog.Attr, 0, len(group))
		for _, child := range group {
			clean = append(clean, redactAttr(child))
		}
		attr.Value = slog.GroupValue(clean...)
	}
	return attr
}

func sensitiveKey(key string) bool {
	key = strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	for _, marker := range []string{"cookie", "authorization", "token", "signature", "a_bogus", "credential", "stream_url", "signed_url"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return key == "url" || strings.HasSuffix(key, "_url")
}

func redactString(value string) string {
	value = urlPattern.ReplaceAllString(value, "[REDACTED-URL]")
	return secretValuePattern.ReplaceAllString(value, "[REDACTED]")
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
