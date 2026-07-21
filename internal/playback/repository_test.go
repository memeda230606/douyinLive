package playback

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jwwsjlm/douyinLive/v2/internal/storage"
)

type playbackFixture struct {
	repository *Repository
	writer     *sql.DB
	close      func()
	roomID     string
	sessionIDs []string
}

func TestRepositoryPaginatesStableTiesAndBindsCursorsToFilters(t *testing.T) {
	fixture := newPlaybackFixture(t)
	defer fixture.close()
	ctx := context.Background()

	first, err := fixture.repository.ListSessions(ctx, SessionFilter{}, PageRequest{Limit: 2})
	if err != nil {
		t.Fatalf("ListSessions() first page error = %v", err)
	}
	if first.Version != ContractVersion || len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatalf("first session page = %+v", first)
	}
	second, err := fixture.repository.ListSessions(ctx, SessionFilter{}, PageRequest{
		Limit: 2, Cursor: first.NextCursor,
	})
	if err != nil {
		t.Fatalf("ListSessions() second page error = %v", err)
	}
	gotSessionIDs := []string{
		first.Items[0].ID, first.Items[1].ID, second.Items[0].ID, second.Items[1].ID,
	}
	wantSessionIDs := append([]string(nil), fixture.sessionIDs...)
	sort.Sort(sort.Reverse(sort.StringSlice(wantSessionIDs)))
	if strings.Join(gotSessionIDs, ",") != strings.Join(wantSessionIDs, ",") {
		t.Fatalf("session keyset order = %v, want %v", gotSessionIDs, wantSessionIDs)
	}
	if _, err := fixture.repository.ListSessions(ctx, SessionFilter{
		RoomConfigID: fixture.roomID,
	}, PageRequest{Cursor: first.NextCursor}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("filter-mismatched session cursor error = %v", err)
	}

	sessionID := fixture.sessionIDs[0]
	eventIDs := insertPlaybackEvents(t, fixture.writer, sessionID)
	eventFilter := EventFilter{SessionID: sessionID, Kinds: []string{"chat"}, Roles: []string{"source"}}
	eventFirst, err := fixture.repository.ListEvents(ctx, eventFilter, PageRequest{Limit: 2})
	if err != nil {
		t.Fatalf("ListEvents() first page error = %v", err)
	}
	eventSecond, err := fixture.repository.ListEvents(ctx, eventFilter, PageRequest{
		Limit: 2, Cursor: eventFirst.NextCursor,
	})
	if err != nil {
		t.Fatalf("ListEvents() second page error = %v", err)
	}
	gotEventIDs := []string{
		eventFirst.Items[0].ID, eventFirst.Items[1].ID,
		eventSecond.Items[0].ID, eventSecond.Items[1].ID,
	}
	wantEventIDs := append([]string(nil), eventIDs[:3]...)
	sort.Strings(wantEventIDs)
	wantEventIDs = append(wantEventIDs, eventIDs[3])
	if strings.Join(gotEventIDs, ",") != strings.Join(wantEventIDs, ",") {
		t.Fatalf("event keyset order = %v, want %v", gotEventIDs, wantEventIDs)
	}
	if _, err := fixture.repository.ListEvents(ctx, EventFilter{
		SessionID: sessionID, Kinds: []string{"gift"}, Roles: []string{"source"},
	}, PageRequest{Cursor: eventFirst.NextCursor}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("filter-mismatched event cursor error = %v", err)
	}
	if _, err := fixture.repository.ListGaps(ctx, GapFilter{
		SessionID: sessionID,
	}, PageRequest{Cursor: eventFirst.NextCursor}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("cross-query cursor error = %v", err)
	}

	gapIDs := insertPlaybackGaps(t, fixture.writer, sessionID)
	gapFirst, err := fixture.repository.ListGaps(ctx, GapFilter{SessionID: sessionID}, PageRequest{Limit: 2})
	if err != nil {
		t.Fatalf("ListGaps() first page error = %v", err)
	}
	gapSecond, err := fixture.repository.ListGaps(ctx, GapFilter{SessionID: sessionID}, PageRequest{
		Limit: 2, Cursor: gapFirst.NextCursor,
	})
	if err != nil {
		t.Fatalf("ListGaps() second page error = %v", err)
	}
	gotGapIDs := []string{gapFirst.Items[0].ID, gapFirst.Items[1].ID, gapSecond.Items[0].ID}
	wantGapIDs := append([]string(nil), gapIDs[:2]...)
	sort.Strings(wantGapIDs)
	wantGapIDs = append(wantGapIDs, gapIDs[2])
	if strings.Join(gotGapIDs, ",") != strings.Join(wantGapIDs, ",") {
		t.Fatalf("gap keyset order = %v, want %v", gotGapIDs, wantGapIDs)
	}
}

