package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var ErrIntegrityCheck = errors.New("sqlite integrity check failed")

type OpenOptions struct {
	Now           time.Time
	MaxReadConns  int
	CreateBackups bool
}

type Store struct {
	writer        *sql.DB
	reader        *sql.DB
	path          string
	schemaVersion int
}

func Open(ctx context.Context, layout Layout, options OpenOptions) (*Store, error) {
	if ctx == nil {
		return nil, errors.New("open sqlite: context is nil")
	}
	if options.Now.IsZero() {
		options.Now = time.Now()
	}
	if options.MaxReadConns <= 0 {
		options.MaxReadConns = 4
	}

	writer, err := sql.Open("sqlite", sqliteDSN(layout.Database, false))
	if err != nil {
		return nil, fmt.Errorf("open sqlite writer: %w", err)
	}
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	closeWriter := true
	defer func() {
		if closeWriter {
			_ = writer.Close()
		}
	}()

	if err := writer.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping sqlite writer: %w", err)
	}
	if err := quickCheck(ctx, writer); err != nil {
		return nil, err
	}
	if err := ensureMigrationTable(ctx, writer); err != nil {
		return nil, err
	}
	current, err := currentSchemaVersion(ctx, writer)
	if err != nil {
		return nil, err
	}
	latest := latestSchemaVersion(schemaMigrations)
	if current > latest {
		return nil, fmt.Errorf("database schema version %d is newer than supported version %d", current, latest)
	}
	if options.CreateBackups && current > 0 && current < latest {
		if _, err := backupDatabase(ctx, writer, layout, current, options.Now); err != nil {
			return nil, err
		}
	}
	version, err := applyMigrationSet(ctx, writer, schemaMigrations, options.Now)
	if err != nil {
		return nil, err
	}
	if err := quickCheck(ctx, writer); err != nil {
		return nil, err
	}

	reader, err := sql.Open("sqlite", sqliteDSN(layout.Database, true))
	if err != nil {
		return nil, fmt.Errorf("open sqlite reader: %w", err)
	}
	reader.SetMaxOpenConns(options.MaxReadConns)
	reader.SetMaxIdleConns(options.MaxReadConns)
	if err := reader.PingContext(ctx); err != nil {
		_ = reader.Close()
		return nil, fmt.Errorf("ping sqlite reader: %w", err)
	}

	closeWriter = false
	return &Store{writer: writer, reader: reader, path: layout.Database, schemaVersion: version}, nil
}

func sqliteDSN(path string, readOnly bool) string {
	query := url.Values{}
	if readOnly {
		query.Set("mode", "ro")
	} else {
		query.Set("mode", "rwc")
		query.Add("_pragma", "journal_mode(WAL)")
		query.Add("_pragma", "synchronous(NORMAL)")
	}
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	slashPath := filepath.ToSlash(path)
	escapedPath := (&url.URL{Path: slashPath}).EscapedPath()
	if filepath.VolumeName(path) != "" {
		escapedPath = "/" + strings.TrimPrefix(escapedPath, "/")
	}
	return "file://" + escapedPath + "?" + query.Encode()
}

func quickCheck(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA quick_check`)
	if err != nil {
		return fmt.Errorf("run sqlite quick check: %w", err)
	}
	defer rows.Close()

	var failures []string
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return fmt.Errorf("read sqlite quick check: %w", err)
		}
		if !strings.EqualFold(result, "ok") {
			failures = append(failures, result)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate sqlite quick check: %w", err)
	}
	if len(failures) != 0 {
		return fmt.Errorf("%w: %s", ErrIntegrityCheck, strings.Join(failures, "; "))
	}
	return nil
}

func backupDatabase(ctx context.Context, db *sql.DB, layout Layout, version int, now time.Time) (string, error) {
	if err := os.MkdirAll(layout.BackupsDir, 0o700); err != nil {
		return "", fmt.Errorf("create database backup directory: %w", err)
	}
	name := fmt.Sprintf("app-v%d-%s.db", version, now.UTC().Format("20060102T150405.000Z"))
	path := filepath.Join(layout.BackupsDir, name)
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("database backup already exists: %s", name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect database backup target: %w", err)
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return "", fmt.Errorf("create consistent database backup: %w", err)
	}
	return path, nil
}

// CreateUpdateBackup creates a transactionally consistent pre-update backup
// while the application still owns the live database connection.
func (s *Store) CreateUpdateBackup(ctx context.Context, now time.Time) (string, error) {
	if s == nil || s.writer == nil || s.path == "" || s.schemaVersion <= 0 {
		return "", errors.New("create update backup: store is not ready")
	}
	if ctx == nil {
		return "", errors.New("create update backup: context is nil")
	}
	root := filepath.Dir(s.path)
	return backupDatabase(ctx, s.writer, Layout{
		Root: root, Database: s.path, BackupsDir: filepath.Join(root, "backups"),
	}, s.schemaVersion, now)
}

func (s *Store) Writer() *sql.DB    { return s.writer }
func (s *Store) Reader() *sql.DB    { return s.reader }
func (s *Store) Path() string       { return s.path }
func (s *Store) SchemaVersion() int { return s.schemaVersion }

func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	return errors.Join(s.reader.Close(), s.writer.Close())
}
