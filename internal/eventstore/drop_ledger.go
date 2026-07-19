package eventstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	dropLedgerVersion           = 1
	dropLedgerFilename          = "drop-ledger.json"
	maxDropLedgerBytes          = 16 << 10
	eventPersistenceGapKind     = "event_persistence"
	eventDroppedLocalReasonCode = "EVENT_DROPPED_LOCAL"
)

var (
	ErrDropLedgerInvalid = errors.New("EVENT_DROP_LEDGER_INVALID")
	ErrDropLedgerCorrupt = errors.New("EVENT_DROP_LEDGER_CORRUPT")
	ErrDropLedgerIO      = errors.New("EVENT_DROP_LEDGER_IO")
)

// DropDelta is the fixed-size worker snapshot of callbacks that could not be
// admitted. The live callback only updates its in-memory accumulator; merging
// a delta into this ledger is worker-owned disk I/O.
type DropDelta struct {
	Count         int64
	StartedAt     time.Time
	EndedAt       time.Time
	StartOffsetMS int64
	EndOffsetMS   int64
}

// DropSnapshot is an immutable cumulative value suitable for Writer.Batch.
// Acknowledge must be called only after that batch commits successfully.
type DropSnapshot struct {
	TotalCount int64
	Gap        CaptureGap
}

// DropLedger retains one monotonic cumulative event-persistence gap per
// session. TotalCount may advance beyond AcknowledgedCount while an older
// snapshot is being committed, so acknowledging that older snapshot cannot
// discard later drops.
type DropLedger struct {
	mu        sync.Mutex
	root      string
	path      string
	sessionID string
	gapID     string
	dedupeKey string
	state     *dropLedgerState
}

type dropLedgerState struct {
	Version           int    `json:"version"`
	SessionID         string `json:"session_id"`
	GapID             string `json:"gap_id"`
	DedupeKey         string `json:"dedupe_key"`
	TotalCount        int64  `json:"total_count"`
	AcknowledgedCount int64  `json:"acknowledged_count"`
	StartedAtMS       int64  `json:"started_at_ms"`
	EndedAtMS         int64  `json:"ended_at_ms"`
	StartOffsetMS     int64  `json:"start_offset_ms"`
	EndOffsetMS       int64  `json:"end_offset_ms"`
}

// OpenDropLedger loads the fixed-size sidecar when present. It never performs
// callback-path work; callers should open and merge it from the session worker.
func OpenDropLedger(eventsRoot, sessionID string) (*DropLedger, error) {
	if strings.TrimSpace(eventsRoot) == "" || !validIdentifier(sessionID) {
		return nil, ErrDropLedgerInvalid
	}
	root, err := filepath.Abs(filepath.Clean(eventsRoot))
	if err != nil {
		return nil, ErrDropLedgerInvalid
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, ErrDropLedgerIO
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrDropLedgerInvalid
	}
	gapID, dedupeKey := stableDropIdentity(sessionID)
	ledger := &DropLedger{
		root: root, path: filepath.Join(root, dropLedgerFilename),
		sessionID: sessionID, gapID: gapID, dedupeKey: dedupeKey,
	}
	if err := ledger.load(); err != nil {
		return nil, err
	}
	return ledger, nil
}

