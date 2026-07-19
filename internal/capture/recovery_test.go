package capture

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"
)

func TestRecoverStartupSessionsUsesFixedKeysetCutoffAndClosesEveryOldSession(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	sessions := []LiveSession{
		startupRecoverySession(t, now.Add(-3*time.Minute), RecordingActive),
		startupRecoverySession(t, now.Add(-2*time.Minute), RecordingDisabled),
		startupRecoverySession(t, now.Add(-time.Minute), RecordingFinalizing),
	}
	sort.Slice(sessions, func(left, right int) bool { return sessions[left].ID < sessions[right].ID })
	repository := &startupRecoveryRepositoryStub{sessions: sessions}
	var processCalls int
	processes := startupProcessRecovererStub{recover: func(
		_ context.Context,
		_ string,
	) (SessionProcessRecoveryResult, error) {
		processCalls++
		return SessionProcessRecoveryResult{AttemptsChecked: 1}, nil
	}}
	media := startupMediaRecovererStub{recover: func(
		_ context.Context,
		session LiveSession,
		scanCutoff time.Time,
	) (SessionMediaRecoveryResult, error) {
		if !scanCutoff.Equal(now) {
			t.Fatalf("media scan cutoff = %v, want %v", scanCutoff, now)
		}
		return SessionMediaRecoveryResult{
			Snapshot:     completeStartupMediaSnapshot(session),
			CutoffAt:     time.UnixMilli(session.StartedAt).Add(5 * time.Second),
			WarningCodes: []string{MediaRecoveryOrphanFileWarning},
			OrphanFiles:  1,
		}, nil
	}}
	var eventMinimums = make(map[string]time.Time)
	events := SessionEventRecoveryFunc(func(
		_ context.Context,
		session LiveSession,
		minimum time.Time,
	) (time.Time, error) {
		eventMinimums[session.ID] = minimum
		return minimum.Add(2 * time.Second), nil
	})
	var reported []StartupRecoveryEvent
	report, err := RecoverStartupSessions(context.Background(), StartupRecoveryOptions{
		Repository:       repository,
		ProcessRecoverer: processes,
		MediaRecoverer:   media,
		EventRecoverer:   events,
		Reporter: StartupRecoveryReporterFunc(func(event StartupRecoveryEvent) {
			reported = append(reported, event)
		}),
		Now:      func() time.Time { return now },
		NewID:    func() (string, error) { return newV7(t), nil },
		PageSize: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if processCalls != 2 || report.ScanCutoffMS != now.UnixMilli() || report.Scanned != 3 ||
		report.Recovered != 3 || report.Failed != 0 || report.Warnings != 2 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if len(repository.queries) != 2 || repository.queries[0].AfterID != "" ||
		repository.queries[1].AfterID != sessions[1].ID ||
		repository.queries[0].ScanCutoffMS != repository.queries[1].ScanCutoffMS {
		t.Fatalf("invalid keyset queries: %+v", repository.queries)
	}
	if len(repository.closed) != 3 || len(reported) != 3 {
		t.Fatalf("closed/reported = %d/%d", len(repository.closed), len(reported))
	}
	for index, input := range repository.closed {
		session := sessions[index]
		if input.SessionID != session.ID || input.ExpectedStatus != session.Status ||
			input.ExpectedRecordingStatus != session.RecordingStatus ||
			input.ExpectedOperationID != session.OperationID ||
			input.RecoveryOperationID == session.OperationID ||
			input.ScanCutoffMS != now.UnixMilli() || len(input.Gaps) != 1 {
			t.Fatalf("invalid close input %d: %+v", index, input)
		}
		gap := input.Gaps[0]
		wantKind := "process_crash"
		wantMinimum := time.UnixMilli(session.StartedAt).Add(5 * time.Second)
		if session.RecordingStatus == RecordingDisabled {
			wantKind = "message_disconnect"
			wantMinimum = time.UnixMilli(session.StartedAt)
		}
		if gap.Kind != wantKind || !gap.Recovered || gap.StartedAtMS != input.EndedAtMS ||
			input.EndedAtMS != wantMinimum.Add(2*time.Second).UnixMilli() ||
			!eventMinimums[session.ID].Equal(wantMinimum) {
			t.Fatalf("invalid recovered gap %d: %+v minimum=%v", index, gap, eventMinimums[session.ID])
		}
		if reported[index].State != StartupRecoverySessionCompleted ||
			reported[index].ErrorCode != "" {
			t.Fatalf("invalid report event %d: %+v", index, reported[index])
		}
	}
}

func TestRecoverStartupSessionsTerminalizesEventFailureAndContinues(t *testing.T) {
	now := time.Date(2026, 7, 19, 13, 0, 0, 0, time.UTC)
	sessions := []LiveSession{
		startupRecoverySession(t, now.Add(-2*time.Minute), RecordingDisabled),
		startupRecoverySession(t, now.Add(-time.Minute), RecordingDisabled),
	}
	sort.Slice(sessions, func(left, right int) bool { return sessions[left].ID < sessions[right].ID })
	repository := &startupRecoveryRepositoryStub{sessions: sessions}
	var calls int
	var reported []StartupRecoveryEvent
	report, err := RecoverStartupSessions(context.Background(), StartupRecoveryOptions{
		Repository:       repository,
		ProcessRecoverer: startupProcessRecovererStub{},
		EventRecoverer: SessionEventRecoveryFunc(func(
			_ context.Context,
			session LiveSession,
			minimum time.Time,
		) (time.Time, error) {
			calls++
			if session.ID == sessions[0].ID {
				return time.Time{}, errors.New("injected event failure")
			}
			return minimum, nil
		}),
		Reporter: StartupRecoveryReporterFunc(func(event StartupRecoveryEvent) {
			reported = append(reported, event)
		}),
		Now:      func() time.Time { return now },
		NewID:    func() (string, error) { return newV7(t), nil },
		PageSize: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || report.Scanned != 2 || report.Recovered != 2 || report.Failed != 0 ||
		len(repository.closed) != 2 || len(reported) != 2 {
		t.Fatalf("recovery did not continue: report=%+v calls=%d closed=%d reported=%d",
			report, calls, len(repository.closed), len(reported))
	}
	if reported[0].State != StartupRecoverySessionCompleted ||
		!containsStartupRecoveryWarning(reported[0].WarningCodes, StartupRecoveryEventFailedCode) ||
		reported[1].State != StartupRecoverySessionCompleted || len(repository.closed[0].Gaps) != 2 ||
		repository.closed[0].Gaps[0].Recovered || repository.closed[0].Gaps[0].Severity != "error" ||
		repository.closed[0].Gaps[1].Kind != "event_persistence" {
		t.Fatalf("unexpected events: %+v", reported)
	}
}

func TestRecoverStartupSessionsUsesCommittedCutoffFromPermanentEventFailure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 13, 20, 0, 0, time.UTC)
	session := startupRecoverySession(t, now.Add(-5*time.Minute), RecordingDisabled)
	committedCutoff := now.Add(-time.Minute)
	repository := &startupRecoveryRepositoryStub{sessions: []LiveSession{session}}
	var reported StartupRecoveryEvent
	report, err := RecoverStartupSessions(context.Background(), StartupRecoveryOptions{
		Repository:       repository,
		ProcessRecoverer: startupProcessRecovererStub{},
		EventRecoverer: SessionEventRecoveryFunc(func(
			_ context.Context,
			got LiveSession,
			minimum time.Time,
		) (time.Time, error) {
			if got.ID != session.ID || !minimum.Equal(time.UnixMilli(session.StartedAt)) {
				t.Fatalf("event recovery input = %s / %v", got.ID, minimum)
			}
			return committedCutoff, errors.New("permanent event tail damage")
		}),
		Reporter: StartupRecoveryReporterFunc(func(event StartupRecoveryEvent) {
			reported = event
		}),
		Now:   func() time.Time { return now },
		NewID: func() (string, error) { return newV7(t), nil },
	})
	if err != nil || report.Recovered != 1 || report.Failed != 0 ||
		len(repository.closed) != 1 {
		t.Fatalf("permanent event recovery = report:%+v err:%v closed:%d",
			report, err, len(repository.closed))
	}
	closed := repository.closed[0]
	if closed.EndedAtMS != committedCutoff.UnixMilli() || len(closed.Gaps) != 2 {
		t.Fatalf("terminal cutoff/gaps = %d/%+v, want %d/two gaps",
			closed.EndedAtMS, closed.Gaps, committedCutoff.UnixMilli())
	}
	for _, gap := range closed.Gaps {
		if gap.StartedAtMS != committedCutoff.UnixMilli() ||
			gap.EndedAtMS == nil || *gap.EndedAtMS != committedCutoff.UnixMilli() {
			t.Fatalf("gap cutoff = %+v, want %d", gap, committedCutoff.UnixMilli())
		}
	}
	if reported.CutoffAtMS != committedCutoff.UnixMilli() ||
		!containsStartupRecoveryWarning(reported.WarningCodes, StartupRecoveryEventFailedCode) {
		t.Fatalf("reported recovery = %+v", reported)
	}
}

func TestRecoverStartupSessionsDefersRetryableEventFailureAndContinues(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 13, 30, 0, 0, time.UTC)
	sessions := []LiveSession{
		startupRecoverySession(t, now.Add(-2*time.Minute), RecordingDisabled),
		startupRecoverySession(t, now.Add(-time.Minute), RecordingDisabled),
	}
	sort.Slice(sessions, func(left, right int) bool { return sessions[left].ID < sessions[right].ID })
	repository := &startupRecoveryRepositoryStub{sessions: sessions}
	eventCause := errors.New("private transient event failure")
	eventCalls := 0
	var reported []StartupRecoveryEvent
	report, err := RecoverStartupSessions(context.Background(), StartupRecoveryOptions{
		Repository:       repository,
		ProcessRecoverer: startupProcessRecovererStub{},
		EventRecoverer: SessionEventRecoveryFunc(func(
			_ context.Context,
			session LiveSession,
			minimum time.Time,
		) (time.Time, error) {
			eventCalls++
			if session.ID == sessions[0].ID {
				return time.Time{}, errors.Join(ErrStartupEventRecoveryDeferred, eventCause)
			}
			return minimum, nil
		}),
		Reporter: StartupRecoveryReporterFunc(func(event StartupRecoveryEvent) {
			reported = append(reported, event)
		}),
		Now:      func() time.Time { return now },
		NewID:    func() (string, error) { return newV7(t), nil },
		PageSize: 1,
	})
	if !errors.Is(err, ErrStartupRecoveryIncomplete) ||
		!errors.Is(err, ErrStartupEventRecoveryDeferred) ||
		errors.Is(err, ErrStartupProcessRecovery) || errors.Is(err, eventCause) {
		t.Fatalf("deferred event aggregate error = %v", err)
	}
	if report.Scanned != 2 || report.Failed != 1 || report.Recovered != 1 ||
		report.Warnings != 1 || eventCalls != 2 || len(repository.closed) != 1 ||
		repository.closed[0].SessionID != sessions[1].ID || len(repository.queries) != 3 ||
		len(reported) != 2 {
		t.Fatalf("deferred event recovery did not continue: report=%+v calls=%d closed=%+v queries=%d reported=%+v",
			report, eventCalls, repository.closed, len(repository.queries), reported)
	}
	if reported[0].SessionID != sessions[0].ID ||
		reported[0].State != StartupRecoverySessionFailed ||
		reported[0].ErrorCode != StartupRecoveryEventFailedCode ||
		!containsStartupRecoveryWarning(reported[0].WarningCodes, StartupRecoveryEventFailedCode) ||
		reported[1].SessionID != sessions[1].ID ||
		reported[1].State != StartupRecoverySessionCompleted {
		t.Fatalf("deferred event reports = %+v", reported)
	}
	for _, input := range repository.closed {
		if input.SessionID == sessions[0].ID {
			t.Fatalf("deferred session was terminalized: %+v", input)
		}
	}
}

func TestRecoverStartupSessionsMissingMediaClosesIncompleteWithStableGap(t *testing.T) {
	now := time.Date(2026, 7, 19, 14, 0, 0, 0, time.UTC)
	session := startupRecoverySession(t, now.Add(-time.Minute), RecordingActive)
	repository := &startupRecoveryRepositoryStub{sessions: []LiveSession{session}}
	report, err := RecoverStartupSessions(context.Background(), StartupRecoveryOptions{
		Repository:       repository,
		ProcessRecoverer: startupProcessRecovererStub{},
		MediaRecoverer: startupMediaRecovererStub{recover: func(
			context.Context,
			LiveSession,
			time.Time,
		) (SessionMediaRecoveryResult, error) {
			return SessionMediaRecoveryResult{}, ErrSessionMediaNotFound
		}},
		EventRecoverer: SessionEventRecoveryFunc(func(
			_ context.Context,
			_ LiveSession,
			minimum time.Time,
		) (time.Time, error) {
			return minimum, nil
		}),
		Now:   func() time.Time { return now },
		NewID: func() (string, error) { return newV7(t), nil },
	})
	if err != nil || report.Recovered != 1 || len(repository.closed) != 1 {
		t.Fatalf("missing media recovery = report %+v error %v closed %d",
			report, err, len(repository.closed))
	}
	input := repository.closed[0]
	if input.IntegrityScore != 0.5 || len(input.Gaps) != 1 ||
		input.Gaps[0].Recovered || input.Gaps[0].Severity != "error" ||
		input.Gaps[0].ReasonCode != "STARTUP_RECOVERY_INTERRUPTED" {
		t.Fatalf("missing media was not audited: %+v", input)
	}
}

func TestRecoverStartupSessionsFailsClosedBeforeMediaWhenProcessSafetyIsUnknown(t *testing.T) {
	now := time.Date(2026, 7, 19, 14, 30, 0, 0, time.UTC)
	sessions := []LiveSession{
		startupRecoverySession(t, now.Add(-3*time.Minute), RecordingActive),
		startupRecoverySession(t, now.Add(-2*time.Minute), RecordingActive),
		startupRecoverySession(t, now.Add(-time.Minute), RecordingActive),
	}
	sort.Slice(sessions, func(left, right int) bool { return sessions[left].ID < sessions[right].ID })
	repository := &startupRecoveryRepositoryStub{sessions: sessions}
	mediaCalls := 0
	eventCalls := 0
	processCalls := 0
	var reported []StartupRecoveryEvent
	report, err := RecoverStartupSessions(context.Background(), StartupRecoveryOptions{
		Repository: repository,
		ProcessRecoverer: startupProcessRecovererStub{recover: func(
			_ context.Context,
			sessionID string,
		) (SessionProcessRecoveryResult, error) {
			processCalls++
			if sessionID != sessions[0].ID {
				return SessionProcessRecoveryResult{}, nil
			}
			return SessionProcessRecoveryResult{
				AttemptsChecked: 1, ProcessesFound: 1,
				ErrorCode: "SESSION_PROCESS_RECOVERY_PROCESS_FAILED",
			}, errors.New("injected process recovery failure")
		}},
		MediaRecoverer: startupMediaRecovererStub{recover: func(
			context.Context,
			LiveSession,
			time.Time,
		) (SessionMediaRecoveryResult, error) {
			mediaCalls++
			return SessionMediaRecoveryResult{
				Snapshot: completeStartupMediaSnapshot(sessions[mediaCalls]),
			}, nil
		}},
		EventRecoverer: SessionEventRecoveryFunc(func(
			_ context.Context,
			_ LiveSession,
			minimum time.Time,
		) (time.Time, error) {
			eventCalls++
			return minimum, nil
		}),
		Reporter: StartupRecoveryReporterFunc(func(event StartupRecoveryEvent) {
			reported = append(reported, event)
		}),
		Now:      func() time.Time { return now },
		NewID:    func() (string, error) { return newV7(t), nil },
		PageSize: 2,
	})
	if !errors.Is(err, ErrStartupRecoveryIncomplete) ||
		!errors.Is(err, ErrStartupProcessRecovery) || report.Scanned != 3 ||
		report.Failed != 1 || report.Recovered != 2 || processCalls != 3 ||
		mediaCalls != 2 || eventCalls != 2 || len(repository.closed) != 2 ||
		len(repository.queries) != 2 || len(reported) != 3 ||
		reported[0].State != StartupRecoverySessionFailed ||
		reported[0].ErrorCode != StartupRecoveryProcessFailedCode ||
		reported[1].State != StartupRecoverySessionCompleted ||
		reported[2].State != StartupRecoverySessionCompleted {
		t.Fatalf("process fail-open: report=%+v err=%v process=%d media=%d event=%d closed=%d queries=%d reported=%+v",
			report, err, processCalls, mediaCalls, eventCalls, len(repository.closed),
			len(repository.queries), reported)
	}
}

func TestRecoverStartupSessionsTreatsIncompleteSnapshotAsUnrecovered(t *testing.T) {
	now := time.Date(2026, 7, 19, 15, 0, 0, 0, time.UTC)
	session := startupRecoverySession(t, now.Add(-time.Minute), RecordingActive)
	repository := &startupRecoveryRepositoryStub{sessions: []LiveSession{session}}
	report, err := RecoverStartupSessions(context.Background(), StartupRecoveryOptions{
		Repository:       repository,
		ProcessRecoverer: startupProcessRecovererStub{},
		MediaRecoverer: startupMediaRecovererStub{recover: func(
			context.Context,
			LiveSession,
			time.Time,
		) (SessionMediaRecoveryResult, error) {
			return SessionMediaRecoveryResult{
				Snapshot: MediaSnapshot{Session: SessionMedia{
					SessionID: session.ID, State: SessionMediaIncomplete,
				}},
				CutoffAt: time.UnixMilli(session.StartedAt).Add(10 * time.Second),
			}, nil
		}},
		EventRecoverer: SessionEventRecoveryFunc(func(
			_ context.Context,
			_ LiveSession,
			minimum time.Time,
		) (time.Time, error) {
			return minimum, nil
		}),
		Now:   func() time.Time { return now },
		NewID: func() (string, error) { return newV7(t), nil },
	})
	if err != nil || report.Recovered != 1 || len(repository.closed) != 1 {
		t.Fatalf("incomplete recovery = report:%+v err:%v closed:%d", report, err, len(repository.closed))
	}
	input := repository.closed[0]
	if report.Warnings != 1 || input.IntegrityScore != 0.5 || input.Gaps[0].Recovered ||
		input.Gaps[0].Severity != "error" {
		t.Fatalf("incomplete media was reported recovered: %+v", input)
	}
}

func TestRecoverStartupSessionsPreservesFutureEventCutoffAndAuditsClock(t *testing.T) {
	now := time.Date(2026, 7, 19, 16, 0, 0, 0, time.UTC)
	session := startupRecoverySession(t, now.Add(-time.Minute), RecordingDisabled)
	repository := &startupRecoveryRepositoryStub{sessions: []LiveSession{session}}
	future := now.Add(time.Minute)
	report, err := RecoverStartupSessions(context.Background(), StartupRecoveryOptions{
		Repository:       repository,
		ProcessRecoverer: startupProcessRecovererStub{},
		EventRecoverer: SessionEventRecoveryFunc(func(
			context.Context,
			LiveSession,
			time.Time,
		) (time.Time, error) {
			return future, nil
		}),
		Now:   func() time.Time { return now },
		NewID: func() (string, error) { return newV7(t), nil },
	})
	if err != nil || report.Recovered != 1 || len(repository.closed) != 1 {
		t.Fatalf("future cutoff recovery = report:%+v err:%v", report, err)
	}
	input := repository.closed[0]
	if input.EndedAtMS != future.UnixMilli() || len(input.Gaps) != 2 ||
		input.Gaps[1].Kind != "clock_uncertain" || input.Gaps[1].Recovered {
		t.Fatalf("future cutoff was lost or unaudited: %+v", input)
	}
}

func TestRecoverStartupSessionsReturnsContextCancellationFromPageLoad(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 16, 30, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	repository := &startupRecoveryRepositoryStub{list: func(
		context.Context,
		RecoverablePageQuery,
	) (RecoverableSessionPage, error) {
		cancel()
		return RecoverableSessionPage{}, errors.New("page stopped after cancellation")
	}}
	report, err := RecoverStartupSessions(ctx, StartupRecoveryOptions{
		Repository:       repository,
		ProcessRecoverer: startupProcessRecovererStub{},
		EventRecoverer: SessionEventRecoveryFunc(func(
			context.Context,
			LiveSession,
			time.Time,
		) (time.Time, error) {
			t.Fatal("cancelled page load must not recover events")
			return time.Time{}, nil
		}),
		Now: func() time.Time { return now },
	})
	if report.Scanned != 0 || !errors.Is(err, context.Canceled) ||
		errors.Is(err, ErrStartupRecoveryIncomplete) ||
		errors.Is(err, ErrStartupProcessRecovery) {
		t.Fatalf("cancelled page result = report:%+v err:%v", report, err)
	}
}

func TestRecoverStartupSessionsTreatsProcessComponentContextErrorsAsFailures(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 16, 40, 0, 0, time.UTC)
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		cause := cause
		t.Run(cause.Error(), func(t *testing.T) {
			t.Parallel()
			sessions := []LiveSession{
				startupRecoverySession(t, now.Add(-2*time.Minute), RecordingActive),
				startupRecoverySession(t, now.Add(-time.Minute), RecordingActive),
			}
			sort.Slice(sessions, func(left, right int) bool { return sessions[left].ID < sessions[right].ID })
			repository := &startupRecoveryRepositoryStub{sessions: sessions}
			processCalls := 0
			mediaCalls := 0
			eventCalls := 0
			var reported []StartupRecoveryEvent
			report, err := RecoverStartupSessions(context.Background(), StartupRecoveryOptions{
				Repository: repository,
				ProcessRecoverer: startupProcessRecovererStub{recover: func(
					_ context.Context,
					sessionID string,
				) (SessionProcessRecoveryResult, error) {
					processCalls++
					if sessionID == sessions[0].ID {
						return SessionProcessRecoveryResult{
							State:     SessionProcessRecoveryFailed,
							ErrorCode: SessionProcessRecoveryInterruptedCode,
						}, cause
					}
					return SessionProcessRecoveryResult{}, nil
				}},
				MediaRecoverer: startupMediaRecovererStub{recover: func(
					_ context.Context,
					session LiveSession,
					_ time.Time,
				) (SessionMediaRecoveryResult, error) {
					mediaCalls++
					return SessionMediaRecoveryResult{
						Snapshot: completeStartupMediaSnapshot(session),
					}, nil
				}},
				EventRecoverer: SessionEventRecoveryFunc(func(
					_ context.Context,
					_ LiveSession,
					minimum time.Time,
				) (time.Time, error) {
					eventCalls++
					return minimum, nil
				}),
				Reporter: StartupRecoveryReporterFunc(func(event StartupRecoveryEvent) {
					reported = append(reported, event)
				}),
				Now:      func() time.Time { return now },
				NewID:    func() (string, error) { return newV7(t), nil },
				PageSize: 1,
			})
			if !errors.Is(err, ErrStartupRecoveryIncomplete) ||
				!errors.Is(err, ErrStartupProcessRecovery) || errors.Is(err, cause) {
				t.Fatalf("component process error = %v", err)
			}
			if report.Scanned != 2 || report.Failed != 1 || report.Recovered != 1 ||
				report.Warnings != 1 || processCalls != 2 || mediaCalls != 1 ||
				eventCalls != 1 || len(repository.closed) != 1 ||
				repository.closed[0].SessionID != sessions[1].ID || len(reported) != 2 ||
				reported[0].State != StartupRecoverySessionFailed ||
				reported[0].ErrorCode != StartupRecoveryProcessFailedCode ||
				reported[1].State != StartupRecoverySessionCompleted {
				t.Fatalf("component process recovery did not continue safely: report=%+v process=%d media=%d event=%d closed=%+v reported=%+v",
					report, processCalls, mediaCalls, eventCalls, repository.closed, reported)
			}
		})
	}
}

