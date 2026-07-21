//go:build p3accacceptance && windows

package main

import (
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	p3ACCDataRootMaximumEntries = 32768
	p3ACCDataRootMaximumDepth   = 64
	p3ACCDataRootReadBatchSize  = 128
	p3ACCDataRootMaximumPath    = 32768
)

type p3ACCDataRootSampleStatus uint8

const (
	p3ACCDataRootSampleTransient p3ACCDataRootSampleStatus = iota
	p3ACCDataRootSampleComplete
	p3ACCDataRootSampleInvalid
)

type p3ACCDataRootFileIdentity struct {
	volumeSerialNumber uint32
	fileIndex          uint64
	creationTime100NS  uint64
}

type p3ACCDataRootGeneration p3ACCDataRootFileIdentity

type p3ACCDataRootFileStandardInfo struct {
	allocationSize int64
	endOfFile      int64
	numberOfLinks  uint32
	deletePending  byte
	directory      byte
}

type p3ACCDataRootWalkState struct {
	dataRoot       string
	rootGeneration p3ACCDataRootGeneration
	entryCount     int
	maximumEntries int
	seen           map[p3ACCDataRootFileIdentity]struct{}
}

var p3ACCDataRootPhysicalTracker = struct {
	sync.Mutex
	initialized bool
	poisoned    bool
	previous    int64
	cumulative  int64
	generation  p3ACCDataRootGeneration
}{}

var p3ACCDataRootPhysicalSampleMu sync.Mutex

var (
	p3ACCDataRootAfterFileEnumeration = func(string) {}
	p3ACCDataRootAfterFileOpen        = func(string) {}
	p3ACCDataRootAfterFirstStandard   = func(string) {}
	p3ACCDataRootAfterSecondStandard  = func(string) {}
)

func resetP3ACCDataRootPhysicalTracker() {
	p3ACCDataRootPhysicalSampleMu.Lock()
	defer p3ACCDataRootPhysicalSampleMu.Unlock()
	p3ACCDataRootPhysicalTracker.Lock()
	p3ACCDataRootPhysicalTracker.initialized = false
	p3ACCDataRootPhysicalTracker.poisoned = false
	p3ACCDataRootPhysicalTracker.previous = 0
	p3ACCDataRootPhysicalTracker.cumulative = 0
	p3ACCDataRootPhysicalTracker.generation = p3ACCDataRootGeneration{}
	p3ACCDataRootPhysicalTracker.Unlock()
}

// readP3ACCDataRootPhysicalSample returns a monotonic lower bound formed by
// accumulating only positive changes in the controlled data-root footprint.
// A shrink updates the raw baseline but cannot invent negative disk writes.
func readP3ACCDataRootPhysicalSample(dataRoot string) (int64, bool) {
	p3ACCDataRootPhysicalSampleMu.Lock()
	defer p3ACCDataRootPhysicalSampleMu.Unlock()
	physicalBytes, generation, status := p3ACCWalkDataRootPhysicalSample(dataRoot)
	return p3ACCUpdateDataRootPhysicalTrackerStatus(physicalBytes, generation, status)
}

func p3ACCUpdateDataRootPhysicalTracker(
	physicalBytes int64,
	generation p3ACCDataRootGeneration,
	complete bool,
) (int64, bool) {
	status := p3ACCDataRootSampleTransient
	if complete {
		status = p3ACCDataRootSampleComplete
	}
	return p3ACCUpdateDataRootPhysicalTrackerStatus(physicalBytes, generation, status)
}

