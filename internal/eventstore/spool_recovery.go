package eventstore

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func RepairSpool(root string) error {
	return RepairSpoolWithOptions(DefaultSpoolOptions(root))
}

func RepairSpoolWithOptions(options SpoolOptions) error {
	normalized, err := normalizeSpoolOptions(options)
	if err != nil {
		return err
	}
	if err := normalized.MkdirAll(filepath.Join(normalized.Root, "spool"), 0o700); err != nil {
		return fmt.Errorf("create spool directory: %w", err)
	}
	_, err = repairSpool(normalized)
	return err
}

func RepairSpoolCheckpointWithOptions(options SpoolOptions, checkpoint Checkpoint) error {
	options.ProtectedSpool = checkpoint.Spool
	options.ProtectedRaw = checkpoint.Raw
	return RepairSpoolWithOptions(options)
}

type repairFilePlan struct {
	relative     string
	originalSize int64
	repairedSize int64
	boundaries   map[int64]struct{}
}

type spoolCursorKey struct {
	file   string
	offset int64
}

type spoolRepairPlan struct {
	recovery     spoolRecovery
	rawNames     []string
	walNames     []string
	raw          map[string]*repairFilePlan
	wal          map[string]*repairFilePlan
	cursorRawEnd map[spoolCursorKey]SpoolPosition
}

func repairSpool(options SpoolOptions) (spoolRecovery, error) {
	plan, err := analyzeSpool(options)
	if err != nil {
		return spoolRecovery{}, err
	}
	if err := validateProtectedCheckpoint(options, plan); err != nil {
		return spoolRecovery{}, err
	}
	if options.MaxTotalBytes > 0 && plan.recovery.TotalBytes > options.MaxTotalBytes {
		return spoolRecovery{}, ErrSpoolLimit
	}
	if err := verifyRepairPlanUnchanged(options, plan); err != nil {
		return spoolRecovery{}, err
	}
	for _, name := range plan.rawNames {
		if err := applyRepairFile(options, plan.raw["spool/"+name]); err != nil {
			return spoolRecovery{}, err
		}
	}
	for _, name := range plan.walNames {
		if err := applyRepairFile(options, plan.wal["spool/"+name]); err != nil {
			return spoolRecovery{}, err
		}
	}
	return plan.recovery, nil
}

func analyzeSpool(options SpoolOptions) (*spoolRepairPlan, error) {
	plan := &spoolRepairPlan{
		raw:          make(map[string]*repairFilePlan),
		wal:          make(map[string]*repairFilePlan),
		cursorRawEnd: make(map[spoolCursorKey]SpoolPosition),
	}
	plan.recovery.NextRawIndex = 1
	plan.recovery.NextWALIndex = 1
	entries, err := options.ReadDir(filepath.Join(options.Root, "spool"))
	if errors.Is(err, fs.ErrNotExist) {
		return plan, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read spool directory: %w", err)
	}
	plan.rawNames = sortedSpoolFiles(entries, ".binpack")
	plan.walNames = sortedSpoolFiles(entries, ".wal")
	if err := validateSegmentContinuity(plan.rawNames, "raw", ".binpack"); err != nil {
		return nil, err
	}
	if err := validateSegmentContinuity(plan.walNames, "wal", ".wal"); err != nil {
		return nil, err
	}
	plan.recovery.NextRawIndex = nextSegmentIndex(plan.rawNames)
	plan.recovery.NextWALIndex = nextSegmentIndex(plan.walNames)

	for index, name := range plan.rawNames {
		filePlan, err := analyzeRawFile(options, name, index == len(plan.rawNames)-1)
		if err != nil {
			return nil, err
		}
		plan.raw[filePlan.relative] = filePlan
		plan.recovery.TotalBytes += filePlan.repairedSize
	}

	rawReader := &rawReferenceReader{options: options}
	defer rawReader.Close()
	var lastRawRef *RawRef
	for index, name := range plan.walNames {
		filePlan, lastSequence, retainedRawRef, err := analyzeWALFile(
			options,
			name,
			index == len(plan.walNames)-1,
			plan.recovery.LastSequence,
			lastRawRef,
			plan,
			rawReader,
		)
		if err != nil {
			return nil, err
		}
		plan.wal[filePlan.relative] = filePlan
		plan.recovery.TotalBytes += filePlan.repairedSize
		plan.recovery.LastSequence = lastSequence
		lastRawRef = retainedRawRef
	}
	return plan, nil
}