func TestRecoverStartupSessionsTreatsMediaComponentContextErrorsAsIncomplete(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 16, 42, 0, 0, time.UTC)
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		cause := cause
		t.Run(cause.Error(), func(t *testing.T) {
			t.Parallel()
			session := startupRecoverySession(t, now.Add(-time.Minute), RecordingActive)
			repository := &startupRecoveryRepositoryStub{sessions: []LiveSession{session}}
			eventCalls := 0
			var reported []StartupRecoveryEvent
			report, err := RecoverStartupSessions(context.Background(), StartupRecoveryOptions{
				Repository:       repository,
				ProcessRecoverer: startupProcessRecovererStub{},
				MediaRecoverer: startupMediaRecovererStub{recover: func(
					context.Context,
					LiveSession,
					time.Time,
				) (SessionMediaRecoveryResult, error) {
					return SessionMediaRecoveryResult{}, cause
				}},
				EventRecoverer: SessionEventRecoveryFunc(func(
					_ context.Context,
					_ LiveSession,
					minimum time.Time,
				) (time.Time, error) {
					eventCalls++
					return minimum, nil
				}),
				Reporter: StartupRecoveryReporterFunc(func(event StartupRecoveryEvent) {
					reported = append(reported, event)
				}),
				Now:   func() time.Time { return now },
				NewID: func() (string, error) { return newV7(t), nil },
			})
			if err != nil || report.Recovered != 1 || report.Failed != 0 ||
				report.Warnings != 1 || eventCalls != 1 || len(repository.closed) != 1 ||
				len(repository.closed[0].Gaps) != 1 || repository.closed[0].Gaps[0].Recovered ||
				repository.closed[0].Gaps[0].Severity != "error" || len(reported) != 1 ||
				reported[0].State != StartupRecoverySessionCompleted ||
				!containsStartupRecoveryWarning(reported[0].WarningCodes, MediaRecoveryFailedWarning) {
				t.Fatalf("component media error was misclassified: cause=%v report=%+v err=%v event=%d closed=%+v reported=%+v",
					cause, report, err, eventCalls, repository.closed, reported)
			}
		})
	}
}