func p3ACCUpdateDataRootPhysicalTrackerStatus(
	physicalBytes int64,
	generation p3ACCDataRootGeneration,
	status p3ACCDataRootSampleStatus,
) (int64, bool) {
	p3ACCDataRootPhysicalTracker.Lock()
	defer p3ACCDataRootPhysicalTracker.Unlock()
	if p3ACCDataRootPhysicalTracker.poisoned {
		return 0, false
	}
	if p3ACCDataRootPhysicalTracker.previous < 0 ||
		p3ACCDataRootPhysicalTracker.cumulative < 0 ||
		(!p3ACCDataRootPhysicalTracker.initialized &&
			(p3ACCDataRootPhysicalTracker.previous != 0 ||
				p3ACCDataRootPhysicalTracker.cumulative != 0 ||
				p3ACCDataRootPhysicalTracker.generation != (p3ACCDataRootGeneration{}))) ||
		(p3ACCDataRootPhysicalTracker.initialized &&
			!p3ACCValidDataRootGeneration(p3ACCDataRootPhysicalTracker.generation)) {
		p3ACCDataRootPhysicalTracker.poisoned = true
		p3ACCDataRootPhysicalTracker.initialized = false
		return 0, false
	}
	if physicalBytes < 0 || status == p3ACCDataRootSampleInvalid ||
		(status != p3ACCDataRootSampleTransient && status != p3ACCDataRootSampleComplete) {
		p3ACCDataRootPhysicalTracker.poisoned = true
		p3ACCDataRootPhysicalTracker.initialized = false
		return 0, false
	}
	if status == p3ACCDataRootSampleTransient {
		if p3ACCDataRootPhysicalTracker.initialized {
			return p3ACCDataRootPhysicalTracker.cumulative, false
		}
		return 0, false
	}
	if !p3ACCValidDataRootGeneration(generation) {
		p3ACCDataRootPhysicalTracker.poisoned = true
		p3ACCDataRootPhysicalTracker.initialized = false
		return 0, false
	}
	if !p3ACCDataRootPhysicalTracker.initialized {
		p3ACCDataRootPhysicalTracker.initialized = true
		p3ACCDataRootPhysicalTracker.previous = physicalBytes
		p3ACCDataRootPhysicalTracker.cumulative = 0
		p3ACCDataRootPhysicalTracker.generation = generation
		return 0, false
	}
	if generation != p3ACCDataRootPhysicalTracker.generation {
		p3ACCDataRootPhysicalTracker.poisoned = true
		p3ACCDataRootPhysicalTracker.initialized = false
		return 0, false
	}
	growth := int64(0)
	if physicalBytes >= p3ACCDataRootPhysicalTracker.previous {
		growth = physicalBytes - p3ACCDataRootPhysicalTracker.previous
	}
	if growth > math.MaxInt64-p3ACCDataRootPhysicalTracker.cumulative {
		p3ACCDataRootPhysicalTracker.poisoned = true
		p3ACCDataRootPhysicalTracker.initialized = false
		return 0, false
	}
	p3ACCDataRootPhysicalTracker.previous = physicalBytes
	p3ACCDataRootPhysicalTracker.cumulative += growth
	return p3ACCDataRootPhysicalTracker.cumulative, true
}

func p3ACCValidDataRootGeneration(generation p3ACCDataRootGeneration) bool {
	return generation.volumeSerialNumber != 0 && generation.fileIndex != 0 &&
		generation.creationTime100NS != 0
}

func p3ACCWalkDataRootPhysicalBytes(dataRoot string) (int64, bool) {
	physicalBytes, _, status := p3ACCWalkDataRootPhysicalSample(dataRoot)
	return physicalBytes, status == p3ACCDataRootSampleComplete
}

// Directory enumeration is deliberately non-atomic: FFmpeg may create a new
// segment and SQLite may create or remove its WAL while a sample is running.
// A complete result is a conservative activity proof, not a point-in-time
// directory snapshot: every observed file crosses held-handle identity fences,
// while the canonical root generation is fixed within and across samples.
func p3ACCWalkDataRootPhysicalSample(
	dataRoot string,
) (int64, p3ACCDataRootGeneration, p3ACCDataRootSampleStatus) {
	clean := filepath.Clean(dataRoot)
	if dataRoot == "" || dataRoot != clean || !filepath.IsAbs(clean) ||
		p3ACCValidateNoReparsePath(clean, true) != nil {
		return 0, p3ACCDataRootGeneration{}, p3ACCDataRootSampleTransient
	}
	state := &p3ACCDataRootWalkState{
		dataRoot:       clean,
		maximumEntries: p3ACCDataRootMaximumEntries,
		seen:           make(map[p3ACCDataRootFileIdentity]struct{}),
	}
	physicalBytes, status := p3ACCWalkDataRootDirectoryCounted(clean, nil, 0, state, false)
	if status != p3ACCDataRootSampleComplete {
		return 0, p3ACCDataRootGeneration{}, status
	}
	if !p3ACCValidDataRootGeneration(state.rootGeneration) {
		return 0, p3ACCDataRootGeneration{}, p3ACCDataRootSampleInvalid
	}
	return physicalBytes, state.rootGeneration, p3ACCDataRootSampleComplete
}