func analyzeRawFile(options SpoolOptions, name string, final bool) (*repairFilePlan, error) {
	relative := "spool/" + name
	if !validSessionRelativePath(relative) {
		return nil, fmt.Errorf("%w: unsafe raw segment", ErrFrameCorrupt)
	}
	file, err := options.OpenFile(filepath.Join(options.Root, filepath.FromSlash(relative)), os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("open raw segment for analysis: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat raw segment: %w", err)
	}
	result := &repairFilePlan{
		relative: relative, originalSize: info.Size(),
		boundaries: map[int64]struct{}{0: {}},
	}
	var offset int64
	for {
		_, frame, err := DecodeRawFrame(file, options.MaxPayloadBytes)
		switch {
		case err == nil:
			offset += frame.EncodedLength
			result.boundaries[offset] = struct{}{}
		case errors.Is(err, io.EOF):
			result.repairedSize = info.Size()
			return result, nil
		case errors.Is(err, ErrFrameTruncated) && final:
			result.repairedSize = offset
			return result, nil
		case errors.Is(err, ErrFrameTruncated):
			return nil, fmt.Errorf("%w: incomplete non-tail raw segment", ErrFrameCorrupt)
		default:
			return nil, fmt.Errorf("scan raw segment %s: %w", name, err)
		}
	}
}

type rawReferenceStatus uint8

const (
	rawReferenceValid rawReferenceStatus = iota
	rawReferenceMissing
	rawReferenceTruncated
)