func TestRecoverStartupSessionsTreatsEventComponentContextErrorsAsDataDamage(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 16, 43, 0, 0, time.UTC)
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		cause := cause
		t.Run(cause.Error(), func(t *testing.T) {
			t.Parallel()
			session := startupRecoverySession(t, now.Add(-time.Minute), RecordingDisabled)
			repository := &startupRecoveryRepositoryStub{sessions: []LiveSession{session}}
			var reported []StartupRecoveryEvent
			report, err := RecoverStartupSessions(context.Background(), StartupRecoveryOptions{
				Repository:       repository,
				ProcessRecoverer: startupProcessRecovererStub{},
				EventRecoverer: SessionEventRecoveryFunc(func(
					context.Context,
					LiveSession,
					time.Time,
				) (time.Time, error) {
					return time.Time{}, cause
				}),
				Reporter: StartupRecoveryReporterFunc(func(event StartupRecoveryEvent) {
					reported = append(reported, event)
				}),
				Now:   func() time.Time { return now },
				NewID: func() (string, error) { return newV7(t), nil },
			})
			if err != nil || report.Recovered != 1 || report.Failed != 0 ||
				report.Warnings != 1 || len(repository.closed) != 1 ||
				len(repository.closed[0].Gaps) != 2 || repository.closed[0].Gaps[0].Recovered ||
				repository.closed[0].Gaps[1].Kind != "event_persistence" ||
				repository.closed[0].Gaps[1].Recovered || len(reported) != 1 ||
				reported[0].State != StartupRecoverySessionCompleted ||
				!containsStartupRecoveryWarning(reported[0].WarningCodes, StartupRecoveryEventFailedCode) {
				t.Fatalf("component event error was misclassified: cause=%v report=%+v err=%v closed=%+v reported=%+v",
					cause, report, err, repository.closed, reported)
			}
		})
	}
}

