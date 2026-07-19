package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jwwsjlm/douyinLive/v2/internal/storage"
)

func TestSQLiteRepositoryRegisterRecordingRootPersistsMarkerAndRow(t *testing.T) {
	repository, store, _ := openRecordingRootRepository(t)
	defer store.Close()
	rootPath := filepath.Join(t.TempDir(), "private-recordings")
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatalf("Mkdir(root) error = %v", err)
	}

	registered, err := repository.RegisterRecordingRoot(context.Background(), rootPath)
	if err != nil {
		t.Fatalf("RegisterRecordingRoot() error = %v", err)
	}
	if parsed, err := uuid.Parse(registered.ID); err != nil || parsed.Version() != 7 {
		t.Fatalf("registered root ID %q is not UUIDv7", registered.ID)
	}
	if registered.Status != RecordingRootReady || registered.absolutePath == "" ||
		!validRecordingRootDigest(registered.canonicalKey) || !validRecordingRootDigest(registered.volumeIdentity) {
		t.Fatalf("registered root internal state is invalid: %v", registered)
	}

	markerPath := filepath.Join(registered.absolutePath, recordingRootMarkerName)
	payload, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("ReadFile(marker) error = %v", err)
	}
	if bytes.Contains(payload, []byte(rootPath)) || bytes.Contains(payload, []byte(registered.absolutePath)) {
		t.Fatal("recording root marker contains an absolute path")
	}
	marker, err := readRecordingRootMarker(markerPath)
	if err != nil {
		t.Fatalf("readRecordingRootMarker() error = %v", err)
	}
	if marker.RootID != registered.ID || marker.VolumeIdentity != registered.volumeIdentity || marker.Version != 1 {
		t.Fatalf("marker = %+v, want registered identity", marker)
	}
	var markerObject map[string]json.RawMessage
	if err := json.Unmarshal(payload, &markerObject); err != nil {
		t.Fatalf("decode marker object: %v", err)
	}
	if len(markerObject) != 3 || markerObject["version"] == nil || markerObject["rootId"] == nil || markerObject["volumeIdentity"] == nil {
		t.Fatalf("marker keys = %v, want only version/rootId/volumeIdentity", markerObject)
	}

	var databaseRoot recordingRootRow
	err = store.Reader().QueryRow(`SELECT id, absolute_path, canonical_key, volume_identity,
		status, created_at, updated_at, last_verified_at FROM recording_roots WHERE id = ?`, registered.ID).Scan(
		&databaseRoot.id, &databaseRoot.absolutePath, &databaseRoot.canonicalKey, &databaseRoot.volumeIdentity,
		&databaseRoot.status, &databaseRoot.createdAt, &databaseRoot.updatedAt, &databaseRoot.lastVerifiedAt,
	)
	if err != nil {
		t.Fatalf("read recording_roots row: %v", err)
	}
	if !recordingRootRowMatches(databaseRoot, registered) {
		t.Fatalf("database row does not match registered identity")
	}

	retried, err := repository.RegisterRecordingRoot(context.Background(), rootPath)
	if err != nil || retried.ID != registered.ID {
		t.Fatalf("idempotent RegisterRecordingRoot() = (%v, %v), want ID %q", retried, err, registered.ID)
	}
	var count int
	if err := store.Reader().QueryRow(`SELECT COUNT(*) FROM recording_roots`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("recording_roots count = %d, error = %v", count, err)
	}
	assertNoRecordingRootTemporaryFiles(t, registered.absolutePath)
}

func TestSQLiteRepositoryRegisterRecordingRootConcurrentIdempotence(t *testing.T) {
	repository, store, _ := openRecordingRootRepository(t)
	defer store.Close()
	rootPath := filepath.Join(t.TempDir(), "concurrent-root")
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatalf("Mkdir(root) error = %v", err)
	}

	const workers = 24
	results := make(chan RecordingRoot, workers)
	errorsFound := make(chan error, workers)
	var group sync.WaitGroup
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			root, err := repository.RegisterRecordingRoot(context.Background(), rootPath)
			if err != nil {
				errorsFound <- err
				return
			}
			results <- root
		}()
	}
	group.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		t.Errorf("concurrent RegisterRecordingRoot() error = %v", err)
	}
	var rootID string
	for result := range results {
		if rootID == "" {
			rootID = result.ID
		}
		if result.ID != rootID {
			t.Errorf("concurrent root ID = %q, want %q", result.ID, rootID)
		}
	}
	if rootID == "" {
		t.Fatal("concurrent registration returned no root")
	}
	marker, err := readRecordingRootMarker(filepath.Join(rootPath, recordingRootMarkerName))
	if err != nil || marker.RootID != rootID {
		t.Fatalf("durable marker = (%+v, %v), want ID %q", marker, err, rootID)
	}
	var count int
	if err := store.Reader().QueryRow(`SELECT COUNT(*) FROM recording_roots`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("recording_roots count = %d, error = %v", count, err)
	}
	assertNoRecordingRootTemporaryFiles(t, rootPath)
}