func analyzeWALFile(
	options SpoolOptions,
	name string,
	final bool,
	previousSequence int64,
	previousRawRef *RawRef,
	plan *spoolRepairPlan,
	rawReader *rawReferenceReader,
) (*repairFilePlan, int64, *RawRef, error) {
	relative := "spool/" + name
	if !validSessionRelativePath(relative) {
		return nil, previousSequence, previousRawRef, fmt.Errorf("%w: unsafe wal segment", ErrFrameCorrupt)
	}
	file, err := options.OpenFile(filepath.Join(options.Root, filepath.FromSlash(relative)), os.O_RDONLY, 0)
	if err != nil {
		return nil, previousSequence, previousRawRef, fmt.Errorf("open wal segment for analysis: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, previousSequence, previousRawRef, fmt.Errorf("stat wal segment: %w", err)
	}
	result := &repairFilePlan{
		relative: relative, originalSize: info.Size(),
		boundaries: map[int64]struct{}{0: {}},
	}
	offset := int64(0)
	scanLastSequence := previousSequence
	retainedLastSequence := previousSequence
	retainedRawRef := cloneRawRef(previousRawRef)
	var crashSuffixStart int64 = -1
	var crashRawRef *RawRef

	for {
		record, frame, decodeErr := DecodeWALFrame(file, options.MaxWALFrameBytes)
		switch {
		case decodeErr == nil:
			if record.Envelope.Sequence <= scanLastSequence {
				return nil, retainedLastSequence, retainedRawRef, ErrSequenceOrder
			}
			scanLastSequence = record.Envelope.Sequence
			frameEnd := offset + frame.EncodedLength
			status, refErr := classifyRawReference(record.Raw, plan, rawReader)
			if refErr != nil {
				return nil, retainedLastSequence, retainedRawRef,
					fmt.Errorf("validate wal raw reference: %w", refErr)
			}
			if crashSuffixStart < 0 {
				switch status {
				case rawReferenceValid:
					if err := validateRetainedRawOrder(retainedRawRef, record.Raw); err != nil {
						return nil, retainedLastSequence, retainedRawRef, err
					}
					retainedLastSequence = record.Envelope.Sequence
					retainedRawRef = cloneRawRef(&record.Raw)
					result.boundaries[frameEnd] = struct{}{}
					rawEnd, _ := rawReferenceEnd(record.Raw)
					plan.cursorRawEnd[spoolCursorKey{file: relative, offset: frameEnd}] = SpoolPosition{
						File: record.Raw.File, Offset: rawEnd,
					}
				case rawReferenceMissing, rawReferenceTruncated:
					if !final || !validCrashSuffixStart(record.Raw, status, plan) {
						return nil, retainedLastSequence, retainedRawRef,
							fmt.Errorf("%w: non-final or unproven raw suffix", ErrFrameCorrupt)
					}
					crashSuffixStart = offset
					crashRawRef = cloneRawRef(&record.Raw)
				}
			} else {
				if status == rawReferenceValid {
					return nil, retainedLastSequence, retainedRawRef,
						fmt.Errorf("%w: valid record follows broken raw suffix", ErrFrameCorrupt)
				}
				if !validCrashSuffixContinuation(crashRawRef, record.Raw) {
					return nil, retainedLastSequence, retainedRawRef,
						fmt.Errorf("%w: discontinuous raw crash suffix", ErrFrameCorrupt)
				}
				crashRawRef = cloneRawRef(&record.Raw)
			}
			offset = frameEnd
		case errors.Is(decodeErr, io.EOF):
			if crashSuffixStart >= 0 {
				result.repairedSize = crashSuffixStart
			} else {
				result.repairedSize = info.Size()
			}
			return result, retainedLastSequence, retainedRawRef, nil
		case errors.Is(decodeErr, ErrFrameTruncated) && final:
			if crashSuffixStart >= 0 {
				result.repairedSize = crashSuffixStart
			} else {
				result.repairedSize = offset
			}
			return result, retainedLastSequence, retainedRawRef, nil
		case errors.Is(decodeErr, ErrFrameTruncated):
			return nil, retainedLastSequence, retainedRawRef,
				fmt.Errorf("%w: incomplete non-tail wal segment", ErrFrameCorrupt)
		default:
			return nil, retainedLastSequence, retainedRawRef,
				fmt.Errorf("scan wal segment %s: %w", name, decodeErr)
		}
	}
}

func cloneRawRef(ref *RawRef) *RawRef {
	if ref == nil {
		return nil
	}
	value := *ref
	return &value
}

func rawReferenceEnd(ref RawRef) (int64, bool) {
	if ref.Offset < 0 || ref.Length < RawFrameHeaderSize ||
		ref.Offset > int64(^uint64(0)>>1)-ref.Length {
		return 0, false
	}
	return ref.Offset + ref.Length, true
}

func classifyRawReference(
	ref RawRef,
	plan *spoolRepairPlan,
	reader *rawReferenceReader,
) (rawReferenceStatus, error) {
	if !validSessionRelativePath(ref.File) || !strings.HasPrefix(ref.File, "spool/") {
		return rawReferenceValid, fmt.Errorf("%w: invalid raw reference", ErrFrameCorrupt)
	}
	name := strings.TrimPrefix(ref.File, "spool/")
	if strings.Contains(name, "/") {
		return rawReferenceValid, fmt.Errorf("%w: invalid raw segment name", ErrFrameCorrupt)
	}
	if _, ok := parseSpoolSegmentName(name, "raw", ".binpack"); !ok {
		return rawReferenceValid, fmt.Errorf("%w: malformed raw segment name", ErrFrameCorrupt)
	}
	end, ok := rawReferenceEnd(ref)
	if !ok {
		return rawReferenceValid, fmt.Errorf("%w: invalid raw reference range", ErrFrameCorrupt)
	}
	filePlan, exists := plan.raw[ref.File]
	if !exists {
		return rawReferenceMissing, nil
	}
	if ref.Offset > filePlan.repairedSize || end > filePlan.repairedSize {
		return rawReferenceTruncated, nil
	}
	if _, err := reader.Read(ref); err != nil {
		return rawReferenceValid, fmt.Errorf("%w: referenced raw frame", ErrFrameCorrupt)
	}
	return rawReferenceValid, nil
}

func validateRetainedRawOrder(previous *RawRef, current RawRef) error {
	if previous == nil {
		return nil
	}
	previousName := strings.TrimPrefix(previous.File, "spool/")
	currentName := strings.TrimPrefix(current.File, "spool/")
	previousIndex, previousOK := parseSpoolSegmentName(previousName, "raw", ".binpack")
	currentIndex, currentOK := parseSpoolSegmentName(currentName, "raw", ".binpack")
	previousEnd, endOK := rawReferenceEnd(*previous)
	if !previousOK || !currentOK || !endOK ||
		currentIndex < previousIndex ||
		(currentIndex == previousIndex && current.Offset < previousEnd) {
		return fmt.Errorf("%w: raw references are not ordered", ErrFrameCorrupt)
	}
	return nil
}

func validCrashSuffixStart(ref RawRef, status rawReferenceStatus, plan *spoolRepairPlan) bool {
	name := strings.TrimPrefix(ref.File, "spool/")
	index, ok := parseSpoolSegmentName(name, "raw", ".binpack")
	if !ok {
		return false
	}
	switch status {
	case rawReferenceMissing:
		return index == nextSegmentIndex(plan.rawNames) && ref.Offset == 0
	case rawReferenceTruncated:
		if len(plan.rawNames) == 0 {
			return false
		}
		last := plan.raw["spool/"+plan.rawNames[len(plan.rawNames)-1]]
		return last != nil && ref.File == last.relative && ref.Offset == last.repairedSize
	default:
		return false
	}
}

func validCrashSuffixContinuation(previous *RawRef, current RawRef) bool {
	if previous == nil {
		return false
	}
	previousEnd, ok := rawReferenceEnd(*previous)
	if !ok {
		return false
	}
	if current.File == previous.File {
		return current.Offset == previousEnd
	}
	previousIndex, previousOK := parseSpoolSegmentName(
		strings.TrimPrefix(previous.File, "spool/"), "raw", ".binpack",
	)
	currentIndex, currentOK := parseSpoolSegmentName(
		strings.TrimPrefix(current.File, "spool/"), "raw", ".binpack",
	)
	return previousOK && currentOK && currentIndex == previousIndex+1 && current.Offset == 0
}

func validateSegmentContinuity(names []string, kind, extension string) error {
	for index, name := range names {
		segmentIndex, ok := parseSpoolSegmentName(name, kind, extension)
		if !ok || segmentIndex != index+1 {
			return fmt.Errorf("%w: %s segment index gap", ErrFrameCorrupt, kind)
		}
	}
	return nil
}

func parseSpoolSegmentName(name, kind, extension string) (int, bool) {
	prefix := kind + "-"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, extension) {
		return 0, false
	}
	base := strings.TrimSuffix(strings.TrimPrefix(name, prefix), extension)
	dash := strings.LastIndexByte(base, '-')
	if dash <= 0 {
		return 0, false
	}
	stamp, digits := base[:dash], base[dash+1:]
	if len(stamp) != len("20060102T15") {
		return 0, false
	}
	if _, err := time.Parse("20060102T15", stamp); err != nil {
		return 0, false
	}
	index, err := strconv.Atoi(digits)
	if err != nil || index <= 0 || fmt.Sprintf("%06d", index) != digits {
		return 0, false
	}
	return index, true
}