func TestRecoverStartupSessionsStopsOnlyForParentSessionRecoveryInterruption(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 16, 45, 0, 0, time.UTC)
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		cause := cause
		t.Run(cause.Error(), func(t *testing.T) {
			t.Parallel()
			ctx := newStartupRecoveryInterruptContext()
			session := startupRecoverySession(t, now.Add(-time.Minute), RecordingActive)
			repository := &startupRecoveryRepositoryStub{sessions: []LiveSession{session}}
			processCalls := 0
			report, err := RecoverStartupSessions(ctx, StartupRecoveryOptions{
				Repository: repository,
				ProcessRecoverer: startupProcessRecovererStub{recover: func(
					context.Context,
					string,
				) (SessionProcessRecoveryResult, error) {
					processCalls++
					ctx.interrupt(cause)
					return SessionProcessRecoveryResult{}, errors.New("component stopped after parent interruption")
				}},
				MediaRecoverer: startupMediaRecovererStub{recover: func(
					context.Context,
					LiveSession,
					time.Time,
				) (SessionMediaRecoveryResult, error) {
					t.Fatal("parent interruption must stop before media")
					return SessionMediaRecoveryResult{}, nil
				}},
				EventRecoverer: SessionEventRecoveryFunc(func(
					context.Context,
					LiveSession,
					time.Time,
				) (time.Time, error) {
					t.Fatal("parent interruption must stop before events")
					return time.Time{}, nil
				}),
				Now: func() time.Time { return now },
			})
			if !errors.Is(err, cause) || errors.Is(err, ErrStartupRecoveryIncomplete) ||
				errors.Is(err, ErrStartupProcessRecovery) {
				t.Fatalf("parent-interrupted session error = %v", err)
			}
			if report.Scanned != 1 || report.Failed != 0 || report.Recovered != 0 ||
				report.Warnings != 0 || processCalls != 1 || len(repository.closed) != 0 ||
				len(repository.queries) != 1 {
				t.Fatalf("parent-interrupted session report = %+v process:%d closed:%d queries:%d", report, processCalls, len(repository.closed), len(repository.queries))
			}
		})
	}
}