func TestRepositoryDTOsAreVersionedPrivacyAllowlistsAndReadOnly(t *testing.T) {
	fixture := newPlaybackFixture(t)
	defer fixture.close()
	sessionID := fixture.sessionIDs[0]
	insertPlaybackEvents(t, fixture.writer, sessionID)
	insertPlaybackGaps(t, fixture.writer, sessionID)

	ctx := context.Background()
	var beforeChanges int64
	if err := fixture.writer.QueryRow(`SELECT total_changes()`).Scan(&beforeChanges); err != nil {
		t.Fatalf("read total_changes before queries: %v", err)
	}
	session, err := fixture.repository.GetSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}
	events, err := fixture.repository.ListEvents(ctx, EventFilter{SessionID: sessionID}, PageRequest{})
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	gaps, err := fixture.repository.ListGaps(ctx, GapFilter{SessionID: sessionID}, PageRequest{})
	if err != nil {
		t.Fatalf("ListGaps() error = %v", err)
	}
	if session.Version != ContractVersion || events.Version != ContractVersion || gaps.Version != ContractVersion {
		t.Fatalf("contract versions = (%d, %d, %d)", session.Version, events.Version, gaps.Version)
	}
	encoded, err := json.Marshal(struct {
		Session SessionResult `json:"session"`
		Events  EventPage     `json:"events"`
		Gaps    GapPage       `json:"gaps"`
	}{session, events, gaps})
	if err != nil {
		t.Fatalf("marshal playback DTOs: %v", err)
	}
	text := string(encoded)
	for _, forbidden := range []string{
		"private-live-id", "private-platform-room", "private-operation",
		"private/data/path", "private-platform-message", "private-dedupe",
		"private-user-hash", "private-normalized-json", "private.raw",
		"private-gap-dedupe", "private-gap-details",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("playback DTO leaked %q: %s", forbidden, text)
		}
	}
	for _, required := range []string{"visible title", "visible alias", "visible content", "VISIBLE_GAP"} {
		if !strings.Contains(text, required) {
			t.Fatalf("playback DTO lacks allowlisted %q: %s", required, text)
		}
	}
	var afterChanges int64
	if err := fixture.writer.QueryRow(`SELECT total_changes()`).Scan(&afterChanges); err != nil {
		t.Fatalf("read total_changes after queries: %v", err)
	}
	if afterChanges != beforeChanges {
		t.Fatalf("read-only repository changed database: before=%d after=%d", beforeChanges, afterChanges)
	}
}

