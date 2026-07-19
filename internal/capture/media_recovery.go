package capture

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var ErrMediaRecovery = errors.New("MEDIA_RECOVERY_FAILED")

const (
	MediaRecoveryOrphanAttemptWarning     = "MEDIA_RECOVERY_ORPHAN_ATTEMPT"
	MediaRecoveryOrphanSegmentWarning     = "MEDIA_RECOVERY_ORPHAN_SEGMENT"
	MediaRecoveryOrphanFileWarning        = "MEDIA_RECOVERY_ORPHAN_FILE"
	MediaRecoveryClockWarning             = "MEDIA_RECOVERY_CLOCK_UNCERTAIN"
	MediaRecoveryFailedWarning            = "MEDIA_RECOVERY_FAILED"
	MediaRecoveryProcessTerminatedWarning = "MEDIA_RECOVERY_PROCESS_TERMINATED"
	MediaRecoveryProcessFailedWarning     = "MEDIA_RECOVERY_PROCESS_FAILED"
)

type SessionMediaRecoveryResult struct {
	Snapshot            MediaSnapshot
	CutoffAt            time.Time
	WarningCodes        []string
	OrphanFiles         int
	ProcessesFound      int
	ProcessesTerminated int
}

func (result SessionMediaRecoveryResult) String() string {
	return fmt.Sprintf(
		"SessionMediaRecoveryResult{session:%s cutoff:%d warnings:%d orphans:%d}",
		mediaCorrelationID(result.Snapshot.Session.SessionID),
		result.CutoffAt.UTC().UnixMilli(),
		len(result.WarningCodes),
		result.OrphanFiles,
	)
}

func (result SessionMediaRecoveryResult) GoString() string { return result.String() }

type SessionMediaRecoverer interface {
	RecoverSessionMedia(context.Context, LiveSession, time.Time) (SessionMediaRecoveryResult, error)
}

type sessionMediaFinalizerFactory func(
	context.Context,
	sessionMediaFinalizerOptions,
) (SessionMediaFinalizer, error)

type recorderAttemptProcessInspector func(
	context.Context,
	string,
	string,
) (RecorderProcessRecoveryResult, error)

type sqliteSessionMediaRecoverer struct {
	repository     *SQLiteRepository
	tools          ffmpegTools
	dataRoot       string
	jobNamespace   string
	proxyCapacity  chan struct{}
	newFinalizer   sessionMediaFinalizerFactory
	inspectProcess recorderAttemptProcessInspector
}

func newSQLiteSessionMediaRecoverer(
	options FFmpegRecorderFactoryOptions,
	tools ffmpegTools,
) (SessionMediaRecoverer, error) {
	if options.Repository == nil ||
		!validRecorderFactoryPath(options.DataRoot, true, true) ||
		!validMediaExecutable(tools.ffmpegPath) ||
		!validMediaExecutable(tools.ffprobePath) {
		return nil, ErrMediaRecovery
	}
	dataRoot := filepath.Clean(options.DataRoot)
	jobNamespace, valid := recorderJobNamespace(dataRoot)
	repositoryNamespace, repositoryValid := recorderJobNamespace(
		options.Repository.dataRoot,
	)
	if !valid || !repositoryValid || jobNamespace != repositoryNamespace {
		return nil, ErrMediaRecovery
	}

	return &sqliteSessionMediaRecoverer{
		repository:    options.Repository,
		tools:         tools,
		dataRoot:      dataRoot,
		jobNamespace:  jobNamespace,
		proxyCapacity: make(chan struct{}, 1),
		newFinalizer: func(
			ctx context.Context,
			finalizerOptions sessionMediaFinalizerOptions,
		) (SessionMediaFinalizer, error) {
			return newSQLiteSessionMediaFinalizer(ctx, finalizerOptions)
		},
		inspectProcess: inspectRecorderAttemptProcess,
	}, nil
}

