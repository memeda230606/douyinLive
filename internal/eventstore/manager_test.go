package eventstore

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	douyinLive "github.com/jwwsjlm/douyinLive/v2"
	"github.com/jwwsjlm/douyinLive/v2/internal/credentials"
	"github.com/jwwsjlm/douyinlive-proto/generated/new_douyin"
	"google.golang.org/protobuf/proto"
)

type managerMemoryCredentials struct {
	mu     sync.Mutex
	values map[string][]byte
}

func newManagerMemoryCredentials(key []byte) *managerMemoryCredentials {
	store := &managerMemoryCredentials{values: make(map[string][]byte)}
	if key != nil {
		store.values[EventPrivacyCredentialRef] = append([]byte(nil), key...)
	}
	return store
}

func (s *managerMemoryCredentials) Put(_ context.Context, ref string, value []byte) (credentials.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[ref] = append([]byte(nil), value...)
	return credentials.Status{Configured: true, UpdatedAt: 1}, nil
}

func (s *managerMemoryCredentials) Get(_ context.Context, ref string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, found := s.values[ref]
	if !found {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), value...), nil
}

func (s *managerMemoryCredentials) Delete(_ context.Context, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, ref)
	return nil
}

func (s *managerMemoryCredentials) Status(_ context.Context, ref string) (credentials.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, found := s.values[ref]
	return credentials.Status{Configured: found, UpdatedAt: 1}, nil
}

type managerFixture struct {
	writerFixture
	manager     *Manager
	credentials *managerMemoryCredentials
	dataRoot    string
	descriptor  SessionDescriptor
}

func newManagerFixture(t *testing.T, configure func(*ManagerOptions)) managerFixture {
	t.Helper()
	writer := newWriterFixture(t)
	dataRoot := t.TempDir()
	credentialStore := newManagerMemoryCredentials(nil)
	options := ManagerOptions{
		DataRoot:      dataRoot,
		Writer:        writer.writer,
		Credentials:   credentialStore,
		BatchSize:     8,
		BatchInterval: 10 * time.Millisecond,
	}
	if configure != nil {
		configure(&options)
	}
	manager, err := NewManager(context.Background(), options)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
	})
	return managerFixture{
		writerFixture: writer,
		manager:       manager,
		credentials:   credentialStore,
		dataRoot:      dataRoot,
		descriptor: SessionDescriptor{
			SessionID:      writer.sessionID,
			DataPath:       "rooms/writer-room/sessions/2026/07/" + writer.sessionID,
			PlatformRoomID: "platform-room",
			StartedAt:      writer.now,
		},
	}
}

func managerLiveMessage(method string, payload []byte, at time.Time) *douyinLive.LiveMessage {
	return &douyinLive.LiveMessage{
		Raw:        &new_douyin.Webcast_Im_Message{Method: method, Payload: payload},
		ReceivedAt: at,
	}
}

func managerProtoPayload(t *testing.T, message proto.Message) []byte {
	t.Helper()
	payload, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not satisfied before timeout")
}