func p3ACCWalkDataRootDirectory(
	directoryPath string,
	expected os.FileInfo,
	depth int,
	state *p3ACCDataRootWalkState,
) (int64, bool) {
	if state == nil {
		return 0, false
	}
	if state.dataRoot == "" {
		clean := filepath.Clean(directoryPath)
		if depth != 0 || expected != nil || directoryPath == "" || directoryPath != clean ||
			!filepath.IsAbs(clean) || p3ACCValidateNoReparsePath(clean, true) != nil {
			return 0, false
		}
		state.dataRoot = clean
	}
	physicalBytes, status := p3ACCWalkDataRootDirectoryCounted(
		directoryPath, expected, depth, state, false,
	)
	return physicalBytes, status == p3ACCDataRootSampleComplete
}

func p3ACCWalkDataRootDirectoryCounted(
	directoryPath string,
	expected os.FileInfo,
	depth int,
	state *p3ACCDataRootWalkState,
	alreadyCounted bool,
) (int64, p3ACCDataRootSampleStatus) {
	if state == nil || state.seen == nil || depth < 0 || depth > p3ACCDataRootMaximumDepth ||
		(!alreadyCounted && !p3ACCConsumeDataRootEntry(state)) {
		return 0, p3ACCDataRootSampleTransient
	}
	allowRoot := depth == 0
	if state.dataRoot == "" || directoryPath == "" || filepath.Clean(directoryPath) != directoryPath ||
		!p3ACCAcceptancePathWithin(state.dataRoot, directoryPath, allowRoot) {
		return 0, p3ACCDataRootSampleTransient
	}
	directory, identity, info, err := p3ACCOpenDataRootPath(
		state.dataRoot, directoryPath, true,
	)
	if err != nil {
		return 0, p3ACCDataRootSampleTransient
	}
	if expected != nil && !os.SameFile(expected, info) {
		_ = directory.Close()
		return 0, p3ACCDataRootSampleTransient
	}
	if _, duplicate := state.seen[identity]; duplicate {
		_ = directory.Close()
		return 0, p3ACCDataRootSampleTransient
	}
	if depth == 0 {
		generation := p3ACCDataRootGeneration(identity)
		if !p3ACCValidDataRootGeneration(generation) ||
			(state.rootGeneration != (p3ACCDataRootGeneration{}) &&
				state.rootGeneration != generation) {
			_ = directory.Close()
			return 0, p3ACCDataRootSampleTransient
		}
		state.rootGeneration = generation
	}
	state.seen[identity] = struct{}{}
	total := int64(0)
	for {
		readCount := p3ACCDataRootReadCount(state)
		if readCount < 1 {
			_ = directory.Close()
			return 0, p3ACCDataRootSampleTransient
		}
		entries, readErr := directory.ReadDir(readCount)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			_ = directory.Close()
			return 0, p3ACCDataRootSampleTransient
		}
		if len(entries) == 0 {
			if !errors.Is(readErr, io.EOF) {
				_ = directory.Close()
				return 0, p3ACCDataRootSampleTransient
			}
			break
		}
		for _, entry := range entries {
			if !p3ACCConsumeDataRootEntry(state) {
				_ = directory.Close()
				return 0, p3ACCDataRootSampleTransient
			}
			name := entry.Name()
			if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "\\/") {
				_ = directory.Close()
				return 0, p3ACCDataRootSampleTransient
			}
			entryInfo, infoErr := entry.Info()
			if infoErr != nil || entryInfo.Mode()&os.ModeSymlink != 0 {
				_ = directory.Close()
				return 0, p3ACCDataRootSampleTransient
			}
			entryPath := filepath.Join(directoryPath, name)
			if filepath.Clean(entryPath) != entryPath ||
				!p3ACCAcceptancePathWithin(state.dataRoot, entryPath, false) {
				_ = directory.Close()
				return 0, p3ACCDataRootSampleTransient
			}
			if entryInfo.IsDir() {
				childBytes, childStatus := p3ACCWalkDataRootDirectoryCounted(
					entryPath, entryInfo, depth+1, state, true,
				)
				if childStatus == p3ACCDataRootSampleInvalid {
					_ = directory.Close()
					return 0, p3ACCDataRootSampleInvalid
				}
				if childStatus != p3ACCDataRootSampleComplete {
					_ = directory.Close()
					return 0, p3ACCDataRootSampleTransient
				}
				if p3ACCAddPhysicalBytesStatus(&total, childBytes) != p3ACCDataRootSampleComplete {
					_ = directory.Close()
					return 0, p3ACCDataRootSampleInvalid
				}
				continue
			}
			if !entryInfo.Mode().IsRegular() {
				_ = directory.Close()
				return 0, p3ACCDataRootSampleTransient
			}
			p3ACCDataRootAfterFileEnumeration(entryPath)
			file, fileIdentity, fileInfo, openErr := p3ACCOpenDataRootPath(
				state.dataRoot, entryPath, false,
			)
			if openErr != nil || !os.SameFile(entryInfo, fileInfo) {
				if file != nil {
					_ = file.Close()
				}
				_ = directory.Close()
				return 0, p3ACCDataRootSampleTransient
			}
			p3ACCDataRootAfterFileOpen(entryPath)
			physicalBytes, physicalOK := p3ACCDataRootFilePhysicalBytes(
				file, state.dataRoot, entryPath, fileIdentity, entryInfo, fileInfo,
			)
			_, duplicate := state.seen[fileIdentity]
			if duplicate || !physicalOK {
				_ = file.Close()
				_ = directory.Close()
				return 0, p3ACCDataRootSampleTransient
			}
			if p3ACCAddPhysicalBytesStatus(&total, physicalBytes) != p3ACCDataRootSampleComplete {
				_ = file.Close()
				_ = directory.Close()
				return 0, p3ACCDataRootSampleInvalid
			}
			state.seen[fileIdentity] = struct{}{}
			if closeErr := file.Close(); closeErr != nil {
				_ = directory.Close()
				return 0, p3ACCDataRootSampleTransient
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	finalIdentity, finalInfo, finalOK := p3ACCDataRootHandleSnapshot(
		directory, state.dataRoot, directoryPath, true,
	)
	if !finalOK || finalIdentity != identity || !os.SameFile(info, finalInfo) ||
		(depth == 0 &&
			p3ACCDataRootGeneration(finalIdentity) != state.rootGeneration) {
		_ = directory.Close()
		return 0, p3ACCDataRootSampleTransient
	}
	if closeErr := directory.Close(); closeErr != nil {
		return 0, p3ACCDataRootSampleTransient
	}
	return total, p3ACCDataRootSampleComplete
}

func p3ACCDataRootReadCount(state *p3ACCDataRootWalkState) int {
	if state == nil || state.maximumEntries < 1 ||
		state.maximumEntries > p3ACCDataRootMaximumEntries ||
		state.entryCount < 0 || state.entryCount > state.maximumEntries {
		return 0
	}
	remaining := state.maximumEntries - state.entryCount
	if remaining >= p3ACCDataRootReadBatchSize {
		return p3ACCDataRootReadBatchSize
	}
	return remaining + 1
}

func p3ACCConsumeDataRootEntry(state *p3ACCDataRootWalkState) bool {
	if state == nil || state.maximumEntries < 1 ||
		state.maximumEntries > p3ACCDataRootMaximumEntries ||
		state.entryCount < 0 || state.entryCount >= state.maximumEntries {
		return false
	}
	state.entryCount++
	return true
}

func p3ACCAddPhysicalBytesStatus(total *int64, value int64) p3ACCDataRootSampleStatus {
	if total == nil || *total < 0 || value < 0 || value > math.MaxInt64-*total {
		return p3ACCDataRootSampleInvalid
	}
	*total += value
	return p3ACCDataRootSampleComplete
}

func p3ACCAddPhysicalBytes(total *int64, value int64) bool {
	return p3ACCAddPhysicalBytesStatus(total, value) == p3ACCDataRootSampleComplete
}

func p3ACCDataRootPathAllowed(dataRoot, filename string, directory bool) bool {
	if dataRoot == "" || filename == "" || filepath.Clean(dataRoot) != dataRoot ||
		filepath.Clean(filename) != filename || !filepath.IsAbs(dataRoot) ||
		!filepath.IsAbs(filename) {
		return false
	}
	allowRoot := directory && strings.EqualFold(dataRoot, filename)
	return p3ACCAcceptancePathWithin(dataRoot, filename, allowRoot)
}

func p3ACCDataRootFinalPath(file *os.File) (string, bool) {
	if file == nil {
		return "", false
	}
	buffer := make([]uint16, p3ACCDataRootMaximumPath)
	length, err := windows.GetFinalPathNameByHandle(
		windows.Handle(file.Fd()), &buffer[0], uint32(len(buffer)), 0,
	)
	if err != nil || length == 0 || length >= uint32(len(buffer)) {
		return "", false
	}
	value := windows.UTF16ToString(buffer[:length])
	switch {
	case strings.HasPrefix(value, `\\?\UNC\`):
		value = `\\` + value[len(`\\?\UNC\`):]
	case strings.HasPrefix(value, `\\?\`):
		value = value[len(`\\?\`):]
	case strings.HasPrefix(value, `\??\`):
		value = value[len(`\??\`):]
	}
	value = filepath.Clean(value)
	if !filepath.IsAbs(value) {
		return "", false
	}
	return value, true
}

func p3ACCDataRootHandleSnapshot(
	file *os.File,
	dataRoot string,
	filename string,
	directory bool,
) (p3ACCDataRootFileIdentity, os.FileInfo, bool) {
	if file == nil || !p3ACCDataRootPathAllowed(dataRoot, filename, directory) {
		return p3ACCDataRootFileIdentity{}, nil, false
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(
		windows.Handle(file.Fd()), &information,
	); err != nil {
		return p3ACCDataRootFileIdentity{}, nil, false
	}
	isDirectory := information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		isDirectory != directory || (!directory && information.NumberOfLinks != 1) {
		return p3ACCDataRootFileIdentity{}, nil, false
	}
	identity := p3ACCDataRootFileIdentity{
		creationTime100NS: uint64(information.CreationTime.HighDateTime)<<32 |
			uint64(information.CreationTime.LowDateTime),
		volumeSerialNumber: information.VolumeSerialNumber,
		fileIndex: uint64(information.FileIndexHigh)<<32 |
			uint64(information.FileIndexLow),
	}
	if identity.volumeSerialNumber == 0 || identity.fileIndex == 0 ||
		identity.creationTime100NS == 0 {
		return p3ACCDataRootFileIdentity{}, nil, false
	}
	finalPath, finalPathOK := p3ACCDataRootFinalPath(file)
	if !finalPathOK || !strings.EqualFold(finalPath, filename) ||
		!p3ACCDataRootPathAllowed(dataRoot, finalPath, directory) {
		return p3ACCDataRootFileIdentity{}, nil, false
	}
	info, err := file.Stat()
	if err != nil || info.IsDir() != directory ||
		(!directory && (!info.Mode().IsRegular() || info.Size() < 0)) {
		return p3ACCDataRootFileIdentity{}, nil, false
	}
	return identity, info, true
}

func p3ACCOpenDataRootPath(
	dataRoot string,
	filename string,
	directory bool,
) (*os.File, p3ACCDataRootFileIdentity, os.FileInfo, error) {
	if !p3ACCDataRootPathAllowed(dataRoot, filename, directory) {
		return nil, p3ACCDataRootFileIdentity{}, nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	pointer, err := windows.UTF16PtrFromString(filename)
	if err != nil {
		return nil, p3ACCDataRootFileIdentity{}, nil, err
	}
	desiredAccess := uint32(windows.FILE_READ_ATTRIBUTES)
	shareMode := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE)
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		desiredAccess = windows.FILE_GENERIC_READ
		shareMode = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	handle, err := windows.CreateFile(
		pointer, desiredAccess, shareMode, nil, windows.OPEN_EXISTING, flags, 0,
	)
	if err != nil {
		return nil, p3ACCDataRootFileIdentity{}, nil, err
	}
	file := os.NewFile(uintptr(handle), filepath.Base(filename))
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, p3ACCDataRootFileIdentity{}, nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	identity, info, ok := p3ACCDataRootHandleSnapshot(file, dataRoot, filename, directory)
	if !ok {
		_ = file.Close()
		return nil, p3ACCDataRootFileIdentity{}, nil, errors.New("P3ACC_SNAPSHOT_FAILED")
	}
	return file, identity, info, nil
}

func p3ACCReadDataRootFileStandardInfo(
	file *os.File,
) (p3ACCDataRootFileStandardInfo, bool) {
	if file == nil {
		return p3ACCDataRootFileStandardInfo{}, false
	}
	standard := p3ACCDataRootFileStandardInfo{}
	err := windows.GetFileInformationByHandleEx(
		windows.Handle(file.Fd()), windows.FileStandardInfo,
		(*byte)(unsafe.Pointer(&standard)), uint32(unsafe.Sizeof(standard)),
	)
	return standard, err == nil
}

func p3ACCValidDataRootFileStandardInfo(standard p3ACCDataRootFileStandardInfo) bool {
	return standard.allocationSize >= 0 && standard.endOfFile >= 0 &&
		standard.numberOfLinks == 1 && standard.deletePending == 0 && standard.directory == 0
}

func p3ACCDataRootFilePhysicalBytes(
	file *os.File,
	dataRoot string,
	filename string,
	expectedIdentity p3ACCDataRootFileIdentity,
	enumerated os.FileInfo,
	handleInfo os.FileInfo,
) (int64, bool) {
	if file == nil || enumerated == nil || handleInfo == nil ||
		expectedIdentity.volumeSerialNumber == 0 || expectedIdentity.fileIndex == 0 ||
		!p3ACCDataRootPathAllowed(dataRoot, filename, false) ||
		!os.SameFile(enumerated, handleInfo) || !enumerated.Mode().IsRegular() ||
		!handleInfo.Mode().IsRegular() || enumerated.Size() < 0 || handleInfo.Size() < 0 {
		return 0, false
	}
	firstStandard, firstStandardOK := p3ACCReadDataRootFileStandardInfo(file)
	if !firstStandardOK || !p3ACCValidDataRootFileStandardInfo(firstStandard) ||
		enumerated.Size() > handleInfo.Size() || handleInfo.Size() > firstStandard.endOfFile {
		return 0, false
	}
	p3ACCDataRootAfterFirstStandard(filename)
	secondStandard, secondStandardOK := p3ACCReadDataRootFileStandardInfo(file)
	if !secondStandardOK || !p3ACCValidDataRootFileStandardInfo(secondStandard) ||
		secondStandard.allocationSize < firstStandard.allocationSize ||
		secondStandard.endOfFile < firstStandard.endOfFile {
		return 0, false
	}
	p3ACCDataRootAfterSecondStandard(filename)
	finalIdentity, finalInfo, finalIdentityOK := p3ACCDataRootHandleSnapshot(
		file, dataRoot, filename, false,
	)
	if !finalIdentityOK || finalIdentity != expectedIdentity ||
		!os.SameFile(enumerated, finalInfo) || !os.SameFile(handleInfo, finalInfo) ||
		!finalInfo.Mode().IsRegular() || finalInfo.Size() < secondStandard.endOfFile {
		return 0, false
	}
	pathFile, pathIdentity, pathInfo, pathErr := p3ACCOpenDataRootPath(
		dataRoot, filename, false,
	)
	if pathErr != nil || pathIdentity != expectedIdentity ||
		!os.SameFile(finalInfo, pathInfo) {
		if pathFile != nil {
			_ = pathFile.Close()
		}
		return 0, false
	}
	if closeErr := pathFile.Close(); closeErr != nil {
		return 0, false
	}
	return secondStandard.allocationSize, true
}