func (recoverer *sqliteSessionMediaRecoverer) RecoverSessionMedia(
	ctx context.Context,
	session LiveSession,
	scanCutoff time.Time,
) (SessionMediaRecoveryResult, error) {
	var result SessionMediaRecoveryResult
	if recoverer == nil || ctx == nil || recoverer.repository == nil ||
		!validRecorderJobNamespace(recoverer.jobNamespace) ||
		recoverer.inspectProcess == nil ||
		recoverer.newFinalizer == nil || recoverer.proxyCapacity == nil ||
		validateUUIDv7("media recovery session", session.ID) != nil ||
		!activeSessionStatus(session.Status) || session.StartedAt <= 0 ||
		scanCutoff.IsZero() || scanCutoff.UTC().UnixMilli() < session.StartedAt {
		return result, ErrMediaRecovery
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	snapshot, err := recoverer.repository.LoadSnapshot(ctx, session.ID)
	if err != nil {
		return result, err
	}
	result.Snapshot = snapshot
	for _, attempt := range snapshot.Session.Attempts {
		processResult, inspectErr := recoverer.inspectProcess(
			ctx, recoverer.jobNamespace, attempt.ID,
		)
		if processResult.Found {
			result.ProcessesFound++
		}
		if processResult.Terminated {
			result.ProcessesTerminated++
			result.WarningCodes = append(
				result.WarningCodes, MediaRecoveryProcessTerminatedWarning,
			)
		}
		if inspectErr != nil || !validSessionProcessInspection(processResult) {
			result.WarningCodes = append(result.WarningCodes, MediaRecoveryProcessFailedWarning)
			result.WarningCodes = stableMediaWarnings(result.WarningCodes)
			return result, mediaRecoveryError(ctx)
		}
	}
	root, err := recoverer.resolveRoot(ctx, snapshot.Session)
	if err != nil {
		return result, ErrMediaRecovery
	}
	sessionDirectory, err := secureMediaSessionDirectory(root, snapshot.Session.RelativePath)
	if err != nil {
		return result, ErrMediaRecovery
	}
	scan, err := scanRecoveryMediaSources(
		ctx, sessionDirectory, snapshot.Session.Attempts,
	)
	if err != nil {
		return result, mediaRecoveryError(ctx)
	}
	result.OrphanFiles = scan.orphanFiles
	result.WarningCodes = append(result.WarningCodes, scan.warningCodes...)
	result.CutoffAt = time.UnixMilli(session.StartedAt).UTC()
	if scan.lastWriteAt.After(result.CutoffAt) {
		result.CutoffAt = scan.lastWriteAt
	}
	scanCutoff = scanCutoff.UTC()
	if result.CutoffAt.After(scanCutoff) {
		result.CutoffAt = scanCutoff
		result.WarningCodes = append(result.WarningCodes, MediaRecoveryClockWarning)
	}

	if _, err := recoverer.resolveRoot(ctx, snapshot.Session); err != nil {
		return result, ErrMediaRecovery
	}
	finalizer, err := recoverer.newFinalizer(ctx, sessionMediaFinalizerOptions{
		Repository:    recoverer.repository,
		Tools:         recoverer.tools,
		Root:          root,
		RootID:        snapshot.Session.RootID,
		SessionID:     session.ID,
		RelativePath:  snapshot.Session.RelativePath,
		StartedAt:     session.StartedAt,
		ProxyCapacity: recoverer.proxyCapacity,
		Recovering:    true,
	})
	if err != nil || finalizer == nil {
		result.WarningCodes = stableMediaWarnings(append(
			result.WarningCodes, MediaRecoveryFailedWarning,
		))
		return result, mediaRecoveryError(ctx)
	}
	finalized, err := finalizer.Finalize(ctx, snapshot.Session.Attempts)
	result.Snapshot = finalized.Snapshot
	result.WarningCodes = stableMediaWarnings(append(
		result.WarningCodes, finalized.WarningCodes...,
	))
	if err != nil {
		result.WarningCodes = stableMediaWarnings(append(
			result.WarningCodes, MediaRecoveryFailedWarning,
		))
		return result, mediaRecoveryError(ctx)
	}
	if _, err := recoverer.resolveRoot(ctx, finalized.Snapshot.Session); err != nil {
		return result, ErrMediaRecovery
	}
	return result, nil
}

func (recoverer *sqliteSessionMediaRecoverer) resolveRoot(
	ctx context.Context,
	media SessionMedia,
) (string, error) {
	if media.RootID == nil {
		return recoverer.repository.resolveSessionMediaRoot(ctx, recoverer.dataRoot, nil)
	}
	if validateUUIDv7("media recovery root", *media.RootID) != nil {
		return "", ErrMediaRecovery
	}
	row, err := queryRecordingRootRow(ctx, recoverer.repository.reader, *media.RootID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrMediaRecovery
	}
	if err != nil {
		return "", ErrMediaRecovery
	}
	return recoverer.repository.resolveSessionMediaRoot(ctx, row.absolutePath, media.RootID)
}

type mediaRecoveryScan struct {
	lastWriteAt  time.Time
	warningCodes []string
	orphanFiles  int
}

func scanRecoveryMediaSources(
	ctx context.Context,
	sessionDirectory string,
	attempts []MediaAttempt,
) (mediaRecoveryScan, error) {
	var scan mediaRecoveryScan
	if ctx == nil || !validMediaAbsolutePath(sessionDirectory) {
		return scan, ErrMediaRecovery
	}
	normalized, err := normalizeMediaScanAttempts(attempts)
	if err != nil {
		return scan, ErrMediaRecovery
	}
	attemptByID := make(map[string]MediaAttempt, len(normalized))
	for _, attempt := range normalized {
		attemptByID[attempt.ID] = attempt
	}
	mediaDirectory := filepath.Join(sessionDirectory, "media")
	info, err := safeMediaRecoveryInfo(mediaDirectory)
	if err != nil || !info.IsDir() {
		return scan, ErrMediaRecovery
	}
	names, err := readMediaDirectoryNames(mediaDirectory, maximumMediaDirectoryEntries)
	if err != nil {
		return scan, ErrMediaRecovery
	}
	remaining := maximumMediaDirectoryEntries - len(names)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return scan, err
		}
		entryPath := filepath.Join(mediaDirectory, name)
		entryInfo, infoErr := safeMediaRecoveryInfo(entryPath)
		if infoErr != nil {
			return scan, ErrMediaRecovery
		}
		if entryInfo.IsDir() {
			attemptID, isAttempt := strings.CutPrefix(name, ".attempt-")
			attempt, knownAttempt := attemptByID[attemptID]
			if !isAttempt || !knownAttempt {
				scan.addOrphan(MediaRecoveryOrphanAttemptWarning)
			}
			if remaining < 1 {
				return scan, ErrMediaRecovery
			}
			childNames, readErr := readMediaDirectoryNames(entryPath, remaining)
			if readErr != nil {
				return scan, ErrMediaRecovery
			}
			remaining -= len(childNames)
			for _, childName := range childNames {
				childPath := filepath.Join(entryPath, childName)
				childInfo, childErr := safeMediaRecoveryInfo(childPath)
				if childErr != nil || !childInfo.Mode().IsRegular() {
					return scan, ErrMediaRecovery
				}
				if strings.HasSuffix(strings.ToLower(childName), ".mkv.partial") {
					scan.observe(childInfo.ModTime())
					if !knownAttempt {
						continue
					}
					if _, ok := parseMediaAttemptSegmentName(childName, attempt); ok {
						continue
					}
				}
				scan.addOrphan(MediaRecoveryOrphanFileWarning)
			}
			continue
		}
		if !entryInfo.Mode().IsRegular() {
			return scan, ErrMediaRecovery
		}
		lowerName := strings.ToLower(name)
		if strings.HasSuffix(lowerName, ".mkv") || strings.HasSuffix(lowerName, ".mkv.partial") {
			scan.observe(entryInfo.ModTime())
		}
		if sequence, startedAt, ok := parseMediaFinalSegmentName(name); ok && sequence > 0 {
			if _, _, matched := matchFinalMediaAttempt(normalized, startedAt); matched {
				continue
			}
			scan.addOrphan(MediaRecoveryOrphanSegmentWarning)
			continue
		}
		if strings.HasPrefix(lowerName, "playback-") && strings.HasSuffix(lowerName, ".mp4") {
			continue
		}
		scan.addOrphan(MediaRecoveryOrphanFileWarning)
	}
	scan.warningCodes = stableMediaWarnings(scan.warningCodes)
	return scan, nil
}

func safeMediaRecoveryInfo(path string) (os.FileInfo, error) {
	if !validMediaAbsolutePath(path) {
		return nil, ErrMediaRecovery
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, ErrMediaRecovery
	}
	reparse, err := mediaPathIsReparsePoint(path, info)
	if err != nil || reparse || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrMediaRecovery
	}
	return info, nil
}

func (scan *mediaRecoveryScan) observe(modified time.Time) {
	modified = modified.UTC()
	if modified.After(scan.lastWriteAt) {
		scan.lastWriteAt = modified
	}
}

func (scan *mediaRecoveryScan) addOrphan(warning string) {
	scan.orphanFiles++
	scan.warningCodes = append(scan.warningCodes, warning)
}

func mediaRecoveryError(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return ErrMediaRecovery
}
