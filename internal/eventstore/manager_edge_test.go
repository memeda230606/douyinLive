package eventstore

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jwwsjlm/douyinlive-proto/generated/new_douyin"
)

func TestManagerCloseTimeoutJoinsCallerIndependentDrain(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	fixture := newManagerFixture(t, func(options *ManagerOptions) {
		options.SpoolOptions = func(root string) SpoolOptions {
			spoolOptions := DefaultSpoolOptions(root)
			spoolOptions.OpenFile = func(name string, flag int, mode fs.FileMode) (SpoolFile, error) {
				file, err := os.OpenFile(name, flag, mode)
				if err != nil {
					return nil, err
				}
				if strings.HasSuffix(name, ".binpack") && flag&os.O_CREATE != 0 {
					return &spoolBlockingSyncFile{
						SpoolFile: file,
						entered:   entered,
						release:   release,
					}, nil
				}
				return file, nil
			}
			return spoolOptions
		}
	})
	sink, err := fixture.manager.OpenSession(context.Background(), fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	sink.Accept(managerLiveMessage(methodChat, []byte{1}, fixture.now.Add(time.Second)))
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not reach durability barrier")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := sink.FlushAndClose(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed close error = %v", err)
	}
	second := make(chan error, 1)
	go func() { second <- sink.FlushAndClose(context.Background()) }()
	select {
	case err := <-second:
		t.Fatalf("shared close returned before drain release: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shared close did not finish")
	}
}

func TestManagerRecoveryReplaysIdleOnNonGiftWithoutAggregateDrift(t *testing.T) {
	fixture := newManagerFixture(t, nil)
	eventsRoot, err := resolveSessionEventsRoot(fixture.dataRoot, fixture.descriptor)
	if err != nil {
		t.Fatal(err)
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
	gift := &new_douyin.Webcast_Im_GiftMessage{
		Common: &new_douyin.Webcast_Im_Common{MsgId: 501},
		GiftId: 13, GroupId: 99, RepeatCount: 1,
		Gift: &new_douyin.Webcast_Data_GiftStruct{Name: "Fire", Combo: true},
	}
	chat := &new_douyin.Webcast_Im_ChatMessage{
		Common:  &new_douyin.Webcast_Im_Common{MsgId: 502},
		Content: "advance idle",
	}
	spool, err := OpenSpool(DefaultSpoolOptions(eventsRoot))
	if err != nil {
		t.Fatal(err)
	}
	envelopes := []IngestEnvelope{
		{
			SessionID: fixture.sessionID, EventID: "018f0000-0000-7000-8000-000000000501",
			Sequence: 1, Method: methodGift, ReceivedAt: fixture.now,
			Payload: managerProtoPayload(t, gift),
		},
		{
			SessionID: fixture.sessionID, EventID: "018f0000-0000-7000-8000-000000000502",
			Sequence: 2, Method: methodChat, ReceivedAt: fixture.now.Add(20 * time.Second),
			SessionOffsetMS: 20000, Payload: managerProtoPayload(t, chat),
		},
	}
	if _, err := spool.AppendBatch(context.Background(), envelopes); err != nil {
		t.Fatal(err)
	}
	if err := spool.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.RecoverSession(context.Background(), fixture.descriptor); err != nil {
		t.Fatal(err)
	}
	var aggregates int
	if err := fixture.store.Reader().QueryRow(`SELECT COUNT(*) FROM live_events
		WHERE session_id = ? AND event_role = 'aggregate'`, fixture.sessionID).Scan(&aggregates); err != nil {
		t.Fatal(err)
	}
	if aggregates != 1 {
		t.Fatalf("aggregate count after recovery = %d", aggregates)
	}
	restarted, err := NewManager(context.Background(), ManagerOptions{
		DataRoot: fixture.dataRoot, Writer: fixture.writer, Credentials: fixture.credentials,
		BatchInterval: 10 * time.Millisecond,
		Now:           func() time.Time { return fixture.now.Add(40 * time.Second) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Shutdown(context.Background())
	sink, err := restarted.OpenSession(context.Background(), fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if err := sink.FlushAndClose(context.Background()); err != nil {
		t.Fatalf("restarted close error = %v", err)
	}
	if err := fixture.store.Reader().QueryRow(`SELECT COUNT(*) FROM live_events
		WHERE session_id = ? AND event_role = 'aggregate'`, fixture.sessionID).Scan(&aggregates); err != nil {
		t.Fatal(err)
	}
	if aggregates != 1 {
		t.Fatalf("aggregate drift after restart = %d", aggregates)
	}
}