// Merge durably adds one worker snapshot. Count and the ending bounds are
// monotonic; start bounds and stable identity are fixed by the first drop.
func (l *DropLedger) Merge(delta DropDelta) (DropSnapshot, error) {
	if l == nil || delta.Count <= 0 || delta.StartedAt.IsZero() || delta.EndedAt.IsZero() ||
		delta.StartOffsetMS < 0 || delta.EndOffsetMS < 0 {
		return DropSnapshot{}, ErrDropLedgerInvalid
	}
	startedAtMS := delta.StartedAt.UTC().UnixMilli()
	endedAtMS := delta.EndedAt.UTC().UnixMilli()
	if endedAtMS < startedAtMS {
		endedAtMS = startedAtMS
	}
	endOffsetMS := delta.EndOffsetMS
	if endOffsetMS < delta.StartOffsetMS {
		endOffsetMS = delta.StartOffsetMS
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	next := dropLedgerState{
		Version: dropLedgerVersion, SessionID: l.sessionID,
		GapID: l.gapID, DedupeKey: l.dedupeKey,
		TotalCount: delta.Count, StartedAtMS: startedAtMS, EndedAtMS: endedAtMS,
		StartOffsetMS: delta.StartOffsetMS, EndOffsetMS: endOffsetMS,
	}
	if l.state != nil {
		next = *l.state
		if next.TotalCount > (1<<63-1)-delta.Count {
			return DropSnapshot{}, ErrDropLedgerInvalid
		}
		next.TotalCount += delta.Count
		if endedAtMS > next.EndedAtMS {
			next.EndedAtMS = endedAtMS
		}
		if endOffsetMS > next.EndOffsetMS {
			next.EndOffsetMS = endOffsetMS
		}
	}
	if err := l.persistLocked(next); err != nil {
		return DropSnapshot{}, err
	}
	l.state = &next
	return l.snapshotLocked(), nil
}

// Pending returns the latest cumulative snapshot when SQLite has not yet
// acknowledged its TotalCount. It performs no I/O.
func (l *DropLedger) Pending() (DropSnapshot, bool) {
	if l == nil {
		return DropSnapshot{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == nil || l.state.TotalCount <= l.state.AcknowledgedCount {
		return DropSnapshot{}, false
	}
	return l.snapshotLocked(), true
}

// Acknowledge advances only the acknowledged count represented by snapshot.
// A later Merge remains pending and cannot be covered by an older snapshot.
func (l *DropLedger) Acknowledge(snapshot DropSnapshot) error {
	if l == nil {
		return ErrDropLedgerInvalid
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == nil || !l.validSnapshotLocked(snapshot) ||
		snapshot.TotalCount > l.state.TotalCount {
		return ErrDropLedgerInvalid
	}
	if snapshot.TotalCount <= l.state.AcknowledgedCount {
		return nil
	}
	next := *l.state
	next.AcknowledgedCount = snapshot.TotalCount
	if err := l.persistLocked(next); err != nil {
		return err
	}
	l.state = &next
	return nil
}

func (l *DropLedger) load() error {
	info, err := os.Lstat(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return ErrDropLedgerIO
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Size() <= 0 || info.Size() > maxDropLedgerBytes {
		return ErrDropLedgerCorrupt
	}
	file, err := os.Open(l.path)
	if err != nil {
		return ErrDropLedgerIO
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxDropLedgerBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(data) > maxDropLedgerBytes {
		return ErrDropLedgerIO
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var state dropLedgerState
	if err := decoder.Decode(&state); err != nil {
		return ErrDropLedgerCorrupt
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrDropLedgerCorrupt
	}
	if !l.validState(state) {
		return ErrDropLedgerCorrupt
	}
	l.state = &state
	return nil
}

func (l *DropLedger) persistLocked(state dropLedgerState) error {
	data, err := json.Marshal(state)
	if err != nil || len(data) > maxDropLedgerBytes {
		return ErrDropLedgerInvalid
	}
	temporaryPath := l.path + ".tmp"
	if err := os.Remove(temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return ErrDropLedgerIO
	}
	temporary, err := os.OpenFile(temporaryPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return ErrDropLedgerIO
	}
	open := true
	defer func() {
		if open {
			_ = temporary.Close()
		}
		_ = os.Remove(temporaryPath)
	}()
	if err := writeDropLedgerFull(temporary, data); err != nil {
		return ErrDropLedgerIO
	}
	if err := temporary.Sync(); err != nil {
		return ErrDropLedgerIO
	}
	if err := temporary.Close(); err != nil {
		open = false
		return ErrDropLedgerIO
	}
	open = false
	if err := os.Rename(temporaryPath, l.path); err != nil {
		return ErrDropLedgerIO
	}
	if directory, err := os.Open(l.root); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func (l *DropLedger) validState(state dropLedgerState) bool {
	return state.Version == dropLedgerVersion && state.SessionID == l.sessionID &&
		state.GapID == l.gapID && state.DedupeKey == l.dedupeKey &&
		state.TotalCount > 0 && state.AcknowledgedCount >= 0 &&
		state.AcknowledgedCount <= state.TotalCount &&
		state.EndedAtMS >= state.StartedAtMS && state.StartOffsetMS >= 0 &&
		state.EndOffsetMS >= state.StartOffsetMS
}

func (l *DropLedger) validSnapshotLocked(snapshot DropSnapshot) bool {
	if snapshot.TotalCount <= 0 || snapshot.Gap.ID != l.gapID ||
		snapshot.Gap.SessionID != l.sessionID || snapshot.Gap.DedupeKey != l.dedupeKey ||
		snapshot.Gap.Kind != eventPersistenceGapKind ||
		snapshot.Gap.ReasonCode != eventDroppedLocalReasonCode ||
		snapshot.Gap.DetailsJSON != dropCountDetails(snapshot.TotalCount) {
		return false
	}
	return true
}

func (l *DropLedger) snapshotLocked() DropSnapshot {
	state := *l.state
	endedAt := time.UnixMilli(state.EndedAtMS).UTC()
	endOffsetMS := state.EndOffsetMS
	return DropSnapshot{
		TotalCount: state.TotalCount,
		Gap: CaptureGap{
			ID: l.gapID, SessionID: l.sessionID, Kind: eventPersistenceGapKind,
			StartedAt: time.UnixMilli(state.StartedAtMS).UTC(), EndedAt: &endedAt,
			StartOffsetMS: state.StartOffsetMS, EndOffsetMS: &endOffsetMS,
			Severity: "error", Recovered: false,
			ReasonCode:  eventDroppedLocalReasonCode,
			DetailsJSON: dropCountDetails(state.TotalCount), DedupeKey: l.dedupeKey,
		},
	}
}

func stableDropIdentity(sessionID string) (string, string) {
	digest := sha256.Sum256([]byte("douyin-event-drop-ledger-v1\x00" + sessionID))
	encoded := hex.EncodeToString(digest[:])
	return "event-drop-" + encoded, "event-drop:" + encoded
}

func dropCountDetails(count int64) string {
	return "{\"count\":" + strconv.FormatInt(count, 10) + "}"
}

func writeDropLedgerFull(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