func TestRecoverStartupSessionsFailsClosedOnUninspectedRepositoryPages(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 17, 0, 0, 0, time.UTC)
	pageFailure := errors.New("injected page failure")
	tests := []struct {
		name          string
		repository    *startupRecoveryRepositoryStub
		wantScanned   int
		wantRecovered int
	}{
		{
			name: "first page failure",
			repository: &startupRecoveryRepositoryStub{list: func(
				context.Context,
				RecoverablePageQuery,
			) (RecoverableSessionPage, error) {
				return RecoverableSessionPage{}, pageFailure
			}},
		},
		{
			name: "second page failure",
			repository: func() *startupRecoveryRepositoryStub {
				session := startupRecoverySession(t, now.Add(-time.Minute), RecordingDisabled)
				repository := &startupRecoveryRepositoryStub{sessions: []LiveSession{session}}
				repository.list = func(
					_ context.Context,
					query RecoverablePageQuery,
				) (RecoverableSessionPage, error) {
					if query.AfterID == "" {
						return RecoverableSessionPage{
							Sessions: []LiveSession{session}, NextID: session.ID,
						}, nil
					}
					return RecoverableSessionPage{}, pageFailure
				}
				return repository
			}(),
			wantScanned: 1, wantRecovered: 1,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			report, err := RecoverStartupSessions(context.Background(), StartupRecoveryOptions{
				Repository:       test.repository,
				ProcessRecoverer: startupProcessRecovererStub{},
				EventRecoverer: SessionEventRecoveryFunc(func(
					_ context.Context,
					_ LiveSession,
					minimum time.Time,
				) (time.Time, error) {
					return minimum, nil
				}),
				Now:      func() time.Time { return now },
				NewID:    func() (string, error) { return newV7(t), nil },
				PageSize: 1,
			})
			if !errors.Is(err, ErrStartupRecoveryIncomplete) ||
				!errors.Is(err, ErrStartupProcessRecovery) ||
				!errors.Is(err, pageFailure) {
				t.Fatalf("page failure error = %v", err)
			}
			if report.Scanned != test.wantScanned || report.Recovered != test.wantRecovered {
				t.Fatalf("page failure report = %+v", report)
			}
		})
	}
}

