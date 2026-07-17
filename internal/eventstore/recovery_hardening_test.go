package eventstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/jwwsjlm/douyinlive-proto/generated/new_douyin"
)

func TestManagerRecoveryRejectsProtectedRawDamageWithoutSpoolOrCheckpointMutation(t *testing.T) {
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
			payloads := [][]byte{
				managerProtoPayload(t, &new_douyin.Webcast_Im_ChatMessage{
					Common:  &new_douyin.Webcast_Im_Common{MsgId: 701},
					Content: "first",
				}),
				managerProtoPayload(t, &new_douyin.Webcast_Im_ChatMessage{
					Common:  &new_douyin.Webcast_Im_Common{MsgId: 702},
					Content: "second",
				}),
			}
			envelopes := make([]IngestEnvelope, len(payloads))
			for index := range envelopes {
				sequence := int64(index + 1)
				envelopes[index] = IngestEnvelope{
					SessionID:       fixture.sessionID,
					EventID:         "protected-raw-recovery-" + string(rune('1'+index)),
					Sequence:        sequence,
					Method:          methodChat,
					PlatformRoomID:  fixture.descriptor.PlatformRoomID,
					ReceivedAt:      fixture.now.Add(time.Duration(sequence) * time.Second),
					SessionOffsetMS: sequence * 1000,
					Payload:         payloads[index],
				}
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
			rawPath := filepath.Join(
				eventsRoot, filepath.FromSlash(results[1].Raw.File),
			)
			if err := os.Truncate(rawPath, results[0].Raw.Offset+3); err != nil {
				t.Fatal(err)
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
					committed,
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
				t.Fatalf("protected raw recovery error = %v", err)
			}
			assertSpoolDiskUnchanged(t, before, eventsRoot)

			after, found, err := fixture.writer.Checkpoint(
				context.Background(), fixture.sessionID,
			)
			if err != nil || !found {
				t.Fatalf("checkpoint after rejection = (%+v, %v, %v)", after, found, err)
			}
			if !reflect.DeepEqual(after, committed) {
				t.Fatalf("checkpoint mutated: before=%+v after=%+v", committed, after)
			}
		})
	}
}
