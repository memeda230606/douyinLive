package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jwwsjlm/douyinLive/v2/internal/storage"
)

func main() {
	dataRoot := flag.String("data-root", "", "absolute DouyinLive data root; defaults to the current user data root")
	backup := flag.String("backup", "", "absolute path to a validated app-vN timestamp backup")
	confirm := flag.String("confirm", "", "must be exactly RESTORE_BACKUP")
	flag.Parse()
	if *backup == "" || *confirm != "RESTORE_BACKUP" {
		fmt.Fprintln(os.Stderr, "DATABASE_ROLLBACK_CONFIRMATION_REQUIRED")
		os.Exit(2)
	}
	root := *dataRoot
	if root == "" {
		var err error
		root, err = storage.DefaultRoot()
		if err != nil {
			fmt.Fprintln(os.Stderr, "DATABASE_ROLLBACK_ROOT_INVALID")
			os.Exit(1)
		}
	}
	absoluteRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil || absoluteRoot == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_ROLLBACK_ROOT_INVALID")
		os.Exit(1)
	}
	layout := storage.Layout{
		Root: absoluteRoot, Database: filepath.Join(absoluteRoot, "app.db"),
		BackupsDir: filepath.Join(absoluteRoot, "backups"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := storage.RestoreBackup(ctx, layout, *backup, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "DATABASE_ROLLBACK_FAILED:", err)
		os.Exit(1)
	}
	fmt.Println("DATABASE_ROLLBACK_PASSED")
	fmt.Printf("restoredSchema=%d\n", result.RestoredSchemaVersion)
	fmt.Printf("preservedSchema=%d\n", result.PreservedSchemaVersion)
	fmt.Printf("preservedDatabase=%s\n", filepath.Base(result.PreservedDatabase))
}