func TestRecoverStartupSessionsFailsClosedOnInvalidCursor(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 17, 30, 0, 0, time.UTC)
	repository := &startupRecoveryRepositoryStub{list: func(
		context.Context,
		RecoverablePageQuery,
	) (RecoverableSessionPage, error) {
		return RecoverableSessionPage{NextID: "invalid-private-cursor"}, nil
	}}
	report, err := RecoverStartupSessions(context.Background(), StartupRecoveryOptions{
		Repository:       repository,
		ProcessRecoverer: startupProcessRecovererStub{},
		EventRecoverer: SessionEventRecoveryFunc(func(
			context.Context,
			LiveSession,
			time.Time,
		) (time.Time, error) {
			t.Fatal("invalid cursor must stop before event recovery")
			return time.Time{}, nil
		}),
		Now: func() time.Time { return now },
	})
	if report.Scanned != 0 || !errors.Is(err, ErrStartupRecoveryIncomplete) ||
		!errors.Is(err, ErrStartupProcessRecovery) {
		t.Fatalf("invalid cursor result = report:%+v err:%v", report, err)
	}
}

func TestRecoverStartupSessionsFailsClosedOnMalformedRepositoryPages(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 17, 45, 0, 0, time.UTC)
	sessions := []LiveSession{
		startupRecoverySession(t, now.Add(-2*time.Minute), RecordingDisabled),
		startupRecoverySession(t, now.Add(-time.Minute), RecordingDisabled),
	}
	sort.Slice(sessions, func(left, right int) bool { return sessions[left].ID < sessions[right].ID })
	invalidSession := sessions[0]
	invalidSession.ID = "not-a-uuid-private-row"
	tests := []struct {
		name     string
		page     RecoverableSessionPage
		pageSize int
	}{
		{
			name: "unordered IDs",
			page: RecoverableSessionPage{
				Sessions: []LiveSession{sessions[1], sessions[0]},
				NextID:   sessions[0].ID,
			},
			pageSize: 2,
		},
		{
			name: "duplicate IDs",
			page: RecoverableSessionPage{
				Sessions: []LiveSession{sessions[0], sessions[0]},
				NextID:   sessions[0].ID,
			},
			pageSize: 2,
		},
		{
			name: "invalid session ID",
			page: RecoverableSessionPage{
				Sessions: []LiveSession{invalidSession},
			},
			pageSize: 2,
		},
		{
			name: "mismatched next ID",
			page: RecoverableSessionPage{
				Sessions: []LiveSession{sessions[0]},
				NextID:   sessions[1].ID,
			},
			pageSize: 1,
		},
		{
			name: "empty page with cursor",
			page: RecoverableSessionPage{
				NextID: sessions[0].ID,
			},
			pageSize: 1,
		},
		{
			name: "full page without cursor",
			page: RecoverableSessionPage{
				Sessions: []LiveSession{sessions[0]},
			},
			pageSize: 1,
		},
		{
			name: "short page with cursor",
			page: RecoverableSessionPage{
				Sessions: []LiveSession{sessions[0]},
				NextID:   sessions[0].ID,
			},
			pageSize: 2,
		},
		{
			name: "page exceeds limit",
			page: RecoverableSessionPage{
				Sessions: []LiveSession{sessions[0], sessions[1]},
				NextID:   sessions[1].ID,
			},
			pageSize: 1,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			processCalls := 0
			repository := &startupRecoveryRepositoryStub{list: func(
				context.Context,
				RecoverablePageQuery,
			) (RecoverableSessionPage, error) {
				return test.page, nil
			}}
			report, err := RecoverStartupSessions(context.Background(), StartupRecoveryOptions{
				Repository: repository,
				ProcessRecoverer: startupProcessRecovererStub{recover: func(
					context.Context,
					string,
				) (SessionProcessRecoveryResult, error) {
					processCalls++
					return SessionProcessRecoveryResult{}, nil
				}},
				EventRecoverer: SessionEventRecoveryFunc(func(
					context.Context,
					LiveSession,
					time.Time,
				) (time.Time, error) {
					t.Fatal("malformed page must fail before event recovery")
					return time.Time{}, nil
				}),
				Reporter: StartupRecoveryReporterFunc(func(StartupRecoveryEvent) {
					t.Fatal("malformed page must fail before reporting a session")
				}),
				Now:      func() time.Time { return now },
				PageSize: test.pageSize,
			})
			if report.Scanned != 0 || report.Recovered != 0 || report.Failed != 0 ||
				processCalls != 0 || len(repository.queries) != 1 || len(repository.closed) != 0 ||
				!errors.Is(err, ErrStartupRecoveryIncomplete) ||
				!errors.Is(err, ErrStartupProcessRecovery) {
				t.Fatalf("malformed page was accepted: report=%+v err=%v process=%d queries=%d closed=%d",
					report, err, processCalls, len(repository.queries), len(repository.closed))
			}
		})
	}
}

