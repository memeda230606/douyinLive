package room

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jwwsjlm/douyinLive/v2/internal/credentials"
	"github.com/jwwsjlm/douyinLive/v2/internal/storage"
)

type memoryCredentialStore struct {
	values map[string][]byte
	now    int64
}

func newMemoryCredentialStore() *memoryCredentialStore {
	return &memoryCredentialStore{values: map[string][]byte{}, now: 1_700_000_000_500}
}

func (s *memoryCredentialStore) Put(_ context.Context, ref string, value []byte) (credentials.Status, error) {
	s.values[ref] = append([]byte(nil), value...)
	return credentials.Status{Configured: true, UpdatedAt: s.now}, nil
}

func (s *memoryCredentialStore) Get(_ context.Context, ref string) ([]byte, error) {
	value, ok := s.values[ref]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), value...), nil
}

func (s *memoryCredentialStore) Delete(_ context.Context, ref string) error {
	delete(s.values, ref)
	return nil
}

func (s *memoryCredentialStore) Status(_ context.Context, ref string) (credentials.Status, error) {
	_, ok := s.values[ref]
	return credentials.Status{Configured: ok, UpdatedAt: map[bool]int64{true: s.now}[ok]}, nil
}

func TestRoomCRUDPersistsNormalizedConfigurationAcrossServices(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	defer store.Close()
	credentials := newMemoryCredentialStore()
	now := time.UnixMilli(1_700_000_000_000)
	service, err := newService(store.Writer(), store.Reader(), credentials, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newService() error = %v", err)
	}

	created, err := service.CreateRoom(ctx, CreateRoomInput{
		LiveID:         " https://live.douyin.com/example-room/?from=test ",
		Alias:          " 示例直播间 ",
		MonitorEnabled: true,
		RecordEnabled:  true,
		RecordingProfile: RecordingProfile{
			Quality: QualityHigh, SegmentMinutes: 12, SaveDirectory: filepath.Join(t.TempDir(), "records"),
		},
	})
	if err != nil {
		t.Fatalf("CreateRoom() error = %v", err)
	}
	if created.LiveID != "example-room" || created.Alias != "示例直播间" || !created.MonitorEnabled || !created.RecordEnabled {
		t.Fatalf("unexpected created room: %#v", created)
	}
	if created.RecordingProfile.Quality != QualityHigh || created.RecordingProfile.SegmentMinutes != 12 {
		t.Fatalf("unexpected recording profile: %#v", created.RecordingProfile)
	}
	if _, err := service.CreateRoom(ctx, CreateRoomInput{LiveID: "example-room"}); ErrorCode(err) != "ROOM_ALREADY_EXISTS" {
		t.Fatalf("duplicate CreateRoom() error = %v", err)
	}

	now = now.Add(time.Minute)
	updated, err := service.UpdateRoom(ctx, created.ID, UpdateRoomInput{
		LiveID: "example-room", Alias: "更新别名", MonitorEnabled: false, RecordEnabled: true,
		RecordingProfile: RecordingProfile{Quality: QualityOriginal, SegmentMinutes: 10},
	})
	if err != nil {
		t.Fatalf("UpdateRoom() error = %v", err)
	}
	if updated.Alias != "更新别名" || updated.MonitorEnabled || updated.UpdatedAt <= created.UpdatedAt {
		t.Fatalf("unexpected updated room: %#v", updated)
	}

	restarted, err := NewService(store.Writer(), store.Reader(), credentials)
	if err != nil {
		t.Fatalf("NewService(restart) error = %v", err)
	}
	rooms, err := restarted.ListRooms(ctx)
	if err != nil || len(rooms) != 1 || rooms[0].Alias != "更新别名" {
		t.Fatalf("ListRooms() after restart = (%#v, %v)", rooms, err)
	}
	if err := restarted.DeleteRoom(ctx, created.ID, false); err != nil {
		t.Fatalf("DeleteRoom() error = %v", err)
	}
	rooms, err = restarted.ListRooms(ctx)
	if err != nil || len(rooms) != 0 {
		t.Fatalf("ListRooms() after delete = (%#v, %v)", rooms, err)
	}
}

func TestRoomCookieIsReferencedAndNeverReturned(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	defer store.Close()
	credentialStore := newMemoryCredentialStore()
	service, err := NewService(store.Writer(), store.Reader(), credentialStore)
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateRoom(ctx, CreateRoomInput{LiveID: "cookie-room"})
	if err != nil {
		t.Fatal(err)
	}
	secret := "sessionid=must-not-appear-in-dto"
	status, err := service.SetRoomCookie(ctx, SetRoomCookieInput{RoomID: created.ID, Cookie: secret})
	if err != nil || !status.Configured {
		t.Fatalf("SetRoomCookie() = (%#v, %v)", status, err)
	}
	got, err := service.GetRoom(ctx, created.ID)
	if err != nil || !got.Cookie.Configured || got.Cookie.UpdatedAt == 0 {
		t.Fatalf("GetRoom() cookie status = (%#v, %v)", got.Cookie, err)
	}
	var reference string
	if err := store.Reader().QueryRow(`SELECT credential_ref FROM rooms WHERE id = ?`, created.ID).Scan(&reference); err != nil {
		t.Fatal(err)
	}
	if reference == "" || reference == secret {
		t.Fatalf("invalid stored credential reference %q", reference)
	}
	if err := service.ClearRoomCookie(ctx, created.ID); err != nil {
		t.Fatalf("ClearRoomCookie() error = %v", err)
	}
	got, err = service.GetRoom(ctx, created.ID)
	if err != nil || got.Cookie.Configured {
		t.Fatalf("cookie remained configured: (%#v, %v)", got.Cookie, err)
	}
}

func TestDeleteRoomRequiresExplicitHistoryDeletion(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	defer store.Close()
	service, err := NewService(store.Writer(), store.Reader(), newMemoryCredentialStore())
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateRoom(ctx, CreateRoomInput{LiveID: "history-room"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Writer().Exec(`INSERT INTO live_sessions(
		id, room_config_id, status, started_at, clock_source, data_path, schema_version, created_at, updated_at
	) VALUES ('session', ?, 'completed', 1, 'received', 'rooms/test', 1, 1, 1)`, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.DeleteRoom(ctx, created.ID, false); ErrorCode(err) != "ROOM_HAS_HISTORY" {
		t.Fatalf("DeleteRoom(false) error = %v", err)
	}
	if err := service.DeleteRoom(ctx, created.ID, true); err != nil {
		t.Fatalf("DeleteRoom(true) error = %v", err)
	}
	var count int
	if err := store.Reader().QueryRow(`SELECT COUNT(*) FROM live_sessions WHERE room_config_id = ?`, created.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("session count after delete = (%d, %v)", count, err)
	}
}

func TestRoomInputValidation(t *testing.T) {
	for _, value := range []string{"", "https://example.com/room", "two rooms", "live.douyin.com/a/b"} {
		if _, err := NormalizeLiveID(value); ErrorCode(err) != "ROOM_INPUT_INVALID" {
			t.Errorf("NormalizeLiveID(%q) error = %v", value, err)
		}
	}
}

func openTestStore(t *testing.T) *storage.Store {
	t.Helper()
	layout, err := storage.PrepareLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), layout, storage.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return store
}