func spoolSegmentIndex(name string) (int, bool) {
	switch {
	case strings.HasSuffix(name, ".binpack"):
		return parseSpoolSegmentName(name, "raw", ".binpack")
	case strings.HasSuffix(name, ".wal"):
		return parseSpoolSegmentName(name, "wal", ".wal")
	default:
		return 0, false
	}
}

func nextSegmentIndex(names []string) int {
	if len(names) == 0 {
		return 1
	}
	index, ok := spoolSegmentIndex(names[len(names)-1])
	if !ok {
		return 1
	}
	return index + 1
}

func validateProtectedCheckpoint(options SpoolOptions, plan *spoolRepairPlan) error {
	if err := validateProtectedPosition(options.ProtectedSpool, "wal", plan.wal); err != nil {
		return err
	}
	if err := validateProtectedPosition(options.ProtectedRaw, "raw", plan.raw); err != nil {
		return err
	}
	if options.ProtectedSpool.File == "" {
		if options.ProtectedRaw.File != "" {
			return fmt.Errorf("%w: raw checkpoint without wal checkpoint", ErrFrameCorrupt)
		}
		return nil
	}
	if options.ProtectedSpool.Offset == 0 {
		if options.ProtectedRaw.File != "" || options.ProtectedRaw.Offset != 0 {
			return fmt.Errorf("%w: raw checkpoint disagrees with empty wal cursor", ErrFrameCorrupt)
		}
		return nil
	}
	derived, ok := plan.cursorRawEnd[spoolCursorKey{
		file: options.ProtectedSpool.File, offset: options.ProtectedSpool.Offset,
	}]
	if !ok {
		return fmt.Errorf("%w: protected wal cursor is not retained", ErrFrameCorrupt)
	}
	if options.ProtectedRaw.File != "" && derived != options.ProtectedRaw {
		return fmt.Errorf("%w: protected raw cursor disagrees with wal", ErrFrameCorrupt)
	}
	return nil
}

