//go:build p3accacceptance

package capture

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
)

var (
	ErrP3AcceptanceRecorderFence       = errors.New("P3ACC_RECORDER_FENCE_MISMATCH")
	ErrP3AcceptanceRecorderUnavailable = errors.New("P3ACC_RECORDER_UNAVAILABLE")
	ErrP3AcceptanceRecorderCrash       = errors.New("P3ACC_RECORDER_CRASH_FAILED")
)

// P3AcceptanceRecorderMediaActivity is an identifier-free physical-media
// observation used only by the tagged acceptance harness. Paths and file names
// never cross this boundary.
type P3AcceptanceRecorderMediaActivity struct {
	FileCount  int
	TotalBytes int64
}

// CrashP3AcceptanceCurrentRecorder is compiled only into the P3-ACC harness.
// It terminates the exact process object already owned by the current recorder;
// it never enumerates processes, reads command lines, or returns a process ID.
func CrashP3AcceptanceCurrentRecorder(
	ctx context.Context,
	coordinator CaptureCoordinator,
	roomID, sessionID, operationID, attemptID string,
) error {
	if ctx == nil {
		return ErrP3AcceptanceRecorderFence
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if validateUUIDv7("p3 acceptance room", roomID) != nil ||
		validateUUIDv7("p3 acceptance session", sessionID) != nil ||
		validateUUIDv7("p3 acceptance operation", operationID) != nil ||
		validateUUIDv7("p3 acceptance attempt", attemptID) != nil {
		return ErrP3AcceptanceRecorderFence
	}
	concrete, ok := coordinator.(*Coordinator)
	if !ok || concrete == nil {
		return ErrP3AcceptanceRecorderUnavailable
	}

	concrete.registryMu.Lock()
	entry := concrete.runtimes[roomID]
	var runtime *sessionRuntime
	if entry != nil {
		runtime = entry.session
	}
	concrete.registryMu.Unlock()
	if runtime == nil {
		return ErrP3AcceptanceRecorderUnavailable
	}

	runtime.operationMu.Lock()
	defer runtime.operationMu.Unlock()
	runtime.mu.Lock()
	current := runtime.current
	recorder, recorderOK := runtime.recorder.(*FFmpegRecorder)
	fenced := !runtime.finalizing && !runtime.finalized &&
		current.RoomConfigID == roomID && current.ID == sessionID &&
		current.OperationID == operationID && runtime.operationID == operationID &&
		current.Status == SessionRecording && current.RecordingStatus == RecordingActive
	runtime.mu.Unlock()
	if !fenced {
		return ErrP3AcceptanceRecorderFence
	}
	if !recorderOK || recorder == nil {
		return ErrP3AcceptanceRecorderUnavailable
	}

	recorder.mu.Lock()
	attempt := recorder.current
	if recorder.stopping || recorder.stopped || attempt == nil || attempt.id != attemptID ||
		attempt.starting || attempt.expected || attempt.process == nil {
		recorder.mu.Unlock()
		return ErrP3AcceptanceRecorderFence
	}
	process := attempt.process
	recorder.mu.Unlock()
	select {
	case <-process.done():
		return ErrP3AcceptanceRecorderFence
	default:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := process.terminateProcess(); err != nil {
		return ErrP3AcceptanceRecorderCrash
	}
	return nil
}

// P3AcceptanceCurrentRecorderMatches verifies the same internal four-part
// fence without returning a process or any correlation identifier.
func P3AcceptanceCurrentRecorderMatches(
	coordinator CaptureCoordinator,
	roomID, sessionID, operationID, attemptID string,
) bool {
	if validateUUIDv7("p3 acceptance room", roomID) != nil ||
		validateUUIDv7("p3 acceptance session", sessionID) != nil ||
		validateUUIDv7("p3 acceptance operation", operationID) != nil ||
		validateUUIDv7("p3 acceptance attempt", attemptID) != nil {
		return false
	}
	concrete, ok := coordinator.(*Coordinator)
	if !ok || concrete == nil {
		return false
	}
	concrete.registryMu.Lock()
	entry := concrete.runtimes[roomID]
	var runtime *sessionRuntime
	if entry != nil {
		runtime = entry.session
	}
	concrete.registryMu.Unlock()
	if runtime == nil {
		return false
	}
	runtime.operationMu.Lock()
	defer runtime.operationMu.Unlock()
	runtime.mu.Lock()
	current := runtime.current
	recorder, recorderOK := runtime.recorder.(*FFmpegRecorder)
	fenced := !runtime.finalizing && !runtime.finalized &&
		current.RoomConfigID == roomID && current.ID == sessionID &&
		current.OperationID == operationID && runtime.operationID == operationID &&
		current.Status == SessionRecording && current.RecordingStatus == RecordingActive
	runtime.mu.Unlock()
	if !fenced || !recorderOK || recorder == nil {
		return false
	}
	recorder.mu.Lock()
	attempt := recorder.current
	matched := !recorder.stopping && !recorder.stopped && attempt != nil &&
		attempt.id == attemptID && !attempt.starting && !attempt.expected && attempt.process != nil
	var process recorderProcess
	if matched {
		process = attempt.process
	}
	recorder.mu.Unlock()
	if matched {
		select {
		case <-process.done():
			return false
		default:
		}
	}
	return matched
}

// P3AcceptanceCurrentRecorderMediaActivity observes only the exact live
// recorder attempt selected by the four-part fence. It returns bounded counts
// and aggregate bytes, never paths, names, identifiers, or process details.
func P3AcceptanceCurrentRecorderMediaActivity(
	coordinator CaptureCoordinator,
	roomID, sessionID, operationID, attemptID string,
) (P3AcceptanceRecorderMediaActivity, bool) {
	if validateUUIDv7("p3 acceptance room", roomID) != nil ||
		validateUUIDv7("p3 acceptance session", sessionID) != nil ||
		validateUUIDv7("p3 acceptance operation", operationID) != nil ||
		validateUUIDv7("p3 acceptance attempt", attemptID) != nil {
		return P3AcceptanceRecorderMediaActivity{}, false
	}
	concrete, ok := coordinator.(*Coordinator)
	if !ok || concrete == nil {
		return P3AcceptanceRecorderMediaActivity{}, false
	}
	concrete.registryMu.Lock()
	entry := concrete.runtimes[roomID]
	var runtime *sessionRuntime
	if entry != nil {
		runtime = entry.session
	}
	concrete.registryMu.Unlock()
	if runtime == nil {
		return P3AcceptanceRecorderMediaActivity{}, false
	}

	runtime.operationMu.Lock()
	defer runtime.operationMu.Unlock()
	runtime.mu.Lock()
	current := runtime.current
	recorder, recorderOK := runtime.recorder.(*FFmpegRecorder)
	fenced := !runtime.finalizing && !runtime.finalized &&
		current.RoomConfigID == roomID && current.ID == sessionID &&
		current.OperationID == operationID && runtime.operationID == operationID &&
		current.Status == SessionRecording && current.RecordingStatus == RecordingActive
	runtime.mu.Unlock()
	if !fenced || !recorderOK || recorder == nil {
		return P3AcceptanceRecorderMediaActivity{}, false
	}

	recorder.mu.Lock()
	attempt := recorder.current
	matched := !recorder.stopping && !recorder.stopped && attempt != nil &&
		attempt.id == attemptID && !attempt.starting && !attempt.expected && attempt.process != nil &&
		attempt.mediaIndex >= 0 && attempt.mediaIndex < len(recorder.attempts)
	var process recorderProcess
	var mediaAttempt MediaAttempt
	mediaDirectory := ""
	if matched {
		process = attempt.process
		mediaAttempt = recorder.attempts[attempt.mediaIndex]
		mediaDirectory = recorder.options.mediaDirectory
		matched = mediaAttempt.ID == attemptID && mediaAttempt.Ordinal == attempt.ordinal &&
			validateMediaAttempt(mediaAttempt) == nil && filepath.IsAbs(mediaDirectory)
	}
	recorder.mu.Unlock()
	if !matched {
		return P3AcceptanceRecorderMediaActivity{}, false
	}
	select {
	case <-process.done():
		return P3AcceptanceRecorderMediaActivity{}, false
	default:
	}

	activity, complete := scanP3AcceptanceAttemptMedia(mediaDirectory, mediaAttempt)
	if !complete {
		return P3AcceptanceRecorderMediaActivity{}, false
	}

	// Revalidate after the filesystem observation so a process exit or attempt
	// rotation cannot publish stale physical evidence.
	runtime.mu.Lock()
	current = runtime.current
	fenced = !runtime.finalizing && !runtime.finalized &&
		current.RoomConfigID == roomID && current.ID == sessionID &&
		current.OperationID == operationID && runtime.operationID == operationID &&
		current.Status == SessionRecording && current.RecordingStatus == RecordingActive
	runtime.mu.Unlock()
	recorder.mu.Lock()
	matched = fenced && !recorder.stopping && !recorder.stopped &&
		recorder.current == attempt && attempt.process != nil && !attempt.starting && !attempt.expected &&
		attempt.id == attemptID && attempt.mediaIndex >= 0 && attempt.mediaIndex < len(recorder.attempts) &&
		recorder.attempts[attempt.mediaIndex].ID == mediaAttempt.ID
	recorder.mu.Unlock()
	if !matched {
		return P3AcceptanceRecorderMediaActivity{}, false
	}
	select {
	case <-process.done():
		return P3AcceptanceRecorderMediaActivity{}, false
	default:
		return activity, true
	}
}

func scanP3AcceptanceAttemptMedia(
	mediaDirectory string,
	attempt MediaAttempt,
) (P3AcceptanceRecorderMediaActivity, bool) {
	if !filepath.IsAbs(mediaDirectory) || validateMediaAttempt(attempt) != nil {
		return P3AcceptanceRecorderMediaActivity{}, false
	}
	attemptDirectory := filepath.Join(mediaDirectory, ".attempt-"+attempt.ID)
	relative, err := filepath.Rel(filepath.Clean(mediaDirectory), filepath.Clean(attemptDirectory))
	if err != nil || filepath.IsAbs(relative) || relative != ".attempt-"+attempt.ID {
		return P3AcceptanceRecorderMediaActivity{}, false
	}
	directoryInfo, err := os.Lstat(attemptDirectory)
	if err != nil || !directoryInfo.IsDir() {
		return P3AcceptanceRecorderMediaActivity{}, false
	}
	reparse, err := mediaPathIsReparsePoint(attemptDirectory, directoryInfo)
	if err != nil || reparse {
		return P3AcceptanceRecorderMediaActivity{}, false
	}
	directory, err := os.Open(attemptDirectory)
	if err != nil {
		return P3AcceptanceRecorderMediaActivity{}, false
	}
	defer func() { _ = directory.Close() }()
	handleInfo, statErr := directory.Stat()
	if statErr != nil || !handleInfo.IsDir() || !os.SameFile(directoryInfo, handleInfo) {
		_ = directory.Close()
		return P3AcceptanceRecorderMediaActivity{}, false
	}
	entries := make([]os.DirEntry, 0, min(maximumMediaSegments, 256))
	for {
		batch, readErr := directory.ReadDir(256)
		if len(entries)+len(batch) > maximumMediaSegments {
			_ = directory.Close()
			return P3AcceptanceRecorderMediaActivity{}, false
		}
		entries = append(entries, batch...)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = directory.Close()
			return P3AcceptanceRecorderMediaActivity{}, false
		}
	}

	seen := make(map[int]struct{}, len(entries))
	var totalBytes int64
	const maximumInt64 = int64(^uint64(0) >> 1)
	for _, entry := range entries {
		sequence, valid := parseMediaAttemptSegmentName(entry.Name(), attempt)
		if !valid || sequence > maximumMediaSegments {
			return P3AcceptanceRecorderMediaActivity{}, false
		}
		if _, exists := seen[sequence]; exists {
			return P3AcceptanceRecorderMediaActivity{}, false
		}
		seen[sequence] = struct{}{}
		filename := filepath.Join(attemptDirectory, entry.Name())
		pathInfo, err := os.Lstat(filename)
		if err != nil || !pathInfo.Mode().IsRegular() {
			return P3AcceptanceRecorderMediaActivity{}, false
		}
		reparse, err := mediaPathIsReparsePoint(filename, pathInfo)
		if err != nil || reparse {
			return P3AcceptanceRecorderMediaActivity{}, false
		}
		file, err := os.Open(filename)
		if err != nil {
			return P3AcceptanceRecorderMediaActivity{}, false
		}
		openInfo, statErr := file.Stat()
		closeErr := file.Close()
		if statErr != nil || closeErr != nil || !openInfo.Mode().IsRegular() ||
			!os.SameFile(pathInfo, openInfo) || openInfo.Size() < 0 ||
			openInfo.Size() > maximumInt64-totalBytes {
			return P3AcceptanceRecorderMediaActivity{}, false
		}
		totalBytes += openInfo.Size()
	}
	finalPathInfo, err := os.Lstat(attemptDirectory)
	if err != nil || !finalPathInfo.IsDir() || !os.SameFile(directoryInfo, finalPathInfo) {
		return P3AcceptanceRecorderMediaActivity{}, false
	}
	reparse, err = mediaPathIsReparsePoint(attemptDirectory, finalPathInfo)
	if err != nil || reparse {
		return P3AcceptanceRecorderMediaActivity{}, false
	}
	finalHandleInfo, statErr := directory.Stat()
	closeErr := directory.Close()
	if statErr != nil || closeErr != nil || !finalHandleInfo.IsDir() ||
		!os.SameFile(directoryInfo, finalHandleInfo) || !os.SameFile(finalPathInfo, finalHandleInfo) {
		return P3AcceptanceRecorderMediaActivity{}, false
	}
	return P3AcceptanceRecorderMediaActivity{FileCount: len(seen), TotalBytes: totalBytes}, true
}