func TestSQLiteRepositoryRegisterRecordingRootMarkerBeforeDatabaseIsRetryable(t *testing.T) {
	repository, store, layout := openRecordingRootRepository(t)
	rootPath := filepath.Join(t.TempDir(), "interrupted-root")
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatalf("Mkdir(root) error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close(store) error = %v", err)
	}

	if _, err := repository.RegisterRecordingRoot(context.Background(), rootPath); !errors.Is(err, ErrRecordingRootPersistence) {
		t.Fatalf("RegisterRecordingRoot() after database close error = %v, want ErrRecordingRootPersistence", err)
	}
	marker, err := readRecordingRootMarker(filepath.Join(rootPath, recordingRootMarkerName))
	if err != nil {
		t.Fatalf("marker was not durable before database failure: %v", err)
	}

	restartedStore, err := storage.Open(context.Background(), layout, storage.OpenOptions{})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer restartedStore.Close()
	restartedRepository, err := newSQLiteRepository(
		restartedStore.Writer(), restartedStore.Reader(), layout.Root,
		func() time.Time { return time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC) },
	)
	if err != nil {
		t.Fatalf("newSQLiteRepository(restart) error = %v", err)
	}
	retried, err := restartedRepository.RegisterRecordingRoot(context.Background(), rootPath)
	if err != nil || retried.ID != marker.RootID {
		t.Fatalf("retry RegisterRecordingRoot() = (%v, %v), want marker ID %q", retried, err, marker.RootID)
	}
	var databaseID string
	if err := restartedStore.Reader().QueryRow(`SELECT id FROM recording_roots`).Scan(&databaseID); err != nil || databaseID != marker.RootID {
		t.Fatalf("database root ID = %q, error = %v, want %q", databaseID, err, marker.RootID)
	}
}

func TestSQLiteRepositoryRegisterRecordingRootRejectsStrictMarkerViolations(t *testing.T) {
	validID := newV7(t)
	validVolume := strings.Repeat("a", recordingRootDigestHexLength)
	tests := map[string]string{
		"malformed":        `{"version":1`,
		"unknown field":    fmt.Sprintf(`{"version":1,"rootId":%q,"volumeIdentity":%q,"path":"secret"}`, validID, validVolume),
		"duplicate field":  fmt.Sprintf(`{"version":1,"version":1,"rootId":%q,"volumeIdentity":%q}`, validID, validVolume),
		"missing field":    fmt.Sprintf(`{"version":1,"rootId":%q}`, validID),
		"wrong version":    fmt.Sprintf(`{"version":2,"rootId":%q,"volumeIdentity":%q}`, validID, validVolume),
		"non UUIDv7":       fmt.Sprintf(`{"version":1,"rootId":"00000000-0000-4000-8000-000000000000","volumeIdentity":%q}`, validVolume),
		"uppercase digest": fmt.Sprintf(`{"version":1,"rootId":%q,"volumeIdentity":%q}`, validID, strings.ToUpper(validVolume)),
		"trailing object":  fmt.Sprintf(`{"version":1,"rootId":%q,"volumeIdentity":%q}{}`, validID, validVolume),
		"oversized":        strings.Repeat("x", recordingRootMarkerMaxBytes+1),
	}
	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			repository, store, _ := openRecordingRootRepository(t)
			defer store.Close()
			rootPath := filepath.Join(t.TempDir(), "invalid-marker")
			if err := os.Mkdir(rootPath, 0o700); err != nil {
				t.Fatalf("Mkdir(root) error = %v", err)
			}
			if err := os.WriteFile(filepath.Join(rootPath, recordingRootMarkerName), []byte(payload), 0o600); err != nil {
				t.Fatalf("WriteFile(marker) error = %v", err)
			}
			if _, err := repository.RegisterRecordingRoot(context.Background(), rootPath); !errors.Is(err, ErrRecordingRootConflict) {
				t.Fatalf("RegisterRecordingRoot() error = %v, want ErrRecordingRootConflict", err)
			}
			var count int
			if err := store.Reader().QueryRow(`SELECT COUNT(*) FROM recording_roots`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("recording_roots count = %d, error = %v", count, err)
			}
		})
	}
}