func validateProtectedPosition(
	position SpoolPosition,
	kind string,
	files map[string]*repairFilePlan,
) error {
	if position.File == "" {
		if position.Offset != 0 {
			return fmt.Errorf("%w: checkpoint offset without file", ErrFrameCorrupt)
		}
		return nil
	}
	extension := ".wal"
	if kind == "raw" {
		extension = ".binpack"
	}
	if position.Offset < 0 || !validSessionRelativePath(position.File) ||
		!strings.HasPrefix(position.File, "spool/") {
		return fmt.Errorf("%w: invalid protected %s checkpoint", ErrFrameCorrupt, kind)
	}
	name := strings.TrimPrefix(position.File, "spool/")
	if strings.Contains(name, "/") {
		return fmt.Errorf("%w: invalid protected %s file", ErrFrameCorrupt, kind)
	}
	if _, ok := parseSpoolSegmentName(name, kind, extension); !ok {
		return fmt.Errorf("%w: malformed protected %s file", ErrFrameCorrupt, kind)
	}
	filePlan, ok := files[position.File]
	if !ok {
		return fmt.Errorf("%w: protected %s file is missing", ErrFrameCorrupt, kind)
	}
	if position.Offset > filePlan.repairedSize {
		return fmt.Errorf("%w: protected %s checkpoint exceeds repair boundary", ErrFrameCorrupt, kind)
	}
	if _, ok := filePlan.boundaries[position.Offset]; !ok {
		return fmt.Errorf("%w: protected %s checkpoint is not a frame boundary", ErrFrameCorrupt, kind)
	}
	return nil
}

func verifyRepairPlanUnchanged(options SpoolOptions, plan *spoolRepairPlan) error {
	for _, name := range plan.rawNames {
		if err := verifyRepairFileUnchanged(options, plan.raw["spool/"+name]); err != nil {
			return err
		}
	}
	for _, name := range plan.walNames {
		if err := verifyRepairFileUnchanged(options, plan.wal["spool/"+name]); err != nil {
			return err
		}
	}
	return nil
}

func verifyRepairFileUnchanged(options SpoolOptions, plan *repairFilePlan) error {
	file, err := options.OpenFile(filepath.Join(options.Root, filepath.FromSlash(plan.relative)), os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("%w: repair input changed", ErrFrameCorrupt)
	}
	info, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil || closeErr != nil || info.Size() != plan.originalSize {
		return fmt.Errorf("%w: repair input changed", ErrFrameCorrupt)
	}
	return nil
}

