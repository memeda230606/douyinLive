// Package credentials stores secrets outside SQLite and never exposes them in DTOs.
package credentials

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const fileVersion = 1

type Status struct {
	Configured bool
	UpdatedAt  int64
}

type Store interface {
	Put(context.Context, string, []byte) (Status, error)
	Get(context.Context, string) ([]byte, error)
	Delete(context.Context, string) error
	Status(context.Context, string) (Status, error)
}

type protector interface {
	Protect([]byte) ([]byte, error)
	Unprotect([]byte) ([]byte, error)
}

type storedEntry struct {
	Blob      string `json:"blob"`
	UpdatedAt int64  `json:"updatedAt"`
}

type storedFile struct {
	Version int                    `json:"version"`
	Entries map[string]storedEntry `json:"entries"`
}

type FileStore struct {
	mu        sync.RWMutex
	path      string
	protector protector
	entries   map[string]storedEntry
	now       func() time.Time
}

func OpenFileStore(path string) (*FileStore, error) {
	return openFileStore(path, platformProtector{}, time.Now)
}

func openFileStore(path string, p protector, now func() time.Time) (*FileStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("credential store path is empty")
	}
	if p == nil {
		return nil, errors.New("credential protector is nil")
	}
	if now == nil {
		now = time.Now
	}
	store := &FileStore{path: path, protector: p, entries: map[string]storedEntry{}, now: now}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read credential store: %w", err)
	}
	var persisted storedFile
	if err := json.Unmarshal(data, &persisted); err != nil {
		return nil, fmt.Errorf("decode credential store: %w", err)
	}
	if persisted.Version != fileVersion {
		return nil, fmt.Errorf("unsupported credential store version %d", persisted.Version)
	}
	if persisted.Entries != nil {
		store.entries = persisted.Entries
	}
	return store, nil
}

func (s *FileStore) Put(ctx context.Context, ref string, secret []byte) (Status, error) {
	if err := validateContext(ctx); err != nil {
		return Status{}, err
	}
	if err := validateRef(ref); err != nil {
		return Status{}, err
	}
	if len(secret) == 0 {
		return Status{}, errors.New("credential value is empty")
	}
	protected, err := s.protector.Protect(secret)
	if err != nil {
		return Status{}, fmt.Errorf("protect credential: %w", err)
	}
	entry := storedEntry{
		Blob:      base64.RawStdEncoding.EncodeToString(protected),
		UpdatedAt: s.now().UTC().UnixMilli(),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.entries[ref]
	s.entries[ref] = entry
	if err := s.persistLocked(); err != nil {
		if existed {
			s.entries[ref] = previous
		} else {
			delete(s.entries, ref)
		}
		return Status{}, err
	}
	return Status{Configured: true, UpdatedAt: entry.UpdatedAt}, nil
}

func (s *FileStore) Get(ctx context.Context, ref string) ([]byte, error) {
	if err := validateContext(ctx); err != nil {
		return nil, err
	}
	if err := validateRef(ref); err != nil {
		return nil, err
	}
	s.mu.RLock()
	entry, ok := s.entries[ref]
	s.mu.RUnlock()
	if !ok {
		return nil, os.ErrNotExist
	}
	protected, err := base64.RawStdEncoding.DecodeString(entry.Blob)
	if err != nil {
		return nil, errors.New("credential store entry is corrupted")
	}
	secret, err := s.protector.Unprotect(protected)
	if err != nil {
		return nil, fmt.Errorf("unprotect credential: %w", err)
	}
	return secret, nil
}

func (s *FileStore) Delete(ctx context.Context, ref string) error {
	if err := validateContext(ctx); err != nil {
		return err
	}
	if err := validateRef(ref); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, ok := s.entries[ref]
	if !ok {
		return nil
	}
	delete(s.entries, ref)
	if err := s.persistLocked(); err != nil {
		s.entries[ref] = previous
		return err
	}
	return nil
}

func (s *FileStore) Status(ctx context.Context, ref string) (Status, error) {
	if err := validateContext(ctx); err != nil {
		return Status{}, err
	}
	if err := validateRef(ref); err != nil {
		return Status{}, err
	}
	s.mu.RLock()
	entry, ok := s.entries[ref]
	s.mu.RUnlock()
	return Status{Configured: ok, UpdatedAt: entry.UpdatedAt}, nil
}

func (s *FileStore) persistLocked() error {
	data, err := json.Marshal(storedFile{Version: fileVersion, Entries: s.entries})
	if err != nil {
		return fmt.Errorf("encode credential store: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create credential directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".credentials-*.tmp")
	if err != nil {
		return fmt.Errorf("create credential temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("restrict credential temporary file: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write credential temporary file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync credential temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close credential temporary file: %w", err)
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		return fmt.Errorf("replace credential store: %w", err)
	}
	return nil
}

func validateContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("credential context is nil")
	}
	return ctx.Err()
}

func validateRef(ref string) error {
	if strings.TrimSpace(ref) == "" || len(ref) > 256 || strings.ContainsAny(ref, "\r\n") {
		return errors.New("credential reference is invalid")
	}
	return nil
}