func TestSQLiteRepositoryRegisterRecordingRootRejectsCopiedMarker(t *testing.T) {
	repository, store, _ := openRecordingRootRepository(t)
	defer store.Close()
	base := t.TempDir()
	firstPath := filepath.Join(base, "first-root")
	secondPath := filepath.Join(base, "second-root")
	if err := os.Mkdir(firstPath, 0o700); err != nil {
		t.Fatalf("Mkdir(first) error = %v", err)
	}
	if err := os.Mkdir(secondPath, 0o700); err != nil {
		t.Fatalf("Mkdir(second) error = %v", err)
	}
	first, err := repository.RegisterRecordingRoot(context.Background(), firstPath)
	if err != nil {
		t.Fatalf("register first root: %v", err)
	}
	payload, err := os.ReadFile(filepath.Join(first.absolutePath, recordingRootMarkerName))
	if err != nil {
		t.Fatalf("read first marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(secondPath, recordingRootMarkerName), payload, 0o600); err != nil {
		t.Fatalf("copy marker: %v", err)
	}
	if _, err := repository.RegisterRecordingRoot(context.Background(), secondPath); !errors.Is(err, ErrRecordingRootConflict) {
		t.Fatalf("register copied marker error = %v, want ErrRecordingRootConflict", err)
	}
	var count int
	if err := store.Reader().QueryRow(`SELECT COUNT(*) FROM recording_roots`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("recording_roots count = %d, error = %v", count, err)
	}
}

func TestSQLiteRepositoryRegisterRecordingRootRejectsIdentityDrift(t *testing.T) {
	tests := map[string]func(*testing.T, *storage.Store, RecordingRoot){
		"database path": func(t *testing.T, store *storage.Store, root RecordingRoot) {
			driftedPath := filepath.Join(filepath.Dir(root.absolutePath), "different-root")
			if _, err := store.Writer().Exec(`UPDATE recording_roots SET absolute_path = ? WHERE id = ?`, driftedPath, root.ID); err != nil {
				t.Fatalf("drift database path: %v", err)
			}
		},
		"database canonical key": func(t *testing.T, store *storage.Store, root RecordingRoot) {
			drifted := strings.Repeat("0", recordingRootDigestHexLength)
			if drifted == root.canonicalKey {
				drifted = strings.Repeat("1", recordingRootDigestHexLength)
			}
			if _, err := store.Writer().Exec(`UPDATE recording_roots SET canonical_key = ? WHERE id = ?`, drifted, root.ID); err != nil {
				t.Fatalf("drift canonical key: %v", err)
			}
		},
		"database volume": func(t *testing.T, store *storage.Store, root RecordingRoot) {
			drifted := strings.Repeat("0", recordingRootDigestHexLength)
			if drifted == root.volumeIdentity {
				drifted = strings.Repeat("1", recordingRootDigestHexLength)
			}
			if _, err := store.Writer().Exec(`UPDATE recording_roots SET volume_identity = ? WHERE id = ?`, drifted, root.ID); err != nil {
				t.Fatalf("drift volume: %v", err)
			}
		},
		"marker volume": func(t *testing.T, _ *storage.Store, root RecordingRoot) {
			drifted := strings.Repeat("0", recordingRootDigestHexLength)
			if drifted == root.volumeIdentity {
				drifted = strings.Repeat("1", recordingRootDigestHexLength)
			}
			payload, err := encodeRecordingRootMarker(recordingRootMarker{
				Version: recordingRootMarkerVersion, RootID: root.ID, VolumeIdentity: drifted,
			})
			if err != nil {
				t.Fatalf("encode drifted marker: %v", err)
			}
			if err := os.WriteFile(filepath.Join(root.absolutePath, recordingRootMarkerName), payload, 0o600); err != nil {
				t.Fatalf("write drifted marker: %v", err)
			}
		},
		"marker root id": func(t *testing.T, _ *storage.Store, root RecordingRoot) {
			payload, err := encodeRecordingRootMarker(recordingRootMarker{
				Version: recordingRootMarkerVersion, RootID: newV7(t), VolumeIdentity: root.volumeIdentity,
			})
			if err != nil {
				t.Fatalf("encode marker with drifted id: %v", err)
			}
			if err := os.WriteFile(filepath.Join(root.absolutePath, recordingRootMarkerName), payload, 0o600); err != nil {
				t.Fatalf("write marker with drifted id: %v", err)
			}
		},
	}
	for name, drift := range tests {
		t.Run(name, func(t *testing.T) {
			repository, store, _ := openRecordingRootRepository(t)
			defer store.Close()
			rootPath := filepath.Join(t.TempDir(), "drifting-root")
			if err := os.Mkdir(rootPath, 0o700); err != nil {
				t.Fatalf("Mkdir(root) error = %v", err)
			}
			registered, err := repository.RegisterRecordingRoot(context.Background(), rootPath)
			if err != nil {
				t.Fatalf("initial registration: %v", err)
			}
			drift(t, store, registered)
			if _, err := repository.RegisterRecordingRoot(context.Background(), rootPath); !errors.Is(err, ErrRecordingRootConflict) {
				t.Fatalf("registration after identity drift error = %v, want ErrRecordingRootConflict", err)
			}
		})
	}
}

func TestSQLiteRepositoryRegisterRecordingRootValidationAndPrivacy(t *testing.T) {
	repository, store, _ := openRecordingRootRepository(t)
	defer store.Close()
	base := t.TempDir()
	invalidPercent := filepath.Join(base, "bad%root")
	invalidControl := filepath.Join(base, "bad\nroot")
	for _, candidate := range []string{"relative-root", invalidPercent, invalidControl} {
		if _, err := repository.RegisterRecordingRoot(context.Background(), candidate); !errors.Is(err, ErrRecordingRootInvalid) {
			t.Errorf("RegisterRecordingRoot(%q) error = %v, want ErrRecordingRootInvalid", candidate, err)
		}
	}
	missing := filepath.Join(base, "missing-root")
	if _, err := repository.RegisterRecordingRoot(context.Background(), missing); !errors.Is(err, ErrRecordingRootUnavailable) {
		t.Errorf("RegisterRecordingRoot(missing) error = %v, want ErrRecordingRootUnavailable", err)
	}
	filePath := filepath.Join(base, "regular-file")
	if err := os.WriteFile(filePath, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write regular file: %v", err)
	}
	if _, err := repository.RegisterRecordingRoot(context.Background(), filePath); !errors.Is(err, ErrRecordingRootUnavailable) {
		t.Errorf("RegisterRecordingRoot(file) error = %v, want ErrRecordingRootUnavailable", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repository.RegisterRecordingRoot(cancelled, base); !errors.Is(err, context.Canceled) {
		t.Errorf("RegisterRecordingRoot(cancelled) error = %v, want context.Canceled", err)
	}

	privatePath := filepath.Join(base, "secret-private-recordings")
	if err := os.Mkdir(privatePath, 0o700); err != nil {
		t.Fatalf("Mkdir(private root) error = %v", err)
	}
	registered, err := repository.RegisterRecordingRoot(context.Background(), privatePath)
	if err != nil {
		t.Fatalf("register private root: %v", err)
	}
	jsonPayload, err := json.Marshal(registered)
	if err != nil {
		t.Fatalf("MarshalJSON(root) error = %v", err)
	}
	var logBuffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuffer, nil))
	logger.Info("root", slog.Any("recording_root", registered))
	surfaces := strings.Join([]string{
		fmt.Sprint(registered), fmt.Sprintf("%+v", registered), fmt.Sprintf("%#v", registered),
		string(jsonPayload), logBuffer.String(),
	}, "\n")
	for _, secret := range []string{registered.absolutePath, registered.canonicalKey, registered.volumeIdentity, privatePath} {
		if strings.Contains(surfaces, secret) {
			t.Fatalf("RecordingRoot output surface leaked protected identity material")
		}
	}
	var dto map[string]any
	if err := json.Unmarshal(jsonPayload, &dto); err != nil || len(dto) != 2 || dto["id"] != registered.ID || dto["status"] != string(RecordingRootReady) {
		t.Fatalf("RecordingRoot JSON = %s, error = %v", jsonPayload, err)
	}
	if _, err := store.Writer().Exec(`UPDATE recording_roots SET absolute_path = ? WHERE id = ?`, filepath.Join(base, "drift"), registered.ID); err != nil {
		t.Fatalf("inject private path drift: %v", err)
	}
	_, err = repository.RegisterRecordingRoot(context.Background(), privatePath)
	if !errors.Is(err, ErrRecordingRootConflict) || strings.Contains(err.Error(), privatePath) {
		t.Fatalf("sanitized conflict error = %v", err)
	}
}

func TestSQLiteRepositoryResolveSessionMediaRootRejectsMismatchedBindingsWithoutMutation(t *testing.T) {
	repository, store, layout := openRecordingRootRepository(t)
	defer store.Close()
	ctx := context.Background()
	firstPath := filepath.Join(t.TempDir(), "first-root")
	secondPath := filepath.Join(t.TempDir(), "second-root")
	for _, rootPath := range []string{firstPath, secondPath} {
		if err := os.Mkdir(rootPath, 0o700); err != nil {
			t.Fatalf("Mkdir(%s): %v", filepath.Base(rootPath), err)
		}
	}
	first, err := repository.RegisterRecordingRoot(ctx, firstPath)
	if err != nil {
		t.Fatalf("register first root: %v", err)
	}
	if _, err := repository.resolveSessionMediaRoot(ctx, secondPath, &first.ID); !errors.Is(err, ErrMediaFinalize) {
		t.Fatalf("mismatched external binding error = %v, want ErrMediaFinalize", err)
	}
	if _, err := os.Stat(filepath.Join(secondPath, recordingRootMarkerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mismatched resolution created a marker: %v", err)
	}
	var rootCount int
	if err := store.Reader().QueryRow(`SELECT COUNT(*) FROM recording_roots`).Scan(&rootCount); err != nil {
		t.Fatal(err)
	}
	if rootCount != 1 {
		t.Fatalf("mismatched resolution changed root rows to %d", rootCount)
	}

	if _, err := repository.resolveSessionMediaRoot(ctx, secondPath, nil); !errors.Is(err, ErrMediaFinalize) {
		t.Fatalf("external path without root ID error = %v, want ErrMediaFinalize", err)
	}
	resolvedInternal, err := repository.resolveSessionMediaRoot(ctx, layout.Root, nil)
	if err != nil || !sameRecorderDirectory(resolvedInternal, layout.Root) {
		t.Fatalf("internal root resolution = (%q, %v)", resolvedInternal, err)
	}
	resolvedExternal, err := repository.resolveSessionMediaRoot(ctx, firstPath, &first.ID)
	if err != nil || !sameRecorderDirectory(resolvedExternal, firstPath) {
		t.Fatalf("external root resolution = (%q, %v)", resolvedExternal, err)
	}
}

func TestSQLiteRepositoryVerifyRecordingRootBindingRejectsIdentityDrift(t *testing.T) {
	repository, store, _ := openRecordingRootRepository(t)
	defer store.Close()
	rootPath := filepath.Join(t.TempDir(), "verified-root")
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	registered, err := repository.RegisterRecordingRoot(context.Background(), rootPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Writer().Exec(`UPDATE recording_roots SET absolute_path = ? WHERE id = ?`,
		filepath.Join(filepath.Dir(rootPath), "drifted"), registered.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.verifyRecordingRootBinding(context.Background(), rootPath, registered.ID); !errors.Is(err, ErrRecordingRootConflict) {
		t.Fatalf("identity drift verification error = %v, want ErrRecordingRootConflict", err)
	}
}

func openRecordingRootRepository(t *testing.T) (*SQLiteRepository, *storage.Store, storage.Layout) {
	t.Helper()
	repository, store, layout, _, _ := openRepository(t)
	return repository, store, layout
}

func assertNoRecordingRootTemporaryFiles(t *testing.T, root string) {
	t.Helper()
	for _, pattern := range []string{
		filepath.Join(root, ".douyinlive-recording-root-*.tmp"),
		filepath.Join(root, recordingRootWriteProbePrefix+"*"),
	} {
		matches, err := filepath.Glob(pattern)
		if err != nil || len(matches) != 0 {
			t.Fatalf("recording root temporary files = %v, error = %v", matches, err)
		}
	}
}
