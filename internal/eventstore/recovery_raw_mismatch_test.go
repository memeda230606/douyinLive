package eventstore

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jwwsjlm/douyinlive-proto/generated/new_douyin"
)

func TestManagerRecoveryRejectsValidRawBoundaryMismatchWithoutMutation(t *testing.T) {
	for _, activePath := range []bool{false, true} {
		name := "inactive"
		if activePath {
			name = "active"
		}
		t.Run(name, func(t *testing.T) {
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

			options := fixture.manager.options.SpoolOptions(eventsRoot)
			options.Root = eventsRoot
			spool, err := OpenSpool(options)
			if err != nil {
				t.Fatal(err)
			}
			envelopes := []IngestEnvelope{
				{
					SessionID:       fixture.sessionID,
					EventID:         "valid-raw-boundary-1",
					Sequence:        1,
					Method:          methodChat,
					PlatformRoomID:  fixture.descriptor.PlatformRoomID,
					ReceivedAt:      fixture.now.Add(time.Second),
					SessionOffsetMS: 1000,
					Payload: managerProtoPayload(t, &new_douyin.Webcast_Im_ChatMessage{
						Common:  &new_douyin.Webcast_Im_Common{MsgId: 801},
						Content: "first",
					}),
				},
				{
					SessionID:       fixture.sessionID,
					EventID:         "valid-raw-boundary-2",
					Sequence:        2,
					Method:          methodChat,
					PlatformRoomID:  fixture.descriptor.PlatformRoomID,
					ReceivedAt:      fixture.now.Add(2 * time.Second),
					SessionOffsetMS: 2000,
					Payload: managerProtoPayload(t, &new_douyin.Webcast_Im_ChatMessage{
						Common:  &new_douyin.Webcast_Im_Common{MsgId: 802},
						Content: "second",
					}),
				},
			}
			results, err := spool.AppendBatch(context.Background(), envelopes)
			if err != nil {
				t.Fatal(err)
			}
			if err := spool.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := fixture.manager.RecoverSession(
				context.Background(), fixture.descriptor,
			); err != nil {
				t.Fatalf("initial RecoverSession() error = %v", err)
			}
			committed, found, err := fixture.writer.Checkpoint(
				context.Background(), fixture.sessionID,
			)
			if err != nil || !found || committed.CommittedSequence != 2 {
				t.Fatalf("committed checkpoint = (%+v, %v, %v)", committed, found, err)
			}

			mismatch := committed
			mismatch.Raw = results[0].Raw
			mismatch.UpdatedAt = committed.UpdatedAt.Add(time.Millisecond)
			if err := fixture.writer.PersistBatch(context.Background(), Batch{
				SessionID:        fixture.sessionID,
				PreviousSequence: committed.CommittedSequence,
				Checkpoint:       mismatch,
			}); err != nil {
				t.Fatalf("persist mismatched checkpoint = %v", err)
			}
			protected, found, err := fixture.writer.Checkpoint(
				context.Background(), fixture.sessionID,
			)
			if err != nil || !found || !reflect.DeepEqual(protected, mismatch) {
				t.Fatalf("protected checkpoint = (%+v, %v, %v), want %+v", protected, found, err, mismatch)
			}

			var active *Spool
			if activePath {
				active, err = OpenSpool(options)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					_ = active.Close(context.Background())
				})
			}
			before := snapshotSpoolDisk(t, eventsRoot)
			if activePath {
				ledger, openErr := OpenDropLedger(eventsRoot, fixture.sessionID)
				if openErr != nil {
					t.Fatal(openErr)
				}
				_, err = fixture.manager.recoverAtRoot(
					context.Background(),
					fixture.descriptor,
					eventsRoot,
					protected,
					active,
					CheckpointOpen,
					ledger,
					NewDeduplicator(DefaultDedupeTTL, DefaultDedupeCapacity),
				)
			} else {
				err = fixture.manager.RecoverSession(
					context.Background(), fixture.descriptor,
				)
			}
			if !errors.Is(err, ErrEventSpoolFatal) {
				t.Fatalf("valid raw boundary mismatch error = %v", err)
			}
			assertSpoolDiskUnchanged(t, before, eventsRoot)

			after, found, err := fixture.writer.Checkpoint(
				context.Background(), fixture.sessionID,
			)
			if err != nil || !found || !reflect.DeepEqual(after, protected) {
				t.Fatalf("checkpoint mutated: before=%+v after=(%+v, %v, %v)",
					protected, after, found, err)
			}
		})
	}
}
