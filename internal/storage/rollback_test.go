package storage

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRestoreBackupReinstatesPreUpgradeDatabaseAndPreservesCurrent(t *testing.T) {
	ctx := context.Background()
	layout, err := PrepareLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", sqliteDSN(layout.Database, false))
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	if _, err := applyMigrationSet(ctx, database, schemaMigrations[:5], time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	upgradeAt := time.Date(2026, 7, 22, 8, 0, 0, 0, time.UTC)
	store, err := Open(ctx, layout, OpenOptions{Now: upgradeAt, CreateBackups: true})
	if err != nil {
		t.Fatal(err)
	}
	if store.SchemaVersion() != 6 {
		t.Fatalf("upgraded schema = %d", store.SchemaVersion())
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	backups, err := filepath.Glob(filepath.Join(layout.BackupsDir, "app-v5-*.db"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups = %v, %v", backups, err)
	}
	rollbackAt := upgradeAt.Add(time.Hour)
	result, err := RestoreBackup(ctx, layout, backups[0], rollbackAt)
	if err != nil {
		t.Fatal(err)
	}
	if result.RestoredSchemaVersion != 5 || result.PreservedSchemaVersion != 6 {
		t.Fatalf("rollback result = %#v", result)
	}
	if filepath.Dir(result.PreservedDatabase) != layout.Root {
		t.Fatalf("preserved database escaped data root: %q", result.PreservedDatabase)
	}
	if version, err := inspectDatabaseFile(ctx, layout.Database); err != nil || version != 5 {
		t.Fatalf("restored version = %d, %v", version, err)
	}
	if version, err := inspectDatabaseFile(ctx, result.PreservedDatabase); err != nil || version != 6 {
		t.Fatalf("preserved version = %d, %v", version, err)
	}
}

func TestRestoreBackupRejectsOutsideBackupAndActiveSidecar(t *testing.T) {
	ctx := context.Background()
	layout, err := PrepareLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(ctx, layout, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(layout.Root, "app-v5-20260722T080000.000Z.db")
	if err := copyDatabaseFile(layout.Database, outside); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreBackup(ctx, layout, outside, time.Now()); err == nil {
		t.Fatal("outside backup accepted")
	}
	backup := filepath.Join(layout.BackupsDir, "app-v6-20260722T080000.000Z.db")
	if err := copyDatabaseFile(layout.Database, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.Database+"-wal", []byte("active"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreBackup(ctx, layout, backup, time.Now()); err == nil {
		t.Fatal("active sqlite sidecar accepted")
	}
}

func TestRestoreBackupRejectsCorruptOrNewerBackup(t *testing.T) {
	ctx := context.Background()
	layout, err := PrepareLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(ctx, layout, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(layout.BackupsDir, "app-v5-20260722T080000.000Z.db")
	if err := os.WriteFile(corrupt, []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreBackup(ctx, layout, corrupt, time.Now()); err == nil {
		t.Fatal("corrupt backup accepted")
	}
}