func TestRecoverStartupSessionsFailsClosedOnRepositoryCursorRollback(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 17, 50, 0, 0, time.UTC)
	sessions := []LiveSession{
		startupRecoverySession(t, now.Add(-2*time.Minute), RecordingDisabled),
		startupRecoverySession(t, now.Add(-time.Minute), RecordingDisabled),
	}
	sort.Slice(sessions, func(left, right int) bool { return sessions[left].ID < sessions[right].ID })
	repository := &startupRecoveryRepositoryStub{sessions: sessions}
	repository.list = func(
		_ context.Context,
		query RecoverablePageQuery,
	) (RecoverableSessionPage, error) {
		if query.AfterID == "" {
			return RecoverableSessionPage{
				Sessions: []LiveSession{sessions[1]}, NextID: sessions[1].ID,
			}, nil
		}
		return RecoverableSessionPage{
			Sessions: []LiveSession{sessions[0]}, NextID: sessions[0].ID,
		}, nil
	}
	eventCalls := 0
	report, err := RecoverStartupSessions(context.Background(), StartupRecoveryOptions{
		Repository:       repository,
		ProcessRecoverer: startupProcessRecovererStub{},
		EventRecoverer: SessionEventRecoveryFunc(func(
			_ context.Context,
			_ LiveSession,
			minimum time.Time,
		) (time.Time, error) {
			eventCalls++
			return minimum, nil
		}),
		Now:      func() time.Time { return now },
		NewID:    func() (string, error) { return newV7(t), nil },
		PageSize: 1,
	})
	if report.Scanned != 1 || report.Recovered != 1 || report.Failed != 0 ||
		eventCalls != 1 || len(repository.queries) != 2 || len(repository.closed) != 1 ||
		repository.closed[0].SessionID != sessions[1].ID ||
		!errors.Is(err, ErrStartupRecoveryIncomplete) ||
		!errors.Is(err, ErrStartupProcessRecovery) {
		t.Fatalf("repository rollback was accepted: report=%+v err=%v events=%d queries=%d closed=%+v",
			report, err, eventCalls, len(repository.queries), repository.closed)
	}
}

func TestRecoverStartupSessionsFailsClosedOnCorruptedRecordingSessionBeforeProcessCheck(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	session := startupRecoverySession(t, now.Add(-time.Minute), RecordingActive)
	session.ID = "https://private.example/session?token=secret\nforged=true"
	session.RoomConfigID = `D:\recordings\private-room`
	var reported []StartupRecoveryEvent
	repository := &startupRecoveryRepositoryStub{list: func(
		context.Context,
		RecoverablePageQuery,
	) (RecoverableSessionPage, error) {
		return RecoverableSessionPage{Sessions: []LiveSession{session}}, nil
	}}
	processCalls := 0
	report, err := RecoverStartupSessions(context.Background(), StartupRecoveryOptions{
		Repository: repository,
		ProcessRecoverer: startupProcessRecovererStub{recover: func(
			context.Context,
			string,
		) (SessionProcessRecoveryResult, error) {
			processCalls++
			return SessionProcessRecoveryResult{}, nil
		}},
		EventRecoverer: SessionEventRecoveryFunc(func(
			context.Context,
			LiveSession,
			time.Time,
		) (time.Time, error) {
			t.Fatal("corrupted recording session must stop before event recovery")
			return time.Time{}, nil
		}),
		Reporter: StartupRecoveryReporterFunc(func(event StartupRecoveryEvent) {
			reported = append(reported, event)
		}),
		Now: func() time.Time { return now },
	})
	if report.Scanned != 0 || report.Failed != 0 || processCalls != 0 ||
		!errors.Is(err, ErrStartupRecoveryIncomplete) ||
		!errors.Is(err, ErrStartupProcessRecovery) || len(reported) != 0 ||
		len(repository.closed) != 0 {
		t.Fatalf("corrupted session result = report:%+v err:%v process:%d reported:%+v",
			report, err, processCalls, reported)
	}
}