func TestManagerEndToEndOwnsPayloadFiltersPrivacyAndClosesCheckpoint(t *testing.T) {
	fixture := newManagerFixture(t, func(options *ManagerOptions) {
		options.PrivacyOptions = PrivacyOptions{
			StoreDisplayName:    false,
			MaxContentBytes:     8,
			MaxDisplayNameBytes: 32,
		}
	})
	sink, err := fixture.manager.OpenSession(context.Background(), fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	payload := managerProtoPayload(t, &new_douyin.Webcast_Im_ChatMessage{
		Common: &new_douyin.Webcast_Im_Common{MsgId: 101},
		User: &new_douyin.Webcast_Data_User{
			WebcastUid: "raw-user-identity",
			Nickname:   "private-nickname",
		},
		Content: "secret-content",
	})
	sink.Accept(managerLiveMessage(methodChat, payload, fixture.now.Add(time.Second)))
	for index := range payload {
		payload[index] = 0xff
	}
	if err := sink.FlushAndClose(context.Background()); err != nil {
		t.Fatalf("FlushAndClose() error = %v", err)
	}
	if err := sink.FlushAndClose(context.Background()); err != nil {
		t.Fatalf("shared FlushAndClose() error = %v", err)
	}
	checkpoint, found, err := fixture.writer.Checkpoint(context.Background(), fixture.sessionID)
	if err != nil || !found || checkpoint.State != CheckpointClosed || checkpoint.CommittedSequence != 1 {
		t.Fatalf("checkpoint = (%+v, %v, %v)", checkpoint, found, err)
	}
	var content, userHash, displayName, normalizedJSON, rawFile string
	if err := fixture.store.Reader().QueryRow(`SELECT content, user_hash,
		COALESCE(display_name, ''), normalized_json, raw_file
		FROM live_events WHERE session_id = ? AND event_role = 'source'`, fixture.sessionID).Scan(
		&content, &userHash, &displayName, &normalizedJSON, &rawFile,
	); err != nil {
		t.Fatal(err)
	}
	if content != "secret-c" || userHash == "" || strings.Contains(userHash, "raw-user") ||
		displayName != "" || rawFile == "" {
		t.Fatalf("privacy row = content=%q hash=%q display=%q raw=%q", content, userHash, displayName, rawFile)
	}
	for _, forbidden := range []string{"raw-user-identity", "private-nickname", "secret-content"} {
		if strings.Contains(normalizedJSON, forbidden) {
			t.Fatalf("normalized JSON leaked %q: %s", forbidden, normalizedJSON)
		}
	}
	sink.Accept(managerLiveMessage(methodChat, []byte{1}, fixture.now.Add(2*time.Second)))
	assertRowCount(t, fixture.store.Reader(), "live_events", 1)
}

func TestManagerGiftDedupeStillAdvancesSourceCheckpoint(t *testing.T) {
	fixture := newManagerFixture(t, nil)
	sink, err := fixture.manager.OpenSession(context.Background(), fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	gift := &new_douyin.Webcast_Im_GiftMessage{
		Common:      &new_douyin.Webcast_Im_Common{MsgId: 202},
		GiftId:      7,
		RepeatCount: 1,
		Gift:        &new_douyin.Webcast_Data_GiftStruct{Name: "Rose"},
	}
	payload := managerProtoPayload(t, gift)
	sink.Accept(managerLiveMessage(methodGift, payload, fixture.now.Add(time.Second)))
	sink.Accept(managerLiveMessage(methodGift, payload, fixture.now.Add(2*time.Second)))
	if err := sink.FlushAndClose(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkpoint, _, err := fixture.writer.Checkpoint(context.Background(), fixture.sessionID)
	if err != nil || checkpoint.CommittedSequence != 2 || checkpoint.State != CheckpointClosed {
		t.Fatalf("checkpoint = (%+v, %v)", checkpoint, err)
	}
	var sources, aggregates, combos int
	if err := fixture.store.Reader().QueryRow(`SELECT COUNT(*) FROM live_events
		WHERE session_id = ? AND event_role = 'source'`, fixture.sessionID).Scan(&sources); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Reader().QueryRow(`SELECT COUNT(*) FROM live_events
		WHERE session_id = ? AND event_role = 'aggregate'`, fixture.sessionID).Scan(&aggregates); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Reader().QueryRow(`SELECT COUNT(*) FROM gift_combo_states
		WHERE session_id = ?`, fixture.sessionID).Scan(&combos); err != nil {
		t.Fatal(err)
	}
	if sources != 1 || aggregates != 1 || combos != 1 {
		t.Fatalf("source/aggregate/combo counts = %d/%d/%d", sources, aggregates, combos)
	}
}

func TestManagerCrashRecoveryRebuildsOpenGiftFold(t *testing.T) {
	fixture := newManagerFixture(t, nil)
	eventsRoot, err := resolveSessionEventsRoot(fixture.dataRoot, fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	initial := Checkpoint{
		SessionID:    fixture.sessionID,
		State:        CheckpointOpen,
		PrivacyKeyID: fixture.manager.privacy.KeyID(),
		UpdatedAt:    fixture.now,
	}
	if err := fixture.writer.PersistBatch(context.Background(), Batch{
		SessionID: fixture.sessionID, Checkpoint: initial,
	}); err != nil {
		t.Fatal(err)
	}
	gift := &new_douyin.Webcast_Im_GiftMessage{
		Common:      &new_douyin.Webcast_Im_Common{MsgId: 301},
		GiftId:      9,
		GroupId:     77,
		RepeatCount: 1,
		Gift:        &new_douyin.Webcast_Data_GiftStruct{Name: "Star", Combo: true},
	}
	spool, err := OpenSpool(DefaultSpoolOptions(eventsRoot))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spool.AppendBatch(context.Background(), []IngestEnvelope{{
		SessionID: fixture.sessionID, EventID: "018f0000-0000-7000-8000-000000000301",
		Sequence: 1, Method: methodGift, PlatformRoomID: fixture.descriptor.PlatformRoomID,
		ReceivedAt: fixture.now.Add(time.Second), SessionOffsetMS: 1000,
		Payload: managerProtoPayload(t, gift),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := spool.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.RecoverSession(context.Background(), fixture.descriptor); err != nil {
		t.Fatalf("RecoverSession() error = %v", err)
	}
	var status string
	if err := fixture.store.Reader().QueryRow(`SELECT status FROM gift_combo_states
		WHERE session_id = ?`, fixture.sessionID).Scan(&status); err != nil || status != "open" {
		t.Fatalf("recovered combo status = %q, err=%v", status, err)
	}
	sink, err := fixture.manager.OpenSession(context.Background(), fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	gift.Common.MsgId = 302
	gift.RepeatCount = 2
	gift.RepeatEnd = 1
	sink.Accept(managerLiveMessage(methodGift, managerProtoPayload(t, gift), fixture.now.Add(2*time.Second)))
	if err := sink.FlushAndClose(context.Background()); err != nil {
		t.Fatal(err)
	}
	var first, last, total int64
	if err := fixture.store.Reader().QueryRow(`SELECT status, first_ingest_sequence,
		last_ingest_sequence, total_count FROM gift_combo_states WHERE session_id = ?`,
		fixture.sessionID).Scan(&status, &first, &last, &total); err != nil {
		t.Fatal(err)
	}
	if status != "closed" || first != 1 || last != 2 || total != 2 {
		t.Fatalf("restarted combo = status=%q first=%d last=%d total=%d", status, first, last, total)
	}
}

func TestManagerFlushesIdleGiftWithoutAnotherMessage(t *testing.T) {
	var clockMu sync.Mutex
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	fixture := newManagerFixture(t, func(options *ManagerOptions) {
		options.Now = func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return now
		}
	})
	sink, err := fixture.manager.OpenSession(context.Background(), fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	gift := &new_douyin.Webcast_Im_GiftMessage{
		Common:      &new_douyin.Webcast_Im_Common{MsgId: 401},
		GiftId:      11,
		GroupId:     88,
		RepeatCount: 1,
		Gift:        &new_douyin.Webcast_Data_GiftStruct{Name: "Light", Combo: true},
	}
	sink.Accept(managerLiveMessage(methodGift, managerProtoPayload(t, gift), now))
	eventually(t, func() bool {
		var status string
		err := fixture.store.Reader().QueryRow(`SELECT status FROM gift_combo_states
			WHERE session_id = ?`, fixture.sessionID).Scan(&status)
		return err == nil && status == "open"
	})
	clockMu.Lock()
	now = now.Add(11 * time.Second)
	clockMu.Unlock()
	eventually(t, func() bool {
		var status string
		err := fixture.store.Reader().QueryRow(`SELECT status FROM gift_combo_states
			WHERE session_id = ?`, fixture.sessionID).Scan(&status)
		return err == nil && status == "closed"
	})
	if err := sink.FlushAndClose(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestManagerSpoolFatalStagesFailedBatchBeforeFirstTicker(t *testing.T) {
	controller := &spoolFaultController{
		kind: "raw", operation: "sync", err: errors.New("injected spool sync failure"),
	}
	fixture := newManagerFixture(t, func(options *ManagerOptions) {
		options.BatchSize = 1
		options.BatchInterval = time.Hour
		options.SpoolOptions = func(root string) SpoolOptions {
			spoolOptions := DefaultSpoolOptions(root)
			spoolOptions.OpenFile = func(name string, flag int, mode fs.FileMode) (SpoolFile, error) {
				file, err := os.OpenFile(name, flag, mode)
				if err != nil {
					return nil, err
				}
				kind := "wal"
				if strings.HasSuffix(name, ".binpack") {
					kind = "raw"
				}
				return &spoolFaultFile{
					SpoolFile: file, kind: kind, controller: controller,
				}, nil
			}
			return spoolOptions
		}
	})
	eventsRoot, err := resolveSessionEventsRoot(fixture.dataRoot, fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	sink, err := fixture.manager.OpenSession(context.Background(), fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	sink.Accept(managerLiveMessage(methodChat, []byte{1}, fixture.now.Add(time.Second)))
	eventually(t, func() bool {
		controller.mu.Lock()
		defer controller.mu.Unlock()
		return controller.fired
	})
	eventually(t, func() bool {
		ledger, err := OpenDropLedger(eventsRoot, fixture.sessionID)
		if err != nil {
			return false
		}
		snapshot, pending := ledger.Pending()
		return pending && snapshot.TotalCount == 1
	})
	if err := sink.FlushAndClose(context.Background()); !errors.Is(err, ErrEventSpoolFatal) {
		t.Fatalf("FlushAndClose() error = %v", err)
	}
}

func TestManagerSpoolFatalRetainsRuntimeAndPersistsDropAuditOnClose(t *testing.T) {
	injected := errors.New("injected spool sync failure")
	controller := &spoolFaultController{kind: "raw", operation: "sync", err: injected}
	fixture := newManagerFixture(t, func(options *ManagerOptions) {
		options.SpoolOptions = func(root string) SpoolOptions {
			spoolOptions := DefaultSpoolOptions(root)
			spoolOptions.OpenFile = func(name string, flag int, mode fs.FileMode) (SpoolFile, error) {
				file, err := os.OpenFile(name, flag, mode)
				if err != nil {
					return nil, err
				}
				kind := "wal"
				if strings.HasSuffix(name, ".binpack") {
					kind = "raw"
				}
				return &spoolFaultFile{SpoolFile: file, kind: kind, controller: controller}, nil
			}
			return spoolOptions
		}
	})
	sink, err := fixture.manager.OpenSession(context.Background(), fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	eventsRoot, err := resolveSessionEventsRoot(fixture.dataRoot, fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	sink.Accept(managerLiveMessage(methodChat, []byte{1}, fixture.now.Add(time.Second)))
	eventually(t, func() bool {
		controller.mu.Lock()
		defer controller.mu.Unlock()
		return controller.fired
	})
	for index := 0; index < 3; index++ {
		sink.Accept(managerLiveMessage(methodChat, []byte{byte(index)}, fixture.now.Add(time.Duration(index+2)*time.Second)))
	}
	eventually(t, func() bool {
		ledger, err := OpenDropLedger(eventsRoot, fixture.sessionID)
		if err != nil {
			return false
		}
		snapshot, pending := ledger.Pending()
		return pending && snapshot.TotalCount == 4
	})
	select {
	case <-sink.runtime.done:
		t.Fatal("fatal runtime was removed before external close")
	default:
	}
	err = sink.FlushAndClose(context.Background())
	if !errors.Is(err, ErrEventSpoolFatal) || err.Error() != ErrEventSpoolFatal.Error() {
		t.Fatalf("FlushAndClose() error = %v", err)
	}
	checkpoint, found, err := fixture.writer.Checkpoint(context.Background(), fixture.sessionID)
	if err != nil || !found || checkpoint.State != CheckpointDegraded {
		t.Fatalf("fatal checkpoint = (%+v, %v, %v)", checkpoint, found, err)
	}
	var details string
	if err := fixture.store.Reader().QueryRow(`SELECT details_json FROM capture_gaps
		WHERE session_id = ? AND kind = 'event_persistence'`, fixture.sessionID).Scan(&details); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(details, `"count":4`) {
		t.Fatalf("drop audit details = %s", details)
	}
}

func TestManagerPathBindingAndPrivacyKeyMismatchFailClosed(t *testing.T) {
	fixture := newManagerFixture(t, nil)
	for _, dataPath := range []string{
		"rooms/writer-room/sessions/2026/07/other-session",
		"rooms/writer-room/2026/07/" + fixture.sessionID,
		"../sessions/" + fixture.sessionID,
	} {
		descriptor := fixture.descriptor
		descriptor.DataPath = dataPath
		if _, err := fixture.manager.OpenSession(context.Background(), descriptor); !errors.Is(err, ErrSessionPathInvalid) {
			t.Fatalf("OpenSession(%q) error = %v", dataPath, err)
		}
	}
	initial := Checkpoint{
		SessionID: fixture.sessionID, State: CheckpointOpen,
		PrivacyKeyID: fixture.manager.privacy.KeyID(), UpdatedAt: fixture.now,
	}
	if err := fixture.writer.PersistBatch(context.Background(), Batch{
		SessionID: fixture.sessionID, Checkpoint: initial,
	}); err != nil {
		t.Fatal(err)
	}
	other, err := NewManager(context.Background(), ManagerOptions{
		DataRoot: fixture.dataRoot, Writer: fixture.writer,
		Credentials: newManagerMemoryCredentials([]byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Shutdown(context.Background())
	if _, err := other.OpenSession(context.Background(), fixture.descriptor); !errors.Is(err, ErrPrivacyKeyMismatch) {
		t.Fatalf("key mismatch error = %v", err)
	}
}
