package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

var backupNamePattern = regexp.MustCompile(`^app-v([1-9][0-9]*)-[0-9]{8}T[0-9]{6}\.[0-9]{3}Z\.db$`)

type RollbackResult struct {
	RestoredSchemaVersion  int
	PreservedSchemaVersion int
	PreservedDatabase      string
}

// RestoreBackup replaces an offline database with a validated pre-upgrade
// backup. The displaced database is preserved beside app.db so an operator can
// reverse the operation. Callers must stop the desktop application first.
func RestoreBackup(ctx context.Context, layout Layout, backupPath string, now time.Time) (result RollbackResult, err error) {
	if ctx == nil {
		return result, errors.New("restore database: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	root, err := filepath.Abs(filepath.Clean(layout.Root))
	if err != nil || root == "" {
		return result, errors.New("restore database: invalid data root")
	}
	target, err := filepath.Abs(filepath.Clean(layout.Database))
	if err != nil || filepath.Dir(target) != root || filepath.Base(target) != "app.db" {
		return result, errors.New("restore database: invalid database target")
	}
	backupsDir, err := filepath.Abs(filepath.Clean(layout.BackupsDir))
	if err != nil || filepath.Dir(backupsDir) != root || filepath.Base(backupsDir) != "backups" {
		return result, errors.New("restore database: invalid backup directory")
	}
	backup, err := filepath.Abs(filepath.Clean(backupPath))
	if err != nil || filepath.Dir(backup) != backupsDir || !backupNamePattern.MatchString(filepath.Base(backup)) {
		return result, errors.New("restore database: backup is not a direct validated backup child")
	}
	if err := requireRegularFile(target); err != nil {
		return result, fmt.Errorf("restore database: current database: %w", err)
	}
	if err := requireRegularFile(backup); err != nil {
		return result, fmt.Errorf("restore database: backup: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, statErr := os.Lstat(target + suffix); statErr == nil {
			return result, fmt.Errorf("restore database: active sqlite sidecar exists: app.db%s", suffix)
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return result, fmt.Errorf("restore database: inspect sqlite sidecar: %w", statErr)
		}
	}

	currentVersion, err := inspectDatabaseFile(ctx, target)
	if err != nil {
		return result, fmt.Errorf("restore database: validate current database: %w", err)
	}
	backupVersion, err := inspectDatabaseFile(ctx, backup)
	if err != nil {
		return result, fmt.Errorf("restore database: validate backup: %w", err)
	}
	if backupVersion <= 0 || backupVersion > currentVersion || backupVersion > LatestSchemaVersion() {
		return result, fmt.Errorf("restore database: incompatible schema transition %d -> %d", currentVersion, backupVersion)
	}

	stamp := now.UTC().Format("20060102T150405.000Z")
	temporary := filepath.Join(root, ".app.db.rollback-"+stamp+".tmp")
	preserved := filepath.Join(root, "app.db.pre-rollback-"+stamp)
	for _, candidate := range []string{temporary, preserved} {
		if _, statErr := os.Lstat(candidate); statErr == nil {
			return result, errors.New("restore database: rollback target already exists")
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return result, fmt.Errorf("restore database: inspect rollback target: %w", statErr)
		}
	}
	defer func() {
		if removeErr := os.Remove(temporary); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove rollback temporary database: %w", removeErr))
		}
	}()
	if err := copyDatabaseFile(backup, temporary); err != nil {
		return result, fmt.Errorf("restore database: stage backup: %w", err)
	}
	stagedVersion, err := inspectDatabaseFile(ctx, temporary)
	if err != nil || stagedVersion != backupVersion {
		return result, fmt.Errorf("restore database: validate staged backup: version=%d: %w", stagedVersion, err)
	}
	if err := os.Rename(target, preserved); err != nil {
		return result, fmt.Errorf("restore database: application must be stopped and database offline: %w", err)
	}
	replaced := false
	defer func() {
		if replaced || err == nil {
			return
		}
		_ = os.Remove(target)
		if restoreErr := os.Rename(preserved, target); restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("restore displaced database after failure: %w", restoreErr))
		}
	}()
	if err := os.Rename(temporary, target); err != nil {
		return result, fmt.Errorf("restore database: publish backup: %w", err)
	}
	version, err := inspectDatabaseFile(ctx, target)
	if err != nil || version != backupVersion {
		return result, fmt.Errorf("restore database: validate published database: version=%d: %w", version, err)
	}
	replaced = true
	return RollbackResult{
		RestoredSchemaVersion: backupVersion, PreservedSchemaVersion: currentVersion,
		PreservedDatabase: preserved,
	}, nil
}

func inspectDatabaseFile(ctx context.Context, path string) (int, error) {
	database, err := sql.Open("sqlite", sqliteDSN(path, true))
	if err != nil {
		return 0, err
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	if err := database.PingContext(ctx); err != nil {
		return 0, err
	}
	if err := quickCheck(ctx, database); err != nil {
		return 0, err
	}
	return currentSchemaVersion(ctx, database)
}

func requireRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("path is not a regular non-symlink file")
	}
	return nil
}

func copyDatabaseFile(source, target string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	syncErr := output.Sync()
	closeErr := output.Close()
	return errors.Join(copyErr, syncErr, closeErr)
}