func TestStartupRecoveryReporterSanitizesCorrelationIDs(t *testing.T) {
	t.Parallel()
	validSessionID := newV7(t)
	validRoomID := newV7(t)
	tests := []struct {
		name          string
		sessionID     string
		roomID        string
		wantSessionID string
		wantRoomID    string
	}{
		{
			name:          "valid UUIDv7 unchanged",
			sessionID:     validSessionID,
			roomID:        validRoomID,
			wantSessionID: validSessionID,
			wantRoomID:    validRoomID,
		},
		{
			name: "filesystem paths", sessionID: `C:\Users\private\capture`,
			roomID: `D:\recordings\private`, wantSessionID: "invalid", wantRoomID: "invalid",
		},
		{
			name: "URLs", sessionID: "https://private.example/session?token=secret",
			roomID: "file:///D:/private/room", wantSessionID: "invalid", wantRoomID: "invalid",
		},
		{
			name: "line injection", sessionID: "session\nforged=true",
			roomID: "room\r\nlevel=admin", wantSessionID: "invalid", wantRoomID: "invalid",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var reported StartupRecoveryEvent
			coordinator := startupRecoveryCoordinator{reporter: StartupRecoveryReporterFunc(func(event StartupRecoveryEvent) {
				reported = event
			})}
			coordinator.report(StartupRecoveryEvent{SessionID: test.sessionID, RoomConfigID: test.roomID})
			if reported.SessionID != test.wantSessionID || reported.RoomConfigID != test.wantRoomID {
				t.Fatalf("reported correlation IDs = %q/%q, want %q/%q",
					reported.SessionID, reported.RoomConfigID, test.wantSessionID, test.wantRoomID)
			}
		})
	}
}

type startupRecoveryInterruptContext struct {
	context.Context
	done chan struct{}
	err  error
}

func newStartupRecoveryInterruptContext() *startupRecoveryInterruptContext {
	return &startupRecoveryInterruptContext{
		Context: context.Background(),
		done:    make(chan struct{}),
	}
}

func (ctx *startupRecoveryInterruptContext) Done() <-chan struct{} {
	return ctx.done
}

func (ctx *startupRecoveryInterruptContext) Err() error {
	return ctx.err
}

func (ctx *startupRecoveryInterruptContext) interrupt(err error) {
	ctx.err = err
	close(ctx.done)
}

type startupRecoveryRepositoryStub struct {
	sessions []LiveSession
	queries  []RecoverablePageQuery
	closed   []RecoverAndCloseInput
	list     func(context.Context, RecoverablePageQuery) (RecoverableSessionPage, error)
}

func (repository *startupRecoveryRepositoryStub) ListRecoverablePage(
	ctx context.Context,
	query RecoverablePageQuery,
) (RecoverableSessionPage, error) {
	repository.queries = append(repository.queries, query)
	if repository.list != nil {
		return repository.list(ctx, query)
	}
	page := RecoverableSessionPage{Sessions: make([]LiveSession, 0, query.Limit)}
	for _, session := range repository.sessions {
		if session.ID <= query.AfterID || session.CreatedAt > query.ScanCutoffMS ||
			!activeSessionStatus(session.Status) {
			continue
		}
		page.Sessions = append(page.Sessions, session)
		if len(page.Sessions) == query.Limit {
			page.NextID = session.ID
			break
		}
	}
	if len(page.Sessions) < query.Limit {
		page.NextID = ""
	}
	return page, nil
}

func (repository *startupRecoveryRepositoryStub) RecoverAndClose(
	_ context.Context,
	input RecoverAndCloseInput,
) (LiveSession, error) {
	repository.closed = append(repository.closed, input)
	for _, session := range repository.sessions {
		if session.ID != input.SessionID {
			continue
		}
		endedAt := input.EndedAtMS
		session.Status = SessionInterrupted
		session.RecordingStatus = RecordingIncomplete
		if input.ExpectedRecordingStatus == RecordingDisabled {
			session.RecordingStatus = RecordingDisabled
		}
		session.OperationID = input.RecoveryOperationID
		session.EndedAt = &endedAt
		return session, nil
	}
	return LiveSession{}, ErrSessionNotFound
}

type startupProcessRecovererStub struct {
	recover func(context.Context, string) (SessionProcessRecoveryResult, error)
}

func (stub startupProcessRecovererStub) RecoverSessionProcesses(
	ctx context.Context,
	sessionID string,
) (SessionProcessRecoveryResult, error) {
	if stub.recover == nil {
		return SessionProcessRecoveryResult{}, nil
	}
	return stub.recover(ctx, sessionID)
}

type startupMediaRecovererStub struct {
	recover func(context.Context, LiveSession, time.Time) (SessionMediaRecoveryResult, error)
}

func (stub startupMediaRecovererStub) RecoverSessionMedia(
	ctx context.Context,
	session LiveSession,
	cutoff time.Time,
) (SessionMediaRecoveryResult, error) {
	return stub.recover(ctx, session, cutoff)
}

func completeStartupMediaSnapshot(session LiveSession) MediaSnapshot {
	return MediaSnapshot{
		Session: SessionMedia{
			SessionID: session.ID,
			State:     SessionMediaCompleted,
		},
		Segments: []MediaSegment{{Status: MediaSegmentRecovered}},
	}
}

func startupRecoverySession(
	t *testing.T,
	startedAt time.Time,
	recording RecordingStatus,
) LiveSession {
	return LiveSession{
		ID: newV7(t), RoomConfigID: newV7(t), OperationID: newV7(t),
		Status: SessionRecording, RecordingStatus: recording,
		StartedAt: startedAt.UnixMilli(), CreatedAt: startedAt.UnixMilli(),
		UpdatedAt: startedAt.Add(10 * time.Second).UnixMilli(), IntegrityScore: 1,
		DataPath: "rooms/room/sessions/2026/07/session", SchemaVersion: 1,
	}
}