func TestRepositoryRejectsMalformedArgumentsAndCursorVersions(t *testing.T) {
	fixture := newPlaybackFixture(t)
	defer fixture.close()
	ctx := context.Background()
	sessionID := fixture.sessionIDs[0]

	if _, err := fixture.repository.ListSessions(nil, SessionFilter{}, PageRequest{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil context error = %v", err)
	}
	if _, err := fixture.repository.ListSessions(ctx, SessionFilter{Statuses: []string{"completed", "completed"}}, PageRequest{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("duplicate status error = %v", err)
	}
	if _, err := fixture.repository.ListEvents(ctx, EventFilter{SessionID: sessionID, Kinds: []string{"private"}}, PageRequest{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalid event kind error = %v", err)
	}
	if _, err := fixture.repository.ListGaps(ctx, GapFilter{SessionID: "not-a-uuid"}, PageRequest{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalid gap session error = %v", err)
	}
	if _, err := fixture.repository.ListSessions(ctx, SessionFilter{}, PageRequest{Limit: MaxPageSize + 1}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("oversized page error = %v", err)
	}
	digest, err := filterDigest(cursorKindSessions, normalizedSessionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	badVersion, err := json.Marshal(cursorEnvelope{
		Version: CursorVersion + 1, Kind: cursorKindSessions, Filter: digest,
		Primary: 1, ID: sessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.repository.ListSessions(ctx, SessionFilter{}, PageRequest{
		Cursor: base64RawURL(badVersion),
	}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("unsupported cursor version error = %v", err)
	}
	unknownField := []byte(`{"v":1,"k":"sessions","f":"` + digest + `","p":1,"i":"` + sessionID + `","x":1}`)
	if _, err := fixture.repository.ListSessions(ctx, SessionFilter{}, PageRequest{
		Cursor: base64RawURL(unknownField),
	}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("unknown cursor field error = %v", err)
	}
}

func newPlaybackFixture(t *testing.T) playbackFixture {
	t.Helper()
	layout, err := storage.PrepareLayout(t.TempDir())
	if err != nil {
		t.Fatalf("PrepareLayout() error = %v", err)
	}
	store, err := storage.Open(context.Background(), layout, storage.OpenOptions{})
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	repository, err := NewRepository(store.Reader())
	if err != nil {
		store.Close()
		t.Fatalf("NewRepository() error = %v", err)
	}
	roomID := newUUIDv7(t)
	if _, err := store.Writer().Exec(`INSERT INTO rooms(
		id, live_id, alias, created_at, updated_at
	) VALUES (?, 'private-live-id', 'visible alias', 1, 1)`, roomID); err != nil {
		store.Close()
		t.Fatalf("insert playback room: %v", err)
	}
	sessionIDs := make([]string, 4)
	for index := range sessionIDs {
		sessionIDs[index] = newUUIDv7(t)
		if _, err := store.Writer().Exec(`INSERT INTO live_sessions(
			id, room_config_id, platform_room_id, title, status, recording_status,
			operation_id, manifest_dirty, started_at, ended_at, media_epoch_at,
			capture_offset_ms, clock_source, integrity_score, data_path,
			schema_version, created_at, updated_at
		) VALUES (?, ?, 'private-platform-room', 'visible title', 'completed',
			'completed', ?, 0, 1000, 2000, 1100, 0,
			'media', 1, 'private/data/path', 1, 1, 1)`, sessionIDs[index], roomID, "private-operation-"+sessionIDs[index]); err != nil {
			store.Close()
			t.Fatalf("insert playback session %d: %v", index, err)
		}
	}
	return playbackFixture{
		repository: repository, writer: store.Writer(), roomID: roomID, sessionIDs: sessionIDs,
		close: func() {
			if err := store.Close(); err != nil {
				t.Errorf("close playback store: %v", err)
			}
		},
	}
}

func insertPlaybackEvents(t *testing.T, writer *sql.DB, sessionID string) []string {
	t.Helper()
	ids := []string{newUUIDv7(t), newUUIDv7(t), newUUIDv7(t), newUUIDv7(t)}
	for index, id := range ids {
		offset := int64(500)
		if index == len(ids)-1 {
			offset = 600
		}
		if _, err := writer.Exec(`INSERT INTO live_events(
			id, session_id, ingest_sequence, event_role, method, kind,
			platform_message_id, dedupe_key, received_at, session_offset_ms,
			clock_confidence, user_hash, display_name, content, numeric_value,
			normalized_json, raw_file, raw_offset, raw_length, parse_status,
			normalizer_version
		) VALUES (?, ?, ?, 'source', 'private-method', 'chat',
			'private-platform-message', ?, 1000, ?, 1, 'private-user-hash',
			'visible name', 'visible content', 1, 'private-normalized-json',
			'private.raw', 1, 1, 'parsed', 'v1')`,
			id, sessionID, index+1, "private-dedupe-"+id, offset,
		); err != nil {
			t.Fatalf("insert playback event %d: %v", index, err)
		}
	}
	return ids
}

func insertPlaybackGaps(t *testing.T, writer *sql.DB, sessionID string) []string {
	t.Helper()
	ids := []string{newUUIDv7(t), newUUIDv7(t), newUUIDv7(t)}
	for index, id := range ids {
		offset := int64(700)
		if index == len(ids)-1 {
			offset = 800
		}
		if _, err := writer.Exec(`INSERT INTO capture_gaps(
			id, session_id, kind, started_at, ended_at, start_offset_ms,
			end_offset_ms, severity, recovered, reason_code, details_json, dedupe_key
		) VALUES (?, ?, 'recording_restart', 1000, 1100, ?, ?, 'warning', 1,
			'VISIBLE_GAP', 'private-gap-details', ?)`,
			id, sessionID, offset, offset+100, "private-gap-dedupe-"+id,
		); err != nil {
			t.Fatalf("insert playback gap %d: %v", index, err)
		}
	}
	return ids
}

func newUUIDv7(t *testing.T) string {
	t.Helper()
	value, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7() error = %v", err)
	}
	return value.String()
}

func base64RawURL(value []byte) string {
	return base64.RawURLEncoding.EncodeToString(value)
}