func applyRepairFile(options SpoolOptions, plan *repairFilePlan) error {
	if plan == nil || plan.repairedSize == plan.originalSize {
		return nil
	}
	file, err := options.OpenFile(
		filepath.Join(options.Root, filepath.FromSlash(plan.relative)),
		os.O_RDWR,
		0o600,
	)
	if err != nil {
		return fmt.Errorf("open spool segment for repair: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return fmt.Errorf("stat spool segment for repair: %w", err)
	}
	if info.Size() != plan.originalSize {
		file.Close()
		return fmt.Errorf("%w: repair input changed", ErrFrameCorrupt)
	}
	if err := file.Truncate(plan.repairedSize); err != nil {
		file.Close()
		return fmt.Errorf("truncate spool tail: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync repaired spool tail: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close repaired spool tail: %w", err)
	}
	return nil
}

// ReplaySpool repairs only a proven final crash suffix, then replays records
// after the protected committed cursor.
func ReplaySpool(root string, after SpoolPosition, visit func(SpoolRecord, SpoolPosition) error) error {
	return ReplaySpoolWithOptions(DefaultSpoolOptions(root), after, visit)
}

func ReplaySpoolCheckpointWithOptions(
	options SpoolOptions,
	checkpoint Checkpoint,
	visit func(SpoolRecord, SpoolPosition) error,
) error {
	options.ProtectedSpool = checkpoint.Spool
	options.ProtectedRaw = checkpoint.Raw
	return ReplaySpoolWithOptions(options, checkpoint.Spool, visit)
}

func ReplaySpoolWithOptions(options SpoolOptions, after SpoolPosition, visit func(SpoolRecord, SpoolPosition) error) error {
	if visit == nil {
		return fmt.Errorf("replay visitor is required")
	}
	if err := validateReplayCursorSyntax(after); err != nil {
		return err
	}
	normalized, err := normalizeSpoolOptions(options)
	if err != nil {
		return err
	}
	if normalized.ProtectedSpool != (SpoolPosition{}) && normalized.ProtectedSpool != after {
		return fmt.Errorf("%w: replay and protected wal cursors disagree", ErrFrameCorrupt)
	}
	normalized.ProtectedSpool = after
	if err := RepairSpoolWithOptions(normalized); err != nil {
		return err
	}
	entries, err := normalized.ReadDir(filepath.Join(normalized.Root, "spool"))
	if err != nil {
		return fmt.Errorf("read spool for replay: %w", err)
	}
	walNames := sortedSpoolFiles(entries, ".wal")
	checkpointFound := after.File == ""
	lastSequence := int64(0)
	rawReader := &rawReferenceReader{options: normalized}
	defer rawReader.Close()
	for _, name := range walNames {
		relative := "spool/" + name
		if !checkpointFound {
			if relative != after.File {
				continue
			}
			checkpointFound = true
		}
		start := int64(0)
		if relative == after.File {
			start = after.Offset
		}
		file, err := normalized.OpenFile(filepath.Join(normalized.Root, filepath.FromSlash(relative)), os.O_RDONLY, 0)
		if err != nil {
			return fmt.Errorf("open wal for replay: %w", err)
		}
		if _, err := file.Seek(start, io.SeekStart); err != nil {
			file.Close()
			return fmt.Errorf("seek wal checkpoint: %w", err)
		}
		offset := start
		for {
			record, frame, err := DecodeWALFrame(file, normalized.MaxWALFrameBytes)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				file.Close()
				return err
			}
			if record.Envelope.Sequence <= lastSequence {
				file.Close()
				return ErrSequenceOrder
			}
			lastSequence = record.Envelope.Sequence
			payload, err := rawReader.Read(record.Raw)
			if err != nil {
				file.Close()
				return err
			}
			record.Envelope.Payload = payload
			offset += frame.EncodedLength
			if err := visit(record, SpoolPosition{File: relative, Offset: offset}); err != nil {
				file.Close()
				return err
			}
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("close replay wal: %w", err)
		}
	}
	if !checkpointFound {
		return fmt.Errorf("%w: checkpoint wal is missing", ErrFrameCorrupt)
	}
	return nil
}

func validateReplayCursorSyntax(position SpoolPosition) error {
	if position.File == "" {
		if position.Offset != 0 {
			return fmt.Errorf("%w: checkpoint without file", ErrFrameCorrupt)
		}
		return nil
	}
	if position.Offset < 0 || !validSessionRelativePath(position.File) ||
		!strings.HasPrefix(position.File, "spool/") ||
		!strings.HasSuffix(position.File, ".wal") {
		return fmt.Errorf("%w: invalid checkpoint", ErrFrameCorrupt)
	}
	return nil
}

type rawReferenceReader struct {
	options  SpoolOptions
	file     SpoolFile
	relative string
}

func (r *rawReferenceReader) Read(ref RawRef) ([]byte, error) {
	if !validSessionRelativePath(ref.File) || ref.Offset < 0 || ref.Length < RawFrameHeaderSize {
		return nil, fmt.Errorf("%w: invalid raw reference", ErrFrameCorrupt)
	}
	if r.file == nil || r.relative != ref.File {
		if err := r.Close(); err != nil {
			return nil, fmt.Errorf("close replay raw: %w", err)
		}
		file, err := r.options.OpenFile(filepath.Join(r.options.Root, filepath.FromSlash(ref.File)), os.O_RDONLY, 0)
		if err != nil {
			return nil, fmt.Errorf("open replay raw: %w", err)
		}
		r.file = file
		r.relative = ref.File
	}
	if _, err := r.file.Seek(ref.Offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek replay raw: %w", err)
	}
	payload, frame, err := DecodeRawFrame(r.file, r.options.MaxPayloadBytes)
	if errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: raw reference beyond tail", ErrFrameTruncated)
	}
	if err != nil {
		return nil, err
	}
	if frame.EncodedLength != ref.Length || frame.CRC32C != ref.CRC32C {
		return nil, fmt.Errorf("%w: replay raw reference mismatch", ErrFrameCorrupt)
	}
	return payload, nil
}

func (r *rawReferenceReader) Close() error {
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	r.relative = ""
	return err
}
