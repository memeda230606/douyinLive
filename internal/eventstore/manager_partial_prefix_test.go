package eventstore

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManagerSpoolFaultAuditsOnlyNonDurableBatchSuffix(t *testing.T) {
	for _, operation := range []string{"write", "sync"} {
		t.Run(operation, func(t *testing.T) {
			injected := errors.New("injected second-segment raw " + operation)
			fault := &targetedSpoolFault{operation: operation, err: injected}
			fixture := newManagerFixture(t, func(options *ManagerOptions) {
				options.BatchSize = 2
				options.BatchInterval = time.Hour
				options.SpoolOptions = func(root string) SpoolOptions {
					spoolOptions := DefaultSpoolOptions(root)
					spoolOptions.RawSegmentBytes = 1
					spoolOptions.WALSegmentBytes = 1
					spoolOptions.OpenFile = func(
						name string, flag int, mode fs.FileMode,
					) (SpoolFile, error) {
						file, err := os.OpenFile(name, flag, mode)
						if err != nil {
							return nil, err
						}
						base := filepath.Base(name)
						if strings.Contains(base, "raw-") &&
							strings.Contains(base, "-000002.binpack") {
							return &targetedSpoolFaultFile{
								SpoolFile: file, fault: fault,
							}, nil
						}
						return file, nil
					}
					return spoolOptions
				}
			})
			sink, err := fixture.manager.OpenSession(
				context.Background(), fixture.descriptor,
			)
			if err != nil {
				t.Fatal(err)
			}
			sink.Accept(managerLiveMessage(
				methodChat, []byte{1}, fixture.now.Add(time.Second),
			))
			sink.Accept(managerLiveMessage(
				methodChat, []byte{2}, fixture.now.Add(2*time.Second),
			))
			eventually(t, func() bool {
				fault.mu.Lock()
				defer fault.mu.Unlock()
				return fault.fired
			})
			if err := sink.FlushAndClose(context.Background()); !errors.Is(
				err, ErrEventSpoolFatal,
			) {
				t.Fatalf("FlushAndClose() error = %v", err)
			}

			checkpoint, found, err := fixture.writer.Checkpoint(
				context.Background(), fixture.sessionID,
			)
			if err != nil || !found || checkpoint.CommittedSequence != 0 ||
				checkpoint.State != CheckpointDegraded {
				t.Fatalf("fatal checkpoint = (%+v, %v, %v)", checkpoint, found, err)
			}
			var details string
			if err := fixture.store.Reader().QueryRow(`SELECT details_json
				FROM capture_gaps WHERE session_id = ? AND kind = 'event_persistence'`,
				fixture.sessionID,
			).Scan(&details); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(details, `"count":1`) {
				t.Fatalf("durable prefix was audited as lost: %s", details)
			}

			if err := fixture.manager.RecoverSession(
				context.Background(), fixture.descriptor,
			); err != nil {
				t.Fatalf("RecoverSession() error = %v", err)
			}
			checkpoint, found, err = fixture.writer.Checkpoint(
				context.Background(), fixture.sessionID,
			)
			if err != nil || !found || checkpoint.CommittedSequence != 1 {
				t.Fatalf("recovered checkpoint = (%+v, %v, %v)", checkpoint, found, err)
			}
			var sources int
			if err := fixture.store.Reader().QueryRow(`SELECT COUNT(*)
				FROM live_events WHERE session_id = ? AND event_role = 'source'`,
				fixture.sessionID,
			).Scan(&sources); err != nil {
				t.Fatal(err)
			}
			if sources != 1 {
				t.Fatalf("recovered durable prefix sources = %d", sources)
			}
			if err := fixture.store.Reader().QueryRow(`SELECT details_json
				FROM capture_gaps WHERE session_id = ? AND kind = 'event_persistence'`,
				fixture.sessionID,
			).Scan(&details); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(details, `"count":1`) {
				t.Fatalf("drop audit changed after prefix recovery: %s", details)
			}
		})
	}
}
